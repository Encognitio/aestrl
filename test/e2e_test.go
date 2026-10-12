package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

	// Step 3: ChatGPT reads source file via read_file tool with explicit share_id
	readReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "read_file",
			"arguments": {
				"share_id": "%s",
				"path": "src/main.go",
				"start_line": 1,
				"end_line": 10
			}
		}`, sess.SessionID)),
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

	// In Conversation 1, read_file reads Share 1 data with explicit share_id
	read1Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","arguments":{"share_id":"%s","path":"src/main.go"}}`, sess1.SessionID)),
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

	// In Conversation 2, read_file reads Share 2 data with explicit share_id
	read2Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","arguments":{"share_id":"%s","path":"src/main.go"}}`, sess2.SessionID)),
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

	// Negative Check: Conversation 1 CANNOT read Share 2 data
	cross1Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      5,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","arguments":{"share_id":"%s","path":"src/main.go"}}`, sess2.SessionID)),
	})
	rCross1, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(cross1Bytes))
	rCross1.Header.Set("Authorization", "Bearer "+mcpToken)
	rCross1.Header.Set("Mcp-Session-Id", "conv-session-1")
	respCross1, _ := http.DefaultClient.Do(rCross1)
	var cross1Parsed mcp.Response
	_ = json.NewDecoder(respCross1.Body).Decode(&cross1Parsed)
	respCross1.Body.Close()

	if cross1Parsed.Result != nil {
		resMap := cross1Parsed.Result.(map[string]interface{})
		if isErr, ok := resMap["isError"].(bool); !ok || !isErr {
			t.Errorf("expected isError: true when Conversation 1 attempts reading Share 2, got: %+v", resMap)
		}
	} else if cross1Parsed.Error == nil {
		t.Errorf("expected error when Conversation 1 attempts reading Share 2, got success")
	}

	// Negative Check: Conversation 2 CANNOT read Share 1 data
	cross2Bytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      6,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","arguments":{"share_id":"%s","path":"src/main.go"}}`, sess1.SessionID)),
	})
	rCross2, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(cross2Bytes))
	rCross2.Header.Set("Authorization", "Bearer "+mcpToken)
	rCross2.Header.Set("Mcp-Session-Id", "conv-session-2")
	respCross2, _ := http.DefaultClient.Do(rCross2)
	var cross2Parsed mcp.Response
	_ = json.NewDecoder(respCross2.Body).Decode(&cross2Parsed)
	respCross2.Body.Close()

	if cross2Parsed.Result != nil {
		resMap := cross2Parsed.Result.(map[string]interface{})
		if isErr, ok := resMap["isError"].(bool); !ok || !isErr {
			t.Errorf("expected isError: true when Conversation 2 attempts reading Share 1, got: %+v", resMap)
		}
	} else if cross2Parsed.Error == nil {
		t.Errorf("expected error when Conversation 2 attempts reading Share 1, got success")
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

	// Step 8c: Dynamic Client Registration (RFC 7591)
	regURL := fmt.Sprintf("%s/%s/oauth/register", ts.URL, ident)
	redirectTarget := "https://chatgpt.com/api/aip/plugin-oauth/callback"
	regBody, _ := json.Marshal(map[string]interface{}{
		"client_name":   "ChatGPT Plugin",
		"redirect_uris": []string{redirectTarget},
	})
	regRes, err := http.Post(regURL, "application/json", bytes.NewReader(regBody))
	if err != nil || regRes.StatusCode != http.StatusCreated {
		t.Fatalf("dynamic client registration failed: status %d, err %v", regRes.StatusCode, err)
	}
	var regData struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(regRes.Body).Decode(&regData)
	regRes.Body.Close()
	if regData.ClientID == "" {
		t.Fatalf("empty client_id returned from DCR")
	}

	// Step 8d: Initiate OAuth Authorization Code Flow with PKCE (S256)
	codeVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirect
		},
	}

	authEndpoint := fmt.Sprintf("%s/%s/oauth/authorize", ts.URL, ident)
	authQuery := fmt.Sprintf("?response_type=code&client_id=%s&redirect_uri=%s&state=state123&code_challenge=%s&code_challenge_method=S256",
		regData.ClientID, url.QueryEscape(redirectTarget), codeChallenge)

	// Step 8e: Unauthenticated GET must show consent form (200 OK), NOT auto-approve
	getResp, err := client.Get(authEndpoint + authQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK consent form for unauthenticated GET, got %d", getResp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(getResp.Body)
	if !strings.Contains(string(bodyBytes), "Authorize MCP Client") {
		t.Fatalf("expected consent form HTML, got:\n%s", string(bodyBytes))
	}

	// Step 8f: Identity owner approves with their persistent credentials from setup (mcpToken)
	approveForm := url.Values{
		"response_type":         {"code"},
		"client_id":             {regData.ClientID},
		"redirect_uri":          {redirectTarget},
		"state":                 {"state123"},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"identity_token":        {mcpToken},
		"action":                {"approve"},
	}
	postResp, err := client.PostForm(authEndpoint, approveForm)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 Found redirect after approval, got %d", postResp.StatusCode)
	}
	loc := postResp.Header.Get("Location")
	parsedLoc, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("failed to parse redirect Location: %v", err)
	}
	authCode := parsedLoc.Query().Get("code")
	if authCode == "" {
		t.Fatalf("no authorization code returned in redirect: %s", loc)
	}

	// Step 8g: Exchange Code for Access Token at token endpoint with PKCE verifier
	tokenEndpoint := fmt.Sprintf("%s/%s/oauth/token", ts.URL, ident)
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectTarget},
		"client_id":     {regData.ClientID},
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

	// Step 8h: Use newly issued OAuth access token to query /<ident>/mcp
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

	// Read file using OAuth token with explicit share_id
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

	// Step 8i: Unauthorized token rejected with 401 and resource_metadata challenge
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

