package tests

import (
	"encoding/json"
	"errors"
	"image"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/mediadevices"

	"github.com/paulorf0/Ares/client"
	"github.com/paulorf0/Ares/messages"
)

// The fake camera's size, what the client asks a real one for.
const (
	cameraWidth  = 640
	cameraHeight = 480
)

// cameraPictures are drawn once and cycled, so fake cameras cost little even
// under the race detector.
var cameraPictures = sync.OnceValue(func() []*image.YCbCr {
	rng := rand.New(rand.NewPCG(7, 8))
	pictures := make([]*image.YCbCr, 8)
	for i := range pictures {
		pictures[i] = testPictureSized(cameraWidth, cameraHeight, i*4, rng)
	}
	return pictures
})

// fakeCamera paces test pictures like a 30 fps camera. A stuck one behaves
// like the Windows driver with a camera held by another app: it opens fine
// and never delivers, even after the other app lets go.
type fakeCamera struct {
	frame int
	stuck bool

	closeOnce sync.Once
	closedCh  chan struct{}
	closed    atomic.Bool
}

func (c *fakeCamera) ID() string { return "fake-camera" }
func (c *fakeCamera) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.closedCh)
	})
	return nil
}
func (c *fakeCamera) Read() (image.Image, func(), error) {
	if c.stuck {
		<-c.closedCh
		return nil, func() {}, io.EOF
	}
	select {
	case <-c.closedCh:
		return nil, func() {}, io.EOF
	case <-time.After(frameInterval):
	}
	pictures := cameraPictures()
	c.frame++
	return pictures[c.frame%len(pictures)], func() {}, nil
}

// cameras opens fake cameras and keeps them, so a test can check they were
// closed.
type cameras struct {
	mu     sync.Mutex
	opened []*fakeCamera
	fail   int         // how many opens fail before one works
	busy   atomic.Bool // cameras opened now get stuck
}

func (cs *cameras) open() (mediadevices.VideoSource, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.fail > 0 {
		cs.fail--
		return nil, errors.New("camera busy")
	}
	cam := &fakeCamera{
		stuck:    cs.busy.Load(),
		closedCh: make(chan struct{}),
	}
	cs.opened = append(cs.opened, cam)
	return cam, nil
}

func (cs *cameras) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.opened)
}

func (cs *cameras) get(i int) *fakeCamera {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.opened[i]
}

func (cs *cameras) option() client.Option { return client.WithCameraSource(cs.open) }

// viewer records what a client shows of the other peer's video.
type viewer struct {
	frames atomic.Int64
	width  atomic.Int64
	height atomic.Int64
	on     atomic.Bool
}

func watch(c *client.Client) *viewer {
	v := &viewer{}
	c.OnRemoteVideo(func(pic *image.YCbCr) {
		v.width.Store(int64(pic.Rect.Dx()))
		v.height.Store(int64(pic.Rect.Dy()))
		v.frames.Add(1)
	})
	c.OnRemoteCamera(v.on.Store)
	return v
}

// waitFrames waits until n more pictures than before have been shown.
func (v *viewer) waitFrames(t *testing.T, n int64) {
	t.Helper()
	target := v.frames.Load() + n
	waitFor(t, "video frames", func() bool { return v.frames.Load() >= target })
}

// cameraStatus records what OnCameraStatus reported last.
type cameraStatus struct {
	mu      sync.Mutex
	sending bool
	err     error
	reports int
}

func followCamera(c *client.Client) *cameraStatus {
	s := &cameraStatus{}
	c.OnCameraStatus(func(sending bool, err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sending, s.err = sending, err
		s.reports++
	})
	return s
}

func (s *cameraStatus) get() (bool, error, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sending, s.err, s.reports
}

func (s *cameraStatus) waitFor(t *testing.T, what string, cond func(sending bool, err error) bool) {
	t.Helper()
	waitFor(t, what, func() bool {
		sending, err, _ := s.get()
		return cond(sending, err)
	})
}

func setCamera(t *testing.T, c *client.Client, on bool) {
	t.Helper()
	if err := c.SetCamera(on); err != nil {
		t.Fatalf("SetCamera(%v): %v", on, err)
	}
}

// setCameraQuickly turns the camera on or off and checks the call did not
// wait on the camera.
func setCameraQuickly(t *testing.T, c *client.Client, on bool) {
	t.Helper()
	start := time.Now()
	setCamera(t, c, on)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("SetCamera(%v) took %v", on, took)
	}
}

