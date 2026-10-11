package hub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Encognitio/aestrl/pkg/config"
)

// OAuthCode represents an issued authorization code with PKCE challenge.
type OAuthCode struct {
	Code                string
	Identity            string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
}

// OAuthToken represents an issued access token.
type OAuthToken struct {
	Token     string
	Identity  string
	ExpiresAt time.Time
}

// OAuthManager manages OAuth 2.1 authorization codes and access tokens per identity.
type OAuthManager struct {
	codes  map[string]*OAuthCode
	tokens map[string]*OAuthToken
	mu     sync.RWMutex
}

// NewOAuthManager creates a new in-memory OAuth 2.1 manager.
func NewOAuthManager() *OAuthManager {
	return &OAuthManager{
		codes:  make(map[string]*OAuthCode),
		tokens: make(map[string]*OAuthToken),
	}
}

// CreateCode generates a short-lived authorization code.
func (m *OAuthManager) CreateCode(identity, clientID, redirectURI, challenge, method string) (string, error) {
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
		ExpiresAt:           time.Now().Add(10 * time.Minute),
	}
	return code, nil
}

// ExchangeCode exchanges an authorization code and verifies PKCE code_verifier.
func (m *OAuthManager) ExchangeCode(code, verifier, redirectURI, clientID string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, exists := m.codes[code]
	if !exists || time.Now().After(rec.ExpiresAt) {
		delete(m.codes, code)
		return "", "", fmt.Errorf("invalid or expired authorization code")
	}
	delete(m.codes, code) // Single-use

	if rec.RedirectURI != "" && redirectURI != "" && rec.RedirectURI != redirectURI {
		return "", "", fmt.Errorf("redirect_uri mismatch")
	}

	// Verify PKCE if challenge was provided
	if rec.CodeChallenge != "" {
		if verifier == "" {
			return "", "", fmt.Errorf("code_verifier required for PKCE")
		}
		if rec.CodeChallengeMethod == "S256" {
			h := sha256.Sum256([]byte(verifier))
			computed := base64.RawURLEncoding.EncodeToString(h[:])
			if computed != rec.CodeChallenge {
				return "", "", fmt.Errorf("invalid PKCE code_verifier")
			}
		} else if rec.CodeChallengeMethod == "plain" || rec.CodeChallengeMethod == "" {
			if verifier != rec.CodeChallenge {
				return "", "", fmt.Errorf("invalid PKCE code_verifier")
			}
		}
	}

	// Generate access token
	accessToken, err := config.GenerateSecureToken(32)
	if err != nil {
		return "", "", err
	}

	m.tokens[accessToken] = &OAuthToken{
		Token:     accessToken,
		Identity:  rec.Identity,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // 30 days
	}

	return accessToken, rec.Identity, nil
}

// IssueTokenForIdentity creates an access token directly for client_credentials flow.
func (m *OAuthManager) IssueTokenForIdentity(identity string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	token, err := config.GenerateSecureToken(32)
	if err != nil {
		return "", err
	}
	m.tokens[token] = &OAuthToken{
		Token:     token,
		Identity:  identity,
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
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"inspect", "read"},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleOAuthAuthorize handles GET and POST to /<identity>/oauth/authorize
func (h *Hub) HandleOAuthAuthorize(w http.ResponseWriter, r *http.Request, identity string) {
	clientID := r.URL.Query().Get("client_id")
	redirectURI := r.URL.Query().Get("redirect_uri")
	state := r.URL.Query().Get("state")
	challenge := r.URL.Query().Get("code_challenge")
	method := r.URL.Query().Get("code_challenge_method")

	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		if clientID == "" {
			clientID = r.FormValue("client_id")
		}
		if redirectURI == "" {
			redirectURI = r.FormValue("redirect_uri")
		}
		if state == "" {
			state = r.FormValue("state")
		}
		if challenge == "" {
			challenge = r.FormValue("code_challenge")
		}
		if method == "" {
			method = r.FormValue("code_challenge_method")
		}
	}

	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}

	// Auto-approve or consent
	code, err := h.oauth.CreateCode(identity, clientID, redirectURI, challenge, method)
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

	// Check client_secret or Basic auth
	clientSecret := r.FormValue("client_secret")
	if user, pass, ok := r.BasicAuth(); ok {
		if clientID == "" {
			clientID = user
		}
		clientSecret = pass
	}

	var accessToken string
	var err error

	h.mu.RLock()
	rec, exists := h.identities[identity]
	h.mu.RUnlock()
	if !exists {
		http.Error(w, "identity not found", http.StatusNotFound)
		return
	}

	if grantType == "client_credentials" {
		if clientSecret != rec.MCPToken {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		accessToken, err = h.oauth.IssueTokenForIdentity(identity)
	} else if grantType == "authorization_code" || grantType == "" {
		accessToken, _, err = h.oauth.ExchangeCode(code, verifier, redirectURI, clientID)
	} else {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
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
		"scope":        "inspect",
	})
}
