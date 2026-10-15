package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Encognitio/aestrl/pkg/engine"
)

func TestP2PEndToEnd(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("Hello, Aestrl P2P! Line two."), 0644); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.New(tempDir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "testsession123"
	secret := "secretcapability456"

	hostHandler := NewSessionHandler(sessionID, secret, eng)
	defer hostHandler.Close()

	// Mock signaling server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/"+sessionID+"/signal" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			OfferSDP string `json:"offer_sdp"`
			Secret   string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		answerSDP, err := hostHandler.HandleOffer(req.OfferSDP)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{
			"answer_sdp": answerSDP,
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := DialWithSignaling(ctx, sessionID, secret, server.URL+"/s/"+sessionID+"/signal")
	if err != nil {
		t.Fatalf("DialWithSignaling failed: %v", err)
	}
	defer client.Close()

	// 1. Test 'ls'
	res, err := client.Execute(ctx, "ls", map[string]interface{}{"path": ""})
	if err != nil {
		t.Fatalf("Execute(ls) failed: %v", err)
	}
	items, ok := res.([]interface{})
	if !ok || len(items) == 0 {
		t.Fatalf("expected items in ls, got %v", res)
	}

	// 2. Test 'cat'
	catRes, err := client.Execute(ctx, "cat", map[string]interface{}{"path": "hello.txt"})
	if err != nil {
		t.Fatalf("Execute(cat) failed: %v", err)
	}
	m, ok := catRes.(map[string]interface{})
	if !ok || m["content"] != "Hello, Aestrl P2P! Line two." {
		t.Fatalf("unexpected cat result: %v", catRes)
	}

	// 3. Test invalid capability secret
	badClient := &ClientSession{
		SessionID: sessionID,
		Secret:    "wrong-secret",
		dc:        client.dc,
		pending:   make(map[string]chan *DCResponse),
		closeChan: make(chan struct{}),
	}
	_, err = badClient.Execute(ctx, "ls", nil)
	if err == nil {
		t.Fatal("expected error with wrong secret, got nil")
	}
}
