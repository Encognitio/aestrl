package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Encognitio/aestrl/pkg/client"
	"github.com/Encognitio/aestrl/pkg/config"
	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/Encognitio/aestrl/pkg/host"
	"github.com/Encognitio/aestrl/pkg/hub"
	"github.com/Encognitio/aestrl/pkg/mcp"
	"github.com/Encognitio/aestrl/web"
	"github.com/pion/webrtc/v4"
)

func createTestDirectory(t *testing.T, prefix, secretWord string) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	_ = os.MkdirAll(filepath.Join(dir, "src"), 0755)
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0755)

	content1 := fmt.Sprintf(`package main

import "fmt"

func main() {
	fmt.Println("Running Aestrl %s")
}
`, secretWord)
	_ = os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(content1), 0644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+prefix+"\nDocumentation for "+secretWord), 0644)
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET_ENV=12345"), 0644)

	cleanup := func() {
		os.RemoveAll(dir)
	}
	return dir, cleanup
}

func startTestHub(t *testing.T) (*hub.Hub, *httptest.Server, func()) {
	t.Helper()
	hubDataDir, err := os.MkdirTemp("", "aestrl-hub-data-*")
	if err != nil {
		t.Fatal(err)
	}

	hubServer, err := hub.New(hub.Config{
		DataDir: hubDataDir,
	}, web.FS())
	if err != nil {
		t.Fatalf("failed to create hub: %v", err)
	}

	ts := httptest.NewServer(hubServer.Handler())
	cleanup := func() {
		ts.Close()
		os.RemoveAll(hubDataDir)
	}
	return hubServer, ts, cleanup
}

// 1. Verify Setup: Hub registration verification and rejection handling
func TestSetup_Verification(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	// Valid registration
	setupPayload := hub.SetupRequest{
		Identity:  ident,
		HostToken: hostToken,
		MCPToken:  mcpToken,
	}
	payloadBytes, _ := json.Marshal(setupPayload)
	res, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(payloadBytes))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("setup registration failed: %v", err)
	}
	var resp hub.SetupResponse
	_ = json.NewDecoder(res.Body).Decode(&resp)
	res.Body.Close()
	if resp.Status != "created" {
		t.Errorf("expected status 'created', got '%s'", resp.Status)
	}

	// Repeated setup with same credentials -> verified
	res2, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(payloadBytes))
	if err != nil || res2.StatusCode != http.StatusOK {
		t.Fatalf("repeated setup failed: %v", err)
	}
	var resp2 hub.SetupResponse
	_ = json.NewDecoder(res2.Body).Decode(&resp2)
	res2.Body.Close()
	if resp2.Status != "verified" {
		t.Errorf("expected status 'verified', got '%s'", resp2.Status)
	}

	// Setup with token conflict -> status 409 Conflict
	conflictPayload := hub.SetupRequest{
		Identity:  ident,
		HostToken: "wrong-token",
		MCPToken:  mcpToken,
	}
	conflictBytes, _ := json.Marshal(conflictPayload)
	resConflict, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(conflictBytes))
	if err != nil {
		t.Fatal(err)
	}
	resConflict.Body.Close()
	if resConflict.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict for mismatched credentials, got %d", resConflict.StatusCode)
	}
}

