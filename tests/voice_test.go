package tests

import (
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/pion/mediadevices/pkg/io/audio"
	"github.com/pion/mediadevices/pkg/wave"

	"github.com/paulorf0/Ares/voice"
)

// noiseSource yields fixed-seed white noise in uneven chunks, like a real
// driver, and ends after total samples.
func noiseSource(amplitude, total int) audio.Reader {
	rng := rand.New(rand.NewPCG(1, 2))
	sizes := []int{700, 1100, 300, 1900}
	sent := 0
	return audio.ReaderFunc(func() (wave.Audio, func(), error) {
		if sent >= total {
			return nil, func() {}, io.EOF
		}
		n := min(sizes[sent%len(sizes)], total-sent)
		sent += n
		chunk := wave.NewInt16Interleaved(wave.ChunkInfo{Len: n, Channels: 1, SamplingRate: 48000})
		for i := range chunk.Data {
			chunk.Data[i] = int16(rng.IntN(2*amplitude) - amplitude)
		}
		return chunk, func() {}, nil
	})
}

func levelDB(samples []int16) float64 {
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return 20 * math.Log10(math.Sqrt(sum/float64(len(samples)))/32768)
}

func readSamples(t *testing.T, r audio.Reader) []int16 {
	t.Helper()
	chunk, _, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return chunk.(*wave.Int16Interleaved).Data
}

func newProcessor(t *testing.T) *voice.Processor {
	t.Helper()
	p, err := voice.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestVoiceKeepsEverySample(t *testing.T) {
	const total = 48000 * 2 // 2 s, a whole number of 10 ms frames
	r := newProcessor(t).CaptureTransform()(noiseSource(4000, total))

	got := 0
	for {
		chunk, _, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got += chunk.ChunkInfo().Len
	}

	if got != total {
		t.Errorf("got %d samples out, sent %d", got, total)
	}
}

func TestVoiceSuppressesSteadyNoise(t *testing.T) {
	const amplitude = 4000
	input := levelDB(readSamples(t, noiseSource(amplitude, 48000)))

	r := newProcessor(t).CaptureTransform()(noiseSource(amplitude, 48000*5))
	// Give it a couple of seconds to learn the noise, then measure.
	var output []int16
	for len(output) < 48000*3 {
		output = append(output, readSamples(t, r)...)
	}
	output = output[48000*2:]

	if got := levelDB(output); got > input-10 {
		t.Errorf("noise went from %.1f to %.1f dBFS, want at least 10 dB less", input, got)
	}
}

func TestVoicePassesAudioThroughAfterClose(t *testing.T) {
	p := newProcessor(t)
	r := p.CaptureTransform()(noiseSource(4000, 48000))
	readSamples(t, r)
	p.Close()

	// A closed processor must not silence the call.
	if got := levelDB(readSamples(t, r)); got < -40 {
		t.Errorf("audio after Close is %.1f dBFS, looks silenced", got)
	}
}
