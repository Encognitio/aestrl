package hub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Encognitio/aestrl/pkg/config"
)

// ClientRegistration represents a registered OAuth client (DCR, CIMD, or Predefined).
type ClientRegistration struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	ClientName   string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
}

// OAuthCode represents an issued authorization code with PKCE challenge.
type OAuthCode struct {
	Code                string
	Identity            string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
	Resource            string
	ExpiresAt           time.Time
}

// OAuthToken represents an issued access token.
type OAuthToken struct {
	Token     string
	Identity  string
	Scope     string
	ClientID  string
	ExpiresAt time.Time
}

// OAuthManager manages OAuth 2.1 authorization codes, client registrations, and access tokens.
type OAuthManager struct {
	codes   map[string]*OAuthCode
	tokens  map[string]*OAuthToken
	clients map[string]*ClientRegistration
	mu      sync.RWMutex
}

// NewOAuthManager creates a new in-memory OAuth 2.1 manager.
func NewOAuthManager() *OAuthManager {
	return &OAuthManager{
		codes:   make(map[string]*OAuthCode),
		tokens:  make(map[string]*OAuthToken),
		clients: make(map[string]*ClientRegistration),
	}
}

// RegisterClient dynamically registers an OAuth client (RFC 7591 DCR).
func (m *OAuthManager) RegisterClient(clientName string, redirectURIs []string) (*ClientRegistration, error) {
	if len(redirectURIs) == 0 {
		return nil, fmt.Errorf("at least one redirect_uri is required")
	}

	for _, u := range redirectURIs {
		parsed, err := url.Parse(u)
		if err != nil || !parsed.IsAbs() {
			return nil, fmt.Errorf("invalid redirect_uri: %s (must be absolute URL)", u)
		}
		if parsed.Fragment != "" {
			return nil, fmt.Errorf("redirect_uri must not contain fragment: %s", u)
		}
		if parsed.Scheme != "https" {
			h := parsed.Hostname()
			if h != "localhost" && h != "127.0.0.1" && h != "::1" {
				return nil, fmt.Errorf("redirect_uri must use https scheme: %s", u)
			}
		}
	}

	clientID, err := config.GenerateSecureToken(16)
	if err != nil {
		return nil, err
	}
	clientSecret, err := config.GenerateSecureToken(32)
	if err != nil {
		return nil, err
	}

	reg := &ClientRegistration{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		ClientName:   clientName,
		RedirectURIs: redirectURIs,
	}

	m.mu.Lock()
	m.clients[clientID] = reg
	m.mu.Unlock()

	return reg, nil
}

// ValidateClient validates client_id and redirect_uri against DCR, CIMD, or Predefined clients.
func (m *OAuthManager) ValidateClient(clientID, redirectURI string) error {
	if clientID == "" {
		return fmt.Errorf("missing client_id")
	}
	if redirectURI == "" {
		return fmt.Errorf("missing redirect_uri")
	}

	parsedURI, err := url.Parse(redirectURI)
	if err != nil || !parsedURI.IsAbs() {
		return fmt.Errorf("invalid redirect_uri: must be an absolute URL")
	}
	if parsedURI.Fragment != "" {
		return fmt.Errorf("invalid redirect_uri: must not contain a fragment")
	}

	// Insecure http is only permitted for localhost/loopback
	if parsedURI.Scheme != "https" {
		h := parsedURI.Hostname()
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return fmt.Errorf("invalid redirect_uri: https required for remote clients")
		}
	}

	// 1. Dynamic Client Registration lookup
	m.mu.RLock()
	reg, exists := m.clients[clientID]
	m.mu.RUnlock()
	if exists {
		matched := false
		for _, u := range reg.RedirectURIs {
			if u == redirectURI || strings.HasPrefix(redirectURI, strings.TrimSuffix(u, "/")) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("redirect_uri not registered for client: %s", redirectURI)
		}
		return nil
	}

	// 2. Predefined clients
	switch clientID {
	case "chatgpt", "openai":
		host := strings.ToLower(parsedURI.Hostname())
		if host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com") ||
			host == "openai.com" || strings.HasSuffix(host, ".openai.com") ||
			host == "oaistatic.com" || strings.HasSuffix(host, ".oaistatic.com") {
			return nil
		}
		return fmt.Errorf("redirect_uri domain not permitted for client '%s': %s", clientID, parsedURI.Hostname())

	case "aestrl-client", "aestrl":
		return nil

	default:
		// 3. CIMD (Client ID Metadata Document - https:// URL)
		if strings.HasPrefix(clientID, "https://") {
			parsedClientID, err := url.Parse(clientID)
			if err == nil && parsedClientID.IsAbs() {
				if strings.EqualFold(parsedURI.Hostname(), parsedClientID.Hostname()) {
					return nil
				}
			}
		}

		return fmt.Errorf("unrecognized client_id: %s", clientID)
	}
}

