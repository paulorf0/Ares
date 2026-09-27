// Package camera sends the local camera. The call carries a video transceiver
// from the first offer, so the camera can be turned on and off at any point
// without renegotiating; while off, the sender holds pion's placeholder.
package camera

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec"
	"github.com/pion/mediadevices/pkg/codec/openh264"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v4"

	"github.com/paulorf0/Ares/framing"
	"github.com/paulorf0/Ares/messages"
	"github.com/paulorf0/Ares/rtpstream"
)

// What the camera is asked for; the quality level then brings it down.
const (
	cameraWidth     = 640
	cameraHeight    = 480
	cameraFrameRate = 30
	// Key frames are sent when the other side asks for one; this periodic
	// one is only a fallback in case a request gets lost.
	videoKeyFrameInterval = 300

	// A camera that sends no picture this long after opening is taken as
	// unavailable, and opened again after cameraRetry.
	cameraStartTimeout = 4 * time.Second
	cameraRetry        = time.Second
)

var (
	// ErrNoPicture reports a camera that opened but sent nothing. The Windows
	// driver opens a camera held by another app without complaint and then
	// never delivers a frame.
	ErrNoPicture = errors.New("client: camera sent no picture, another app may be using it")
	ErrClosed    = errors.New("client: closed")
)

// Notifier tells the other peer whether the camera is sending.
type Notifier interface {
	Ready() bool
	Send(messages.Message) error
}

// Camera is the local camera and the sender it is bound to.
type Camera struct {
	openSource func() (mediadevices.VideoSource, error)
	codecs     *mediadevices.CodecSelector
	notifier   Notifier
	sender     *webrtc.RTPSender
	idle       webrtc.TrackLocal // pion's placeholder: bound while off, sends nothing
	out        *rtpstream.Stream
	shaper     *framing.Shaper
	encoder    *h264Builder

	// Read by the encoder builder, which runs inside pion while mu is held.
	quality atomic.Int32

	mu      sync.Mutex
	want    bool               // what Set asked for
	sending bool               // the camera is bound to the sender
	track   mediadevices.Track // the open camera while sending
	bound   webrtc.TrackLocal  // track as handed to the sender
	cancel  chan struct{}      // stops the pending start; nil when none runs
	closed  bool

	statusMu  sync.Mutex
	onStatus  func(sending bool, err error)
	statusKey string // last status reported, so repeats are skipped
}

// Stats describes what the camera sends.
type Stats struct {
	Sending       bool
	Quality       Quality
	Width, Height int // zero while not sending
	Sent          rtpstream.Stats
}

// New prepares the camera at quality q. open replaces the camera device when
// set. The camera is not usable until Attach.
func New(q Quality, open func() (mediadevices.VideoSource, error)) (*Camera, error) {
	if !q.Valid() {
		return nil, errUnknown(q)
	}
	c := &Camera{
		openSource: open,
		shaper:     framing.New(q.shape()),
	}
	c.quality.Store(int32(q))

	params, err := openh264.NewParams()
	if err != nil {
		return nil, fmt.Errorf("h264 params: %w", err)
	}
	params.IntraPeriod = videoKeyFrameInterval
	c.encoder = &h264Builder{Params: params, quality: &c.quality}
	return c, nil
}

// Encoder is the H.264 encoder to offer in the codec selector.
func (c *Camera) Encoder() codec.VideoEncoderBuilder {
	return c.encoder
}

// h264Builder builds the openh264 encoder for the current quality level,
// each time the camera track is bound. The frame rate comes from the level:
// mediadevices measures it from the frames and hands 0 for the first one,
// which turns off openh264's rate control and the bitrate target with it.
type h264Builder struct {
	openh264.Params
	quality *atomic.Int32
}

func (b *h264Builder) BuildVideoEncoder(r video.Reader, p prop.Media) (codec.ReadCloser, error) {
	level := levels[b.quality.Load()]
	params := b.Params
	params.BitRate = level.bitRate
	p.FrameRate = float32(level.frameRate)
	return params.BuildVideoEncoder(r, p)
}

