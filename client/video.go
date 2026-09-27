package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec"
	"github.com/pion/mediadevices/pkg/codec/openh264"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/paulorf0/Ares/framing"
	"github.com/paulorf0/Ares/messages"
	"github.com/paulorf0/Ares/rtpstream"
	videoin "github.com/paulorf0/Ares/video"
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
	// ErrCameraNoPicture reports a camera that opened but sent nothing. The
	// Windows driver opens a camera held by another app without complaint and
	// then never delivers a frame.
	ErrCameraNoPicture = errors.New("client: camera sent no picture, another app may be using it")
	ErrClosed          = errors.New("client: closed")
)

// VideoQuality is how much of the camera is sent: the lower, the smaller the
// picture, the fewer frames and bits, and the less CPU on both sides.
type VideoQuality int

const (
	QualityHigh    VideoQuality = iota // 640 wide, 30 fps, 1 Mbps
	QualityMedium                      // 480 wide, 24 fps, 500 kbps
	QualityLow                         // 320 wide, 15 fps, 250 kbps
	QualityMinimum                     // 160 wide, 10 fps, 100 kbps
)

var qualityLevels = [...]struct {
	name, alias string
	width       int
	frameRate   float64
	bitRate     int
}{
	QualityHigh:    {"high", "alta", 640, 30, 1_000_000},
	QualityMedium:  {"medium", "media", 480, 24, 500_000},
	QualityLow:     {"low", "baixa", 320, 15, 250_000},
	QualityMinimum: {"minimum", "minima", 160, 10, 100_000},
}

func (q VideoQuality) valid() bool {
	return q >= 0 && int(q) < len(qualityLevels)
}

func (q VideoQuality) String() string {
	if !q.valid() {
		return fmt.Sprintf("VideoQuality(%d)", int(q))
	}
	return qualityLevels[q].name
}

func (q VideoQuality) shape() framing.Shape {
	return framing.Shape{Width: qualityLevels[q].width, FrameRate: qualityLevels[q].frameRate}
}

// ParseVideoQuality reads a level by name, in English or Portuguese
// (high/alta, medium/media, low/baixa, minimum/minima).
func ParseVideoQuality(s string) (VideoQuality, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for q, level := range qualityLevels {
		if s == level.name || s == level.alias {
			return VideoQuality(q), nil
		}
	}
	return 0, fmt.Errorf("client: unknown video quality %q", s)
}

// WithVideoQuality sets the camera quality from the start; the default is
// QualityHigh.
func WithVideoQuality(q VideoQuality) Option {
	return func(c *Client) { c.video.quality.Store(int32(q)) }
}

// WithCameraSource replaces the camera with frames from open, called each
// time the camera is turned on. Tests use it to send video without hardware.
func WithCameraSource(open func() (mediadevices.VideoSource, error)) Option {
	return func(c *Client) { c.video.openSource = open }
}

// videoState is the camera and the other peer's video. Every call carries a
// video transceiver from the first offer, so the camera can be turned on and
// off at any point without renegotiating.
type videoState struct {
	openSource func() (mediadevices.VideoSource, error)
	sender     *webrtc.RTPSender
	idle       webrtc.TrackLocal // pion's placeholder: bound while off, sends nothing
	out        *rtpstream.Stream
	shaper     *framing.Shaper

	// Read by the encoder builder, which runs inside pion while mu is held.
	quality atomic.Int32

	mu      sync.Mutex
	want    bool               // what SetCamera asked for
	sending bool               // the camera is bound to the sender
	track   mediadevices.Track // the open camera while sending
	bound   webrtc.TrackLocal  // track as handed to the sender
	cancel  chan struct{}      // stops the pending start; nil when none runs
	closed  bool

	onFrame  func(*image.YCbCr) // under handlerMu, like onMessage
	onCamera func(on bool)

	statusMu  sync.Mutex
	onStatus  func(sending bool, err error)
	statusKey string // last status reported, so repeats are skipped

	inMu     sync.Mutex
	in       *videoin.Stream
	remoteOn bool
}

