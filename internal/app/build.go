package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

const maxLogBytes = 2 << 20
const buildRelative = ".local-tex-editor/build"

type Build struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Command       string `json:"command"`
	Log           string `json:"log"`
	ExitCode      *int   `json:"exitCode"`
	Error         string `json:"error"`
	StartedAt     string `json:"startedAt"`
	FinishedAt    string `json:"finishedAt"`
	PDFReady      bool   `json:"pdfReady"`
	PDFBuildID    string `json:"pdfBuildId"`
	SyncTeXReady  bool   `json:"synctexReady"`
	ElapsedMS     int64  `json:"elapsedMs"`
	SourceChanged bool   `json:"sourceChanged"`
	Restored      bool   `json:"restored"`
}

type buildRun struct {
	result          Build
	started         time.Time
	finished        time.Time
	process         *os.Process
	cancelRequested bool
	log             []byte
	truncated       bool
	synctex         *synctexVersion
	sources         map[string]string
}

type commandOutput struct {
	app *App
	run *buildRun
}

func (output *commandOutput) Write(data []byte) (int, error) {
	output.app.mu.Lock()
	output.app.appendLogLocked(output.run, data)
	output.app.mu.Unlock()
	return len(data), nil
}

func (a *App) appendLogLocked(run *buildRun, data []byte) {
	run.log = append(run.log, data...)
	if len(run.log) > maxLogBytes {
		run.log = append([]byte(nil), run.log[len(run.log)-maxLogBytes:]...)
		run.truncated = true
	}
}

func readableLog(data []byte, complete bool) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var text strings.Builder
	lines := bytes.SplitAfter(data, []byte("\n"))
	for index, line := range lines {
		if utf8.Valid(line) {
			text.Write(line)
			continue
		}
		if !complete && index == len(lines)-1 {
			// A pipe read may end in the middle of one UTF-8 character.
			foundPrefix := false
			for removed := 1; removed < 4 && removed <= len(line); removed++ {
				prefix := line[:len(line)-removed]
				if utf8.Valid(prefix) {
					text.Write(prefix)
					foundPrefix = true
					break
				}
			}
			if foundPrefix {
				continue
			}
		}
		// An external Windows tool can use its local encoding even when TeX
		// and this application's messages use UTF-8 on adjacent lines.
		text.WriteString(platform.DecodeOutput(line))
	}
	return text.String()
}

func (a *App) buildSnapshotLocked() *Build {
	if a.build == nil {
		return nil
	}
	run := a.build
	result := run.result
	result.Log = readableLog(run.log, result.Status != "running")
	if run.truncated {
		result.Log = "[ログが長いため，末尾の 2 MiB を表示しています．]\n" + result.Log
	}
	result.PDFReady = a.lastPDF != ""
	result.PDFBuildID = a.lastPDFID
	result.SyncTeXReady = a.synctexVersions[a.lastPDFID] != nil
	end := run.finished
	if end.IsZero() {
		end = time.Now()
	}
	result.ElapsedMS = end.Sub(run.started).Milliseconds()
	return &result
}

func (a *App) isRunningLocked() bool { return a.build != nil && a.build.result.Status == "running" }

func randomID() (string, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405.000000000") + "-" + hex.EncodeToString(data[:]), nil
}

func (a *App) startBuild(expectedProject string) (*Build, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.checkProjectLocked(expectedProject); err != nil {
		return nil, err
	}
	if a.closed {
		return nil, fail(http.StatusServiceUnavailable, "アプリは終了しています．")
	}
	if a.isRunningLocked() {
		return nil, fail(http.StatusConflict, "コンパイルが実行中です．完了を待つか停止してください．")
	}
	if a.project == nil {
		return nil, fail(http.StatusBadRequest, "原稿フォルダーを開いてください．")
	}
	if a.project.Main == "" {
		return nil, fail(http.StatusBadRequest, "メインの .tex ファイルを選んでください．")
	}
	if _, _, err := projectFilePath(a.project.Root, a.project.Main, false); err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	run := &buildRun{started: now, result: Build{ID: id, Status: "running", StartedAt: now.UTC().Format(time.RFC3339Nano)}}
	a.build = run
	project, texBin := *a.project, a.config.TexBin
	a.workers.Add(1)
	go a.compile(run, project, texBin)
	return a.buildSnapshotLocked(), nil
}

