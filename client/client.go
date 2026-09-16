package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulorf0/Ares/messages"
	"github.com/pion/webrtc/v4"
)

const writeTimeout = 10 * time.Second

var (
	ErrNoWebSocketConnection = errors.New("client: no websocket connection")
	ErrSignalingClosed       = errors.New("client: signaling connection closed")
)

type Client struct {
	id   string
	name string

	connWS  *websocket.Conn
	connRTC *webrtc.PeerConnection

	// Set from a pion callback goroutine, so access goes through DataChannel.
	channelMu sync.Mutex
	channel   *webrtc.DataChannel

	polite bool
	roomID string

	writeMu sync.Mutex

	closed    chan struct{}
	closeOnce sync.Once

	pendingICE []webrtc.ICECandidateInit
}

func New(signalURL, roomID string, id string, name string) (*Client, error) {
	c := &Client{
		id:     id,
		name:   name,
		roomID: roomID,
		closed: make(chan struct{}),
	}

	if err := c.connect(signalURL, roomID); err != nil {
		return nil, err
	}
	if err := c.createPeer(); err != nil {
		c.closeSignaling()
		return nil, err
	}

	go c.readPump()

	return c, nil
}

func (c *Client) connect(signalURL, roomID string) error {
	u, err := url.Parse(signalURL)
	if err != nil {
		return fmt.Errorf("parse signaling url: %w", err)
	}
	query := u.Query()
	query.Set("room", roomID)
	u.RawQuery = query.Encode()

	conn, res, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if res != nil {
			return fmt.Errorf("signaling handshake rejected (%s): %w", res.Status, err)
		}
		return fmt.Errorf("dial signaling server: %w", err)
	}

	var role messages.RoleMessage
	if err := conn.ReadJSON(&role); err != nil {
		conn.Close()
		return fmt.Errorf("read role message: %w", err)
	}
	if role.Type != messages.TypeRole {
		conn.Close()
		return fmt.Errorf("expected %q as first message, got %q", messages.TypeRole, role.Type)
	}

	c.connWS = conn
	c.polite = role.Payload.Polite

	return nil
}

func (c *Client) createPeer() error {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	conn, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	c.connRTC = conn

	conn.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		if err := c.sendEnvelope(messages.TypeICE, candidate.ToJSON()); err != nil &&
			!errors.Is(err, ErrSignalingClosed) {
			slog.Error("send ice candidate", "error", err)
		}
	})

	// Fires only on the answering side; the offering peer creates its own.
	conn.OnDataChannel(func(dc *webrtc.DataChannel) {
		c.setChannel(dc)
		c.setupDataChannel(dc)
	})

	conn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("peer connection state changed", "state", state.String())
	})

	return nil
}

func (c *Client) setupDataChannel(dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		slog.Info("data channel open", "label", dc.Label())
		c.closeSignaling()
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		fmt.Println(string(msg.Data))
	})
}

func (c *Client) readPump() {
	defer c.closeSignaling()

	for {
		var env messages.Envelope
		if err := c.connWS.ReadJSON(&env); err != nil {
			if !c.isClosed() {
				slog.Error("read signaling message", "error", err)
			}
			return
		}

		if err := c.handle(env); err != nil {
			slog.Error("handle signaling message", "type", env.Type, "error", err)
		}
	}
}

func (c *Client) handle(env messages.Envelope) error {
	switch env.Type {
	case messages.TypePeerJoined:
		return c.startOffer()

	case messages.TypeSDP:
		var desc webrtc.SessionDescription
		if err := json.Unmarshal(env.Payload, &desc); err != nil {
			return fmt.Errorf("decode session description: %w", err)
		}
		return c.handleRemoteDescription(desc)

	case messages.TypePeerLeft:
		slog.Warn("peer left before the connection was established")
		c.closeSignaling()
		return nil

	case messages.TypeICE:
		var candidate webrtc.ICECandidateInit
		if err := json.Unmarshal(env.Payload, &candidate); err != nil {
			return fmt.Errorf("decode ice candidate: %w", err)
		}
		return c.handleRemoteCandidate(candidate)

	default:
		slog.Warn("unknown signaling message", "type", env.Type)
		return nil
	}
}

