package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSyncTeXUsesTheSelectedTeXInstallation(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "TeX programs")
	if _, err := findSynctex(bin); err == nil {
		t.Fatal("a missing executable was accepted")
	}
	if _, err := findSynctex(""); err == nil {
		t.Fatal("an empty binary directory was accepted")
	}
	name := "synctex"
	if runtime.GOOS == "windows" {
		name = "synctex.exe"
		writeTestFile(t, filepath.Join(bin, "synctex.bat"), []byte("not a TeX Live executable"))
		if _, err := findSynctex(bin); err == nil {
			t.Fatal("a batch file was substituted for the missing TeX Live executable")
		}
	}
	path := filepath.Join(bin, name)
	writeTestFile(t, path, []byte("test fixture"))
	got, err := findSynctex(bin)
	if err != nil || got != path {
		t.Fatalf("executable = %q, %v; want %q", got, err, path)
	}
	tool, message, commands := synctexStatus(bin)
	if tool != path || message != "" || len(commands) != 0 {
		t.Fatalf("installed SyncTeX status = %q, %q, %v", tool, message, commands)
	}
}

func TestSyncTeXRepairUsesTheInstallationYear(t *testing.T) {
	root := filepath.Join(t.TempDir(), "author's TeX", "2024")
	bin := filepath.Join(root, "bin", "windows")
	writeTestFile(t, filepath.Join(root, "release-texlive.txt"), []byte("TeX Live (https://tug.org/texlive) version 2025\n"))
	writeTestFile(t, filepath.Join(root, "tlpkg", "texlive.tlpdb"), []byte("name 00texlive.config\ncategory TLCore\ndepend release/2023\n\nname something\n"))
	if year := texLiveYear(bin); year != 2025 {
		t.Fatalf("release file year = %d", year)
	}
	if err := os.Remove(filepath.Join(root, "release-texlive.txt")); err != nil {
		t.Fatal(err)
	}
	if year := texLiveYear(bin); year != 2023 {
		t.Fatalf("package database year = %d", year)
	}
	if err := os.Remove(filepath.Join(root, "tlpkg", "texlive.tlpdb")); err != nil {
		t.Fatal(err)
	}
	if year := texLiveYear(bin); year != 2024 {
		t.Fatalf("installation directory year = %d", year)
	}
	commands := synctexRepairCommands(bin, 2025, 2026)
	if len(commands) != 2 || !strings.Contains(commands[0], "/2025/tlnet-final' install --reinstall synctex synctex.windows") || !strings.Contains(commands[0], "author''s TeX") {
		t.Fatalf("PowerShell repair commands = %v", commands)
	}
	if current := synctexRepairCommands(bin, 2026, 2026); !strings.Contains(current[0], "mirror.ctan.org/systems/texlive/tlnet") {
		t.Fatalf("current release command = %v", current)
	}
	if commands := synctexRepairCommands(bin, 0, 2026); len(commands) != 0 {
		t.Fatal("an installation with an unknown year should not select an arbitrary repository")
	}
}
