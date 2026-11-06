package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadActiveInstanceUsesVersionOfRunningServer(t *testing.T) {
	token := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state" || r.Header.Get("X-Local-Token") != token {
			t.Error("unexpected request for the running application")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"version":"0.1.2"}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	data, _ := json.Marshal(activeInstance{URL: server.URL + "/", Token: token, Version: "0.1.1"})
	if err := os.WriteFile(filepath.Join(dir, "active.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	instance, err := readActiveInstance(context.Background(), dir, instanceClient(time.Second))
	if err != nil || instance.Version != "0.1.2" {
		t.Fatalf("version = %q, error = %v", instance.Version, err)
	}
}

func TestActiveInstanceRejectsUnsafeURLs(t *testing.T) {
	for _, address := range []string{
		"https://127.0.0.1:1234/", "http://example.com:1234/", "http://127.0.0.1:1234.evil.example/",
		"http://user@127.0.0.1:1234/", "http://127.0.0.1:1234/project/", "http://127.0.0.1:1234/?query=1",
		"http://127.0.0.1:1234/#fragment", "http://127.0.0.1/", "http://localhost:1234/",
	} {
		t.Run(address, func(t *testing.T) {
			if validInstance(activeInstance{URL: address, Token: strings.Repeat("a", 64)}) {
				t.Fatal("accepted an address outside the expected loopback form")
			}
		})
	}
}

func TestReadActiveInstanceDoesNotForwardTokenAcrossRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/api/state", http.StatusFound)
	}))
	defer server.Close()
	dir := t.TempDir()
	data, _ := json.Marshal(activeInstance{URL: server.URL + "/", Token: strings.Repeat("a", 64)})
	if err := os.WriteFile(filepath.Join(dir, "active.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readActiveInstance(context.Background(), dir, instanceClient(time.Second))
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("error = %v, redirected calls = %d", err, redirected.Load())
	}
}