func (a *App) finishBuild(run *buildRun, code *int, err error, pdf, pdfID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	run.finished = time.Now()
	run.result.FinishedAt = run.finished.UTC().Format(time.RFC3339Nano)
	run.result.ExitCode = code
	run.process = nil
	if run.cancelRequested {
		run.result.Status = "cancelled"
		run.result.Error = "コンパイルを停止しました．"
	} else if err != nil {
		run.result.Status = "failed"
		run.result.Error = err.Error()
	} else {
		run.result.Status = "success"
		run.result.Error = ""
		a.lastPDF, a.lastPDFID = pdf, pdfID
		a.pdfVersions[pdfID] = pdf
		if run.synctex != nil {
			run.synctex.ChangedDuringBuild = run.result.SourceChanged
			a.synctexVersions[pdfID] = run.synctex
		}
		a.pdfOrder = append(a.pdfOrder, pdfID)
		for len(a.pdfOrder) > 2 {
			delete(a.pdfVersions, a.pdfOrder[0])
			delete(a.synctexVersions, a.pdfOrder[0])
			a.pdfOrder = a.pdfOrder[1:]
		}
		if a.project != nil {
			if err := a.saveCachedBuild(*a.project, run, pdf); err != nil {
				a.appendLogLocked(run, []byte("\n[前回の PDF] 次の起動時に読み込む情報を保存できません: "+err.Error()+"\n"))
			}
		}
	}
	if run.result.Error != "" {
		a.appendLogLocked(run, []byte("\n[Local TeX Editor] "+run.result.Error+"\n"))
	}
	if pdf != "" {
		a.trimPDFCacheLocked(filepath.Dir(pdf))
	}
	if code != nil {
		a.appendLogLocked(run, []byte(fmt.Sprintf("\n[終了コード: %d]\n", *code)))
	}
}

func (a *App) compile(run *buildRun, project Project, texBin string) {
	defer a.workers.Done()
	tool, bin, err := findLatexmk(texBin)
	if err != nil {
		a.finishBuild(run, nil, err, "", "")
		return
	}
	outputDirectory, err := ensureBuildDirectory(project.Root)
	if err != nil {
		a.finishBuild(run, nil, err, "", "")
		return
	}
	release, err := platform.AcquireProjectLock(project.Root)
	if err != nil {
		a.finishBuild(run, nil, fmt.Errorf("コンパイルを開始できません: %w", err), "", "")
		return
	}
	defer release()
	sources := snapshotSourceRevisions(project.Root)
	run.sources = sources
	args := latexmkArguments(project)
	cmd := exec.Command(tool, args...)
	cmd.Dir = project.Root
	cmd.Env = commandEnvironment(bin)
	platform.PrepareCommand(cmd)
	writer := &commandOutput{app: a, run: run}
	cmd.Stdout, cmd.Stderr = writer, writer
	a.mu.Lock()
	run.result.Command = displayCommand(tool, args)
	a.appendLogLocked(run, []byte("[作業フォルダー] "+project.Root+"\n[実行] "+run.result.Command+"\n\n"))
	cancelled := run.cancelRequested || a.closed
	a.mu.Unlock()
	if cancelled {
		a.finishBuild(run, nil, errors.New("コンパイルを停止しました．"), "", "")
		return
	}
	if err := cmd.Start(); err != nil {
		a.finishBuild(run, nil, fmt.Errorf("latexmk を起動できません: %w", err), "", "")
		return
	}
	a.mu.Lock()
	run.process = cmd.Process
	cancelled = run.cancelRequested || a.closed
	a.mu.Unlock()
	if cancelled {
		_ = platform.StopProcess(cmd.Process)
	}
	waitErr := cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	a.mu.Lock()
	cancelled = run.cancelRequested
	a.mu.Unlock()
	if cancelled || waitErr != nil {
		if !cancelled {
			waitErr = fmt.Errorf("latexmk が終了コード %d で終了しました．上のログを確認してください．", code)
		}
		a.finishBuild(run, &code, waitErr, "", "")
		return
	}
	pdfSource, err := findOutputPDF(project.Root, project.Main, outputDirectory)
	if err != nil {
		a.finishBuild(run, &code, err, "", "")
		return
	}
	pdf, err := a.copySuccessfulPDF(project.Root, pdfSource, run.result.ID)
	if err != nil {
		a.finishBuild(run, &code, fmt.Errorf("PDF を表示用にコピーできません: %w", err), "", "")
		return
	}
	_, _ = writer.Write([]byte("\n[PDF] " + pdfSource + "\n"))
	run.synctex, err = a.cacheSyncTeX(project.Root, pdfSource, pdf, run.result.ID, bin, sources)
	if err != nil {
		_, _ = writer.Write([]byte("[SyncTeX] 原稿と PDF の位置を照合できません: " + err.Error() + "\n"))
	} else {
		_, _ = writer.Write([]byte("[SyncTeX] この PDF に対応するデータを保存しました．\n"))
	}
	a.finishBuild(run, &code, nil, pdf, run.result.ID)
}