// VideoStats describes the video in both directions.
type VideoStats struct {
	// Sending reports whether the camera is being sent right now.
	Sending bool
	// Quality is the level chosen; Width and Height the size sent, zero
	// while not sending.
	Quality       VideoQuality
	Width, Height int
	// Sent counts what the camera sent since the call started.
	Sent rtpstream.Stats
	// RemoteCameraOn reports whether the other peer says its camera is on.
	RemoteCameraOn bool
	// Receiving describes the other peer's video, nil until any came in.
	Receiving *videoin.Stats
}

func videoParams(v *videoState) (*h264Builder, error) {
	params, err := openh264.NewParams()
	if err != nil {
		return nil, err
	}
	params.IntraPeriod = videoKeyFrameInterval
	return &h264Builder{Params: params, video: v}, nil
}

// h264Builder builds the openh264 encoder for the current quality level,
// each time the camera track is bound. The frame rate comes from the level:
// mediadevices measures it from the frames and hands 0 for the first one,
// which turns off openh264's rate control and the bitrate target with it.
type h264Builder struct {
	openh264.Params
	video *videoState
}

func (b *h264Builder) BuildVideoEncoder(r video.Reader, p prop.Media) (codec.ReadCloser, error) {
	level := qualityLevels[b.video.quality.Load()]
	params := b.Params
	params.BitRate = level.bitRate
	p.FrameRate = float32(level.frameRate)
	return params.BuildVideoEncoder(r, p)
}

// addVideoTransceiver reserves the video slot before the first offer. pion
// fills the sender with a placeholder track that never writes, which is what
// the sender goes back to when the camera is turned off.
func (c *Client) addVideoTransceiver() error {
	tr, err := c.connRTC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	if err != nil {
		return fmt.Errorf("add video transceiver: %w", err)
	}
	c.video.sender = tr.Sender()
	c.video.idle = tr.Sender().Track()
	c.video.out = rtpstream.New()
	return nil
}

// SetVideoQuality changes how much of the camera is sent. With the camera on,
// the encoder is rebuilt at the new level while the camera stays open, which
// freezes the picture for a moment; otherwise the level applies the next time
// it comes on.
func (c *Client) SetVideoQuality(q VideoQuality) error {
	if !q.valid() {
		return fmt.Errorf("client: unknown video quality %d", int(q))
	}
	v := &c.video
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrClosed
	}
	if VideoQuality(v.quality.Load()) == q {
		v.mu.Unlock()
		return nil
	}
	if !v.sending {
		v.setQuality(q)
		v.mu.Unlock()
		return nil
	}

	// Rebinding builds a new encoder: the level's bitrate and frame rate are
	// only taken when one is created, and the frame rate sets how many bits
	// each frame gets.
	if err := v.sender.ReplaceTrack(v.idle); err != nil {
		v.mu.Unlock()
		return fmt.Errorf("change video quality: %w", err)
	}
	v.setQuality(q)
	err := v.sender.ReplaceTrack(v.bound)
	if err != nil {
		// The camera is open but no longer sent: turn it off properly.
		if cerr := v.track.Close(); cerr != nil {
			slog.Warn("close camera", "error", cerr)
		}
		v.track, v.bound = nil, nil
		v.sending, v.want = false, false
		c.sendCameraState()
	}
	v.mu.Unlock()

	if err != nil {
		c.reportCamera(nil)
		return fmt.Errorf("change video quality: %w", err)
	}
	return nil
}

// VideoQuality reports the camera quality level.
func (c *Client) VideoQuality() VideoQuality {
	return VideoQuality(c.video.quality.Load())
}

func (v *videoState) setQuality(q VideoQuality) {
	v.quality.Store(int32(q))
	v.shaper.Set(q.shape())
}

