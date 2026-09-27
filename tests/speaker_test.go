package tests

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/io/audio"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/mediadevices/pkg/wave"
	"github.com/pion/rtp"

	"github.com/paulorf0/Ares/speaker"
)

const (
	packetSamples = 960 // 20 ms, what the client's encoder sends
	packetTime    = 20 * time.Millisecond
	toneAmplitude = 8000
)

// talkPackets encodes n packets with the client's Opus encoder, numbered from
// seq and ts. Packet i carries a 440 Hz tone if talking(i), silence otherwise.
func talkPackets(t *testing.T, n int, seq uint16, ts uint32, talking func(i int) bool) []*rtp.Packet {
	t.Helper()

	phase, packet := 0, 0
	source := audio.ReaderFunc(func() (wave.Audio, func(), error) {
		chunk := wave.NewInt16Interleaved(wave.ChunkInfo{Len: packetSamples, Channels: 1, SamplingRate: 48000})
		on := talking(packet)
		packet++
		for i := range chunk.Data {
			if on {
				chunk.Data[i] = int16(toneAmplitude * math.Sin(2*math.Pi*440*float64(phase)/48000))
			}
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

func tonePackets(t *testing.T, n int, seq uint16, ts uint32) []*rtp.Packet {
	return talkPackets(t, n, seq, ts, func(int) bool { return true })
}

// call plays packets through a Stream on a simulated clock. The sender emits
// one packet every 20 ms, the network adds a delay per packet, and the speaker
// reads 10 ms every tick.
type call struct {
	t        *testing.T
	stream   *speaker.Stream
	now      time.Time
	levels   []float64
	buffered []time.Duration
}

func newCall(t *testing.T) *call {
	t.Helper()
	stream, err := speaker.NewStream()
	if err != nil {
		t.Fatal(err)
	}
	return &call{t: t, stream: stream, now: time.Unix(0, 0)}
}

// send plays packets, delayed by delay(i), and returns the levels of the
// frames played meanwhile. It stops once the last packet has arrived.
func (c *call) send(packets []*rtp.Packet, delay func(i int) time.Duration) []float64 {
	type arrival struct {
		at  time.Time
		pkt *rtp.Packet
	}
	start := c.now
	arrivals := make([]arrival, len(packets))
	for i, pkt := range packets {
		arrivals[i] = arrival{start.Add(time.Duration(i)*packetTime + delay(i)), pkt}
	}
	sort.SliceStable(arrivals, func(a, b int) bool { return arrivals[a].at.Before(arrivals[b].at) })

	from := len(c.levels)
	for len(arrivals) > 0 {
		for len(arrivals) > 0 && !arrivals[0].at.After(c.now) {
			c.stream.Push(arrivals[0].pkt, arrivals[0].at)
			arrivals = arrivals[1:]
		}
		c.tick()
	}
	return c.levels[from:]
}

// drain plays on until whatever is buffered is out.
func (c *call) drain() []float64 {
	from := len(c.levels)
	for range 60 {
		c.tick()
	}
	return c.levels[from:]
}

func (c *call) tick() {
	frame := make([]int16, 480)
	c.stream.Read(frame)
	c.levels = append(c.levels, frameLevel(frame))
	c.buffered = append(c.buffered, c.stream.Stats().Buffered)
	c.now = c.now.Add(10 * time.Millisecond)
}

// averageDelay is the mean buffered audio over the last second.
func (c *call) averageDelay() time.Duration {
	last := c.buffered[len(c.buffered)-100:]
	var sum time.Duration
	for _, d := range last {
		sum += d
	}
	return sum / time.Duration(len(last))
}

// speech is on for 400 ms and off for 200 ms, like syllables and pauses.
func speech(i int) bool { return i%30 < 20 }

func onTime(int) time.Duration { return 0 }

// play sends packets on time and drains, returning every frame's level.
func play(t *testing.T, packets []*rtp.Packet, delay func(i int) time.Duration) ([]float64, speaker.Stats) {
	t.Helper()
	c := newCall(t)
	c.send(packets, delay)
	c.drain()
	return c.levels, c.stream.Stats()
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

func TestSpeakerPlaysEverythingThatArrives(t *testing.T) {
	levels, stats := play(t, tonePackets(t, 50, 1000, 0), onTime)

	// 1 s in, 1 s out. Opus warms up over its first frames, hence the slack.
	if got := toneFrames(levels); got < 97 || got > 100 {
		t.Fatalf("played %d frames of tone, want about 100", got)
	}
	if stats.Lost != 0 || stats.Underruns != 0 {
		t.Errorf("clean network: lost %d, underruns %d", stats.Lost, stats.Underruns)
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
	inOrder, _ := play(t, packets, onTime)

	// Every seventh packet overtakes the one before it.
	swapped, stats := play(t, packets, func(i int) time.Duration {
		if i >= 10 && i%7 == 3 {
			return packetTime + time.Millisecond
		}
		return 0
	})

	if toneFrames(swapped) != toneFrames(inOrder) || stats.Lost != 0 {
		t.Errorf("reordered: %d frames of tone, %d lost; in order: %d",
			toneFrames(swapped), stats.Lost, toneFrames(inOrder))
	}
}

func TestSpeakerLosesOnlyTheMissingPacket(t *testing.T) {
	packets := tonePackets(t, 50, 1000, 0)
	inOrder, _ := play(t, packets, onTime)

	lossy := append(append([]*rtp.Packet(nil), packets[:25]...), packets[26:]...)
	got, stats := play(t, lossy, onTime)

	if stats.Lost != 1 {
		t.Errorf("Lost = %d, want 1", stats.Lost)
	}
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

func TestSpeakerPlaysALatePacketInTime(t *testing.T) {
	packets := tonePackets(t, 50, 1000, 0)
	inOrder, _ := play(t, packets, onTime)

	// Packet 25 shows up 60 ms late, behind three newer ones.
	got, stats := play(t, packets, func(i int) time.Duration {
		if i == 25 {
			return 60 * time.Millisecond
		}
		return 0
	})

	if stats.Late != 0 || stats.Lost != 0 {
		t.Errorf("a packet 60 ms late: late %d, lost %d, want it played", stats.Late, stats.Lost)
	}
	if toneFrames(got) != toneFrames(inOrder) {
		t.Errorf("played %d frames of tone, on time %d", toneFrames(got), toneFrames(inOrder))
	}
}

func TestSpeakerDropsAPacketThatComesTooLate(t *testing.T) {
	packets := tonePackets(t, 50, 1000, 0)
	inOrder, _ := play(t, packets, onTime)

	got, stats := play(t, packets, func(i int) time.Duration {
		if i == 25 {
			return 400 * time.Millisecond
		}
		return 0
	})

	if stats.Late != 1 || stats.Lost != 1 {
		t.Errorf("a packet 400 ms late: late %d, lost %d, want 1 and 1", stats.Late, stats.Lost)
	}
	if lost := toneFrames(inOrder) - toneFrames(got); lost > 3 {
		t.Errorf("waiting for it cost %d frames of tone", lost)
	}
}

func TestSpeakerAbsorbsJitter(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	jitter := func(int) time.Duration { return time.Duration(rng.IntN(100)) * time.Millisecond }
	packets := talkPackets(t, 300, 1000, 0, speech)
	c := newCall(t)

	// Two seconds to learn the network, then four more on the same jitter.
	c.send(packets[:100], jitter)
	warm := c.stream.Stats()
	levels := append(c.send(packets[100:], jitter), c.drain()...)
	after := c.stream.Stats()

	if n := after.Underruns - warm.Underruns; n != 0 {
		t.Errorf("ran dry %d times with up to 100 ms of jitter", n)
	}
	if n := after.Lost - warm.Lost; n != 0 {
		t.Errorf("lost %d packets to up to 100 ms of jitter", n)
	}
	sent := 0
	for i := 100; i < 300; i++ {
		if speech(i) {
			sent += 2
		}
	}
	// Speech still in the buffer when the second half starts plays first.
	if got := toneFrames(levels); got < sent-4 {
		t.Errorf("played %d frames of tone, sent %d", got, sent)
	}
}

func TestSpeakerShrinksTheDelayOnSilence(t *testing.T) {
	packets := talkPackets(t, 650, 1000, 0, speech)
	rng := rand.New(rand.NewPCG(7, 8))
	c := newCall(t)

	// Three seconds of bad network push the delay up; ten calm ones follow.
	// The delay only starts coming down once the bad stretch leaves the
	// jitter window, and then slowly.
	c.send(packets[:150], func(int) time.Duration { return time.Duration(rng.IntN(150)) * time.Millisecond })
	bad := c.averageDelay()
	levels := c.send(packets[150:], onTime)
	calm := c.averageDelay()

	if calm > bad-50*time.Millisecond {
		t.Errorf("delay went from %v to %v on a calm network, want it lower", bad, calm)
	}
	if cut := c.stream.Stats().SpeechCut; cut != 0 {
		t.Errorf("cut %v of speech to shrink the delay, want silence only", cut)
	}

	sent := 0
	for i := 150; i < 650; i++ {
		if speech(i) {
			sent += 2
		}
	}
	if got := toneFrames(append(levels, c.drain()...)); got < sent-4 {
		t.Errorf("played %d frames of tone, sent %d", got, sent)
	}
}

func TestSpeakerPlaysSilenceWhenNothingArrives(t *testing.T) {
	c := newCall(t)
	frame := []int16{1, 2, 3}
	c.stream.Read(frame)
	for _, s := range frame {
		if s != 0 {
			t.Fatalf("got %v, want silence", frame)
		}
	}
}

func TestSpeakerPlaysAShortTalkSpurt(t *testing.T) {
	// A quick "ok": shorter than the delay the buffer aims for.
	levels, _ := play(t, tonePackets(t, 2, 1000, 0), onTime)
	if toneFrames(levels) < 3 {
		t.Errorf("a 40 ms spurt played %d frames of tone, want about 4", toneFrames(levels))
	}
}

func TestSpeakerResumesAfterPushToTalkPause(t *testing.T) {
	c := newCall(t)
	first := toneFrames(append(c.send(tonePackets(t, 50, 1000, 0), onTime), c.drain()...))

	// The other side talks again: its encoder starts over from new numbers.
	second := toneFrames(append(c.send(tonePackets(t, 50, 40000, 3_000_000_000), onTime), c.drain()...))

	if second < first-3 {
		t.Errorf("after the pause only %d frames of tone played, before %d", second, first)
	}
	if stats := c.stream.Stats(); stats.Underruns != 0 {
		t.Errorf("a push-to-talk pause counted as %d underruns", stats.Underruns)
	}
}

func TestSpeakerStatsSettleWhenAudioStops(t *testing.T) {
	// Speech, then the mic keeps sending silence until the call ends.
	c := newCall(t)
	c.send(talkPackets(t, 50, 1000, 0, func(i int) bool { return i < 40 }), onTime)
	c.drain()
	settled := c.stream.Stats()

	// The call is over and nothing arrives; the numbers must stay put.
	c.drain()
	c.drain()
	if after := c.stream.Stats(); after != settled {
		t.Errorf("stats kept moving with no audio:\nbefore %+v\nafter  %+v", settled, after)
	}
}

func TestSpeakerResumesAfterShortPauseWithContinuousNumbers(t *testing.T) {
	c := newCall(t)
	first := toneFrames(append(c.send(tonePackets(t, 50, 1000, 0), onTime), c.drain()...))

	// Half a second off: numbering goes on where it stopped, the timestamp
	// moves by the time that passed.
	pause := uint32(48000 / 2)
	second := toneFrames(append(c.send(tonePackets(t, 50, 1050, 50*packetSamples+pause), onTime), c.drain()...))

	if second < first-3 {
		t.Errorf("after the pause only %d frames of tone played, before %d", second, first)
	}
	stats := c.stream.Stats()
	if stats.Underruns != 0 {
		t.Errorf("a short pause counted as %d underruns", stats.Underruns)
	}
	if stats.Lost != 0 {
		t.Errorf("a short pause counted as %d lost packets", stats.Lost)
	}
}
