package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type browserTestResponse struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
	fail    bool
}

func (w *browserTestResponse) FlushError() error {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.flushed) })
	if w.fail {
		return errors.New("browser disconnected")
	}
	return nil
}

type browserTestSession struct {
	cancel   context.CancelFunc
	done     chan struct{}
	response *browserTestResponse
}

func openBrowserTestSession(t *testing.T, a *App, failWrite bool) browserTestSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/api/session", nil).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Local-Token", testToken)
	response := &browserTestResponse{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}), fail: failWrite}
	session := browserTestSession{cancel: cancel, done: make(chan struct{}), response: response}
	go func() {
		a.Handler().ServeHTTP(response, request)
		close(session.done)
	}()
	t.Cleanup(func() {
		cancel()
		waitForBrowserEvent(t, session.done, "request to finish")
	})
	waitForBrowserEvent(t, response.flushed, "initial session response")
	return session
}

func waitForBrowserEvent(t *testing.T, event <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func keepBrowserRunning(t *testing.T, stopped <-chan struct{}, duration time.Duration) {
	t.Helper()
	select {
	case <-stopped:
		t.Fatal("the application stopped while a browser tab was open")
	case <-time.After(duration):
	}
}

func TestBrowserLastTabStopsApplicationAndBuild(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 30 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	a.build = &buildRun{result: Build{Status: "running"}}
	first := openBrowserTestSession(t, a, false)
	second := openBrowserTestSession(t, a, false)
	first.cancel()
	waitForBrowserEvent(t, first.done, "first tab to close")
	keepBrowserRunning(t, stopped, 3*a.browserCloseDelay)
	second.cancel()
	waitForBrowserEvent(t, second.done, "last tab to close")
	waitForBrowserEvent(t, stopped, "application shutdown")
	a.mu.Lock()
	closed, cancelRequested := a.closed, a.build.cancelRequested
	a.mu.Unlock()
	if !closed || !cancelRequested {
		t.Fatalf("closed=%v, build cancel requested=%v", closed, cancelRequested)
	}
	if first.response.Code != http.StatusOK || first.response.Body.String() != ": connected\n\n" {
		t.Fatalf("session response: %d %q", first.response.Code, first.response.Body.String())
	}
	response := apiRequest(a, http.MethodGet, "/api/session", nil, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("new session after shutdown: %d", response.Code)
	}
}

func TestBrowserReloadCancelsPendingShutdown(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 60 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	first := openBrowserTestSession(t, a, false)
	first.cancel()
	waitForBrowserEvent(t, first.done, "old page to close")
	second := openBrowserTestSession(t, a, false)
	keepBrowserRunning(t, stopped, 3*a.browserCloseDelay)
	second.cancel()
	waitForBrowserEvent(t, stopped, "shutdown after the reloaded tab closes")
}

func TestBrowserNeverConnectedDoesNotStopAPIProcess(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 10 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	response := apiRequest(a, http.MethodGet, "/api/session", nil, map[string]string{"X-Local-Token": ""})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated session: %d", response.Code)
	}
	response = apiRequest(a, http.MethodPost, "/api/session", nil, nil)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("invalid session method: %d", response.Code)
	}
	keepBrowserRunning(t, stopped, 3*a.browserCloseDelay)
	a.mu.Lock()
	connections, timer := a.browserConnections, a.browserTimer
	a.mu.Unlock()
	if connections != 0 || timer != nil {
		t.Fatal("a request without a browser session changed the lifetime")
	}
}

func TestExpectedBrowserThatNeverConnectsStopsApplication(t *testing.T) {
	a, _ := testApp(t)
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	a.ExpectBrowserConnection(10 * time.Millisecond)
	a.ExpectBrowserConnection(time.Hour)
	waitForBrowserEvent(t, stopped, "first browser connection deadline")
	a.ExpectBrowserConnection(time.Hour)
	a.mu.Lock()
	timer, closed := a.browserTimer, a.closed
	a.mu.Unlock()
	if timer != nil || !closed {
		t.Fatal("an expectation reopened the closed application")
	}
}

