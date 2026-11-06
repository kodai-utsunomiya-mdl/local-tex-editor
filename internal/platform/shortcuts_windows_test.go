//go:build windows

package platform

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/branding"
)

func TestWindowsShortcutRoundTripInTemporaryFolders(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Downloads", "日本語のアプリ with spaces.exe")
	writeShortcutTestFile(t, source, []byte("MZ executable fixture\n"))
	localAppData := filepath.Join(root, "日本語の AppData")
	desktop := filepath.Join(root, "OneDrive - example", "デスクトップ")
	for _, directory := range []string{localAppData, desktop} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	result, err := createShortcutAt(source, localAppData, desktop)
	if err != nil {
		t.Fatal(err)
	}
	if result.Shortcut != filepath.Join(desktop, shortcutName) || result.Executable != filepath.Join(localAppData, "Programs", "LocalTeXEditor", shortcutExecutableName) {
		t.Fatalf("unexpected paths: %#v", result)
	}
	checkLink := func(path string) {
		t.Helper()
		link, err := readShellShortcut(path)
		if err != nil {
			t.Fatal(err)
		}
		actual, actualErr := os.Stat(link.Target)
		expected, expectedErr := os.Stat(result.Executable)
		if actualErr != nil || expectedErr != nil || !os.SameFile(actual, expected) {
			t.Fatalf("shortcut target = %q, want %q (%v %v)", link.Target, result.Executable, actualErr, expectedErr)
		}
		actualDirectory, actualErr := filepath.EvalSymlinks(link.WorkingDirectory)
		expectedDirectory, expectedErr := filepath.EvalSymlinks(filepath.Dir(result.Executable))
		if actualErr != nil || expectedErr != nil || !shortcutPathsEqual(actualDirectory, expectedDirectory) || link.Arguments != "" {
			t.Fatalf("shortcut working directory or arguments: %#v", link)
		}
		icon, err := os.ReadFile(link.IconPath)
		if err != nil || !bytes.Equal(icon, branding.Icon) || link.IconIndex != 0 || !shortcutPathsEqual(filepath.Dir(link.IconPath), filepath.Dir(result.Executable)) {
			t.Fatalf("shortcut icon is not the installed artwork: %#v (%v)", link, err)
		}
	}
	checkLink(result.Shortcut)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	checkLink(result.Shortcut)
	if _, err := createShortcutAt(result.Executable, localAppData, desktop); err != nil {
		t.Fatalf("creating the same shortcut from the installed executable: %v", err)
	}
	checkLink(result.Shortcut)
	startMenu := filepath.Join(root, "スタートメニュー", "Programs")
	if err := os.MkdirAll(startMenu, 0700); err != nil {
		t.Fatal(err)
	}
	second, err := createShortcutAt(result.Executable, localAppData, startMenu)
	if err != nil || !shortcutPathsEqual(second.Executable, result.Executable) {
		t.Fatalf("another destination: %#v %v", second, err)
	}
	checkLink(second.Shortcut)
}

func TestWindowsShortcutLeavesAnotherApplicationLinkUntouched(t *testing.T) {
	root := t.TempDir()
	source, other := filepath.Join(root, "this.exe"), filepath.Join(root, "other.exe")
	writeShortcutTestFile(t, source, []byte("this application\n"))
	writeShortcutTestFile(t, other, []byte("another application\n"))
	shortcut := filepath.Join(root, shortcutName)
	if err := writeShellShortcut(shortcut, other, other); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(shortcut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createShortcutAt(source, root, root); !errors.Is(err, ErrShortcutConflict) {
		t.Fatalf("unrelated shortcut was accepted: %v", err)
	}
	after, err := os.ReadFile(shortcut)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("another application's shortcut was changed: %v", err)
	}
}

func TestWindowsShortcutUpdatesThePreviousExecutableIcon(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "download.exe")
	writeShortcutTestFile(t, source, []byte("MZ executable fixture\n"))
	result, err := createShortcutAt(source, root, root)
	if err != nil {
		t.Fatal(err)
	}
	// v0.1.1 used the installed executable itself as IconLocation, and skipped
	// rewriting a link that already pointed to that executable.
	if err := writeShellShortcut(result.Shortcut, result.Executable, result.Executable); err != nil {
		t.Fatal(err)
	}
	before, err := readShellShortcut(result.Shortcut)
	if err != nil || !shortcutPathsEqual(before.IconPath, result.Executable) {
		t.Fatalf("pre-update shortcut: %#v (%v)", before, err)
	}
	if _, err := createShortcutAt(result.Executable, root, root); err != nil {
		t.Fatal(err)
	}
	after, err := readShellShortcut(result.Shortcut)
	if err != nil || !shortcutPathsEqual(after.Target, result.Executable) || shortcutPathsEqual(after.IconPath, before.IconPath) {
		t.Fatalf("updated shortcut did not acquire a new icon path: %#v (%v)", after, err)
	}
	data, err := os.ReadFile(after.IconPath)
	if err != nil || !bytes.Equal(data, branding.Icon) {
		t.Fatalf("updated shortcut artwork: %v", err)
	}
}

func TestWindowsShortcutUpdateExplainsALockedExecutable(t *testing.T) {
	root := t.TempDir()
	directory, err := shortcutInstallDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "first.exe")
	writeShortcutTestFile(t, source, []byte("first executable\n"))
	target, err := installShortcutExecutable(source, directory)
	if err != nil {
		t.Fatal(err)
	}
	wide, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(wide, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if handle != syscall.InvalidHandle {
			_ = syscall.CloseHandle(handle)
		}
	}()
	updated := filepath.Join(root, "updated.exe")
	writeShortcutTestFile(t, updated, []byte("new executable\n"))
	if _, err := installShortcutExecutable(updated, directory); !errors.Is(err, ErrShortcutConflict) {
		t.Fatalf("locked executable update: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "first executable\n" {
		t.Fatalf("locked executable was changed: %q %v", data, err)
	}
	if err := syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = syscall.InvalidHandle
	if _, err := installShortcutExecutable(updated, directory); err != nil {
		t.Fatalf("retry after releasing the executable: %v", err)
	}
}
