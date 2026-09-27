// Package viewer shows the other peer's video in a desktop window. It is a
// stopgap for the terminal client until a real UI exists.
package viewer

import (
	"image"
	"image/color"
	"sync"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

const (
	defaultWidth  = 640
	defaultHeight = 480
)

var background = color.Gray{Y: 0x20}

// Window holds the latest picture and draws it scaled to fit. Show and
// SetRemoteCamera can be called from any goroutine; Run needs the main one.
type Window struct {
	title     string
	onToggle  func()
	onQuality func(level int)

	back *image.RGBA // converted into by Show, outside the lock

	mu       sync.Mutex
	front    *image.RGBA
	fresh    bool
	remoteOn bool

	texture *ebiten.Image
	quit    <-chan struct{}
}

// New creates a window. onToggle runs when the C key is pressed, and
// onQuality with 0 to 3 when the keys 1 to 4 are; both on their own goroutine.
func New(title string, onToggle func(), onQuality func(level int)) *Window {
	return &Window{title: title, onToggle: onToggle, onQuality: onQuality}
}

var qualityKeys = []ebiten.Key{ebiten.Key1, ebiten.Key2, ebiten.Key3, ebiten.Key4}

// Show replaces the picture on screen. pic is converted before Show returns,
// so it can be reused afterwards. Calls must not overlap.
func (w *Window) Show(pic *image.YCbCr) {
	b := pic.Rect
	if w.back == nil || w.back.Rect.Dx() != b.Dx() || w.back.Rect.Dy() != b.Dy() {
		w.back = image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	}
	toRGBA(w.back, pic)

	w.mu.Lock()
	w.front, w.back = w.back, w.front
	w.fresh = true
	w.mu.Unlock()
}

// SetRemoteCamera switches between the picture and a blank screen, so a
// camera that was turned off does not leave its last frame frozen.
func (w *Window) SetRemoteCamera(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.remoteOn = on
}

// Run opens the window and blocks until it is closed or quit is closed. It
// has to run on the main goroutine.
func (w *Window) Run(quit <-chan struct{}) error {
	w.quit = quit
	ebiten.SetWindowTitle(w.title)
	ebiten.SetWindowSize(defaultWidth, defaultHeight)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	return ebiten.RunGame(w)
}

func (w *Window) Update() error {
	select {
	case <-w.quit:
		return ebiten.Termination
	default:
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyC) && w.onToggle != nil {
		go w.onToggle()
	}
	for level, key := range qualityKeys {
		if inpututil.IsKeyJustPressed(key) && w.onQuality != nil {
			go w.onQuality(level)
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.fresh {
		return nil
	}
	w.fresh = false
	size := w.front.Rect.Size()
	if w.texture == nil || w.texture.Bounds().Size() != size {
		if w.texture != nil {
			w.texture.Deallocate()
		}
		w.texture = ebiten.NewImage(size.X, size.Y)
	}
	w.texture.WritePixels(w.front.Pix)
	return nil
}

func (w *Window) Draw(screen *ebiten.Image) {
	screen.Fill(background)

	w.mu.Lock()
	on := w.remoteOn
	w.mu.Unlock()
	if !on || w.texture == nil {
		ebitenutil.DebugPrint(screen, "camera off\nC: turn yours on or off\n1-4: your quality, high to minimum")
		return
	}

	// Scale to fit, centered, keeping the aspect ratio.
	sw, sh := screen.Bounds().Dx(), screen.Bounds().Dy()
	tw, th := w.texture.Bounds().Dx(), w.texture.Bounds().Dy()
	scale := min(float64(sw)/float64(tw), float64(sh)/float64(th))
	op := &ebiten.DrawImageOptions{Filter: ebiten.FilterLinear}
	op.GeoM.Scale(scale, scale)
	op.GeoM.Translate((float64(sw)-float64(tw)*scale)/2, (float64(sh)-float64(th)*scale)/2)
	screen.DrawImage(w.texture, op)
}

func (w *Window) Layout(outsideWidth, outsideHeight int) (int, int) {
	return outsideWidth, outsideHeight
}

// toRGBA converts a 4:2:0 picture with BT.601 limited range, what H.264
// cameras send, in integer math.
func toRGBA(dst *image.RGBA, src *image.YCbCr) {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	for y := range h {
		yRow := src.Y[y*src.YStride:]
		cRow := (y / 2) * src.CStride
		out := dst.Pix[y*dst.Stride:]
		for x := range w {
			c := int32(yRow[x]) - 16
			d := int32(src.Cb[cRow+x/2]) - 128
			e := int32(src.Cr[cRow+x/2]) - 128
			out[4*x] = clamp((298*c + 409*e + 128) >> 8)
			out[4*x+1] = clamp((298*c - 100*d - 208*e + 128) >> 8)
			out[4*x+2] = clamp((298*c + 516*d + 128) >> 8)
			out[4*x+3] = 0xff
		}
	}
}

func clamp(v int32) uint8 {
	return uint8(min(max(v, 0), 255))
}