// startOffer creates the data channel and sends the offer. It runs on the peer
// that was already in the room when the second one joined.
func (c *Client) startOffer() error {
	dc, err := c.connRTC.CreateDataChannel(fmt.Sprintf("CHANNEL-%s", c.roomID), nil)
	if err != nil {
		return fmt.Errorf("create data channel: %w", err)
	}
	c.setChannel(dc)
	c.setupDataChannel(dc)

	offer, err := c.connRTC.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	// SetLocalDescription is what starts ICE gathering.
	if err := c.connRTC.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	return c.sendEnvelope(messages.TypeSDP, offer)
}

func (c *Client) handleRemoteDescription(desc webrtc.SessionDescription) error {
	if err := c.connRTC.SetRemoteDescription(desc); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	c.flushPendingICE()

	if desc.Type != webrtc.SDPTypeOffer {
		return nil
	}

	answer, err := c.connRTC.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := c.connRTC.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	return c.sendEnvelope(messages.TypeSDP, answer)
}

func (c *Client) handleRemoteCandidate(candidate webrtc.ICECandidateInit) error {
	if c.connRTC.RemoteDescription() == nil {
		c.pendingICE = append(c.pendingICE, candidate)
		return nil
	}
	return c.connRTC.AddICECandidate(candidate)
}

func (c *Client) flushPendingICE() {
	for _, candidate := range c.pendingICE {
		if err := c.connRTC.AddICECandidate(candidate); err != nil {
			slog.Error("add buffered ice candidate", "error", err)
		}
	}
	c.pendingICE = nil
}

func (c *Client) sendEnvelope(msgType string, payload any) error {
	if c.connWS == nil {
		return ErrNoWebSocketConnection
	}
	if c.isClosed() {
		return ErrSignalingClosed
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", msgType, err)
	}
	data, err := json.Marshal(messages.Envelope{Type: msgType, Payload: raw})
	if err != nil {
		return fmt.Errorf("encode %s envelope: %w", msgType, err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.connWS.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.connWS.WriteMessage(websocket.TextMessage, data)
}

// closeSignaling tears down the websocket once, whichever goroutine gets there
// first.
func (c *Client) closeSignaling() {
	c.closeOnce.Do(func() {
		close(c.closed)

		if c.connWS == nil {
			return
		}

		c.writeMu.Lock()
		defer c.writeMu.Unlock()

		_ = c.connWS.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = c.connWS.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = c.connWS.Close()
	})
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Polite reports the negotiation role assigned by the server at handshake time.
func (c *Client) Polite() bool {
	return c.polite
}

// ConnectionState reports the state of the peer connection.
func (c *Client) ConnectionState() webrtc.PeerConnectionState {
	return c.connRTC.ConnectionState()
}

// SignalingClosed reports whether the signaling connection has been torn down.
// It closes on its own once the data channel opens.
func (c *Client) SignalingClosed() bool {
	return c.isClosed()
}

func (c *Client) setChannel(dc *webrtc.DataChannel) {
	c.channelMu.Lock()
	defer c.channelMu.Unlock()
	c.channel = dc
}

// DataChannel returns the negotiated channel, or nil while the handshake is
// still in flight.
func (c *Client) DataChannel() *webrtc.DataChannel {
	c.channelMu.Lock()
	defer c.channelMu.Unlock()
	return c.channel
}

// Close releases both the signaling connection and the peer connection.
func (c *Client) Close() error {
	c.closeSignaling()
	if c.connRTC != nil {
		return c.connRTC.Close()
	}
	return nil
}

// SendMessage stamps the message with the local identity and sends it over the
// data channel.
func (c *Client) SendMessage(msg messages.Message) error {
	if c.connRTC == nil {
		return fmt.Errorf("send message: connRTC == nil")
	}

	channel := c.DataChannel()
	if channel == nil {
		return fmt.Errorf("send message: channel == nil")
	}

	msg.SendAt = time.Now()
	msg.ClientName = c.name
	msg.ClientID = c.id
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	err = channel.Send(raw)
	if err != nil {
		return fmt.Errorf("channel sendtext: %w", err)
	}

	return nil
}

// ReceiveMessage registers fn as the handler for incoming messages. It blocks
// until the handshake settles, and gives up if no channel was negotiated.
func (c *Client) ReceiveMessage(fn func(msg []byte)) {
	<-c.closed

	channel := c.DataChannel()
	if channel == nil {
		slog.Warn("no data channel to receive on: signaling ended before the handshake completed")
		return
	}

	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		fn(msg.Data)
	})
}
