// Package video turns the other peer's H.264 RTP packets into pictures.
package video

import (
	"image"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/paulorf0/Ares/h264"
)

const (
	clockRate = 90000
	// A frame still missing packets this long after later ones arrived is
	// given up; NACK retransmissions have until then to fill the hole.
	maxFrameDelay = 200 * time.Millisecond
	// Packets held while a frame is incomplete.
	maxLatePackets = 512
	// While frames keep coming without a key frame, the request is repeated
	// this often in case it got lost.
	keyFrameRetry = 500 * time.Millisecond
	// Rates are measured over this window.
	rateWindow = time.Second
)

// Stats describes the received video. Counters run from the first packet.
type Stats struct {
	Width, Height int
	FrameRate     float64 // frames shown per second, over the last second
	Bitrate       float64 // bits per second received, over the last second

	Frames           uint64 // pictures shown
	Dropped          uint64 // frames skipped: damaged, or waiting for a key frame
	DecodeErrors     uint64
	KeyFrameRequests uint64
}

// Stream decodes incoming packets and hands each picture to onFrame. When a
// frame is lost it asks the sender for a key frame through onKeyFrame and
// skips frames until one arrives, since the ones in between would show
// damage. Push and Close run on one goroutine; Stats can be called from any.
type Stream struct {
	decoder    *h264.Decoder
	builder    *samplebuilder.SampleBuilder
	onFrame    func(*image.YCbCr)
	onKeyFrame func()

	needKey bool
	askedAt time.Time

	mu        sync.Mutex
	stats     Stats
	frameTime []time.Time
	byteTime  []sized
}

type sized struct {
	at    time.Time
	bytes int
}

// NewStream creates a Stream. onFrame gets a picture that is only valid
// during the call; it runs on the goroutine calling Push and must not block.
func NewStream(onFrame func(*image.YCbCr), onKeyFrame func()) (*Stream, error) {
	decoder, err := h264.NewDecoder()
	if err != nil {
		return nil, err
	}
	return &Stream{
		decoder: decoder,
		builder: samplebuilder.New(maxLatePackets, &codecs.H264Packet{}, clockRate,
			samplebuilder.WithMaxTimeDelay(maxFrameDelay)),
		onFrame:    onFrame,
		onKeyFrame: onKeyFrame,
		needKey:    true,
	}, nil
}

// Push adds a packet as it comes off the network, stamped with when it arrived.
func (s *Stream) Push(pkt *rtp.Packet, arrival time.Time) {
	s.mu.Lock()
	s.byteTime = append(trim(s.byteTime, arrival, sizedAt), sized{arrival, len(pkt.Payload)})
	s.mu.Unlock()

	s.builder.Push(pkt)
	for sample := s.builder.Pop(); sample != nil; sample = s.builder.Pop() {
		if sample.PrevDroppedPackets > 0 {
			s.lost(arrival)
		}
		s.decode(sample.Data, arrival)
	}
}

func (s *Stream) decode(au []byte, now time.Time) {
	if s.needKey {
		if !h264.IsKeyFrame(au) {
			s.count(func(st *Stats) { st.Dropped++ })
			if s.askedAt.IsZero() || now.Sub(s.askedAt) >= keyFrameRetry {
				s.askKeyFrame(now)
			}
			return
		}
		s.needKey = false
	}

	pic, err := s.decoder.Decode(au)
	if err != nil {
		s.count(func(st *Stats) { st.DecodeErrors++; st.Dropped++ })
		s.lost(now)
		return
	}
	if pic == nil {
		return
	}
	s.count(func(st *Stats) {
		st.Frames++
		st.Width, st.Height = pic.Rect.Dx(), pic.Rect.Dy()
		s.frameTime = append(trim(s.frameTime, now, timeAt), now)
	})
	s.onFrame(pic)
}

// lost switches to waiting for a key frame and asks for one.
func (s *Stream) lost(now time.Time) {
	if !s.needKey {
		s.needKey = true
		s.askKeyFrame(now)
	}
}

func (s *Stream) askKeyFrame(now time.Time) {
	s.askedAt = now
	s.count(func(st *Stats) { st.KeyFrameRequests++ })
	s.onKeyFrame()
}

func (s *Stream) count(fn func(*Stats)) {
	s.mu.Lock()
	fn(&s.stats)
	s.mu.Unlock()
}

// Stats reports how the received video has fared so far, with the rates
// measured over the second before now.
func (s *Stream) Stats(now time.Time) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frameTime = trim(s.frameTime, now, timeAt)
	s.byteTime = trim(s.byteTime, now, sizedAt)
	bytes := 0
	for _, b := range s.byteTime {
		bytes += b.bytes
	}
	stats := s.stats
	stats.FrameRate = float64(len(s.frameTime)) / rateWindow.Seconds()
	stats.Bitrate = float64(bytes*8) / rateWindow.Seconds()
	return stats
}

func (s *Stream) Close() {
	s.decoder.Close()
}

func timeAt(t time.Time) time.Time { return t }
func sizedAt(x sized) time.Time    { return x.at }

// trim drops the entries older than the rate window, reusing the slice.
func trim[T any](xs []T, now time.Time, at func(T) time.Time) []T {
	n := 0
	for n < len(xs) && now.Sub(at(xs[n])) > rateWindow {
		n++
	}
	return append(xs[:0], xs[n:]...)
}
