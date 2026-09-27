package tests

import (
	"math"
	"testing"

	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/io/audio"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/mediadevices/pkg/wave"
	"github.com/pion/rtp"

	"github.com/paulorf0/Ares/speaker"
)

const (
	packetSamples = 960 // 20 ms, what the client's encoder sends
	toneAmplitude = 8000
)

// tonePackets encodes n packets of a 440 Hz tone with the same Opus encoder
// the client uses, numbered from seq and ts.
func tonePackets(t *testing.T, n int, seq uint16, ts uint32) []*rtp.Packet {
	t.Helper()

	phase := 0
	source := audio.ReaderFunc(func() (wave.Audio, func(), error) {
		chunk := wave.NewInt16Interleaved(wave.ChunkInfo{Len: packetSamples, Channels: 1, SamplingRate: 48000})
		for i := range chunk.Data {
			chunk.Data[i] = int16(toneAmplitude * math.Sin(2*math.Pi*440*float64(phase)/48000))
			phase++
		}
		return chunk, func() {}, nil
	})

	params, err := opus.NewParams()
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := params.BuildAudioEncoder(source, prop.Media{
		Audio: prop.Audio{SampleRate: 48000, ChannelCount: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()

	packets := make([]*rtp.Packet, n)
	for i := range packets {
		data, release, err := encoder.Read()
		if err != nil {
			t.Fatal(err)
		}
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: seq + uint16(i),
				Timestamp:      ts + uint32(i*packetSamples),
			},
			Payload: append([]byte(nil), data...),
		}
		release()
	}
	return packets
}

// play pushes packets as if they came in real time, one every 20 ms, reading
// 20 ms after each, then drains what is left. It returns the level of every
// 10 ms frame played.
func play(t *testing.T, stream *speaker.Stream, packets []*rtp.Packet) []float64 {
	t.Helper()

	var levels []float64
	frame := make([]int16, 480)
	read := func() {
		stream.Read(frame)
		levels = append(levels, frameLevel(frame))
	}

	for _, pkt := range packets {
		stream.Push(pkt)
		read()
		read()
	}
	for range 30 {
		read()
	}
	return levels
}

func frameLevel(frame []int16) float64 {
	var sum float64
	for _, s := range frame {
		sum += float64(s) * float64(s)
	}
	return 20 * math.Log10(math.Max(math.Sqrt(sum/float64(len(frame))), 1)/32768)
}

// toneFrames counts the frames that carry the tone rather than silence.
func toneFrames(levels []float64) int {
	n := 0
	for _, l := range levels {
		if l > -40 {
			n++
		}
	}
	return n
}

func newStream(t *testing.T) *speaker.Stream {
	t.Helper()
	stream, err := speaker.NewStream()
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestSpeakerPlaysEverythingThatArrives(t *testing.T) {
	levels := play(t, newStream(t), tonePackets(t, 50, 1000, 0))

	// 1 s in, 1 s out. Opus warms up over its first frames, hence the slack.
	if got := toneFrames(levels); got < 97 || got > 100 {
		t.Fatalf("played %d frames of tone, want about 100", got)
	}

	want := 20 * math.Log10(toneAmplitude/math.Sqrt2/32768) // RMS of the input tone
	var sum float64
	var n int
	for _, l := range levels {
		if l > -40 {
			sum += l
			n++
		}
	}
	if got := sum / float64(n); math.Abs(got-want) > 3 {
		t.Errorf("tone plays at %.1f dBFS, sent at %.1f", got, want)
	}
}

func TestSpeakerPutsReorderedPacketsBack(t *testing.T) {
	packets := tonePackets(t, 50, 1000, 0)
	inOrder := toneFrames(play(t, newStream(t), packets))

	shuffled := append([]*rtp.Packet(nil), packets...)
	for i := 10; i+1 < len(shuffled); i += 7 {
		shuffled[i], shuffled[i+1] = shuffled[i+1], shuffled[i]
	}

	if got := toneFrames(play(t, newStream(t), shuffled)); got != inOrder {
		t.Errorf("reordered packets played %d frames of tone, in order %d", got, inOrder)
	}
}

func TestSpeakerLosesOnlyTheMissingPacket(t *testing.T) {
	packets := tonePackets(t, 50, 1000, 0)
	inOrder := play(t, newStream(t), packets)

	lossy := append(append([]*rtp.Packet(nil), packets[:25]...), packets[26:]...)
	got := play(t, newStream(t), lossy)

	// One packet is two frames. Anything more means the loss took good audio
	// down with it.
	if lost := toneFrames(inOrder) - toneFrames(got); lost > 3 {
		t.Errorf("losing one packet dropped %d frames of tone", lost)
	}
	// The hole plays as silence instead of being skipped, so what follows
	// stays in time.
	if span(got) != span(inOrder) {
		t.Errorf("tone lasted %d frames with a loss, %d without", span(got), span(inOrder))
	}
}

// span is the number of frames from the first tone frame to the last.
func span(levels []float64) int {
	first, last := -1, -1
	for i, l := range levels {
		if l > -40 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return last - first + 1
}

func TestSpeakerPlaysSilenceWhenNothingArrives(t *testing.T) {
	stream := newStream(t)
	frame := []int16{1, 2, 3}
	stream.Read(frame)
	for _, s := range frame {
		if s != 0 {
			t.Fatalf("got %v, want silence", frame)
		}
	}
}

func TestSpeakerResumesAfterPushToTalkPause(t *testing.T) {
	stream := newStream(t)
	first := toneFrames(play(t, stream, tonePackets(t, 50, 1000, 0)))

	// The other side talks again: its encoder starts over from new numbers.
	second := toneFrames(play(t, stream, tonePackets(t, 50, 40000, 3_000_000_000)))

	if second < first-3 {
		t.Errorf("after the pause only %d frames of tone played, before %d", second, first)
	}
}
