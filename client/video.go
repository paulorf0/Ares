package client

import (
	"fmt"
	"image"

	"github.com/paulorf0/Ares/client/internal/camera"
	"github.com/paulorf0/Ares/rtpstream"
	videoin "github.com/paulorf0/Ares/video"
)

var (
	// ErrCameraNoPicture reports a camera that opened but sent nothing. The
	// Windows driver opens a camera held by another app without complaint and
	// then never delivers a frame.
	ErrCameraNoPicture = camera.ErrNoPicture
	ErrClosed          = camera.ErrClosed
)

// VideoQuality is how much of the camera is sent: the lower, the smaller the
// picture, the fewer frames and bits, and the less CPU on both sides.
type VideoQuality int

const (
	QualityHigh    = VideoQuality(camera.High)    // 640 wide, 30 fps, 1 Mbps
	QualityMedium  = VideoQuality(camera.Medium)  // 480 wide, 24 fps, 500 kbps
	QualityLow     = VideoQuality(camera.Low)     // 320 wide, 15 fps, 250 kbps
	QualityMinimum = VideoQuality(camera.Minimum) // 160 wide, 10 fps, 100 kbps
)

func (q VideoQuality) String() string {
	if !camera.Quality(q).Valid() {
		return fmt.Sprintf("VideoQuality(%d)", int(q))
	}
	return camera.Quality(q).Name()
}

// ParseVideoQuality reads a level by name, in English or Portuguese
// (high/alta, medium/media, low/baixa, minimum/minima).
func ParseVideoQuality(s string) (VideoQuality, error) {
	q, err := camera.Parse(s)
	return VideoQuality(q), err
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

// SetVideoQuality changes how much of the camera is sent. With the camera on,
// the encoder is rebuilt at the new level while the camera stays open, which
// freezes the picture for a moment; otherwise the level applies the next time
// it comes on.
func (c *Client) SetVideoQuality(q VideoQuality) error {
	return c.camera.SetQuality(camera.Quality(q))
}

// VideoQuality reports the camera quality level.
func (c *Client) VideoQuality() VideoQuality {
	return VideoQuality(c.camera.Quality())
}

// SetCamera turns the camera on or off. It returns at once: turning it on
// starts a background attempt that keeps trying, a new open each time, until
// the camera delivers a picture, so a camera busy in another app comes on by
// itself once freed. OnCameraStatus reports how it goes. It can be called at
// any time, even before the other peer joins.
func (c *Client) SetCamera(on bool) error {
	return c.camera.Set(on)
}

// CameraOn reports whether the camera was asked to be on, whether or not it
// is sending yet; Stats tells the two apart.
func (c *Client) CameraOn() bool {
	return c.camera.Wanted()
}

// OnCameraStatus registers fn to hear how the camera is doing: sending, off,
// or waiting on a camera that could not start, with the reason. Calls come
// one at a time from background goroutines; fn must not call SetCamera.
func (c *Client) OnCameraStatus(fn func(sending bool, err error)) {
	c.camera.OnStatus(fn)
}

// OnRemoteVideo registers fn to receive each picture of the other peer's
// video. The picture is reused: it is only valid during the call, and fn runs
// on the network goroutine, so it should copy what it needs and return.
func (c *Client) OnRemoteVideo(fn func(*image.YCbCr)) {
	c.remote.OnFrame(fn)
}

// OnRemoteCamera registers fn to hear when the other peer turns its camera on
// or off. When it goes off the pictures stop and the last one would stay
// frozen, so this is the cue to stop showing it. The end of the call counts
// as off.
func (c *Client) OnRemoteCamera(fn func(on bool)) {
	c.remote.OnCamera(fn)
}

func (c *Client) videoStats() VideoStats {
	sent := c.camera.Stats()
	received := c.remote.Stats()
	return VideoStats{
		Sending:        sent.Sending,
		Quality:        VideoQuality(sent.Quality),
		Width:          sent.Width,
		Height:         sent.Height,
		Sent:           sent.Sent,
		RemoteCameraOn: received.RemoteCameraOn,
		Receiving:      received.Receiving,
	}
}
