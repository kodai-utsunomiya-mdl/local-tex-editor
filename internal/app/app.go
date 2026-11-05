package app

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

const maxTextSize = 8 << 20

type Options struct {
	ConfigDir   string
	ProjectPath string
	TexBin      string
	Token       string
	Version     string
	Shutdown    func()
}

type App struct {
	uiMu            sync.Mutex
	mu              sync.Mutex
	cacheMu         sync.RWMutex
	shortcutMu      sync.Mutex
	shortcutCreator func(string) (platform.ShortcutResult, error)
	options         Options
	config          configuration
	project         *Project
	build           *buildRun
	address         string
	closed          bool
	workers         sync.WaitGroup
	lastPDF         string
	lastPDFID       string
	pdfVersions     map[string]string
	pdfOrder        []string
	synctexVersions map[string]*synctexVersion
	synctexSlots    chan struct{}
}

type State struct {
	Version                string   `json:"version"`
	Platform               string   `json:"platform"`
	TexBin                 string   `json:"texBin"`
	Latexmk                string   `json:"latexmk"`
	ToolError              string   `json:"toolError,omitempty"`
	Synctex                string   `json:"synctex"`
	SynctexError           string   `json:"synctexError,omitempty"`
	SynctexInstallCommands []string `json:"synctexInstallCommands"`
	ShortcutsSupported     bool     `json:"shortcutsSupported"`
	Project                *Project `json:"project"`
	RecentProjects         []string `json:"recentProjects"`
	Build                  *Build   `json:"build"`
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func fail(status int, format string, args ...any) error {
	return &apiError{status, fmt.Sprintf(format, args...)}
}

func New(options Options) (*App, error) {
	if len(options.Token) < 16 {
		return nil, errors.New("the local API token must contain at least 16 characters")
	}
	if options.ConfigDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("設定フォルダーを取得できません: %w", err)
		}
		options.ConfigDir = filepath.Join(base, "LocalTeXEditor")
	}
	var err error
	options.ConfigDir, err = filepath.Abs(options.ConfigDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(options.ConfigDir, 0700); err != nil {
		return nil, fmt.Errorf("設定フォルダーを作成できません: %w", err)
	}
	a := &App{options: options, pdfVersions: make(map[string]string), synctexVersions: make(map[string]*synctexVersion), synctexSlots: make(chan struct{}, 2)}
	if err := a.loadConfig(); err != nil {
		return nil, err
	}
	if options.TexBin != "" {
		a.config.TexBin = options.TexBin
	}
	if options.ProjectPath != "" {
		if err := a.openProjectLocked(options.ProjectPath); err != nil {
			return nil, err
		}
	} else if len(a.config.RecentProjects) > 0 {
		// An unavailable recent folder should still allow the user to open another one.
		_ = a.openProjectLocked(a.config.RecentProjects[0])
	}
	return a, nil
}

// SetAddress receives the actual address of the listening loopback socket.
func (a *App) SetAddress(address string) {
	address = strings.TrimPrefix(address, "http://")
	address = strings.TrimSuffix(address, "/")
	a.mu.Lock()
	a.address = strings.ToLower(address)
	a.mu.Unlock()
}

func (a *App) Close() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	platform.CancelFolderPicker()
	_ = a.stopBuild()
	done := make(chan struct{})
	go func() { a.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("コンパイルの停止を待っています．")
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", a.stateHandler)
	mux.HandleFunc("/api/project/open", a.openHandler)
	mux.HandleFunc("/api/project/pick", a.pickHandler)
	mux.HandleFunc("/api/settings", a.settingsHandler)
	mux.HandleFunc("/api/shortcuts", a.shortcutsHandler)
	mux.HandleFunc("/api/storage", a.storageHandler)
	mux.HandleFunc("/api/file", a.fileHandler)
	mux.HandleFunc("/api/asset", a.assetHandler)
	mux.HandleFunc("/api/build", a.buildHandler)
	mux.HandleFunc("/api/build/stop", a.stopHandler)
	mux.HandleFunc("/api/pdf", a.pdfHandler)
	mux.HandleFunc("/api/synctex/forward", a.forwardSyncHandler)
	mux.HandleFunc("/api/synctex/inverse", a.inverseSyncHandler)
	mux.HandleFunc("/api/shutdown", a.shutdownHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, fail(http.StatusNotFound, "API が見つかりません．"))
	})
	return a.authenticate(mux)
}