// ValidatePKCE ensures PKCE S256 is strictly enforced for OAuth 2.1.
func (m *OAuthManager) ValidatePKCE(challenge, method string) error {
	if challenge == "" {
		return fmt.Errorf("code_challenge is required")
	}
	if method != "S256" {
		return fmt.Errorf("code_challenge_method must be S256 (plain is forbidden in OAuth 2.1)")
	}
	return nil
}

// ValidateResource verifies the requested resource audience (RFC 8707).
func (m *OAuthManager) ValidateResource(publicURL, identity, resource string) error {
	if resource == "" {
		return nil
	}
	baseURL := strings.TrimRight(publicURL, "/")
	expectedMcp := fmt.Sprintf("%s/%s/mcp", baseURL, identity)
	expectedBase := fmt.Sprintf("%s/%s", baseURL, identity)

	if resource != expectedMcp && resource != expectedBase {
		return fmt.Errorf("resource audience mismatch: expected '%s', got '%s'", expectedMcp, resource)
	}
	return nil
}

// ValidateScopes verifies that requested scopes are supported.
func (m *OAuthManager) ValidateScopes(scope string) error {
	if scope == "" {
		return nil
	}
	parts := strings.Fields(scope)
	for _, s := range parts {
		if s != "inspect" && s != "read" {
			return fmt.Errorf("unsupported scope: %s (supported: inspect, read)", s)
		}
	}
	return nil
}

// CreateCode generates a short-lived authorization code.
func (m *OAuthManager) CreateCode(identity, clientID, redirectURI, challenge, method, scope, resource string) (string, error) {
	if challenge == "" || method != "S256" {
		return "", fmt.Errorf("PKCE S256 challenge required")
	}

	code, err := config.GenerateSecureToken(24)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.codes[code] = &OAuthCode{
		Code:                code,
		Identity:            identity,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: method,
		Scope:               scope,
		Resource:            resource,
		ExpiresAt:           time.Now().Add(10 * time.Minute),
	}
	return code, nil
}

// ExchangeCode exchanges an authorization code and verifies PKCE code_verifier.
func (m *OAuthManager) ExchangeCode(code, verifier, redirectURI, clientID, resource string) (string, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, exists := m.codes[code]
	if !exists || time.Now().After(rec.ExpiresAt) {
		delete(m.codes, code)
		return "", "", "", fmt.Errorf("invalid or expired authorization code")
	}
	delete(m.codes, code) // Single-use!

	if rec.RedirectURI != "" && redirectURI != "" && rec.RedirectURI != redirectURI {
		return "", "", "", fmt.Errorf("redirect_uri mismatch")
	}

	if rec.ClientID != "" && clientID != "" && rec.ClientID != clientID {
		return "", "", "", fmt.Errorf("client_id mismatch")
	}

	if rec.Resource != "" && resource != "" && rec.Resource != resource {
		return "", "", "", fmt.Errorf("resource audience mismatch")
	}

	// Verify PKCE S256
	if rec.CodeChallenge == "" || rec.CodeChallengeMethod != "S256" {
		return "", "", "", fmt.Errorf("invalid code state: PKCE S256 required")
	}
	if verifier == "" {
		return "", "", "", fmt.Errorf("code_verifier required for PKCE")
	}
	h := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(h[:])
	if computed != rec.CodeChallenge {
		return "", "", "", fmt.Errorf("invalid PKCE code_verifier")
	}

	// Generate access token
	accessToken, err := config.GenerateSecureToken(32)
	if err != nil {
		return "", "", "", err
	}

	m.tokens[accessToken] = &OAuthToken{
		Token:     accessToken,
		Identity:  rec.Identity,
		Scope:     rec.Scope,
		ClientID:  rec.ClientID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // 30 days
	}

	return accessToken, rec.Scope, rec.Identity, nil
}

