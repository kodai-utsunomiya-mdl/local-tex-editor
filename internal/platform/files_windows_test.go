//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCloudFileTags(t *testing.T) {
	for variant := uint32(0); variant < 16; variant++ {
		tag := uint32(0x9000001a) | (variant << 12)
		if !isCloudFileTag(tag) {
			t.Errorf("Cloud Files tag 0x%08x was rejected", tag)
		}
	}
	for _, tag := range []uint32{0, 0xa000000c, 0xa0000003, 0x8000001b, 0xa000001a, 0x9001001a} {
		if isCloudFileTag(tag) {
			t.Errorf("non-cloud tag 0x%08x was accepted", tag)
		}
	}
}

func TestWindowsFileAttributesAndRedirects(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "原稿 with spaces.tex")
	if err := os.WriteFile(path, []byte("text"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if regular, err := IsRegularFile(path, info); err != nil || !regular {
		t.Fatalf("regular file = %v, %v", regular, err)
	}
	if blocked, err := IsReparsePoint(path); err != nil || blocked {
		t.Fatalf("ordinary file redirect = %v, %v", blocked, err)
	}
	t.Run("symbolic link", func(t *testing.T) {
		link := filepath.Join(directory, "redirect.tex")
		if err := os.Symlink(path, link); err != nil {
			t.Skipf("symbolic links are unavailable to this user: %v", err)
		}
		attributes, err := readFileAttributeTag(link)
		if err != nil || attributes.tag != 0xa000000c {
			t.Fatalf("symbolic link attributes = %+v, %v", attributes, err)
		}
		if blocked, err := IsReparsePoint(link); err != nil || !blocked {
			t.Fatalf("symbolic link redirect = %v, %v", blocked, err)
		}
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if regular, err := IsRegularFile(link, info); err != nil || regular {
			t.Fatalf("symbolic link regular = %v, %v", regular, err)
		}
	})
}
