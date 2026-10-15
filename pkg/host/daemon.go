package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Daemon manages multiple concurrent active shares on the local machine.
type Daemon struct {
	socketPath string
	listener   net.Listener
	server     *http.Server
	shares     map[string]*ActiveShare // sessionID -> ActiveShare
	byPath     map[string]string       // absPath -> sessionID
	mu         sync.RWMutex
	stopChan   chan struct{}
}

// DefaultSocketPath returns the standard Unix domain socket path for Aestrl.
func DefaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "aestrl-daemon.sock")
	}
	dir := filepath.Join(home, ".aestrl")
	_ = os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "daemon.sock")
}

// NewDaemon creates a new daemon instance.
func NewDaemon(socketPath string) (*Daemon, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}

	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on socket %s: %w", socketPath, err)
	}
	_ = os.Chmod(socketPath, 0600)

	d := &Daemon{
		socketPath: socketPath,
		listener:   ln,
		shares:     make(map[string]*ActiveShare),
		byPath:     make(map[string]string),
		stopChan:   make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/share", d.handleShare)
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/stop", d.handleStop)

	d.server = &http.Server{Handler: mux}

	go func() {
		_ = d.server.Serve(ln)
	}()

	go d.expiryLoop()

	return d, nil
}

func (d *Daemon) expiryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			d.mu.Lock()
			now := time.Now()
			for id, s := range d.shares {
				if now.After(s.Info.ExpiresAt) {
					s.Close()
					delete(d.byPath, s.Info.RootDir)
					delete(d.shares, id)
				}
			}
			d.mu.Unlock()
		}
	}
}

// Close stops the daemon and all active shares.
func (d *Daemon) Close() error {
	d.mu.Lock()
	select {
	case <-d.stopChan:
		d.mu.Unlock()
		return nil
	default:
		close(d.stopChan)
	}

	for _, s := range d.shares {
		s.Close()
	}
	d.shares = make(map[string]*ActiveShare)
	d.byPath = make(map[string]string)
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.server.Shutdown(ctx)
	_ = os.Remove(d.socketPath)
	return nil
}

func (d *Daemon) handleShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path       string   `json:"path"`
		TTL        string   `json:"ttl"`
		EnableAI   bool     `json:"enable_ai"`
		AIAddr     string   `json:"ai_addr"`
		AIDomain   string   `json:"ai_domain"`
		AllowHTTP  bool     `json:"allow_http"`
		Exclusions []string `json:"exclusions"`
		HubURL     string   `json:"hub_url"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	absPath, err := filepath.Abs(req.Path)
	if err != nil {
		http.Error(w, "Invalid path: "+err.Error(), http.StatusBadRequest)
		return
	}

	ttl := 2 * time.Hour
	if req.TTL != "" {
		if d, err := time.ParseDuration(req.TTL); err == nil && d > 0 {
			ttl = d
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	var existingID, existingSecret string
	if sessID, exists := d.byPath[absPath]; exists {
		if existing, ok := d.shares[sessID]; ok {
			existingID = existing.Info.SessionID
			existingSecret = existing.Info.Secret
			existing.Close()
			delete(d.shares, sessID)
		}
	}

	opts := ShareOptions{
		TTL:               ttl,
		EnableAI:          req.EnableAI,
		AIAddr:            req.AIAddr,
		AIDomain:          req.AIDomain,
		AllowHTTP:         req.AllowHTTP,
		Exclusions:        req.Exclusions,
		HubURL:            req.HubURL,
		ExistingSessionID: existingID,
		ExistingSecret:    existingSecret,
	}

	share, err := NewActiveShare(absPath, opts)
	if err != nil {
		http.Error(w, "Failed to start share: "+err.Error(), http.StatusInternalServerError)
		return
	}

	d.shares[share.Info.SessionID] = share
	d.byPath[absPath] = share.Info.SessionID

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(share.Info)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []ShareInfo
	for _, s := range d.shares {
		info := s.Info
		info.PeerCount = s.PeerCount()
		list = append(list, info)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	d.mu.Lock()
	defer d.mu.Unlock()

	if req.SessionID == "" || req.SessionID == "all" {
		for _, s := range d.shares {
			s.Close()
		}
		d.shares = make(map[string]*ActiveShare)
		d.byPath = make(map[string]string)
		w.Write([]byte(`{"status":"all_stopped"}`))
		return
	}

	if s, ok := d.shares[req.SessionID]; ok {
		s.Close()
		delete(d.byPath, s.Info.RootDir)
		delete(d.shares, req.SessionID)
		w.Write([]byte(`{"status":"stopped"}`))
		return
	}

	http.Error(w, "Session not found", http.StatusNotFound)
}

// --- Daemon Client for CLI ---

// Client communicates with the local Aestrl daemon over Unix socket.
type Client struct {
	httpCli *http.Client
}

// NewClient creates a client talking to the local daemon socket.
func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}

	return &Client{
		httpCli: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
	}
}

// EnsureDaemon ensures the background daemon is running, starting it if necessary.
func EnsureDaemon(binaryPath, socketPath string) (*Client, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}

	client := NewClient(socketPath)

	// Test if daemon is already alive
	if _, err := client.Status(); err == nil {
		return client, nil
	}

	if binaryPath == "" {
		var err error
		binaryPath, err = os.Executable()
		if err != nil {
			binaryPath = "aestrl"
		}
	}

	// Launch daemon in background
	cmd := exec.Command(binaryPath, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background daemon: %w", err)
	}

	// Poll socket up to 3 seconds
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		time.Sleep(100 * time.Millisecond)
		if _, err := client.Status(); err == nil {
			return client, nil
		}
	}

	return nil, errors.New("timed out waiting for background daemon to start")
}

// Share registers a share with the daemon.
func (c *Client) Share(path string, opts ShareOptions) (*ShareInfo, error) {
	reqBody := map[string]interface{}{
		"path":        path,
		"ttl":         opts.TTL.String(),
		"enable_ai":   opts.EnableAI,
		"ai_addr":     opts.AIAddr,
		"ai_domain":   opts.AIDomain,
		"allow_http":  opts.AllowHTTP,
		"exclusions":  opts.Exclusions,
		"hub_url":     opts.HubURL,
	}
	data, _ := json.Marshal(reqBody)

	resp, err := c.httpCli.Post("http://unix/api/share", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon error (%d): %s", resp.StatusCode, string(b))
	}

	var info ShareInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Status returns all active shares from the daemon.
func (c *Client) Status() ([]ShareInfo, error) {
	resp, err := c.httpCli.Get("http://unix/api/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon status error: %d", resp.StatusCode)
	}

	var list []ShareInfo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
}

// Stop terminates an active share by session ID, or all if empty.
func (c *Client) Stop(sessionID string) error {
	reqBody := map[string]string{"session_id": sessionID}
	data, _ := json.Marshal(reqBody)

	resp, err := c.httpCli.Post("http://unix/api/stop", "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon stop error (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}
