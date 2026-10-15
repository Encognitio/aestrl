package host

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonMultiShare(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "daemon.sock")

	dir1 := filepath.Join(tempDir, "dir1")
	_ = os.Mkdir(dir1, 0755)
	_ = os.WriteFile(filepath.Join(dir1, "a.txt"), []byte("file a"), 0644)

	dir2 := filepath.Join(tempDir, "dir2")
	_ = os.Mkdir(dir2, 0755)
	_ = os.WriteFile(filepath.Join(dir2, "b.txt"), []byte("file b"), 0644)

	d, err := NewDaemon(sockPath)
	if err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}
	defer d.Close()

	cli := NewClient(sockPath)

	opts := ShareOptions{TTL: 1 * time.Hour, HubURL: "https://aestrl.dvrsfy.org"}

	// 1. Share dir1
	info1, err := cli.Share(dir1, opts)
	if err != nil {
		t.Fatalf("Share(dir1) failed: %v", err)
	}
	if info1.SessionID == "" || info1.Secret == "" {
		t.Fatalf("invalid share info: %+v", info1)
	}

	// 2. Share dir2
	info2, err := cli.Share(dir2, opts)
	if err != nil {
		t.Fatalf("Share(dir2) failed: %v", err)
	}

	// 3. Status check
	list, err := cli.Status()
	if err != nil {
		t.Fatalf("Status() failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 active shares, got %d", len(list))
	}

	// 4. Re-share dir1 (must retain session ID and secret)
	_ = os.WriteFile(filepath.Join(dir1, "c.txt"), []byte("new file c"), 0644)
	info1Updated, err := cli.Share(dir1, opts)
	if err != nil {
		t.Fatalf("Re-share(dir1) failed: %v", err)
	}
	if info1Updated.SessionID != info1.SessionID {
		t.Fatalf("expected retained session ID %s, got %s", info1.SessionID, info1Updated.SessionID)
	}
	if info1Updated.Secret != info1.Secret {
		t.Fatalf("expected retained secret %s, got %s", info1.Secret, info1Updated.Secret)
	}

	// 5. Stop dir1
	if err := cli.Stop(info1.SessionID); err != nil {
		t.Fatalf("Stop(%s) failed: %v", info1.SessionID, err)
	}

	listAfterStop, err := cli.Status()
	if err != nil {
		t.Fatalf("Status() failed: %v", err)
	}
	if len(listAfterStop) != 1 || listAfterStop[0].SessionID != info2.SessionID {
		t.Fatalf("expected only dir2 active, got %+v", listAfterStop)
	}
}