// 2. Demonstrate ChatGPT Web -> Permanent MCP Endpoint -> Active Share -> Source File Read
func TestChatGPTWeb_EndToEndFlow(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	setupPayload := hub.SetupRequest{
		Identity:  ident,
		HostToken: hostToken,
		MCPToken:  mcpToken,
	}
	payloadBytes, _ := json.Marshal(setupPayload)
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(payloadBytes))

	cfg := &config.Config{
		Identity:  ident,
		HubURL:    ts.URL,
		HostToken: hostToken,
		MCPToken:  mcpToken,
	}

	dir, cleanupDir := createTestDirectory(t, "chatgpt-flow", "CHATGPT_VERIFIED_SECRET_TOKEN")
	defer cleanupDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.StartShare(ctx, cfg, dir, 1*time.Hour)
	if err != nil {
		t.Fatalf("failed to start share: %v", err)
	}
	defer sess.Close()

	mcpEndpoint := fmt.Sprintf("%s/%s/mcp", ts.URL, ident)

	// Step 1: ChatGPT initializes MCP session (Streamable HTTP)
	initReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","clientInfo":{"name":"ChatGPT-Web","version":"1.0"}}`),
	}
	initBytes, _ := json.Marshal(initReq)
	httpReq, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(initBytes))
	httpReq.Header.Set("Authorization", "Bearer "+mcpToken)
	httpReq.Header.Set("Mcp-Session-Id", "chatgpt-conv-101")

	initRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || initRes.StatusCode != http.StatusOK {
		t.Fatalf("ChatGPT initialize failed: %v", err)
	}
	var initResp mcp.Response
	_ = json.NewDecoder(initRes.Body).Decode(&initResp)
	initRes.Body.Close()
	if initResp.Error != nil {
		t.Fatalf("ChatGPT initialize returned error: %+v", initResp.Error)
	}

	// Step 2: User pastes temporary share URL in chat -> ChatGPT calls connect_share tool
	connReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "connect_share",
			"arguments": {
				"share_url": "%s"
			}
		}`, sess.ShareURL)),
	}
	connBytes, _ := json.Marshal(connReq)
	httpReq, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(connBytes))
	httpReq.Header.Set("Authorization", "Bearer "+mcpToken)
	httpReq.Header.Set("Mcp-Session-Id", "chatgpt-conv-101")

	connRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || connRes.StatusCode != http.StatusOK {
		t.Fatalf("connect_share tool call failed: %v", err)
	}
	var connRPCResp mcp.Response
	_ = json.NewDecoder(connRes.Body).Decode(&connRPCResp)
	connRes.Body.Close()
	if connRPCResp.Error != nil {
		t.Fatalf("connect_share error: %+v", connRPCResp.Error)
	}

	// Step 3: ChatGPT reads source file via read_file tool
	readReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
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
	readBytes, _ := json.Marshal(readReq)
	httpReq, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(readBytes))
	httpReq.Header.Set("Authorization", "Bearer "+mcpToken)
	httpReq.Header.Set("Mcp-Session-Id", "chatgpt-conv-101")

	readRes, err := http.DefaultClient.Do(httpReq)
	if err != nil || readRes.StatusCode != http.StatusOK {
		t.Fatalf("read_file tool call failed: %v", err)
	}
	var readRPCResp mcp.Response
	_ = json.NewDecoder(readRes.Body).Decode(&readRPCResp)
	readRes.Body.Close()
	if readRPCResp.Error != nil {
		t.Fatalf("read_file returned error: %+v", readRPCResp.Error)
	}

	resultMap, ok := readRPCResp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected result type: %T", readRPCResp.Result)
	}
	contentList, ok := resultMap["content"].([]interface{})
	if !ok || len(contentList) == 0 {
		t.Fatalf("missing content in result: %+v", resultMap)
	}
	contentItem := contentList[0].(map[string]interface{})
	text := contentItem["text"].(string)

	if !strings.Contains(text, "CHATGPT_VERIFIED_SECRET_TOKEN") {
		t.Fatalf("expected text to contain 'CHATGPT_VERIFIED_SECRET_TOKEN', got:\n%s", text)
	}
}

// 3. Enforce Identity Isolation and Capability Validation
func TestIdentityAndCapabilityIsolation(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	// Setup Identity A and Identity B
	identA := config.GenerateIdentityID()
	hostTokenA, _ := config.GenerateSecureToken(16)
	mcpTokenA, _ := config.GenerateSecureToken(16)

	identB := config.GenerateIdentityID()
	hostTokenB, _ := config.GenerateSecureToken(16)
	mcpTokenB, _ := config.GenerateSecureToken(16)

	regA, _ := json.Marshal(hub.SetupRequest{Identity: identA, HostToken: hostTokenA, MCPToken: mcpTokenA})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regA))

	regB, _ := json.Marshal(hub.SetupRequest{Identity: identB, HostToken: hostTokenB, MCPToken: mcpTokenB})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regB))

	dirA, cleanA := createTestDirectory(t, "userA", "IDENT_A_CONTENT")
	defer cleanA()

	dirB, cleanB := createTestDirectory(t, "userB", "IDENT_B_CONTENT")
	defer cleanB()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfgA := &config.Config{Identity: identA, HubURL: ts.URL, HostToken: hostTokenA, MCPToken: mcpTokenA}
	cfgB := &config.Config{Identity: identB, HubURL: ts.URL, HostToken: hostTokenB, MCPToken: mcpTokenB}

	sessA, err := host.StartShare(ctx, cfgA, dirA, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sessA.Close()

	sessB, err := host.StartShare(ctx, cfgB, dirB, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sessB.Close()

	mcpEndpointA := fmt.Sprintf("%s/%s/mcp", ts.URL, identA)

	// Test 3a: Calling read_file WITHOUT connecting share -> FAILS (unauthorized)
	readReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "read_file",
			"arguments": { "path": "src/main.go" }
		}`),
	}
	readBytes, _ := json.Marshal(readReq)
	req, _ := http.NewRequest(http.MethodPost, mcpEndpointA, bytes.NewReader(readBytes))
	req.Header.Set("Authorization", "Bearer "+mcpTokenA)
	req.Header.Set("Mcp-Session-Id", "attacker-conv-1")
	res, _ := http.DefaultClient.Do(req)
	var resp1 mcp.Response
	_ = json.NewDecoder(res.Body).Decode(&resp1)
	res.Body.Close()

	resultMap, _ := resp1.Result.(map[string]interface{})
	if isErr, ok := resultMap["isError"].(bool); !ok || !isErr {
		t.Errorf("expected isError: true when reading without connect_share, got: %+v", resultMap)
	}

	// Test 3b: Calling connect_share with corrupted secret capability -> FAILS
	tamperedURL := fmt.Sprintf("%s/s/%s#wrongsecret123456", ts.URL, sessA.SessionID)
	connBadReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "connect_share",
			"arguments": { "share_url": "%s" }
		}`, tamperedURL)),
	}
	badBytes, _ := json.Marshal(connBadReq)
	req, _ = http.NewRequest(http.MethodPost, mcpEndpointA, bytes.NewReader(badBytes))
	req.Header.Set("Authorization", "Bearer "+mcpTokenA)
	req.Header.Set("Mcp-Session-Id", "attacker-conv-1")
	res, _ = http.DefaultClient.Do(req)
	var respBad mcp.Response
	_ = json.NewDecoder(res.Body).Decode(&respBad)
	res.Body.Close()

	badResult, _ := respBad.Result.(map[string]interface{})
	if isErr, ok := badResult["isError"].(bool); !ok || !isErr {
		t.Errorf("expected isError: true when connecting with bad secret, got: %+v", badResult)
	}

	// Test 3c: Identity A trying to connect Identity B's share -> FAILS (Identity Isolation!)
	connCrossReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "connect_share",
			"arguments": { "share_url": "%s" }
		}`, sessB.ShareURL)),
	}
	crossBytes, _ := json.Marshal(connCrossReq)
	req, _ = http.NewRequest(http.MethodPost, mcpEndpointA, bytes.NewReader(crossBytes))
	req.Header.Set("Authorization", "Bearer "+mcpTokenA)
	req.Header.Set("Mcp-Session-Id", "attacker-conv-1")
	res, _ = http.DefaultClient.Do(req)
	var respCross mcp.Response
	_ = json.NewDecoder(res.Body).Decode(&respCross)
	res.Body.Close()

	crossResult, _ := respCross.Result.(map[string]interface{})
	if isErr, ok := crossResult["isError"].(bool); !ok || !isErr {
		t.Errorf("expected isError: true when cross-connecting other identity's share, got: %+v", crossResult)
	}
}

