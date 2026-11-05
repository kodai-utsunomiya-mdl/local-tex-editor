//go:build windows

package platform

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeOutputKeepsUTF8LinesBesideSystemOutput(t *testing.T) {
	prefix := "[作業フォルダー] C:\\研究\\原稿 with spaces\r\n"
	suffix := "日本語の TeX の出力\n"
	data := append([]byte(prefix), 0xff, '\r', '\n')
	data = append(data, []byte(suffix)...)
	got := DecodeOutput(data)
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
		t.Fatalf("UTF-8 lines changed while reading system output: %q", got)
	}
	if !utf8.ValidString(got) || strings.Count(got, "\n") != 3 {
		t.Fatalf("decoded output is not complete UTF-8 text: %q", got)
	}
}

func TestDecodeOutputKeepsTextAfterEmbeddedNUL(t *testing.T) {
	got := DecodeOutput([]byte{0xff, ' ', 0, ' ', 'o', 'k', '\n'})
	if !strings.HasSuffix(got, " \x00 ok\n") {
		t.Fatalf("output after NUL was lost: %q", got)
	}
}

func TestEncodeFileNameUsesTheSystemCodePage(t *testing.T) {
	if got := string(EncodeFileName("./chapter one.tex")); got != "./chapter one.tex" {
		t.Fatalf("ASCII file name changed: %q", got)
	}
	name := "追加の 原稿.tex"
	encoded := EncodeFileName(name)
	if len(encoded) == 0 || strings.ContainsRune(string(encoded), '\x00') {
		t.Fatalf("invalid encoded file name: %v", encoded)
	}
	// The system may use UTF-8, CP932, or a code page with replacement
	// characters. Its own decoder and encoder should agree on that spelling.
	decoded := decodeSystemCodePage(encoded)
	if got := string(EncodeFileName(decoded)); got != string(encoded) {
		t.Fatalf("the system spelling changed on a round trip: %v -> %q -> %v", encoded, decoded, []byte(got))
	}
}
