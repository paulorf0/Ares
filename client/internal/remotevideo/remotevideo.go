// Package remotevideo receives the other peer's video and tracks whether its
// camera is on.
package remotevideo

import (
	"encoding/json"
	"errors"
	"image"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/paulorf0/Ares/messages"
	videoin "github.com/paulorf0/Ares/video"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Receiver decodes the other peer's video and hands each picture out.
type Receiver struct {
	writeRTCP func([]rtcp.Packet) error

	mu       sync.Mutex
	in       *videoin.Stream
	remoteOn bool

	handlerMu sync.Mutex
	onFrame   func(*image.YCbCr)
	onCamera  func(on bool)
}

// Stats describes the other peer's video.
type Stats struct {
	RemoteCameraOn bool
	Receiving      *videoin.Stats // nil until any came in
}

// New returns a receiver that asks for key frames through writeRTCP.
func New(writeRTCP func([]rtcp.Packet) error) *Receiver {
	return &Receiver{writeRTCP: writeRTCP}
}

// OnFrame registers fn to receive each picture.
func (r *Receiver) OnFrame(fn func(*image.YCbCr)) {
	r.handlerMu.Lock()
	defer r.handlerMu.Unlock()
	r.onFrame = fn
}

// OnCamera registers fn to hear when the other peer's camera goes on or off.
func (r *Receiver) OnCamera(fn func(on bool)) {
	r.handlerMu.Lock()
	defer r.handlerMu.Unlock()
	r.onCamera = fn
}

// Read decodes the other peer's video until the track ends. Lost frames are
// asked for again as a picture loss indication.
func (r *Receiver) Read(track *webrtc.TrackRemote) {
	ssrc := uint32(track.SSRC())
	stream, err := videoin.NewStream(r.deliver, func() {
		pli := []rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}
		if err := r.writeRTCP(pli); err != nil {
			slog.Warn("ask for key frame", "error", err)
		}
	})
	if err != nil {
		slog.Error("start video decoder", "error", err)
		return
	}
	defer stream.Close()

	r.mu.Lock()
	r.in = stream
	r.mu.Unlock()

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Error("read remote video", "error", err)
			}
			return
		}
		stream.Push(pkt, time.Now())
	}
}

func (r *Receiver) deliver(pic *image.YCbCr) {
	r.handlerMu.Lock()
	fn := r.onFrame
	r.handlerMu.Unlock()
	if fn != nil {
		fn(pic)
	}
}

// HandleState takes the camera state the other peer sent.
func (r *Receiver) HandleState(payload json.RawMessage) {
	var state messages.VideoPayload
	if err := json.Unmarshal(payload, &state); err != nil {
		slog.Warn("bad camera state from peer", "error", err)
		return
	}
	r.SetCamera(state.On)
}

// SetCamera records whether the other peer's camera is on and tells the
// OnCamera handler when that changes.
func (r *Receiver) SetCamera(on bool) {
	r.mu.Lock()
	changed := r.remoteOn != on
	r.remoteOn = on
	r.mu.Unlock()
	if !changed {
		return
	}

	r.handlerMu.Lock()
	fn := r.onCamera
	r.handlerMu.Unlock()
	if fn != nil {
		fn(on)
	}
}

// Stats reports the other peer's camera state and what came in.
func (r *Receiver) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := Stats{RemoteCameraOn: r.remoteOn}
	if r.in != nil {
		received := r.in.Stats(time.Now())
		stats.Receiving = &received
	}
	return stats
}
