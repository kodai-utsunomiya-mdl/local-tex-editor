package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestProjectListsAssetsAndEmptyDirectories(t *testing.T) {
	a, root := testApp(t)
	fixtures := map[string]struct{ kind, content string }{
		"figures/日本語の図.PNG":  {"image", "image bytes"},
		"figures/chart.pdf":  {"pdf", "%PDF-1.7\n"},
		"figures/vector.svg": {"image", `<svg xmlns="http://www.w3.org/2000/svg"/>`},
		"data/table.csv":     {"text", "x,y\n1,2\n"},
		"data/source.zip":    {"binary", "archive bytes"},
		"figures/chart.eps":  {"binary", "%!PS-Adobe-3.0 EPSF-3.0\n"},
		"page.html":          {"binary", "<html>project contents</html>"},
	}
	for name, fixture := range fixtures {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), []byte(fixture.content))
	}
	for _, name := range []string{"empty", "空のフォルダー/子フォルダー", "figures/empty"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(name)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".git", "node_modules", ".local-tex-editor"} {
		writeTestFile(t, filepath.Join(root, name, "hidden.png"), testPNG(t))
	}
	response := apiRequest(a, http.MethodGet, "/api/state", nil, nil)
	var state State
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Project == nil {
		t.Fatalf("state: %d %s", response.Code, response.Body.String())
	}
	wantDirectories := []string{"data", "empty", "figures", "figures/empty", "空のフォルダー", "空のフォルダー/子フォルダー"}
	if !reflect.DeepEqual(state.Project.Directories, wantDirectories) {
		t.Fatalf("directories = %v, want %v", state.Project.Directories, wantDirectories)
	}
	if len(state.Project.Files) != len(fixtures)+1 {
		t.Fatalf("unexpected files: %v", state.Project.Files)
	}
	for _, file := range state.Project.Files {
		if file.Path == "main.tex" {
			if file.Kind != "text" {
				t.Fatalf("main.tex kind = %q", file.Kind)
			}
			continue
		}
		fixture, exists := fixtures[file.Path]
		if !exists || file.Kind != fixture.kind || file.Size != int64(len(fixture.content)) {
			t.Fatalf("incorrect file metadata: %#v", file)
		}
	}
	revisions := snapshotSourceRevisions(root)
	if len(revisions) != 2 || revisions["main.tex"] == "" || revisions["data/table.csv"] == "" {
		t.Fatalf("source revisions include assets: %#v", revisions)
	}
}

