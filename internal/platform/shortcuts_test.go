package platform

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeShortcutTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestShortcutInstallationCopiesAndUpdatesItsOwnExecutable(t *testing.T) {
	root := t.TempDir()
	directory, err := shortcutInstallDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "Downloads", "日本語のアプリ with spaces.exe")
	first := []byte("first executable contents\n")
	writeShortcutTestFile(t, source, first)
	installed, err := installShortcutExecutable(source, directory)
	if err != nil {
		t.Fatal(err)
	}
	if installed != filepath.Join(root, "Programs", "LocalTeXEditor", shortcutExecutableName) {
		t.Fatalf("installation path = %s", installed)
	}
	data, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(data, first) {
		t.Fatalf("installed bytes = %q, error = %v", data, err)
	}
	before, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []string{source, installed} {
		if _, err := installShortcutExecutable(current, directory); err != nil {
			t.Fatalf("same executable: %v", err)
		}
		after, err := os.Stat(installed)
		if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("identical executable was replaced: %v", err)
		}
	}
	updated := filepath.Join(root, "Downloads", "next version.exe")
	second := []byte("updated executable contents\n")
	writeShortcutTestFile(t, updated, second)
	if _, err := installShortcutExecutable(updated, directory); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(installed)
	if err != nil || !bytes.Equal(data, second) {
		t.Fatalf("updated bytes = %q, error = %v", data, err)
	}
	manifest, err := readShortcutInstallation(filepath.Join(directory, shortcutManifestName))
	hash, hashErr := shortcutFileHash(installed)
	if err != nil || hashErr != nil || manifest == nil || manifest.SHA256 != hash {
		t.Fatalf("installation record does not describe the updated executable: %#v %v %v", manifest, err, hashErr)
	}
	if data, err := os.ReadFile(source); err != nil || !bytes.Equal(data, first) {
		t.Fatalf("the original download was changed: %v", err)
	}
}

func TestShortcutInstallationLeavesUnrelatedFilesUntouched(t *testing.T) {
	for _, scenario := range []string{"unregistered executable", "changed executable", "unrelated manifest"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			directory, err := shortcutInstallDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "download.exe")
			writeShortcutTestFile(t, source, []byte("this application\n"))
			target := filepath.Join(directory, shortcutExecutableName)
			protected := target
			foreign := []byte("a different file\n")
			if scenario == "changed executable" {
				if _, err := installShortcutExecutable(source, directory); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unrelated manifest" {
				protected = filepath.Join(directory, shortcutManifestName)
				foreign = []byte(`{"belongsTo":"another application"}`)
			}
			writeShortcutTestFile(t, protected, foreign)
			_, err = installShortcutExecutable(source, directory)
			if !errors.Is(err, ErrShortcutConflict) {
				t.Fatalf("expected a conflict, got %v", err)
			}
			data, readErr := os.ReadFile(protected)
			if readErr != nil || !bytes.Equal(data, foreign) {
				t.Fatalf("unrelated file was changed: %q %v", data, readErr)
			}
		})
	}
}

func TestShortcutInstallationRejectsLinkedDestinations(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "Programs")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if _, err := shortcutInstallDirectory(root); !errors.Is(err, ErrShortcutConflict) {
		t.Fatalf("linked Programs directory was accepted: %v", err)
	}
	root = t.TempDir()
	directory, err := shortcutInstallDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(outside, "other.exe")
	foreign := []byte("external executable\n")
	writeShortcutTestFile(t, protected, foreign)
	if err := os.Symlink(protected, filepath.Join(directory, shortcutExecutableName)); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "download.exe")
	writeShortcutTestFile(t, source, []byte("this application\n"))
	if _, err := installShortcutExecutable(source, directory); !errors.Is(err, ErrShortcutConflict) {
		t.Fatalf("linked executable was accepted: %v", err)
	}
	if data, err := os.ReadFile(protected); err != nil || !bytes.Equal(data, foreign) {
		t.Fatalf("external executable was changed: %v", err)
	}
}

func TestShortcutCreationCannotReplaceAnUnexpectedNewFile(t *testing.T) {
	root := t.TempDir()
	staged, target := filepath.Join(root, "staged.tmp"), filepath.Join(root, "target.exe")
	writeShortcutTestFile(t, staged, []byte("new contents\n"))
	writeShortcutTestFile(t, target, []byte("file created by another operation\n"))
	if err := commitShortcutFile(staged, target, false); err == nil {
		t.Fatal("new-file placement replaced an existing file")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "file created by another operation\n" {
		t.Fatalf("existing file was changed: %q %v", data, err)
	}
}
