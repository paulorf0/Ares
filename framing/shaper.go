// Package framing brings camera frames down to the size and rate a call can
// afford, before they reach the encoder.
package framing

import (
	"image"
	"sync"
	"time"

	"github.com/pion/mediadevices/pkg/io/video"
)

// Shape is what frames are brought down to. The height follows the source's
// aspect ratio. A Width of 0, or one wider than the source, keeps the source
// size; a FrameRate of 0 keeps every frame.
type Shape struct {
	Width     int
	FrameRate float64
}

// Shaper resizes and drops frames to match a Shape that can change at any
// time. It is safe for concurrent use.
type Shaper struct {
	mu     sync.Mutex
	shape  Shape
	source image.Point // size of the last source frame; zero until one came
}

func New(shape Shape) *Shaper {
	return &Shaper{shape: shape}
}

// Set changes the shape; the next frame out follows it.
func (s *Shaper) Set(shape Shape) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shape = shape
}

// OutputSize reports the size frames come out at now. ok is false until a
// source frame has been seen.
func (s *Shaper) OutputSize() (width, height int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.source == (image.Point{}) {
		return 0, 0, false
	}
	size := outputSize(s.shape, s.source)
	return size.X, size.Y, true
}

// Transform returns the video transform that applies the shape. Each reader
// it wraps keeps its own pacing and buffers.
func (s *Shaper) Transform() video.TransformFunc {
	return func(r video.Reader) video.Reader {
		var pace pacer
		var out *image.YCbCr
		var tables scaleTables
		return video.ReaderFunc(func() (image.Image, func(), error) {
			for {
				img, release, err := r.Read()
				if err != nil {
					return nil, func() {}, err
				}
				src, ok := img.(*image.YCbCr)
				if !ok {
					// Not a camera frame; hand it on untouched.
					return img, release, nil
				}

				s.mu.Lock()
				s.source = src.Rect.Size()
				shape := s.shape
				s.mu.Unlock()

				if !pace.take(shape.FrameRate, time.Now()) {
					release()
					continue
				}
				size := outputSize(shape, src.Rect.Size())
				if size == src.Rect.Size() && src.SubsampleRatio == image.YCbCrSubsampleRatio420 {
					return src, release, nil
				}
				if out == nil || out.Rect.Size() != size {
					out = image.NewYCbCr(image.Rectangle{Max: size}, image.YCbCrSubsampleRatio420)
				}
				tables.scale(out, src)
				release()
				return out, func() {}, nil
			}
		})
	}
}

// outputSize is the source size brought down to the shape's width, keeping
// the aspect ratio. Sides are even, as 4:2:0 needs.
func outputSize(shape Shape, source image.Point) image.Point {
	w, h := source.X, source.Y
	if shape.Width > 0 && shape.Width < w {
		h = h * shape.Width / w
		w = shape.Width
	}
	return image.Pt(max(w&^1, 2), max(h&^1, 2))
}

// pacer lets frames through at a target rate. Each frame taken moves the next
// due time one interval on, so a 30 fps source brought to 24 fps drops one
// frame in five instead of rounding to 15 or 30.
type pacer struct {
	due time.Time
}

func (p *pacer) take(rate float64, now time.Time) bool {
	if rate <= 0 {
		p.due = time.Time{}
		return true
	}
	interval := time.Duration(float64(time.Second) / rate)
	// Camera frames jitter; a frame this close to its due time still counts.
	slack := min(interval/4, 10*time.Millisecond)
	if !p.due.IsZero() && now.Before(p.due.Add(-slack)) {
		return false
	}
	if p.due.IsZero() || now.Sub(p.due) > interval {
		p.due = now // fell behind, as after a pause: start over from now
	}
	p.due = p.due.Add(interval)
	return true
}

// scaleTables maps output columns and rows to source ones, kept between
// frames of the same sizes.
type scaleTables struct {
	src, dst   image.Point
	ratio      image.YCbCrSubsampleRatio
	lumaX      []int
	lumaY      []int
	chromaOffs []int // source chroma offset for each output chroma sample
}

// scale fills dst, 4:2:0, from src of any subsampling with nearest-neighbour
// sampling.
func (t *scaleTables) scale(dst, src *image.YCbCr) {
	if t.src != src.Rect.Size() || t.dst != dst.Rect.Size() || t.ratio != src.SubsampleRatio {
		t.build(dst, src)
	}
	for y, sy := range t.lumaY {
		row := dst.Y[y*dst.YStride:]
		srcRow := src.Y[sy*src.YStride:]
		for x, sx := range t.lumaX {
			row[x] = srcRow[sx]
		}
	}
	cw := (dst.Rect.Dx() + 1) / 2
	for i, off := range t.chromaOffs {
		d := (i/cw)*dst.CStride + i%cw
		dst.Cb[d] = src.Cb[off]
		dst.Cr[d] = src.Cr[off]
	}
}

func (t *scaleTables) build(dst, src *image.YCbCr) {
	t.src, t.dst, t.ratio = src.Rect.Size(), dst.Rect.Size(), src.SubsampleRatio
	sw, sh := t.src.X, t.src.Y
	dw, dh := t.dst.X, t.dst.Y

	t.lumaX = make([]int, dw)
	for x := range t.lumaX {
		t.lumaX[x] = x * sw / dw
	}
	t.lumaY = make([]int, dh)
	for y := range t.lumaY {
		t.lumaY[y] = y * sh / dh
	}

	// Each output chroma sample covers a 2x2 block of output luma; it takes
	// the source chroma under the block's top-left pixel.
	cw, ch := (dw+1)/2, (dh+1)/2
	t.chromaOffs = make([]int, 0, cw*ch)
	for cy := range ch {
		sy := (2 * cy) * sh / dh
		for cx := range cw {
			sx := (2 * cx) * sw / dw
			t.chromaOffs = append(t.chromaOffs, src.COffset(src.Rect.Min.X+sx, src.Rect.Min.Y+sy))
		}
	}
}