// 9. Negative tests proving unauthorized OAuth requests are rejected
func TestOAuth_StrictValidationAndNegativeCases(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regBytes, _ := json.Marshal(hub.SetupRequest{Identity: ident, HostToken: hostToken, MCPToken: mcpToken})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regBytes))

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	authEndpoint := fmt.Sprintf("%s/%s/oauth/authorize", ts.URL, ident)
	tokenEndpoint := fmt.Sprintf("%s/%s/oauth/token", ts.URL, ident)

	codeVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	// Case 1: Untrusted / unregistered client_id -> 400 Bad Request
	res1, _ := client.Get(authEndpoint + "?client_id=evil-client&redirect_uri=https://evil.com/callback&response_type=code&code_challenge=" + codeChallenge + "&code_challenge_method=S256")
	if res1.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for untrusted client_id, got %d", res1.StatusCode)
	}
	res1.Body.Close()

	// Case 2: Untrusted redirect URI for predefined client (chatgpt client targeting attacker domain) -> 400
	res2, _ := client.Get(authEndpoint + "?client_id=chatgpt&redirect_uri=https://attacker.org/callback&response_type=code&code_challenge=" + codeChallenge + "&code_challenge_method=S256")
	if res2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for untrusted redirect URI, got %d", res2.StatusCode)
	}
	res2.Body.Close()

	// Case 3: Redirect URI containing fragment -> 400
	res3, _ := client.Get(authEndpoint + "?client_id=chatgpt&redirect_uri=https://chatgpt.com/callback#fragment&response_type=code&code_challenge=" + codeChallenge + "&code_challenge_method=S256")
	if res3.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for redirect URI with fragment, got %d", res3.StatusCode)
	}
	res3.Body.Close()

	// Case 4: Missing code_challenge -> 400
	res4, _ := client.Get(authEndpoint + "?client_id=chatgpt&redirect_uri=https://chatgpt.com/callback&response_type=code&code_challenge_method=S256")
	if res4.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing code_challenge, got %d", res4.StatusCode)
	}
	res4.Body.Close()

	// Case 5: Plain PKCE method (forbidden in OAuth 2.1) -> 400
	res5, _ := client.Get(authEndpoint + "?client_id=chatgpt&redirect_uri=https://chatgpt.com/callback&response_type=code&code_challenge=" + codeChallenge + "&code_challenge_method=plain")
	if res5.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for plain PKCE, got %d", res5.StatusCode)
	}
	res5.Body.Close()

	// Case 6: Invalid resource audience -> 400
	res6, _ := client.Get(authEndpoint + "?client_id=chatgpt&redirect_uri=https://chatgpt.com/callback&response_type=code&code_challenge=" + codeChallenge + "&code_challenge_method=S256&resource=https://other-domain/mcp")
	if res6.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for mismatched resource audience, got %d", res6.StatusCode)
	}
	res6.Body.Close()

	// Case 7: Invalid scope -> 400
	res7, _ := client.Get(authEndpoint + "?client_id=chatgpt&redirect_uri=https://chatgpt.com/callback&response_type=code&code_challenge=" + codeChallenge + "&code_challenge_method=S256&scope=admin_super")
	if res7.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid scope, got %d", res7.StatusCode)
	}
	res7.Body.Close()

	// Case 8: POST authorization with invalid identity credentials -> 401 Unauthorized
	postFormBad := url.Values{
		"response_type":         {"code"},
		"client_id":             {"chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/callback"},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"identity_token":        {"wrong-identity-token"},
		"action":                {"approve"},
	}
	res8, _ := client.PostForm(authEndpoint, postFormBad)
	if res8.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for invalid identity credentials, got %d", res8.StatusCode)
	}
	res8.Body.Close()

	// Case 9: Identity owner explicitly denies authorization -> 302 redirect with error=access_denied
	postFormDeny := url.Values{
		"response_type":         {"code"},
		"client_id":             {"chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/callback"},
		"state":                 {"state-deny"},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"identity_token":        {mcpToken},
		"action":                {"deny"},
	}
	res9, _ := client.PostForm(authEndpoint, postFormDeny)
	if res9.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 Found redirect for deny, got %d", res9.StatusCode)
	}
	denyLoc, _ := url.Parse(res9.Header.Get("Location"))
	if denyLoc.Query().Get("error") != "access_denied" {
		t.Errorf("expected error=access_denied, got: %s", denyLoc.Query().Get("error"))
	}
	res9.Body.Close()

	// Case 10: Valid authorization followed by invalid PKCE verifier at token endpoint -> 400 invalid_grant
	postFormOK := url.Values{
		"response_type":         {"code"},
		"client_id":             {"chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/callback"},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"identity_token":        {mcpToken},
		"action":                {"approve"},
	}
	res10, _ := client.PostForm(authEndpoint, postFormOK)
	locOK, _ := url.Parse(res10.Header.Get("Location"))
	res10.Body.Close()
	validCode := locOK.Query().Get("code")

	// Exchange with wrong verifier
	badTokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {validCode},
		"code_verifier": {"wrong-code-verifier-that-fails-s256"},
		"redirect_uri":  {"https://chatgpt.com/callback"},
		"client_id":     {"chatgpt"},
	}
	resTokenBad, _ := http.PostForm(tokenEndpoint, badTokenForm)
	if resTokenBad.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for bad code_verifier, got %d", resTokenBad.StatusCode)
	}
	resTokenBad.Body.Close()

	// Reusing the same code must fail (single-use)
	resTokenReuse, _ := http.PostForm(tokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {validCode},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {"https://chatgpt.com/callback"},
		"client_id":     {"chatgpt"},
	})
	if resTokenReuse.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for reused code, got %d", resTokenReuse.StatusCode)
	}
	resTokenReuse.Body.Close()
}

