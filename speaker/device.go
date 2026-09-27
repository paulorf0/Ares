package speaker

import (
	"encoding/binary"
	"sync"

	"github.com/gen2brain/malgo"
)

// Player sends a Stream to the default output device.
type Player struct {
	ctx    *malgo.AllocatedContext
	device *malgo.Device

	stream *Stream
	render func(frame []int16)

	// Only touched by the audio callback.
	frame []int16
	left  []int16

	closeOnce sync.Once
}

// NewPlayer starts playing stream. render, if set, gets every 10 ms frame
// right before it is played, silence included, which is what an echo
// canceller needs to see.
func NewPlayer(stream *Stream, render func(frame []int16)) (*Player, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}

	p := &Player{
		ctx:    ctx,
		stream: stream,
		render: render,
		frame:  make([]int16, frameSamples),
	}

	config := malgo.DefaultDeviceConfig(malgo.Playback)
	config.PerformanceProfile = malgo.LowLatency
	config.Playback.Format = malgo.FormatS16
	config.Playback.Channels = 1
	config.SampleRate = sampleRate
	config.PeriodSizeInMilliseconds = 10

	device, err := malgo.InitDevice(ctx.Context, config, malgo.DeviceCallbacks{Data: p.fill})
	if err != nil {
		p.freeContext()
		return nil, err
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		p.freeContext()
		return nil, err
	}
	p.device = device
	return p, nil
}

// fill runs on the audio thread, so it must not block or allocate. The device
// can ask for any amount, so leftovers of a 10 ms frame carry over.
func (p *Player) fill(out, _ []byte, _ uint32) {
	for len(out) >= 2 {
		if len(p.left) == 0 {
			p.stream.Read(p.frame)
			if p.render != nil {
				p.render(p.frame)
			}
			p.left = p.frame
		}

		n := min(len(p.left), len(out)/2)
		for i, s := range p.left[:n] {
			binary.NativeEndian.PutUint16(out[2*i:], uint16(s))
		}
		out = out[2*n:]
		p.left = p.left[n:]
	}
}

// Close stops playback. No render call happens after it returns.
func (p *Player) Close() {
	p.closeOnce.Do(func() {
		p.device.Uninit()
		p.freeContext()
	})
}

func (p *Player) freeContext() {
	_ = p.ctx.Uninit()
	p.ctx.Free()
}
