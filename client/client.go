package client

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/paulorf0/Ares/client/internal/audio"
	"github.com/paulorf0/Ares/client/internal/camera"
	"github.com/paulorf0/Ares/client/internal/channel"
	"github.com/paulorf0/Ares/client/internal/peer"
	"github.com/paulorf0/Ares/client/internal/remotevideo"
	"github.com/paulorf0/Ares/client/internal/signaling"
	"github.com/paulorf0/Ares/messages"
	"github.com/paulorf0/Ares/speaker"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

var (
	ErrSignalingClosed = signaling.ErrClosed
	ErrAudioDisabled   = errors.New("client: audio is off, create the client with WithAudio")
	ErrNotConnected    = errors.New("client: not connected to the other peer")
)

// Client is one peer of the call. It wires the parts together: signaling
// until the data channel opens, the peer connection, the data channel, and
// audio and video in both directions.
type Client struct {
	signal  *signaling.Conn
	peer    *peer.Peer
	channel *channel.Channel
	audio   *audio.Call // nil without WithAudio
	camera  *camera.Camera
	remote  *remotevideo.Receiver

	ended atomic.Bool
}

// New joins the room and starts the handshake in the background.
func New(signalURL, roomID string, id string, name string, opts ...Option) (*Client, error) {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.voiceMode != VoiceOpen && !cfg.audio {
		return nil, ErrAudioDisabled
	}

	opusParams, err := opus.NewParams()
	if err != nil {
		return nil, fmt.Errorf("opus params: %w", err)
	}
	cam, err := camera.New(camera.Quality(cfg.quality), cfg.openSource)
	if err != nil {
		return nil, err
	}
	// Always offers both codecs, so a peer without audio or a camera can
	// still receive them.
	codecs := mediadevices.NewCodecSelector(
		mediadevices.WithAudioEncoders(&opusParams),
		mediadevices.WithVideoEncoders(cam.Encoder()),
	)

	c := &Client{
		channel: channel.New(id, name),
		camera:  cam,
	}
	c.remote = remotevideo.New(func(pkts []rtcp.Packet) error {
		return c.peer.Conn().WriteRTCP(pkts)
	})
	c.channel.Handle(messages.TypeVideo, c.remote.HandleState)
	c.channel.OnOpen(func() {
		c.signal.Close()
		c.camera.ChannelOpened()
	})

	c.signal, err = signaling.Dial(signalURL, roomID)
	if err != nil {
		return nil, err
	}
	c.peer, err = peer.New(peer.Config{
		Codecs:         codecs,
		Signaler:       c.signal,
		ChannelLabel:   fmt.Sprintf("CHANNEL-%s", roomID),
		OnTrack:        c.onTrack,
		OnDataChannel:  c.channel.Attach,
		OnSendersReady: c.onSendersReady,
		OnEnded:        c.onEnded,
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	if cfg.audio {
		c.audio, err = audio.New(c.peer.Conn(), codecs, audio.Mode(cfg.voiceMode))
		if err != nil {
			c.Close()
			return nil, err
		}
	}
	if err := c.camera.Attach(c.peer.Conn(), codecs, c.channel); err != nil {
		c.Close()
		return nil, err
	}
	go c.signal.Run(c.handleSignal)

	return c, nil
}

// handleSignal passes negotiation messages to the peer. A peer that leaves
// before the connection is up ends the signaling.
func (c *Client) handleSignal(env messages.Envelope) error {
	if env.Type == messages.TypePeerLeft {
		slog.Warn("peer left before the connection was established")
		c.signal.Close()
		return nil
	}
	return c.peer.Handle(env)
}

func (c *Client) onTrack(track *webrtc.TrackRemote) {
	switch track.Kind() {
	case webrtc.RTPCodecTypeAudio:
		if c.audio != nil {
			c.audio.Play(track)
		} else {
			drain(track)
		}
	case webrtc.RTPCodecTypeVideo:
		c.remote.Read(track)
	}
}

// drain reads the other peer's audio and drops it, for a client without
// WithAudio.
func drain(track *webrtc.TrackRemote) {
	for {
		if _, _, err := track.ReadRTP(); err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Error("read remote audio", "error", err)
			}
			return
		}
	}
}

func (c *Client) onSendersReady() {
	if c.audio != nil {
		c.audio.SendersReady()
	}
}

// onEnded freezes the audio numbers before marking the call ended, so Stats
// never sees an ended call with numbers still moving.
func (c *Client) onEnded() {
	if c.audio != nil {
		c.audio.End()
	}
	c.ended.Store(true)
	c.remote.SetCamera(false)
}

// Polite reports the negotiation role assigned by the server at handshake time.
func (c *Client) Polite() bool {
	return c.signal.Polite()
}

// ConnectionState reports the state of the peer connection.
func (c *Client) ConnectionState() webrtc.PeerConnectionState {
	return c.peer.State()
}

// Ping returns the round-trip time to the other peer. ICE keeps checking the
// path in use every few seconds, so this is the latest of those measurements,
// with no extra traffic. It fails with ErrNotConnected until a path exists.
func (c *Client) Ping() (time.Duration, error) {
	rtt, ok := c.peer.RTT()
	if !ok {
		return 0, ErrNotConnected
	}
	return rtt, nil
}

// SignalingClosed reports whether the signaling connection has been torn down.
// It closes on its own once the data channel opens.
func (c *Client) SignalingClosed() bool {
	return c.signal.Closed()
}

// DataChannel returns the negotiated channel, or nil while the handshake is
// still in flight.
func (c *Client) DataChannel() *webrtc.DataChannel {
	return c.channel.DataChannel()
}

// SendMessage stamps the message with the local identity and sends it over the
// data channel.
func (c *Client) SendMessage(msg messages.Message) error {
	return c.channel.Send(msg)
}

// ReceiveMessage registers fn as the handler for incoming messages. It returns
// right away and can be called before the channel exists; messages that arrive
// with no handler set are dropped.
func (c *Client) ReceiveMessage(fn func(msg []byte)) {
	c.channel.OnMessage(fn)
}

// Close releases the signaling connection, the peer connection, the mic, the
// camera and the speaker.
func (c *Client) Close() error {
	c.signal.Close()

	var err error
	if c.peer != nil {
		err = c.peer.Close()
	}
	if c.audio != nil {
		err = errors.Join(err, c.audio.Close())
	}
	return errors.Join(err, c.camera.Close())
}

// Stats is a snapshot of how the call is doing.
type Stats struct {
	// Ended reports that the connection closed or failed; the other numbers
	// are then final.
	Ended bool
	// RTT is the round trip to the other peer, zero until measured.
	RTT time.Duration
	// Audio reports whether the client was created WithAudio.
	Audio bool
	// Receiving describes the other peer's audio, nil while none comes in.
	Receiving *speaker.Stats
	// Transmitting reports whether the mic is being sent right now.
	Transmitting bool
	// MicDropped counts mic chunks lost while sending; each is a gap the
	// other peer hears.
	MicDropped uint64
	// Video describes the camera and the other peer's video.
	Video VideoStats
}

// Stats reports the connection, and audio and video in both directions.
func (c *Client) Stats() Stats {
	stats := Stats{Audio: c.audio != nil, Video: c.videoStats()}
	stats.RTT, _ = c.Ping()

	// Read before the audio numbers, which are final once it is set.
	stats.Ended = c.ended.Load()
	if c.audio != nil {
		a := c.audio.Stats()
		stats.Receiving = a.Receiving
		stats.Transmitting = a.Transmitting
		stats.MicDropped = a.MicDropped
	}
	return stats
}