// Attach reserves the video slot in pc before the first offer. pion fills the
// sender with a placeholder track that never writes, which is what the sender
// goes back to when the camera is turned off.
func (c *Camera) Attach(pc *webrtc.PeerConnection, codecs *mediadevices.CodecSelector, notifier Notifier) error {
	tr, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	if err != nil {
		return fmt.Errorf("add video transceiver: %w", err)
	}
	c.codecs = codecs
	c.notifier = notifier
	c.sender = tr.Sender()
	c.idle = tr.Sender().Track()
	c.out = rtpstream.New()
	return nil
}

// SetQuality changes how much of the camera is sent. With the camera on, the
// encoder is rebuilt at the new level while the camera stays open, which
// freezes the picture for a moment; otherwise the level applies the next time
// it comes on.
func (c *Camera) SetQuality(q Quality) error {
	if !q.Valid() {
		return errUnknown(q)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.Quality() == q {
		c.mu.Unlock()
		return nil
	}
	if !c.sending {
		c.setQuality(q)
		c.mu.Unlock()
		return nil
	}

	// Rebinding builds a new encoder: the level's bitrate and frame rate are
	// only taken when one is created, and the frame rate sets how many bits
	// each frame gets.
	if err := c.sender.ReplaceTrack(c.idle); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("change video quality: %w", err)
	}
	c.setQuality(q)
	err := c.sender.ReplaceTrack(c.bound)
	if err != nil {
		// The camera is open but no longer sent: turn it off properly.
		if cerr := c.track.Close(); cerr != nil {
			slog.Warn("close camera", "error", cerr)
		}
		c.track, c.bound = nil, nil
		c.sending, c.want = false, false
		c.notify()
	}
	c.mu.Unlock()

	if err != nil {
		c.report(nil)
		return fmt.Errorf("change video quality: %w", err)
	}
	return nil
}

// Quality reports the current level.
func (c *Camera) Quality() Quality {
	return Quality(c.quality.Load())
}

func (c *Camera) setQuality(q Quality) {
	c.quality.Store(int32(q))
	c.shaper.Set(q.shape())
}

// Set turns the camera on or off. It returns at once: turning it on starts a
// background attempt that keeps trying, a new open each time, until the
// camera delivers a picture, so a camera busy in another app comes on by
// itself once freed.
func (c *Camera) Set(on bool) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if on == c.want {
		c.mu.Unlock()
		return nil
	}
	c.want = on

	if on {
		c.cancel = make(chan struct{})
		go c.start(c.cancel)
		c.mu.Unlock()
		return nil
	}

	if c.cancel != nil {
		close(c.cancel)
		c.cancel = nil
	}
	var err error
	if c.sending {
		// Unbound before closing: mediadevices' unbind waits for the encoder
		// goroutine, which quits without answering if the camera closed first.
		if rerr := c.sender.ReplaceTrack(c.idle); rerr != nil {
			err = fmt.Errorf("stop camera: %w", rerr)
		}
		if cerr := c.track.Close(); cerr != nil {
			slog.Warn("close camera", "error", cerr)
		}
		c.track, c.bound = nil, nil
		c.sending = false
		c.notify()
	}
	c.mu.Unlock()

	c.report(nil)
	return err
}

// Wanted reports whether the camera was asked to be on, whether or not it is
// sending yet.
func (c *Camera) Wanted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.want
}

// OnStatus registers fn to hear how the camera is doing. Calls come one at a
// time from background goroutines; fn must not call Set.
func (c *Camera) OnStatus(fn func(sending bool, err error)) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.onStatus = fn
}

// start opens the camera until it works or cancel is closed. Nothing here
// holds mu while waiting on the camera, so the call, the chat and the other
// camera calls never wait on it.
func (c *Camera) start(cancel <-chan struct{}) {
	for {
		track, err := c.open(cancel)

		c.mu.Lock()
		select {
		case <-cancel:
			c.mu.Unlock()
			if track != nil {
				_ = track.Close()
			}
			return
		default:
		}
		if err == nil {
			// With a picture already in, the bind inside ReplaceTrack returns
			// at once. On failure pion puts the placeholder back.
			bound := c.out.Wrap(track)
			if err = c.sender.ReplaceTrack(bound); err == nil {
				c.track, c.bound = track, bound
				c.sending = true
				c.cancel = nil
				c.notify()
				c.mu.Unlock()
				c.report(nil)
				return
			}
			_ = track.Close()
			err = fmt.Errorf("send camera: %w", err)
		}
		c.mu.Unlock()

		c.report(err)
		select {
		case <-cancel:
			return
		case <-time.After(cameraRetry):
		}
	}
}

