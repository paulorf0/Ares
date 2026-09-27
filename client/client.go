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
	"github.com/paulorf0/Ares/speaker"
	"github.com/paulorf0/Ares/voice"
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
	ErrAudioDisabled         = errors.New("client: audio is off, create the client with WithAudio")
)

// Option configures a Client at creation.
type Option func(*Client)

// WithAudio turns on the voice call: the mic is captured and sent, and the
// other peer's audio is played. It has to be decided up front, because a track
// added after the handshake would never reach the other peer.
func WithAudio() Option {
	return func(c *Client) { c.audio = true }
}

// WithVoiceMode sets the voice mode from the start, so push-to-talk never
// sends anything before the first SetTalking(true). Needs WithAudio.
func WithVoiceMode(mode VoiceMode) Option {
	return func(c *Client) { c.voiceMode = mode }
}

// VoiceMode says when the mic is sent.
type VoiceMode int

const (
	// VoiceOpen sends the mic all the time.
	VoiceOpen VoiceMode = iota
	// VoicePushToTalk sends it only while SetTalking(true). Silent stretches
	// cost no CPU: capture processing and encoding stop.
	VoicePushToTalk
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

	// Codecs offered in the SDP. Always set, so a peer without audio can still
	// receive it.
	codecAudio *mediadevices.CodecSelector

	// Voice call, only with WithAudio.
	audio       bool
	audioTrack  *mediadevices.AudioTrack
	audioSender *webrtc.RTPSender
	processor   *voice.Processor

	// Push-to-talk state. The track can only be swapped once the sender has
	// started, which happens while the descriptions are set.
	voiceMu        sync.Mutex
	voiceMode      VoiceMode
	talking        bool
	transmitting   bool
	sendersStarted bool

	playerMu sync.Mutex
	player   *speaker.Player
	stopped  bool

	polite bool
	roomID string

	writeMu sync.Mutex

	closed    chan struct{}
	closeOnce sync.Once

	pendingICE []webrtc.ICECandidateInit
}

// New joins the room and starts the handshake in the background.
func New(signalURL, roomID string, id string, name string, opts ...Option) (*Client, error) {
	c := &Client{
		id:     id,
		name:   name,
		roomID: roomID,
		closed: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.voiceMode != VoiceOpen && !c.audio {
		return nil, ErrAudioDisabled
	}

	opusParams, err := opus.NewParams()
	if err != nil {
		return nil, fmt.Errorf("opus params: %w", err)
	}
	c.codecAudio = mediadevices.NewCodecSelector(mediadevices.WithAudioEncoders(&opusParams))

	if err := c.connect(signalURL, roomID); err != nil {
		return nil, err
	}
	if err := c.createPeer(); err != nil {
		c.Close()
		return nil, err
	}
	if c.audio {
		if err := c.openMicrophone(); err != nil {
			c.Close()
			return nil, err
		}
		if err := c.addAudioTrack(); err != nil {
			c.Close()
			return nil, err
		}
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
		c.sendersReady()
		return nil
	}

	answer, err := c.connRTC.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := c.connRTC.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	c.sendersReady()

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

// Close releases the signaling connection, the peer connection, the mic and
// the speaker.
func (c *Client) Close() error {
	c.closeSignaling()

	var err error
	if c.connRTC != nil {
		err = c.connRTC.Close()
	}
	if c.audioTrack != nil {
		err = errors.Join(err, c.audioTrack.Close())
	}

	c.playerMu.Lock()
	c.stopped = true
	if c.player != nil {
		c.player.Close()
	}
	c.playerMu.Unlock()

	// Last, so neither side of the audio reaches a closed processor.
	if c.processor != nil {
		c.processor.Close()
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

// openMicrophone opens the mic and sets up cleanup and Opus. The capture itself
// only starts once the track is bound to the connection.
func (c *Client) openMicrophone() error {
	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		// Mono int16 is what the voice processor takes.
		Audio: func(constraints *mediadevices.MediaTrackConstraints) {
			constraints.SampleRate = prop.Int(48000)
			constraints.ChannelCount = prop.IntExact(1)
			constraints.SampleSize = prop.IntExact(2)
			constraints.IsFloat = prop.BoolExact(false)
		},
		Codec: c.codecAudio,
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

	processor, err := voice.New()
	if err != nil {
		audioTrack.Close()
		return fmt.Errorf("voice processor: %w", err)
	}
	audioTrack.Transform(processor.CaptureTransform())

	c.audioTrack = audioTrack
	c.processor = processor
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
	c.audioSender = sender
	c.transmitting = true

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

// readRemoteAudio plays the other peer's audio until the track ends. Without
// WithAudio the packets are just drained.
func (c *Client) readRemoteAudio(track *webrtc.TrackRemote) {
	var stream *speaker.Stream
	if c.audio {
		s, err := c.startPlayback()
		if err != nil {
			slog.Error("start playback", "error", err)
		}
		stream = s
	}

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Error("read remote audio", "error", err)
			}
			return
		}
		if stream != nil {
			stream.Push(pkt)
		}
	}
}

// startPlayback opens the speaker. Every played frame goes through the voice
// processor too, which is how the echo gets removed from the mic.
func (c *Client) startPlayback() (*speaker.Stream, error) {
	stream, err := speaker.NewStream()
	if err != nil {
		return nil, err
	}
	player, err := speaker.NewPlayer(stream, func(frame []int16) {
		_ = c.processor.Render(frame)
	})
	if err != nil {
		return nil, err
	}

	c.playerMu.Lock()
	defer c.playerMu.Unlock()
	if c.stopped {
		player.Close()
		return nil, errors.New("client closed")
	}
	c.player = player
	return stream, nil
}

// SetVoiceMode switches between an open mic and push-to-talk. Push-to-talk
// starts silent until SetTalking(true).
func (c *Client) SetVoiceMode(mode VoiceMode) error {
	if !c.audio {
		return ErrAudioDisabled
	}
	c.voiceMu.Lock()
	defer c.voiceMu.Unlock()
	c.voiceMode = mode
	return c.applyVoice()
}

// SetTalking opens or closes the mic in push-to-talk. With an open mic it only
// records the flag.
func (c *Client) SetTalking(talking bool) error {
	if !c.audio {
		return ErrAudioDisabled
	}
	c.voiceMu.Lock()
	defer c.voiceMu.Unlock()
	c.talking = talking
	return c.applyVoice()
}

// sendersReady runs once the descriptions have started the RTP senders; from
// then on the mic track can be swapped out.
func (c *Client) sendersReady() {
	if c.audioSender == nil {
		return
	}
	c.voiceMu.Lock()
	defer c.voiceMu.Unlock()
	c.sendersStarted = true
	if err := c.applyVoice(); err != nil {
		slog.Error("apply voice mode", "error", err)
	}
}

// applyVoice sends or stops the mic to match the current mode. Removing the
// track stops the encoder and the capture processing, not just the sending.
// Before the senders start, pion refuses a missing track, so it waits; nothing
// goes out before then anyway. Callers hold voiceMu.
func (c *Client) applyVoice() error {
	want := c.voiceMode == VoiceOpen || c.talking
	if !c.sendersStarted || want == c.transmitting {
		return nil
	}

	var track webrtc.TrackLocal
	if want {
		track = c.audioTrack
	}
	if err := c.audioSender.ReplaceTrack(track); err != nil {
		return fmt.Errorf("switch mic: %w", err)
	}
	c.transmitting = want
	return nil
}