// SetCamera turns the camera on or off. It returns at once: turning it on
// starts a background attempt that keeps trying, a new open each time, until
// the camera delivers a picture, so a camera busy in another app comes on by
// itself once freed. OnCameraStatus reports how it goes. It can be called at
// any time, even before the other peer joins.
func (c *Client) SetCamera(on bool) error {
	v := &c.video
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrClosed
	}
	if on == v.want {
		v.mu.Unlock()
		return nil
	}
	v.want = on

	if on {
		v.cancel = make(chan struct{})
		go c.startCamera(v.cancel)
		v.mu.Unlock()
		return nil
	}

	if v.cancel != nil {
		close(v.cancel)
		v.cancel = nil
	}
	var err error
	if v.sending {
		// Unbound before closing: mediadevices' unbind waits for the encoder
		// goroutine, which quits without answering if the camera closed first.
		if rerr := v.sender.ReplaceTrack(v.idle); rerr != nil {
			err = fmt.Errorf("stop camera: %w", rerr)
		}
		if cerr := v.track.Close(); cerr != nil {
			slog.Warn("close camera", "error", cerr)
		}
		v.track, v.bound = nil, nil
		v.sending = false
		c.sendCameraState()
	}
	v.mu.Unlock()

	c.reportCamera(nil)
	return err
}

// CameraOn reports whether the camera was asked to be on, whether or not it
// is sending yet; Stats tells the two apart.
func (c *Client) CameraOn() bool {
	c.video.mu.Lock()
	defer c.video.mu.Unlock()
	return c.video.want
}

// OnCameraStatus registers fn to hear how the camera is doing: sending, off,
// or waiting on a camera that could not start, with the reason. Calls come
// one at a time from background goroutines; fn must not call SetCamera.
func (c *Client) OnCameraStatus(fn func(sending bool, err error)) {
	c.video.statusMu.Lock()
	defer c.video.statusMu.Unlock()
	c.video.onStatus = fn
}

// startCamera opens the camera until it works or cancel is closed. Nothing
// here holds video.mu while waiting on the camera, so the call, the chat and
// the other camera calls never wait on it.
func (c *Client) startCamera(cancel <-chan struct{}) {
	v := &c.video
	for {
		track, err := c.openCamera(cancel)

		v.mu.Lock()
		select {
		case <-cancel:
			v.mu.Unlock()
			if track != nil {
				_ = track.Close()
			}
			return
		default:
		}
		if err == nil {
			// With a picture already in, the bind inside ReplaceTrack returns
			// at once. On failure pion puts the placeholder back.
			bound := v.out.Wrap(track)
			if err = v.sender.ReplaceTrack(bound); err == nil {
				v.track, v.bound = track, bound
				v.sending = true
				v.cancel = nil
				c.sendCameraState()
				v.mu.Unlock()
				c.reportCamera(nil)
				return
			}
			_ = track.Close()
			err = fmt.Errorf("send camera: %w", err)
		}
		v.mu.Unlock()

		c.reportCamera(err)
		select {
		case <-cancel:
			return
		case <-time.After(cameraRetry):
		}
	}
}

// reportCamera tells the OnCameraStatus handler the current state, if it
// changed. err is the reason a start attempt failed; it only counts while the
// camera is still wanted.
func (c *Client) reportCamera(err error) {
	v := &c.video
	v.statusMu.Lock()
	defer v.statusMu.Unlock()

	v.mu.Lock()
	sending, want := v.sending, v.want
	v.mu.Unlock()

	key := "off"
	switch {
	case sending:
		key, err = "on", nil
	case want && err != nil:
		key = "error: " + err.Error()
	default:
		err = nil
	}
	if key == v.statusKey {
		return
	}
	v.statusKey = key
	if v.onStatus != nil {
		v.onStatus(sending, err)
	}
}

// openCamera opens the camera and waits for its first picture. Handing pion
// a camera that never delivers would hang the handshake or ReplaceTrack,
// since the encoder reads a frame when it is bound.
func (c *Client) openCamera(cancel <-chan struct{}) (mediadevices.Track, error) {
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
	videoTrack.Transform(c.video.shaper.Transform())

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
		err = ErrCameraNoPicture
	case <-cancel:
		err = ErrClosed
	}
	// Closing ends the read still waiting on the camera.
	_ = track.Close()
	return nil, err
}

