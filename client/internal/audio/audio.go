// Package audio is the voice side of a call: the mic, cleaned up by the voice
// processor and sent while the voice mode allows, and the other peer's audio
// played through the same processor so the echo can be removed.
package audio

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/paulorf0/Ares/microphone"
	"github.com/paulorf0/Ares/rtpstream"
	"github.com/paulorf0/Ares/speaker"
	"github.com/paulorf0/Ares/voice"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v4"
)

// Mode says when the mic is sent.
type Mode int

const (
	// Open sends the mic all the time.
	Open Mode = iota
	// PushToTalk sends it only while talking.
	PushToTalk
)

// Call is the mic and the speaker of one call.
type Call struct {
	track     *mediadevices.AudioTrack
	out       *rtpstream.Track // track as bound to the sender
	sender    *webrtc.RTPSender
	processor *voice.Processor

	// Push-to-talk state. The track can only be swapped once the sender has
	// started, which happens while the descriptions are set.
	voiceMu        sync.Mutex
	mode           Mode
	talking        bool
	transmitting   bool
	sendersStarted bool

	// Mic chunks dropped while sending: the count at the last time sending
	// started, and what was lost in earlier stretches.
	micDropBase uint64
	micDropSent uint64
	ended       bool

	playerMu sync.Mutex
	player   *speaker.Player
	stream   *speaker.Stream
	stopped  bool
}

// Stats describes the audio in both directions.
type Stats struct {
	Receiving    *speaker.Stats
	Transmitting bool
	MicDropped   uint64
}

// New opens the mic and adds it to pc before the first offer. AddTrack makes
// the transceiver sendrecv, so both peers can talk after the first
// negotiation. The capture itself only starts once the track is bound.
func New(pc *webrtc.PeerConnection, codecs *mediadevices.CodecSelector, mode Mode) (*Call, error) {
	a := &Call{mode: mode}
	if err := a.openMicrophone(codecs); err != nil {
		return nil, err
	}

	sender, err := pc.AddTrack(a.out)
	if err != nil {
		a.track.Close()
		a.processor.Close()
		return nil, fmt.Errorf("add audio track: %w", err)
	}
	a.sender = sender
	a.transmitting = true

	// RTCP feedback for the sender is only processed while something reads it.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	return a, nil
}

// openMicrophone opens the mic and sets up cleanup and Opus.
func (a *Call) openMicrophone(codecs *mediadevices.CodecSelector) error {
	stream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		// Mono int16 is what the voice processor takes.
		Audio: func(constraints *mediadevices.MediaTrackConstraints) {
			constraints.SampleRate = prop.Int(48000)
			constraints.ChannelCount = prop.IntExact(1)
			constraints.SampleSize = prop.IntExact(2)
			constraints.IsFloat = prop.BoolExact(false)
		},
		Codec: codecs,
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

	a.track = audioTrack
	a.out = rtpstream.Wrap(audioTrack)
	a.processor = processor
	return nil
}

// Play plays the other peer's audio until the track ends.
func (a *Call) Play(track *webrtc.TrackRemote) {
	stream, err := a.startPlayback()
	if err != nil {
		slog.Error("start playback", "error", err)
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
			stream.Push(pkt, time.Now())
		}
	}
}

// startPlayback opens the speaker. Every played frame goes through the voice
// processor too, which is how the echo gets removed from the mic.
func (a *Call) startPlayback() (*speaker.Stream, error) {
	stream, err := speaker.NewStream()
	if err != nil {
		return nil, err
	}
	player, err := speaker.NewPlayer(stream, func(frame []int16) {
		_ = a.processor.Render(frame)
	})
	if err != nil {
		return nil, err
	}

	a.playerMu.Lock()
	defer a.playerMu.Unlock()
	if a.stopped {
		player.Close()
		return nil, errors.New("client closed")
	}
	a.player = player
	a.stream = stream
	return stream, nil
}

// SetMode switches between an open mic and push-to-talk.
func (a *Call) SetMode(mode Mode) error {
	a.voiceMu.Lock()
	defer a.voiceMu.Unlock()
	a.mode = mode
	return a.applyVoice()
}

// SetTalking opens or closes the mic in push-to-talk. With an open mic it only
// records the flag.
func (a *Call) SetTalking(talking bool) error {
	a.voiceMu.Lock()
	defer a.voiceMu.Unlock()
	a.talking = talking
	return a.applyVoice()
}

// SendersReady runs once the descriptions have started the RTP senders; from
// then on the mic track can be swapped out.
func (a *Call) SendersReady() {
	a.voiceMu.Lock()
	defer a.voiceMu.Unlock()
	a.sendersStarted = true
	a.micDropBase = microphone.DroppedChunks()
	if err := a.applyVoice(); err != nil {
		slog.Error("apply voice mode", "error", err)
	}
}

// applyVoice sends or stops the mic to match the current mode. Removing the
// track stops the encoder and the capture processing, not just the sending.
// Before the senders start, pion refuses a missing track, so it waits; nothing
// goes out before then anyway. Callers hold voiceMu.
func (a *Call) applyVoice() error {
	want := a.mode == Open || a.talking
	if !a.sendersStarted || a.ended || want == a.transmitting {
		return nil
	}

	var track webrtc.TrackLocal
	if want {
		track = a.out
	}
	if err := a.sender.ReplaceTrack(track); err != nil {
		return fmt.Errorf("switch mic: %w", err)
	}
	if want {
		a.micDropBase = microphone.DroppedChunks()
	} else {
		a.micDropSent += microphone.DroppedChunks() - a.micDropBase
	}
	a.transmitting = want
	return nil
}

// End freezes the sending numbers once the connection is gone. The mic is no
// longer read from then on, so its dropped chunks mean nothing.
func (a *Call) End() {
	a.voiceMu.Lock()
	defer a.voiceMu.Unlock()
	if a.ended {
		return
	}
	if a.sendersStarted && a.transmitting {
		a.micDropSent += microphone.DroppedChunks() - a.micDropBase
	}
	a.transmitting = false
	a.ended = true
}

// Stats reports what is being received and sent.
func (a *Call) Stats() Stats {
	var stats Stats

	a.playerMu.Lock()
	if a.stream != nil {
		received := a.stream.Stats()
		stats.Receiving = &received
	}
	a.playerMu.Unlock()

	a.voiceMu.Lock()
	defer a.voiceMu.Unlock()
	if a.sendersStarted {
		stats.Transmitting = a.transmitting
		stats.MicDropped = a.micDropSent
		if a.transmitting {
			stats.MicDropped += microphone.DroppedChunks() - a.micDropBase
		}
	}
	return stats
}

// Close releases the mic and the speaker. Call it after the peer connection is
// closed.
func (a *Call) Close() error {
	err := a.track.Close()

	a.playerMu.Lock()
	a.stopped = true
	if a.player != nil {
		a.player.Close()
	}
	a.playerMu.Unlock()

	// Last, so neither side of the audio reaches a closed processor.
	a.processor.Close()
	return err
}
