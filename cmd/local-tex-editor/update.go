package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed update.html
var updateHTML string

//go:embed update.js
var updateJS string

type updateState struct {
	Phase            string `json:"phase"`
	CurrentVersion   string `json:"currentVersion"`
	RequestedVersion string `json:"requestedVersion"`
	Executable       string `json:"executable"`
	CurrentURL       string `json:"currentURL"`
	ReadyURL         string `json:"readyURL,omitempty"`
	Stopping         bool   `json:"stopping"`
	Error            string `json:"error,omitempty"`
}

type updatePage struct {
	mu          sync.Mutex
	state       updateState
	previous    activeInstance
	host        string
	token       string
	url         string
	readyFile   string
	server      *http.Server
	client      *http.Client
	clients     map[chan struct{}]struct{}
	grace       time.Duration
	idle        *time.Timer
	idleEpoch   uint64
	closed      bool
	cancelled   chan struct{}
	closeSignal chan struct{}
	cancelOnce  sync.Once
	closeOnce   sync.Once
}

func newUpdatePage(previous activeInstance, requested, executable string) *updatePage {
	return &updatePage{
		state: updateState{Phase: "waiting", CurrentVersion: previous.Version,
			RequestedVersion: requested, Executable: executable, CurrentURL: previous.browserURL()},
		previous: previous, client: instanceClient(30 * time.Second), grace: 5 * time.Second,
		clients: make(map[chan struct{}]struct{}), cancelled: make(chan struct{}), closeSignal: make(chan struct{}),
	}
}

func startUpdatePage(previous activeInstance, requested, executable, configDir, readyFile string, noBrowser bool, openBrowser func(string) error) (*updatePage, error) {
	page := newUpdatePage(previous, requested, executable)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		listener.Close()
		return nil, err
	}
	page.host = listener.Addr().String()
	page.token = hex.EncodeToString(secret)
	page.url = "http://" + page.host + "/"
	page.server = &http.Server{Handler: page.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := page.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			page.cancel()
		}
	}()
	ready := map[string]any{"url": page.url, "token": page.token, "pid": os.Getpid(), "version": requested, "phase": "update"}
	data, _ := json.Marshal(ready)
	if readyFile != "" {
		absolute, err := filepath.Abs(readyFile)
		if err != nil {
			page.Close()
			return nil, err
		}
		activePath := filepath.Join(configDir, "active.json")
		first, firstErr := os.Stat(absolute)
		second, secondErr := os.Stat(activePath)
		if strings.EqualFold(filepath.Clean(absolute), filepath.Clean(activePath)) || (firstErr == nil && secondErr == nil && os.SameFile(first, second)) {
			page.Close()
			return nil, errors.New("--ready-file には，起動中のアプリの active.json と別のファイルを指定してください．")
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			page.Close()
			return nil, err
		}
		if err := os.WriteFile(absolute, append(data, '\n'), 0600); err != nil {
			page.Close()
			return nil, err
		}
		page.readyFile = absolute
	}
	fmt.Println(string(data))
	if !noBrowser {
		if err := openBrowser(page.url + "#token=" + page.token); err != nil {
			page.Close()
			return nil, err
		}
	}
	return page, nil
}

func (page *updatePage) snapshot() updateState {
	page.mu.Lock()
	defer page.mu.Unlock()
	return page.state
}

func (page *updatePage) notifyLocked() {
	for client := range page.clients {
		select {
		case client <- struct{}{}:
		default:
		}
	}
}

func (page *updatePage) Starting() {
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.state.Phase == "cancelled" || page.closed {
		return
	}
	page.state.Phase, page.state.Error = "starting", ""
	page.notifyLocked()
}

func (page *updatePage) Complete(url string) bool {
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.state.Phase == "cancelled" || page.closed {
		return false
	}
	page.state.Phase, page.state.ReadyURL = "ready", url
	if page.idle != nil {
		page.idle.Stop()
	}
	page.idleEpoch++
	page.notifyLocked()
	// The page usually navigates immediately. Leave enough time for an active
	// browser to read the ready message if its stream is reconnecting.
	page.idle = time.AfterFunc(30*time.Second, page.Close)
	return true
}

func (page *updatePage) Cancelled() bool {
	select {
	case <-page.cancelled:
		return true
	default:
		return false
	}
}

func (page *updatePage) cancel() {
	page.mu.Lock()
	defer page.mu.Unlock()
	page.cancelLocked()
}

func (page *updatePage) cancelLocked() {
	if page.closed || page.state.Phase == "ready" {
		return
	}
	page.state.Phase = "cancelled"
	page.cancelOnce.Do(func() { close(page.cancelled) })
	page.notifyLocked()
}