func (a *App) stopBuild(expectedProject ...string) error {
	a.mu.Lock()
	if len(expectedProject) > 0 {
		if err := a.checkProjectLocked(expectedProject[0]); err != nil {
			a.mu.Unlock()
			return err
		}
	}
	if !a.isRunningLocked() {
		a.mu.Unlock()
		return nil
	}
	run := a.build
	run.cancelRequested = true
	process := run.process
	a.mu.Unlock()
	if process != nil {
		if err := platform.StopProcess(process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			a.mu.Lock()
			a.appendLogLocked(run, []byte("\n[停止要求] "+err.Error()+"\n"))
			a.mu.Unlock()
			return fmt.Errorf("停止処理を完了できません: %w", err)
		}
	}
	return nil
}

func latexmkArguments(project Project) []string {
	args := []string{"-interaction=nonstopmode", "-file-line-error", "-halt-on-error", "-synctex=1", "-recorder",
		"-outdir=" + buildRelative, "-auxdir=" + buildRelative}
	if runtime.GOOS == "windows" {
		// Keep BibTeX in the source directory. upBibTeX resolves absolute
		// BIBINPUTS paths through Windows' legacy code page, which can lose
		// characters in the project's parent directories. The auxiliary
		// files are still addressed relative to the source directory.
		args = append(args, "-bibtexfudge-")
	}
	switch project.Mode {
	case "pdflatex":
		args = append(args, "-pdf")
	case "xelatex":
		args = append(args, "-xelatex")
	case "lualatex":
		args = append(args, "-lualatex")
	case "uplatex":
		args = append(args, "-pdfdvi", "-e", `$latex='uplatex %O %S'; $bibtex='upbibtex %O %B'; $dvipdf='dvipdfmx %O -o %D %S';`)
	}
	return append(args, "./"+project.Main)
}

func commandEnvironment(bin string) []string {
	environment := os.Environ()
	if bin == "" {
		return environment
	}
	for index, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(key, "PATH") {
			environment[index] = key + "=" + bin + string(os.PathListSeparator) + value
			return environment
		}
	}
	return append(environment, "PATH="+bin)
}

func displayCommand(executable string, arguments []string) string {
	words := append([]string{executable}, arguments...)
	for index, word := range words {
		if strings.ContainsAny(word, " \t\r\n\"") {
			words[index] = strconv.Quote(word)
		}
	}
	return strings.Join(words, " ")
}

