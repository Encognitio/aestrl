package p2p

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/pion/webrtc/v4"
)

var (
	ErrPeerConnectionFailed = errors.New("WebRTC direct P2P connection failed (direct ICE candidate could not be connected)")
)

// Default STUN servers (direct P2P only, no TURN relay)
var DefaultICEServers = []webrtc.ICEServer{
	{
		URLs: []string{
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
		},
	},
}

// DataChannel message formats
type DCRequest struct {
	ID     string                 `json:"id"`
	Action string                 `json:"action"`
	Secret string                 `json:"secret"`
	Args   map[string]interface{} `json:"args"`
}

type DCResponse struct {
	ID       string      `json:"id"`
	Action   string      `json:"action"`
	Result   interface{} `json:"result,omitempty"`
	Error    string      `json:"error,omitempty"`
	Chunk    string      `json:"chunk,omitempty"`
	IsLast   bool        `json:"is_last,omitempty"`
	MimeType string      `json:"mime_type,omitempty"`
	Total    int64       `json:"total,omitempty"`
}

// SessionHandler handles WebRTC peer connections for a single active share session.
type SessionHandler struct {
	sessionID  string
	secretHash string
	engine     *engine.Engine
	api        *webrtc.API
	peers      map[string]*webrtc.PeerConnection
	mu         sync.Mutex
}

// NewSessionHandler creates a WebRTC P2P handler for a share.
func NewSessionHandler(sessionID, secret string, eng *engine.Engine) *SessionHandler {
	hash := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(hash[:])

	mediaEngine := &webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	return &SessionHandler{
		sessionID:  sessionID,
		secretHash: secretHash,
		engine:     eng,
		api:        api,
		peers:      make(map[string]*webrtc.PeerConnection),
	}
}

// HandleOffer processes an incoming SDP offer from a browser and returns an SDP answer.
func (sh *SessionHandler) HandleOffer(offerSDP string) (string, error) {
	config := webrtc.Configuration{
		ICEServers: DefaultICEServers,
	}

	peerConnection, err := sh.api.NewPeerConnection(config)
	if err != nil {
		return "", fmt.Errorf("failed to create peer connection: %w", err)
	}

	peerID := fmt.Sprintf("peer-%d", time.Now().UnixNano())
	sh.mu.Lock()
	sh.peers[peerID] = peerConnection
	sh.mu.Unlock()

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			sh.mu.Lock()
			delete(sh.peers, peerID)
			sh.mu.Unlock()
			_ = peerConnection.Close()
		}
	})

	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		sh.setupDataChannel(dc)
	})

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}

	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		_ = peerConnection.Close()
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		_ = peerConnection.Close()
		return "", fmt.Errorf("failed to create answer: %w", err)
	}

	// Gather local ICE candidates
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	if err := peerConnection.SetLocalDescription(answer); err != nil {
		_ = peerConnection.Close()
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	// Wait for local candidate gathering (or timeout after 2.5 seconds)
	select {
	case <-gatherComplete:
	case <-time.After(2500 * time.Millisecond):
	}

	localDesc := peerConnection.LocalDescription()
	if localDesc == nil {
		return "", fmt.Errorf("local description unavailable after gathering")
	}

	return localDesc.SDP, nil
}

func (sh *SessionHandler) setupDataChannel(dc *webrtc.DataChannel) {
	if dc.Label() != "aestrl-inspect" {
		return
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}

		var req DCRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			sh.sendDCError(dc, req.ID, req.Action, "Invalid request JSON")
			return
		}

		// Verify secret
		reqHash := sha256.Sum256([]byte(req.Secret))
		if hex.EncodeToString(reqHash[:]) != sh.secretHash {
			sh.sendDCError(dc, req.ID, req.Action, "Unauthorized: invalid share secret")
			return
		}

		// Dispatch action
		go sh.dispatchAction(dc, &req)
	})
}

