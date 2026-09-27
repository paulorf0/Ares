// Package rtpstream keeps a sender's RTP numbering continuous while its track
// is swapped in and out.
package rtpstream

import (
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Stream is the numbering of one sender. Local tracks whose packetizer starts
// over from random numbers on every Bind, as mediadevices tracks do, are
// wrapped with it. The receiver's SRTP expects the sequence to go on where it
// stopped and drops everything else, so each bind is shifted to continue the
// previous one: the sequence number by one packet, the timestamp by the time
// that passed. Every track wrapped by the same Stream shares the numbering.
type Stream struct {
	mu      sync.Mutex
	started bool
	lastSeq uint16
	lastTS  uint32
	lastAt  time.Time
	stats   Stats
}

// Stats counts what went out through a Stream.
type Stats struct {
	Packets uint64
	Bytes   uint64 // RTP payload
}

func New() *Stream {
	return &Stream{}
}

// Wrap returns track numbered by s.
func (s *Stream) Wrap(track webrtc.TrackLocal) *Track {
	return &Track{TrackLocal: track, stream: s}
}

// Stats reports what went out so far.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Wrap returns track with a numbering of its own, continuous across binds.
func Wrap(track webrtc.TrackLocal) *Track {
	return New().Wrap(track)
}

// Track is a local track whose packets go through a Stream.
type Track struct {
	webrtc.TrackLocal
	stream *Stream
}

func (t *Track) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	return t.TrackLocal.Bind(boundContext{ctx, t.stream})
}

func (t *Track) Unbind(ctx webrtc.TrackLocalContext) error {
	return t.TrackLocal.Unbind(boundContext{ctx, t.stream})
}

// boundContext hands the wrapped track a writer that renumbers its packets.
type boundContext struct {
	webrtc.TrackLocalContext
	stream *Stream
}

func (c boundContext) WriteStream() webrtc.TrackLocalWriter {
	return &writer{out: c.TrackLocalContext.WriteStream(), ctx: c.TrackLocalContext, stream: c.stream}
}

// writer holds the shift for one bind, fixed by its first packet.
type writer struct {
	out    webrtc.TrackLocalWriter
	ctx    webrtc.TrackLocalContext
	stream *Stream

	shifted  bool
	seqShift uint16
	tsShift  uint32
}

func (w *writer) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	s := w.stream
	now := time.Now()

	s.mu.Lock()
	if !w.shifted {
		w.shifted = true
		if s.started {
			elapsed := uint32(now.Sub(s.lastAt).Seconds() * float64(w.clockRate(header.PayloadType)))
			w.seqShift = s.lastSeq + 1 - header.SequenceNumber
			w.tsShift = s.lastTS + max(elapsed, 1) - header.Timestamp
		}
	}
	header.SequenceNumber += w.seqShift
	header.Timestamp += w.tsShift
	s.started = true
	s.lastSeq = header.SequenceNumber
	s.lastTS = header.Timestamp
	s.lastAt = now
	s.stats.Packets++
	s.stats.Bytes += uint64(len(payload))
	s.mu.Unlock()

	return w.out.WriteRTP(header, payload)
}

func (w *writer) Write(b []byte) (int, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(b); err != nil {
		return 0, err
	}
	return w.WriteRTP(&pkt.Header, pkt.Payload)
}

// clockRate looks up the negotiated codec by payload type; 0 if unknown.
func (w *writer) clockRate(payloadType uint8) uint32 {
	for _, codec := range w.ctx.CodecParameters() {
		if uint8(codec.PayloadType) == payloadType {
			return codec.ClockRate
		}
	}
	return 0
}
