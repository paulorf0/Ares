package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulorf0/Ares/messages"
	_ "github.com/paulorf0/Ares/microphone"
	"github.com/pion/interceptor"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/prop"
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

	// Registered by the caller, invoked from a pion callback goroutine.
	handlerMu sync.Mutex
	onMessage func(msg []byte)
	onAudio   func(frame []byte)

	// Microphone capture and the codecs it was set up with; the same selector
	// populates the MediaEngine so the SDP only offers what can be encoded.
	codecAudio *mediadevices.CodecSelector
	audioTrack *mediadevices.AudioTrack

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

	//if err := c.connect(signalURL, roomID); err != nil {
	//	return nil, err
	//}
	if err := c.captureAudio(); err != nil {
		c.closeSignaling()
		return nil, err
	}
	if err := c.createPeer(); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.addAudioTrack(); err != nil {
		c.Close()
		return nil, err
	}
	//go c.readPump()

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

	mediaEngine := &webrtc.MediaEngine{}
	c.codecAudio.Populate(mediaEngine)

	// A custom MediaEngine skips pion's defaults, so NACK and RTCP reports
	// have to be registered by hand.
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return fmt.Errorf("register interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	)

	conn, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	c.connRTC = conn

	conn.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		slog.Info("remote track", "kind", track.Kind().String(), "codec", track.Codec().MimeType)
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		c.readRemoteAudio(track)
	})

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
		c.handlerMu.Lock()
		handler := c.onMessage
		c.handlerMu.Unlock()

		if handler == nil {
			slog.Warn("incoming message dropped: no handler registered")
			return
		}
		handler(msg.Data)
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

// Close releases the signaling connection, the peer connection and the
// microphone.
func (c *Client) Close() error {
	c.closeSignaling()

	var err error
	if c.connRTC != nil {
		err = c.connRTC.Close()
	}
	if c.audioTrack != nil {
		err = errors.Join(err, c.audioTrack.Close())
	}
	return err
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

// ReceiveMessage registers fn as the handler for incoming messages. It returns
// right away and can be called before the channel exists; messages that arrive
// with no handler set are dropped.
func (c *Client) ReceiveMessage(fn func(msg []byte)) {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	c.onMessage = fn
}

// ReceiveAudio registers fn as the handler for incoming audio. Each call carries
// one Opus packet taken from the remote track's RTP payload; frames that arrive
// with no handler set are dropped.
func (c *Client) ReceiveAudio(fn func(frame []byte)) {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	c.onAudio = fn
}

// captureAudio opens the microphone with an Opus encoder attached.
func (c *Client) captureAudio() error {
	opusParams, err := opus.NewParams()
	if err != nil {
		return fmt.Errorf("opus params: %w", err)
	}
	codecSelector := mediadevices.NewCodecSelector(mediadevices.WithAudioEncoders(&opusParams))

	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Audio: func(constraints *mediadevices.MediaTrackConstraints) {
			constraints.SampleRate = prop.Int(48000)
			constraints.ChannelCount = prop.Int(2)
		},
		Codec: codecSelector,
	})
	if err != nil {
		return fmt.Errorf("get user media: %w", err)
	}

	tracks := stream.GetAudioTracks()
	if len(tracks) == 0 {
		return errors.New("get user media: no audio track")
	}
	audioTrack, ok := tracks[0].(*mediadevices.AudioTrack)
	if !ok {
		tracks[0].Close()
		return fmt.Errorf("get user media: unexpected track type %T", tracks[0])
	}

	c.codecAudio = codecSelector
	c.audioTrack = audioTrack
	return nil
}

// addAudioTrack attaches the microphone track to the peer connection. AddTrack
// makes the transceiver sendrecv, so both peers can talk after the first
// negotiation.
func (c *Client) addAudioTrack() error {
	if c.connRTC == nil {
		return errors.New("add audio track: connRTC == nil")
	}

	sender, err := c.connRTC.AddTrack(c.audioTrack)
	if err != nil {
		return fmt.Errorf("add audio track: %w", err)
	}

	// RTCP feedback for the sender is only processed while something reads it.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	return nil
}

// readRemoteAudio hands each RTP payload of the remote track to the audio
// handler until the track ends.
func (c *Client) readRemoteAudio(track *webrtc.TrackRemote) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Error("read remote audio", "error", err)
			}
			return
		}

		c.handlerMu.Lock()
		fn := c.onAudio
		c.handlerMu.Unlock()

		if fn != nil {
			fn(pkt.Payload)
		}
	}
}
