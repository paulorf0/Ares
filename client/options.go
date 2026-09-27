package client

import "github.com/pion/mediadevices"

// Option configures a Client at creation.
type Option func(*config)

type config struct {
	audio      bool
	voiceMode  VoiceMode
	quality    VideoQuality
	openSource func() (mediadevices.VideoSource, error)
}

// WithAudio turns on the voice call: the mic is captured and sent, and the
// other peer's audio is played. It has to be decided up front, because a track
// added after the handshake would never reach the other peer.
func WithAudio() Option {
	return func(c *config) { c.audio = true }
}

// WithVoiceMode sets the voice mode from the start, so push-to-talk never
// sends anything before the first SetTalking(true). Needs WithAudio.
func WithVoiceMode(mode VoiceMode) Option {
	return func(c *config) { c.voiceMode = mode }
}

// WithVideoQuality sets the camera quality from the start; the default is
// QualityHigh.
func WithVideoQuality(q VideoQuality) Option {
	return func(c *config) { c.quality = q }
}

// WithCameraSource replaces the camera with frames from open, called each
// time the camera is turned on. Tests use it to send video without hardware.
func WithCameraSource(open func() (mediadevices.VideoSource, error)) Option {
	return func(c *config) { c.openSource = open }
}
