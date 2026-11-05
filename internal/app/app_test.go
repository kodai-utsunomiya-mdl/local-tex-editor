package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "a-valid-local-token-with-32-characters"

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func testApp(t *testing.T) (*App, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "日本語の原稿 with spaces")
	writeTestFile(t, filepath.Join(root, "main.tex"), []byte("\\documentclass{article}\n\\begin{document}Hello\\end{document}\n"))
	a, err := New(Options{ConfigDir: t.TempDir(), ProjectPath: root, Token: testToken, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	a.SetAddress("127.0.0.1:8765")
	t.Cleanup(func() { _ = a.Close() })
	return a, root
}

func apiRequest(a *App, method, path string, value any, headers map[string]string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	if value != nil {
		_ = json.NewEncoder(&body).Encode(value)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, &body)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Local-Token", testToken)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	return response
}

func TestAuthentication(t *testing.T) {
	a, _ := testApp(t)
	for _, test := range []struct {
		name, token, origin, host, remote string
		want                              int
	}{
		{"same origin", testToken, "http://127.0.0.1:8765", "127.0.0.1:8765", "127.0.0.1:4000", 200},
		{"no origin", testToken, "", "127.0.0.1:8765", "127.0.0.1:4000", 200},
		{"missing token", "", "", "127.0.0.1:8765", "127.0.0.1:4000", 401},
		{"wrong token", "wrong", "", "127.0.0.1:8765", "127.0.0.1:4000", 401},
		{"different origin", testToken, "https://example.test", "127.0.0.1:8765", "127.0.0.1:4000", 403},
		{"null origin", testToken, "null", "127.0.0.1:8765", "127.0.0.1:4000", 403},
		{"different host", testToken, "", "example.test:8765", "127.0.0.1:4000", 403},
		{"network client", testToken, "", "127.0.0.1:8765", "192.168.1.2:4000", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "http://127.0.0.1:8765/api/state", nil)
			request.RemoteAddr, request.Host = test.remote, test.host
			request.Header.Set("X-Local-Token", test.token)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			a.Handler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
				t.Fatal("API errors must have a JSON body")
			}
		})
	}
}

func TestFileSavePreservesCRLFAndBOMAndRejectsExternalChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "本文.tex")
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte("日本語\r\n第二行\r\n")...)
	writeTestFile(t, path, original)
	document, err := readDocument(root, "本文.tex")
	if err != nil {
		t.Fatal(err)
	}
	if document.Content != "日本語\n第二行\n" || document.LineEnding != "crlf" {
		t.Fatalf("unexpected document: %#v", document)
	}
	document.Content += "第三行\n"
	saved, err := saveDocument(root, document)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(path)
	expected := append([]byte{0xef, 0xbb, 0xbf}, []byte("日本語\r\n第二行\r\n第三行\r\n")...)
	if !bytes.Equal(actual, expected) {
		t.Fatalf("saved bytes = %q", actual)
	}
	if saved.Revision == document.Revision {
		t.Fatal("the revision should change after the content changed")
	}
	external := []byte("changed outside the editor\n")
	writeTestFile(t, path, external)
	saved.Content = "this must not overwrite the external change"
	_, err = saveDocument(root, saved)
	var conflict *apiError
	if !errors.As(err, &conflict) || conflict.status != http.StatusConflict {
		t.Fatalf("want HTTP 409, got %v", err)
	}
	actual, _ = os.ReadFile(path)
	if !bytes.Equal(actual, external) {
		t.Fatal("external edits were overwritten")
	}
}

func TestNewFileCannotOverwriteAnExistingFile(t *testing.T) {
	root := t.TempDir()
	input := FileDocument{Path: "notes.tex", Content: "first\n"}
	first, err := saveDocument(root, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == "" {
		t.Fatal("a new file needs a revision")
	}
	input.Content = "second\n"
	_, err = saveDocument(root, input)
	var conflict *apiError
	if !errors.As(err, &conflict) || conflict.status != 409 {
		t.Fatalf("expected conflict, got %v", err)
	}
	actual, _ := os.ReadFile(filepath.Join(root, "notes.tex"))
	if string(actual) != "first\n" {
		t.Fatal("new-file operation overwrote an existing file")
	}
}

func TestFileAccessStaysInsideProject(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../outside.tex", "sub/../../outside.tex", `..\outside.tex`, "/tmp/outside.tex", `C:\outside.tex`, "file.tex:stream", ".git/config", ".local-tex-editor/build/x.tex", "NUL.tex", "part./main.tex"} {
		t.Run(path, func(t *testing.T) {
			if _, err := saveDocument(root, FileDocument{Path: path, Content: "forbidden"}); err == nil {
				t.Fatal("unsafe path was accepted")
			}
		})
	}
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.tex"), []byte("outside"))
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if _, err := readDocument(root, "linked/secret.tex"); err == nil {
		t.Fatal("symlink allowed reading outside the project")
	}
	files, err := listProjectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("linked files appeared in the project: %v", files)
	}
}

