package app

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

const maxAssetSize int64 = 128 << 20

func assetContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".apng":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/vnd.microsoft.icon"
	case ".avif":
		return "image/avif"
	case ".pdf":
		return "application/pdf"
	default:
		return ""
	}
}

func projectFileKind(path string) string {
	if isTextFile(path) {
		return "text"
	}
	contentType := assetContentType(path)
	if contentType == "application/pdf" {
		return "pdf"
	}
	if contentType != "" {
		return "image"
	}
	return "binary"
}

func (a *App) assetHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodHead) {
		return
	}
	a.mu.Lock()
	err := a.checkProjectLocked(r.Header.Get("X-Project-Root"))
	var root string
	if err == nil {
		if a.project == nil {
			err = fail(http.StatusBadRequest, "原稿フォルダーを開いてください．")
		} else {
			root = a.project.Root
		}
	}
	a.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	clean, err := cleanRelative(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	contentType := assetContentType(clean)
	if contentType == "" {
		writeError(w, fail(http.StatusUnsupportedMediaType, "この種類のファイルにはプレビューがありません．画像または PDF を選んでください．"))
		return
	}
	path, err := safePath(root, clean, false)
	if err != nil {
		writeError(w, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, fmt.Errorf("プレビューするファイルを開けません: %w", err))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, err)
		return
	}
	regular, err := platform.IsRegularFile(path, info)
	if err != nil {
		writeError(w, err)
		return
	}
	if !regular {
		writeError(w, fail(http.StatusBadRequest, "通常の画像または PDF ファイルを選んでください．"))
		return
	}
	if info.Size() > maxAssetSize {
		writeError(w, fail(http.StatusRequestEntityTooLarge, "128 MiB を超えるファイルはプレビューできません．"))
		return
	}
	if !assetHeaderMatches(file, info.Size(), contentType) {
		writeError(w, fail(http.StatusUnsupportedMediaType, "ファイルの内容が拡張子に対応する画像または PDF の形式ではありません．"))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(clean)}))
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The browser UI displays SVG through <img>. These headers also restrict
	// an authenticated request opened as a document instead of an image.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	// A project file may change between requests. Do not use second-resolution
	// modification times for conditional responses. SectionReader also bounds
	// the response if another program appends bytes while it is being served.
	http.ServeContent(w, r, filepath.Base(clean), time.Time{}, io.NewSectionReader(file, 0, info.Size()))
}

func assetHeaderMatches(file io.ReaderAt, size int64, contentType string) bool {
	if size <= 0 {
		return false
	}
	if contentType == "image/svg+xml" {
		decoder := xml.NewDecoder(io.NewSectionReader(file, 0, min(size, 64<<10)))
		for {
			token, err := decoder.Token()
			if err != nil {
				return false
			}
			if start, ok := token.(xml.StartElement); ok {
				return start.Name.Local == "svg" && start.Name.Space == "http://www.w3.org/2000/svg"
			}
		}
	}
	header := make([]byte, min(size, 512))
	if _, err := file.ReadAt(header, 0); err != nil {
		return false
	}
	switch contentType {
	case "image/png":
		return bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return bytes.HasPrefix(header, []byte{0xff, 0xd8, 0xff})
	case "image/gif":
		return bytes.HasPrefix(header, []byte("GIF87a")) || bytes.HasPrefix(header, []byte("GIF89a"))
	case "image/webp":
		return len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP"
	case "image/bmp":
		return bytes.HasPrefix(header, []byte("BM"))
	case "image/vnd.microsoft.icon":
		return bytes.HasPrefix(header, []byte{0, 0, 1, 0})
	case "image/avif":
		if len(header) < 16 || string(header[4:8]) != "ftyp" {
			return false
		}
		boxSize := min(int64(binary.BigEndian.Uint32(header[:4])), int64(len(header)))
		for offset := int64(8); offset+4 <= boxSize; offset += 4 {
			if offset == 12 { // Minor version is between the major and compatible brands.
				continue
			}
			if brand := string(header[offset : offset+4]); brand == "avif" || brand == "avis" {
				return true
			}
		}
		return false
	case "application/pdf":
		return bytes.HasPrefix(header, []byte("%PDF-"))
	default:
		return false
	}
}
