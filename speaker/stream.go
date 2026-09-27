// Package speaker plays the audio received from the other peer: it puts RTP
// packets back in order, decodes the Opus and feeds the speaker.
package speaker

import (
	"sync"

	"github.com/pion/opus"
	"github.com/pion/rtp"
)

const (
	sampleRate   = 48000
	frameSamples = sampleRate / 100 // 10 ms

	// How many packets to hold while waiting for a late one. The wait has to
	// fit in the prebuffer, or every loss also becomes an underrun.
	reorderWindow = 2
	// A sequence jump beyond this is a new talk spurt, not loss.
	restartGap = 50
	// Timestamp gaps up to this are filled with silence; longer ones restart.
	maxConcealSamples = sampleRate
	// Audio kept before playback starts, and the most kept before dropping.
	prebufferSamples   = sampleRate * 80 / 1000
	maxBufferedSamples = sampleRate * 200 / 1000
	// The longest Opus packet is 120 ms.
	maxPacketSamples = sampleRate * 120 / 1000
)

// Stream turns incoming RTP packets into a steady 48 kHz mono signal. Push and
// Read may run on different goroutines.
type Stream struct {
	mu sync.Mutex

	decoder opus.Decoder
	decoded []int16

	started bool
	nextSeq uint16
	nextTS  uint32
	pending map[uint16]*rtp.Packet

	fifo    []int16
	playing bool
}

// NewStream creates an empty Stream.
func NewStream() (*Stream, error) {
	decoder, err := opus.NewDecoderWithOutput(sampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &Stream{
		decoder: decoder,
		decoded: make([]int16, maxPacketSamples),
		pending: make(map[uint16]*rtp.Packet),
	}, nil
}

// Push adds a packet as it comes off the network.
func (s *Stream) Push(pkt *rtp.Packet) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.isRestart(pkt) {
		s.restart(pkt)
	}
	if int16(pkt.SequenceNumber-s.nextSeq) < 0 {
		return // too late, its slot was already filled with silence
	}

	s.pending[pkt.SequenceNumber] = pkt
	for {
		if next, ok := s.pending[s.nextSeq]; ok {
			delete(s.pending, s.nextSeq)
			s.nextSeq++
			s.decode(next)
			continue
		}
		if len(s.pending) > reorderWindow {
			s.nextSeq++ // give up on the missing one
			continue
		}
		break
	}
}

// isRestart spots the other peer starting over, as it does after each
// push-to-talk pause: sequence and timestamp jump to new random values.
func (s *Stream) isRestart(pkt *rtp.Packet) bool {
	seqGap := int16(pkt.SequenceNumber - s.nextSeq)
	tsGap := int32(pkt.Timestamp - s.nextTS)
	return seqGap > restartGap || seqGap < -restartGap ||
		tsGap > maxConcealSamples || tsGap < -maxConcealSamples
}

func (s *Stream) restart(pkt *rtp.Packet) {
	_ = s.decoder.Init(sampleRate, 1)
	clear(s.pending)
	s.fifo = s.fifo[:0]
	s.playing = false
	s.started = true
	s.nextSeq = pkt.SequenceNumber
	s.nextTS = pkt.Timestamp
}

func (s *Stream) decode(pkt *rtp.Packet) {
	// Fill whatever went missing before this packet with silence.
	if gap := int32(pkt.Timestamp - s.nextTS); gap > 0 {
		s.fifo = append(s.fifo, make([]int16, gap)...)
	}

	n, err := s.decoder.DecodeToInt16(pkt.Payload, s.decoded)
	if err != nil {
		return // counts as lost; the next packet fills the gap
	}
	s.fifo = append(s.fifo, s.decoded[:n]...)
	s.nextTS = pkt.Timestamp + uint32(n)

	// The two machines' clocks drift apart; drop old audio so the delay
	// doesn't keep growing.
	if len(s.fifo) > maxBufferedSamples {
		s.fifo = append(s.fifo[:0], s.fifo[len(s.fifo)-prebufferSamples:]...)
	}
}

// Read fills out with the next audio to play. It never blocks: when nothing
// is buffered it plays silence and waits for a little audio to build up.
func (s *Stream) Read(out []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.playing {
		if len(s.fifo) < prebufferSamples {
			clear(out)
			return
		}
		s.playing = true
	}

	n := copy(out, s.fifo)
	s.fifo = s.fifo[n:]
	if n < len(out) {
		clear(out[n:])
		s.playing = false
	}
}