// IssueTokenForIdentity creates an access token directly for client_credentials flow.
func (m *OAuthManager) IssueTokenForIdentity(identity, scope, clientID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	token, err := config.GenerateSecureToken(32)
	if err != nil {
		return "", err
	}
	m.tokens[token] = &OAuthToken{
		Token:     token,
		Identity:  identity,
		Scope:     scope,
		ClientID:  clientID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	return token, nil
}

// ValidateToken verifies if a token is valid for the given identity.
func (m *OAuthManager) ValidateToken(identity, token string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, exists := m.tokens[token]
	if !exists {
		return false
	}
	if rec.Identity != identity {
		return false
	}
	return time.Now().Before(rec.ExpiresAt)
}

// GetToken returns an OAuth token record if valid.
func (m *OAuthManager) GetToken(token string) *OAuthToken {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, exists := m.tokens[token]
	if !exists || time.Now().After(rec.ExpiresAt) {
		return nil
	}
	return rec
}

// HandleProtectedResourceMetadata serves /.well-known/oauth-protected-resource
func (h *Hub) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request, identity string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	baseURL := strings.TrimRight(h.config.PublicURL, "/")
	resp := map[string]interface{}{
		"resource":              fmt.Sprintf("%s/%s/mcp", baseURL, identity),
		"authorization_servers": []string{fmt.Sprintf("%s/%s", baseURL, identity)},
		"scopes_supported":      []string{"inspect", "read"},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleAuthorizationServerMetadata serves /.well-known/oauth-authorization-server (RFC 8414)
func (h *Hub) HandleAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request, identity string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	baseURL := strings.TrimRight(h.config.PublicURL, "/")
	resp := map[string]interface{}{
		"issuer":                                fmt.Sprintf("%s/%s", baseURL, identity),
		"authorization_endpoint":                fmt.Sprintf("%s/%s/oauth/authorize", baseURL, identity),
		"token_endpoint":                        fmt.Sprintf("%s/%s/oauth/token", baseURL, identity),
		"registration_endpoint":                 fmt.Sprintf("%s/%s/oauth/register", baseURL, identity),
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"inspect", "read"},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleOAuthRegister handles dynamic client registration (RFC 7591) at /<identity>/oauth/register
func (h *Hub) HandleOAuthRegister(w http.ResponseWriter, r *http.Request, identity string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_request",
			"error_description": "Failed to parse JSON body",
		})
		return
	}

	reg, err := h.oauth.RegisterClient(req.ClientName, req.RedirectURIs)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_client_metadata",
			"error_description": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"client_id":                  reg.ClientID,
		"client_secret":              reg.ClientSecret,
		"client_name":                reg.ClientName,
		"redirect_uris":              reg.RedirectURIs,
		"grant_types":                []string{"authorization_code", "client_credentials"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "client_secret_post",
	})
}

