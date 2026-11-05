package app

import (
	"errors"
	"net/http"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

func (a *App) shortcutsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var input map[string]string
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	destination := input["destination"]
	if len(input) != 1 || (destination != "start-menu" && destination != "desktop") {
		writeError(w, fail(http.StatusBadRequest, "作成先に start-menu または desktop を指定してください．"))
		return
	}
	if !a.shortcutMu.TryLock() {
		writeError(w, fail(http.StatusConflict, "ショートカットを作成しています．処理が終わるまでお待ちください．"))
		return
	}
	defer a.shortcutMu.Unlock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		writeError(w, fail(http.StatusServiceUnavailable, "アプリを起動し直してください．"))
		return
	}
	a.workers.Add(1)
	create := a.shortcutCreator
	a.mu.Unlock()
	defer a.workers.Done()
	if create == nil {
		create = platform.CreateShortcut
	}
	result, err := create(destination)
	if err != nil {
		switch {
		case errors.Is(err, platform.ErrShortcutsUnsupported):
			err = fail(http.StatusNotImplemented, "%v", err)
		case errors.Is(err, platform.ErrShortcutConflict), errors.Is(err, platform.ErrLocked):
			err = fail(http.StatusConflict, "%v", err)
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
