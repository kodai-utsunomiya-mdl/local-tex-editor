package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

func TestSyncTeXNamesAndAnchorsForUnicodeSources(t *testing.T) {
	_, root := testApp(t)
	name := "章/追加の 原稿.tex"
	writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), []byte("first\nsecond\n"))
	outside := filepath.Join(filepath.Dir(root), "outside.tex")
	writeTestFile(t, outside, []byte("outside"))
	input := "SyncTeX Version:1\nInput:1:./main.tex\nInput:2:./章/追加の 原稿.tex\nInput:3:" + filepath.ToSlash(outside) + "\nOutput:pdf\nContent:\n!1\n{1\n!1\n}1\nPostamble:\n"
	var output bytes.Buffer
	inputs, err := normalizeSyncTeX(strings.NewReader(input), &output, root, snapshotSourceRevisions(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 3 || inputs[1].Alias != "input-2.tex" || inputs[1].Path != name || inputs[1].Revision != revision([]byte("first\nsecond\n")) || inputs[2].Path != "" {
		t.Fatalf("unexpected Input mapping: %#v", inputs)
	}
	if strings.Contains(output.String(), "追加") || strings.Contains(output.String(), outside) {
		t.Fatalf("the CLI data still has original file names: %s", output.String())
	}
	bytesSinceAnchor := 0
	for _, line := range strings.SplitAfter(output.String(), "\n") {
		if strings.HasPrefix(line, "!") {
			count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil || count != bytesSinceAnchor {
				t.Fatalf("anchor has %d bytes, expected %d", count, bytesSinceAnchor)
			}
			bytesSinceAnchor = 0
		}
		bytesSinceAnchor += len(line)
	}
}

func TestSyncTeXMatchesWindowsFileNamesUsingTheRecorder(t *testing.T) {
	_, root := testApp(t)
	first, second := "追加の 原稿.tex", "別の名 原稿.tex"
	writeTestFile(t, filepath.Join(root, first), []byte("one\n"))
	writeTestFile(t, filepath.Join(root, second), []byte("two\n"))
	revisions := snapshotSourceRevisions(root)
	// Reproduce a legacy code page which cannot represent either Japanese
	// name. The recorder gives us the actual UTF-8 paths opened by TeX.
	lossy := func(name string) []byte {
		return []byte(strings.Map(func(r rune) rune {
			if r > 127 {
				return '?'
			}
			return r
		}, name))
	}
	recordedPath := filepath.Join(root, ".local-tex-editor", "build", "main.fls")
	writeTestFile(t, recordedPath, []byte("INPUT ./main.tex\nINPUT ./"+first+"\nINPUT ./"+first+"\n"))
	recorded := recorderSources(root, recordedPath, revisions)
	if len(recorded) != 2 || !recorded[first] {
		t.Fatalf("could not read the actual recorder inputs: %#v", recorded)
	}
	resolve := newSyncSourceResolver(root, revisions, recorded, lossy)
	if actual := resolve(string(lossy("./" + first))); actual != first {
		t.Fatalf("lost a uniquely recorded name: %q", actual)
	}
	resolve = newSyncSourceResolver(root, revisions, map[string]bool{first: true, second: true}, lossy)
	if actual := resolve(string(lossy("./" + first))); actual != "" {
		t.Fatalf("an ambiguous legacy name selected %q", actual)
	}
	resolve = newSyncSourceResolver(root, revisions, nil, platform.EncodeFileName)
	actual := resolve(string(platform.EncodeFileName("./" + first)))
	// On an ANSI code page which can encode the name, both sources remain
	// distinct. If it cannot, filtering through the recorder resolves it.
	if actual == "" {
		resolve = newSyncSourceResolver(root, revisions, recorded, platform.EncodeFileName)
		actual = resolve(string(platform.EncodeFileName("./" + first)))
	}
	if actual != first {
		t.Fatalf("could not match the system's file name representation: %q", actual)
	}
}

func TestSyncTeXRecorderAlsoMatchesLegacyNames(t *testing.T) {
	_, root := testApp(t)
	name := "追加の 原稿.tex"
	writeTestFile(t, filepath.Join(root, name), []byte("one\n"))
	revisions := snapshotSourceRevisions(root)
	lossy := func(name string) []byte {
		return []byte(strings.Map(func(r rune) rune {
			if r > 127 {
				return '?'
			}
			return r
		}, name))
	}
	data := []byte("INPUT ./main.tex\r\nINPUT " + string(lossy("./"+name)) + "\r\n")
	recorded := resolveRecorderSources(root, data, revisions, lossy)
	if len(recorded) != 2 || !recorded[name] {
		t.Fatalf("the recorder lost a source written in a legacy code page: %#v", recorded)
	}
	resolve := newSyncSourceResolver(root, revisions, recorded, lossy)
	if actual := resolve(string(lossy("./" + name))); actual != name {
		t.Fatalf("a partial recorder list removed a valid source: %q", actual)
	}
}

func TestCancellingWhileCopyingLeavesTheDisplayedPDFAvailable(t *testing.T) {
	a, root := testApp(t)
	source := filepath.Join(t.TempDir(), "result.pdf")
	writeTestFile(t, source, []byte("%PDF-1.7\nlast good PDF\n"))
	currentID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	current, err := a.copySuccessfulPDF(root, source, currentID)
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	run := &buildRun{started: time.Now(), result: Build{ID: currentID, Status: "running"}}
	a.build = run
	a.finishBuild(run, &code, nil, current, currentID)
	for attempt := 0; attempt < 3; attempt++ {
		id, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		copy, err := a.copySuccessfulPDF(root, source, id)
		if err != nil {
			t.Fatal(err)
		}
		run := &buildRun{started: time.Now(), cancelRequested: true, result: Build{ID: id, Status: "running"}}
		a.build = run
		a.finishBuild(run, &code, nil, copy, id)
	}
	response := apiRequest(a, http.MethodGet, "/api/pdf?build="+currentID, nil, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "last good PDF") {
		t.Fatalf("cancelling removed the displayed PDF: %d %s", response.Code, response.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Dir(current))
	if len(entries) != 2 {
		t.Fatalf("cancelled copies remain in the cache: %d files", len(entries))
	}
}

func TestSyncTeXBoxCoordinatesAndRecords(t *testing.T) {
	output := "This is SyncTeX\r\nSyncTeX result begin\r\nOutput:pdf-1.pdf\r\nPage:2\r\nx:90\r\ny:210\r\nh:72\r\nv:216\r\nW:430\r\nH:12\r\nOutput:pdf-1.pdf\r\nPage:2\r\nh:72\r\nv:216\r\nW:430\r\nH:12\r\nOutput:pdf-1.pdf\r\nPage:3\r\nh:90\r\nv:200\r\nW:-10\r\nH:15\r\nSyncTeX result end\r\n"
	locations, err := parseForwardSync(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 2 || locations[0] != (syncLocation{Page: 2, X: 72, Y: 204, Width: 430, Height: 12}) || locations[1] != (syncLocation{Page: 3, X: 80, Y: 185, Width: 10, Height: 15}) {
		t.Fatalf("incorrect rectangles: %#v", locations)
	}
	for _, value := range []string{"NaN", "Inf", "bad"} {
		_, err := parseForwardSync(strings.ReplaceAll(output, "h:72", "h:"+value))
		if err == nil {
			t.Fatalf("accepted invalid coordinate %q", value)
		}
	}
	empty, err := parseForwardSync("This is SyncTeX\n")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("a query without results should return an empty list: %#v %v", empty, err)
	}
	records := syncRecords("SyncTeX result begin\nOutput:pdf-1.pdf\nInput:input-23.tex\nLine:14\nColumn:-1\nSyncTeX result end\n")
	if len(records) != 1 || records[0]["Input"] != "input-23.tex" || records[0]["Line"] != "14" {
		t.Fatalf("could not read inverse result: %#v", records)
	}
}

func TestSyncTeXChecksProjectVersionAndSourcePath(t *testing.T) {
	a, root := testApp(t)
	version := &synctexVersion{ID: "one", Root: root, PDF: filepath.Join(t.TempDir(), "pdf-one.pdf"), Bin: t.TempDir(), Inputs: []synctexInput{{Alias: "input-1.tex", Path: "main.tex"}}}
	a.pdfVersions[version.ID] = version.PDF
	a.synctexVersions[version.ID] = version
	a.pdfVersions["no-sync"] = version.PDF
	for _, test := range []struct {
		name string
		body forwardSyncRequest
		head map[string]string
		want int
	}{
		{"missing version", forwardSyncRequest{Path: "main.tex", Line: 2}, nil, 400},
		{"expired version", forwardSyncRequest{BuildID: "expired", Path: "main.tex", Line: 2}, nil, 410},
		{"missing data", forwardSyncRequest{BuildID: "no-sync", Path: "main.tex", Line: 2}, nil, 404},
		{"other project", forwardSyncRequest{BuildID: "one", Path: "main.tex", Line: 2}, map[string]string{"X-Project-Root": url.PathEscape(t.TempDir())}, 409},
		{"parent path", forwardSyncRequest{BuildID: "one", Path: "../outside.tex", Line: 2}, nil, 400},
		{"absolute path", forwardSyncRequest{BuildID: "one", Path: filepath.Join(root, "main.tex"), Line: 2}, nil, 400},
		{"line zero", forwardSyncRequest{BuildID: "one", Path: "main.tex", Line: 0}, nil, 400},
		{"missing executable", forwardSyncRequest{BuildID: "one", Path: "main.tex", Line: 2}, nil, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := apiRequest(a, http.MethodPost, "/api/synctex/forward", test.body, test.head)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	writeTestFile(t, filepath.Join(root, "unused.tex"), []byte("not included"))
	response := apiRequest(a, http.MethodPost, "/api/synctex/forward", forwardSyncRequest{BuildID: "one", Path: "unused.tex", Line: 1}, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"locations":[]`) {
		t.Fatalf("an unused file should have no locations: %d %s", response.Code, response.Body.String())
	}
	response = apiRequest(a, http.MethodPost, "/api/synctex/inverse", inverseSyncRequest{BuildID: "one", Page: 0}, nil)
	if response.Code != 400 {
		t.Fatalf("an invalid page was accepted: %d", response.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A queued operation must obey the caller's cancellation too.
	a.synctexSlots <- struct{}{}
	a.synctexSlots <- struct{}{}
	_, err := a.runSyncTeX(ctx, version, "view")
	<-a.synctexSlots
	<-a.synctexSlots
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
}

func TestSyncTeXEnvironmentDoesNotLaunchConfiguredEditors(t *testing.T) {
	t.Setenv("SYNCTEX_VIEWER", "unexpected command")
	t.Setenv("SYNCTEX_EDITOR", "unexpected command")
	t.Setenv("SYNCTEX_BUILD_DIRECTORY", "unrelated data")
	for _, entry := range syncEnvironment(t.TempDir()) {
		if strings.HasPrefix(strings.ToUpper(entry), "SYNCTEX_") {
			t.Fatalf("external SyncTeX configuration leaked into the command: %s", entry)
		}
	}
}

func TestPDFAndSyncTeXCacheRestoresTogetherAndExpiresTogether(t *testing.T) {
	a, root := testApp(t)
	output, err := ensureBuildDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	originalPDF := filepath.Join(output, "main.pdf")
	originalSync := filepath.Join(output, "main.synctex")
	var ids, copies []string
	for index := 0; index < 3; index++ {
		writeTestFile(t, originalPDF, []byte(fmt.Sprintf("%%PDF-1.7\nversion %d\n", index)))
		writeTestFile(t, originalSync, []byte("SyncTeX Version:1\nInput:1:./main.tex\nOutput:pdf\nContent:\n!1\nPostamble:\n"))
		id, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		pdf, err := a.copySuccessfulPDF(root, originalPDF, id)
		if err != nil {
			t.Fatal(err)
		}
		sources := snapshotSourceRevisions(root)
		sync, err := a.cacheSyncTeX(root, originalPDF, pdf, id, t.TempDir(), sources)
		if err != nil {
			t.Fatal(err)
		}
		if sync.Inputs[0].Path != "main.tex" {
			t.Fatalf("source was lost: %#v", sync.Inputs)
		}
		run := &buildRun{started: time.Now(), sources: sources, synctex: sync, result: Build{ID: id, Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}}
		a.build = run
		code := 0
		a.finishBuild(run, &code, nil, pdf, id)
		ids, copies = append(ids, id), append(copies, pdf)
	}
	for _, extension := range []string{".pdf", ".synctex.gz", ".json"} {
		if _, err := os.Stat(strings.TrimSuffix(copies[0], ".pdf") + extension); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired file remains: %s (%v)", extension, err)
		}
	}
	writeTestFile(t, originalSync, []byte("overwritten by another compile"))
	reopened, err := New(a.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.SetAddress("127.0.0.1:8765")
	state := apiRequest(reopened, http.MethodGet, "/api/build", nil, nil)
	var build Build
	if err := json.Unmarshal(state.Body.Bytes(), &build); err != nil {
		t.Fatal(err)
	}
	if !build.Restored || !build.PDFReady || !build.SyncTeXReady || build.SourceChanged || build.PDFBuildID != ids[2] {
		t.Fatalf("did not restore the last successful version: %#v", build)
	}
	if len(reopened.pdfVersions) != 2 || reopened.synctexVersions[ids[1]] == nil {
		t.Fatal("the preceding PDF and SyncTeX version did not reopen")
	}
	read, err := os.Open(strings.TrimSuffix(reopened.lastPDF, ".pdf") + ".synctex.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	compressed, err := gzip.NewReader(read)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(compressed)
	compressed.Close()
	if err != nil || !strings.Contains(string(data), "Input:1:input-1.tex") {
		t.Fatalf("cached SyncTeX changed with the original: %q %v", data, err)
	}
	writeTestFile(t, filepath.Join(root, "main.tex"), []byte("changed after compilation"))
	response := apiRequest(reopened, http.MethodPost, "/api/project/open", map[string]string{"path": root}, nil)
	var reopenedState State
	if err := json.Unmarshal(response.Body.Bytes(), &reopenedState); err != nil {
		t.Fatal(err)
	}
	if reopenedState.Build == nil || !reopenedState.Build.SourceChanged {
		t.Fatal("a changed source was not reported when restoring its PDF")
	}
}

func TestSyncTeXWithRealTeXAndUnicodePaths(t *testing.T) {
	latex, err := exec.LookPath("pdftex")
	if err != nil {
		t.Skip("pdftex is not installed; Windows browser tests run after TeX Live setup")
	}
	synctex, err := exec.LookPath("synctex")
	if err != nil {
		t.Skip("synctex is not installed")
	}
	a, root := testApp(t)
	// Primitive boxes exercise the real engine and CLI without requiring a
	// prebuilt LaTeX format or system fonts in the Go test environment.
	main := "\\catcode123=1 \\catcode125=2\n\\pdfoutput=1\n\\pdfpagewidth=200pt\n\\pdfpageheight=200pt\n\\setbox0=\\vbox{\\hbox{\\vrule width120pt height10pt}}\n\\shipout\\box0\n\\input \"追加の 原稿.tex\"\n\\end\n"
	include := "% included source\n\\setbox0=\\vbox{\\hbox{\\vrule width120pt height10pt}}\n\\shipout\\box0\n"
	writeTestFile(t, filepath.Join(root, "main.tex"), []byte(main))
	writeTestFile(t, filepath.Join(root, "追加の 原稿.tex"), []byte(include))
	sources := snapshotSourceRevisions(root)
	outputDir, err := ensureBuildDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, latex, "-ini", "-interaction=nonstopmode", "-halt-on-error", "-synctex=1", "-output-directory="+buildRelative, "./main.tex")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("could not create the SyncTeX fixture: %v\n%s", err, output)
	}
	id, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	originalPDF := filepath.Join(outputDir, "main.pdf")
	pdf, err := a.copySuccessfulPDF(root, originalPDF, id)
	if err != nil {
		t.Fatal(err)
	}
	version, err := a.cacheSyncTeX(root, originalPDF, pdf, id, filepath.Dir(synctex), sources)
	if err != nil {
		t.Fatal(err)
	}
	run := &buildRun{started: time.Now(), sources: sources, synctex: version, result: Build{ID: id, Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}}
	a.build = run
	code := 0
	a.finishBuild(run, &code, nil, pdf, id)
	// The successful cached version must still work after the mutable build
	// files have been replaced by the next compiler invocation.
	writeTestFile(t, filepath.Join(outputDir, "main.synctex.gz"), []byte("new unfinished data"))
	forward := apiRequest(a, http.MethodPost, "/api/synctex/forward", forwardSyncRequest{BuildID: id, Path: "追加の 原稿.tex", Line: 2, Column: 1}, nil)
	if forward.Code != 200 {
		t.Fatalf("forward synchronization failed: %d %s", forward.Code, forward.Body.String())
	}
	var found forwardSyncResponse
	if err := json.Unmarshal(forward.Body.Bytes(), &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Locations) == 0 || found.Locations[0].Page != 2 || found.SourceChanged {
		t.Fatalf("unexpected forward result: %#v", found)
	}
	location := found.Locations[0]
	inverse := apiRequest(a, http.MethodPost, "/api/synctex/inverse", inverseSyncRequest{BuildID: id, Page: location.Page, X: location.X + 10, Y: location.Y + location.Height/2}, nil)
	if inverse.Code != 200 {
		t.Fatalf("inverse synchronization failed: %d %s", inverse.Code, inverse.Body.String())
	}
	var returned inverseSyncResponse
	if err := json.Unmarshal(inverse.Body.Bytes(), &returned); err != nil {
		t.Fatal(err)
	}
	if returned.Path != "追加の 原稿.tex" || returned.Line != 2 || returned.SourceChanged {
		t.Fatalf("unexpected inverse result: %#v", returned)
	}
	writeTestFile(t, filepath.Join(root, "追加の 原稿.tex"), []byte("% added a line\n"+include))
	forward = apiRequest(a, http.MethodPost, "/api/synctex/forward", forwardSyncRequest{BuildID: id, Path: "追加の 原稿.tex", Line: 2, Column: 1}, nil)
	if forward.Code != 200 || json.Unmarshal(forward.Body.Bytes(), &found) != nil || !found.SourceChanged {
		t.Fatalf("source modification was not reported: %s", forward.Body.String())
	}
}
