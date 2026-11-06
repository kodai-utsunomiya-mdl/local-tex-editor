package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDraftSurvivesRestartWithADifferentBrowserOrigin(t *testing.T) {
	a, root := testApp(t)
	key := "drafts:" + root
	value := map[string]any{"path": "main.tex", "revision": "before-edit", "content": "日本語の未保存原稿\n\\section{続きを書く}\n"}
	response := apiRequest(a, http.MethodPut, "/api/storage", map[string]any{"key": key, "value": value}, nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	token := "a-new-session-token-with-32-characters"
	restarted, err := New(Options{ConfigDir: a.options.ConfigDir, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.SetAddress("127.0.0.1:9911")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9911/api/storage?key="+url.QueryEscape(key), nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("X-Local-Token", token)
	request.Header.Set("Origin", "http://127.0.0.1:9911")
	result := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(result, request)
	var decoded struct {
		Value map[string]any `json:"value"`
	}
	if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &decoded) != nil || decoded.Value["content"] != value["content"] {
		t.Fatalf("draft after restart: %d %s", result.Code, result.Body.String())
	}
}

func TestStorageTreatsKeysAsDataAndSupportsDeletion(t *testing.T) {
	a, _ := testApp(t)
	key := "../outside/日本語の原稿"
	response := apiRequest(a, http.MethodPut, "/api/storage", map[string]any{"key": key, "value": false}, nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(a.options.ConfigDir, "ui-state"))
	if err != nil || len(entries) != 1 || strings.Contains(entries[0].Name(), "outside") {
		t.Fatalf("unexpected storage entries: %v, %v", entries, err)
	}
	endpoint := "/api/storage?key=" + url.QueryEscape(key)
	response = apiRequest(a, http.MethodGet, endpoint, nil, nil)
	stored := decodeUIStorageResponse(t, response, http.StatusOK)
	if string(stored.Value) != "false" || stored.Serialized != "false" || stored.Revision == "" {
		t.Fatalf("stored false: %s", response.Body.String())
	}
	response = apiRequest(a, http.MethodDelete, endpoint, nil, nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	response = apiRequest(a, http.MethodGet, endpoint, nil, nil)
	stored = decodeUIStorageResponse(t, response, http.StatusOK)
	if string(stored.Value) != "null" || stored.Serialized != "" || stored.Revision != "" {
		t.Fatalf("deleted value: %s", response.Body.String())
	}
}

func decodeUIStorageResponse(t *testing.T, response *httptest.ResponseRecorder, status int) uiStorageResponse {
	t.Helper()
	if response.Code != status {
		t.Fatalf("storage response status %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var result uiStorageResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Serialized != "" {
		hash := sha256.Sum256([]byte(result.Serialized))
		if result.Revision != hex.EncodeToString(hash[:]) {
			t.Fatal("storage revision does not match the exact serialized bytes")
		}
	}
	return result
}

func TestStorageConditionalPutAndPatchPreserveNewestDraft(t *testing.T) {
	for _, putFirst := range []bool{true, false} {
		name := "patch arrives first"
		if putFirst {
			name = "put arrives first"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := testApp(t)
			key := "drafts:large manuscript"
			// The full draft cannot fit in a browser's 64 KiB keepalive budget.
			paragraphs := strings.Repeat("日本語の原稿 ", 10000)
			acknowledged := `{"version":1,"updatedAt":100,"entries":[{"content":"` + paragraphs + `元` + paragraphs + `","updatedAt":101,"selection":123}]}`
			inFlight := strings.NewReplacer(`"updatedAt":100`, `"updatedAt":200`, `"updatedAt":101`, `"updatedAt":201`, `"selection":123`, `"selection":234`, "元", "中").Replace(acknowledged)
			latest := strings.NewReplacer(`"updatedAt":100`, `"updatedAt":300`, `"updatedAt":101`, `"updatedAt":301`, `"selection":123`, `"selection":345`, "元", "終😀").Replace(acknowledged)
			response := apiRequest(a, http.MethodPut, "/api/storage", map[string]any{
				"key": key, "value": json.RawMessage(acknowledged), "baseRevision": "",
			}, nil)
			base := decodeUIStorageResponse(t, response, http.StatusOK)
			pending := uiStorageResult(json.RawMessage(inFlight))
			put := map[string]any{"key": key, "value": json.RawMessage(inFlight), "baseRevision": base.Revision}
			candidates := make([]uiStoragePatch, 0, 2)
			for index, original := range []string{acknowledged, inFlight} {
				candidate := uiStoragePatch{BaseRevision: []string{base.Revision, pending.Revision}[index]}
				changes := [][2]string{
					{[]string{`"updatedAt":100`, `"updatedAt":200`}[index], `"updatedAt":300`},
					{[]string{`"updatedAt":101`, `"updatedAt":201`}[index], `"updatedAt":301`},
					{[]string{`"selection":123`, `"selection":234`}[index], `"selection":345`},
					{[]string{"元", "中"}[index], "終😀"},
				}
				for _, change := range changes {
					start := strings.Index(original, change[0])
					candidate.Edits = append(candidate.Edits, uiStorageEdit{
						PrefixBytes: start, SuffixBytes: len(original) - start - len(change[0]), Insert: change[1],
					})
				}
				candidates = append(candidates, candidate)
			}
			patch := map[string]any{"key": key, "patches": candidates}
			payload, _ := json.Marshal(patch)
			if len(payload) >= 2048 {
				t.Fatalf("small edit produced %d bytes", len(payload))
			}
			if putFirst {
				decodeUIStorageResponse(t, apiRequest(a, http.MethodPut, "/api/storage", put, nil), http.StatusOK)
			}
			changed := decodeUIStorageResponse(t, apiRequest(a, http.MethodPatch, "/api/storage", patch, nil), http.StatusOK)
			if changed.Serialized != latest {
				t.Fatal("the close-time patch did not save the latest draft")
			}
			if !putFirst {
				conflict := decodeUIStorageResponse(t, apiRequest(a, http.MethodPut, "/api/storage", put, nil), http.StatusConflict)
				if conflict.Serialized != latest || conflict.Error == "" {
					t.Fatal("the older in-flight PUT overwrote the last edit")
				}
			}
			stored := decodeUIStorageResponse(t, apiRequest(a, http.MethodGet, "/api/storage?key="+url.QueryEscape(key), nil, nil), http.StatusOK)
			if stored.Serialized != latest {
				t.Fatal("the saved draft changed after resolving competing requests")
			}
		})
	}
}

func TestStorageRevisionsMatchBrowserJSONIncludingLegacyEscapes(t *testing.T) {
	a, _ := testApp(t)
	key := "drafts:legacy"
	path, err := a.storagePath(key)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"key":"drafts:legacy","value": {"content":"<>&日本語😀\u2028\u2029\\u003c","label":"keep order"}}`)
	legacy = []byte(strings.ReplaceAll(string(legacy), "<>&", `\u003c\u003e\u0026`))
	writeTestFile(t, path, legacy)
	want := "{\"content\":\"<>&日本語😀\u2028\u2029\\\\u003c\",\"label\":\"keep order\"}"
	response := apiRequest(a, http.MethodGet, "/api/storage?key="+url.QueryEscape(key), nil, nil)
	stored := decodeUIStorageResponse(t, response, http.StatusOK)
	if stored.Serialized != want {
		t.Fatalf("legacy normalization: %q, want %q", stored.Serialized, want)
	}
	// Normal PUT uses JSON.stringify's bytes, including literal line separators.
	response = apiRequest(a, http.MethodPut, "/api/storage", map[string]any{
		"key": key, "value": json.RawMessage(want), "baseRevision": stored.Revision,
	}, nil)
	written := decodeUIStorageResponse(t, response, http.StatusOK)
	if written.Serialized != want || written.Revision != stored.Revision {
		t.Fatal("writing the unchanged value changed its revision")
	}
	response = apiRequest(a, http.MethodGet, "/api/storage?key="+url.QueryEscape(key), nil, nil)
	if decodeUIStorageResponse(t, response, http.StatusOK).Serialized != want {
		t.Fatal("JSON encoding changed the saved bytes after a read")
	}
}

func TestStoragePatchCreatesMissingValueAndRejectsStaleRevision(t *testing.T) {
	a, _ := testApp(t)
	key := "drafts:new"
	missing := decodeUIStorageResponse(t, apiRequest(a, http.MethodGet, "/api/storage?key="+key, nil, nil), http.StatusOK)
	if missing.Revision != "" || missing.Serialized != "" {
		t.Fatal("missing value has a revision")
	}
	patch := map[string]any{"key": key, "baseRevision": "", "prefixBytes": 0, "suffixBytes": 0, "insert": `{"content":"新しい原稿"}`}
	created := decodeUIStorageResponse(t, apiRequest(a, http.MethodPatch, "/api/storage", patch, nil), http.StatusOK)
	conflict := decodeUIStorageResponse(t, apiRequest(a, http.MethodPatch, "/api/storage", patch, nil), http.StatusConflict)
	if conflict.Serialized != created.Serialized || conflict.Error == "" {
		t.Fatal("a stale patch did not report the current value")
	}
	response := apiRequest(a, http.MethodPut, "/api/storage", map[string]any{"key": key, "value": false, "baseRevision": ""}, nil)
	if decodeUIStorageResponse(t, response, http.StatusConflict).Serialized != created.Serialized {
		t.Fatal("conditional creation replaced an existing value")
	}
}

func TestStorageRejectsInvalidPatchWithoutChangingDraft(t *testing.T) {
	a, _ := testApp(t)
	key := "drafts:validation"
	base := `{"content":"原稿😀"}`
	stored := decodeUIStorageResponse(t, apiRequest(a, http.MethodPut, "/api/storage", map[string]any{
		"key": key, "value": json.RawMessage(base),
	}, nil), http.StatusOK)
	for _, test := range []struct {
		name  string
		patch uiStoragePatch
	}{
		{"negative prefix", uiStoragePatch{PrefixBytes: -1, Insert: `{}`}},
		{"prefix past end", uiStoragePatch{PrefixBytes: len(base) + 1}},
		{"overlapping suffix", uiStoragePatch{PrefixBytes: len(base), SuffixBytes: 1}},
		{"prefix inside UTF8", uiStoragePatch{PrefixBytes: len(`{"content":"`) + 1, SuffixBytes: 2}},
		{"suffix inside UTF8", uiStoragePatch{PrefixBytes: len(`{"content":"`), SuffixBytes: 3}},
		{"invalid JSON", uiStoragePatch{Insert: `{"content":`}},
		{"overlapping edits", uiStoragePatch{Edits: []uiStorageEdit{{PrefixBytes: 0, SuffixBytes: 2}, {PrefixBytes: 1, SuffixBytes: 1}}}},
		{"too many edits", uiStoragePatch{Edits: make([]uiStorageEdit, 257)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.patch.BaseRevision = stored.Revision
			response := apiRequest(a, http.MethodPatch, "/api/storage", map[string]any{"key": key, "patches": []uiStoragePatch{test.patch}}, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid patch response: %d %s", response.Code, response.Body.String())
			}
		})
	}
	for _, patches := range [][]uiStoragePatch{{}, {{}, {}, {}}} {
		response := apiRequest(a, http.MethodPatch, "/api/storage", map[string]any{"key": key, "patches": patches}, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid number of candidates: %d", response.Code)
		}
	}
	result := decodeUIStorageResponse(t, apiRequest(a, http.MethodGet, "/api/storage?key="+key, nil, nil), http.StatusOK)
	if result.Serialized != base || result.Revision != stored.Revision {
		t.Fatal("invalid patches changed the saved draft")
	}
	if _, err := applyUIStoragePatch(json.RawMessage(base), uiStoragePatch{Insert: string([]byte{0xff})}); err == nil {
		t.Fatal("invalid UTF-8 insert accepted")
	}
	if _, err := applyUIStoragePatch(nil, uiStoragePatch{Insert: `"` + strings.Repeat("x", 2*maxTextSize) + `"`}); err == nil {
		t.Fatal("oversize result accepted")
	}
	if !utf8.ValidString(result.Serialized) {
		t.Fatal("the saved JSON contains invalid UTF-8")
	}
}
