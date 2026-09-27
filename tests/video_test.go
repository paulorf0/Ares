package tests

import (
	"image"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/paulorf0/Ares/video"
)

const frameInterval = 33 * time.Millisecond

// packetize splits access units into RTP packets the way the sender does,
// one slice per frame.
func packetize(units [][]byte) [][]*rtp.Packet {
	p := rtp.NewPacketizer(1200, 102, 1, &codecs.H264Payloader{}, rtp.NewFixedSequencer(100), 90000)
	frames := make([][]*rtp.Packet, len(units))
	for i, au := range units {
		frames[i] = p.Packetize(au, 3000)
	}
	return frames
}

// videoRun feeds frames into a Stream on a simulated clock and records what
// comes out.
type videoRun struct {
	t        *testing.T
	stream   *video.Stream
	now      time.Time
	pictures []*image.YCbCr
	shown    int
	damaged  int
	requests []int // frame being received when a key frame was asked for
	current  int
}

func newVideoRun(t *testing.T, pictures []*image.YCbCr) *videoRun {
	t.Helper()
	r := &videoRun{t: t, now: time.Unix(0, 0), pictures: pictures}
	stream, err := video.NewStream(r.frame, func() { r.requests = append(r.requests, r.current) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stream.Close)
	r.stream = stream
	return r
}

// frame counts a shown picture, and whether it looks like any sent one.
func (r *videoRun) frame(pic *image.YCbCr) {
	r.shown++
	best := 0.0
	for _, src := range r.pictures {
		best = max(best, psnr(src, pic))
	}
	if best < 30 {
		r.damaged++
	}
}

func (r *videoRun) send(i int, packets []*rtp.Packet) {
	r.current = i
	for _, pkt := range packets {
		r.stream.Push(pkt, r.now)
	}
	r.now = r.now.Add(frameInterval)
}

func TestVideoShowsEveryFrame(t *testing.T) {
	units, pictures := encodeFrames(t, 30)
	r := newVideoRun(t, pictures)
	for i, packets := range packetize(units) {
		r.send(i, packets)
	}

	// A frame is complete once the next one starts, so the last is pending.
	if r.shown != len(units)-1 {
		t.Errorf("showed %d frames, want %d", r.shown, len(units)-1)
	}
	if r.damaged != 0 {
		t.Errorf("%d frames looked damaged", r.damaged)
	}
	if len(r.requests) != 0 {
		t.Errorf("asked for %d key frames on a clean stream", len(r.requests))
	}
	stats := r.stream.Stats(r.now)
	if stats.Width != testWidth || stats.Height != testHeight {
		t.Errorf("stats say %dx%d, want %dx%d", stats.Width, stats.Height, testWidth, testHeight)
	}
	if stats.FrameRate < 25 || stats.FrameRate > 35 {
		t.Errorf("frame rate %.1f, want about 30", stats.FrameRate)
	}
	if stats.Bitrate <= 0 {
		t.Error("no bitrate measured")
	}
	if later := r.stream.Stats(r.now.Add(3 * time.Second)); later.FrameRate != 0 || later.Bitrate != 0 {
		t.Errorf("rates %.1f fps, %.0f bps after the video stopped, want 0", later.FrameRate, later.Bitrate)
	}
}

func TestVideoPutsReorderedPacketsBack(t *testing.T) {
	units, pictures := encodeFrames(t, 20)
	r := newVideoRun(t, pictures)
	frames := packetize(units)
	r.send(0, frames[0])
	for i := 1; i+1 < len(frames); i += 2 {
		// The two frames arrive interleaved and backwards.
		a, b := frames[i], frames[i+1]
		mixed := make([]*rtp.Packet, 0, len(a)+len(b))
		for j := len(b) - 1; j >= 0; j-- {
			mixed = append(mixed, b[j])
		}
		for j := len(a) - 1; j >= 0; j-- {
			mixed = append(mixed, a[j])
		}
		r.send(i, mixed)
	}
	if r.shown < len(units)-2 || r.damaged != 0 || len(r.requests) != 0 {
		t.Errorf("shown %d (want %d), damaged %d, key frame requests %d",
			r.shown, len(units)-2, r.damaged, len(r.requests))
	}
}

func TestVideoAsksForAKeyFrameAfterLoss(t *testing.T) {
	units, pictures := encodeFrames(t, 40, 25)
	r := newVideoRun(t, pictures)
	for i, packets := range packetize(units) {
		if i == 10 {
			packets = packets[1:] // one packet never arrives
		}
		r.send(i, packets)
	}

	// The hole is given 200 ms for a retransmission before the frame is
	// written off, so the request comes within about 7 frames.
	if len(r.requests) == 0 || r.requests[0] < 10 || r.requests[0] > 18 {
		t.Errorf("key frame requests at frames %v, want the first between 10 and 18", r.requests)
	}
	if r.damaged != 0 {
		t.Errorf("%d damaged frames were shown", r.damaged)
	}
	// Frames 0-9 and 25-38 are clean; 10-24 depend on the lost one.
	if want := 10 + 14; r.shown != want {
		t.Errorf("showed %d frames, want %d", r.shown, want)
	}
	// The loss is seen in the numbering, so the frames that depend on it
	// are skipped rather than fed to the decoder.
	if stats := r.stream.Stats(r.now); stats.Dropped == 0 || stats.KeyFrameRequests == 0 || stats.DecodeErrors != 0 {
		t.Errorf("stats %+v, want dropped frames, key frame requests and no decode errors", stats)
	}
}

func TestVideoWaitsForAKeyFrameWhenJoiningLate(t *testing.T) {
	units, pictures := encodeFrames(t, 40, 30)
	r := newVideoRun(t, pictures)
	for i, packets := range packetize(units) {
		if i < 5 {
			continue // the call picks up mid-stream
		}
		r.send(i, packets)
	}

	if r.damaged != 0 {
		t.Errorf("%d damaged frames were shown", r.damaged)
	}
	if want := 40 - 30 - 1; r.shown != want {
		t.Errorf("showed %d frames, want %d from the key frame on", r.shown, want)
	}
	// The first frame it can't use is the cue to ask, so the freeze lasts
	// one round trip rather than a timeout. Frame 5 is only complete once
	// frame 6 starts.
	if len(r.requests) == 0 || r.requests[0] != 6 {
		t.Errorf("key frame requests at frames %v, want the first at frame 6", r.requests)
	}
	if stats := r.stream.Stats(r.now); stats.DecodeErrors != 0 {
		t.Errorf("%d decode errors: frames before the key frame reached the decoder", stats.DecodeErrors)
	}
}
