package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Browser origins change when the local server chooses a new port. Keep drafts
// and preferences beside the app's settings so they survive that change.
type storedUIValue struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func (a *App) storagePath(key string) (string, error) {
	if len(key) == 0 || len(key) > 8192 {
		return "", fail(http.StatusBadRequest, "設定のキーは 1～8192 バイトで指定してください．")
	}
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(a.options.ConfigDir, "ui-state", hex.EncodeToString(hash[:])+".json"), nil
}

func (a *App) storageHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPut, http.MethodDelete) {
		return
	}
	key := r.URL.Query().Get("key")
	var record storedUIValue
	if r.Method == http.MethodPut {
		if err := readJSON(w, r, &record); err != nil {
			writeError(w, err)
			return
		}
		key = record.Key
		if len(record.Value) == 0 {
			writeError(w, fail(http.StatusBadRequest, "保存する value を指定してください．"))
			return
		}
	}
	path, err := a.storagePath(key)
	if err != nil {
		writeError(w, err)
		return
	}
	a.uiMu.Lock()
	defer a.uiMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"value": nil})
			return
		}
		if err != nil {
			writeError(w, err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 2*maxTextSize+1))
		if err != nil {
			writeError(w, err)
			return
		}
		if len(data) > 2*maxTextSize {
			writeError(w, fail(http.StatusRequestEntityTooLarge, "保存した設定が大きすぎます．"))
			return
		}
		if err := json.Unmarshal(data, &record); err != nil || record.Key != key || len(record.Value) == 0 {
			writeError(w, fail(http.StatusInternalServerError, "保存した設定を読み取れません．"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"value": record.Value})
	case http.MethodPut:
		data, err := json.Marshal(record)
		if err != nil {
			writeError(w, err)
			return
		}
		if len(data) > 2*maxTextSize {
			writeError(w, fail(http.StatusRequestEntityTooLarge, "保存する設定が大きすぎます．"))
			return
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
			err = writeAtomic(path, data, 0600)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"value": record.Value})
	case http.MethodDelete:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