// 10. Negative tests proving cross-session file reads and unauthorized shares are strictly rejected
func TestCrossSession_StrictIsolationAndNegativeCases(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regBytes, _ := json.Marshal(hub.SetupRequest{Identity: ident, HostToken: hostToken, MCPToken: mcpToken})
	_, _ = http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regBytes))

	cfg := &config.Config{Identity: ident, HubURL: ts.URL, HostToken: hostToken, MCPToken: mcpToken}
	dir1, clean1 := createTestDirectory(t, "sess-a-dir", "SECRET_A_PAYLOAD")
	defer clean1()
	dir2, clean2 := createTestDirectory(t, "sess-b-dir", "SECRET_B_PAYLOAD")
	defer clean2()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	share1, err := host.StartShare(ctx, cfg, dir1, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer share1.Close()

	share2, err := host.StartShare(ctx, cfg, dir2, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer share2.Close()

	mcpEndpoint := fmt.Sprintf("%s/%s/mcp", ts.URL, ident)

	// Session Alpha authorizes Share 1
	connABool, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"connect_share","arguments":{"share_url":"%s"}}`, share1.ShareURL)),
	})
	reqA, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(connABool))
	reqA.Header.Set("Authorization", "Bearer "+mcpToken)
	reqA.Header.Set("Mcp-Session-Id", "session-alpha")
	resA, _ := http.DefaultClient.Do(reqA)
	resA.Body.Close()

	// Session Beta (never called connect_share) attempts to read Share 1 with share_id -> MUST BE REJECTED
	readBBytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  json.RawMessage(fmt.Sprintf(`{"name":"read_file","arguments":{"share_id":"%s","path":"src/main.go"}}`, share1.SessionID)),
	})
	reqB, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(readBBytes))
	reqB.Header.Set("Authorization", "Bearer "+mcpToken)
	reqB.Header.Set("Mcp-Session-Id", "session-beta")
	resB, _ := http.DefaultClient.Do(reqB)
	var respB mcp.Response
	_ = json.NewDecoder(resB.Body).Decode(&respB)
	resB.Body.Close()

	if respB.Result != nil {
		resMap := respB.Result.(map[string]interface{})
		if isErr, ok := resMap["isError"].(bool); !ok || !isErr {
			t.Errorf("expected isError: true when session-beta reads share without authorizing, got: %+v", resMap)
		}
	} else if respB.Error == nil {
		t.Errorf("expected error when session-beta reads unauthorized share, got success")
	}

	// Session Beta attempts list_shares -> Share 1 must NOT be in the list
	listBBytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"list_shares","arguments":{}}`),
	})
	reqListB, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(listBBytes))
	reqListB.Header.Set("Authorization", "Bearer "+mcpToken)
	reqListB.Header.Set("Mcp-Session-Id", "session-beta")
	resListB, _ := http.DefaultClient.Do(reqListB)
	var respListB mcp.Response
	_ = json.NewDecoder(resListB.Body).Decode(&respListB)
	resListB.Body.Close()

	listBText := respListB.Result.(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if strings.Contains(listBText, share1.SessionID) {
		t.Errorf("expected Share 1 not to appear in session-beta list_shares, got: %s", listBText)
	}

	// Inspection operation without share_id parameter -> MUST BE REJECTED
	noShareIDBytes, _ := json.Marshal(mcp.Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"read_file","arguments":{"path":"src/main.go"}}`),
	})
	reqNoID, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(noShareIDBytes))
	reqNoID.Header.Set("Authorization", "Bearer "+mcpToken)
	reqNoID.Header.Set("Mcp-Session-Id", "session-alpha")
	resNoID, _ := http.DefaultClient.Do(reqNoID)
	var respNoID mcp.Response
	_ = json.NewDecoder(resNoID.Body).Decode(&respNoID)
	resNoID.Body.Close()

	if respNoID.Result != nil {
		resMap := respNoID.Result.(map[string]interface{})
		if isErr, ok := resMap["isError"].(bool); !ok || !isErr {
			t.Errorf("expected isError: true for missing share_id, got: %+v", resMap)
		}
	} else if respNoID.Error == nil {
		t.Errorf("expected error for missing share_id, got success")
	}
}

// TestChatGPT_SessionContinuityAcrossSeparateRequests verifies that ChatGPT's stateless
// HTTP tool calls (without Mcp-Session-Id header) maintain session continuity and authorization
// across separate HTTP requests, while isolating unrelated clients.
func TestChatGPT_SessionContinuityAcrossSeparateRequests(t *testing.T) {
	_, ts, cleanup := startTestHub(t)
	defer cleanup()

	ident := config.GenerateIdentityID()
	hostToken, _ := config.GenerateSecureToken(16)
	mcpToken, _ := config.GenerateSecureToken(16)

	regJSON, _ := json.Marshal(hub.SetupRequest{
		Identity:  ident,
		HostToken: hostToken,
		MCPToken:  mcpToken,
	})
	resSetup, err := http.Post(ts.URL+"/api/setup", "application/json", bytes.NewReader(regJSON))
	if err != nil || resSetup.StatusCode != http.StatusOK {
		t.Fatalf("setup failed: %v", err)
	}
	resSetup.Body.Close()

	testDir, cleanDir := createTestDirectory(t, "chatgpt-continuity", "CHATGPT_CONTINUITY_SUCCESS")
	defer cleanDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{
		Identity:  ident,
		HubURL:    ts.URL,
		HostToken: hostToken,
		MCPToken:  mcpToken,
	}

	sess, err := host.StartShare(ctx, cfg, testDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("StartShare failed: %v", err)
	}
	defer sess.Close()

	mcpEndpoint := fmt.Sprintf("%s/%s/mcp", ts.URL, ident)

	// Step 1: ChatGPT tool call 1 - connect_share WITHOUT Mcp-Session-Id header
	connReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "connect_share",
			"arguments": {
				"share_url": "%s"
			}
		}`, sess.ShareURL)),
	}
	connBytes, _ := json.Marshal(connReq)
	httpReq1, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(connBytes))
	httpReq1.Header.Set("Authorization", "Bearer "+mcpToken)
	// Notice: NO Mcp-Session-Id header (stateless ChatGPT tool runner)

	connRes, err := http.DefaultClient.Do(httpReq1)
	if err != nil || connRes.StatusCode != http.StatusOK {
		t.Fatalf("connect_share HTTP failed: %v", err)
	}
	var connRPCResp mcp.Response
	_ = json.NewDecoder(connRes.Body).Decode(&connRPCResp)
	connRes.Body.Close()

	if connRPCResp.Error != nil {
		t.Fatalf("connect_share RPC error: %+v", connRPCResp.Error)
	}
	connResult, ok := connRPCResp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("invalid connect_share result: %+v", connRPCResp.Result)
	}
	contentList := connResult["content"].([]interface{})
	var parsedConn map[string]interface{}
	if err := json.Unmarshal([]byte(contentList[0].(map[string]interface{})["text"].(string)), &parsedConn); err != nil {
		t.Fatalf("failed to parse connect_share content JSON: %v", err)
	}
	shareID, _ := parsedConn["share_id"].(string)
	if shareID == "" {
		t.Fatalf("expected share_id in result, got: %+v", parsedConn)
	}

	// Step 2: ChatGPT tool call 2 - list_directory in a separate HTTP request, WITHOUT Mcp-Session-Id header
	listReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "list_directory",
			"arguments": {
				"share_id": "%s",
				"path": ""
			}
		}`, shareID)),
	}
	listBytes, _ := json.Marshal(listReq)
	httpReq2, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(listBytes))
	httpReq2.Header.Set("Authorization", "Bearer "+mcpToken)
	// Notice: NO Mcp-Session-Id header

	listRes, err := http.DefaultClient.Do(httpReq2)
	if err != nil || listRes.StatusCode != http.StatusOK {
		t.Fatalf("list_directory HTTP failed: %v", err)
	}
	var listRPCResp mcp.Response
	_ = json.NewDecoder(listRes.Body).Decode(&listRPCResp)
	listRes.Body.Close()

	if listRPCResp.Error != nil {
		t.Fatalf("list_directory RPC error: %+v", listRPCResp.Error)
	}
	listResult, ok := listRPCResp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("invalid list_directory result: %+v", listRPCResp.Result)
	}
	if isErr, ok := listResult["isError"].(bool); ok && isErr {
		t.Fatalf("list_directory returned error result: %+v", listResult)
	}

	// Step 3: ChatGPT tool call 3 - read_file in a separate HTTP request, WITHOUT Mcp-Session-Id header
	readReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "read_file",
			"arguments": {
				"share_id": "%s",
				"path": "src/main.go"
			}
		}`, shareID)),
	}
	readBytes, _ := json.Marshal(readReq)
	httpReq3, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(readBytes))
	httpReq3.Header.Set("Authorization", "Bearer "+mcpToken)
	// Notice: NO Mcp-Session-Id header

	readRes, err := http.DefaultClient.Do(httpReq3)
	if err != nil || readRes.StatusCode != http.StatusOK {
		t.Fatalf("read_file HTTP failed: %v", err)
	}
	var readRPCResp mcp.Response
	_ = json.NewDecoder(readRes.Body).Decode(&readRPCResp)
	readRes.Body.Close()

	if readRPCResp.Error != nil {
		t.Fatalf("read_file RPC error: %+v", readRPCResp.Error)
	}
	readResult, ok := readRPCResp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("invalid read_file result: %+v", readRPCResp.Result)
	}
	if isErr, ok := readResult["isError"].(bool); ok && isErr {
		t.Fatalf("read_file returned error result: %+v", readResult)
	}
	contentList = readResult["content"].([]interface{})
	readText := contentList[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(readText, "CHATGPT_CONTINUITY_SUCCESS") {
		t.Fatalf("expected file content to contain CHATGPT_CONTINUITY_SUCCESS, got:\n%s", readText)
	}

	// Step 4: Negative check - An unrelated client with a different token cannot access this share
	unrelatedToken, _ := config.GenerateSecureToken(16)
	unrelatedReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params: json.RawMessage(fmt.Sprintf(`{
			"name": "read_file",
			"arguments": {
				"share_id": "%s",
				"path": "src/main.go"
			}
		}`, shareID)),
	}
	unrelatedBytes, _ := json.Marshal(unrelatedReq)
	httpReq4, _ := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewReader(unrelatedBytes))
	httpReq4.Header.Set("Authorization", "Bearer "+unrelatedToken)

	unrelatedRes, err := http.DefaultClient.Do(httpReq4)
	if err != nil {
		t.Fatalf("unrelated request failed: %v", err)
	}
	defer unrelatedRes.Body.Close()
	// Unrelated token must be rejected with 401 Unauthorized
	if unrelatedRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for unrelated client token, got: %d", unrelatedRes.StatusCode)
	}
}

var _ = hex.EncodeToString

