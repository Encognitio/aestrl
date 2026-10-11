package hub

import (
	"encoding/json"
	"time"
)

// HostMessage represents a message exchanged over the host-hub WebSocket.
type HostMessage struct {
	Action     string                 `json:"action"`
	RequestID  string                 `json:"request_id,omitempty"`
	SessionID  string                 `json:"session_id,omitempty"`
	SecretHash string                 `json:"secret_hash,omitempty"`
	Secret     string                 `json:"secret,omitempty"`
	Name       string                 `json:"name,omitempty"`
	ExpiresAt  time.Time              `json:"expires_at,omitempty"`
	Op         string                 `json:"op,omitempty"`
	Args       map[string]interface{} `json:"args,omitempty"`
	Result     json.RawMessage        `json:"result,omitempty"`
	Error      string                 `json:"error,omitempty"`
	OfferSDP   string                 `json:"offer_sdp,omitempty"`
	AnswerSDP  string                 `json:"answer_sdp,omitempty"`
}

// InspectRequest is sent from the terminal CLI to /api/inspect.
type InspectRequest struct {
	SessionID string                 `json:"session_id"`
	Secret    string                 `json:"secret"`
	Op        string                 `json:"op"`
	Args      map[string]interface{} `json:"args"`
}

// InspectResponse is returned by /api/inspect.
type InspectResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// SetupRequest is sent from aestrl setup to /api/setup.
type SetupRequest struct {
	Identity  string `json:"identity"`
	HostToken string `json:"host_token"`
	MCPToken  string `json:"mcp_token"`
}

// SetupResponse is returned by /api/setup.
type SetupResponse struct {
	Status      string `json:"status"` // "created" or "verified"
	Identity    string `json:"identity"`
	MCPEndpoint string `json:"mcp_endpoint"`
}

// ShareMeta represents public info for a temporary share.
type ShareMeta struct {
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	Active    bool      `json:"active"`
}