func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		a.mu.Lock()
		address, closed := a.address, a.closed
		a.mu.Unlock()
		if address == "" || closed {
			writeError(w, fail(http.StatusServiceUnavailable, "アプリを起動し直してください．"))
			return
		}
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(remote) == nil || !net.ParseIP(remote).IsLoopback() || !strings.EqualFold(r.Host, address) {
			writeError(w, fail(http.StatusForbidden, "この接続からは利用できません．起動時に開いた画面を使用してください．"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+address {
			writeError(w, fail(http.StatusForbidden, "編集画面と異なるサイトからの接続です．"))
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, fail(http.StatusForbidden, "編集画面から操作してください．"))
			return
		}
		token := r.Header.Get("X-Local-Token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.options.Token)) != 1 {
			writeError(w, fail(http.StatusUnauthorized, "接続情報が一致しません．アプリを起動したときの URL を開き直してください．"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, fail(http.StatusMethodNotAllowed, "この操作は受け付けていません．"))
	return false
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fail(http.StatusUnsupportedMediaType, "JSON 形式で送信してください．")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*maxTextSize))
	if err != nil {
		return fail(http.StatusRequestEntityTooLarge, "送信するテキストが大きすぎます．")
	}
	if !utf8.Valid(body) {
		return fail(http.StatusBadRequest, "UTF-8 のテキストを送信してください．")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fail(http.StatusBadRequest, "送信内容を読み取れません: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var known *apiError
	if errors.As(err, &known) {
		status = known.status
	} else if errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	} else if errors.Is(err, os.ErrPermission) {
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (a *App) stateLocked(refresh bool) State {
	if refresh && a.project != nil {
		if files, directories, err := listProjectEntries(a.project.Root); err == nil {
			a.project.Files = files
			a.project.Directories = directories
		}
	}
	result := State{
		Version: a.options.Version, Platform: runtime.GOOS,
		ShortcutsSupported: runtime.GOOS == "windows",
		TexBin:             a.config.TexBin, RecentProjects: append([]string{}, a.config.RecentProjects...),
		Build: a.buildSnapshotLocked(),
	}
	if a.project != nil {
		copy := *a.project
		copy.Files = append([]ProjectFile{}, copy.Files...)
		copy.Directories = append([]string{}, copy.Directories...)
		result.Project = &copy
	}
	tool, bin, err := findLatexmk(a.config.TexBin)
	result.Latexmk = tool
	result.Synctex, result.SynctexError, result.SynctexInstallCommands = synctexStatus(bin)
	if a.config.TexBin == "" && bin != "" {
		result.TexBin = bin
	}
	if err != nil {
		result.ToolError = err.Error()
	}
	return result
}

func (a *App) stateHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	a.mu.Lock()
	state := a.stateLocked(true)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, state)
}

func (a *App) openHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input struct {
		Path string `json:"path"`
	}
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	if a.isRunningLocked() {
		a.mu.Unlock()
		writeError(w, fail(http.StatusConflict, "コンパイルが終わるか，停止してから別のプロジェクトを開いてください．"))
		return
	}
	err := a.openProjectLocked(input.Path)
	var state State
	if err == nil {
		state = a.stateLocked(false)
	}
	a.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *App) settingsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	var input struct {
		TexBin *string `json:"texBin"`
		Main   *string `json:"main"`
		Mode   *string `json:"mode"`
	}
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	err := a.checkProjectLocked(r.Header.Get("X-Project-Root"))
	if err == nil {
		err = a.updateSettingsLocked(input.TexBin, input.Main, input.Mode)
	}
	var state State
	if err == nil {
		state = a.stateLocked(false)
	}
	a.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *App) fileHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	var input FileDocument
	if r.Method == http.MethodPut {
		if err := readJSON(w, r, &input); err != nil {
			writeError(w, err)
			return
		}
	} else {
		input.Path = r.URL.Query().Get("path")
	}
	a.mu.Lock()
	var document FileDocument
	err := a.checkProjectLocked(r.Header.Get("X-Project-Root"))
	if err != nil {
		// The request belongs to a different project opened in another tab.
	} else if a.project == nil {
		err = fail(http.StatusBadRequest, "原稿フォルダーを開いてください．")
	} else if r.Method == http.MethodGet {
		document, err = readDocument(a.project.Root, input.Path)
	} else {
		document, err = saveDocument(a.project.Root, input)
		if err == nil {
			if a.build != nil && document.Revision != input.Revision {
				a.build.result.SourceChanged = true
			}
			if files, directories, listErr := listProjectEntries(a.project.Root); listErr == nil {
				a.project.Files = files
				a.project.Directories = directories
			}
		}
	}
	a.mu.Unlock()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (a *App) buildHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodPost {
		build, err := a.startBuild(r.Header.Get("X-Project-Root"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, build)
		return
	}
	a.mu.Lock()
	if err := a.checkProjectLocked(r.Header.Get("X-Project-Root")); err != nil {
		a.mu.Unlock()
		writeError(w, err)
		return
	}
	build := a.buildSnapshotLocked()
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, build)
}

func (a *App) stopHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := a.stopBuild(r.Header.Get("X-Project-Root")); err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	build := a.buildSnapshotLocked()
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, build)
}

func (a *App) pdfHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodHead) {
		return
	}
	a.mu.Lock()
	if err := a.checkProjectLocked(r.Header.Get("X-Project-Root")); err != nil {
		a.mu.Unlock()
		writeError(w, err)
		return
	}
	path, buildID := a.lastPDF, a.lastPDFID
	if requested := r.URL.Query().Get("build"); requested != "" && requested != buildID {
		path, buildID = a.pdfVersions[requested], requested
		if path == "" {
			a.mu.Unlock()
			writeError(w, fail(http.StatusGone, "この版の PDF はキャッシュから削除されました．最新の PDF を開き直してください．"))
			return
		}
	}
	a.mu.Unlock()
	if path == "" {
		writeError(w, fail(http.StatusNotFound, "PDF はまだ生成されていません．コンパイルのログを確認してください．"))
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, fmt.Errorf("PDF を開けません: %w", err))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename=\"document.pdf\"")
	w.Header().Set("ETag", `"`+buildID+`"`)
	http.ServeContent(w, r, "document.pdf", info.ModTime(), file)
}

func (a *App) shutdownHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	_ = a.stopBuild()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if a.options.Shutdown != nil {
		go func() {
			time.Sleep(100 * time.Millisecond)
			a.options.Shutdown()
		}()
	}
}

func (a *App) checkProjectLocked(expected string) error {
	if expected == "" {
		return nil
	}
	path, err := url.PathUnescape(expected)
	if err != nil {
		return fail(http.StatusBadRequest, "プロジェクトのパスを読み取れません．")
	}
	if a.project == nil || !samePath(a.project.Root, path) {
		return fail(http.StatusConflict, "別の画面でプロジェクトが切り替わりました．編集中の内容を確認してから，この画面を開き直してください．")
	}
	return nil
}
