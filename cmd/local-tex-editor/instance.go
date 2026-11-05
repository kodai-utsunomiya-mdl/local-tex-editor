package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

// A second double-click opens the active editor instead of starting another server.
func reopenExisting(configDir string) error {
	client := &http.Client{
		Timeout:       time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ready struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		data, err := os.ReadFile(filepath.Join(configDir, "active.json"))
		if err == nil && json.Unmarshal(data, &ready) == nil && len(ready.Token) >= 32 {
			address, err := url.Parse(ready.URL)
			if err == nil && address.Scheme == "http" && address.User == nil && address.Path == "/" && address.RawQuery == "" && address.Fragment == "" {
				ip := net.ParseIP(address.Hostname())
				if ip != nil && ip.IsLoopback() && address.Port() != "" {
					request, _ := http.NewRequest(http.MethodGet, ready.URL+"api/state", nil)
					request.Header.Set("X-Local-Token", ready.Token)
					response, err := client.Do(request)
					if err == nil {
						response.Body.Close()
						if response.StatusCode == http.StatusOK {
							return platform.OpenBrowser(ready.URL + "#token=" + url.QueryEscape(ready.Token))
						}
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Local TeX Editor は既に起動していますが，接続を確認できません．開いている編集画面から終了して，再度起動してください．")
}
