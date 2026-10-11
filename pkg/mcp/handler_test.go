package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type mockExecutor struct{}

func (m *mockExecutor) ConnectShare(sessionID, shareURL string) (interface{}, error) {
	return map[string]string{"status": "connected", "share_url": shareURL, "session_id": sessionID}, nil
}
func (m *mockExecutor) ListShares(sessionID string) (interface{}, error) {
	return []string{"session-123"}, nil
}
func (m *mockExecutor) ListDirectory(sessionID, shareID, path string) (interface{}, error) {
	return []map[string]interface{}{{"name": "test.go", "is_dir": false}}, nil
}
func (m *mockExecutor) Tree(sessionID, shareID, path string, depth int) (interface{}, error) {
	return map[string]string{"tree": "sample"}, nil
}
func (m *mockExecutor) SearchFiles(sessionID, shareID, query, path string) (interface{}, error) {
	return []string{"test.go"}, nil
}
func (m *mockExecutor) SearchContent(sessionID, shareID, query, path string, isRegex bool) (interface{}, error) {
	return []string{"match line"}, nil
}
func (m *mockExecutor) ReadFile(sessionID, shareID, path string, startLine, endLine int) (interface{}, error) {
	return map[string]string{"content": "package main"}, nil
}
func (m *mockExecutor) FileInfo(sessionID, shareID, path string) (interface{}, error) {
	return map[string]string{"name": "test.go"}, nil
}
func (m *mockExecutor) GetFile(sessionID, shareID, path string) (interface{}, error) {
	return map[string]string{"data": "content"}, nil
}

func TestMCP_InitializeAndTools(t *testing.T) {
	handler := NewHandler("secret-mcp-token", &mockExecutor{})

	// 1. Test unauthorized request returns 401 and WWW-Authenticate
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`
	req := httptest.NewRequest(http.MethodPost, "/h7af8ven8/mcp", bytes.NewBufferString(reqBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without Bearer token, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("expected WWW-Authenticate header on 401, got empty")
	}

	// 2. Test authorized initialize with Mcp-Session-Id header
	req = httptest.NewRequest(http.MethodPost, "/h7af8ven8/mcp", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer secret-mcp-token")
	req.Header.Set("Mcp-Session-Id", "conv-session-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Mcp-Session-Id") != "conv-session-1" {
		t.Fatalf("expected Mcp-Session-Id 'conv-session-1', got '%s'", rec.Header().Get("Mcp-Session-Id"))
	}

	var initResp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", initResp.Error)
	}

	// 3. Test tools/list
	toolsReqBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	req = httptest.NewRequest(http.MethodPost, "/h7af8ven8/mcp", bytes.NewBufferString(toolsReqBody))
	req.Header.Set("Authorization", "Bearer secret-mcp-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var toolsResp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &toolsResp); err != nil {
		t.Fatalf("failed to decode tools response: %v", err)
	}
	resultMap, ok := toolsResp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map result, got %T", toolsResp.Result)
	}
	toolsList, ok := resultMap["tools"].([]interface{})
	if !ok || len(toolsList) == 0 {
		t.Fatalf("expected tools list, got %+v", resultMap)
	}

	// 4. Test tools/call (connect_share)
	callReqBody := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"connect_share","arguments":{"share_url":"https://aestrl.dvrsfy.org/s/abc#123"}}}`
	req = httptest.NewRequest(http.MethodPost, "/h7af8ven8/mcp", bytes.NewBufferString(callReqBody))
	req.Header.Set("Authorization", "Bearer secret-mcp-token")
	req.Header.Set("Mcp-Session-Id", "conv-session-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var callResp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &callResp); err != nil {
		t.Fatalf("failed to decode call response: %v", err)
	}
	if callResp.Error != nil {
		t.Fatalf("tool call error: %+v", callResp.Error)
	}
}
