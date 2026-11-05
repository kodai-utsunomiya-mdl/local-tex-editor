//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
)

func waitForAppFolderWindow(t *testing.T, exists bool) uintptr {
	t.Helper()
	user32 := syscall.NewLazyDLL("user32.dll")
	title := syscall.StringToUTF16Ptr("TeX のプロジェクトフォルダーを選択")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var previous, found uintptr
		for {
			hwnd, _, _ := user32.NewProc("FindWindowExW").Call(0, previous, 0, uintptr(unsafe.Pointer(title)))
			if hwnd == 0 {
				break
			}
			previous = hwnd
			var pid uint32
			user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			visible, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd)
			if pid == uint32(os.Getpid()) && visible != 0 {
				found = hwnd
				break
			}
		}
		if (found != 0) == exists {
			return found
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("native folder window did not reach visible=%v", exists)
	return 0
}

func TestNativeFolderPickerClosesWhenHTTPDisconnects(t *testing.T) {
	if os.Getenv("LOCAL_TEX_EDITOR_TEST_NATIVE_PICKER") != "1" {
		t.Skip("set LOCAL_TEX_EDITOR_TEST_NATIVE_PICKER=1 on an interactive Windows runner")
	}
	a, _ := testApp(t)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	defer platform.CancelFolderPicker()
	a.SetAddress(strings.TrimPrefix(server.URL, "http://"))
	requestContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(requestContext, http.MethodPost, server.URL+"/api/project/pick", nil)
		request.Header.Set("X-Local-Token", testToken)
		response, err := server.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		finished <- err
	}()
	waitForAppFolderWindow(t, true)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the aborted HTTP request returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the HTTP request did not stop")
	}
	waitForAppFolderWindow(t, false)
	t.Log("disconnecting the authenticated HTTP request closed the native dialog")
	requestContext, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		request, _ := http.NewRequestWithContext(requestContext, http.MethodPost, server.URL+"/api/project/pick", nil)
		request.Header.Set("X-Local-Token", testToken)
		response, err := server.Client().Do(request)
		if err == nil {
			defer response.Body.Close()
			var value map[string]string
			err = json.NewDecoder(response.Body).Decode(&value)
			if response.StatusCode != http.StatusOK || value["path"] != "" {
				err = errors.New("native cancellation did not return an empty path with HTTP 200")
			}
		}
		finished <- err
	}()
	hwnd := waitForAppFolderWindow(t, true)
	syscall.NewLazyDLL("user32.dll").NewProc("PostMessageW").Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second HTTP request did not finish after closing the dialog")
	}
	waitForAppFolderWindow(t, false)
	t.Log("the next HTTP request opened a fresh dialog and returned its cancellation")
}
