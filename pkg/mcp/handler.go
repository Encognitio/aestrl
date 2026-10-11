package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Executor defines the operations needed by MCP tools.
type Executor interface {
	ConnectShare(shareURL string) (interface{}, error)
	ListShares() (interface{}, error)
	ListDirectory(shareID, path string) (interface{}, error)
	Tree(shareID, path string, depth int) (interface{}, error)
	SearchFiles(shareID, query, path string) (interface{}, error)
	SearchContent(shareID, query, path string, isRegex bool) (interface{}, error)
	ReadFile(shareID, path string, startLine, endLine int) (interface{}, error)
	FileInfo(shareID, path string) (interface{}, error)
	GetFile(shareID, path string) (interface{}, error)
}

// Handler handles Streamable HTTP and SSE MCP requests.
type Handler struct {
	expectedToken string
	executor      Executor
}

// NewHandler creates a new MCP Handler.
func NewHandler(expectedToken string, executor Executor) *Handler {
	return &Handler{
		expectedToken: expectedToken,
		executor:      executor,
	}
}

// ServeHTTP implements the HTTP transport for MCP (Streamable HTTP / SSE).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Enable CORS for web-based MCP clients (ChatGPT web, etc.)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Authenticate Bearer token if token is configured
	if h.expectedToken != "" {
		authHeader := r.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			token = strings.TrimSpace(authHeader[7:])
		} else if qToken := r.URL.Query().Get("token"); qToken != "" {
			token = qToken
		}

		if token != h.expectedToken {
			writeRPCError(w, nil, CodeUnauthorized, "Unauthorized: invalid or missing MCP token", http.StatusUnauthorized)
			return
		}
	}

	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			writeRPCError(w, nil, CodeParseError, "Failed to read request body", http.StatusBadRequest)
			return
		}

		var req Request
		if err := json.Unmarshal(body, &req); err != nil {
			writeRPCError(w, nil, CodeParseError, "Invalid JSON-RPC request", http.StatusBadRequest)
			return
		}

		resp := h.handleRequest(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)

	case http.MethodGet:
		// Handle SSE or info endpoint
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || strings.HasSuffix(r.URL.Path, "/sse") {
			h.handleSSE(w, r)
			return
		}

		// Simple status check
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"service": "aestrl-mcp",
			"version": "1.0.0",
		})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleRequest(req *Request) *Response {
	resp := &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	switch req.Method {
	case "initialize":
		var params InitializeParams
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		protoVer := params.ProtocolVersion
		if protoVer == "" {
			protoVer = "2024-11-05"
		}

		resp.Result = InitializeResult{
			ProtocolVersion: protoVer,
			Capabilities: ServerCapabilities{
				Tools: map[string]interface{}{
					"listChanged": false,
				},
			},
			ServerInfo: ServerInfo{
				Name:    "aestrl",
				Version: "1.0.0",
			},
		}

	case "notifications/initialized":
		// No response required for notification, or empty result
		resp.Result = map[string]interface{}{}

	case "ping":
		resp.Result = map[string]interface{}{}

	case "tools/list":
		resp.Result = ToolsListResult{
			Tools: AllTools(),
		}

	case "tools/call":
		var callParams CallToolParams
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			resp.Error = &RPCError{
				Code:    CodeInvalidParams,
				Message: "Invalid tool call parameters",
			}
			return resp
		}

		toolResult := h.executeTool(callParams.Name, callParams.Arguments)
		resp.Result = toolResult

	default:
		resp.Error = &RPCError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("Method '%s' not found", req.Method),
		}
	}

	return resp
}

func (h *Handler) executeTool(name string, args map[string]interface{}) CallToolResult {
	if h.executor == nil {
		return formatError(fmt.Errorf("no executor attached"))
	}

	shareID := getString(args, "share_id")

	switch name {
	case "connect_share":
		url := getString(args, "share_url")
		if url == "" {
			return formatError(fmt.Errorf("missing required 'share_url' argument"))
		}
		res, err := h.executor.ConnectShare(url)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "list_shares":
		res, err := h.executor.ListShares()
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "list_directory":
		path := getString(args, "path")
		res, err := h.executor.ListDirectory(shareID, path)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "tree":
		path := getString(args, "path")
		depth := getInt(args, "depth", 3)
		res, err := h.executor.Tree(shareID, path, depth)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "search_files":
		query := getString(args, "query")
		if query == "" {
			return formatError(fmt.Errorf("missing required 'query' argument"))
		}
		path := getString(args, "path")
		res, err := h.executor.SearchFiles(shareID, query, path)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "search_content":
		query := getString(args, "query")
		if query == "" {
			return formatError(fmt.Errorf("missing required 'query' argument"))
		}
		path := getString(args, "path")
		isRegex := getBool(args, "is_regex", false)
		res, err := h.executor.SearchContent(shareID, query, path, isRegex)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "read_file":
		path := getString(args, "path")
		if path == "" {
			return formatError(fmt.Errorf("missing required 'path' argument"))
		}
		startLine := getInt(args, "start_line", 0)
		endLine := getInt(args, "end_line", 0)
		res, err := h.executor.ReadFile(shareID, path, startLine, endLine)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "file_info":
		path := getString(args, "path")
		if path == "" {
			return formatError(fmt.Errorf("missing required 'path' argument"))
		}
		res, err := h.executor.FileInfo(shareID, path)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	case "get_file":
		path := getString(args, "path")
		if path == "" {
			return formatError(fmt.Errorf("missing required 'path' argument"))
		}
		res, err := h.executor.GetFile(shareID, path)
		if err != nil {
			return formatError(err)
		}
		return formatSuccess(res)

	default:
		return formatError(fmt.Errorf("unknown tool: %s", name))
	}
}

func (h *Handler) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send endpoint event with session address
	_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", r.URL.Path)
	flusher.Flush()

	// Keep stream alive until client disconnects
	<-r.Context().Done()
}

func formatSuccess(data interface{}) CallToolResult {
	var text string
	switch v := data.(type) {
	case string:
		text = v
	default:
		bytes, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			text = fmt.Sprintf("%v", data)
		} else {
			text = string(bytes)
		}
	}
	return CallToolResult{
		Content: []ContentItem{
			{
				Type: "text",
				Text: text,
			},
		},
		IsError: false,
	}
}

func formatError(err error) CallToolResult {
	return CallToolResult{
		Content: []ContentItem{
			{
				Type: "text",
				Text: fmt.Sprintf("Error: %v", err),
			},
		},
		IsError: true,
	}
}

func writeRPCError(w http.ResponseWriter, id interface{}, code int, message string, httpStatus int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		if s, ok := val.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]interface{}, key string, def int) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return def
}

func getBool(m map[string]interface{}, key string, def bool) bool {
	if val, ok := m[key]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return def
}
