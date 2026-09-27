// Package hotkey turns a global key into push-to-talk: talking while the key
// is held, even with the terminal out of focus.
package hotkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.design/x/hotkey"
)

// ReleaseDelay is how long a key has to stay up to count as released. X11
// auto-repeat sends up/down pairs the whole time a key is held.
const ReleaseDelay = 60 * time.Millisecond

var keys = map[string]hotkey.Key{
	"a":     hotkey.KeyA,
	"b":     hotkey.KeyB,
	"c":     hotkey.KeyC,
	"d":     hotkey.KeyD,
	"e":     hotkey.KeyE,
	"f":     hotkey.KeyF,
	"g":     hotkey.KeyG,
	"h":     hotkey.KeyH,
	"i":     hotkey.KeyI,
	"j":     hotkey.KeyJ,
	"k":     hotkey.KeyK,
	"l":     hotkey.KeyL,
	"m":     hotkey.KeyM,
	"n":     hotkey.KeyN,
	"o":     hotkey.KeyO,
	"p":     hotkey.KeyP,
	"q":     hotkey.KeyQ,
	"r":     hotkey.KeyR,
	"s":     hotkey.KeyS,
	"t":     hotkey.KeyT,
	"u":     hotkey.KeyU,
	"v":     hotkey.KeyV,
	"w":     hotkey.KeyW,
	"x":     hotkey.KeyX,
	"y":     hotkey.KeyY,
	"z":     hotkey.KeyZ,
	"0":     hotkey.Key0,
	"1":     hotkey.Key1,
	"2":     hotkey.Key2,
	"3":     hotkey.Key3,
	"4":     hotkey.Key4,
	"5":     hotkey.Key5,
	"6":     hotkey.Key6,
	"7":     hotkey.Key7,
	"8":     hotkey.Key8,
	"9":     hotkey.Key9,
	"f1":    hotkey.KeyF1,
	"f2":    hotkey.KeyF2,
	"f3":    hotkey.KeyF3,
	"f4":    hotkey.KeyF4,
	"f5":    hotkey.KeyF5,
	"f6":    hotkey.KeyF6,
	"f7":    hotkey.KeyF7,
	"f8":    hotkey.KeyF8,
	"f9":    hotkey.KeyF9,
	"f10":   hotkey.KeyF10,
	"f11":   hotkey.KeyF11,
	"f12":   hotkey.KeyF12,
	"space": hotkey.KeySpace,
}

var baseModifiers = map[string]hotkey.Modifier{
	"ctrl":  hotkey.ModCtrl,
	"shift": hotkey.ModShift,
}

// Parse reads a key like "ctrl+f9" or "ctrl+shift+space". At least one
// modifier is required: a bare key would be taken from every other app.
func Parse(spec string) ([]hotkey.Modifier, hotkey.Key, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(spec)), "+")
	if len(parts) < 2 {
		return nil, 0, fmt.Errorf("hotkey %q: needs a modifier, like ctrl+%s", spec, parts[0])
	}

	var mods []hotkey.Modifier
	for _, name := range parts[:len(parts)-1] {
		mod, ok := modifier(strings.TrimSpace(name))
		if !ok {
			return nil, 0, fmt.Errorf("hotkey %q: unknown modifier %q (ctrl, shift, alt, super)", spec, name)
		}
		mods = append(mods, mod)
	}

	name := strings.TrimSpace(parts[len(parts)-1])
	key, ok := keys[name]
	if !ok {
		return nil, 0, fmt.Errorf("hotkey %q: unknown key %q (a-z, 0-9, f1-f12, space)", spec, name)
	}
	return mods, key, nil
}

func modifier(name string) (hotkey.Modifier, bool) {
	if mod, ok := baseModifiers[name]; ok {
		return mod, true
	}
	mod, ok := platformModifiers[name]
	return mod, ok
}

// Filter turns raw key events into talking on and off, ignoring auto-repeat.
// The zero value is ready to use.
type Filter struct {
	talking   bool
	releaseAt time.Time
}

// Down handles a key press and reports whether talking just started.
func (f *Filter) Down(now time.Time) bool {
	f.releaseAt = time.Time{}
	if f.talking {
		return false
	}
	f.talking = true
	return true
}

// Up handles a key release. It only counts once Tick sees the key stayed up
// for ReleaseDelay.
func (f *Filter) Up(now time.Time) {
	if f.talking {
		f.releaseAt = now.Add(ReleaseDelay)
	}
}

// Tick settles a pending release and reports whether talking just stopped.
func (f *Filter) Tick(now time.Time) bool {
	if f.releaseAt.IsZero() || now.Before(f.releaseAt) {
		return false
	}
	f.releaseAt = time.Time{}
	f.talking = false
	return true
}

// Listen grabs the key and calls onTalk(true) when it goes down and
// onTalk(false) when it comes back up, until ctx ends.
func Listen(ctx context.Context, spec string, onTalk func(talking bool)) error {
	mods, key, err := Parse(spec)
	if err != nil {
		return err
	}
	if err := checkSession(); err != nil {
		return err
	}

	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		return fmt.Errorf("hotkey %q: %w", spec, err)
	}

	go func() {
		defer func() { _ = hk.Unregister() }()

		var filter Filter
		release := time.NewTimer(time.Hour)
		release.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hk.Keydown():
				if filter.Down(time.Now()) {
					onTalk(true)
				}
			case <-hk.Keyup():
				filter.Up(time.Now())
				release.Reset(ReleaseDelay)
			case now := <-release.C:
				if filter.Tick(now) {
					onTalk(false)
				}
			}
		}
	}()
	return nil
}

// ErrUnsupported means global keys can't be grabbed in this session.
var ErrUnsupported = errors.New("hotkey: global keys are not available here")
