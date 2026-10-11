package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Encognitio/aestrl/pkg/mcp"
	"github.com/gorilla/websocket"
)

var (
	ErrIdentityNotFound   = errors.New("identity not registered")
	ErrInvalidHostToken   = errors.New("invalid host authentication token")
	ErrShareNotFound      = errors.New("share session not found or expired")
	ErrShareUnauthorized  = errors.New("invalid share secret capability")
	ErrHostNotConnected   = errors.New("no host connected for this share session")
	ErrRequestTimeout     = errors.New("inspection request timed out")
)

// IdentityRecord stores persistent identity credentials on the hub.
type IdentityRecord struct {
	Identity  string    `json:"identity"`
	HostToken string    `json:"host_token"`
	MCPToken  string    `json:"mcp_token"`
	CreatedAt time.Time `json:"created_at"`
}

// ActiveShare represents an active directory share served by a connected host.
type ActiveShare struct {
	SessionID  string          `json:"session_id"`
	Identity   string          `json:"identity"`
	SecretHash string          `json:"secret_hash"`
	Name       string          `json:"name"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Host       *HostConnection `json:"-"`
}

// HostConnection tracks a live host WebSocket connection.
type HostConnection struct {
	ID        string
	Identity  string
	ws        *websocket.Conn
	shares    map[string]*ActiveShare
	pending   map[string]chan *HostMessage
	mu        sync.Mutex
	isClosed  bool
	closeChan chan struct{}
}

// Config configures the Hub.
type Config struct {
	ListenAddr string
	PublicURL  string
	DataDir    string
}

// MCPSession tracks active shares and verified capabilities for a distinct conversation.
type MCPSession struct {
	ID               string
	Identity         string
	ActiveShareID    string
	AuthorizedShares map[string]string // shareID -> capabilitySecret
	LastSeen         time.Time
}

// Hub coordinates identities, hosts, MCP gateway, and WebRTC signaling.
type Hub struct {
	config                   Config
	identities               map[string]*IdentityRecord
	hosts                    map[string][]*HostConnection            // Identity -> []*HostConnection
	shares                   map[string]*ActiveShare                 // SessionID -> *ActiveShare
	mcpSessions              map[string]*MCPSession                  // SessionID -> *MCPSession
	identityAuthorizedShares map[string]map[string]string            // Identity -> ShareID -> Secret
	oauth                    *OAuthManager
	mu                       sync.RWMutex
	upgrader                 websocket.Upgrader
	webAssets                http.FileSystem
}

// New creates a new Hub instance.
func New(cfg Config, webAssets http.FileSystem) (*Hub, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create hub data dir: %w", err)
	}

	h := &Hub{
		config:                   cfg,
		identities:               make(map[string]*IdentityRecord),
		hosts:                    make(map[string][]*HostConnection),
		shares:                   make(map[string]*ActiveShare),
		mcpSessions:              make(map[string]*MCPSession),
		identityAuthorizedShares: make(map[string]map[string]string),
		oauth:                    NewOAuthManager(),
		webAssets:                webAssets,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for WebRTC signaling and MCP
			},
		},
	}

	// Load persistent identities from disk
	if err := h.loadIdentities(); err != nil {
		return nil, err
	}

	return h, nil
}

func (h *Hub) loadIdentities() error {
	filePath := filepath.Join(h.config.DataDir, "identities.json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read identities file: %w", err)
	}

	var list []*IdentityRecord
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("invalid identities file JSON: %w", err)
	}

	for _, rec := range list {
		h.identities[rec.Identity] = rec
	}
	return nil
}

func (h *Hub) saveIdentities() error {
	filePath := filepath.Join(h.config.DataDir, "identities.json")
	var list []*IdentityRecord
	for _, rec := range h.identities {
		list = append(list, rec)
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, filePath)
}

// Handler returns the HTTP handler for all Hub endpoints.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()

	// Identity Setup API
	mux.HandleFunc("/api/setup", h.handleSetup)

	// CLI Terminal Inspection API
	mux.HandleFunc("/api/inspect", h.handleInspect)

	// Public share metadata
	mux.HandleFunc("/api/share/", h.handleShareMeta)

	// WebRTC signaling
	mux.HandleFunc("/s/", h.handleShareRouting)

	// Catch-all for host WebSocket and MCP endpoints
	mux.HandleFunc("/", h.handleRootRouting)

	return mux
}

// handleSetup registers or verifies a persistent identity.
func (h *Hub) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	req.Identity = strings.TrimSpace(req.Identity)
	req.HostToken = strings.TrimSpace(req.HostToken)
	req.MCPToken = strings.TrimSpace(req.MCPToken)

	if req.Identity == "" || req.HostToken == "" || req.MCPToken == "" {
		http.Error(w, "identity, host_token and mcp_token are required", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	rec, exists := h.identities[req.Identity]
	status := "verified"
	if !exists {
		rec = &IdentityRecord{
			Identity:  req.Identity,
			HostToken: req.HostToken,
			MCPToken:  req.MCPToken,
			CreatedAt: time.Now(),
		}
		h.identities[req.Identity] = rec
		status = "created"
		if err := h.saveIdentities(); err != nil {
			http.Error(w, "Failed to persist identity: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		// Verify tokens match
		if rec.HostToken != req.HostToken || rec.MCPToken != req.MCPToken {
			http.Error(w, "Identity exists with different credentials. Use --reset to reassign.", http.StatusConflict)
			return
		}
	}

	pubURL := strings.TrimRight(h.config.PublicURL, "/")
	resp := SetupResponse{
		Status:      status,
		Identity:    req.Identity,
		MCPEndpoint: fmt.Sprintf("%s/%s/mcp", pubURL, req.Identity),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleRootRouting dispatches to Host WS (/<id>/host/ws) or MCP (/<id>/mcp).
func (h *Hub) handleRootRouting(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	if len(parts) >= 2 {
		identity := parts[0]
		action := parts[1]

		h.mu.RLock()
		rec, exists := h.identities[identity]
		h.mu.RUnlock()

		if exists {
			// OAuth 2.1 discovery & authorization endpoints per identity
			if action == ".well-known" && len(parts) >= 3 {
				if parts[2] == "oauth-protected-resource" {
					h.HandleProtectedResourceMetadata(w, r, identity)
					return
				}
				if parts[2] == "oauth-authorization-server" {
					h.HandleAuthorizationServerMetadata(w, r, identity)
					return
				}
			}
			if action == "oauth" && len(parts) >= 3 {
				if parts[2] == "authorize" {
					h.HandleOAuthAuthorize(w, r, identity)
					return
				}
				if parts[2] == "token" {
					h.HandleOAuthToken(w, r, identity)
					return
				}
			}

			if action == "host" && len(parts) >= 3 && parts[2] == "ws" {
				h.handleHostWS(w, r, rec)
				return
			}
			if action == "mcp" {
				mcpHandler := mcp.NewHandler(rec.MCPToken, &hubExecutor{hub: h, identity: identity})
				mcpHandler.SetTokenValidator(func(token string) bool {
					return token == rec.MCPToken || h.oauth.ValidateToken(identity, token)
				})
				baseURL := strings.TrimRight(h.config.PublicURL, "/")
				mcpHandler.SetMetadataURL(fmt.Sprintf("%s/%s/.well-known/oauth-protected-resource", baseURL, identity))
				mcpHandler.ServeHTTP(w, r)
				return
			}
		}
	}

	// Global discovery fallback
	if path == ".well-known/oauth-protected-resource" {
		w.Header().Set("Content-Type", "application/json")
		baseURL := strings.TrimRight(h.config.PublicURL, "/")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"resource":              baseURL,
			"scopes_supported":      []string{"inspect", "read"},
		})
		return
	}

	// Serve root web asset or 404
	if h.webAssets != nil {
		http.FileServer(h.webAssets).ServeHTTP(w, r)
		return
	}

	http.NotFound(w, r)
}

// handleHostWS upgrades and manages persistent host WebSocket connections.
func (h *Hub) handleHostWS(w http.ResponseWriter, r *http.Request, rec *IdentityRecord) {
	token := r.URL.Query().Get("token")
	if token == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[7:])
		}
	}

	if token != rec.HostToken {
		http.Error(w, "Unauthorized host token", http.StatusUnauthorized)
		return
	}

	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	connID := fmt.Sprintf("host-%d", time.Now().UnixNano())
	hostConn := &HostConnection{
		ID:        connID,
		Identity:  rec.Identity,
		ws:        ws,
		shares:    make(map[string]*ActiveShare),
		pending:   make(map[string]chan *HostMessage),
		closeChan: make(chan struct{}),
	}

	h.mu.Lock()
	h.hosts[rec.Identity] = append(h.hosts[rec.Identity], hostConn)
	h.mu.Unlock()

	defer func() {
		h.cleanupHost(hostConn)
		_ = ws.Close()
	}()

	// WebSocket read loop
	for {
		var msg HostMessage
		if err := ws.ReadJSON(&msg); err != nil {
			break
		}
		h.processHostMessage(hostConn, &msg)
	}
}

func (h *Hub) cleanupHost(hc *HostConnection) {
	hc.mu.Lock()
	hc.isClosed = true
	close(hc.closeChan)
	// Cancel pending requests
	for reqID, ch := range hc.pending {
		close(ch)
		delete(hc.pending, reqID)
	}
	hc.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()

	// Remove shares owned by this host
	for sessID := range hc.shares {
		delete(h.shares, sessID)
	}

	// Remove host connection
	conns := h.hosts[hc.Identity]
	var updated []*HostConnection
	for _, c := range conns {
		if c.ID != hc.ID {
			updated = append(updated, c)
		}
	}
	h.hosts[hc.Identity] = updated
}

func (h *Hub) processHostMessage(hc *HostConnection, msg *HostMessage) {
	switch msg.Action {
	case "register_share":
		h.mu.Lock()
		share := &ActiveShare{
			SessionID:  msg.SessionID,
			Identity:   hc.Identity,
			SecretHash: msg.SecretHash,
			Name:       msg.Name,
			ExpiresAt:  msg.ExpiresAt,
			Host:       hc,
		}
		h.shares[msg.SessionID] = share
		hc.shares[msg.SessionID] = share
		h.mu.Unlock()

		hc.mu.Lock()
		_ = hc.ws.WriteJSON(HostMessage{
			Action:    "share_registered",
			RequestID: msg.RequestID,
			SessionID: msg.SessionID,
		})
		hc.mu.Unlock()

	case "unregister_share":
		h.mu.Lock()
		delete(h.shares, msg.SessionID)
		delete(hc.shares, msg.SessionID)
		h.mu.Unlock()

	case "inspect_response", "signal_answer":
		hc.mu.Lock()
		if ch, exists := hc.pending[msg.RequestID]; exists {
			ch <- msg
			delete(hc.pending, msg.RequestID)
		}
		hc.mu.Unlock()

	case "heartbeat":
		// No-op keepalive
	}
}

// ForwardInspectRequest sends an inspection operation with verified capability secret to the host and waits for the response.
func (h *Hub) ForwardInspectRequest(share *ActiveShare, secret string, op string, args map[string]interface{}) (json.RawMessage, error) {
	hc := share.Host
	if hc == nil {
		return nil, ErrHostNotConnected
	}

	reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	respChan := make(chan *HostMessage, 1)

	hc.mu.Lock()
	if hc.isClosed {
		hc.mu.Unlock()
		return nil, ErrHostNotConnected
	}
	hc.pending[reqID] = respChan
	hc.mu.Unlock()

	req := HostMessage{
		Action:    "inspect_request",
		RequestID: reqID,
		SessionID: share.SessionID,
		Secret:    secret,
		Op:        op,
		Args:      args,
	}

	hc.mu.Lock()
	err := hc.ws.WriteJSON(req)
	hc.mu.Unlock()

	if err != nil {
		hc.mu.Lock()
		delete(hc.pending, reqID)
		hc.mu.Unlock()
		return nil, fmt.Errorf("failed to send request to host: %w", err)
	}

	select {
	case resp, ok := <-respChan:
		if !ok || resp == nil {
			return nil, ErrHostNotConnected
		}
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Result, nil

	case <-time.After(15 * time.Second):
		hc.mu.Lock()
		delete(hc.pending, reqID)
		hc.mu.Unlock()
		return nil, ErrRequestTimeout
	}
}

// handleInspect processes inspection requests from the terminal client.
func (h *Hub) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req InspectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	share, exists := h.shares[req.SessionID]
	h.mu.RUnlock()

	if !exists || time.Now().After(share.ExpiresAt) {
		http.Error(w, "Share session expired or not found", http.StatusNotFound)
		return
	}

	// Verify secret hash
	hash := sha256.Sum256([]byte(req.Secret))
	if hex.EncodeToString(hash[:]) != share.SecretHash {
		http.Error(w, "Unauthorized: invalid share secret", http.StatusUnauthorized)
		return
	}

	result, err := h.ForwardInspectRequest(share, req.Secret, req.Op, req.Args)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(InspectResponse{Error: err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(InspectResponse{Result: result})
}

// handleShareRouting handles /s/{session_id} web UI and /s/{session_id}/signal.
func (h *Hub) handleShareRouting(w http.ResponseWriter, r *http.Request) {
	// Enable CORS
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/s/")
	parts := strings.Split(path, "/")
	sessionID := parts[0]

	if len(parts) >= 2 && parts[1] == "signal" {
		h.handleSignal(w, r, sessionID)
		return
	}

	// Serve browser interface index.html
	if h.webAssets != nil {
		f, err := h.webAssets.Open("index.html")
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.Copy(w, f)
			return
		}
	}

	http.NotFound(w, r)
}

// handleSignal relays WebRTC SDP offer/answer between browser and host.
func (h *Hub) handleSignal(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		OfferSDP string `json:"offer_sdp"`
		Secret   string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	share, exists := h.shares[sessionID]
	h.mu.RUnlock()

	if !exists || time.Now().After(share.ExpiresAt) {
		http.Error(w, "Share session expired or not found", http.StatusNotFound)
		return
	}

	hash := sha256.Sum256([]byte(req.Secret))
	if hex.EncodeToString(hash[:]) != share.SecretHash {
		http.Error(w, "Unauthorized: invalid secret", http.StatusUnauthorized)
		return
	}

	hc := share.Host
	if hc == nil {
		http.Error(w, "Host not connected", http.StatusBadGateway)
		return
	}

	reqID := fmt.Sprintf("sig-%d", time.Now().UnixNano())
	respChan := make(chan *HostMessage, 1)

	hc.mu.Lock()
	hc.pending[reqID] = respChan
	hc.mu.Unlock()

	signalMsg := HostMessage{
		Action:    "signal_offer",
		RequestID: reqID,
		SessionID: sessionID,
		OfferSDP:  req.OfferSDP,
	}

	hc.mu.Lock()
	err := hc.ws.WriteJSON(signalMsg)
	hc.mu.Unlock()

	if err != nil {
		hc.mu.Lock()
		delete(hc.pending, reqID)
		hc.mu.Unlock()
		http.Error(w, "Failed to relay signal to host", http.StatusInternalServerError)
		return
	}

	select {
	case resp, ok := <-respChan:
		if !ok || resp == nil || resp.Error != "" {
			errMsg := "signaling error"
			if resp != nil && resp.Error != "" {
				errMsg = resp.Error
			}
			http.Error(w, errMsg, http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"answer_sdp": resp.AnswerSDP,
		})

	case <-time.After(8 * time.Second):
		hc.mu.Lock()
		delete(hc.pending, reqID)
		hc.mu.Unlock()
		http.Error(w, "Signaling timeout", http.StatusGatewayTimeout)
	}
}

// handleShareMeta returns public metadata for a share.
func (h *Hub) handleShareMeta(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/share/")
	h.mu.RLock()
	share, exists := h.shares[sessionID]
	h.mu.RUnlock()

	if !exists || time.Now().After(share.ExpiresAt) {
		http.Error(w, "Share not found or expired", http.StatusNotFound)
		return
	}

	meta := ShareMeta{
		SessionID: share.SessionID,
		Name:      share.Name,
		ExpiresAt: share.ExpiresAt,
		Active:    true,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meta)
}

// hubExecutor implements mcp.Executor by forwarding calls to the active share's host.
type hubExecutor struct {
	hub      *Hub
	identity string
}

func (e *hubExecutor) resolveShare(mcpSessionID, shareID string) (*ActiveShare, string, error) {
	e.hub.mu.RLock()
	defer e.hub.mu.RUnlock()

	sess := e.hub.mcpSessions[mcpSessionID]
	if shareID == "" && sess != nil {
		shareID = sess.ActiveShareID
	}

	if shareID == "" {
		return nil, "", errors.New("missing required 'share_id' parameter. Please call connect_share(share_url) and pass the returned share_id.")
	}

	share, exists := e.hub.shares[shareID]
	if !exists || time.Now().After(share.ExpiresAt) {
		return nil, "", ErrShareNotFound
	}

	// Enforce Identity Isolation
	if share.Identity != e.identity {
		return nil, "", errors.New("share session not found under this identity")
	}

	// Enforce Capability Validation for this conversation or identity
	secret := ""
	if sess != nil {
		secret = sess.AuthorizedShares[shareID]
	}
	if secret == "" && e.hub.identityAuthorizedShares[e.identity] != nil {
		secret = e.hub.identityAuthorizedShares[e.identity][shareID]
	}
	if secret == "" {
		return nil, "", errors.New("unauthorized: share '" + shareID + "' has not been authorized; call connect_share(share_url) first")
	}

	return share, secret, nil
}

func (e *hubExecutor) ConnectShare(mcpSessionID, shareURL string) (interface{}, error) {
	parsed, err := url.Parse(shareURL)
	if err != nil {
		return nil, fmt.Errorf("invalid share URL: %w", err)
	}

	pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	var sessionID string
	for i, part := range pathParts {
		if part == "s" && i+1 < len(pathParts) {
			sessionID = pathParts[i+1]
			break
		}
	}
	if sessionID == "" {
		return nil, fmt.Errorf("could not extract session ID from share URL: %s", shareURL)
	}

	secret := parsed.Fragment
	if secret == "" {
		secret = parsed.Query().Get("secret")
	}
	if secret == "" {
		return nil, errors.New("missing capability secret in share URL (must include #<secret>)")
	}

	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()

	share, exists := e.hub.shares[sessionID]
	if !exists || time.Now().After(share.ExpiresAt) {
		return nil, fmt.Errorf("share session '%s' is not active or has expired", sessionID)
	}

	// Enforce Identity Isolation
	if share.Identity != e.identity {
		return nil, fmt.Errorf("share session '%s' does not belong to this identity", sessionID)
	}

	// Enforce Capability Secret validation against share's secret hash
	hash := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(hash[:]) != share.SecretHash {
		return nil, ErrShareUnauthorized
	}

	// Record authorized capability in this conversation's MCP session
	sess, exists := e.hub.mcpSessions[mcpSessionID]
	if !exists {
		sess = &MCPSession{
			ID:               mcpSessionID,
			Identity:         e.identity,
			AuthorizedShares: make(map[string]string),
		}
		e.hub.mcpSessions[mcpSessionID] = sess
	}
	sess.ActiveShareID = sessionID
	sess.AuthorizedShares[sessionID] = secret
	sess.LastSeen = time.Now()

	// Also record in identity-scoped authorized shares for stateless tool calls
	if e.hub.identityAuthorizedShares[e.identity] == nil {
		e.hub.identityAuthorizedShares[e.identity] = make(map[string]string)
	}
	e.hub.identityAuthorizedShares[e.identity][sessionID] = secret

	return map[string]interface{}{
		"status":     "connected",
		"share_id":   share.SessionID,
		"name":       share.Name,
		"expires_at": share.ExpiresAt.Format(time.RFC3339),
		"message":    fmt.Sprintf("Successfully authorized share '%s' with share_id: '%s'. You must provide share_id: '%s' in all subsequent inspection tool calls.", share.Name, share.SessionID, share.SessionID),
	}, nil
}

func (e *hubExecutor) ListShares(mcpSessionID string) (interface{}, error) {
	e.hub.mu.RLock()
	defer e.hub.mu.RUnlock()

	sess := e.hub.mcpSessions[mcpSessionID]
	var shares []ShareMeta
	for _, s := range e.hub.shares {
		if s.Identity == e.identity && time.Now().Before(s.ExpiresAt) {
			isAuth := false
			if sess != nil && sess.AuthorizedShares[s.SessionID] != "" {
				isAuth = true
			} else if e.hub.identityAuthorizedShares[e.identity] != nil && e.hub.identityAuthorizedShares[e.identity][s.SessionID] != "" {
				isAuth = true
			}
			if isAuth {
				shares = append(shares, ShareMeta{
					SessionID: s.SessionID,
					Name:      s.Name,
					ExpiresAt: s.ExpiresAt,
					Active:    true,
				})
			}
		}
	}
	return shares, nil
}

func (e *hubExecutor) ListDirectory(mcpSessionID, shareID, path string) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "list_directory", map[string]interface{}{"path": path})
}

func (e *hubExecutor) Tree(mcpSessionID, shareID, path string, depth int) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "tree", map[string]interface{}{"path": path, "depth": depth})
}

func (e *hubExecutor) SearchFiles(mcpSessionID, shareID, query, path string) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "search_files", map[string]interface{}{"query": query, "path": path})
}

func (e *hubExecutor) SearchContent(mcpSessionID, shareID, query, path string, isRegex bool) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "search_content", map[string]interface{}{"query": query, "path": path, "is_regex": isRegex})
}

func (e *hubExecutor) ReadFile(mcpSessionID, shareID, path string, startLine, endLine int) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "read_file", map[string]interface{}{"path": path, "start_line": startLine, "end_line": endLine})
}

func (e *hubExecutor) FileInfo(mcpSessionID, shareID, path string) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "file_info", map[string]interface{}{"path": path})
}

func (e *hubExecutor) GetFile(mcpSessionID, shareID, path string) (interface{}, error) {
	share, secret, err := e.resolveShare(mcpSessionID, shareID)
	if err != nil {
		return nil, err
	}
	return e.hub.ForwardInspectRequest(share, secret, "get_file", map[string]interface{}{"path": path})
}