func (c *Client) openTrack() (mediadevices.Track, error) {
	if open := c.video.openSource; open != nil {
		source, err := open()
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

// sendCameraState tells the other peer whether the camera is sending. Before
// the data channel opens there is no one to tell; the state goes out on open.
// Callers hold video.mu.
func (c *Client) sendCameraState() {
	channel := c.DataChannel()
	if channel == nil || channel.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	payload, err := json.Marshal(messages.VideoPayload{On: c.video.sending})
	if err != nil {
		slog.Error("encode camera state", "error", err)
		return
	}
	if err := c.SendMessage(messages.Message{Type: messages.TypeVideo, Payload: payload}); err != nil {
		slog.Error("send camera state", "error", err)
	}
}

// channelOpened tells the other peer about a camera turned on before the
// channel existed.
func (c *Client) channelOpened() {
	c.video.mu.Lock()
	defer c.video.mu.Unlock()
	if c.video.sending {
		c.sendCameraState()
	}
}

// handleControl takes the data channel messages meant for the client itself.
// It reports whether msg was one of them.
func (c *Client) handleControl(msg []byte) bool {
	var head struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(msg, &head) != nil || head.Type != messages.TypeVideo {
		return false
	}
	var state messages.VideoPayload
	if err := json.Unmarshal(head.Payload, &state); err != nil {
		slog.Warn("bad camera state from peer", "error", err)
		return true
	}
	c.setRemoteCamera(state.On)
	return true
}

func (c *Client) setRemoteCamera(on bool) {
	c.video.inMu.Lock()
	changed := c.video.remoteOn != on
	c.video.remoteOn = on
	c.video.inMu.Unlock()
	if !changed {
		return
	}

	c.handlerMu.Lock()
	fn := c.video.onCamera
	c.handlerMu.Unlock()
	if fn != nil {
		fn(on)
	}
}

// OnRemoteVideo registers fn to receive each picture of the other peer's
// video. The picture is reused: it is only valid during the call, and fn runs
// on the network goroutine, so it should copy what it needs and return.
func (c *Client) OnRemoteVideo(fn func(*image.YCbCr)) {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	c.video.onFrame = fn
}

// OnRemoteCamera registers fn to hear when the other peer turns its camera on
// or off. When it goes off the pictures stop and the last one would stay
// frozen, so this is the cue to stop showing it. The end of the call counts
// as off.
func (c *Client) OnRemoteCamera(fn func(on bool)) {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	c.video.onCamera = fn
}

// readRemoteVideo decodes the other peer's video until the track ends. Lost
// frames are asked for again as a picture loss indication.
func (c *Client) readRemoteVideo(track *webrtc.TrackRemote) {
	ssrc := uint32(track.SSRC())
	stream, err := videoin.NewStream(c.deliverFrame, func() {
		pli := []rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}
		if err := c.connRTC.WriteRTCP(pli); err != nil {
			slog.Warn("ask for key frame", "error", err)
		}
	})
	if err != nil {
		slog.Error("start video decoder", "error", err)
		return
	}
	defer stream.Close()

	c.video.inMu.Lock()
	c.video.in = stream
	c.video.inMu.Unlock()

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Error("read remote video", "error", err)
			}
			return
		}
		stream.Push(pkt, time.Now())
	}
}

func (c *Client) deliverFrame(pic *image.YCbCr) {
	c.handlerMu.Lock()
	fn := c.video.onFrame
	c.handlerMu.Unlock()
	if fn != nil {
		fn(pic)
	}
}

func (c *Client) videoStats() VideoStats {
	v := &c.video
	var stats VideoStats

	v.mu.Lock()
	stats.Sending = v.sending
	v.mu.Unlock()
	stats.Quality = c.VideoQuality()
	if stats.Sending {
		stats.Width, stats.Height, _ = v.shaper.OutputSize()
	}
	if v.out != nil {
		stats.Sent = v.out.Stats()
	}

	v.inMu.Lock()
	defer v.inMu.Unlock()
	stats.RemoteCameraOn = v.remoteOn
	if v.in != nil {
		received := v.in.Stats(time.Now())
		stats.Receiving = &received
	}
	return stats
}

// closeCamera stops any pending start and releases the camera device.
func (c *Client) closeCamera() error {
	v := &c.video
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	v.want = false
	if v.cancel != nil {
		close(v.cancel)
		v.cancel = nil
	}
	if v.track == nil {
		return nil
	}
	err := v.track.Close()
	v.track, v.bound = nil, nil
	v.sending = false
	return err
}
