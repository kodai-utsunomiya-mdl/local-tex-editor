//go:build windows

package platform

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type testWindowMessage struct {
	hwnd           uintptr
	message        uint32
	wparam, lparam uintptr
	time           uint32
	x, y           int32
	private        uint32
}

var testOwnerCallback = syscall.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	if message == 0x0002 { // WM_DESTROY
		user32.NewProc("PostQuitMessage").Call(0)
		return 0
	}
	result, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, uintptr(message), wparam, lparam)
	return result
})

func nativeTestOwner(t *testing.T, title string) uintptr {
	t.Helper()
	ready := make(chan uintptr, 1)
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
		name := syscall.StringToUTF16Ptr(title)
		class := nativeWindowClass{procedure: testOwnerCallback, instance: instance, className: name}
		class.size = uint32(unsafe.Sizeof(class))
		atom, _, _ := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
		if atom == 0 {
			ready <- 0
			return
		}
		defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(name)), instance)
		hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), 0x00cf0000,
			100, 100, 640, 480, 0, 0, instance, 0)
		if hwnd == 0 {
			ready <- 0
			return
		}
		user32.NewProc("ShowWindow").Call(hwnd, 5)
		bringFolderDialogForward(hwnd)
		ready <- hwnd
		var message testWindowMessage
		for {
			result, _, _ := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
			if int32(result) <= 0 {
				return
			}
			user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&message)))
			user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&message)))
		}
	}()
	var hwnd uintptr
	select {
	case hwnd = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the owner window did not start")
	}
	if hwnd == 0 {
		t.Fatal("could not create a native owner window")
	}
	t.Cleanup(func() {
		user32.NewProc("PostMessageW").Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the native owner window did not close")
		}
	})
	return hwnd
}

func nativeFolderWindows() []uintptr {
	var windows []uintptr
	title := syscall.StringToUTF16Ptr(folderDialogTitle)
	var previous uintptr
	for {
		hwnd, _, _ := user32.NewProc("FindWindowExW").Call(0, previous, 0, uintptr(unsafe.Pointer(title)))
		if hwnd == 0 {
			break
		}
		previous = hwnd
		var pid uint32
		user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != uint32(os.Getpid()) {
			continue
		}
		visible, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd)
		if visible != 0 {
			windows = append(windows, hwnd)
		}
	}
	return windows
}

func waitForNativeFolder(t *testing.T, exists bool, foreground bool) uintptr {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		windows := nativeFolderWindows()
		if !exists && len(windows) == 0 {
			return 0
		}
		if exists && len(windows) == 1 {
			active, _, _ := user32.NewProc("GetForegroundWindow").Call()
			if !foreground || active == windows[0] {
				return windows[0]
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("native folder windows = %v; want visible=%v, foreground=%v", nativeFolderWindows(), exists, foreground)
	return 0
}

func TestNativeFolderPickerForegroundCancelAndReopen(t *testing.T) {
	if os.Getenv("LOCAL_TEX_EDITOR_TEST_NATIVE_PICKER") != "1" {
		t.Skip("set LOCAL_TEX_EDITOR_TEST_NATIVE_PICKER=1 on an interactive Windows runner")
	}
	owner := nativeTestOwner(t, "Local TeX Editor native owner")
	t.Cleanup(CancelFolderPicker)
	firstContext, cancelFirst := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelFirst()
	first := pickerAsync(&projectFolderPicker, firstContext)
	hwnd := waitForNativeFolder(t, true, true)
	actualOwner, _, _ := user32.NewProc("GetWindow").Call(hwnd, 4) // GW_OWNER
	outerOwner, _, _ := user32.NewProc("GetWindow").Call(actualOwner, 4)
	if outerOwner != owner {
		t.Fatalf("folder owner chain = %x -> %x, want foreground window %x", actualOwner, outerOwner, owner)
	}
	if enabled, _, _ := user32.NewProc("IsWindowEnabled").Call(owner); enabled == 0 {
		t.Fatal("the browser owner was disabled while the folder dialog was open")
	}
	t.Logf("the visible dialog %x is in front of its owner %x", hwnd, owner)
	_ = nativeTestOwner(t, "Local TeX Editor other window")
	secondContext, cancelSecond := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelSecond()
	second := pickerAsync(&projectFolderPicker, secondContext)
	if focused := waitForNativeFolder(t, true, true); focused != hwnd {
		t.Fatal("a repeated request opened another dialog")
	}
	cancelFirst()
	if result := pickerResult(t, first); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("first request = %+v", result)
	}
	waitForNativeFolder(t, true, false)
	cancelSecond()
	if result := pickerResult(t, second); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("second request = %+v", result)
	}
	waitForNativeFolder(t, false, false)
	t.Log("the last cancelled request closed the native dialog")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	third := pickerAsync(&projectFolderPicker, ctx)
	hwnd = waitForNativeFolder(t, true, true)
	user32.NewProc("PostMessageW").Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
	if result := pickerResult(t, third); result.path != "" || result.err != nil {
		t.Fatalf("native user cancellation = %+v", result)
	}
	waitForNativeFolder(t, false, false)
	t.Log("a new dialog opened after cancellation and closed through its own window")
}
