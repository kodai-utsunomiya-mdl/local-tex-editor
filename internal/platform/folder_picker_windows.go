//go:build windows

package platform

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	folderDialogTitle = "TeX のプロジェクトフォルダーを選択"
	hresultCancelled  = 0x800704c7
)

type nativeGUID struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

var (
	classFileOpenDialog = nativeGUID{0xdc1c5a9c, 0xe88a, 0x4dde, [8]byte{0xa5, 0xa1, 0x60, 0xf8, 0x2a, 0x20, 0xae, 0xf7}}
	iidFileOpenDialog   = nativeGUID{0xd57c7288, 0xd4ad, 0x4768, [8]byte{0xbe, 0x02, 0x9d, 0x96, 0x95, 0x32, 0xd9, 0x60}}
)

type nativeCOMObject struct {
	vtable *[29]uintptr
}

//go:uintptrescapes
func (object *nativeCOMObject) call(index int, arguments ...uintptr) uintptr {
	all := make([]uintptr, 1, len(arguments)+1)
	all[0] = uintptr(unsafe.Pointer(object))
	all = append(all, arguments...)
	result, _, _ := syscall.SyscallN(object.vtable[index], all...)
	return result
}

func folderDialogError(action string, result uintptr) error {
	return fmt.Errorf("フォルダーの選択画面を%sできません (HRESULT 0x%08X)．", action, uint32(result))
}

func showNativeFolderDialog(request *folderPickRequest) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := request.ctx.Err(); err != nil {
		return "", err
	}
	owner, _, _ := user32.NewProc("GetForegroundWindow").Call()
	result, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 2) // COINIT_APARTMENTTHREADED
	if int32(result) < 0 {
		return "", folderDialogError("初期化", result)
	}
	defer ole32.NewProc("CoUninitialize").Call()
	var dialog *nativeCOMObject
	result, _, _ = ole32.NewProc("CoCreateInstance").Call(
		uintptr(unsafe.Pointer(&classFileOpenDialog)), 0, 1,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dialog)))
	if int32(result) < 0 || dialog == nil {
		return "", folderDialogError("作成", result)
	}
	defer dialog.call(2) // IUnknown::Release
	var options uint32
	if result = dialog.call(10, uintptr(unsafe.Pointer(&options))); int32(result) < 0 {
		return "", folderDialogError("設定", result)
	}
	// FOS_PICKFOLDERS | FOS_FORCEFILESYSTEM | FOS_NOCHANGEDIR |
	// FOS_PATHMUSTEXIST. Keep the other defaults provided by Windows.
	if result = dialog.call(9, uintptr(options|0x20|0x40|0x8|0x800)); int32(result) < 0 {
		return "", folderDialogError("設定", result)
	}
	title, _ := syscall.UTF16PtrFromString(folderDialogTitle)
	if result = dialog.call(17, uintptr(unsafe.Pointer(title))); int32(result) < 0 {
		return "", folderDialogError("設定", result)
	}
	controller, err := newFolderDialogController(request, owner)
	if err != nil {
		return "", err
	}
	defer controller.destroy()
	stopForwarding := controller.forwardRequests()
	defer stopForwarding()
	if err := request.ctx.Err(); err != nil {
		return "", err
	}
	result = dialog.call(3, controller.hwnd) // IModalWindow::Show
	if uint32(result) == hresultCancelled {
		return "", nil
	}
	if int32(result) < 0 {
		return "", folderDialogError("表示", result)
	}
	var item *nativeCOMObject
	if result = dialog.call(20, uintptr(unsafe.Pointer(&item))); int32(result) < 0 || item == nil {
		return "", folderDialogError("読み取り", result)
	}
	defer item.call(2)
	var path *uint16
	if result = item.call(5, 0x80058000, uintptr(unsafe.Pointer(&path))); int32(result) < 0 || path == nil { // SIGDN_FILESYSPATH
		return "", errors.New("選択したフォルダーのパスを取得できません．")
	}
	defer ole32.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(path)))
	return nativeWideString(path), nil
}

func nativeWideString(text *uint16) string {
	if text == nil {
		return ""
	}
	length := 0
	for length < 32768 && *(*uint16)(unsafe.Add(unsafe.Pointer(text), length*2)) != 0 {
		length++
	}
	return syscall.UTF16ToString(unsafe.Slice(text, length))
}

type nativeWindowClass struct {
	size        uint32
	style       uint32
	procedure   uintptr
	classExtra  int32
	windowExtra int32
	instance    uintptr
	icon        uintptr
	cursor      uintptr
	background  uintptr
	menuName    *uint16
	className   *uint16
	smallIcon   uintptr
}

type folderDialogController struct {
	hwnd    uintptr
	request *folderPickRequest
}

var folderControllerClassOnce sync.Once
var folderControllerClassError error
var folderControllerClassName = syscall.StringToUTF16Ptr("LocalTeXEditor.FolderDialogController")
var folderControllerCallback = syscall.NewCallback(folderControllerWindowProcedure)

