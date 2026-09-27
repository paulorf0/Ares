package tests

import (
	"image"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/pion/mediadevices/pkg/io/video"

	"github.com/paulorf0/Ares/framing"
)

// shapeOnce runs one picture through a Shaper.
func shapeOnce(t *testing.T, s *framing.Shaper, src image.Image) *image.YCbCr {
	t.Helper()
	r := s.Transform()(video.ReaderFunc(func() (image.Image, func(), error) {
		return src, func() {}, nil
	}))
	img, release, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, ok := img.(*image.YCbCr)
	if !ok {
		t.Fatalf("shaper gave %T, want *image.YCbCr", img)
	}
	return out
}

// boxDown averages the luma of src down to w x h, as a reference for what a
// good downscale looks like.
func boxDown(src *image.YCbCr, w, h int) []float64 {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	out := make([]float64, w*h)
	for y := range h {
		for x := range w {
			var sum, n float64
			for sy := y * sh / h; sy < (y+1)*sh/h; sy++ {
				for sx := x * sw / w; sx < (x+1)*sw/w; sx++ {
					sum += float64(src.Y[sy*src.YStride+sx])
					n++
				}
			}
			out[y*w+x] = sum / n
		}
	}
	return out
}

func lumaPSNR(img *image.YCbCr, ref []float64) float64 {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	var sum float64
	for y := range h {
		for x := range w {
			d := float64(img.Y[y*img.YStride+x]) - ref[y*w+x]
			sum += d * d
		}
	}
	return 10 * math.Log10(255*255/(sum/float64(w*h)))
}

func TestFramingScalesDown(t *testing.T) {
	src := testPictureSized(640, 480, 3, rand.New(rand.NewPCG(1, 1)))
	for _, width := range []int{480, 320, 160} {
		s := framing.New(framing.Shape{Width: width})
		out := shapeOnce(t, s, src)
		wantH := 480 * width / 640
		if out.Rect.Dx() != width || out.Rect.Dy() != wantH {
			t.Errorf("width %d: got %v, want %dx%d", width, out.Rect.Size(), width, wantH)
			continue
		}
		if out.SubsampleRatio != image.YCbCrSubsampleRatio420 {
			t.Errorf("width %d: subsampling %v, want 4:2:0", width, out.SubsampleRatio)
		}
		if q := lumaPSNR(out, boxDown(src, width, wantH)); q < 25 {
			t.Errorf("width %d: %.1f dB against an averaged downscale, want 25 or more", width, q)
		}
		if w, h, ok := s.OutputSize(); !ok || w != width || h != wantH {
			t.Errorf("width %d: OutputSize %dx%d %v", width, w, h, ok)
		}
	}
}

func TestFramingKeepsTheAspectRatio(t *testing.T) {
	wide := testPictureSized(640, 360, 0, rand.New(rand.NewPCG(1, 2)))
	out := shapeOnce(t, framing.New(framing.Shape{Width: 320}), wide)
	if got := out.Rect.Size(); got != image.Pt(320, 180) {
		t.Errorf("16:9 source brought to %v, want 320x180", got)
	}
}

func TestFramingNeverEnlarges(t *testing.T) {
	small := testPictureSized(320, 240, 0, rand.New(rand.NewPCG(1, 3)))
	out := shapeOnce(t, framing.New(framing.Shape{Width: 640}), small)
	if got := out.Rect.Size(); got != image.Pt(320, 240) {
		t.Errorf("320x240 source came out %v, want it untouched", got)
	}
}

func TestFramingTakes422(t *testing.T) {
	src := testPictureSized(640, 480, 5, rand.New(rand.NewPCG(1, 4)))
	// The same picture in 4:2:2, as a YUYV camera delivers it.
	wide := image.NewYCbCr(src.Rect, image.YCbCrSubsampleRatio422)
	copy(wide.Y, src.Y)
	for y := range 480 {
		copy(wide.Cb[y*wide.CStride:], src.Cb[(y/2)*src.CStride:(y/2+1)*src.CStride])
		copy(wide.Cr[y*wide.CStride:], src.Cr[(y/2)*src.CStride:(y/2+1)*src.CStride])
	}

	from420 := shapeOnce(t, framing.New(framing.Shape{Width: 320}), src)
	from422 := shapeOnce(t, framing.New(framing.Shape{Width: 320}), wide)
	if from422.SubsampleRatio != image.YCbCrSubsampleRatio420 || from422.Rect.Size() != image.Pt(320, 240) {
		t.Fatalf("4:2:2 came out %v at %v", from422.SubsampleRatio, from422.Rect.Size())
	}
	for _, plane := range []struct {
		name string
		a, b []byte
	}{{"Y", from420.Y, from422.Y}, {"Cb", from420.Cb, from422.Cb}, {"Cr", from420.Cr, from422.Cr}} {
		for i := range plane.a {
			if plane.a[i] != plane.b[i] {
				t.Errorf("%s differs at %d between 4:2:0 and 4:2:2 sources", plane.name, i)
				break
			}
		}
	}
}

func TestFramingFollowsAChangeAtOnce(t *testing.T) {
	src := testPictureSized(640, 480, 0, rand.New(rand.NewPCG(1, 5)))
	s := framing.New(framing.Shape{Width: 640})
	if _, _, ok := s.OutputSize(); ok {
		t.Error("OutputSize known before any frame")
	}
	r := s.Transform()(video.ReaderFunc(func() (image.Image, func(), error) {
		return src, func() {}, nil
	}))

	sizes := []image.Point{}
	for _, width := range []int{640, 160, 320} {
		s.Set(framing.Shape{Width: width})
		img, _, err := r.Read()
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, img.Bounds().Size())
	}
	want := []image.Point{{640, 480}, {160, 120}, {320, 240}}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("frames came out %v, want %v", sizes, want)
			break
		}
	}
}

func TestFramingDropsFramesToTheRate(t *testing.T) {
	src := testPictureSized(320, 240, 0, rand.New(rand.NewPCG(1, 6)))
	for _, tc := range []struct {
		source, target float64
	}{{30, 15}, {30, 24}, {30, 10}} {
		interval := time.Duration(float64(time.Second) / tc.source)
		r := framing.New(framing.Shape{FrameRate: tc.target}).Transform()(video.ReaderFunc(func() (image.Image, func(), error) {
			time.Sleep(interval)
			return src, func() {}, nil
		}))

		const window = 1500 * time.Millisecond
		frames := 0
		for start := time.Now(); time.Since(start) < window; frames++ {
			if _, _, err := r.Read(); err != nil {
				t.Fatal(err)
			}
		}
		got := float64(frames) / window.Seconds()
		if math.Abs(got-tc.target) > tc.target*0.15 {
			t.Errorf("%v fps source brought to %v: got %.1f fps", tc.source, tc.target, got)
		}
	}
}