// chatWorks checks a text message goes each way.
func chatWorks(t *testing.T, a, b *client.Client) {
	t.Helper()
	send := func(from, to *client.Client, text string) {
		t.Helper()
		got := make(chan string, 1)
		to.ReceiveMessage(func(msg []byte) {
			var m messages.Message
			var s string
			if json.Unmarshal(msg, &m) == nil && json.Unmarshal(m.Payload, &s) == nil {
				got <- s
			}
		})
		payload, _ := json.Marshal(text)
		if err := from.SendMessage(messages.Message{Type: messages.TypeString, Payload: payload}); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		select {
		case s := <-got:
			if s != text {
				t.Fatalf("received %q, want %q", s, text)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("%q never arrived", text)
		}
	}
	send(a, b, "hello")
	send(b, a, "hi")
}

func TestCameraStartsOff(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-off", cams.option())
	seen := watch(b)
	waitForOpenChannels(t, a, b)
	time.Sleep(300 * time.Millisecond)

	if a.CameraOn() || seen.frames.Load() != 0 || seen.on.Load() {
		t.Errorf("camera on %v, frames %d, remote on %v; want all off", a.CameraOn(), seen.frames.Load(), seen.on.Load())
	}
	if cams.count() != 0 {
		t.Errorf("the camera was opened %d times while off", cams.count())
	}
	if stats := b.Stats().Video; stats.Receiving != nil || stats.RemoteCameraOn {
		t.Errorf("video stats %+v with no camera on", stats)
	}
}

func TestCameraTurnsOnAndOffMidCall(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-toggle", cams.option())
	seen := watch(b)
	status := followCamera(a)
	waitForOpenChannels(t, a, b)

	for turn := range 3 {
		setCameraQuickly(t, a, true)
		waitFor(t, "the peer to hear the camera is on", seen.on.Load)
		seen.waitFrames(t, 10)
		if w := seen.width.Load(); w != cameraWidth {
			t.Fatalf("turn %d: pictures are %d wide, want %d", turn, w, cameraWidth)
		}
		if sending, err, _ := status.get(); !sending || err != nil {
			t.Fatalf("turn %d: status sending %v, error %v", turn, sending, err)
		}

		setCameraQuickly(t, a, false)
		waitFor(t, "the peer to hear the camera is off", func() bool { return !seen.on.Load() })
		time.Sleep(200 * time.Millisecond) // frames already on the way
		before := seen.frames.Load()
		time.Sleep(300 * time.Millisecond)
		if after := seen.frames.Load(); after != before {
			t.Fatalf("turn %d: %d frames arrived with the camera off", turn, after-before)
		}
		if !cams.get(turn).closed.Load() {
			t.Fatalf("turn %d: the camera was left open after turning it off", turn)
		}
	}

	stats := b.Stats().Video
	if stats.Receiving == nil || stats.Receiving.Frames == 0 {
		t.Fatalf("receiving stats %+v, want frames", stats.Receiving)
	}
	if sent := a.Stats().Video.Sent; sent.Packets == 0 {
		t.Error("the sender counted no packets")
	}
}

func TestCameraWorksBothWays(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-both", cams.option())
	seenByA, seenByB := watch(a), watch(b)
	waitForOpenChannels(t, a, b)

	setCamera(t, a, true)
	setCamera(t, b, true)
	seenByA.waitFrames(t, 10)
	seenByB.waitFrames(t, 10)
}

func TestCameraTurnedOnBeforeThePeerJoins(t *testing.T) {
	var cams cameras
	signalURL := newSignalingServer(t, 0)
	a := newClient(t, signalURL, "camera-early", cams.option())
	setCamera(t, a, true)

	b := newClient(t, signalURL, "camera-early", cams.option())
	seen := watch(b)
	waitFor(t, "the peer to hear the camera is on", seen.on.Load)
	seen.waitFrames(t, 10)
}

func TestCameraThatFailsToOpenIsTriedAgain(t *testing.T) {
	cams := cameras{fail: 2}
	a, b := connectedPair(t, "camera-retry", cams.option())
	seen := watch(b)
	status := followCamera(a)
	waitForOpenChannels(t, a, b)

	setCameraQuickly(t, a, true)
	status.waitFor(t, "the failure to be reported", func(_ bool, err error) bool { return err != nil })
	seen.waitFrames(t, 10)
	status.waitFor(t, "the camera to be reported on", func(sending bool, _ error) bool { return sending })
}

// The bug seen on Windows: the camera was held by another app, the program
// hung with no chat, and freeing the camera did not help.
func TestBusyCameraComesOnOnceFreed(t *testing.T) {
	var cams cameras
	cams.busy.Store(true)
	a, b := connectedPair(t, "camera-busy", cams.option())
	seen := watch(b)
	status := followCamera(a)
	waitForOpenChannels(t, a, b)

	setCameraQuickly(t, a, true)
	status.waitFor(t, "the busy camera to be reported", func(_ bool, err error) bool {
		return errors.Is(err, client.ErrCameraNoPicture)
	})
	if !cams.get(0).closed.Load() {
		t.Error("the camera that sent nothing was left open")
	}
	chatWorks(t, a, b)
	if !a.CameraOn() || a.Stats().Video.Sending {
		t.Errorf("CameraOn %v, sending %v; want asked for but not sending", a.CameraOn(), a.Stats().Video.Sending)
	}

	// The other app lets go; nothing else is called.
	cams.busy.Store(false)
	seen.waitFrames(t, 10)
	status.waitFor(t, "the camera to be reported on", func(sending bool, err error) bool { return sending && err == nil })
	chatWorks(t, a, b)
}

func TestBusyCameraDoesNotHoldUpTheHandshake(t *testing.T) {
	var cams cameras
	cams.busy.Store(true)
	signalURL := newSignalingServer(t, 0)
	a := newClient(t, signalURL, "camera-busy-early", cams.option())
	setCameraQuickly(t, a, true)

	b := newClient(t, signalURL, "camera-busy-early", cams.option())
	seen := watch(b)
	waitForOpenChannels(t, a, b)
	chatWorks(t, a, b)

	cams.busy.Store(false)
	seen.waitFrames(t, 10)
}

func TestWaitingForTheCameraCanBeCancelled(t *testing.T) {
	var cams cameras
	cams.busy.Store(true)
	a, b := connectedPair(t, "camera-cancel", cams.option())
	status := followCamera(a)
	waitForOpenChannels(t, a, b)

	setCameraQuickly(t, a, true)
	status.waitFor(t, "the busy camera to be reported", func(_ bool, err error) bool { return err != nil })
	setCameraQuickly(t, a, false)
	status.waitFor(t, "the camera to be reported off", func(sending bool, err error) bool { return !sending && err == nil })

	opened := cams.count()
	cams.busy.Store(false)
	time.Sleep(2 * time.Second)
	if n := cams.count(); n != opened {
		t.Errorf("the camera was opened %d more times after cancelling", n-opened)
	}
	if a.CameraOn() {
		t.Error("CameraOn after cancelling")
	}
	for i := range cams.count() {
		if !cams.get(i).closed.Load() {
			t.Errorf("camera %d was left open", i)
		}
	}
}

func TestCameraStopsAtClose(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-close", cams.option())
	seen := watch(b)
	waitForOpenChannels(t, a, b)
	setCamera(t, a, true)
	seen.waitFrames(t, 1)

	a.Close()
	if !cams.get(0).closed.Load() {
		t.Error("the camera was left open after Close")
	}
	waitFor(t, "the peer to see the camera go off", func() bool { return !seen.on.Load() })
}

// The client aims video at about 1 Mbps; a call over home connections can't
// take much more.
const videoTarget = 1_000_000

func TestCameraKeepsToItsBitrate(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-bitrate", cams.option())
	seen := watch(b)
	waitForOpenChannels(t, a, b)

	setCamera(t, a, true)
	seen.waitFrames(t, 30) // past the first key frame

	start, before := time.Now(), a.Stats().Video.Sent.Bytes
	time.Sleep(3 * time.Second)
	sent := a.Stats().Video.Sent.Bytes - before
	rate := float64(sent*8) / time.Since(start).Seconds()

	if rate > 1.5*videoTarget {
		t.Errorf("camera sent %.0f kbps, want at most %.0f", rate/1000, 1.5*videoTarget/1000)
	}
	if rate == 0 {
		t.Error("camera sent nothing")
	}
	t.Logf("camera sent %.0f kbps", rate/1000)
}

// waitSize waits until the pictures shown are width wide.
func (v *viewer) waitSize(t *testing.T, width, height int64) {
	t.Helper()
	waitFor(t, "pictures to change size", func() bool {
		return v.width.Load() == width && v.height.Load() == height
	})
}

func TestCameraQualityChangesMidCall(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-quality", cams.option())
	seen := watch(b)
	waitForOpenChannels(t, a, b)

	setCamera(t, a, true)
	seen.waitFrames(t, 10)
	seen.waitSize(t, 640, 480)
	opened := cams.count()

	for _, step := range []struct {
		quality       client.VideoQuality
		width, height int64
	}{
		{client.QualityLow, 320, 240},
		{client.QualityMinimum, 160, 120},
		{client.QualityMedium, 480, 360},
		{client.QualityHigh, 640, 480},
		{client.QualityLow, 320, 240},
	} {
		start := time.Now()
		if err := a.SetVideoQuality(step.quality); err != nil {
			t.Fatalf("SetVideoQuality(%v): %v", step.quality, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("SetVideoQuality(%v) took %v", step.quality, took)
		}
		seen.waitSize(t, step.width, step.height)
		seen.waitFrames(t, 5)
		if stats := a.Stats().Video; stats.Quality != step.quality || stats.Width != int(step.width) {
			t.Fatalf("sender stats say %v at %d wide, want %v at %d", stats.Quality, stats.Width, step.quality, step.width)
		}
	}

	if n := cams.count(); n != opened {
		t.Errorf("the camera was opened %d more times; changing quality should keep it open", n-opened)
	}
	if r := b.Stats().Video.Receiving; r == nil || r.DecodeErrors != 0 {
		t.Errorf("receiving stats %+v, want no decode errors", r)
	}
	chatWorks(t, a, b)
}

func TestCameraQualityHoldsItsBitrate(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-quality-bitrate", cams.option())
	seen := watch(b)
	waitForOpenChannels(t, a, b)

	if err := a.SetVideoQuality(client.QualityLow); err != nil {
		t.Fatal(err)
	}
	setCamera(t, a, true)
	seen.waitFrames(t, 15)

	start, before := time.Now(), a.Stats().Video.Sent.Bytes
	time.Sleep(3 * time.Second)
	rate := float64((a.Stats().Video.Sent.Bytes-before)*8) / time.Since(start).Seconds()

	// Too little is a fault too: an encoder told the wrong frame rate gives
	// each frame the wrong share and lands at a fraction of the target.
	const target = 250_000 // what QualityLow aims at
	if rate > 1.5*target || rate < 0.7*target {
		t.Errorf("low quality sent %.0f kbps, want %.0f to %.0f", rate/1000, 0.7*target/1000, 1.5*target/1000)
	}
	if fps := b.Stats().Video.Receiving.FrameRate; fps < 10 || fps > 20 {
		t.Errorf("low quality arrives at %.0f fps, want about 15", fps)
	}
	t.Logf("low quality: %.0f kbps", rate/1000)
}

func TestCameraQualitySetBeforeTurningOn(t *testing.T) {
	var cams cameras
	a, b := connectedPair(t, "camera-quality-early", cams.option(), client.WithVideoQuality(client.QualityMinimum))
	seen := watch(b)
	waitForOpenChannels(t, a, b)

	setCamera(t, a, true)
	seen.waitSize(t, 160, 120)

	setCamera(t, a, false)
	if err := a.SetVideoQuality(client.QualityMedium); err != nil {
		t.Fatal(err)
	}
	setCamera(t, a, true)
	seen.waitSize(t, 480, 360)
}

func TestVideoQualityNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		want client.VideoQuality
	}{
		{"high", client.QualityHigh}, {"alta", client.QualityHigh},
		{"Media", client.QualityMedium}, {"baixa", client.QualityLow},
		{" minimum ", client.QualityMinimum}, {"minima", client.QualityMinimum},
	} {
		if got, err := client.ParseVideoQuality(tc.name); err != nil || got != tc.want {
			t.Errorf("ParseVideoQuality(%q) = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := client.ParseVideoQuality("ultra"); err == nil {
		t.Error("ParseVideoQuality accepted an unknown level")
	}

	signalURL := newSignalingServer(t, 0)
	if c, err := client.New(signalURL, "quality-bad", "1", "A", client.WithVideoQuality(42)); err == nil {
		c.Close()
		t.Error("New accepted an unknown quality level")
	}
	c := newClient(t, signalURL, "quality-bad-set")
	if err := c.SetVideoQuality(42); err == nil {
		t.Error("SetVideoQuality accepted an unknown level")
	}
}