func TestGeneratedDirectoryCannotBeALink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".local-tex-editor")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	if _, err := ensureBuildDirectory(root); err == nil {
		t.Fatal("linked build directory was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "build")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an outside directory was created: %v", err)
	}
}

func TestProjectHeaderPreventsSavingIntoAnotherProject(t *testing.T) {
	a, first := testApp(t)
	document, err := readDocument(first, "main.tex")
	if err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	writeTestFile(t, filepath.Join(second, "main.tex"), []byte(document.Content))
	opened := apiRequest(a, "POST", "/api/project/open", map[string]string{"path": second}, nil)
	if opened.Code != 200 {
		t.Fatalf("open failed: %s", opened.Body.String())
	}
	document.Content = "wrong project"
	response := apiRequest(a, "PUT", "/api/file", document, map[string]string{"X-Project-Root": url.PathEscape(first)})
	if response.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", response.Code, response.Body.String())
	}
	actual, _ := os.ReadFile(filepath.Join(second, "main.tex"))
	if string(actual) == document.Content {
		t.Fatal("a stale browser tab changed a different project")
	}
	response = apiRequest(a, "POST", "/api/build", nil, map[string]string{"X-Project-Root": url.PathEscape(first)})
	if response.Code != 409 {
		t.Fatalf("a stale tab could compile another project: %s", response.Body.String())
	}
}

func TestMissingCompilerProducesAnExplicitBuildError(t *testing.T) {
	a, _ := testApp(t)
	a.mu.Lock()
	a.config.TexBin = filepath.Join(t.TempDir(), "TeX is not installed here")
	a.mu.Unlock()
	response := apiRequest(a, "POST", "/api/build", nil, nil)
	if response.Code != 202 {
		t.Fatalf("compile request failed: %s", response.Body.String())
	}
	a.workers.Wait()
	response = apiRequest(a, "GET", "/api/build", nil, nil)
	var build Build
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}
	if build.Status != "failed" || build.Error == "" || build.Log == "" || build.PDFReady {
		t.Fatalf("missing compiler must be visible: %#v", build)
	}
	if build.ExitCode != nil {
		t.Fatal("no process was started, so no process exit code exists")
	}
}

func TestPDFDiscoveryUsesTheCurrentMainAndCustomJobName(t *testing.T) {
	root := t.TempDir()
	directory, err := ensureBuildDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "renamed.pdf"), []byte("%PDF-1.7\ncurrent PDF\n"))
	writeTestFile(t, filepath.Join(directory, "main.pdf"), []byte("%PDF-1.7\nstale PDF\n"))
	fdb := `["latex"] 1 "./main.tex" ".local-tex-editor/build/renamed.dvi" "renamed" 1 0
["dvipdf"] 1 ".local-tex-editor/build/renamed.dvi" ".local-tex-editor/build/renamed.pdf" "renamed" 1 0
`
	writeTestFile(t, filepath.Join(directory, "renamed.fdb_latexmk"), []byte(fdb))
	actual, err := findOutputPDF(root, "main.tex", directory)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(actual, filepath.Join(directory, "renamed.pdf")) {
		t.Fatalf("found %s", actual)
	}
	// A successful DVI-only run must not expose an older PDF as its new output.
	fdb = `["latex"] 1 "./main.tex" ".local-tex-editor/build/renamed.dvi" "renamed" 1 0` + "\n"
	writeTestFile(t, filepath.Join(directory, "renamed.fdb_latexmk"), []byte(fdb))
	if path, err := findOutputPDF(root, "main.tex", directory); err == nil {
		t.Fatalf("accepted stale PDF: %s", path)
	}
}

