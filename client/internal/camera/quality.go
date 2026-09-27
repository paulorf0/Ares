package camera

import (
	"fmt"
	"strings"

	"github.com/paulorf0/Ares/framing"
)

// Quality is how much of the camera is sent: the lower, the smaller the
// picture, the fewer frames and bits, and the less CPU on both sides.
type Quality int

const (
	High Quality = iota
	Medium
	Low
	Minimum
)

var levels = [...]struct {
	name, alias string
	width       int
	frameRate   float64
	bitRate     int
}{
	High:    {"high", "alta", 640, 30, 1_000_000},
	Medium:  {"medium", "media", 480, 24, 500_000},
	Low:     {"low", "baixa", 320, 15, 250_000},
	Minimum: {"minimum", "minima", 160, 10, 100_000},
}

// Valid reports whether q is one of the levels.
func (q Quality) Valid() bool {
	return q >= 0 && int(q) < len(levels)
}

// Name is the English name of the level.
func (q Quality) Name() string {
	if !q.Valid() {
		return ""
	}
	return levels[q].name
}

func (q Quality) shape() framing.Shape {
	return framing.Shape{Width: levels[q].width, FrameRate: levels[q].frameRate}
}

// Parse reads a level by name, in English or Portuguese.
func Parse(s string) (Quality, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for q, level := range levels {
		if s == level.name || s == level.alias {
			return Quality(q), nil
		}
	}
	return 0, fmt.Errorf("client: unknown video quality %q", s)
}

func errUnknown(q Quality) error {
	return fmt.Errorf("client: unknown video quality %d", int(q))
}
