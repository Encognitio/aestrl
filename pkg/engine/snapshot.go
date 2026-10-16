package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// SnapshotEntry records file state at indexing time.
type SnapshotEntry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	SHA256   string    `json:"sha256,omitempty"`
	IsBinary bool      `json:"is_binary"`
	Vector   []float32 `json:"vector,omitempty"`
}

// IndexSnapshot records the entire indexed state of a shared directory.
type IndexSnapshot struct {
	RootPath  string                   `json:"root_path"`
	UpdatedAt time.Time                `json:"updated_at"`
	Files     map[string]SnapshotEntry `json:"files"` // relPath -> SnapshotEntry
}

// DeltaReport describes additions, modifications, and deletions between snapshots.
type DeltaReport struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Deleted  []string `json:"deleted"`
}

// SaveSnapshot saves the current index snapshot to disk.
func (idx *HybridIndex) SaveSnapshot(destFile string) error {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	dir := filepath.Dir(destFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	snap := IndexSnapshot{
		RootPath:  idx.eng.rootPath,
		UpdatedAt: time.Now(),
		Files:     make(map[string]SnapshotEntry),
	}

	for path := range idx.docs {
		realPath := filepath.Join(idx.eng.realRoot, filepath.FromSlash(path))
		fi, err := os.Stat(realPath)
		if err != nil {
			continue
		}
		snap.Files[path] = SnapshotEntry{
			Path:     path,
			Size:     fi.Size(),
			ModTime:  fi.ModTime(),
			IsBinary: false,
			Vector:   idx.docs[path].Vector,
		}
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := destFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, destFile)
}

// LoadOrDeltaIndex loads an existing snapshot if present and only indexes additions/modifications.
func (idx *HybridIndex) LoadOrDeltaIndex(snapshotFile string) (*DeltaReport, error) {
	data, err := os.ReadFile(snapshotFile)
	if err != nil {
		// No existing snapshot, do full index
		err := idx.BuildIndex()
		if err != nil {
			return nil, err
		}
		_ = idx.SaveSnapshot(snapshotFile)
		return &DeltaReport{}, nil
	}

	var prev IndexSnapshot
	if err := json.Unmarshal(data, &prev); err != nil {
		// Corrupt snapshot, rebuild
		_ = idx.BuildIndex()
		_ = idx.SaveSnapshot(snapshotFile)
		return &DeltaReport{}, nil
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Initial scan of current files
	currentFiles := make(map[string]os.FileInfo)
	_ = filepath.Walk(idx.eng.realRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(idx.eng.realRoot, path)
		if err != nil {
			return nil
		}
		normRel := filepath.ToSlash(rel)
		if idx.eng.isExcluded(info.Name(), normRel) {
			return nil
		}
		currentFiles[normRel] = info
		return nil
	})

	delta := &DeltaReport{}

	// Detect added or modified
	for normRel, curInfo := range currentFiles {
		prevEntry, exists := prev.Files[normRel]
		if !exists {
			delta.Added = append(delta.Added, normRel)
		} else if curInfo.Size() != prevEntry.Size || !curInfo.ModTime().Equal(prevEntry.ModTime) {
			delta.Modified = append(delta.Modified, normRel)
		}
	}

	// Detect deleted
	for prevPath := range prev.Files {
		if _, exists := currentFiles[prevPath]; !exists {
			delta.Deleted = append(delta.Deleted, prevPath)
		}
	}

	// If no changes, restore documents from files
	// Rebuild index updated with delta
	idx.docs = make(map[string]*Document)
	idx.docFreq = make(map[string]int)

	totalLength := 0
	for normRel, curInfo := range currentFiles {
		if curInfo.Size() > idx.eng.maxReadBytes || isFileBinary(filepath.Join(idx.eng.realRoot, filepath.FromSlash(normRel)), curInfo.Size()) {
			continue
		}
		realPath := filepath.Join(idx.eng.realRoot, filepath.FromSlash(normRel))
		doc := idx.indexFile(realPath, normRel)
		if doc != nil {
			if prevEntry, exists := prev.Files[normRel]; exists && len(prevEntry.Vector) > 0 && curInfo.Size() == prevEntry.Size && curInfo.ModTime().Equal(prevEntry.ModTime) {
				doc.Vector = prevEntry.Vector
			} else {
				doc.Vector = idx.encodeVector(doc)
			}
			idx.docs[normRel] = doc
			totalLength += doc.TermCount
			for term := range doc.Terms {
				idx.docFreq[term]++
			}
		}
	}

	idx.totalDocs = len(idx.docs)
	if idx.totalDocs > 0 {
		idx.avgDocLen = float64(totalLength) / float64(idx.totalDocs)
	}

	// Save updated snapshot
	go func() {
		_ = idx.SaveSnapshot(snapshotFile)
	}()

	return delta, nil
}

// SnapshotDir returns .aestrl/index beside executable, ~/.aestrl/index, or temp dir fallback.
// Index files are always stored outside the shared directory tree.
func SnapshotDir() string {
	if exe, err := os.Executable(); err == nil {
		sidecarDir := filepath.Join(filepath.Dir(exe), ".aestrl")
		if fi, err := os.Stat(sidecarDir); err == nil && fi.IsDir() {
			dir := filepath.Join(sidecarDir, "index")
			_ = os.MkdirAll(dir, 0700)
			return dir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		dir := filepath.Join(os.TempDir(), "aestrl", "index")
		_ = os.MkdirAll(dir, 0700)
		return dir
	}
	dir := filepath.Join(home, ".aestrl", "index")
	_ = os.MkdirAll(dir, 0700)
	return dir
}

// SaveSnapshot saves the current index snapshot for a given share session ID.
func (e *Engine) SaveSnapshot(shareID string) error {
	dest := filepath.Join(SnapshotDir(), shareID+".json")
	return e.index.SaveSnapshot(dest)
}

// LoadOrDeltaIndex loads an existing snapshot if present and only indexes deltas.
func (e *Engine) LoadOrDeltaIndex(shareID string) (*DeltaReport, error) {
	dest := filepath.Join(SnapshotDir(), shareID+".json")
	return e.index.LoadOrDeltaIndex(dest)
}

