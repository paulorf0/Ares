// Package speaker plays the audio received from the other peer: it holds RTP
// packets in a jitter buffer, decodes the Opus and feeds the speaker.
package speaker

import (
	"slices"
	"sync"
	"time"

	"github.com/pion/opus"
	"github.com/pion/rtp"
)

const (
	sampleRate   = 48000
	frameSamples = sampleRate / 100 // 10 ms

	// A sequence jump beyond this is a new talk spurt, not loss.
	restartGap = 50
	// Timestamp gaps up to this are loss; longer ones mean a new talk spurt.
	maxGapSamples = sampleRate
	// The longest Opus packet is 120 ms.
	maxPacketSamples = sampleRate * 120 / 1000
	// What the other side's encoder sends, until the timestamps say otherwise.
	defaultPacketSamples = sampleRate * 20 / 1000

	// The buffer aims to hold enough audio to ride out the recent jitter.
	startTarget  = 80 * time.Millisecond
	minTarget    = 40 * time.Millisecond
	maxTarget    = 300 * time.Millisecond
	targetMargin = 20 * time.Millisecond
	// It grows at once but shrinks slowly, so it doesn't swing back and forth.
	shrinkPerSecond = 20 * time.Millisecond
	// Arrival delays kept to measure jitter: about 2 s of packets.
	delayWindow = 100

	// Above target plus this (or plus the jitter, when larger: the level
	// swings that much on its own), silent audio is skipped until the delay
	// is back on target. Below target, silence is stretched to let it grow.
	trimSlack = 40 * time.Millisecond
	// Above this, audio is skipped even if it is speech.
	hardLimit = 500 * time.Millisecond
	// 10 ms frames quieter than about -50 dBFS count as silence.
	silenceRMS = 100
	// Stretch credit kept at most, so a burst of packets can't bank much.
	maxStretch = 3
)

// Stats describes how the received audio has fared. Counters run from the
// start of the call.
type Stats struct {
	Received uint64 // packets that made it in time
	Lost     uint64 // packets played as silence because they never came in time
	Late     uint64 // packets that came after their turn and were thrown away

	// Times playback ran dry in the middle of a talk spurt.
	Underruns uint64

	SilenceTrimmed time.Duration // skipped to bring the delay down
	SilenceAdded   time.Duration // stretched to let the delay grow
	SpeechCut      time.Duration // skipped even though it was speech

	Jitter   time.Duration // recent spread of arrival delays
	Target   time.Duration // delay the buffer is aiming for
	Buffered time.Duration // audio waiting to be played
}

// Stream turns incoming RTP packets into a steady 48 kHz mono signal. Push
// runs on the network goroutine and Read on the audio thread.
type Stream struct {
	mu sync.Mutex

	decoder opus.Decoder
	decoded []int16
	pcmBuf  []int16 // reused for every packet, so the audio thread never allocates
	pcm     []int16 // decoded audio not played yet, inside pcmBuf

	started bool
	queue   map[uint16]*rtp.Packet
	playSeq uint16 // next packet to decode
	playTS  uint32 // timestamp of the next sample to play
	endTS   uint32 // timestamp where the newest received audio ends

	highSeq       uint16
	highTS        uint32
	packetSamples uint32

	baseArrival time.Time
	baseTS      uint32
	delays      []time.Duration
	scratch     []time.Duration
	lastAdjust  time.Time

	playing  bool
	trimming bool
	stretch  int  // 10 ms frames of silence that may be stretched; earned by arrivals
	waited   int  // samples played as silence while audio was queued
	dry      bool // ran out mid-spurt; counts as an underrun if the spurt goes on
	stats    Stats
}

