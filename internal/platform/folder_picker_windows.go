//go:build windows

package platform

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	folderDialogTitle = "TeX のプロジェクトフォルダーを選択"
	hresultCancelled  = 0x800704c7
	wmTimer           = 0x0113
	wmPickerFocus     = 0x8001
	wmPickerCancel    = 0x8002
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
	iidOleWindow        = nativeGUID{0x00000114, 0, 0, [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
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
	var window *nativeCOMObject
	if result = dialog.call(0, uintptr(unsafe.Pointer(&iidOleWindow)), uintptr(unsafe.Pointer(&window))); int32(result) < 0 || window == nil {
		return "", folderDialogError("準備", result)
	}
	defer window.call(2)
	controller, err := newFolderDialogController(request, dialog, window, owner)
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
	id           uintptr
	hwnd         uintptr
	request      *folderPickRequest
	dialog       *nativeCOMObject
	window       *nativeCOMObject
	pendingFocus bool
}

var folderDialogControllers = struct {
	sync.Mutex
	items map[uintptr]*folderDialogController
}{items: make(map[uintptr]*folderDialogController)}

var folderControllerSequence atomic.Uintptr
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

func newFolderDialogController(request *folderPickRequest, dialog, window *nativeCOMObject, owner uintptr) (*folderDialogController, error) {
	if err := registerFolderControllerClass(); err != nil {
		return nil, err
	}
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	// The dialog disables this hidden owner, leaving the browser's Cancel and
	// Show-dialog buttons usable. Its ownership chain still places the dialog
	// above the browser. Messages run on the same STA while Show is modal.
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
	controller := &folderDialogController{id: folderControllerSequence.Add(1), hwnd: hwnd, request: request, dialog: dialog, window: window, pendingFocus: true}
	folderDialogControllers.Lock()
	folderDialogControllers.items[hwnd] = controller
	folderDialogControllers.Unlock()
	timer, _, err := user32.NewProc("SetTimer").Call(hwnd, 1, 50, 0)
	if timer == 0 {
		controller.destroy()
		return nil, fmt.Errorf("フォルダーの選択画面を準備できません: %w", err)
	}
	return controller, nil
}

func (controller *folderDialogController) forwardRequests() func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-controller.request.focus:
				user32.NewProc("PostMessageW").Call(controller.hwnd, wmPickerFocus, controller.id, 0)
			case <-controller.request.ctx.Done():
				user32.NewProc("PostMessageW").Call(controller.hwnd, wmPickerCancel, controller.id, 0)
				return
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop); <-done }
}

func (controller *folderDialogController) destroy() {
	user32.NewProc("KillTimer").Call(controller.hwnd, 1)
	folderDialogControllers.Lock()
	delete(folderDialogControllers.items, controller.hwnd)
	folderDialogControllers.Unlock()
	user32.NewProc("DestroyWindow").Call(controller.hwnd)
}

func folderControllerWindowProcedure(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	if message == wmTimer || message == wmPickerFocus || message == wmPickerCancel {
		folderDialogControllers.Lock()
		controller := folderDialogControllers.items[hwnd]
		folderDialogControllers.Unlock()
		if controller != nil && (message == wmTimer || wparam == controller.id) {
			if message == wmPickerFocus {
				controller.pendingFocus = true
			}
			controller.update()
			return 0
		}
	}
	result, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, uintptr(message), wparam, lparam)
	return result
}

func (controller *folderDialogController) update() {
	if controller.request.ctx.Err() != nil {
		// IFileDialog::Close must be called on this STA, not the HTTP goroutine.
		controller.dialog.call(23, hresultCancelled)
		return
	}
	if !controller.pendingFocus {
		return
	}
	var hwnd uintptr
	if result := controller.window.call(3, uintptr(unsafe.Pointer(&hwnd))); int32(result) < 0 || hwnd == 0 {
		return
	}
	visible, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd)
	if visible == 0 {
		return
	}
	controller.pendingFocus = false
	bringFolderDialogForward(hwnd)
}

func bringFolderDialogForward(hwnd uintptr) {
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
