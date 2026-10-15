package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Encognitio/aestrl/pkg/host"
	"github.com/Encognitio/aestrl/pkg/p2p"
	"github.com/gorilla/websocket"
)

// MockCoordinator implements Cloudflare coordination signaling relay and discovery in memory.
type MockCoordinator struct {
	mu            sync.Mutex
	sessions      map[string]map[string]interface{}
	hostConns     map[string]*websocket.Conn
	pendingAnswer map[string]chan string
	upgrader      websocket.Upgrader
}

func newMockCoordinator() *MockCoordinator {
	return &MockCoordinator{
		sessions:      make(map[string]map[string]interface{}),
		hostConns:     make(map[string]*websocket.Conn),
		pendingAnswer: make(map[string]chan string),
		upgrader:      websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
}

func (mc *MockCoordinator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 1. POST /s/:session/register
	if r.Method == "POST" && strings.HasSuffix(path, "/register") {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		sessID := parts[1]
		var data map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&data)
		mc.mu.Lock()
		mc.sessions[sessID] = data
		mc.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
		return
	}

	// 2. GET /s/:session/host (WebSocket upgrade)
	if strings.HasSuffix(path, "/host") {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		sessID := parts[1]
		conn, err := mc.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mc.mu.Lock()
		mc.hostConns[sessID] = conn
		mc.mu.Unlock()

		go func() {
			defer conn.Close()
			for {
				var msg struct {
					Type      string `json:"type"`
					RequestID string `json:"request_id"`
					AnswerSDP string `json:"answer_sdp"`
				}
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
				if msg.Type == "answer" {
					mc.mu.Lock()
					ch, ok := mc.pendingAnswer[msg.RequestID]
					mc.mu.Unlock()
					if ok && ch != nil {
						ch <- msg.AnswerSDP
					}
				}
			}
		}()
		return
	}

	// 3. POST /s/:session/signal (WebRTC SDP offer exchange)
	if r.Method == "POST" && strings.HasSuffix(path, "/signal") {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		sessID := parts[1]

		var body struct {
			OfferSDP string `json:"offer_sdp"`
			Secret   string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		mc.mu.Lock()
		hostConn, ok := mc.hostConns[sessID]
		mc.mu.Unlock()

		if !ok || hostConn == nil {
			http.Error(w, "host not connected", 503)
			return
		}

		reqID := "req-" + time.Now().Format("150405.000000")
		answerChan := make(chan string, 1)

		mc.mu.Lock()
		mc.pendingAnswer[reqID] = answerChan
		mc.mu.Unlock()

		// Send offer to host
		_ = hostConn.WriteJSON(map[string]string{
			"type":       "offer",
			"request_id": reqID,
			"offer_sdp":  body.OfferSDP,
		})

		select {
		case answerSDP := <-answerChan:
			mc.mu.Lock()
			delete(mc.pendingAnswer, reqID)
			mc.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"answer_sdp": answerSDP,
			})
		case <-time.After(10 * time.Second):
			mc.mu.Lock()
			delete(mc.pendingAnswer, reqID)
			mc.mu.Unlock()
			http.Error(w, "timeout waiting for host answer", 504)
		}
		return
	}

	http.NotFound(w, r)
}

