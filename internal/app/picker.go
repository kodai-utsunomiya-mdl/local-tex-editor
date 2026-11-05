package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

func (a *App) pickHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
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
