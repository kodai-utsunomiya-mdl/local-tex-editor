package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

type ProjectFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type Project struct {
	Root  string        `json:"root"`
	Main  string        `json:"main"`
	Mode  string        `json:"mode"`
	Files []ProjectFile `json:"files"`
}

type FileDocument struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Revision   string `json:"revision"`
	LineEnding string `json:"lineEnding"`
}

type projectSettings struct {
	Main string `json:"main"`
	Mode string `json:"mode"`
}

type configuration struct {
	TexBin         string                     `json:"texBin"`
	RecentProjects []string                   `json:"recentProjects"`
	Projects       map[string]projectSettings `json:"projects"`
}

func (a *App) loadConfig() error {
	a.config = configuration{Projects: map[string]projectSettings{}, RecentProjects: []string{}}
	path := filepath.Join(a.options.ConfigDir, "settings.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("設定を読めません: %w", err)
	}
	if len(data) > 1<<20 {
		return errors.New("設定ファイルが大きすぎます．settings.json を確認してください．")
	}
	if err := json.Unmarshal(data, &a.config); err != nil {
		return fmt.Errorf("設定ファイル %s を読めません: %w", path, err)
	}
	if a.config.Projects == nil {
		a.config.Projects = map[string]projectSettings{}
	}
	return nil
}

func (a *App) saveConfig(config configuration) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(a.options.ConfigDir, "settings.json"), append(data, '\n'), 0600)
}

func cloneConfig(config configuration) configuration {
	copy := config
	copy.RecentProjects = append([]string{}, config.RecentProjects...)
	copy.Projects = make(map[string]projectSettings, len(config.Projects))
	for key, value := range config.Projects {
		copy.Projects[key] = value
	}
	return copy
}

func (a *App) openProjectLocked(rawPath string) error {
	rawPath = strings.TrimSpace(rawPath)
	if !filepath.IsAbs(rawPath) {
		return fail(http.StatusBadRequest, "フォルダーの絶対パスを入力してください．")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(rawPath))
	if err != nil {
		return fmt.Errorf("原稿フォルダーを開けません: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fail(http.StatusBadRequest, "ファイルが選ばれています．原稿を含むフォルダーを選んでください．")
	}
	files, err := listProjectFiles(root)
	if err != nil {
		return err
	}
	settings, exists := a.config.Projects[root]
	if !exists {
		settings.Mode = "pdflatex"
		for _, name := range []string{"latexmkrc", ".latexmkrc"} {
			if info, err := os.Stat(filepath.Join(root, name)); err == nil && info.Mode().IsRegular() {
				settings.Mode = "project"
			}
		}
	}
	if !validMode(settings.Mode) {
		settings.Mode = "project"
	}
	if !hasFile(files, settings.Main) || !strings.EqualFold(filepath.Ext(settings.Main), ".tex") {
		settings.Main = chooseMain(root, files)
	}
	config := cloneConfig(a.config)
	config.Projects[root] = settings
	config.RecentProjects = []string{root}
	for _, previous := range a.config.RecentProjects {
		if !samePath(root, previous) && len(config.RecentProjects) < 12 {
			config.RecentProjects = append(config.RecentProjects, previous)
		}
	}
	if err := a.saveConfig(config); err != nil {
		return fmt.Errorf("プロジェクトの設定を保存できません: %w", err)
	}
	a.config = config
	a.project = &Project{Root: root, Main: settings.Main, Mode: settings.Mode, Files: files}
	a.build = nil
	a.lastPDF, a.lastPDFID = "", ""
	a.pdfVersions = make(map[string]string)
	a.pdfOrder = nil
	a.synctexVersions = make(map[string]*synctexVersion)
	a.restoreCachedBuildLocked(*a.project)
	return nil
}

func (a *App) updateSettingsLocked(texBin, main, mode *string) error {
	if a.isRunningLocked() {
		return fail(http.StatusConflict, "コンパイルが終わるか，停止してから設定を変更してください．")
	}
	config := cloneConfig(a.config)
	var project Project
	if a.project != nil {
		project = *a.project
	}
	if texBin != nil {
		config.TexBin = strings.TrimSpace(*texBin)
		if config.TexBin != "" && !filepath.IsAbs(config.TexBin) {
			return fail(http.StatusBadRequest, "TeX Live のフォルダーには絶対パスを指定してください．")
		}
	}
	if main != nil || mode != nil {
		if a.project == nil {
			return fail(http.StatusBadRequest, "先に原稿フォルダーを開いてください．")
		}
		if main != nil {
			clean, path, err := projectFilePath(project.Root, *main, false)
			if err != nil {
				return err
			}
			if !strings.EqualFold(filepath.Ext(clean), ".tex") {
				return fail(http.StatusBadRequest, "メインの文書には .tex ファイルを選んでください．")
			}
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				return fail(http.StatusBadRequest, "選択したメインの文書を開けません．")
			}
			project.Main = clean
		}
		if mode != nil {
			if !validMode(*mode) {
				return fail(http.StatusBadRequest, "コンパイル方法を選び直してください．")
			}
			project.Mode = *mode
		}
		config.Projects[project.Root] = projectSettings{Main: project.Main, Mode: project.Mode}
	}
	if err := a.saveConfig(config); err != nil {
		return fmt.Errorf("設定を保存できません: %w", err)
	}
	a.config = config
	if a.project != nil {
		a.project = &project
	}
	if a.build != nil {
		a.build.result.SourceChanged = true
	}
	return nil
}

