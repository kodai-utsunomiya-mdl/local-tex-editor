package app

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

// Each editor tab keeps an authenticated response open. Its request context
// ends when the tab closes, including when its unload JavaScript cannot run.
func (a *App) browserSessionHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, fail(http.StatusInternalServerError, "ブラウザとの接続を開始できません．"))
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		writeError(w, fail(http.StatusServiceUnavailable, "アプリを起動し直してください．"))
		return
	}
	a.browserConnections++
	a.browserEverConnected = true
	a.browserGeneration++
	if a.browserTimer != nil {
		a.browserTimer.Stop()
		a.browserTimer = nil
	}
	end := a.browserEnd
	a.mu.Unlock()
	defer a.browserDisconnected()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	writeComment := func(comment string) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := io.WriteString(w, ": "+comment+"\n\n"); err != nil {
			return false
		}
		if err := controller.Flush(); err != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Time{})
		return true
	}
	if !writeComment("connected") {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-end:
			return
		case <-ticker.C:
			if !writeComment("keepalive") {
				return
			}
		}
	}
}

func (a *App) browserDisconnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.browserConnections--
	if a.closed || a.browserConnections != 0 {
		return
	}
	a.scheduleBrowserShutdownLocked(a.browserCloseDelay)
}

// ExpectBrowserConnection bounds the first connection wait after opening the
// editor or completing an update. API-only launches do not call this method.
func (a *App) ExpectBrowserConnection(timeout time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.browserEverConnected || a.browserTimer != nil {
		return
	}
	a.scheduleBrowserShutdownLocked(timeout)
}

func (a *App) scheduleBrowserShutdownLocked(delay time.Duration) {
	a.browserGeneration++
	generation := a.browserGeneration
	a.browserTimer = time.AfterFunc(delay, func() {
		a.mu.Lock()
		if a.closed || a.browserConnections != 0 || a.browserGeneration != generation {
			a.mu.Unlock()
			return
		}
		// Claim the shutdown while holding the same mutex used by new tabs.
		// A reload that already reconnected cancels this pending shutdown.
		a.closeBrowserSessionsLocked()
		a.mu.Unlock()
		a.requestShutdown()
	})
}

// CloseBrowserSessions ends the open responses before http.Server.Shutdown
// waits for handlers. The application no longer accepts requests after this.
func (a *App) CloseBrowserSessions() {
	a.mu.Lock()
	a.closeBrowserSessionsLocked()
	a.mu.Unlock()
}

func (a *App) closeBrowserSessionsLocked() {
	a.closed = true
	a.browserGeneration++
	if a.browserTimer != nil {
		a.browserTimer.Stop()
		a.browserTimer = nil
	}
	if a.browserEnd != nil {
		close(a.browserEnd)
		a.browserEnd = nil
	}
}

func (a *App) requestShutdown() {
	a.shutdownOnce.Do(func() {
		a.CloseBrowserSessions()
		platform.CancelFolderPicker()
		_ = a.stopBuild()
		if a.options.Shutdown != nil {
			a.options.Shutdown()
		}
	})
}
