package app

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

func (a *App) pickHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// net/http starts watching for a disconnected HTTP/1.1 client after the
	// request body reaches EOF. The browser sends an empty JSON object here.
	if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 4096)); err != nil {
		writeError(w, fail(http.StatusBadRequest, "フォルダー選択のリクエストを読み取れません．"))
		return
	}
	path, err := platform.PickFolder(r.Context())
	if errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}