func (page *updatePage) Close() {
	page.closeOnce.Do(func() {
		page.mu.Lock()
		page.closed = true
		if page.idle != nil {
			page.idle.Stop()
		}
		page.idleEpoch++
		close(page.closeSignal)
		page.mu.Unlock()
		if page.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := page.server.Shutdown(ctx); err != nil {
				_ = page.server.Close()
			}
		}
		if page.readyFile != "" {
			var ready activeInstance
			if data, err := os.ReadFile(page.readyFile); err == nil && json.Unmarshal(data, &ready) == nil && ready.URL == page.url && ready.Token == page.token {
				_ = os.Remove(page.readyFile)
			}
		}
	})
}

func (page *updatePage) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Host != page.host {
			http.Error(w, "Unexpected Host", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/update.js" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			content := updateHTML
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.URL.Path == "/update.js" {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				content = updateJS
			}
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, content)
			}
			return
		}
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(remote) == nil || !net.ParseIP(remote).IsLoopback() ||
			(r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+page.host) ||
			r.Header.Get("Sec-Fetch-Site") == "cross-site" ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Local-Token")), []byte(page.token)) != 1 {
			http.Error(w, "この接続からは操作できません．起動時に開いた画面を使用してください．", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/update/state":
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeUpdateJSON(w, http.StatusOK, page.snapshot())
		case "/api/update/events":
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			page.events(w, r)
		case "/api/update/stop", "/api/update/cancel":
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			var input struct {
				Saved bool `json:"saved"`
			}
			if mediaType != "application/json" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 512)).Decode(&input) != nil {
				writeUpdateJSON(w, http.StatusBadRequest, map[string]string{"error": "操作を JSON 形式で送信してください．"})
				return
			}
			if r.URL.Path == "/api/update/cancel" {
				writeUpdateJSON(w, http.StatusOK, map[string]bool{"ok": true})
				if flush, ok := w.(http.Flusher); ok {
					flush.Flush()
				}
				page.cancel()
				return
			}
			if !input.Saved {
				writeUpdateJSON(w, http.StatusBadRequest, map[string]string{"error": "開いている原稿を保存したことを確認してください．"})
				return
			}
			page.stopPrevious(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func writeUpdateJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (page *updatePage) stopPrevious(w http.ResponseWriter, r *http.Request) {
	page.mu.Lock()
	if page.state.Phase != "waiting" || page.state.Stopping {
		page.mu.Unlock()
		writeUpdateJSON(w, http.StatusConflict, map[string]string{"error": "アプリの切り替えを待っています．"})
		return
	}
	page.state.Stopping, page.state.Error = true, ""
	page.notifyLocked()
	page.mu.Unlock()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, page.previous.URL+"api/shutdown", strings.NewReader("{}"))
	if err == nil {
		request.Header.Set("X-Local-Token", page.previous.Token)
		request.Header.Set("Content-Type", "application/json")
		var response *http.Response
		response, err = page.client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("起動中のアプリが終了要求を受け付けませんでした (HTTP %d)．", response.StatusCode)
			}
		}
	}
	if err != nil {
		page.mu.Lock()
		if page.state.Phase == "waiting" {
			page.state.Stopping = false
			page.state.Error = "旧版の終了を確認できません．起動中の編集画面から「終了」を押してください．"
			page.notifyLocked()
		}
		page.mu.Unlock()
		writeUpdateJSON(w, http.StatusBadGateway, map[string]string{"error": "旧版の終了を確認できません．起動中の編集画面から「終了」を押してください．"})
		return
	}
	writeUpdateJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (page *updatePage) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming is unavailable", http.StatusInternalServerError)
		return
	}
	changes := make(chan struct{}, 1)
	page.mu.Lock()
	if page.closed {
		page.mu.Unlock()
		http.Error(w, "更新画面は終了しました．実行ファイルを開き直してください．", http.StatusGone)
		return
	}
	if page.idle != nil {
		page.idle.Stop()
	}
	page.idleEpoch++
	page.clients[changes] = struct{}{}
	page.mu.Unlock()
	defer func() {
		page.mu.Lock()
		delete(page.clients, changes)
		if len(page.clients) == 0 && !page.closed {
			if page.idle != nil {
				page.idle.Stop()
			}
			page.idleEpoch++
			epoch := page.idleEpoch
			page.idle = time.AfterFunc(page.grace, func() {
				page.mu.Lock()
				if page.closed || len(page.clients) > 0 || page.idleEpoch != epoch {
					page.mu.Unlock()
					return
				}
				if page.state.Phase == "ready" {
					page.mu.Unlock()
					page.Close()
				} else {
					page.cancelLocked()
					page.mu.Unlock()
				}
			})
		}
		page.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	writeState := func() bool {
		data, _ := json.Marshal(page.snapshot())
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeState() {
		return
	}
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-page.closeSignal:
			return
		case <-changes:
			if !writeState() {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
