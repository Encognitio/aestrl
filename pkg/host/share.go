package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Encognitio/aestrl/pkg/config"
	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/Encognitio/aestrl/pkg/hostedai"
	"github.com/Encognitio/aestrl/pkg/p2p"
	"github.com/gorilla/websocket"
)

// ShareInfo holds metadata and URLs for an active share.
type ShareInfo struct {
	SessionID string    `json:"session_id"`
	Secret    string    `json:"secret,omitempty"`
	ShareURL  string    `json:"share_url"`
	AIURL     string    `json:"ai_url,omitempty"`
	RootDir   string    `json:"root_dir"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	PeerCount int       `json:"peer_count"`
}

// ActiveShare represents a running share session on the host.
type ActiveShare struct {
	Info       ShareInfo
	SecretHash string
	Engine     *engine.Engine
	P2PHandler *p2p.SessionHandler
	AIServer   *hostedai.Server
	HubURL     string
	wsConn     *websocket.Conn
	stopChan   chan struct{}
	mu         sync.Mutex
}

// ShareOptions defines configuration for starting a share session.
type ShareOptions struct {
	TTL               time.Duration
	EnableAI          bool
	AIAddr            string
	AIDomain          string
	AllowHTTP         bool
	Exclusions        []string
	HubURL            string
	ExistingSessionID string
	ExistingSecret    string
}

// NewActiveShare initializes and starts a new share session for a local directory.
func NewActiveShare(path string, opts ShareOptions) (*ActiveShare, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path must be a directory: %s", absPath)
	}

	eng, err := engine.New(absPath, opts.Exclusions, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize engine: %w", err)
	}

	sessionID := opts.ExistingSessionID
	secret := opts.ExistingSecret

	// Auto-discover reusable share state from target directory if not provided
	if sessionID == "" {
		sessionID, secret = config.DiscoverShareState(absPath)
	}

	if sessionID == "" {
		token, err := config.GenerateSecureToken(8)
		if err != nil {
			return nil, err
		}
		sessionID = token
	}

	if secret == "" {
		sec, err := config.GenerateSecureToken(16)
		if err != nil {
			return nil, err
		}
		secret = sec
	}

	// Persist reusable share state in sidecar
	_ = config.SaveShareState(absPath, sessionID, secret)

	// Update snapshot index
	_ = eng.SaveSnapshot(sessionID)

	hash := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(hash[:])

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	now := time.Now()
	expiresAt := now.Add(ttl)

	hubURL := opts.HubURL
	if hubURL == "" {
		hubURL = config.DefaultHubURL
	}
	hubURL = strings.TrimRight(hubURL, "/")

	shareURL := fmt.Sprintf("%s/s/%s#%s", hubURL, sessionID, secret)

	p2pHandler := p2p.NewSessionHandler(sessionID, secret, eng)

	var aiServer *hostedai.Server
	var aiURL string
	if opts.EnableAI {
		aiCfg := hostedai.Config{
			Addr:      opts.AIAddr,
			Domain:    opts.AIDomain,
			AllowHTTP: opts.AllowHTTP,
		}
		if aiCfg.Addr == "" {
			aiCfg.Addr = "127.0.0.1:0"
		}
		srv, urlStr, err := hostedai.StartServer(eng, aiCfg, sessionID, secret)
		if err == nil {
			aiServer = srv
			aiURL = urlStr

			// Extract bound port and attempt UPnP port mapping
			u, pErr := url.Parse(urlStr)
			if pErr == nil {
				_, pStr, hErr := net.SplitHostPort(u.Host)
				if hErr == nil {
					if port, cErr := strconv.Atoi(pStr); cErr == nil && port > 0 {
						natRes := AttemptPortMapping(context.Background(), port)
						if natRes.Success && natRes.ExternalIP != "" {
							aiURL = fmt.Sprintf("http://%s:%d", natRes.ExternalIP, natRes.ExternalPort)
						}
					}
				}
			}
		}
	}

	share := &ActiveShare{
		Info: ShareInfo{
			SessionID: sessionID,
			Secret:    secret,
			ShareURL:  shareURL,
			AIURL:     aiURL,
			RootDir:   absPath,
			Name:      filepath.Base(absPath),
			CreatedAt: now,
			ExpiresAt: expiresAt,
		},
		SecretHash: secretHash,
		Engine:     eng,
		P2PHandler: p2pHandler,
		AIServer:   aiServer,
		HubURL:     hubURL,
		stopChan:   make(chan struct{}),
	}

	// Start signaling loop in background
	go share.signalingLoop()

	return share, nil
}

func (s *ActiveShare) signalingLoop() {
	// Attempt initial registration with Cloudflare coordinator
	s.registerWithCoordinator()

	// Connect WebSocket for WebRTC signaling relay
	u, err := url.Parse(s.HubURL)
	if err != nil {
		return
	}

	wsScheme := "wss"
	if u.Scheme == "http" {
		wsScheme = "ws"
	}
	wsEndpoint := fmt.Sprintf("%s://%s/s/%s/host", wsScheme, u.Host, s.Info.SessionID)

	backoff := 1 * time.Second
	for {
		select {
		case <-s.stopChan:
			return
		default:
		}

		dialer := websocket.DefaultDialer
		dialer.HandshakeTimeout = 5 * time.Second
		conn, _, err := dialer.Dial(wsEndpoint, nil)
		if err != nil {
			select {
			case <-s.stopChan:
				return
			case <-time.After(backoff):
				if backoff < 15*time.Second {
					backoff *= 2
				}
				continue
			}
		}

		backoff = 1 * time.Second
		s.mu.Lock()
		s.wsConn = conn
		s.mu.Unlock()

		s.handleWebSocketMessages(conn)

		s.mu.Lock()
		if s.wsConn == conn {
			s.wsConn = nil
		}
		s.mu.Unlock()
	}
}

func (s *ActiveShare) registerWithCoordinator() {
	regURL := fmt.Sprintf("%s/s/%s/register", s.HubURL, s.Info.SessionID)
	payload := map[string]interface{}{
		"session_id":  s.Info.SessionID,
		"secret_hash": s.SecretHash,
		"name":        s.Info.Name,
		"expires_at":  s.Info.ExpiresAt.Format(time.RFC3339),
		"direct_url":  s.Info.AIURL,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", regURL, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func (s *ActiveShare) handleWebSocketMessages(conn *websocket.Conn) {
	defer conn.Close()

	for {
		select {
		case <-s.stopChan:
			return
		default:
		}

		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			OfferSDP  string `json:"offer_sdp"`
		}

		if err := conn.ReadJSON(&msg); err != nil {
			return
		}

		if msg.Type == "offer" && msg.OfferSDP != "" {
			answerSDP, err := s.P2PHandler.HandleOffer(msg.OfferSDP)
			resp := map[string]string{
				"type":       "answer",
				"request_id": msg.RequestID,
			}
			if err != nil {
				resp["error"] = err.Error()
			} else {
				resp["answer_sdp"] = answerSDP
			}
			_ = conn.WriteJSON(resp)
		}
	}
}

// PeerCount returns active connected peer count.
func (s *ActiveShare) PeerCount() int {
	if s.P2PHandler != nil {
		return s.P2PHandler.PeerCount()
	}
	return 0
}

// Close terminates the share, closes P2P connections, AI server, and signals shutdown.
func (s *ActiveShare) Close() {
	s.mu.Lock()
	select {
	case <-s.stopChan:
		s.mu.Unlock()
		return
	default:
		close(s.stopChan)
	}

	if s.wsConn != nil {
		_ = s.wsConn.Close()
		s.wsConn = nil
	}
	s.mu.Unlock()

	if s.P2PHandler != nil {
		s.P2PHandler.Close()
	}
	if s.AIServer != nil {
		_ = s.AIServer.Close()
	}
}