// 4. Isolate share selection across concurrent MCP conversations
func TestConcurrentConversationIsolation(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regBytes, _ := json.Marshal(hub.SetupRequest{Identity: ident, HostToken: hostToken, MCPToken: mcpToken})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regBytes))

	cfg := &config.Config{Identity: ident, HubURL: ts.URL, HostToken: hostToken, MCPToken: mcpToken}

	dir1, clean1 := createTestDirectory(t, "conv1-project", "CONV_ONE_DATA")
	defer clean1()

	dir2, clean2 := createTestDirectory(t, "conv2-project", "CONV_TWO_DATA")
	defer clean2()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess1, err := host.StartShare(ctx, cfg, dir1, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sess1.Close()

	sess2, err := host.StartShare(ctx, cfg, dir2, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()

	mcpEndpoint := fmt.Sprintf("%s/%s/mcp", ts.URL, ident)

	// Conversation 1 connects Share 1
	conn1Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"connect_share","arguments":{"share_url":"%s"}}`, sess1.ShareURL)),
	})
	req1, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(conn1Bytes))
	req1.Header.Set("Authorization", "Bearer "+mcpToken)
	req1.Header.Set("Mcp-Session-Id", "conv-session-1")
	res1, _ := http.DefaultClient.Do(req1)
	res1.Body.Close()

	// Conversation 2 connects Share 2
	conn2Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"connect_share","arguments":{"share_url":"%s"}}`, sess2.ShareURL)),
	})
	req2, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(conn2Bytes))
	req2.Header.Set("Authorization", "Bearer "+mcpToken)
	req2.Header.Set("Mcp-Session-Id", "conv-session-2")
	res2, _ := http.DefaultClient.Do(req2)
	res2.Body.Close()

	// In Conversation 1, read_file reads Share 1 data
	read1Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read_file","arguments":{"path":"src/main.go"}}`),
	})
	r1, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(read1Bytes))
	r1.Header.Set("Authorization", "Bearer "+mcpToken)
	r1.Header.Set("Mcp-Session-Id", "conv-session-1")
	respR1, _ := http.DefaultClient.Do(r1)
	var r1Parsed mcp.Response
	_ = json.NewDecoder(respR1.Body).Decode(&r1Parsed)
	respR1.Body.Close()

	text1 := r1Parsed.Result.(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text1, "CONV_ONE_DATA") {
		t.Errorf("expected Conversation 1 to read CONV_ONE_DATA, got:\n%s", text1)
	}

	// In Conversation 2, read_file reads Share 2 data
	read2Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read_file","arguments":{"path":"src/main.go"}}`),
	})
	r2, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(read2Bytes))
	r2.Header.Set("Authorization", "Bearer "+mcpToken)
	r2.Header.Set("Mcp-Session-Id", "conv-session-2")
	respR2, _ := http.DefaultClient.Do(r2)
	var r2Parsed mcp.Response
	_ = json.NewDecoder(respR2.Body).Decode(&r2Parsed)
	respR2.Body.Close()

	text2 := r2Parsed.Result.(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text2, "CONV_TWO_DATA") {
		t.Errorf("expected Conversation 2 to read CONV_TWO_DATA, got:\n%s", text2)
	}
}

