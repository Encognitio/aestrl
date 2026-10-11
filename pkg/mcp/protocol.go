package mcp

import (
	"encoding/json"
)

// JSON-RPC 2.0 primitives
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeUnauthorized   = -32000
)

// MCP Protocol structures
type InitializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      *ClientInfo     `json:"clientInfo,omitempty"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerCapabilities struct {
	Tools map[string]interface{} `json:"tools"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]PropertyDef `json:"properties"`
	Required   []string               `json:"required,omitempty"`
}

type PropertyDef struct {
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Default     interface{} `json:"default,omitempty"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

type CallToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type CallToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// AllTools returns the complete set of Aestrl MCP read-only inspection tools.
func AllTools() []Tool {
	return []Tool{
		{
			Name:        "connect_share",
			Description: "Connect and authorize an Aestrl temporary share URL (https://.../s/<session>#<secret>) for inspection. Returns the share_id required for all subsequent inspection tool calls.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_url": {
						Type:        "string",
						Description: "The full temporary Aestrl share URL pasted by the user.",
					},
				},
				Required: []string{"share_url"},
			},
		},
		{
			Name:        "list_shares",
			Description: "List all currently connected active shares under this persistent identity.",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]PropertyDef{},
			},
		},
		{
			Name:        "list_directory",
			Description: "List files and directories at the specified path within a shared directory.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"path": {
						Type:        "string",
						Description: "Relative directory path (e.g. '' or 'src'). Defaults to root.",
					},
				},
				Required: []string{"share_id"},
			},
		},
		{
			Name:        "tree",
			Description: "Display recursive directory structure as a visual tree with bounded depth.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"path": {
						Type:        "string",
						Description: "Relative directory path to start from. Defaults to root.",
					},
					"depth": {
						Type:        "integer",
						Description: "Maximum depth to recurse (1 to 10). Defaults to 3.",
					},
				},
				Required: []string{"share_id"},
			},
		},
		{
			Name:        "search_files",
			Description: "Search for files by name using substring or glob pattern matching.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"query": {
						Type:        "string",
						Description: "Filename or pattern to search for (e.g. '*.go' or 'config').",
					},
					"path": {
						Type:        "string",
						Description: "Relative directory to search within. Defaults to root.",
					},
				},
				Required: []string{"share_id", "query"},
			},
		},
		{
			Name:        "search_content",
			Description: "Search file contents for literal text or regex patterns, returning matching lines and context.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"query": {
						Type:        "string",
						Description: "Text string or regex pattern to search for.",
					},
					"path": {
						Type:        "string",
						Description: "Relative directory to search within. Defaults to root.",
					},
					"is_regex": {
						Type:        "boolean",
						Description: "Set to true if query is a regular expression. Defaults to false.",
					},
				},
				Required: []string{"share_id", "query"},
			},
		},
		{
			Name:        "read_file",
			Description: "Read the full contents or a specific line range of a text file within bounded limits.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"path": {
						Type:        "string",
						Description: "Relative path of the file to read.",
					},
					"start_line": {
						Type:        "integer",
						Description: "Optional starting line number (1-indexed).",
					},
					"end_line": {
						Type:        "integer",
						Description: "Optional ending line number (1-indexed).",
					},
				},
				Required: []string{"share_id", "path"},
			},
		},
		{
			Name:        "file_info",
			Description: "Retrieve detailed metadata for a file or directory (size, modification time, MIME type, binary flag).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"path": {
						Type:        "string",
						Description: "Relative path of the file or directory.",
					},
				},
				Required: []string{"share_id", "path"},
			},
		},
		{
			Name:        "get_file",
			Description: "Retrieve permitted file content within transfer limits.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"share_id": {
						Type:        "string",
						Description: "The session ID of the share returned by connect_share.",
					},
					"path": {
						Type:        "string",
						Description: "Relative path of the file.",
					},
				},
				Required: []string{"share_id", "path"},
			},
		},
	}
}
