package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Browser origins change when the local server chooses a new port. Keep drafts
// and preferences beside the app's settings so they survive that change.
type storedUIValue struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type uiStorageResponse struct {
	Value      json.RawMessage `json:"value"`
	Serialized string          `json:"serialized"`
	Revision   string          `json:"revision"`
	Error      string          `json:"error,omitempty"`
}

type uiStoragePatch struct {
	BaseRevision string          `json:"baseRevision"`
	PrefixBytes  int             `json:"prefixBytes"`
	SuffixBytes  int             `json:"suffixBytes"`
	Insert       string          `json:"insert"`
	Edits        []uiStorageEdit `json:"edits,omitempty"`
}

type uiStorageEdit struct {
	PrefixBytes int    `json:"prefixBytes"`
	SuffixBytes int    `json:"suffixBytes"`
	Insert      string `json:"insert"`
}

func uiStorageResult(value json.RawMessage) uiStorageResponse {
	if len(value) == 0 {
		return uiStorageResponse{Value: json.RawMessage("null")}
	}
	hash := sha256.Sum256(value)
	return uiStorageResponse{Value: value, Serialized: string(value), Revision: hex.EncodeToString(hash[:])}
}

// Existing files may use encoding/json's escapes for HTML characters and line
// separators. Match JSON.stringify without changing key order or string data,
// so a small edit to an older draft also produces a small browser-close patch.
func compactUIValue(value []byte) (json.RawMessage, error) {
	if !utf8.Valid(value) {
		return nil, fail(http.StatusBadRequest, "UTF-8 のテキストを送信してください．")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return nil, fail(http.StatusBadRequest, "保存する JSON を読み取れません: %v", err)
	}
	data := compact.Bytes()
	result := make([]byte, 0, len(data))
	for index := 0; index < len(data); {
		if data[index] != '\\' || index+1 >= len(data) {
			result = append(result, data[index])
			index++
			continue
		}
		if data[index+1] == 'u' && index+6 <= len(data) {
			code, err := strconv.ParseUint(string(data[index+2:index+6]), 16, 16)
			if err == nil && (code == '<' || code == '>' || code == '&' || code == 0x2028 || code == 0x2029) {
				result = utf8.AppendRune(result, rune(code))
				index += 6
				continue
			}
		}
		// Copy a complete escape. In particular, a literal backslash followed
		// by "u003c" must remain the user's text rather than becoming "<".
		result = append(result, data[index], data[index+1])
		index += 2
	}
	return result, nil
}

func readStoredUIValue(path, key string) (json.RawMessage, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 2*maxTextSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2*maxTextSize {
		return nil, fail(http.StatusRequestEntityTooLarge, "保存した設定が大きすぎます．")
	}
	var record storedUIValue
	if !utf8.Valid(data) || json.Unmarshal(data, &record) != nil || record.Key != key || len(record.Value) == 0 {
		return nil, fail(http.StatusInternalServerError, "保存した設定を読み取れません．")
	}
	return compactUIValue(record.Value)
}

func writeStoredUIValue(path, key string, value json.RawMessage) error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(storedUIValue{Key: key, Value: value}); err != nil {
		return err
	}
	if data.Len() > 2*maxTextSize {
		return fail(http.StatusRequestEntityTooLarge, "保存する設定が大きすぎます．")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeAtomic(path, data.Bytes(), 0600)
}

func applyUIStoragePatch(base json.RawMessage, patch uiStoragePatch) (json.RawMessage, error) {
	edits := patch.Edits
	if edits == nil {
		edits = []uiStorageEdit{{PrefixBytes: patch.PrefixBytes, SuffixBytes: patch.SuffixBytes, Insert: patch.Insert}}
	}
	if len(edits) < 1 || len(edits) > 256 {
		return nil, fail(http.StatusBadRequest, "草稿の変更範囲は 1 個から 256 個まで指定できます．")
	}
	edits = append([]uiStorageEdit(nil), edits...)
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].PrefixBytes < edits[j].PrefixBytes })
	previousEnd, resultSize := 0, len(base)
	for _, edit := range edits {
		if edit.PrefixBytes < previousEnd || edit.PrefixBytes > len(base) || edit.SuffixBytes < 0 || edit.SuffixBytes > len(base)-edit.PrefixBytes {
			return nil, fail(http.StatusBadRequest, "草稿の変更範囲が重なっているか，読み取れません．")
		}
		end := len(base) - edit.SuffixBytes
		if (edit.PrefixBytes < len(base) && !utf8.RuneStart(base[edit.PrefixBytes])) || (end < len(base) && !utf8.RuneStart(base[end])) || !utf8.ValidString(edit.Insert) {
			return nil, fail(http.StatusBadRequest, "UTF-8 の文字の途中を変更範囲に指定できません．")
		}
		resultSize += len(edit.Insert) - (end - edit.PrefixBytes)
		previousEnd = end
	}
	if resultSize > 2*maxTextSize {
		return nil, fail(http.StatusRequestEntityTooLarge, "保存する設定が大きすぎます．")
	}
	value := make([]byte, 0, resultSize)
	previousEnd = 0
	for _, edit := range edits {
		value = append(value, base[previousEnd:edit.PrefixBytes]...)
		value = append(value, edit.Insert...)
		previousEnd = len(base) - edit.SuffixBytes
	}
	value = append(value, base[previousEnd:]...)
	return compactUIValue(value)
}

func (a *App) storagePath(key string) (string, error) {
	if len(key) == 0 || len(key) > 8192 {
		return "", fail(http.StatusBadRequest, "設定のキーは 1～8192 バイトで指定してください．")
	}
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(a.options.ConfigDir, "ui-state", hex.EncodeToString(hash[:])+".json"), nil
}

func (a *App) storageHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete) {
		return
	}
	key := r.URL.Query().Get("key")
	var record struct {
		storedUIValue
		BaseRevision *string `json:"baseRevision"`
	}
	var patches []uiStoragePatch
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
	if r.Method == http.MethodPatch {
		var input struct {
			Key string `json:"key"`
			uiStoragePatch
			Patches []uiStoragePatch `json:"patches"`
		}
		if err := readJSON(w, r, &input); err != nil {
			writeError(w, err)
			return
		}
		key = input.Key
		patches = input.Patches
		if patches == nil {
			patches = []uiStoragePatch{input.uiStoragePatch}
		}
		if len(patches) < 1 || len(patches) > 2 {
			writeError(w, fail(http.StatusBadRequest, "草稿の変更は 1 個または 2 個の候補で送信してください．"))
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
	var current json.RawMessage
	if r.Method != http.MethodDelete {
		current, err = readStoredUIValue(path, key)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	result := uiStorageResult(current)
	conflict := func() {
		result.Error = "保存済みの内容が変わりました．最新の草稿を確認してから再度保存してください．"
		writeJSON(w, http.StatusConflict, result)
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, result)
	case http.MethodPut:
		if record.BaseRevision != nil && *record.BaseRevision != result.Revision {
			conflict()
			return
		}
		value, err := compactUIValue(record.Value)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := writeStoredUIValue(path, key, value); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, uiStorageResult(value))
	case http.MethodPatch:
		var matching *uiStoragePatch
		for index := range patches {
			if patches[index].BaseRevision == result.Revision {
				matching = &patches[index]
				break
			}
		}
		if matching == nil {
			conflict()
			return
		}
		value, err := applyUIStoragePatch(current, *matching)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := writeStoredUIValue(path, key, value); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, uiStorageResult(value))
	case http.MethodDelete:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
