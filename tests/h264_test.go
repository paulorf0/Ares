package tests

import (
	"image"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/pion/mediadevices/pkg/codec"
	"github.com/pion/mediadevices/pkg/codec/openh264"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/mediadevices/pkg/prop"

	"github.com/paulorf0/Ares/h264"
)

const (
	testWidth  = 320
	testHeight = 240
)

// testPicture draws frame i: a gradient with a moving box and fixed-seed
// noise, so consecutive frames differ like camera frames do.
func testPicture(i int, rng *rand.Rand) *image.YCbCr {
	return testPictureSized(testWidth, testHeight, i, rng)
}

func testPictureSized(width, height, i int, rng *rand.Rand) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	bx, by := (i*7)%(width-60), (i*5)%(height-60)
	for y := range height {
		for x := range width {
			v := (x+y+i*2)%256/2 + 40
			if x >= bx && x < bx+60 && y >= by && y < by+60 {
				v = 220
			}
			img.Y[y*img.YStride+x] = uint8(v + rng.IntN(5) - 2)
		}
	}
	for j := range img.Cb {
		img.Cb[j] = uint8(128 + (j/img.CStride+i)%40 - 20)
		img.Cr[j] = uint8(128 + j%img.CStride%40 - 20)
	}
	return img
}

// encodeFrames encodes n test pictures with the client's encoder and returns
// the access units along with the pictures. keyAt lists extra frames forced
// to be key frames; the first one always is.
func encodeFrames(t *testing.T, n int, keyAt ...int) ([][]byte, []*image.YCbCr) {
	t.Helper()

	rng := rand.New(rand.NewPCG(3, 4))
	pictures := make([]*image.YCbCr, n)
	next := 0
	source := video.ReaderFunc(func() (image.Image, func(), error) {
		img := testPicture(next, rng)
		pictures[next] = img
		next++
		return img, func() {}, nil
	})

	params, err := openh264.NewParams()
	if err != nil {
		t.Fatal(err)
	}
	params.BitRate = 500_000
	params.EnableFrameSkip = false
	encoder, err := params.BuildVideoEncoder(source, prop.Media{Video: prop.Video{
		Width: testWidth, Height: testHeight, FrameRate: 30, FrameFormat: frame.FormatI420,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	keys, _ := encoder.Controller().(codec.KeyFrameController)

	units := make([][]byte, n)
	for i := range units {
		for _, k := range keyAt {
			if k == i && keys != nil {
				if err := keys.ForceKeyFrame(); err != nil {
					t.Fatal(err)
				}
			}
		}
		data, release, err := encoder.Read()
		if err != nil {
			t.Fatal(err)
		}
		units[i] = append([]byte(nil), data...)
		release()
	}
	return units, pictures
}

func psnr(a, b *image.YCbCr) float64 {
	var sum float64
	for y := range testHeight {
		for x := range testWidth {
			d := float64(a.Y[y*a.YStride+x]) - float64(b.Y[y*b.YStride+x])
			sum += d * d
		}
	}
	mse := sum / (testWidth * testHeight)
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

func TestH264DecodesWhatTheEncoderSends(t *testing.T) {
	units, pictures := encodeFrames(t, 30)
	if !h264.IsKeyFrame(units[0]) {
		t.Fatal("the first frame is not a key frame")
	}

	dec, err := h264.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	decoded := 0
	for i, au := range units {
		pic, err := dec.Decode(au)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if pic == nil {
			continue
		}
		decoded++
		if pic.Rect.Dx() != testWidth || pic.Rect.Dy() != testHeight {
			t.Fatalf("frame %d is %v, want %dx%d", i, pic.Rect, testWidth, testHeight)
		}
		if q := psnr(pictures[i], pic); q < 30 {
			t.Errorf("frame %d decoded at %.1f dB PSNR, want 30 or more", i, q)
		}
	}
	if decoded != len(units) {
		t.Errorf("decoded %d of %d frames", decoded, len(units))
	}
}

func TestH264KeyFramesAreRecognized(t *testing.T) {
	units, _ := encodeFrames(t, 12, 8)
	for i, au := range units {
		want := i == 0 || i == 8
		if got := h264.IsKeyFrame(au); got != want {
			t.Errorf("frame %d: key frame %v, want %v", i, got, want)
		}
	}
}

func TestH264SurvivesGarbage(t *testing.T) {
	dec, err := h264.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	rng := rand.New(rand.NewPCG(5, 6))
	for range 200 {
		junk := make([]byte, 1+rng.IntN(2000))
		for i := range junk {
			junk[i] = byte(rng.IntN(256))
		}
		if rng.IntN(2) == 0 {
			copy(junk, []byte{0, 0, 0, 1})
		}
		_, _ = dec.Decode(junk) // must not crash
	}

	// It still decodes a real stream afterwards.
	units, _ := encodeFrames(t, 3)
	var pic *image.YCbCr
	for _, au := range units {
		if p, err := dec.Decode(au); err == nil && p != nil {
			pic = p
		}
	}
	if pic == nil {
		t.Error("no picture from a valid stream after garbage")
	}
}
