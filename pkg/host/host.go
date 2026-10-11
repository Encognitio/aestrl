package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Encognitio/aestrl/pkg/config"
	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/Encognitio/aestrl/pkg/hub"
	"github.com/Encognitio/aestrl/pkg/p2p"
	"github.com/gorilla/websocket"
)

// Session represents a live sharing session.
type Session struct {
	SessionID  string
	Secret     string
	SecretHash string
	ShareURL   string
	MCPURL     string
	ExpiresAt  time.Time
	RootDir    string
	engine     *engine.Engine
	p2pHandler *p2p.SessionHandler
	ws         *websocket.Conn
	cfg        *config.Config
	stopChan   chan struct{}
	mu         sync.Mutex
}

// StartShare starts sharing a directory according to config and options.
func StartShare(ctx context.Context, cfg *config.Config, rootDir string, ttl time.Duration) (*Session, error) {
	eng, err := engine.New(rootDir, cfg.Exclusions, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize inspection engine: %w", err)
	}

	sessionID, err := config.GenerateSecureToken(8) // 16 hex chars
	if err != nil {
		return nil, err
	}
	secret, err := config.GenerateSecureToken(16) // 32 hex chars
	if err != nil {
		return nil, err
	}

	hash := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(hash[:])

	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	expiresAt := time.Now().Add(ttl)

	p2pHandler := p2p.NewSessionHandler(sessionID, secret, eng)

	pubHub := strings.TrimRight(cfg.HubURL, "/")
	shareURL := fmt.Sprintf("%s/s/%s#%s", pubHub, sessionID, secret)
	mcpURL := cfg.MCPEndpoint()

	sess := &Session{
		SessionID:  sessionID,
		Secret:     secret,
		SecretHash: secretHash,
		ShareURL:   shareURL,
		MCPURL:     mcpURL,
		ExpiresAt:  expiresAt,
		RootDir:    eng.RootPath(),
		engine:     eng,
		p2pHandler: p2pHandler,
		cfg:        cfg,
		stopChan:   make(chan struct{}),
	}

	// Connect outbound to Hub over WebSocket
	if err := sess.connectHub(); err != nil {
		p2pHandler.Close()
		return nil, fmt.Errorf("failed to connect to Aestrl hub: %w", err)
	}

	// Start background reader and heartbeat
	go sess.readLoop()
	go sess.heartbeatLoop()

	// Auto-expire timer
	go func() {
		select {
		case <-time.After(time.Until(expiresAt)):
			sess.Close()
		case <-sess.stopChan:
		case <-ctx.Done():
			sess.Close()
		}
	}()

	return sess, nil
}

func (s *Session) connectHub() error {
	wsEndpoint := s.cfg.HostWSEndpoint()
	u, err := url.Parse(wsEndpoint)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("token", s.cfg.HostToken)
	u.RawQuery = q.Encode()

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	ws, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	s.ws = ws

	// Send register_share
	regMsg := hub.HostMessage{
		Action:     "register_share",
		SessionID:  s.SessionID,
		SecretHash: s.SecretHash,
		Name:       filepath.Base(s.RootDir),
		ExpiresAt:  s.ExpiresAt,
	}

	if err := ws.WriteJSON(regMsg); err != nil {
		return err
	}

	// Wait for registration acknowledgment
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ack hub.HostMessage
	if err := ws.ReadJSON(&ack); err != nil {
		return fmt.Errorf("failed to receive share registration ack: %w", err)
	}
	_ = ws.SetReadDeadline(time.Time{})

	if ack.Action != "share_registered" {
		return fmt.Errorf("unexpected message during registration: %s", ack.Action)
	}

	return nil
}

func (s *Session) readLoop() {
	defer s.Close()

	for {
		select {
		case <-s.stopChan:
			return
		default:
		}

		var msg hub.HostMessage
		if err := s.ws.ReadJSON(&msg); err != nil {
			return
		}

		go s.handleHostMessage(&msg)
	}
}

func (s *Session) handleHostMessage(msg *hub.HostMessage) {
	switch msg.Action {
	case "inspect_request":
		res, err := s.executeInspectOp(msg.Op, msg.Args)
		resp := hub.HostMessage{
			Action:    "inspect_response",
			RequestID: msg.RequestID,
		}
		if err != nil {
			resp.Error = err.Error()
		} else {
			raw, _ := json.Marshal(res)
			resp.Result = raw
		}
		s.writeMessage(resp)

	case "signal_offer":
		answerSDP, err := s.p2pHandler.HandleOffer(msg.OfferSDP)
		resp := hub.HostMessage{
			Action:    "signal_answer",
			RequestID: msg.RequestID,
			SessionID: s.SessionID,
		}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.AnswerSDP = answerSDP
		}
		s.writeMessage(resp)
	}
}

func (s *Session) executeInspectOp(op string, args map[string]interface{}) (interface{}, error) {
	switch op {
	case "list_directory":
		path, _ := args["path"].(string)
		return s.engine.ListDirectory(path)

	case "tree":
		path, _ := args["path"].(string)
		depth := 3
		if d, ok := args["depth"].(float64); ok && d > 0 {
			depth = int(d)
		}
		return s.engine.Tree(path, depth, 1000)

	case "search_files":
		query, _ := args["query"].(string)
		path, _ := args["path"].(string)
		return s.engine.SearchFiles(query, path, 100)

	case "search_content":
		query, _ := args["query"].(string)
		path, _ := args["path"].(string)
		isRegex, _ := args["is_regex"].(bool)
		return s.engine.SearchContent(query, path, isRegex, 100, 2)

	case "read_file":
		path, _ := args["path"].(string)
		startLine := 0
		if sl, ok := args["start_line"].(float64); ok {
			startLine = int(sl)
		}
		endLine := 0
		if el, ok := args["end_line"].(float64); ok {
			endLine = int(el)
		}
		return s.engine.ReadFile(path, startLine, endLine)

	case "file_info":
		path, _ := args["path"].(string)
		return s.engine.FileInfo(path)

	case "get_file":
		path, _ := args["path"].(string)
		return s.engine.ReadFile(path, 0, 0)

	default:
		return nil, errors.New("unknown operation: " + op)
	}
}

func (s *Session) writeMessage(msg hub.HostMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws != nil {
		_ = s.ws.WriteJSON(msg)
	}
}

func (s *Session) heartbeatLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.writeMessage(hub.HostMessage{Action: "heartbeat"})
		}
	}
}

// Close terminates the share session cleanly.
func (s *Session) Close() {
	s.mu.Lock()
	select {
	case <-s.stopChan:
		s.mu.Unlock()
		return
	default:
		close(s.stopChan)
	}

	if s.ws != nil {
		// Attempt unregister
		_ = s.ws.WriteJSON(hub.HostMessage{
			Action:    "unregister_share",
			SessionID: s.SessionID,
		})
		_ = s.ws.Close()
		s.ws = nil
	}
	s.mu.Unlock()

	if s.p2pHandler != nil {
		s.p2pHandler.Close()
	}
}

// Done returns a channel signaling session termination.
func (s *Session) Done() <-chan struct{} {
	return s.stopChan
}