// NewStream creates an empty Stream.
func NewStream() (*Stream, error) {
	decoder, err := opus.NewDecoderWithOutput(sampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &Stream{
		decoder:       decoder,
		decoded:       make([]int16, maxPacketSamples),
		pcmBuf:        make([]int16, 0, maxGapSamples+maxPacketSamples),
		queue:         make(map[uint16]*rtp.Packet),
		packetSamples: defaultPacketSamples,
		delays:        make([]time.Duration, 0, delayWindow),
		scratch:       make([]time.Duration, 0, delayWindow),
		stats:         Stats{Target: startTarget},
	}, nil
}

// Push adds a packet as it comes off the network, stamped with when it arrived.
func (s *Stream) Push(pkt *rtp.Packet, arrival time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.isRestart(pkt) {
		s.restart(pkt, arrival)
	}
	if int16(pkt.SequenceNumber-s.playSeq) < 0 {
		s.stats.Late++ // its turn already passed
		return
	}
	if _, dup := s.queue[pkt.SequenceNumber]; dup {
		return
	}

	s.queue[pkt.SequenceNumber] = pkt
	s.stats.Received++
	s.stretch = min(s.stretch+1, maxStretch)
	if s.dry {
		s.stats.Underruns++
		s.dry = false
	}

	if pkt.SequenceNumber == s.highSeq+1 {
		if n := pkt.Timestamp - s.highTS; n > 0 && n <= maxPacketSamples {
			s.packetSamples = n
		}
	}
	if int16(pkt.SequenceNumber-s.highSeq) > 0 {
		s.highSeq = pkt.SequenceNumber
		s.highTS = pkt.Timestamp
	}
	if end := pkt.Timestamp + s.packetSamples; int32(end-s.endTS) > 0 {
		s.endTS = end
	}

	s.track(pkt.Timestamp, arrival)
}

// isRestart spots the other peer starting over, as it does after each
// push-to-talk pause: sequence and timestamp jump to new random values.
func (s *Stream) isRestart(pkt *rtp.Packet) bool {
	seqGap := int16(pkt.SequenceNumber - s.playSeq)
	tsGap := int32(pkt.Timestamp - s.playTS)
	return seqGap > restartGap || seqGap < -restartGap ||
		tsGap > maxGapSamples || tsGap < -maxGapSamples
}

// restart forgets the previous talk spurt. The target delay is kept: the
// network is still the same.
func (s *Stream) restart(pkt *rtp.Packet, arrival time.Time) {
	_ = s.decoder.Init(sampleRate, 1)
	clear(s.queue)
	s.pcm = nil
	s.playing = false
	s.trimming = false
	s.stretch = 0
	s.waited = 0
	s.dry = false
	s.started = true
	s.playSeq = pkt.SequenceNumber
	s.playTS = pkt.Timestamp
	s.endTS = pkt.Timestamp
	s.highSeq = pkt.SequenceNumber - 1
	s.highTS = pkt.Timestamp - s.packetSamples
	s.baseArrival = arrival
	s.baseTS = pkt.Timestamp
	s.delays = s.delays[:0]
	s.lastAdjust = arrival
}

// track records how late a packet came compared to when it was sent, and
// moves the target delay to cover the recent spread.
func (s *Stream) track(ts uint32, arrival time.Time) {
	sent := samplesToDuration(int32(ts - s.baseTS))
	delay := arrival.Sub(s.baseArrival) - sent
	if len(s.delays) == delayWindow {
		s.delays = s.delays[1:]
	}
	s.delays = append(s.delays, delay)

	s.scratch = append(s.scratch[:0], s.delays...)
	slices.Sort(s.scratch)
	s.stats.Jitter = s.scratch[len(s.scratch)*97/100] - s.scratch[0]

	want := min(max(s.stats.Jitter+targetMargin, minTarget), maxTarget)
	if want >= s.stats.Target {
		s.stats.Target = want
	} else {
		step := shrinkPerSecond * arrival.Sub(s.lastAdjust) / time.Second
		s.stats.Target = max(want, s.stats.Target-step)
	}
	s.lastAdjust = arrival
}

// Read fills out with the next audio to play. It never blocks: with nothing
// buffered it plays silence and waits for the target delay to build up.
func (s *Stream) Read(out []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Wait for the target delay to build up, or for that long, so a talk
	// spurt shorter than the target still gets played.
	if !s.playing {
		if len(s.queue) == 0 && len(s.pcm) == 0 {
			clear(out)
			return
		}
		target := int(s.stats.Target * sampleRate / time.Second)
		if s.buffered() < s.stats.Target && s.waited < target {
			s.waited += len(out)
			clear(out)
			return
		}
		s.playing = true
		s.waited = 0
	}

	for filled := 0; filled < len(out); {
		if len(s.pcm) == 0 && !s.next() {
			clear(out[filled:])
			s.playing = false
			s.dry = true
			return
		}
		filled += s.play(out[filled:])
	}
}

// play moves up to 10 ms from pcm to out and returns how much of out it
// filled. The delay is steered toward the target through silence only: when
// too much is buffered, silent audio is skipped until the target is reached;
// when too little, silence is played twice so the buffer can grow. The buffer
// only grows while packets arrive, so each arrival allows one stretched frame;
// without that, a stream that stopped would stretch forever. Past the hard
// limit speech is skipped too.
func (s *Stream) play(out []int16) int {
	level := s.buffered()
	switch {
	case level > s.stats.Target+max(trimSlack, s.stats.Jitter):
		s.trimming = true
	case level <= s.stats.Target:
		s.trimming = false
	}

	chunk := s.pcm[:min(len(s.pcm), frameSamples, len(out))]
	length := samplesToDuration(int32(len(chunk)))
	silent := isSilent(chunk)

	switch {
	case s.trimming && (silent || level > hardLimit):
		if silent {
			s.stats.SilenceTrimmed += length
		} else {
			s.stats.SpeechCut += length
		}
		s.pcm = s.pcm[len(chunk):]
		s.playTS += uint32(len(chunk))
		return 0
	case silent && level < s.stats.Target && s.stretch > 0:
		s.stretch--
		clear(out[:len(chunk)])
		s.stats.SilenceAdded += length
		return len(chunk)
	}

	n := copy(out, chunk)
	s.pcm = s.pcm[n:]
	s.playTS += uint32(n)
	return n
}

// next loads the audio of the next packet into pcm. A packet that is still
// missing when its turn comes is played as silence, but only if later ones
// are here; otherwise the stream has simply run dry.
func (s *Stream) next() bool {
	pkt, ok := s.queue[s.playSeq]
	if !ok {
		if len(s.queue) == 0 {
			return false
		}
		s.stats.Lost++
		s.playSeq++
		s.pcm = s.conceal(s.pcmBuf[:0], s.packetSamples)
		return true
	}
	delete(s.queue, s.playSeq)
	s.playSeq++

	// Anything skipped before this packet plays as silence; audio that
	// overlaps what was already played is dropped.
	pcm := s.pcmBuf[:0]
	if gap := int32(pkt.Timestamp - s.playTS); gap > 0 {
		pcm = s.conceal(pcm, uint32(gap))
	}
	n, err := s.decoder.DecodeToInt16(pkt.Payload, s.decoded)
	if err != nil {
		s.stats.Lost++
		s.pcm = s.conceal(pcm, s.packetSamples)
		return true
	}
	decoded := s.decoded[:n]
	if overlap := int32(s.playTS - pkt.Timestamp); overlap > 0 {
		decoded = decoded[min(int(overlap), n):]
	}
	s.pcm = append(pcm, decoded...)
	return true
}

// conceal fills missing audio with silence. Packet loss concealment from the
// decoder would go here.
func (s *Stream) conceal(pcm []int16, samples uint32) []int16 {
	n := len(pcm)
	pcm = slices.Grow(pcm, int(samples))[:n+int(samples)]
	clear(pcm[n:])
	return pcm
}

// buffered is how much audio sits between the play position and the end of
// the newest packet.
func (s *Stream) buffered() time.Duration {
	return samplesToDuration(max(int32(s.endTS-s.playTS), 0))
}

// Stats reports how the received audio has fared so far.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := s.stats
	if s.started {
		stats.Buffered = s.buffered()
	}
	return stats
}

func isSilent(samples []int16) bool {
	var sum int64
	for _, v := range samples {
		sum += int64(v) * int64(v)
	}
	return sum <= silenceRMS*silenceRMS*int64(len(samples))
}

func samplesToDuration(samples int32) time.Duration {
	return time.Duration(samples) * time.Second / sampleRate
}