func TestAssetPreviewSupportsImagesPDFAndRanges(t *testing.T) {
	a, root := testApp(t)
	fixtures := []struct {
		path, contentType string
		data              []byte
	}{
		{"figures/日本語の図 with spaces.PNG", "image/png", testPNG(t)},
		{"figures/chart.pdf", "application/pdf", []byte("%PDF-1.7\nPDF bytes\n%%EOF\n")},
		{"figures/vector.svg", "image/svg+xml", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><script>alert(1)</script><circle cx="10" cy="10" r="8"/></svg>`)},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.path, func(t *testing.T) {
			writeTestFile(t, filepath.Join(root, filepath.FromSlash(fixture.path)), fixture.data)
			endpoint := "/api/asset?path=" + url.QueryEscape(fixture.path)
			response := apiRequest(a, http.MethodGet, endpoint, nil, map[string]string{"X-Project-Root": url.PathEscape(root)})
			if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), fixture.data) {
				t.Fatalf("asset: %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Content-Type") != fixture.contentType || response.Header().Get("Content-Length") != fmt.Sprint(len(fixture.data)) {
				t.Fatalf("asset headers: %v", response.Header())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("missing asset response restrictions: %v", response.Header())
			}
			if !strings.Contains(response.Header().Get("Content-Security-Policy"), "sandbox; default-src 'none'") {
				t.Fatalf("asset document needs a sandbox: %v", response.Header())
			}
			response = apiRequest(a, http.MethodGet, endpoint, nil, map[string]string{"Range": "bytes=0-4"})
			if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), fixture.data[:5]) || response.Header().Get("Content-Range") != fmt.Sprintf("bytes 0-4/%d", len(fixture.data)) {
				t.Fatalf("asset range: %d %q %v", response.Code, response.Body.Bytes(), response.Header())
			}
			response = apiRequest(a, http.MethodHead, endpoint, nil, nil)
			if response.Code != http.StatusOK || response.Body.Len() != 0 || response.Header().Get("Content-Length") != fmt.Sprint(len(fixture.data)) {
				t.Fatalf("asset HEAD: %d %v", response.Code, response.Header())
			}
			response = apiRequest(a, http.MethodGet, "/api/file?path="+url.QueryEscape(fixture.path), nil, nil)
			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("asset entered the text editor: %d %s", response.Code, response.Body.String())
			}
			response = apiRequest(a, http.MethodPut, "/api/file", FileDocument{Path: fixture.path, Content: "overwritten", Revision: revision(fixture.data)}, nil)
			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("asset was accepted for text saving: %d %s", response.Code, response.Body.String())
			}
			actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(fixture.path)))
			if err != nil || !bytes.Equal(actual, fixture.data) {
				t.Fatalf("asset changed through the text API: %v", err)
			}
		})
	}
}

func TestAssetPreviewRejectsUnsupportedContentAndUnsafePaths(t *testing.T) {
	a, root := testApp(t)
	for _, name := range []string{"page.html", "disguised.png", "disguised.pdf", "disguised.svg"} {
		writeTestFile(t, filepath.Join(root, name), []byte("<!doctype html><html><script>alert(1)</script></html>"))
	}
	writeTestFile(t, filepath.Join(root, "empty.png"), nil)
	writeTestFile(t, filepath.Join(root, "archive.zip"), []byte("PK\x03\x04"))
	if err := os.MkdirAll(filepath.Join(root, "folder.png"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want int
	}{
		{"page.html", 415}, {"main.tex", 415}, {"archive.zip", 415},
		{"disguised.png", 415}, {"disguised.pdf", 415}, {"disguised.svg", 415}, {"empty.png", 415},
		{"folder.png", 400}, {"missing.png", 404}, {"../outside.png", 400}, {`..\outside.png`, 400},
		{".git/secret.png", 400}, {".local-tex-editor/build/main.pdf", 400}, {"/outside.png", 400},
		{`C:\outside.png`, 400}, {"file.png:stream", 400}, {"NUL.png", 400},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := apiRequest(a, http.MethodGet, "/api/asset?path="+url.QueryEscape(test.path), nil, nil)
			if response.Code != test.want || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("%q: %d %s", test.path, response.Code, response.Body.String())
			}
		})
	}
	large, err := os.Create(filepath.Join(root, "too-large.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(maxAssetSize + 1); err != nil {
		_ = large.Close()
		t.Fatal(err)
	}
	_ = large.Close()
	response := apiRequest(a, http.MethodGet, "/api/asset?path=too-large.pdf", nil, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized asset: %d %s", response.Code, response.Body.String())
	}
}

func TestAssetPreviewRequiresCurrentProjectAndAuthentication(t *testing.T) {
	a, root := testApp(t)
	writeTestFile(t, filepath.Join(root, "figure.png"), testPNG(t))
	for _, test := range []struct {
		headers map[string]string
		want    int
	}{
		{map[string]string{"X-Local-Token": ""}, 401},
		{map[string]string{"Origin": "https://example.test"}, 403},
		{map[string]string{"X-Project-Root": url.PathEscape(filepath.Join(root, "other"))}, 409},
	} {
		response := apiRequest(a, http.MethodGet, "/api/asset?path=figure.png", nil, test.headers)
		if response.Code != test.want {
			t.Fatalf("headers %v: %d %s", test.headers, response.Code, response.Body.String())
		}
	}
}

func TestAssetPreviewReadsChangesWithTheSameTimestamp(t *testing.T) {
	a, root := testApp(t)
	path := filepath.Join(root, "figure.pdf")
	modified := time.Now().Truncate(time.Second)
	for _, text := range []string{"%PDF-1.7\nfirst\n", "%PDF-1.7\nother\n"} {
		writeTestFile(t, path, []byte(text))
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
		response := apiRequest(a, http.MethodGet, "/api/asset?path=figure.pdf", nil, map[string]string{"If-Modified-Since": modified.UTC().Format(http.TimeFormat)})
		if response.Code != http.StatusOK || response.Body.String() != text {
			t.Fatalf("asset changed during the same second: %d %q", response.Code, response.Body.String())
		}
	}
}

func TestAssetPreviewRejectsLinkedFilesAndDirectories(t *testing.T) {
	a, root := testApp(t)
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.png"), testPNG(t))
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(root, "linked.png")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"linked/secret.png", "linked.png"} {
		response := apiRequest(a, http.MethodGet, "/api/asset?path="+url.QueryEscape(path), nil, nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("linked asset %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	files, directories, err := listProjectEntries(root)
	if err != nil || len(files) != 1 || files[0].Path != "main.tex" || len(directories) != 0 {
		t.Fatalf("linked entries appeared in project: %v %v %v", files, directories, err)
	}
}
