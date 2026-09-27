// Package voice cleans up call audio with the WebRTC APM. Mic and speaker go
// through the same Processor, since echo cancellation needs to see both.
package voice

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/livekit/livekit-cli/v2/pkg/apm"
	"github.com/pion/mediadevices/pkg/io/audio"
	"github.com/pion/mediadevices/pkg/wave"
)

// FrameSamples is 10 ms at 48 kHz, the only frame size the APM accepts.
const FrameSamples = 480

// streamDelayMs is a starting guess for speaker-to-mic delay: one output and
// one input period plus slack. The echo canceller refines it on its own.
const streamDelayMs = 40

var (
	errClosed    = errors.New("voice: processor closed")
	errFrameSize = errors.New("voice: render frames must be 10 ms")
)

// Processor wraps one APM. Close can run while audio is still flowing.
type Processor struct {
	mu  sync.Mutex
	apm *apm.APM

	// Keeps a failing APM from logging every 10 ms.
	warned atomic.Bool
}

// New creates a mono Processor with echo cancellation, noise suppression,
// automatic gain and a high-pass filter.
func New() (*Processor, error) {
	a, err := apm.NewAPM(apm.APMConfig{
		EchoCanceller:   true,
		GainController:  true,
		HighPassFilter:  true,
		NoiseSuppressor: true,
		CaptureChannels: 1,
		RenderChannels:  1,
	})
	if err != nil {
		return nil, err
	}
	a.SetStreamDelayMs(streamDelayMs)
	return &Processor{apm: a}, nil
}

// Render shows the APM a 10 ms frame right before the speaker plays it, so its
// echo can be removed from the mic. The frame may be adjusted in place; play
// it as it comes back.
func (p *Processor) Render(frame []int16) error {
	if len(frame) != FrameSamples {
		return errFrameSize
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.apm == nil {
		return errClosed
	}
	return p.apm.ProcessRender(frame)
}

// CaptureTransform cuts the mic audio into 10 ms frames and cleans each one.
// Expects 48 kHz mono int16. If something fails, the audio goes out raw
// rather than silent.
func (p *Processor) CaptureTransform() audio.TransformFunc {
	process := func(r audio.Reader) audio.Reader {
		return audio.ReaderFunc(func() (wave.Audio, func(), error) {
			chunk, release, err := r.Read()
			if err != nil {
				return nil, func() {}, err
			}
			if err := p.processCapture(chunk); err != nil {
				p.warnOnce(err)
			}
			return chunk, release, nil
		})
	}
	return audio.Merge(audio.NewBuffer(FrameSamples), process)
}

func (p *Processor) processCapture(chunk wave.Audio) error {
	frame, ok := chunk.(*wave.Int16Interleaved)
	if !ok {
		return errors.New("voice: capture is not int16, pass SampleSize 2 and IsFloat false")
	}
	if frame.Size.Channels != 1 || frame.Size.SamplingRate != 48000 {
		return errors.New("voice: capture must be 48 kHz mono")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.apm == nil {
		return errClosed
	}
	return p.apm.ProcessCapture(frame.Data)
}

func (p *Processor) warnOnce(err error) {
	if errors.Is(err, errClosed) || p.warned.Swap(true) {
		return
	}
	slog.Warn("voice: sending unprocessed audio", "error", err)
}

// Close frees the APM. Audio read after this goes out raw.
func (p *Processor) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.apm != nil {
		p.apm.Close()
		p.apm = nil
	}
}