func TestMajorUserJourney_EndToEnd(t *testing.T) {
	// Step 0: Setup mock project
	tempDir := t.TempDir()
	sampleGo := `package main

import "fmt"

// ServerConfig stores network parameters.
type ServerConfig struct {
	Port int
	Host string
}

// StartServer launches listening loop.
func StartServer(cfg ServerConfig) error {
	fmt.Printf("Listening on %s:%d\n", cfg.Host, cfg.Port)
	return nil
}

func main() {
	cfg := ServerConfig{Port: 8080, Host: "0.0.0.0"}
	StartServer(cfg)
}
`
	_ = os.WriteFile(filepath.Join(tempDir, "main.go"), []byte(sampleGo), 0644)

	docMd := `# System Documentation

Welcome to Aestrl project.

## Architecture
P2P WebRTC inspection tool.

## Deployment
Cloudflare coordination and native binary.
`
	_ = os.WriteFile(filepath.Join(tempDir, "README.md"), []byte(docMd), 0644)

	binaryBytes := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03, 0xAA, 0xBB}
	_ = os.WriteFile(filepath.Join(tempDir, "firmware.bin"), binaryBytes, 0644)

	// Step 1: Start mock Cloudflare coordinator
	coord := newMockCoordinator()
	coordServer := httptest.NewServer(coord)
	defer coordServer.Close()

	// Step 2: SHARE — Start sharing session
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	opts := host.ShareOptions{
		TTL:       2 * time.Hour,
		EnableAI:  true,
		AllowHTTP: true,
		HubURL:    coordServer.URL,
	}
	share, err := host.StartShare(ctx, tempDir, opts)
	if err != nil {
		t.Fatalf("Share failed: %v", err)
	}
	defer share.Close()

	// Wait briefly for coordinator registration and host WebSocket connection
	time.Sleep(500 * time.Millisecond)

	// Step 3: CONNECT — Client connects over encrypted WebRTC DataChannel
	client, err := p2p.DialShare(ctx, share.Info.ShareURL)
	if err != nil {
		t.Fatalf("P2P DialShare failed: %v", err)
	}
	defer client.Close()

	// Step 4: DISCOVER — Directory listing, tree, and manifest
	lsRes, err := client.Execute(ctx, "ls", map[string]interface{}{"path": ""})
	if err != nil {
		t.Fatalf("Operation 'ls' failed: %v", err)
	}
	lsItems, ok := lsRes.([]interface{})
	if !ok || len(lsItems) < 3 {
		t.Fatalf("expected at least 3 files in ls, got %v", lsRes)
	}

	treeRes, err := client.Execute(ctx, "tree", map[string]interface{}{"path": "", "depth": 2})
	if err != nil {
		t.Fatalf("Operation 'tree' failed: %v", err)
	}
	if treeMap, ok := treeRes.(map[string]interface{}); !ok || treeMap["name"] == "" {
		t.Fatalf("unexpected tree result: %v", treeRes)
	}

	manifestRes, err := client.Execute(ctx, "manifest", nil)
	if err != nil {
		t.Fatalf("Operation 'manifest' failed: %v", err)
	}
	if manMap, ok := manifestRes.(map[string]interface{}); !ok || manMap["total_files"].(float64) < 3 {
		t.Fatalf("unexpected manifest result: %v", manifestRes)
	}

	// Step 5: SEARCH — Hybrid BM25 & symbol search and regex grep
	searchRes, err := client.Execute(ctx, "search", map[string]interface{}{"query": "ServerConfig"})
	if err != nil {
		t.Fatalf("Operation 'search' failed: %v", err)
	}
	sList, ok := searchRes.([]interface{})
	if !ok || len(sList) == 0 {
		t.Fatalf("expected ranked search results for 'ServerConfig', got: %v", searchRes)
	}

	grepRes, err := client.Execute(ctx, "grep", map[string]interface{}{"query": "Architecture"})
	if err != nil {
		t.Fatalf("Operation 'grep' failed: %v", err)
	}
	gList, ok := grepRes.([]interface{})
	if !ok || len(gList) == 0 {
		t.Fatalf("expected grep matches for 'Architecture', got: %v", grepRes)
	}

	// Step 6: INSPECT — Read content, code outline, symbol resolution, and context pack
	readRes, err := client.Execute(ctx, "read", map[string]interface{}{"path": "main.go"})
	if err != nil {
		t.Fatalf("Operation 'read' failed: %v", err)
	}
	rMap, ok := readRes.(map[string]interface{})
	if !ok || !strings.Contains(rMap["content"].(string), "StartServer") {
		t.Fatalf("unexpected read content: %v", readRes)
	}

	outlineRes, err := client.Execute(ctx, "outline", map[string]interface{}{"path": "main.go"})
	if err != nil {
		t.Fatalf("Operation 'outline' failed: %v", err)
	}
	outMap, ok := outlineRes.(map[string]interface{})
	if !ok || len(outMap["items"].([]interface{})) == 0 {
		t.Fatalf("expected items in outline: %v", outlineRes)
	}

	symRes, err := client.Execute(ctx, "symbol", map[string]interface{}{"name": "StartServer"})
	if err != nil {
		t.Fatalf("Operation 'symbol' failed: %v", err)
	}
	symList, ok := symRes.([]interface{})
	if !ok || len(symList) == 0 {
		t.Fatalf("expected symbol definition for StartServer: %v", symRes)
	}

	ctxRes, err := client.Execute(ctx, "context", map[string]interface{}{"query": "ServerConfig port", "budget": 2000})
	if err != nil {
		t.Fatalf("Operation 'context' failed: %v", err)
	}
	ctxMap, ok := ctxRes.(map[string]interface{})
	if !ok || len(ctxMap["excerpts"].([]interface{})) == 0 {
		t.Fatalf("expected context excerpts: %v", ctxRes)
	}

	// Step 7: RETRIEVE — Binary file retrieval with exact bytes
	rawRes, err := client.Execute(ctx, "raw", map[string]interface{}{"path": "firmware.bin", "offset": 0, "length": -1})
	if err != nil {
		t.Fatalf("Operation 'raw' failed: %v", err)
	}
	rawMap, ok := rawRes.(map[string]interface{})
	if !ok || int64(rawMap["size"].(float64)) != int64(len(binaryBytes)) {
		t.Fatalf("unexpected raw file metadata: %v", rawRes)
	}

	// Step 8: Verify Hosted-AI endpoint
	if share.Info.AIURL != "" {
		aiResp, err := http.Get(share.Info.AIURL + "/context?query=StartServer&secret=" + share.Info.Secret + "&format=json")
		if err != nil {
			t.Fatalf("Hosted-AI request failed: %v", err)
		}
		if aiResp.StatusCode != http.StatusOK {
			t.Fatalf("Hosted-AI returned status %d", aiResp.StatusCode)
		}
		_ = aiResp.Body.Close()
	}

	share.Close()
	time.Sleep(100 * time.Millisecond)

	_, err = client.Execute(ctx, "ls", nil)
	if err == nil {
		t.Fatal("expected error after share stopped, got success")
	}
}

