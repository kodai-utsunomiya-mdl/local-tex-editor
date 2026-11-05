package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

const maxSyncTeXSize = 128 << 20
const syncTeXTimeout = 6 * time.Second

// Each version refers to the PDF and SyncTeX data from one successful compile.
// Input aliases keep Windows' command-line code page out of source file names.
type synctexVersion struct {
	ID                 string         `json:"id"`
	Root               string         `json:"-"`
	PDF                string         `json:"-"`
	Bin                string         `json:"bin"`
	Inputs             []synctexInput `json:"inputs"`
	ChangedDuringBuild bool           `json:"changedDuringBuild"`
}

type synctexInput struct {
	Alias    string `json:"alias"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
}

type syncLocation struct {
	Page   int     `json:"page"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type forwardSyncRequest struct {
	BuildID string `json:"buildId"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
}

type forwardSyncResponse struct {
	BuildID       string         `json:"buildId"`
	Locations     []syncLocation `json:"locations"`
	SourceChanged bool           `json:"sourceChanged"`
}

type inverseSyncRequest struct {
	BuildID string  `json:"buildId"`
	Page    int     `json:"page"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
}

type inverseSyncResponse struct {
	BuildID       string `json:"buildId"`
	Path          string `json:"path"`
	Line          int    `json:"line"`
	Column        int    `json:"column"`
	SourceChanged bool   `json:"sourceChanged"`
}

func (a *App) forwardSyncHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input forwardSyncRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	if input.Line < 1 || input.Line > 10_000_000 || input.Column < 0 || input.Column > 10_000_000 {
		writeError(w, fail(http.StatusBadRequest, "原稿の行と列を指定してください．"))
		return
	}
	version, err := a.syncVersion(r.Header.Get("X-Project-Root"), input.BuildID)
	if err != nil {
		writeError(w, err)
		return
	}
	clean, _, err := projectFilePath(version.Root, input.Path, false)
	if err != nil {
		writeError(w, err)
		return
	}
	response := forwardSyncResponse{BuildID: version.ID, Locations: []syncLocation{}}
	var source *synctexInput
	for index := range version.Inputs {
		if samePath(version.Inputs[index].Path, clean) {
			source = &version.Inputs[index]
			break
		}
	}
	if source == nil {
		// Files without typeset material, such as a bibliography database, do
		// not necessarily have an Input record in this PDF's SyncTeX data.
		writeJSON(w, http.StatusOK, response)
		return
	}
	argument := fmt.Sprintf("%d:%d:%s", input.Line, input.Column, source.Alias)
	output, err := a.runSyncTeX(r.Context(), version, "view", "-i", argument, "-o", filepath.Base(version.PDF))
	if err == nil {
		response.Locations, err = parseForwardSync(output)
	}
	if err != nil {
		a.logSyncError(version, err)
		writeError(w, err)
		return
	}
	if err := a.checkSyncProject(version.Root); err != nil {
		writeError(w, err)
		return
	}
	response.SourceChanged = syncSourceChanged(version, *source)
	writeJSON(w, http.StatusOK, response)
}

func (a *App) inverseSyncHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input inverseSyncRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	if input.Page < 1 || input.Page > 100_000 || !finiteCoordinate(input.X) || !finiteCoordinate(input.Y) || input.X < 0 || input.Y < 0 {
		writeError(w, fail(http.StatusBadRequest, "PDF のページと位置を指定してください．"))
		return
	}
	version, err := a.syncVersion(r.Header.Get("X-Project-Root"), input.BuildID)
	if err != nil {
		writeError(w, err)
		return
	}
	argument := fmt.Sprintf("%d:%s:%s:%s", input.Page, strconv.FormatFloat(input.X, 'f', 6, 64), strconv.FormatFloat(input.Y, 'f', 6, 64), filepath.Base(version.PDF))
	output, err := a.runSyncTeX(r.Context(), version, "edit", "-o", argument)
	if err != nil {
		a.logSyncError(version, err)
		writeError(w, err)
		return
	}
	if err := a.checkSyncProject(version.Root); err != nil {
		writeError(w, err)
		return
	}
	for _, record := range syncRecords(output) {
		line, err := strconv.Atoi(record["Line"])
		if err != nil || line < 1 || line > 10_000_000 {
			continue
		}
		column, _ := strconv.Atoi(record["Column"])
		if column < 0 || column > 10_000_000 {
			column = 0
		}
		for _, source := range version.Inputs {
			if source.Alias != record["Input"] || source.Path == "" {
				continue
			}
			clean, _, err := projectFilePath(version.Root, source.Path, false)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, inverseSyncResponse{BuildID: version.ID, Path: clean, Line: line, Column: column, SourceChanged: syncSourceChanged(version, source)})
			return
		}
	}
	writeError(w, fail(http.StatusNotFound, "この位置に対応するプロジェクト内の原稿が見つかりません．文章の近くをダブルクリックしてください．"))
}

