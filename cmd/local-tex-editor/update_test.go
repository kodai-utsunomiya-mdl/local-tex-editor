package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testUpdatePage(t *testing.T) *updatePage {
	t.Helper()
	page := newUpdatePage(activeInstance{URL: "http://127.0.0.1:19001/", Token: strings.Repeat("a", 64), Version: "0.1.2"}, "0.1.3", "app.exe")
	page.host, page.token = "127.0.0.1:19002", strings.Repeat("b", 64)
	page.url = "http://" + page.host + "/"
	t.Cleanup(page.Close)
	return page
}

func updateRequest(page *updatePage, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, page.url+strings.TrimPrefix(path, "/"), strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:20000"
	r.Header.Set("X-Local-Token", page.token)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func TestUpdateRequiresAuthorizationAndSavedConfirmation(t *testing.T) {
	var stopped atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/shutdown" || r.Method != http.MethodPost || r.Header.Get("X-Local-Token") != strings.Repeat("a", 64) {
			t.Error("previous application received an unexpected request")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		stopped.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer old.Close()
	page := testUpdatePage(t)
	page.previous.URL = old.URL + "/"
	for _, tc := range []struct {
		name string
		body string
		edit func(*http.Request)
		want int
	}{
		{"no token", `{"saved":true}`, func(r *http.Request) { r.Header.Del("X-Local-Token") }, http.StatusForbidden},
		{"foreign origin", `{"saved":true}`, func(r *http.Request) { r.Header.Set("Origin", "https://example.com") }, http.StatusForbidden},
		{"remote caller", `{"saved":true}`, func(r *http.Request) { r.RemoteAddr = "192.0.2.1:20000" }, http.StatusForbidden},
		{"foreign host", `{"saved":true}`, func(r *http.Request) { r.Host = "example.com" }, http.StatusForbidden},
		{"no save confirmation", `{}`, nil, http.StatusBadRequest},
		{"save not confirmed", `{"saved":false}`, nil, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := updateRequest(page, http.MethodPost, "/api/update/stop", tc.body)
			if tc.edit != nil {
				tc.edit(r)
			}
			w := httptest.NewRecorder()
			page.handler().ServeHTTP(w, r)
			if w.Code != tc.want || stopped.Load() != 0 {
				t.Fatalf("status = %d, stop count = %d", w.Code, stopped.Load())
			}
		})
	}
	w := httptest.NewRecorder()
	page.handler().ServeHTTP(w, updateRequest(page, http.MethodPost, "/api/update/stop", `{"saved":true}`))
	if w.Code != http.StatusOK || stopped.Load() != 1 {
		t.Fatalf("confirmed stop: status = %d, stop count = %d", w.Code, stopped.Load())
	}
	w = httptest.NewRecorder()
	page.handler().ServeHTTP(w, updateRequest(page, http.MethodPost, "/api/update/stop", `{"saved":true}`))
	if w.Code != http.StatusConflict || stopped.Load() != 1 {
		t.Fatalf("duplicate stop: status = %d, stop count = %d", w.Code, stopped.Load())
	}
}

func TestUpdateCompletionAndCancellationAreAtomic(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		page := newUpdatePage(activeInstance{}, "0.1.3", "app.exe")
		start := make(chan struct{})
		completed := make(chan bool, 1)
		cancelled := make(chan struct{})
		go func() { <-start; completed <- page.Complete("http://127.0.0.1:19003/#token=new") }()
		go func() { <-start; page.cancel(); close(cancelled) }()
		close(start)
		ok := <-completed
		<-cancelled
		state := page.snapshot()
		if ok {
			if state.Phase != "ready" || state.ReadyURL == "" || page.Cancelled() {
				t.Fatalf("completion succeeded but state = %#v, cancelled = %v", state, page.Cancelled())
			}
		} else if state.Phase != "cancelled" || state.ReadyURL != "" || !page.Cancelled() {
			t.Fatalf("completion failed but state = %#v, cancelled = %v", state, page.Cancelled())
		}
		page.Close()
	}
}

func TestUpdateCancelBeforeCompletionDoesNotStartApplication(t *testing.T) {
	page := testUpdatePage(t)
	w := httptest.NewRecorder()
	page.handler().ServeHTTP(w, updateRequest(page, http.MethodPost, "/api/update/cancel", `{}`))
	if w.Code != http.StatusOK || !page.Cancelled() {
		t.Fatalf("cancel: status = %d, cancelled = %v", w.Code, page.Cancelled())
	}
	page.Starting()
	if page.Complete("http://127.0.0.1:19003/") || page.snapshot().ReadyURL != "" {
		t.Fatal("cancelled update entered the ready phase")
	}
}

type updateStreamWriter struct {
	header  http.Header
	flushed chan struct{}
	once    sync.Once
}

func (w *updateStreamWriter) Header() http.Header            { return w.header }
func (w *updateStreamWriter) WriteHeader(int)                {}
func (w *updateStreamWriter) Write(data []byte) (int, error) { return len(data), nil }
func (w *updateStreamWriter) Flush()                         { w.once.Do(func() { close(w.flushed) }) }

func openUpdateStream(t *testing.T, page *updatePage) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &updateStreamWriter{header: make(http.Header), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		page.handler().ServeHTTP(w, updateRequest(page, http.MethodGet, "/api/update/events", "").WithContext(ctx))
		close(done)
	}()
	select {
	case <-w.flushed:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("update stream did not connect")
	}
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(closeStream)
	return closeStream
}

func TestUpdateStreamReconnectCancelsPendingExit(t *testing.T) {
	page := testUpdatePage(t)
	page.grace = 30 * time.Millisecond
	first := openUpdateStream(t, page)
	second := openUpdateStream(t, page)
	first()
	select {
	case <-page.cancelled:
		t.Fatal("one remaining tab should keep the update open")
	case <-time.After(2 * page.grace):
	}
	second()
	reloaded := openUpdateStream(t, page)
	select {
	case <-page.cancelled:
		t.Fatal("reloading the update page should cancel its pending exit")
	case <-time.After(2 * page.grace):
	}
	reloaded()
	select {
	case <-page.cancelled:
	case <-time.After(time.Second):
		t.Fatal("closing the last update tab did not cancel startup")
	}
	if page.Complete("http://127.0.0.1:19003/") {
		t.Fatal("completed after the last tab had cancelled startup")
	}
}

func TestUpdateCloseLeavesNewApplicationReadyFile(t *testing.T) {
	page := testUpdatePage(t)
	page.readyFile = filepath.Join(t.TempDir(), "ready.json")
	newReady := activeInstance{URL: "http://127.0.0.1:19003/", Token: strings.Repeat("c", 64), Version: "0.1.3"}
	data, _ := json.Marshal(newReady)
	if err := os.WriteFile(page.readyFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	page.Close()
	actual, err := os.ReadFile(page.readyFile)
	if err != nil || string(actual) != string(data) {
		t.Fatalf("ready file from the new application changed: %s, %v", actual, err)
	}
}
