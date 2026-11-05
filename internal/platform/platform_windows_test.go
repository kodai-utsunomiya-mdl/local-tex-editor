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
