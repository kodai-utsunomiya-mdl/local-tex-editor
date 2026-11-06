//go:build windows

package platform

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/branding"
)

func TestRefreshShortcutsDoesNotCreateAnInstallation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "download.exe")
	writeShortcutTestFile(t, source, []byte("MZ new application"))
	if err := refreshInstalledShortcutsAt(source, root, []string{filepath.Join(root, "Desktop")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Programs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("starting a portable application created an installation: %v", err)
	}
}

func TestRefreshShortcutsUpdatesTheInstalledApplicationAndExistingLinks(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "old", "editor.exe")
	newer := filepath.Join(root, "new", "editor.exe")
	writeShortcutTestFile(t, old, []byte("MZ version 0.1.1"))
	newBytes := []byte("MZ version 0.1.3")
	writeShortcutTestFile(t, newer, newBytes)
	desktop, programs := filepath.Join(root, "Desktop"), filepath.Join(root, "Start Menu")
	for _, dir := range []string{desktop, programs} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	first, err := createShortcutAt(old, root, desktop)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createShortcutAt(old, root, programs); err != nil {
		t.Fatal(err)
	}
	// The old shortcuts selected the executable's default icon.
	for _, dir := range []string{desktop, programs} {
		if err := writeShellShortcut(filepath.Join(dir, shortcutName), first.Executable, first.Executable); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(root, "Not Created")
	if err := refreshInstalledShortcutsAt(newer, root, []string{desktop, programs, missing}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(first.Executable)
	if err != nil || !bytes.Equal(data, newBytes) {
		t.Fatalf("installed executable was not updated: %v", err)
	}
	for _, dir := range []string{desktop, programs} {
		link, err := readShellShortcut(filepath.Join(dir, shortcutName))
		if err != nil {
			t.Fatal(err)
		}
		requireSameShortcutFile(t, link.Target, first.Executable)
		icon, err := os.ReadFile(link.IconPath)
		if err != nil || !bytes.Equal(icon, branding.Icon) || link.IconIndex != 0 {
			t.Fatalf("shortcut icon was not updated: %#v (%v)", link, err)
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created another shortcut directory: %v", err)
	}
}

func TestRefreshShortcutsLeavesUnrelatedLinksAndChangedExecutables(t *testing.T) {
	root := t.TempDir()
	old, newer, other := filepath.Join(root, "old.exe"), filepath.Join(root, "new.exe"), filepath.Join(root, "other.exe")
	writeShortcutTestFile(t, old, []byte("MZ old"))
	writeShortcutTestFile(t, newer, []byte("MZ new"))
	writeShortcutTestFile(t, other, []byte("MZ other"))
	result, err := createShortcutAt(old, root, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeShellShortcut(result.Shortcut, other, other); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(result.Shortcut)
	if err != nil {
		t.Fatal(err)
	}
	if err := refreshInstalledShortcutsAt(newer, root, []string{root}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(result.Shortcut)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("changed a link to another application: %v", err)
	}
	changed := []byte("MZ separately changed file")
	if err := os.WriteFile(result.Executable, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := refreshInstalledShortcutsAt(newer, root, []string{root}); !errors.Is(err, ErrShortcutConflict) {
		t.Fatalf("expected the existing file conflict: %v", err)
	}
	data, err := os.ReadFile(result.Executable)
	if err != nil || !bytes.Equal(data, changed) {
		t.Fatalf("replaced an unrecognized executable: %v", err)
	}
}

func TestRefreshShortcutsLeavesUserArgumentsAndWorkingDirectory(t *testing.T) {
	for _, property := range []string{"arguments", "working directory"} {
		t.Run(property, func(t *testing.T) {
			root := t.TempDir()
			old, newer := filepath.Join(root, "old.exe"), filepath.Join(root, "new.exe")
			writeShortcutTestFile(t, old, []byte("MZ old application"))
			newBytes := []byte("MZ new application")
			writeShortcutTestFile(t, newer, newBytes)
			result, err := createShortcutAt(old, root, root)
			if err != nil {
				t.Fatal(err)
			}
			customDirectory := filepath.Join(root, "原稿の作業フォルダー")
			if err := os.Mkdir(customDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			index, value := 11, `--project "`+customDirectory+`" --config "`+filepath.Join(root, "settings")+`"`
			if property == "working directory" {
				index, value = 9, customDirectory
			}
			err = withShellShortcut(func(link, persistent *nativeCOMObject) error {
				path, err := syscall.UTF16PtrFromString(result.Shortcut)
				if err != nil {
					return err
				}
				if result := persistent.call(5, uintptr(unsafe.Pointer(path)), 0); int32(result) < 0 {
					return shortcutCOMError("読み取り", result)
				}
				wide, err := syscall.UTF16PtrFromString(value)
				if err != nil {
					return err
				}
				if result := link.call(index, uintptr(unsafe.Pointer(wide))); int32(result) < 0 {
					return shortcutCOMError("設定", result)
				}
				if result := persistent.call(6, uintptr(unsafe.Pointer(path)), 1); int32(result) < 0 {
					return shortcutCOMError("保存", result)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			link, err := readShellShortcut(result.Shortcut)
			if err != nil {
				t.Fatal(err)
			}
			if property == "arguments" && link.Arguments != value {
				t.Fatalf("custom arguments were not written: %q", link.Arguments)
			}
			if property == "working directory" && !shortcutSameFile(link.WorkingDirectory, customDirectory) {
				t.Fatalf("custom working directory was not written: %q", link.WorkingDirectory)
			}
			before, err := os.ReadFile(result.Shortcut)
			if err != nil {
				t.Fatal(err)
			}
			if err := refreshInstalledShortcutsAt(newer, root, []string{root}); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(result.Shortcut)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("changed a shortcut with user %s: %v", property, err)
			}
			installed, err := os.ReadFile(result.Executable)
			if err != nil || !bytes.Equal(installed, newBytes) {
				t.Fatalf("installed executable was not updated: %v", err)
			}
		})
	}
}
