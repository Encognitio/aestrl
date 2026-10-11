package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Encognitio/aestrl/pkg/client"
	"github.com/Encognitio/aestrl/pkg/config"
	"github.com/Encognitio/aestrl/pkg/host"
	"github.com/Encognitio/aestrl/pkg/hub"
	"github.com/Encognitio/aestrl/pkg/mcp"
	"github.com/Encognitio/aestrl/web"
	"github.com/pion/webrtc/v4"
)

func createTestDirectory(t *testing.T, prefix string) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	_ = os.MkdirAll(filepath.Join(dir, "src"), 0755)
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0755)

	content1 := `package main

import "fmt"

func main() {
	fmt.Println("Running Aestrl E2E test")
}
`
	_ = os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(content1), 0644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test Repo\nAestrl inspection test"), 0644)
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET_ENV=12345"), 0644)

	cleanup := func() {
		os.RemoveAll(dir)
	}
	return dir, cleanup
}

func TestEndToEnd_FullInspectionWorkflow(t *testing.T) {
	// 1. Start test Hub server
	hubDataDir, err := os.MkdirTemp("", "aestrl-hub-data-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(hubDataDir)

	hubServer, err := hub.New(hub.Config{
		DataDir: hubDataDir,
	}, web.FS())
	if err != nil {
		t.Fatalf("failed to create hub: %v", err)
	}

	ts := httptest.NewServer(hubServer.Handler())
	defer ts.Close()

	// 2. Identity A and Identity B setup
	identA := config.GenerateIdentityID()
	hostTokenA, _ := config.GenerateSecureToken(16)
	mcpTokenA, _ := config.GenerateSecureToken(16)

	identB := config.GenerateIdentityID()
	hostTokenB, _ := config.GenerateSecureToken(16)
	mcpTokenB, _ := config.GenerateSecureToken(16)

	if identA == identB {
		t.Fatal("identities collided")
	}

	// Register Identity A with Hub
	setupPayloadA := hub.SetupRequest{
		Identity:  identA,
		HostToken: hostTokenA,
		MCPToken:  mcpTokenA,
	}
	payloadBytesA, _ := json.Marshal(setupPayloadA)
	resA, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(payloadBytesA))
	if err != nil || resA.StatusCode != http.StatusOK {
		t.Fatalf("failed setup for Identity A: status %d, err %v", resA.StatusCode, err)
	}
	resA.Body.Close()

	// Register Identity B with Hub
	setupPayloadB := hub.SetupRequest{
		Identity:  identB,
		HostToken: hostTokenB,
		MCPToken:  mcpTokenB,
	}
	payloadBytesB, _ := json.Marshal(setupPayloadB)
	resB, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(payloadBytesB))
	if err != nil || resB.StatusCode != http.StatusOK {
		t.Fatalf("failed setup for Identity B: status %d, err %v", resB.StatusCode, err)
	}
	resB.Body.Close()

	// Validate repeated setup returns existing / verified
	resA2, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(payloadBytesA))
	if err != nil || resA2.StatusCode != http.StatusOK {
		t.Fatalf("failed repeated setup for Identity A: status %d, err %v", resA2.StatusCode, err)
	}
	var setupRespA hub.SetupResponse
	_ = json.NewDecoder(resA2.Body).Decode(&setupRespA)
	resA2.Body.Close()
	if setupRespA.Status != "verified" {
		t.Errorf("expected status 'verified', got '%s'", setupRespA.Status)
	}

	cfgA := &config.Config{
		Identity:  identA,
		HubURL:    ts.URL,
		HostToken: hostTokenA,
		MCPToken:  mcpTokenA,
	}

	// 3. Start Host Sharing Directory 1
	dir1, cleanupDir1 := createTestDirectory(t, "share1")
	defer cleanupDir1()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess1, err := host.StartShare(ctx, cfgA, dir1, 1*time.Hour)
	if err != nil {
		t.Fatalf("StartShare 1 failed: %v", err)
	}
	defer sess1.Close()

	// 3b. Start a Second Concurrent Share under same identity
	dir2, cleanupDir2 := createTestDirectory(t, "share2")
	defer cleanupDir2()

	sess2, err := host.StartShare(ctx, cfgA, dir2, 1*time.Hour)
	if err != nil {
		t.Fatalf("StartShare 2 failed: %v", err)
	}
	defer sess2.Close()

	// 4. Validate Terminal CLI inspection client (ls, tree, rg, cat) on sess1
	cli, err := client.ParseShareURL(sess1.ShareURL)
	if err != nil {
		t.Fatalf("ParseShareURL failed: %v", err)
	}

	// CLI ls
	items, err := cli.ListDirectory("")
	if err != nil {
		t.Fatalf("CLI ListDirectory failed: %v", err)
	}
	hasSrc := false
	for _, it := range items {
		if it.Name == "src" {
			hasSrc = true
		}
		if it.Name == ".git" || it.Name == ".env" {
			t.Errorf("excluded file exposed in listing: %s", it.Name)
		}
	}
	if !hasSrc {
		t.Errorf("expected 'src' directory in listing, got: %+v", items)
	}

	// CLI tree
	treeNode, err := cli.Tree("", 3)
	if err != nil {
		t.Fatalf("CLI Tree failed: %v", err)
	}
	renderedTree := client.RenderTree(treeNode, "", true)
	if !strings.Contains(renderedTree, "main.go") {
		t.Errorf("tree does not contain main.go:\n%s", renderedTree)
	}

	// CLI rg (search content)
	matches, err := cli.SearchContent("Running Aestrl", "", false)
	if err != nil {
		t.Fatalf("CLI SearchContent failed: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected search match for 'Running Aestrl', got 0")
	}
	if matches[0].Path != "src/main.go" {
		t.Errorf("expected match path src/main.go, got: %s", matches[0].Path)
	}

	// CLI cat (read line range)
	readRes, err := cli.ReadFile("src/main.go", 4, 7)
	if err != nil {
		t.Fatalf("CLI ReadFile failed: %v", err)
	}
	if !strings.Contains(readRes.Content, "func main()") {
		t.Errorf("expected line range to include func main(), got: %s", readRes.Content)
	}

	// 5. Test MCP Gateway Protocol against /<identA>/mcp
	mcpEndpoint := fmt.Sprintf("%s/%s/mcp", ts.URL, identA)

	// Step 5a: MCP initialize
	initReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
	}
	initBody, _ := json.Marshal(initReq)
	httpReq, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(initBody))
	httpReq.Header.Set("Authorization", "Bearer "+mcpTokenA)
	initRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || initRes.StatusCode != http.StatusOK {
		t.Fatalf("MCP initialize failed: status %d, err %v", initRes.StatusCode, err)
	}
	var initRPCResp mcp.Response
	_ = json.NewDecoder(initRes.Body).Decode(&initRPCResp)
	initRes.Body.Close()
	if initRPCResp.Error != nil {
		t.Fatalf("MCP initialize RPC error: %+v", initRPCResp.Error)
	}

	// Step 5b: MCP tools/list
	listReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
	}
	listBody, _ := json.Marshal(listReq)
	httpReq, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(listBody))
	httpReq.Header.Set("Authorization", "Bearer "+mcpTokenA)
	listRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || listRes.StatusCode != http.StatusOK {
		t.Fatalf("MCP tools/list failed: %v", err)
	}
	var listRPCResp mcp.Response
	_ = json.NewDecoder(listRes.Body).Decode(&listRPCResp)
	listRes.Body.Close()
	if listRPCResp.Error != nil {
		t.Fatalf("MCP tools/list error: %+v", listRPCResp.Error)
	}

	// Step 5c: MCP connect_share tool call
	connToolReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "connect_share",
			"arguments": {
				"share_url": "%s"
			}
		}`, sess1.ShareURL)),
	}
	connBody, _ := json.Marshal(connToolReq)
	httpReq, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(connBody))
	httpReq.Header.Set("Authorization", "Bearer "+mcpTokenA)
	connRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || connRes.StatusCode != http.StatusOK {
		t.Fatalf("MCP connect_share failed: %v", err)
	}
	var connRPCResp mcp.Response
	_ = json.NewDecoder(connRes.Body).Decode(&connRPCResp)
	connRes.Body.Close()
	if connRPCResp.Error != nil {
		t.Fatalf("MCP connect_share error: %+v", connRPCResp.Error)
	}

	// Step 5d: MCP list_directory tool call
	listDirReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "list_directory",
			"arguments": {
				"path": ""
			}
		}`),
	}
	listDirBody, _ := json.Marshal(listDirReq)
	httpReq, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(listDirBody))
	httpReq.Header.Set("Authorization", "Bearer "+mcpTokenA)
	listDirRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || listDirRes.StatusCode != http.StatusOK {
		t.Fatalf("MCP list_directory failed: %v", err)
	}
	var listDirRPCResp mcp.Response
	_ = json.NewDecoder(listDirRes.Body).Decode(&listDirRPCResp)
	listDirRes.Body.Close()
	if listDirRPCResp.Error != nil {
		t.Fatalf("MCP list_directory error: %+v", listDirRPCResp.Error)
	}

	// Step 5e: MCP read_file tool call
	readFileReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      5,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "read_file",
			"arguments": {
				"path": "src/main.go",
				"start_line": 1,
				"end_line": 10
			}
		}`),
	}
	readFileBody, _ := json.Marshal(readFileReq)
	httpReq, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(readFileBody))
	httpReq.Header.Set("Authorization", "Bearer "+mcpTokenA)
	readFileRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || readFileRes.StatusCode != http.StatusOK {
		t.Fatalf("MCP read_file failed: %v", err)
	}
	var readFileRPCResp mcp.Response
	_ = json.NewDecoder(readFileRes.Body).Decode(&readFileRPCResp)
	readFileRes.Body.Close()
	if readFileRPCResp.Error != nil {
		t.Fatalf("MCP read_file error: %+v", readFileRPCResp.Error)
	}

	// 6. Direct Browser WebRTC P2P Inspection Test
	testWebRTCBrowserP2P(t, ts.URL, sess1.SessionID, sess1.Secret)

	// 7. Test Revocation / Expiry
	sess1.Close()
	// Try accessing revoked share
	_, err = cli.ListDirectory("")
	if err == nil {
		t.Errorf("expected error accessing closed share, got nil")
	}
}

func testWebRTCBrowserP2P(t *testing.T, hubURL, sessionID, secret string) {
	// Create Pion WebRTC peer representing browser
	m := &webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	clientPC, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		t.Fatalf("failed to create browser WebRTC peer: %v", err)
	}
	defer clientPC.Close()

	dc, err := clientPC.CreateDataChannel("aestrl-inspect", nil)
	if err != nil {
		t.Fatalf("failed to create data channel: %v", err)
	}

	openChan := make(chan struct{})
	msgChan := make(chan string, 10)

	dc.OnOpen(func() {
		close(openChan)
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		msgChan <- string(msg.Data)
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription failed: %v", err)
	}

	select {
	case <-gatherComplete:
	case <-time.After(2 * time.Second):
	}

	// Send offer to Hub signaling endpoint: /s/<sessionID>/signal
	signalURL := fmt.Sprintf("%s/s/%s/signal", hubURL, sessionID)
	reqPayload := map[string]string{
		"offer_sdp": clientPC.LocalDescription().SDP,
		"secret":    secret,
	}
	data, _ := json.Marshal(reqPayload)
	sigRes, err := http.Post(signalURL, "application/json", bytes.NewReader(data))
	if err != nil || sigRes.StatusCode != http.StatusOK {
		t.Fatalf("Signaling failed: status %d, err %v", sigRes.StatusCode, err)
	}

	var sigResp struct {
		AnswerSDP string `json:"answer_sdp"`
	}
	_ = json.NewDecoder(sigRes.Body).Decode(&sigResp)
	sigRes.Body.Close()

	if sigResp.AnswerSDP == "" {
		t.Fatalf("no answer SDP received")
	}

	// Set remote description
	err = clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sigResp.AnswerSDP,
	})
	if err != nil {
		t.Fatalf("SetRemoteDescription failed: %v", err)
	}

	// Wait for data channel open (direct P2P established)
	select {
	case <-openChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("WebRTC data channel failed to open within 5s")
	}

	// Send inspection request over WebRTC DataChannel directly
	dcReq := map[string]interface{}{
		"id":     "dc-req-1",
		"action": "list_directory",
		"secret": secret,
		"args":   map[string]interface{}{"path": ""},
	}
	reqBytes, _ := json.Marshal(dcReq)
	if err := dc.SendText(string(reqBytes)); err != nil {
		t.Fatalf("failed to send over data channel: %v", err)
	}

	select {
	case respStr := <-msgChan:
		if !strings.Contains(respStr, "src") {
			t.Errorf("unexpected WebRTC DC response: %s", respStr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for WebRTC inspection response")
	}
}
