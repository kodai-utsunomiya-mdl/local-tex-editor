package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

type activeInstance struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

func (instance activeInstance) browserURL() string {
	return instance.URL + "#token=" + url.QueryEscape(instance.Token)
}

func validInstance(instance activeInstance) bool {
	if len(instance.Token) < 32 || len(instance.Token) > 1024 {
		return false
	}
	address, err := url.Parse(instance.URL)
	if err != nil || address.Scheme != "http" || address.User != nil || address.Path != "/" || address.RawQuery != "" || address.Fragment != "" {
		return false
	}
	ip := net.ParseIP(address.Hostname())
	return ip != nil && ip.IsLoopback() && address.Port() != ""
}

func instanceClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func readActiveInstance(ctx context.Context, configDir string, client *http.Client) (activeInstance, error) {
	var instance activeInstance
	data, err := os.ReadFile(filepath.Join(configDir, "active.json"))
	if err != nil {
		return instance, err
	}
	if len(data) > 16<<10 || json.Unmarshal(data, &instance) != nil || !validInstance(instance) {
		return activeInstance{}, errors.New("起動中のアプリの接続情報を読み取れません．")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, instance.URL+"api/state", nil)
	if err != nil {
		return activeInstance{}, err
	}
	request.Header.Set("X-Local-Token", instance.Token)
	response, err := client.Do(request)
	if err != nil {
		return activeInstance{}, err
	}
	defer response.Body.Close()
	var state struct {
		Version string `json:"version"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&state) != nil || state.Version == "" || len(state.Version) > 128 {
		return activeInstance{}, errors.New("起動中のアプリのバージョンを確認できません．")
	}
	// The response comes from the process that will serve the editing screen.
	// active.json can still contain the previous process while it is exiting.
	instance.Version = state.Version
	return instance, nil
}

// Acquiring and releasing the Windows mutex must happen in this goroutine.
// A different version gets its own waiting page until the old editor exits.
func acquireStartupInstance(ctx context.Context, configDir, readyFile string, noBrowser bool, openBrowser func(string) error) (func(), *updatePage, error) {
	deadline := time.Now().Add(5 * time.Second)
	client := instanceClient(time.Second)
	for {
		if ctx.Err() != nil {
			return nil, nil, nil
		}
		release, err := platform.AcquireAppLock(configDir)
		if err == nil {
			return release, nil, nil
		}
		if !errors.Is(err, platform.ErrLocked) || (noBrowser && readyFile == "") {
			return nil, nil, err
		}
		instance, readErr := readActiveInstance(ctx, configDir, client)
		if readErr == nil {
			if instance.Version == version {
				if noBrowser {
					return nil, nil, err
				}
				return nil, nil, openBrowser(instance.browserURL())
			}
			executable, err := os.Executable()
			if err != nil {
				return nil, nil, err
			}
			page, err := startUpdatePage(instance, version, executable, configDir, readyFile, noBrowser, openBrowser)
			if err != nil {
				return nil, nil, err
			}
			for {
				select {
				case <-ctx.Done():
					page.Close()
					return nil, nil, nil
				case <-page.cancelled:
					page.Close()
					return nil, nil, nil
				default:
				}
				release, err := platform.AcquireAppLock(configDir)
				if err == nil {
					if page.Cancelled() || ctx.Err() != nil {
						release()
						page.Close()
						return nil, nil, nil
					}
					page.Starting()
					return release, page, nil
				}
				if !errors.Is(err, platform.ErrLocked) {
					page.Close()
					return nil, nil, err
				}
				select {
				case <-ctx.Done():
				case <-page.cancelled:
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("Local TeX Editor は既に起動していますが，接続を確認できません．開いている編集画面から終了して，再度起動してください．%w", readErr)
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