func TestPDFRangeAndFailureKeepThePreviousSuccessfulPDF(t *testing.T) {
	a, root := testApp(t)
	pdf := []byte("%PDF-1.7\nexample PDF bytes\n%%EOF\n")
	path := filepath.Join(t.TempDir(), "result.pdf")
	writeTestFile(t, path, pdf)
	a.mu.Lock()
	a.lastPDF, a.lastPDFID = path, "previous-success"
	run := &buildRun{started: time.Now(), result: Build{ID: "failed-run", Status: "running"}}
	a.build = run
	a.mu.Unlock()
	code := 1
	a.finishBuild(run, &code, errors.New("TeX error"), "", "")
	response := apiRequest(a, "GET", "/api/pdf?build=previous-success", nil, map[string]string{"Range": "bytes=0-4", "X-Project-Root": url.PathEscape(root)})
	if response.Code != 206 || response.Body.String() != "%PDF-" {
		t.Fatalf("PDF range failed: %d %q", response.Code, response.Body.String())
	}
	response = apiRequest(a, "GET", "/api/build", nil, nil)
	var build Build
	_ = json.Unmarshal(response.Body.Bytes(), &build)
	if build.Status != "failed" || !build.PDFReady || build.PDFBuildID != "previous-success" || build.ExitCode == nil || *build.ExitCode != 1 {
		t.Fatalf("failure state lost the successful PDF: %#v", build)
	}
}

func TestPDFCopiesAreBounded(t *testing.T) {
	a, root := testApp(t)
	path := filepath.Join(t.TempDir(), "result.pdf")
	writeTestFile(t, path, []byte("%PDF-1.7\nexample\n"))
	var last string
	for index := 0; index < 4; index++ {
		id, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		last, err = a.copySuccessfulPDF(root, path, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(last); err != nil {
		t.Fatalf("the newest PDF was deleted: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(last))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("kept %d PDF copies", len(entries))
	}
}

func TestPDFRangeUsesTheRequestedSuccessfulBuild(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(t.TempDir(), "result.pdf")
	versions := []string{"%PDF-1.7\nfirst version\n", "%PDF-1.7\nsecond version\n", "%PDF-1.7\nthird version\n"}
	ids := []string{}
	for _, content := range versions {
		writeTestFile(t, source, []byte(content))
		id, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		copied, err := a.copySuccessfulPDF(root, source, id)
		if err != nil {
			t.Fatal(err)
		}
		run := &buildRun{started: time.Now(), result: Build{ID: id, Status: "running"}}
		a.mu.Lock()
		a.build = run
		a.mu.Unlock()
		code := 0
		a.finishBuild(run, &code, nil, copied, id)
		if len(ids) == 2 {
			response := apiRequest(a, "GET", "/api/pdf?build="+ids[0], nil, map[string]string{"Range": "bytes=9-13"})
			if response.Code != 206 || response.Body.String() != "first" {
				t.Fatalf("an old PDF range changed after a new compilation: %d %q", response.Code, response.Body.String())
			}
		}
	}
	response := apiRequest(a, "GET", "/api/pdf?build="+ids[1], nil, nil)
	if response.Code != 200 || response.Body.String() != versions[1] {
		t.Fatalf("previous PDF was not available: %d %q", response.Code, response.Body.String())
	}
	response = apiRequest(a, "GET", "/api/pdf?build="+ids[0], nil, nil)
	if response.Code != http.StatusGone {
		t.Fatalf("discarded version status = %d", response.Code)
	}
	response = apiRequest(a, "GET", "/api/pdf?build=../../secret", nil, nil)
	if response.Code != http.StatusGone {
		t.Fatalf("unissued PDF ID status = %d", response.Code)
	}
}

func TestUTF8LogLinesRemainReadableWithAnotherEncoding(t *testing.T) {
	header := "[作業フォルダー] 日本語の原稿\n"
	data := append([]byte(header), []byte{0x83, 0x47, 0x83, 0x89, 0x81, 0x5b, '\n'}...)
	data = append(data, []byte("[終了コード: 1]\n")...)
	result := readableLog(data, true)
	if !strings.HasPrefix(result, header) || !strings.HasSuffix(result, "[終了コード: 1]\n") {
		t.Fatalf("UTF-8 messages changed when a tool used another encoding: %q", result)
	}
	partial := []byte("開始\n日本語")
	if result := readableLog(partial[:len(partial)-1], false); result != "開始\n日本" {
		t.Fatalf("a partial UTF-8 character was decoded prematurely: %q", result)
	}
}
