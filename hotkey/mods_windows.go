package hotkey

import "golang.design/x/hotkey"

var platformModifiers = map[string]hotkey.Modifier{
	"alt":   hotkey.ModAlt,
	"super": hotkey.ModWin,
}

func checkSession() error { return nil }
