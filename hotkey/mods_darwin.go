package hotkey

import "golang.design/x/hotkey"

var platformModifiers = map[string]hotkey.Modifier{
	"alt":   hotkey.ModOption,
	"super": hotkey.ModCmd,
}

func checkSession() error { return nil }