func (a *App) syncVersion(expectedProject, id string) (*synctexVersion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.checkProjectLocked(expectedProject); err != nil {
		return nil, err
	}
	if a.project == nil {
		return nil, fail(http.StatusBadRequest, "原稿フォルダーを開いてください．")
	}
	if id == "" {
		return nil, fail(http.StatusBadRequest, "表示中の PDF の版を指定してください．")
	}
	if _, exists := a.pdfVersions[id]; !exists {
		return nil, fail(http.StatusGone, "この PDF の版はキャッシュにありません．最新の PDF を表示してください．")
	}
	version := a.synctexVersions[id]
	if version == nil {
		return nil, fail(http.StatusNotFound, "この PDF の SyncTeX データがありません．コンパイルし直し，ログの SyncTeX の項目を確認してください．")
	}
	return version, nil
}

func (a *App) checkSyncProject(root string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project == nil || !samePath(a.project.Root, root) {
		return fail(http.StatusConflict, "開いているプロジェクトが変わりました．現在の原稿で操作してください．")
	}
	return nil
}

func (a *App) logSyncError(version *synctexVersion, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project != nil && samePath(a.project.Root, version.Root) && a.build != nil {
		a.appendLogLocked(a.build, []byte("\n[SyncTeX] "+err.Error()+"\n"))
	}
}

func syncSourceChanged(version *synctexVersion, source synctexInput) bool {
	if version.ChangedDuringBuild || source.Revision == "" {
		return true
	}
	_, path, err := projectFilePath(version.Root, source.Path, false)
	if err != nil {
		return true
	}
	data, err := readBytes(path)
	return err != nil || revision(data) != source.Revision
}

func snapshotSourceRevisions(root string) map[string]string {
	result := make(map[string]string)
	files, err := listProjectFiles(root)
	if err != nil {
		return result
	}
	remaining := int64(128 << 20)
	for _, file := range files {
		if file.Size > maxTextSize || file.Size > remaining {
			continue
		}
		_, path, err := projectFilePath(root, file.Path, false)
		if err != nil {
			continue
		}
		data, err := readBytes(path)
		if err != nil {
			continue
		}
		remaining -= int64(len(data))
		result[file.Path] = revision(data)
	}
	return result
}

func (a *App) cacheSyncTeX(root, originalPDF, cachedPDF, id, bin string, revisions map[string]string) (*synctexVersion, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	stem := strings.TrimSuffix(originalPDF, filepath.Ext(originalPDF))
	var source string
	var newest time.Time
	for _, extension := range []string{".synctex.gz", ".synctex"} {
		candidate := stem + extension
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		relative, err := filepath.Rel(root, candidate)
		if err != nil {
			continue
		}
		if _, err := safePath(root, filepath.ToSlash(relative), false); err != nil {
			continue
		}
		if source == "" || info.ModTime().After(newest) {
			source, newest = candidate, info.ModTime()
		}
	}
	if source == "" {
		return nil, errors.New("生成された .synctex.gz / .synctex が見つかりません．latexmkrc の設定でも SyncTeX を有効にしてください．")
	}
	in, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	buffered := bufio.NewReader(in)
	var reader io.Reader = buffered
	if magic, _ := buffered.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		compressed, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, fmt.Errorf("SyncTeX の圧縮データを読めません: %w", err)
		}
		defer compressed.Close()
		reader = compressed
	}
	out, err := os.CreateTemp(filepath.Dir(cachedPDF), ".synctex-*")
	if err != nil {
		return nil, err
	}
	temp := out.Name()
	defer os.Remove(temp)
	compressed := gzip.NewWriter(out)
	inputs, copyErr := normalizeSyncTeX(reader, compressed, root, revisions, recorderSources(root, stem+".fls"))
	if err := compressed.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr == nil {
		copyErr = out.Sync()
	}
	if err := out.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return nil, copyErr
	}
	destination := strings.TrimSuffix(cachedPDF, ".pdf") + ".synctex.gz"
	if err := platform.ReplaceFile(temp, destination); err != nil {
		return nil, err
	}
	return &synctexVersion{ID: id, Root: root, PDF: cachedPDF, Bin: bin, Inputs: inputs}, nil
}

