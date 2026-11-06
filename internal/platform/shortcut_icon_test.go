package platform

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/branding"
)

func TestShortcutIconIsInstalledOnceAndRejectsConflictingContents(t *testing.T) {
	directory := t.TempDir()
	path, created, err := installShortcutIcon(directory)
	if err != nil || !created {
		t.Fatalf("install icon: %q %v %v", path, created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, branding.Icon) {
		t.Fatalf("installed icon: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	samePath, created, err := installShortcutIcon(directory)
	if err != nil || created || samePath != path {
		t.Fatalf("reuse icon: %q %v %v", samePath, created, err)
	}
	second, err := os.Stat(path)
	if err != nil || !os.SameFile(first, second) {
		t.Fatalf("unchanged artwork was rewritten: %v", err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installShortcutIcon(directory); !errors.Is(err, ErrShortcutConflict) {
		t.Fatalf("conflicting icon contents were accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("conflicting file was changed: %v", err)
	}
}
