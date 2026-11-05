package app

import (
	"encoding/json"
	"net/http"
	"runtime"
	"testing"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

func TestShortcutAPIValidatesTheDestinationAndAuthentication(t *testing.T) {
	a, _ := testApp(t)
	called := []string{}
	a.shortcutCreator = func(destination string) (platform.ShortcutResult, error) {
		called = append(called, destination)
		return platform.ShortcutResult{Executable: "installed executable", Shortcut: destination + "/Local TeX Editor.lnk", Message: "ready"}, nil
	}
	for _, test := range []struct {
		name    string
		body    map[string]string
		headers map[string]string
		want    int
	}{
		{"no token", map[string]string{"destination": "desktop"}, map[string]string{"X-Local-Token": ""}, 401},
		{"another origin", map[string]string{"destination": "desktop"}, map[string]string{"Origin": "https://example.test"}, 403},
		{"missing destination", map[string]string{}, nil, 400},
		{"arbitrary destination", map[string]string{"destination": "../outside"}, nil, 400},
		{"arbitrary output", map[string]string{"destination": "desktop", "path": "outside.lnk"}, nil, 400},
		{"arbitrary executable", map[string]string{"destination": "start-menu", "executable": "other.exe"}, nil, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := apiRequest(a, http.MethodPost, "/api/shortcuts", test.body, test.headers)
			if response.Code != test.want {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
	if len(called) != 0 {
		t.Fatalf("invalid requests reached the creator: %v", called)
	}
	for _, destination := range []string{"start-menu", "desktop"} {
		response := apiRequest(a, http.MethodPost, "/api/shortcuts", map[string]string{"destination": destination}, map[string]string{"X-Project-Root": "an-old-project"})
		var result platform.ShortcutResult
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Executable != "installed executable" || result.Shortcut != destination+"/Local TeX Editor.lnk" {
			t.Fatalf("shortcut response: %d %s", response.Code, response.Body.String())
		}
	}
	if len(called) != 2 || called[0] != "start-menu" || called[1] != "desktop" {
		t.Fatalf("unexpected destinations: %v", called)
	}
	response := apiRequest(a, http.MethodGet, "/api/state", nil, nil)
	var state State
	if json.Unmarshal(response.Body.Bytes(), &state) != nil || state.ShortcutsSupported != (runtime.GOOS == "windows") {
		t.Fatalf("shortcut support: %s", response.Body.String())
	}
}

func TestShortcutAPIReportsUnavailableAndConflictingDestinations(t *testing.T) {
	a, _ := testApp(t)
	for _, test := range []struct {
		err  error
		want int
	}{{platform.ErrShortcutsUnsupported, 501}, {platform.ErrShortcutConflict, 409}, {platform.ErrLocked, 409}} {
		a.shortcutCreator = func(_ string) (platform.ShortcutResult, error) { return platform.ShortcutResult{}, test.err }
		response := apiRequest(a, http.MethodPost, "/api/shortcuts", map[string]string{"destination": "desktop"}, nil)
		if response.Code != test.want {
			t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
		}
	}
}