// SyncTeX uses the Input tag in its position records. The CLI only needs a
// matching file name for that tag; it does not open the TeX source. Substituting
// ASCII names makes the same data usable on Windows with any system code page.
// Byte counts on ! lines are updated as described in synctex_record_anchor.
func normalizeSyncTeX(in io.Reader, out io.Writer, root string, revisions map[string]string, recorded map[string]bool) ([]synctexInput, error) {
	limited := &io.LimitedReader{R: in, N: maxSyncTeXSize + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	inputs := []synctexInput{}
	resolve := newSyncSourceResolver(root, revisions, recorded, platform.EncodeFileName)
	seen := make(map[int]string)
	read, sinceAnchor := 0, 0
	header, content, postamble := false, false, false
	for scanner.Scan() {
		line := scanner.Text()
		read += len(line) + 1
		if read > maxSyncTeXSize {
			return nil, errors.New("SyncTeX データが 128 MiB を超えています．")
		}
		if strings.HasPrefix(line, "SyncTeX Version:") {
			header = true
		}
		if line == "Content:" {
			content = true
		}
		if line == "Postamble:" {
			postamble = true
		}
		if strings.HasPrefix(line, "Input:") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 {
				return nil, errors.New("SyncTeX の Input 記録を読めません．")
			}
			tag, err := strconv.Atoi(parts[1])
			if err != nil || tag < 1 || len(inputs) >= 50_000 {
				return nil, errors.New("SyncTeX の Input 番号を読めません．")
			}
			alias := fmt.Sprintf("input-%d.tex", tag)
			rawPath := platform.DecodeOutput([]byte(parts[2]))
			if previous, exists := seen[tag]; exists {
				if previous != rawPath {
					return nil, errors.New("SyncTeX の Input 番号が重複しています．")
				}
			} else {
				seen[tag] = rawPath
				path := resolve(parts[2])
				inputs = append(inputs, synctexInput{Alias: alias, Path: path, Revision: revisions[path]})
			}
			line = "Input:" + strconv.Itoa(tag) + ":" + alias
		} else if strings.HasPrefix(line, "!") {
			if _, err := strconv.Atoi(line[1:]); err != nil {
				return nil, errors.New("SyncTeX の位置記録を読めません．")
			}
			line = "!" + strconv.Itoa(sinceAnchor)
			sinceAnchor = 0
		}
		n, err := io.WriteString(out, line+"\n")
		if err != nil {
			return nil, err
		}
		sinceAnchor += n
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SyncTeX データを読めません: %w", err)
	}
	if limited.N == 0 {
		return nil, errors.New("SyncTeX データが 128 MiB を超えています．")
	}
	if !header || !content || !postamble || len(inputs) == 0 {
		return nil, errors.New("SyncTeX の原稿情報がありません．コンパイルし直してください．")
	}
	return inputs, nil
}

func recorderSources(root, path string) map[string]bool {
	data, err := readBytes(path)
	if err != nil {
		return nil
	}
	result := make(map[string]bool)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("INPUT ")) {
			continue
		}
		name := platform.DecodeOutput(bytes.TrimSuffix(line[6:], []byte("\r")))
		if path, err := syncProjectPath(root, name); err == nil {
			result[path] = true
		}
	}
	return result
}

// The recorder narrows the choices to sources that TeX actually opened. A
// Unicode file name may become question marks when the SyncTeX writer converts
// it to Windows' ANSI code page. Only a unique match is used for navigation.
func newSyncSourceResolver(root string, revisions map[string]string, recorded map[string]bool, encode func(string) []byte) func(string) string {
	paths := make(map[string]bool)
	for path := range revisions {
		if len(recorded) == 0 || recorded[path] {
			paths[path] = true
		}
	}
	for path := range recorded {
		paths[path] = true
	}
	spellings := make(map[string]string)
	for path := range paths {
		native := filepath.FromSlash(path)
		absolute := filepath.Join(root, native)
		for _, name := range []string{path, "./" + path, native, "." + string(filepath.Separator) + native, absolute, filepath.ToSlash(absolute)} {
			for _, spelling := range []string{name, string(encode(name))} {
				key := syncNameKey(spelling)
				if previous, exists := spellings[key]; exists && previous != path {
					spellings[key] = ""
				} else if !exists {
					spellings[key] = path
				}
			}
		}
	}
	return func(raw string) string {
		if path, known := spellings[syncNameKey(raw)]; known {
			return path
		}
		path, _ := syncProjectPath(root, platform.DecodeOutput([]byte(raw)))
		return path
	}
}

func syncNameKey(raw string) string {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, `"`), `"`)
	if runtime.GOOS != "windows" {
		return raw
	}
	// A trail byte of a Windows multibyte character can be an ASCII letter.
	// Case folding is therefore limited to entirely ASCII spellings.
	data := []byte(raw)
	for _, value := range data {
		if value >= 0x80 {
			return raw
		}
	}
	for index, value := range data {
		if value >= 'A' && value <= 'Z' {
			data[index] += 'a' - 'A'
		}
	}
	return string(data)
}

