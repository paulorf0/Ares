// Package microphone registers every capture device found by miniaudio as a
// mediadevices driver. It replaces mediadevices/pkg/driver/microphone, which
// only exposes devices whose native format is already F32 or S16 and so drops
// microphones that PipeWire, PulseAudio or WASAPI report as S32 or S24.
//
// Here every device advertises F32 and S16 at 48 kHz in mono and stereo,
// whatever its native format; miniaudio converts to the requested format.
package microphone

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/pion/mediadevices/pkg/driver"
	"github.com/pion/mediadevices/pkg/io/audio"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/mediadevices/pkg/wave"
)

const (
	// Opus only runs at 48 kHz; asking for it avoids resampling in the encoder.
	sampleRate = 48000
	latency    = 20 * time.Millisecond
)

var errUnsupportedFormat = errors.New("microphone: unsupported sample format")

var (
	malgoCtx   *malgo.AllocatedContext
	bigEndian  = binary.NativeEndian.Uint16([]byte{0x12, 0x34}) == 0x1234
	hostEndian = binary.NativeEndian
)

func init() {
	if err := register(); err != nil {
		slog.Error("microphone: register capture devices", "error", err)
	}
}

// register opens a miniaudio context and adds one driver per capture device.
// A failure leaves no microphone registered instead of crashing the program,
// so machines without audio still run.
func register() error {
	var err error
	malgoCtx, err = malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return err
	}

	devices, err := malgoCtx.Devices(malgo.Capture)
	if err != nil {
		return err
	}

	for _, device := range devices {
		info, err := malgoCtx.DeviceInfo(malgo.Capture, device.ID, malgo.Shared)
		if err != nil {
			slog.Warn("microphone: skip device", "name", device.Name(), "error", err)
			continue
		}

		priority := driver.PriorityNormal
		if info.IsDefault > 0 {
			priority = driver.PriorityHigh
		}
		err = driver.GetManager().Register(&microphone{id: info.ID}, driver.Info{
			Label:      info.ID.String(),
			DeviceType: driver.Microphone,
			Priority:   priority,
			Name:       info.Name(),
		})
		if err != nil {
			slog.Warn("microphone: register device", "name", info.Name(), "error", err)
		}
	}

	return nil
}

type microphone struct {
	id malgo.DeviceID

	chunks    chan []byte
	closeFunc func()
}

func (m *microphone) Open() error {
	m.chunks = make(chan []byte, 1)
	return nil
}

func (m *microphone) Close() error {
	if m.closeFunc != nil {
		m.closeFunc()
	}
	return nil
}

// Properties advertises the formats miniaudio can deliver, not the device's
// native ones.
func (m *microphone) Properties() []prop.Media {
	var props []prop.Media
	for _, channels := range []int{2, 1} {
		for _, isFloat := range []bool{true, false} {
			size := 2
			if isFloat {
				size = 4
			}
			props = append(props, prop.Media{
				Audio: prop.Audio{
					ChannelCount:  channels,
					SampleRate:    sampleRate,
					Latency:       latency,
					SampleSize:    size,
					IsFloat:       isFloat,
					IsBigEndian:   bigEndian,
					IsInterleaved: true,
				},
			})
		}
	}
	return props
}

// AudioRecord starts capturing in the requested format and returns a reader
// that yields one chunk per miniaudio period.
func (m *microphone) AudioRecord(p prop.Media) (audio.Reader, error) {
	var format malgo.FormatType
	switch {
	case p.SampleSize == 4 && p.IsFloat:
		format = malgo.FormatF32
	case p.SampleSize == 2 && !p.IsFloat:
		format = malgo.FormatS16
	default:
		return nil, errUnsupportedFormat
	}

	decoder, err := wave.NewDecoder(&wave.RawFormat{
		SampleSize:  p.SampleSize,
		IsFloat:     p.IsFloat,
		Interleaved: p.IsInterleaved,
	})
	if err != nil {
		return nil, err
	}

	config := malgo.DefaultDeviceConfig(malgo.Capture)
	config.PerformanceProfile = malgo.LowLatency
	config.Capture.DeviceID = m.id.Pointer()
	config.Capture.Format = format
	config.Capture.Channels = uint32(p.ChannelCount)
	config.SampleRate = uint32(p.SampleRate)
	config.PeriodSizeInMilliseconds = uint32(p.Latency.Milliseconds())

	cancelCtx, cancel := context.WithCancel(context.Background())
	chunks := m.chunks

	callbacks := malgo.DeviceCallbacks{
		// The input slice points at miniaudio's buffer, which is reused after
		// the callback returns, so it has to be copied before leaving.
		Data: func(_, input []byte, _ uint32) {
			chunk := make([]byte, len(input))
			copy(chunk, input)
			select {
			case <-cancelCtx.Done():
			case chunks <- chunk:
			}
		},
	}

	device, err := malgo.InitDevice(malgoCtx.Context, config, callbacks)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := device.Start(); err != nil {
		cancel()
		device.Uninit()
		return nil, err
	}

	var once sync.Once
	m.closeFunc = func() {
		once.Do(func() {
			// Cancel first so a blocked callback returns and Uninit can finish.
			cancel()
			device.Uninit()
			close(chunks)
		})
	}

	reader := audio.ReaderFunc(func() (wave.Audio, func(), error) {
		chunk, ok := <-chunks
		if !ok {
			return nil, func() {}, io.EOF
		}

		decoded, err := decoder.Decode(hostEndian, chunk, p.ChannelCount)
		if err != nil {
			return nil, func() {}, err
		}

		// The decoder does not know the sample rate, so it is filled in here.
		switch decoded := decoded.(type) {
		case *wave.Float32Interleaved:
			decoded.Size.SamplingRate = p.SampleRate
		case *wave.Int16Interleaved:
			decoded.Size.SamplingRate = p.SampleRate
		}
		return decoded, func() {}, nil
	})

	return reader, nil
}