func registerFolderControllerClass() error {
	folderControllerClassOnce.Do(func() {
		instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
		class := nativeWindowClass{procedure: folderControllerCallback, instance: instance, className: folderControllerClassName}
		class.size = uint32(unsafe.Sizeof(class))
		atom, _, err := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
		if atom == 0 {
			folderControllerClassError = fmt.Errorf("フォルダーの選択画面を準備できません: %w", err)
		}
	})
	return folderControllerClassError
}

func newFolderDialogController(request *folderPickRequest, owner uintptr) (*folderDialogController, error) {
	if err := registerFolderControllerClass(); err != nil {
		return nil, err
	}
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	// The dialog disables this hidden owner, leaving the browser's Cancel and
	// Show-dialog buttons usable. Its ownership chain still places the dialog
	// above the browser.
	var bounds struct{ left, top, right, bottom int32 }
	if owner != 0 {
		user32.NewProc("GetWindowRect").Call(owner, uintptr(unsafe.Pointer(&bounds)))
	}
	if bounds.right <= bounds.left || bounds.bottom <= bounds.top {
		bounds.left, bounds.top, bounds.right, bounds.bottom = 100, 100, 740, 580
	}
	hwnd, _, err := user32.NewProc("CreateWindowExW").Call(0x80, uintptr(unsafe.Pointer(folderControllerClassName)), 0, 0x80000000,
		uintptr(bounds.left), uintptr(bounds.top), uintptr(bounds.right-bounds.left), uintptr(bounds.bottom-bounds.top), owner, 0, instance, 0) // WS_EX_TOOLWINDOW; WS_POPUP
	if hwnd == 0 {
		return nil, fmt.Errorf("フォルダーの選択画面を準備できません: %w", err)
	}
	return &folderDialogController{hwnd: hwnd, request: request}, nil
}

func (controller *folderDialogController) forwardRequests() func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		cancelled, pendingFocus := false, true
		cancelRequest := controller.request.ctx.Done()
		for {
			select {
			case <-controller.request.focus:
				pendingFocus = true
			case <-cancelRequest:
				cancelled = true
				cancelRequest = nil
			case <-ticker.C:
			case <-stop:
				return
			}
			hwnd := controller.dialogWindow()
			if hwnd == 0 {
				continue
			}
			if cancelled {
				// Address the dialog itself. Its modal loop does not reliably
				// dispatch messages sent to a separate hidden owner window.
				user32.NewProc("PostMessageW").Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
				continue
			}
			if pendingFocus {
				if visible, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd); visible != 0 {
					bringFolderDialogForward(hwnd)
					pendingFocus = false
				}
			}
		}
	}()
	return func() {
		close(stop)
		// SetForegroundWindow can send a message across threads. Continue
		// servicing this STA until the sender finishes before freeing its owner.
		for {
			select {
			case <-done:
				return
			default:
			}
			var message nativeWindowMessage
			if ok, _, _ := user32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1); ok != 0 {
				user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&message)))
				user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&message)))
			} else {
				time.Sleep(time.Millisecond)
			}
		}
	}
}

func (controller *folderDialogController) destroy() {
	user32.NewProc("DestroyWindow").Call(controller.hwnd)
}

func folderControllerWindowProcedure(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	result, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, uintptr(message), wparam, lparam)
	return result
}

type nativeWindowMessage struct {
	hwnd           uintptr
	message        uint32
	wparam, lparam uintptr
	time           uint32
	x, y           int32
	private        uint32
}

func (controller *folderDialogController) dialogWindow() uintptr {
	title := syscall.StringToUTF16Ptr(folderDialogTitle)
	var previous uintptr
	for {
		hwnd, _, _ := user32.NewProc("FindWindowExW").Call(0, previous, 0, uintptr(unsafe.Pointer(title)))
		if hwnd == 0 {
			return 0
		}
		owner, _, _ := user32.NewProc("GetWindow").Call(hwnd, 4) // GW_OWNER
		if owner == controller.hwnd {
			return hwnd
		}
		previous = hwnd
	}
}

func bringFolderDialogForward(hwnd uintptr) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	iconic, _, _ := user32.NewProc("IsIconic").Call(hwnd)
	if iconic != 0 {
		user32.NewProc("ShowWindow").Call(hwnd, 9) // SW_RESTORE
	}
	foreground, _, _ := user32.NewProc("GetForegroundWindow").Call()
	if foreground == hwnd {
		return
	}
	currentThread, _, _ := kernel32.NewProc("GetCurrentThreadId").Call()
	foregroundThread, _, _ := user32.NewProc("GetWindowThreadProcessId").Call(foreground, 0)
	if foregroundThread != 0 && foregroundThread != currentThread {
		attached, _, _ := user32.NewProc("AttachThreadInput").Call(currentThread, foregroundThread, 1)
		if attached != 0 {
			defer user32.NewProc("AttachThreadInput").Call(currentThread, foregroundThread, 0)
		}
	}
	user32.NewProc("SetWindowPos").Call(hwnd, 0, 0, 0, 0, 0, 0x1|0x2|0x40) // HWND_TOP; keep size and position, show
	user32.NewProc("BringWindowToTop").Call(hwnd)
	user32.NewProc("SetForegroundWindow").Call(hwnd)
}