func syncProjectPath(root, raw string) (string, error) {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, `"`), `"`)
	path := filepath.FromSlash(strings.ReplaceAll(raw, `\`, "/"))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	// EvalSymlinks also expands Windows' short directory names. The returned
	// name is checked again against the selected project and its safe paths.
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, canonical)
	if err != nil {
		return "", err
	}
	clean, _, err := projectFilePath(root, filepath.ToSlash(relative), false)
	return clean, err
}

func findSynctex(bin string) (string, error) {
	name := "synctex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(bin, name)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return filepath.Abs(path)
	}
	return "", fail(http.StatusServiceUnavailable, "synctex が見つかりません (%s)．TeX Live で synctex パッケージを利用できる状態にしてください．", path)
}

type syncOutput struct {
	data []byte
	full bool
}

func (output *syncOutput) Write(data []byte) (int, error) {
	remaining := (2 << 20) - len(output.data)
	if len(data) > remaining {
		output.full = true
	}
	output.data = append(output.data, data[:minimum(len(data), remaining)]...)
	return len(data), nil
}

func syncEnvironment(bin string) []string {
	environment := commandEnvironment(bin)
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "SYNCTEX_VIEWER") || strings.EqualFold(key, "SYNCTEX_EDITOR") || strings.EqualFold(key, "SYNCTEX_BUILD_DIRECTORY") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func (a *App) runSyncTeX(parent context.Context, version *synctexVersion, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, syncTeXTimeout)
	defer cancel()
	select {
	case a.synctexSlots <- struct{}{}:
		defer func() { <-a.synctexSlots }()
	case <-ctx.Done():
		return "", syncContextError(ctx)
	}
	tool, err := findSynctex(version.Bin)
	if err != nil {
		return "", err
	}
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	for _, path := range []string{version.PDF, strings.TrimSuffix(version.PDF, ".pdf") + ".synctex.gz"} {
		if _, err := os.Stat(path); err != nil {
			return "", fail(http.StatusGone, "この PDF の位置データはキャッシュにありません．最新の PDF を表示してください．")
		}
	}
	command := exec.CommandContext(ctx, tool, args...)
	command.Dir = filepath.Dir(version.PDF)
	command.Env = syncEnvironment(version.Bin)
	platform.PrepareCommand(command)
	command.Cancel = func() error { return platform.StopProcess(command.Process) }
	command.WaitDelay = time.Second
	var output, stderr syncOutput
	command.Stdout, command.Stderr = &output, &stderr
	err = command.Run()
	if ctx.Err() != nil {
		return "", syncContextError(ctx)
	}
	if output.full || stderr.full {
		return "", fail(http.StatusBadGateway, "SyncTeX が返した情報が大きすぎます．別の位置でもう一度操作してください．")
	}
	text := readableLog(output.data, true)
	errorText := strings.TrimSpace(readableLog(stderr.data, true))
	if err != nil || strings.Contains(strings.ToLower(errorText), "error") {
		if errorText == "" {
			errorText = strings.TrimSpace(text)
		}
		if len(errorText) > 2000 {
			errorText = errorText[:2000]
		}
		return "", fail(http.StatusBadGateway, "SyncTeX を実行できません: %v\n%s", err, errorText)
	}
	return text, nil
}

func syncContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	return fail(http.StatusGatewayTimeout, "SyncTeX の応答を 6 秒待ちました．別の位置でもう一度操作してください．")
}

func syncRecords(output string) []map[string]string {
	result := []map[string]string{}
	var current map[string]string
	active := false
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if line == "SyncTeX result begin" {
			active = true
			continue
		}
		if line == "SyncTeX result end" {
			break
		}
		if !active {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if key == "Output" {
			if len(result) >= 256 {
				break
			}
			current = make(map[string]string)
			result = append(result, current)
		}
		if current != nil {
			current[key] = value
		}
	}
	return result
}

func finiteCoordinate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 10_000_000
}

func parseForwardSync(output string) ([]syncLocation, error) {
	locations := []syncLocation{}
	seen := make(map[syncLocation]bool)
	for _, record := range syncRecords(output) {
		page, err := strconv.Atoi(record["Page"])
		if err != nil || page < 1 || page > 100_000 {
			return nil, errors.New("SyncTeX が返したページ番号を読めません．")
		}
		var values [4]float64
		for index, key := range []string{"h", "v", "W", "H"} {
			value, err := strconv.ParseFloat(record[key], 64)
			if err != nil || !finiteCoordinate(value) {
				return nil, errors.New("SyncTeX が返した PDF の位置を読めません．")
			}
			values[index] = value
		}
		// The CLI reports v at the lower edge of the enclosing box, and H
		// extends upward. Browser rectangles use the upper-left corner.
		h, v, width, height := values[0], values[1], values[2], values[3]
		location := syncLocation{Page: page, X: math.Min(h, h+width), Y: math.Min(v, v-height), Width: math.Abs(width), Height: math.Abs(height)}
		if !seen[location] {
			seen[location] = true
			locations = append(locations, location)
		}
	}
	return locations, nil
}
