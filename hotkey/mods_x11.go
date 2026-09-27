//go:build linux || openbsd

package hotkey

import (
	"fmt"
	"os"

	"golang.design/x/hotkey"
)

var platformModifiers = map[string]hotkey.Modifier{
	"alt":   hotkey.Mod1,
	"super": hotkey.Mod4,
}

// checkSession refuses Wayland, which doesn't let apps grab global keys.
func checkSession() error {
	if os.Getenv("XDG_SESSION_TYPE") == "wayland" {
		return fmt.Errorf("%w: Wayland session, use the open mic instead", ErrUnsupported)
	}
	return nil
}