// 5. Host-side capability validation enforcement
func TestHostSideCapabilityValidation(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regBytes, _ := json.Marshal(hub.SetupRequest{Identity: ident, HostToken: hostToken, MCPToken: mcpToken})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regBytes))

	cfg := &config.Config{Identity: ident, HubURL: ts.URL, HostToken: hostToken, MCPToken: mcpToken}

	dir, clean := createTestDirectory(t, "host-defense", "SUPER_SECURE")
	defer clean()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.StartShare(ctx, cfg, dir, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Direct call to /api/inspect with invalid secret
	badInspectPayload := hub.InspectRequest{
		SessionID: sess.SessionID,
		Secret:    "tampered-secret",
		Op:        "read_file",
		Args:      map[string]interface{}{"path": "src/main.go"},
	}
	badData, _ := json.Marshal(badInspectPayload)
	resp, err := http.Post(ts.URL+"/api/inspect", "application/json", bytes.NewReader(badData))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for invalid capability secret, got %d", resp.StatusCode)
	}
}

// 6. Direct browser WebRTC P2P inspection test
func TestBrowserWebRTC_P2PDirect(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regBytes, _ := json.Marshal(hub.SetupRequest{Identity: ident, HostToken: hostToken, MCPToken: mcpToken})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regBytes))

	cfg := &config.Config{Identity: ident, HubURL: ts.URL, HostToken: hostToken, MCPToken: mcpToken}

	dir, clean := createTestDirectory(t, "webrtc-dir", "WEBRTC_PAYLOAD")
	defer clean()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.StartShare(ctx, cfg, dir, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Verify terminal CLI can inspect
	cli, err := client.ParseShareURL(sess.ShareURL)
	if err != nil {
		t.Fatalf("ParseShareURL failed: %v", err)
	}
	readRes, err := cli.ReadFile("src/main.go", 1, 10)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !strings.Contains(readRes.Content, "WEBRTC_PAYLOAD") {
		t.Errorf("expected WEBRTC_PAYLOAD in content, got: %s", readRes.Content)
	}

	// WebRTC direct browser P2P connection test
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

	dc.OnOpen(func() { close(openChan) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) { msgChan <- string(msg.Data) })

	offer, _ := clientPC.CreateOffer(nil)
	gatherComplete := webrtc.GatheringCompletePromise(clientPC)
	_ = clientPC.SetLocalDescription(offer)

	select {
	case <-gatherComplete:
	case <-time.After(2 * time.Second):
	}

	signalURL := fmt.Sprintf("%s/s/%s/signal", ts.URL, sess.SessionID)
	reqPayload := map[string]string{
		"offer_sdp": clientPC.LocalDescription().SDP,
		"secret":    sess.Secret,
	}
	data, _ := json.Marshal(reqPayload)
	sigRes, err := http.Post(signalURL, "application/json", bytes.NewReader(data))
	if err != nil || sigRes.StatusCode != http.StatusOK {
		t.Fatalf("Signaling failed: %v", err)
	}

	var sigResp struct {
		AnswerSDP string `json:"answer_sdp"`
	}
	_ = json.NewDecoder(sigRes.Body).Decode(&sigResp)
	sigRes.Body.Close()

	_ = clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sigResp.AnswerSDP,
	})

	select {
	case <-openChan:
	case <-time.After(5 * time.Second):
		t.Fatalf("Data channel did not open")
	}

	dcReq := map[string]interface{}{
		"id":     "p2p-req-1",
		"action": "read_file",
		"secret": sess.Secret,
		"args":   map[string]interface{}{"path": "src/main.go"},
	}
	dcBytes, _ := json.Marshal(dcReq)
	_ = dc.SendText(string(dcBytes))

	select {
	case respStr := <-msgChan:
		if !strings.Contains(respStr, "WEBRTC_PAYLOAD") {
			t.Errorf("expected WEBRTC_PAYLOAD via data channel, got: %s", respStr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for data channel inspection response")
	}
}

// 7. Test Traversal and Symlink protection
func TestTraversalAndExclusionGuards(t *testing.T) {
	dir, cleanup := createTestDirectory(t, "guards", "GUARD_DATA")
	defer cleanup()

	eng, err := engine.New(dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Path traversal
	_, err = eng.ReadFile("../../../etc/passwd", 0, 0)
	if err != engine.ErrAccessDenied {
		t.Errorf("expected ErrAccessDenied for path traversal, got %v", err)
	}

	// Secret exclusion
	_, err = eng.ReadFile(".env", 0, 0)
	if err != engine.ErrAccessDenied {
		t.Errorf("expected ErrAccessDenied for .env file, got %v", err)
	}

	// Git exclusion
	_, err = eng.ReadFile(".git/config", 0, 0)
	if err != engine.ErrAccessDenied {
		t.Errorf("expected ErrAccessDenied for .git, got %v", err)
	}
}

// 8. Test OAuth 2.1 RFC 8414 metadata discovery, PKCE authorization code flow and MCP token verification
func TestOAuth21_Flow(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regBytes, _ := json.Marshal(hub.SetupRequest{Identity: ident, HostToken: hostToken, MCPToken: mcpToken})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regBytes))

	cfg := &config.Config{Identity: ident, HubURL: ts.URL, HostToken: hostToken, MCPToken: mcpToken}
	dir, cleanDir := createTestDirectory(t, "oauth-test", "OAUTH_PROTECTED_DATA")
	defer cleanDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.StartShare(ctx, cfg, dir, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Step 8a: Verify protected resource metadata
	resMeta, err := http.Get(fmt.Sprintf("%s/%s/.well-known/oauth-protected-resource", ts.URL, ident))
	if err != nil || resMeta.StatusCode != http.StatusOK {
		t.Fatalf("protected-resource metadata failed: status %d, err %v", resMeta.StatusCode, err)
	}
	var metaMap map[string]interface{}
	_ = json.NewDecoder(resMeta.Body).Decode(&metaMap)
	resMeta.Body.Close()
	if !strings.Contains(metaMap["resource"].(string), "/mcp") {
		t.Errorf("unexpected resource metadata: %+v", metaMap)
	}

	// Step 8b: Verify authorization server metadata (RFC 8414)
	resAuthServer, err := http.Get(fmt.Sprintf("%s/%s/.well-known/oauth-authorization-server", ts.URL, ident))
	if err != nil || resAuthServer.StatusCode != http.StatusOK {
		t.Fatalf("authorization-server metadata failed: status %d, err %v", resAuthServer.StatusCode, err)
	}
	var asMap map[string]interface{}
	_ = json.NewDecoder(resAuthServer.Body).Decode(&asMap)
	resAuthServer.Body.Close()
	if asMap["token_endpoint"] == "" || asMap["authorization_endpoint"] == "" {
		t.Errorf("missing endpoints in auth server metadata: %+v", asMap)
	}

	// Step 8c: Initiate OAuth Authorization Code Flow with PKCE (S256)
	codeVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirect
		},
	}

	redirectTarget := "https://chatgpt.com/api/aip/plugin-oauth/callback"
	authURL := fmt.Sprintf("%s/%s/oauth/authorize?response_type=code&client_id=chatgpt-client&redirect_uri=%s&state=state123&code_challenge=%s&code_challenge_method=S256",
		ts.URL, ident, url.QueryEscape(redirectTarget), codeChallenge)

	authResp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	defer authResp.Body.Close()

	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 Found redirect, got %d", authResp.StatusCode)
	}

	loc := authResp.Header.Get("Location")
	parsedLoc, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("failed to parse redirect Location: %v", err)
	}
	authCode := parsedLoc.Query().Get("code")
	if authCode == "" {
		t.Fatalf("no authorization code returned in redirect: %s", loc)
	}

	// Step 8d: Exchange Code for Access Token at token endpoint
	tokenEndpoint := fmt.Sprintf("%s/%s/oauth/token", ts.URL, ident)
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectTarget},
		"client_id":     {"chatgpt-client"},
	}
	tokenRes, err := http.PostForm(tokenEndpoint, tokenForm)
	if err != nil || tokenRes.StatusCode != http.StatusOK {
		t.Fatalf("token exchange failed: status %d, err %v", tokenRes.StatusCode, err)
	}
	var tokenData struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	_ = json.NewDecoder(tokenRes.Body).Decode(&tokenData)
	tokenRes.Body.Close()

	if tokenData.AccessToken == "" || tokenData.TokenType != "Bearer" {
		t.Fatalf("invalid token response: %+v", tokenData)
	}

	// Step 8e: Use newly issued OAuth access token to query /<ident>/mcp
	mcpEndpoint := fmt.Sprintf("%s/%s/mcp", ts.URL, ident)

	// Connect share using OAuth token
	connReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"connect_share","arguments":{"share_url":"%s"}}`, sess.ShareURL)),
	}
	connBytes, _ := json.Marshal(connReq)
	req, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(connBytes))
	req.Header.Set("Authorization", "Bearer "+tokenData.AccessToken)
	req.Header.Set("Mcp-Session-Id", "oauth-session-1")
	cRes, err := http.DefaultClient.Do(req)
	if err != nil || cRes.StatusCode != http.StatusOK {
		t.Fatalf("MCP tool call with OAuth token failed: status %d, err %v", cRes.StatusCode, err)
	}
	var cRPC mcp.Response
	_ = json.NewDecoder(cRes.Body).Decode(&cRPC)
	cRes.Body.Close()
	if cRPC.Error != nil {
		t.Fatalf("connect_share error: %+v", cRPC.Error)
	}

	// Read file using OAuth token
	readReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","arguments":{"share_id":"%s","path":"src/main.go"}}`, sess.SessionID)),
	}
	readBytes, _ := json.Marshal(readReq)
	req, _ = http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(readBytes))
	req.Header.Set("Authorization", "Bearer "+tokenData.AccessToken)
	req.Header.Set("Mcp-Session-Id", "oauth-session-1")
	rRes, err := http.DefaultClient.Do(req)
	if err != nil || rRes.StatusCode != http.StatusOK {
		t.Fatalf("read_file with OAuth token failed: %v", err)
	}
	var rRPC mcp.Response
	_ = json.NewDecoder(rRes.Body).Decode(&rRPC)
	rRes.Body.Close()
	if rRPC.Error != nil {
		t.Fatalf("read_file error: %+v", rRPC.Error)
	}
	text := rRPC.Result.(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "OAUTH_PROTECTED_DATA") {
		t.Fatalf("expected OAUTH_PROTECTED_DATA, got:\n%s", text)
	}

	// Step 8f: Unauthorized token rejected with 401 and resource_metadata challenge
	reqBad, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(readBytes))
	reqBad.Header.Set("Authorization", "Bearer invalid-oauth-token")
	badRes, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatal(err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for bad token, got %d", badRes.StatusCode)
	}
	challenge := badRes.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, "resource_metadata") {
		t.Errorf("expected WWW-Authenticate to contain resource_metadata, got: %s", challenge)
	}
}

// Ensure unused import warning prevention
var _ = sha256.New
var _ = hex.EncodeToString