func validMode(mode string) bool {
	switch mode {
	case "project", "pdflatex", "uplatex", "xelatex", "lualatex":
		return true
	default:
		return false
	}
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func hasFile(files []ProjectFile, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}

func chooseMain(root string, files []ProjectFile) string {
	for _, file := range files {
		if file.Path == "main.tex" {
			return file.Path
		}
	}
	var first string
	for _, file := range files {
		if !strings.EqualFold(filepath.Ext(file.Path), ".tex") {
			continue
		}
		if first == "" {
			first = file.Path
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			continue
		}
		prefix, _ := io.ReadAll(io.LimitReader(f, 8192))
		_ = f.Close()
		if bytes.Contains(prefix, []byte(`\documentclass`)) {
			return file.Path
		}
	}
	return first
}

func isTextFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == "latexmkrc" || base == ".latexmkrc" || base == "makefile" {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tex", ".bib", ".sty", ".cls", ".bst", ".clo", ".def", ".cfg", ".bbx", ".cbx", ".lbx", ".dtx", ".ins", ".ltx", ".tikz", ".pgf", ".txt", ".md", ".csv", ".tsv", ".json", ".yaml", ".yml", ".toml", ".lua", ".asy":
		return true
	default:
		return false
	}
}

func excludedDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".local-tex-editor", "node_modules", ".venv", "venv", "__pycache__":
		return true
	default:
		return false
	}
}

func listProjectFiles(root string) ([]ProjectFile, error) {
	files := []ProjectFile{}
	visited := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("フォルダーを読み取れません (%s): %w", path, walkErr)
		}
		if path == root {
			return nil
		}
		visited++
		if visited > 50000 {
			return fail(http.StatusBadRequest, "ファイルが多すぎます．原稿を含むフォルダーを直接選んでください．")
		}
		if entry.IsDir() && excludedDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		linked, err := platform.IsReparsePoint(path)
		if err != nil {
			return err
		}
		if linked {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() || !isTextFile(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, ProjectFile{Path: filepath.ToSlash(relative), Size: info.Size()})
		if len(files) > 10000 {
			return fail(http.StatusBadRequest, "テキストファイルが多すぎます．原稿のフォルダーを直接選んでください．")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func cleanRelative(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, `\`, "/")
	if raw == "" || strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || strings.ContainsAny(raw, ":\x00") {
		return "", fail(http.StatusBadRequest, "プロジェクト内の相対パスを指定してください．")
	}
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, " .") != part || excludedDirectory(part) {
			return "", fail(http.StatusBadRequest, "このパスは開けません．")
		}
		if strings.ContainsAny(part, `<>"|?*`) {
			return "", fail(http.StatusBadRequest, "ファイル名に使用できない文字があります．")
		}
		for _, character := range part {
			if character < 32 {
				return "", fail(http.StatusBadRequest, "ファイル名に制御文字を使用できません．")
			}
		}
		device := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if device == "CON" || device == "PRN" || device == "AUX" || device == "NUL" || (len(device) == 4 && (strings.HasPrefix(device, "COM") || strings.HasPrefix(device, "LPT")) && device[3] >= '1' && device[3] <= '9') {
			return "", fail(http.StatusBadRequest, "このファイル名は Windows が使用する名前です．")
		}
	}
	return strings.Join(parts, "/"), nil
}

func projectFilePath(root, relative string, allowMissing bool) (string, string, error) {
	clean, err := cleanRelative(relative)
	if err != nil {
		return "", "", err
	}
	if !isTextFile(clean) {
		return "", "", fail(http.StatusUnsupportedMediaType, "この種類のファイルはテキストとして編集できません．")
	}
	path, err := safePath(root, clean, allowMissing)
	return clean, path, err
}