// HandleOAuthAuthorize handles GET and POST to /<identity>/oauth/authorize
func (h *Hub) HandleOAuthAuthorize(w http.ResponseWriter, r *http.Request, identity string) {
	// 1. Verify identity exists
	h.mu.RLock()
	rec, exists := h.identities[identity]
	h.mu.RUnlock()
	if !exists {
		http.Error(w, "Identity not found", http.StatusNotFound)
		return
	}

	_ = r.ParseForm()
	clientID := r.FormValue("client_id")
	if clientID == "" {
		clientID = r.URL.Query().Get("client_id")
	}
	redirectURI := r.FormValue("redirect_uri")
	if redirectURI == "" {
		redirectURI = r.URL.Query().Get("redirect_uri")
	}
	state := r.FormValue("state")
	if state == "" {
		state = r.URL.Query().Get("state")
	}
	challenge := r.FormValue("code_challenge")
	if challenge == "" {
		challenge = r.URL.Query().Get("code_challenge")
	}
	method := r.FormValue("code_challenge_method")
	if method == "" {
		method = r.URL.Query().Get("code_challenge_method")
	}
	responseType := r.FormValue("response_type")
	if responseType == "" {
		responseType = r.URL.Query().Get("response_type")
	}
	scope := r.FormValue("scope")
	if scope == "" {
		scope = r.URL.Query().Get("scope")
	}
	resource := r.FormValue("resource")
	if resource == "" {
		resource = r.URL.Query().Get("resource")
	}

	// 2. Validate client & redirect_uri
	if err := h.oauth.ValidateClient(clientID, redirectURI); err != nil {
		http.Error(w, fmt.Sprintf("invalid_client or redirect_uri: %v", err), http.StatusBadRequest)
		return
	}

	// 3. Validate response_type
	if responseType != "code" {
		http.Error(w, "unsupported_response_type: response_type must be 'code'", http.StatusBadRequest)
		return
	}

	// 4. Validate PKCE S256
	if err := h.oauth.ValidatePKCE(challenge, method); err != nil {
		http.Error(w, fmt.Sprintf("invalid_request: %v", err), http.StatusBadRequest)
		return
	}

	// 5. Validate resource audience
	if err := h.oauth.ValidateResource(h.config.PublicURL, identity, resource); err != nil {
		http.Error(w, fmt.Sprintf("invalid_target: %v", err), http.StatusBadRequest)
		return
	}

	// 6. Validate scope
	if err := h.oauth.ValidateScopes(scope); err != nil {
		http.Error(w, fmt.Sprintf("invalid_scope: %v", err), http.StatusBadRequest)
		return
	}
	if scope == "" {
		scope = "inspect read"
	}

	// 7. Identity-owner authentication and authorization
	token := r.FormValue("identity_token")
	if token == "" {
		token = r.FormValue("mcp_token")
	}
	if token == "" {
		token = r.FormValue("identity_key")
	}
	if token == "" {
		token = r.URL.Query().Get("identity_token")
	}
	if token == "" {
		token = r.URL.Query().Get("mcp_token")
	}
	if token == "" {
		if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			token = strings.TrimSpace(authHeader[7:])
		} else if user, pass, ok := r.BasicAuth(); ok {
			if pass != "" {
				token = pass
			} else {
				token = user
			}
		}
	}

	action := r.FormValue("action")

	// If no credential provided:
	if token == "" {
		if r.Method == http.MethodGet {
			// Render consent page for identity owner
			h.renderConsentPage(w, identity, clientID, redirectURI, state, challenge, method, scope, resource, "")
			return
		}
		http.Error(w, "Unauthorized: missing identity credentials", http.StatusUnauthorized)
		return
	}

	// Verify credential matches persistent identity MCPToken
	if token != rec.MCPToken {
		if r.Method == http.MethodGet {
			h.renderConsentPage(w, identity, clientID, redirectURI, state, challenge, method, scope, resource, "Invalid identity token. Access denied.")
			return
		}
		http.Error(w, "Unauthorized: invalid identity credentials", http.StatusUnauthorized)
		return
	}

	// Check if user clicked Deny
	if action == "deny" {
		parsedRedirect, _ := url.Parse(redirectURI)
		q := parsedRedirect.Query()
		q.Set("error", "access_denied")
		q.Set("error_description", "The identity owner denied access")
		if state != "" {
			q.Set("state", state)
		}
		parsedRedirect.RawQuery = q.Encode()
		http.Redirect(w, r, parsedRedirect.String(), http.StatusFound)
		return
	}

	// Identity owner authorized! Generate single-use authorization code
	code, err := h.oauth.CreateCode(identity, clientID, redirectURI, challenge, method, scope, resource)
	if err != nil {
		http.Error(w, "failed to generate authorization code", http.StatusInternalServerError)
		return
	}

	parsedRedirect, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	q := parsedRedirect.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	parsedRedirect.RawQuery = q.Encode()

	http.Redirect(w, r, parsedRedirect.String(), http.StatusFound)
}