func findLatexmk(texBin string) (string, string, error) {
	name := "latexmk"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if texBin != "" {
		path := filepath.Join(texBin, name)
		if info, err := os.Stat(path); err == nil {
			if regular, err := platform.IsRegularFile(path, info); err == nil && regular {
				absolute, err := filepath.Abs(path)
				return absolute, filepath.Dir(absolute), err
			}
		}
		return "", texBin, fmt.Errorf("%s が見つかりません．設定で latexmk が入っている TeX Live のフォルダーを指定してください．", path)
	}
	if path, err := exec.LookPath(name); err == nil {
		absolute, err := filepath.Abs(path)
		return absolute, filepath.Dir(absolute), err
	}
	if runtime.GOOS == "windows" {
		drive := os.Getenv("SystemDrive")
		if drive == "" {
			drive = "C:"
		}
		candidates, _ := filepath.Glob(filepath.Join(drive+`\`, "texlive", "*", "bin", "windows", name))
		sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil {
				if regular, err := platform.IsRegularFile(candidate, info); err == nil && regular {
					return candidate, filepath.Dir(candidate), nil
				}
			}
		}
	}
	return "", "", errors.New("latexmk が見つかりません．設定で TeX Live の bin フォルダーを指定してください．")
}

func ensureBuildDirectory(root string) (string, error) {
	path := root
	for _, part := range strings.Split(buildRelative, "/") {
		path = filepath.Join(path, part)
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("コンパイル用のフォルダーを作れません: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		linked, err := platform.IsReparsePoint(path)
		if err != nil {
			return "", err
		}
		if linked || !info.IsDir() {
			return "", fmt.Errorf("%s が通常のフォルダーではありません．", path)
		}
	}
	return path, nil
}

var quotedFDBValue = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

type fdbRecord struct {
	modified time.Time
	outputs  []string
}

func fdbValues(line string) []string {
	items := quotedFDBValue.FindAllString(line, -1)
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, err := strconv.Unquote(item)
		if err != nil {
			value = strings.Trim(item, `"`)
		}
		values = append(values, value)
	}
	return values
}

func absoluteFrom(root, path string) string {
	path = filepath.FromSlash(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

// latexmk records a separate rule for DVI-to-PDF, including a custom job name.
func findOutputPDF(root, main, directory string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	mainPath := absoluteFrom(root, main)
	records := []fdbRecord{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".fdb_latexmk") {
			continue
		}
		path, err := safePath(root, buildRelative+"/"+entry.Name(), false)
		if err != nil {
			continue
		}
		data, err := readBytes(path)
		if err != nil {
			continue
		}
		matchesMain := false
		outputs := []string{}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "[\"") {
				continue
			}
			values := fdbValues(line)
			if len(values) < 3 {
				continue
			}
			if strings.EqualFold(filepath.Ext(values[1]), ".tex") && samePath(absoluteFrom(root, values[1]), mainPath) {
				matchesMain = true
			}
			if strings.EqualFold(filepath.Ext(values[2]), ".pdf") {
				outputs = append(outputs, absoluteFrom(root, values[2]))
			}
		}
		if matchesMain {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			records = append(records, fdbRecord{modified: info.ModTime(), outputs: outputs})
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].modified.After(records[j].modified) })
	if len(records) > 0 {
		for _, path := range records[0].outputs {
			if checked, err := validatePDF(root, directory, path); err == nil {
				return checked, nil
			}
		}
		return "", fmt.Errorf("latexmk は終了しましたが，%s の処理に対応する PDF がありません．設定のコンパイル方法と latexmkrc の PDF 出力設定を確認してください．出力先: %s", main, directory)
	}
	// Versions without an fdb record can still use the conventional job name.
	stem := strings.TrimSuffix(filepath.Base(main), filepath.Ext(main))
	fallback := filepath.Join(directory, stem+".pdf")
	if checked, err := validatePDF(root, directory, fallback); err == nil {
		return checked, nil
	}
	return "", fmt.Errorf("latexmk は終了しましたが，PDF が見つかりません．メインの文書と latexmkrc の出力設定を確認してください．検索先: %s", directory)
}

func validatePDF(root, directory, path string) (string, error) {
	within, err := filepath.Rel(directory, path)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return "", errors.New("PDF is outside the build directory")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	path, err = safePath(root, filepath.ToSlash(relative), false)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var header [5]byte
	if _, err := io.ReadFull(file, header[:]); err != nil || !bytes.Equal(header[:], []byte("%PDF-")) {
		return "", errors.New("生成されたファイルは PDF の形式ではありません．")
	}
	return path, nil
}

func (a *App) copySuccessfulPDF(root, source, id string) (string, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	directory := a.projectCacheDirectory(root)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 512<<20 {
		return "", errors.New("PDF が 512 MiB を超えています．画像のサイズを確認してください．")
	}
	out, err := os.CreateTemp(directory, ".pdf-*")
	if err != nil {
		return "", err
	}
	temp := out.Name()
	defer os.Remove(temp)
	_, copyErr := io.Copy(out, io.LimitReader(in, (512<<20)+1))
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	destination := filepath.Join(directory, "pdf-"+id+".pdf")
	if err := platform.ReplaceFile(temp, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func minimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}
