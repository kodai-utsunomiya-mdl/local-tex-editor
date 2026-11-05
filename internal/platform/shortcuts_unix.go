//go:build !windows

package platform

import "os"

func CreateShortcut(_ string) (ShortcutResult, error) {
	return ShortcutResult{}, ErrShortcutsUnsupported
}

func commitShortcutFile(source, target string, replace bool) error {
	if replace {
		return ReplaceFile(source, target)
	}
	if err := os.Link(source, target); err != nil {
		return err
	}
	return os.Remove(source)
}
