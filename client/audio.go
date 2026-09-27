package client

import "github.com/paulorf0/Ares/client/internal/audio"

// VoiceMode says when the mic is sent.
type VoiceMode int

const (
	// VoiceOpen sends the mic all the time.
	VoiceOpen = VoiceMode(audio.Open)
	// VoicePushToTalk sends it only while SetTalking(true). Silent stretches
	// cost no CPU: capture processing and encoding stop.
	VoicePushToTalk = VoiceMode(audio.PushToTalk)
)

// SetVoiceMode switches between an open mic and push-to-talk. Push-to-talk
// starts silent until SetTalking(true).
func (c *Client) SetVoiceMode(mode VoiceMode) error {
	if c.audio == nil {
		return ErrAudioDisabled
	}
	return c.audio.SetMode(audio.Mode(mode))
}

// SetTalking opens or closes the mic in push-to-talk. With an open mic it only
// records the flag.
func (c *Client) SetTalking(talking bool) error {
	if c.audio == nil {
		return ErrAudioDisabled
	}
	return c.audio.SetTalking(talking)
}