func TestBrowserP2P_And_RemoteCLI_Parity(t *testing.T) {
	tempDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tempDir, "doc.txt"), []byte("Parity Content 12345"), 0644)

	coord := newMockCoordinator()
	coordServer := httptest.NewServer(coord)
	defer coordServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	opts := host.ShareOptions{
		TTL:       1 * time.Hour,
		EnableAI:  true,
		AllowHTTP: true,
		HubURL:    coordServer.URL,
	}
	share, err := host.StartShare(ctx, tempDir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer share.Close()
	time.Sleep(300 * time.Millisecond)

	// 1. Remote CLI P2P connection
	cliClient, err := p2p.DialShare(ctx, share.Info.ShareURL)
	if err != nil {
		t.Fatalf("CLI DialShare failed: %v", err)
	}
	defer cliClient.Close()

	cliRead, err := cliClient.Execute(ctx, "read", map[string]interface{}{"path": "doc.txt"})
	if err != nil {
		t.Fatalf("CLI P2P read failed: %v", err)
	}

	// 2. Browser P2P simulation (exact browser signaling endpoint)
	browserClient, err := p2p.DialWithSignaling(ctx, share.Info.SessionID, share.Info.Secret, coordServer.URL+"/s/"+share.Info.SessionID+"/signal")
	if err != nil {
		t.Fatalf("Browser P2P connection failed: %v", err)
	}
	defer browserClient.Close()

	browserRead, err := browserClient.Execute(ctx, "read", map[string]interface{}{"path": "doc.txt"})
	if err != nil {
		t.Fatalf("Browser P2P read failed: %v", err)
	}

	// 3. Separate HTTP inspection verification
	httpResp, err := http.Get(share.Info.AIURL + "/read?path=doc.txt&secret=" + share.Info.Secret + "&format=json")
	if err != nil {
		t.Fatalf("HTTP inspection GET failed: %v", err)
	}
	defer httpResp.Body.Close()
	var httpRead struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(httpResp.Body).Decode(&httpRead)

	// 4. Parity verification
	cliContent := cliRead.(map[string]interface{})["content"].(string)
	browserContent := browserRead.(map[string]interface{})["content"].(string)

	if cliContent != "Parity Content 12345" {
		t.Fatalf("unexpected CLI content: %s", cliContent)
	}
	if browserContent != cliContent {
		t.Fatalf("Browser content %q != CLI content %q", browserContent, cliContent)
	}
	if httpRead.Content != cliContent {
		t.Fatalf("HTTP content %q != CLI content %q", httpRead.Content, cliContent)
	}
}