func (sh *SessionHandler) dispatchAction(dc *webrtc.DataChannel, req *DCRequest) {
	switch req.Action {
	case "list_directory":
		path, _ := req.Args["path"].(string)
		items, err := sh.engine.ListDirectory(path)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.sendDCSuccess(dc, req.ID, req.Action, items)

	case "tree":
		path, _ := req.Args["path"].(string)
		depth := 3
		if d, ok := req.Args["depth"].(float64); ok && d > 0 {
			depth = int(d)
		}
		tree, err := sh.engine.Tree(path, depth, 1000)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.sendDCSuccess(dc, req.ID, req.Action, tree)

	case "search_files":
		query, _ := req.Args["query"].(string)
		path, _ := req.Args["path"].(string)
		items, err := sh.engine.SearchFiles(query, path, 100)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.sendDCSuccess(dc, req.ID, req.Action, items)

	case "search_content":
		query, _ := req.Args["query"].(string)
		path, _ := req.Args["path"].(string)
		isRegex, _ := req.Args["is_regex"].(bool)
		matches, err := sh.engine.SearchContent(query, path, isRegex, 100, 2)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.sendDCSuccess(dc, req.ID, req.Action, matches)

	case "read_file":
		path, _ := req.Args["path"].(string)
		startLine := 0
		if s, ok := req.Args["start_line"].(float64); ok {
			startLine = int(s)
		}
		endLine := 0
		if e, ok := req.Args["end_line"].(float64); ok {
			endLine = int(e)
		}
		res, err := sh.engine.ReadFile(path, startLine, endLine)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.sendDCSuccess(dc, req.ID, req.Action, res)

	case "file_info":
		path, _ := req.Args["path"].(string)
		info, err := sh.engine.FileInfo(path)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.sendDCSuccess(dc, req.ID, req.Action, info)

	case "get_file":
		path, _ := req.Args["path"].(string)
		reader, size, mimeType, err := sh.engine.GetRawFile(path)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		defer reader.Close()
		sh.streamFileChunks(dc, req.ID, req.Action, reader, size, mimeType)

	default:
		sh.sendDCError(dc, req.ID, req.Action, "Unknown action: "+req.Action)
	}
}

// streamFileChunks streams chunks over DataChannel with backpressure control.
func (sh *SessionHandler) streamFileChunks(dc *webrtc.DataChannel, reqID, action string, r io.Reader, totalSize int64, mimeType string) {
	buf := make([]byte, 32*1024) // 32KB chunks
	for {
		n, err := r.Read(buf)
		if n > 0 {
			isLast := (err == io.EOF)
			resp := DCResponse{
				ID:       reqID,
				Action:   action,
				Chunk:    hex.EncodeToString(buf[:n]),
				IsLast:   isLast,
				MimeType: mimeType,
				Total:    totalSize,
			}
			data, _ := json.Marshal(resp)

			// Backpressure check: wait if buffer amount > 512KB
			for dc.BufferedAmount() > 512*1024 {
				time.Sleep(10 * time.Millisecond)
			}

			if err := dc.SendText(string(data)); err != nil {
				return
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			sh.sendDCError(dc, reqID, action, err.Error())
			return
		}
	}
}

func (sh *SessionHandler) sendDCSuccess(dc *webrtc.DataChannel, reqID, action string, result interface{}) {
	resp := DCResponse{
		ID:     reqID,
		Action: action,
		Result: result,
	}
	data, _ := json.Marshal(resp)
	_ = dc.SendText(string(data))
}

func (sh *SessionHandler) sendDCError(dc *webrtc.DataChannel, reqID, action, errMsg string) {
	resp := DCResponse{
		ID:     reqID,
		Action: action,
		Error:  errMsg,
	}
	data, _ := json.Marshal(resp)
	_ = dc.SendText(string(data))
}

// Close closes all peer connections.
func (sh *SessionHandler) Close() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, pc := range sh.peers {
		_ = pc.Close()
	}
	sh.peers = make(map[string]*webrtc.PeerConnection)
}