// safePath checks each component; the caller already validated the relative name.
func safePath(root, relative string, allowMissing bool) (string, error) {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	current := root
	for index, part := range parts {
		if part == "" || part == "." || part == ".." || filepath.IsAbs(part) {
			return "", fail(http.StatusBadRequest, "このパスは開けません．")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if allowMissing && index == len(parts)-1 && errors.Is(err, os.ErrNotExist) {
				return current, nil
			}
			return "", fmt.Errorf("ファイルを開けません (%s): %w", relative, err)
		}
		linked, err := platform.IsReparsePoint(current)
		if err != nil {
			return "", err
		}
		if linked || info.Mode()&os.ModeSymlink != 0 {
			return "", fail(http.StatusForbidden, "リンクを経由したファイルは開けません．実際の原稿フォルダーを選んでください．")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fail(http.StatusBadRequest, "途中のパスがフォルダーではありません．")
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return "", fail(http.StatusBadRequest, "通常のファイルを選んでください．")
		}
	}
	return current, nil
}

func readBytes(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fail(http.StatusBadRequest, "通常のファイルを選んでください．")
	}
	if info.Size() > maxTextSize {
		return nil, fail(http.StatusRequestEntityTooLarge, "このテキストは 8 MiB を超えているため，エディタで開けません．")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTextSize+1))
	if len(data) > maxTextSize {
		return nil, fail(http.StatusRequestEntityTooLarge, "テキストが大きすぎます．")
	}
	return data, err
}

func revision(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func documentFromBytes(path string, data []byte) (FileDocument, error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return FileDocument{}, fail(http.StatusUnsupportedMediaType, "このファイルは UTF-8 のテキストではありません．文字コードを UTF-8 に変換してから開いてください．")
	}
	text := bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	ending := "lf"
	if bytes.Contains(text, []byte("\r\n")) {
		ending = "crlf"
	}
	return FileDocument{Path: path, Content: strings.ReplaceAll(string(text), "\r\n", "\n"), Revision: revision(data), LineEnding: ending}, nil
}

func readDocument(root, relative string) (FileDocument, error) {
	clean, path, err := projectFilePath(root, relative, false)
	if err != nil {
		return FileDocument{}, err
	}
	data, err := readBytes(path)
	if err != nil {
		return FileDocument{}, err
	}
	return documentFromBytes(clean, data)
}

func saveDocument(root string, document FileDocument) (FileDocument, error) {
	clean, path, err := projectFilePath(root, document.Path, true)
	if err != nil {
		return FileDocument{}, err
	}
	if len(document.Content) > maxTextSize || !utf8.ValidString(document.Content) || strings.ContainsRune(document.Content, 0) {
		return FileDocument{}, fail(http.StatusBadRequest, "保存するテキストは 8 MiB 以内の UTF-8 にしてください．")
	}
	old, readErr := readBytes(path)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileDocument{}, readErr
	}
	if (exists && document.Revision != revision(old)) || (!exists && document.Revision != "") {
		return FileDocument{}, fail(http.StatusConflict, "ディスク上のファイルが変更されています．内容を確認してから保存してください．")
	}
	text := strings.ReplaceAll(document.Content, "\r\n", "\n")
	if exists && bytes.Contains(old, []byte("\r\n")) {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	data := []byte(text)
	if exists && bytes.HasPrefix(old, []byte{0xef, 0xbb, 0xbf}) {
		data = append([]byte{0xef, 0xbb, 0xbf}, data...)
	}
	if len(data) > maxTextSize {
		return FileDocument{}, fail(http.StatusRequestEntityTooLarge, "保存するファイルが 8 MiB を超えています．")
	}
	if exists && bytes.Equal(old, data) {
		return documentFromBytes(clean, data)
	}
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeAtomicChecked(path, data, mode, func() error {
		if _, _, err := projectFilePath(root, clean, true); err != nil {
			return err
		}
		current, err := readBytes(path)
		if exists {
			if err != nil || revision(current) != document.Revision {
				return fail(http.StatusConflict, "保存中にディスク上のファイルが変更されました．")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(http.StatusConflict, "同じ名前のファイルが作成されました．内容を確認してください．")
		}
		return nil
	}); err != nil {
		return FileDocument{}, fmt.Errorf("保存できません: %w", err)
	}
	return documentFromBytes(clean, data)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	return writeAtomicChecked(path, data, mode, nil)
}

func writeAtomicChecked(path string, data []byte, mode os.FileMode, check func() error) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".local-tex-editor-save-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(mode); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	return platform.ReplaceFile(name, path)
}