func TestExpectedBrowserConnectsBeforeDeadline(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 10 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	a.ExpectBrowserConnection(100 * time.Millisecond)
	session := openBrowserTestSession(t, a, false)
	keepBrowserRunning(t, stopped, 150*time.Millisecond)
	session.cancel()
	waitForBrowserEvent(t, stopped, "connected tab to close")
}

func TestExpectedBrowserDoesNotExtendLastTabGrace(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 10 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	session := openBrowserTestSession(t, a, false)
	a.ExpectBrowserConnection(time.Hour)
	session.cancel()
	waitForBrowserEvent(t, session.done, "last tab to close")
	a.ExpectBrowserConnection(time.Hour)
	waitForBrowserEvent(t, stopped, "unchanged last-tab deadline")
}

func TestBrowserSessionsCloseBeforeServerShutdown(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "server stops"
		if explicit {
			name = "exit button"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := testApp(t)
			a.browserCloseDelay = 10 * time.Millisecond
			var calls atomic.Int32
			stopped := make(chan struct{})
			a.options.Shutdown = func() {
				calls.Add(1)
				close(stopped)
			}
			first := openBrowserTestSession(t, a, false)
			second := openBrowserTestSession(t, a, false)
			if explicit {
				response := apiRequest(a, http.MethodPost, "/api/shutdown", nil, nil)
				if response.Code != http.StatusOK {
					t.Fatal(response.Body.String())
				}
				waitForBrowserEvent(t, stopped, "explicit shutdown")
			} else {
				a.CloseBrowserSessions()
			}
			waitForBrowserEvent(t, first.done, "first response to close")
			waitForBrowserEvent(t, second.done, "second response to close")
			a.CloseBrowserSessions()
			if explicit {
				a.requestShutdown()
				if calls.Load() != 1 {
					t.Fatalf("shutdown callback count: %d", calls.Load())
				}
			} else {
				keepBrowserRunning(t, stopped, 3*a.browserCloseDelay)
			}
		})
	}
}

func TestBrowserFailedWriteReleasesSession(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 10 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	session := openBrowserTestSession(t, a, true)
	waitForBrowserEvent(t, session.done, "failed response to close")
	waitForBrowserEvent(t, stopped, "shutdown after the connection fails")
}

func TestBrowserShutdownAllowsArrivedDraftPatchToFinish(t *testing.T) {
	a, _ := testApp(t)
	a.browserCloseDelay = 10 * time.Millisecond
	stopped := make(chan struct{})
	a.options.Shutdown = func() { close(stopped) }
	patchArrived := make(chan struct{})
	server := httptest.NewUnstartedServer(a.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/storage" {
			close(patchArrived)
			a.storageHandler(w, r)
			return
		}
		a.browserSessionHandler(w, r)
	})))
	server.Start()
	t.Cleanup(server.Close)
	a.SetAddress(server.Listener.Addr().String())
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/session", nil)
	request.Header.Set("X-Local-Token", testToken)
	session, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Body.Close()
	if session.StatusCode != http.StatusOK {
		t.Fatalf("session status: %d", session.StatusCode)
	}
	// Pause the accepted patch at its disk lock while the last tab closes.
	a.uiMu.Lock()
	var unlock sync.Once
	releaseWrite := func() { unlock.Do(a.uiMu.Unlock) }
	defer releaseWrite()
	patchResult := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPatch, server.URL+"/api/storage", strings.NewReader(`{"key":"closing-draft","baseRevision":"","prefixBytes":0,"suffixBytes":0,"insert":"{\"content\":\"最後の文字\"}"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Local-Token", testToken)
		response, err := server.Client().Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = errors.New("draft patch was rejected during shutdown")
			}
		}
		patchResult <- err
	}()
	waitForBrowserEvent(t, patchArrived, "draft patch to arrive")
	session.Body.Close()
	waitForBrowserEvent(t, stopped, "last tab to start shutdown")
	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { shutdownDone <- server.Config.Shutdown(ctx) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("server stopped before the accepted patch finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	releaseWrite()
	if err := <-patchResult; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	path, _ := a.storagePath("closing-draft")
	value, err := readStoredUIValue(path, "closing-draft")
	if err != nil || string(value) != `{"content":"最後の文字"}` {
		t.Fatalf("draft after shutdown: %s, %v", value, err)
	}
}
