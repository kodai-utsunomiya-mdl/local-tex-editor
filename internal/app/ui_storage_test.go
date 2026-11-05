package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"value":false}` {
		t.Fatalf("stored false: %s", response.Body.String())
	}
	response = apiRequest(a, http.MethodDelete, endpoint, nil, nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	response = apiRequest(a, http.MethodGet, endpoint, nil, nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"value":null}` {
		t.Fatalf("deleted value: %s", response.Body.String())
	}
}
