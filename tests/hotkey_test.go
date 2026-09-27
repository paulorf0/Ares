package tests

import (
	"testing"
	"time"

	"github.com/paulorf0/Ares/hotkey"
)

func TestHotkeyAcceptsKeysWithModifiers(t *testing.T) {
	for _, spec := range []string{"ctrl+f9", "ctrl+shift+space", "alt+t", "Ctrl + F9", "super+1"} {
		if _, _, err := hotkey.Parse(spec); err != nil {
			t.Errorf("Parse(%q): %v", spec, err)
		}
	}
}

func TestHotkeyRefusesBareOrUnknownKeys(t *testing.T) {
	// A bare key would be grabbed from every other app.
	for _, spec := range []string{"f9", "", "ctrl+", "ctrl+f99", "hyper+f9", "ctrl+enter"} {
		if _, _, err := hotkey.Parse(spec); err == nil {
			t.Errorf("Parse(%q) accepted it", spec)
		}
	}
}

func TestHotkeyIgnoresAutoRepeat(t *testing.T) {
	var f hotkey.Filter
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	if !f.Down(at(0)) {
		t.Fatal("pressing the key did not start talking")
	}
	// Held key: X11 sends a release and a press every ~30 ms.
	for ms := 30; ms < 300; ms += 30 {
		f.Up(at(ms))
		if f.Down(at(ms + 5)) {
			t.Fatalf("auto-repeat at %d ms restarted talking", ms)
		}
		if f.Tick(at(ms + 20)) {
			t.Fatalf("auto-repeat at %d ms stopped talking", ms)
		}
	}
}

func TestHotkeyStopsWhenKeyStaysUp(t *testing.T) {
	var f hotkey.Filter
	t0 := time.Now()

	f.Down(t0)
	f.Up(t0.Add(500 * time.Millisecond))
	if f.Tick(t0.Add(510 * time.Millisecond)) {
		t.Fatal("stopped talking before the release settled")
	}
	if !f.Tick(t0.Add(500*time.Millisecond + hotkey.ReleaseDelay)) {
		t.Fatal("still talking after the key stayed up")
	}
	if !f.Down(t0.Add(time.Second)) {
		t.Fatal("pressing again did not start talking")
	}
}
