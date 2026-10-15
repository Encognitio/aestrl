package p2p

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/pion/webrtc/v4"
)

var (
	ErrPeerConnectionFailed = errors.New("WebRTC direct P2P connection failed (direct ICE candidate could not be connected)")
	ErrDataChannelTimeout   = errors.New("timeout waiting for WebRTC DataChannel to open")
	ErrSessionClosed        = errors.New("P2P session closed")
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

// SessionHandler handles WebRTC peer connections for a single active share session on the host.
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

// PeerCount returns the count of active peer connections.
func (sh *SessionHandler) PeerCount() int {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.peers)
}

// HandleOffer processes an incoming SDP offer from a peer and returns an SDP answer.
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

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	if err := peerConnection.SetLocalDescription(answer); err != nil {
		_ = peerConnection.Close()
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

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

		// Verify secret capability
		reqHash := sha256.Sum256([]byte(req.Secret))
		if hex.EncodeToString(reqHash[:]) != sh.secretHash {
			sh.sendDCError(dc, req.ID, req.Action, "Unauthorized: invalid share secret capability")
			return
		}

		// Dispatch action
		go sh.dispatchAction(dc, &req)
	})
}

func (sh *SessionHandler) dispatchAction(dc *webrtc.DataChannel, req *DCRequest) {
	if req.Args == nil {
		req.Args = make(map[string]interface{})
	}

	// Legacy browser streaming download support
	if req.Action == "get_file" {
		path, _ := req.Args["path"].(string)
		rawRes, err := sh.engine.GetRawFile(path, 0, -1)
		if err != nil {
			sh.sendDCError(dc, req.ID, req.Action, err.Error())
			return
		}
		sh.streamFileChunks(dc, req.ID, req.Action, bytes.NewReader(rawRes.Data), rawRes.Size, rawRes.MimeType)
		return
	}

	// Unified execution through engine dispatcher
	res, err := sh.engine.Execute(req.Action, req.Args)
	if err != nil {
		sh.sendDCError(dc, req.ID, req.Action, err.Error())
		return
	}
	sh.sendDCSuccess(dc, req.ID, req.Action, res)
}

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

// --- P2P Client Implementation ---

// ClientSession manages an encrypted WebRTC DataChannel connection to a remote share.
type ClientSession struct {
	SessionID  string
	Secret     string
	HubURL     string
	pc         *webrtc.PeerConnection
	dc         *webrtc.DataChannel
	mu         sync.Mutex
	reqCounter uint64
	pending    map[string]chan *DCResponse
	closeChan  chan struct{}
}

// ParseShareURL parses a full Aestrl share URL into its session ID, secret, and hub URL.
func ParseShareURL(rawURL string) (sessionID, secret, hubURL string, err error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", "", "", errors.New("empty share URL")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid URL: %w", err)
	}

	pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range pathParts {
		if part == "s" && i+1 < len(pathParts) {
			sessionID = pathParts[i+1]
			break
		}
	}
	if sessionID == "" && len(pathParts) > 0 {
		sessionID = pathParts[len(pathParts)-1]
	}

	secret = u.Fragment
	if secret == "" {
		secret = u.Query().Get("secret")
	}

	hubURL = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	if u.Scheme == "" || u.Host == "" {
		hubURL = "https://aestrl.dvrsfy.org"
	}

	if sessionID == "" {
		return "", "", "", errors.New("unable to determine session ID from URL")
	}
	if secret == "" {
		return "", "", "", errors.New("share secret capability missing in URL (must include #<secret>)")
	}

	return sessionID, secret, hubURL, nil
}

// DialShare connects to a remote share over WebRTC DataChannel via the signaling coordinator.
func DialShare(ctx context.Context, shareURL string) (*ClientSession, error) {
	sessionID, secret, hubURL, err := ParseShareURL(shareURL)
	if err != nil {
		return nil, err
	}

	signalEndpoint := fmt.Sprintf("%s/s/%s/signal", strings.TrimRight(hubURL, "/"), sessionID)
	return DialWithSignaling(ctx, sessionID, secret, signalEndpoint)
}