// report tells the OnStatus handler the current state, if it changed. err is
// the reason a start attempt failed; it only counts while the camera is still
// wanted.
func (c *Camera) report(err error) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()

	c.mu.Lock()
	sending, want := c.sending, c.want
	c.mu.Unlock()

	key := "off"
	switch {
	case sending:
		key, err = "on", nil
	case want && err != nil:
		key = "error: " + err.Error()
	default:
		err = nil
	}
	if key == c.statusKey {
		return
	}
	c.statusKey = key
	if c.onStatus != nil {
		c.onStatus(sending, err)
	}
}

// open opens the camera and waits for its first picture. Handing pion a
// camera that never delivers would hang the handshake or ReplaceTrack, since
// the encoder reads a frame when it is bound.
func (c *Camera) open(cancel <-chan struct{}) (mediadevices.Track, error) {
	track, err := c.openTrack()
	if err != nil {
		return nil, err
	}
	videoTrack, ok := track.(*mediadevices.VideoTrack)
	if !ok {
		_ = track.Close()
		return nil, fmt.Errorf("unexpected camera track %T", track)
	}
	// Frames are shaped before anything reads them, the check below included.
	videoTrack.Transform(c.shaper.Transform())

	first := make(chan error, 1)
	go func() {
		_, release, err := videoTrack.NewReader(false).Read()
		if err == nil {
			release()
		}
		first <- err
	}()

	timer := time.NewTimer(cameraStartTimeout)
	defer timer.Stop()
	select {
	case err = <-first:
		if err == nil {
			return track, nil
		}
	case <-timer.C:
		err = ErrNoPicture
	case <-cancel:
		err = ErrClosed
	}
	// Closing ends the read still waiting on the camera.
	_ = track.Close()
	return nil, err
}

func (c *Camera) openTrack() (mediadevices.Track, error) {
	if c.openSource != nil {
		source, err := c.openSource()
		if err != nil {
			return nil, err
		}
		return mediadevices.NewVideoTrack(source, c.codecs), nil
	}

	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(constraints *mediadevices.MediaTrackConstraints) {
			constraints.Width = prop.Int(cameraWidth)
			constraints.Height = prop.Int(cameraHeight)
			constraints.FrameRate = prop.Float(cameraFrameRate)
		},
		Codec: c.codecs,
	})
	if err != nil {
		return nil, err
	}
	tracks := stream.GetVideoTracks()
	if len(tracks) == 0 {
		return nil, errors.New("no video track")
	}
	return tracks[0], nil
}

// notify tells the other peer whether the camera is sending. Before the data
// channel opens there is no one to tell; the state goes out on open. Callers
// hold mu.
func (c *Camera) notify() {
	if !c.notifier.Ready() {
		return
	}
	payload, err := json.Marshal(messages.VideoPayload{On: c.sending})
	if err != nil {
		slog.Error("encode camera state", "error", err)
		return
	}
	if err := c.notifier.Send(messages.Message{Type: messages.TypeVideo, Payload: payload}); err != nil {
		slog.Error("send camera state", "error", err)
	}
}

// ChannelOpened tells the other peer about a camera turned on before the
// channel existed.
func (c *Camera) ChannelOpened() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sending {
		c.notify()
	}
}

// Stats reports what the camera is sending.
func (c *Camera) Stats() Stats {
	var stats Stats

	c.mu.Lock()
	stats.Sending = c.sending
	c.mu.Unlock()
	stats.Quality = c.Quality()
	if stats.Sending {
		stats.Width, stats.Height, _ = c.shaper.OutputSize()
	}
	if c.out != nil {
		stats.Sent = c.out.Stats()
	}
	return stats
}

// Close stops any pending start and releases the camera device.
func (c *Camera) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.want = false
	if c.cancel != nil {
		close(c.cancel)
		c.cancel = nil
	}
	if c.track == nil {
		return nil
	}
	err := c.track.Close()
	c.track, c.bound = nil, nil
	c.sending = false
	return err
}