// renderConsentPage serves a clean, secure HTML consent page for the identity owner.
func (h *Hub) renderConsentPage(w http.ResponseWriter, identity, clientID, redirectURI, state, challenge, method, scope, resource, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	errorHTML := ""
	if errMsg != "" {
		errorHTML = `<div class="error">` + html.EscapeString(errMsg) + `</div>`
	}

	tmpl := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Aestrl — Authorize MCP Client</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0d1117; color: #c9d1d9; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; padding: 20px; box-sizing: border-box; }
    .card { background: #161b22; border: 1px solid #30363d; border-radius: 8px; max-width: 460px; width: 100%; padding: 24px; box-shadow: 0 8px 24px rgba(0,0,0,0.5); }
    h1 { font-size: 1.25rem; margin: 0 0 16px; color: #58a6ff; }
    p { font-size: 0.9rem; margin: 0 0 16px; line-height: 1.5; color: #8b949e; }
    .info { background: #0d1117; border: 1px solid #21262d; border-radius: 6px; padding: 12px; margin-bottom: 20px; font-size: 0.85rem; }
    .info-row { display: flex; justify-content: space-between; margin-bottom: 6px; }
    .info-row:last-child { margin-bottom: 0; }
    .label { color: #8b949e; }
    .value { color: #f0f6fc; font-family: monospace; word-break: break-all; }
    label { display: block; font-size: 0.85rem; margin-bottom: 6px; color: #c9d1d9; font-weight: 600; }
    input[type="password"] { width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 10px 12px; color: #f0f6fc; font-family: monospace; font-size: 0.9rem; margin-bottom: 20px; }
    input[type="password"]:focus { outline: none; border-color: #58a6ff; }
    .actions { display: flex; gap: 12px; }
    button { flex: 1; padding: 10px; border-radius: 6px; font-weight: 600; font-size: 0.9rem; cursor: pointer; border: none; }
    .btn-approve { background: #238636; color: #fff; }
    .btn-approve:hover { background: #2ea043; }
    .btn-deny { background: #21262d; color: #c9d1d9; border: 1px solid #30363d; }
    .btn-deny:hover { background: #30363d; }
    .error { background: #f8514926; border: 1px solid #f85149; color: #ff7b72; padding: 10px; border-radius: 6px; margin-bottom: 16px; font-size: 0.85rem; }
  </style>
</head>
<body>
  <div class="card">
    <h1>Authorize MCP Client</h1>
    <p>An application is requesting access to your persistent Aestrl identity.</p>
    {{ERROR_BLOCK}}
    <div class="info">
      <div class="info-row"><span class="label">Identity:</span><span class="value">{{IDENTITY}}</span></div>
      <div class="info-row"><span class="label">Client ID:</span><span class="value">{{CLIENT_ID}}</span></div>
      <div class="info-row"><span class="label">Scopes:</span><span class="value">{{SCOPE}}</span></div>
      <div class="info-row"><span class="label">Redirect URI:</span><span class="value">{{REDIRECT_URI}}</span></div>
    </div>
    <form method="POST" action="">
      <input type="hidden" name="client_id" value="{{CLIENT_ID}}">
      <input type="hidden" name="redirect_uri" value="{{REDIRECT_URI}}">
      <input type="hidden" name="state" value="{{STATE}}">
      <input type="hidden" name="code_challenge" value="{{CHALLENGE}}">
      <input type="hidden" name="code_challenge_method" value="{{METHOD}}">
      <input type="hidden" name="scope" value="{{SCOPE}}">
      <input type="hidden" name="resource" value="{{RESOURCE}}">
      <input type="hidden" name="response_type" value="code">
      <label for="identity_token">Identity MCP Token (from 'aestrl setup')</label>
      <input type="password" id="identity_token" name="identity_token" placeholder="Enter persistent MCP token" required autofocus>
      <div class="actions">
        <button type="submit" name="action" value="deny" class="btn-deny" formnovalidate>Deny</button>
        <button type="submit" name="action" value="approve" class="btn-approve">Approve Access</button>
      </div>
    </form>
  </div>
</body>
</html>`

	page := strings.ReplaceAll(tmpl, "{{ERROR_BLOCK}}", errorHTML)
	page = strings.ReplaceAll(page, "{{IDENTITY}}", html.EscapeString(identity))
	page = strings.ReplaceAll(page, "{{CLIENT_ID}}", html.EscapeString(clientID))
	page = strings.ReplaceAll(page, "{{REDIRECT_URI}}", html.EscapeString(redirectURI))
	page = strings.ReplaceAll(page, "{{STATE}}", html.EscapeString(state))
	page = strings.ReplaceAll(page, "{{CHALLENGE}}", html.EscapeString(challenge))
	page = strings.ReplaceAll(page, "{{METHOD}}", html.EscapeString(method))
	page = strings.ReplaceAll(page, "{{SCOPE}}", html.EscapeString(scope))
	page = strings.ReplaceAll(page, "{{RESOURCE}}", html.EscapeString(resource))

	_, _ = w.Write([]byte(page))
}

// HandleOAuthToken handles POST to /<identity>/oauth/token
func (h *Hub) HandleOAuthToken(w http.ResponseWriter, r *http.Request, identity string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	_ = r.ParseForm()
	grantType := r.FormValue("grant_type")
	code := r.FormValue("code")
	verifier := r.FormValue("code_verifier")
	redirectURI := r.FormValue("redirect_uri")
	clientID := r.FormValue("client_id")
	resource := r.FormValue("resource")

	// Check client_secret or Basic auth
	clientSecret := r.FormValue("client_secret")
	if user, pass, ok := r.BasicAuth(); ok {
		if clientID == "" {
			clientID = user
		}
		clientSecret = pass
	}

	h.mu.RLock()
	rec, exists := h.identities[identity]
	h.mu.RUnlock()
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_request",
			"error_description": "Identity not found",
		})
		return
	}

	var accessToken string
	var tokenScope string
	var err error

	if grantType == "client_credentials" {
		if clientSecret != rec.MCPToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_client",
				"error_description": "Invalid client secret",
			})
			return
		}
		tokenScope = "inspect read"
		accessToken, err = h.oauth.IssueTokenForIdentity(identity, tokenScope, clientID)
	} else if grantType == "authorization_code" {
		accessToken, tokenScope, _, err = h.oauth.ExchangeCode(code, verifier, redirectURI, clientID, resource)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "unsupported_grant_type",
			"error_description": "grant_type must be authorization_code or client_credentials",
		})
		return
	}

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   2592000, // 30 days
		"scope":        tokenScope,
	})
}