// DialWithSignaling connects to a share given its credentials and a signaling endpoint URL.
func DialWithSignaling(ctx context.Context, sessionID, secret, signalEndpoint string) (*ClientSession, error) {
	mediaEngine := &webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	config := webrtc.Configuration{
		ICEServers: DefaultICEServers,
	}

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	dc, err := pc.CreateDataChannel("aestrl-inspect", &webrtc.DataChannelInit{
		Ordered: &[]bool{true}[0],
	})
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to create data channel: %w", err)
	}

	client := &ClientSession{
		SessionID: sessionID,
		Secret:    secret,
		pc:        pc,
		dc:        dc,
		pending:   make(map[string]chan *DCResponse),
		closeChan: make(chan struct{}),
	}

	openChan := make(chan struct{})
	var once sync.Once
	dc.OnOpen(func() {
		once.Do(func() {
			close(openChan)
		})
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var resp DCResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return
		}

		client.mu.Lock()
		ch, ok := client.pending[resp.ID]
		if ok {
			delete(client.pending, resp.ID)
		}
		client.mu.Unlock()

		if ok && ch != nil {
			ch <- &resp
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to create offer: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)

	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	select {
	case <-gatherComplete:
	case <-time.After(1500 * time.Millisecond):
	}

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		_ = pc.Close()
		return nil, fmt.Errorf("local description unavailable after gathering")
	}

	// Send offer to signaling endpoint
	reqBody := map[string]string{
		"offer_sdp": localDesc.SDP,
		"secret":    secret,
	}
	bodyData, _ := json.Marshal(reqBody)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", signalEndpoint, bytes.NewReader(bodyData))
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpCli := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpCli.Do(httpReq)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("signaling request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = pc.Close()
		return nil, fmt.Errorf("signaling error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var signalResp struct {
		AnswerSDP string `json:"answer_sdp"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&signalResp); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to decode signaling response: %w", err)
	}
	if signalResp.Error != "" {
		_ = pc.Close()
		return nil, fmt.Errorf("signaling error: %s", signalResp.Error)
	}
	if signalResp.AnswerSDP == "" {
		_ = pc.Close()
		return nil, errors.New("empty answer SDP from signaling server")
	}

	// Set remote description
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  signalResp.AnswerSDP,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to set remote description: %w", err)
	}

	// Wait for DataChannel to open
	select {
	case <-openChan:
	case <-time.After(8 * time.Second):
		_ = pc.Close()
		return nil, ErrDataChannelTimeout
	case <-ctx.Done():
		_ = pc.Close()
		return nil, ctx.Err()
	}

	return client, nil
}

// Execute performs an operation over the WebRTC DataChannel.
func (cs *ClientSession) Execute(ctx context.Context, action string, args map[string]interface{}) (interface{}, error) {
	if args == nil {
		args = make(map[string]interface{})
	}

	reqID := fmt.Sprintf("req-%d", atomic.AddUint64(&cs.reqCounter, 1))
	respChan := make(chan *DCResponse, 1)

	cs.mu.Lock()
	select {
	case <-cs.closeChan:
		cs.mu.Unlock()
		return nil, ErrSessionClosed
	default:
		cs.pending[reqID] = respChan
	}
	cs.mu.Unlock()

	req := DCRequest{
		ID:     reqID,
		Action: action,
		Secret: cs.Secret,
		Args:   args,
	}

	data, err := json.Marshal(req)
	if err != nil {
		cs.mu.Lock()
		delete(cs.pending, reqID)
		cs.mu.Unlock()
		return nil, err
	}

	if err := cs.dc.SendText(string(data)); err != nil {
		cs.mu.Lock()
		delete(cs.pending, reqID)
		cs.mu.Unlock()
		return nil, fmt.Errorf("failed to send DC message: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Result, nil
	case <-time.After(20 * time.Second):
		cs.mu.Lock()
		delete(cs.pending, reqID)
		cs.mu.Unlock()
		return nil, errors.New("P2P request timed out")
	case <-ctx.Done():
		cs.mu.Lock()
		delete(cs.pending, reqID)
		cs.mu.Unlock()
		return nil, ctx.Err()
	case <-cs.closeChan:
		return nil, ErrSessionClosed
	}
}

// Close closes the P2P client connection.
func (cs *ClientSession) Close() error {
	cs.mu.Lock()
	select {
	case <-cs.closeChan:
		cs.mu.Unlock()
		return nil
	default:
		close(cs.closeChan)
	}

	for id, ch := range cs.pending {
		delete(cs.pending, id)
		close(ch)
	}
	cs.mu.Unlock()

	if cs.dc != nil {
		_ = cs.dc.Close()
	}
	if cs.pc != nil {
		return cs.pc.Close()
	}
	return nil
}
