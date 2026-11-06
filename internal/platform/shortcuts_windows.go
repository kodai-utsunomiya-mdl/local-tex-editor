//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	folderLocalAppData = nativeGUID{0xf1b32785, 0x6fba, 0x4fcf, [8]byte{0x9d, 0x55, 0x7b, 0x8e, 0x7f, 0x15, 0x70, 0x91}}
	folderDesktop      = nativeGUID{0xb4bfcc3a, 0xdb2c, 0x424c, [8]byte{0xb0, 0x29, 0x7f, 0xe9, 0x9a, 0x87, 0xc6, 0x41}}
	folderPrograms     = nativeGUID{0xa77f5d77, 0x2e2b, 0x44c3, [8]byte{0xa6, 0xa2, 0xab, 0xa6, 0x01, 0x05, 0x4a, 0x51}}
	classShellLink     = nativeGUID{0x00021401, 0, 0, [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW      = nativeGUID{0x000214f9, 0, 0, [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPersistFile     = nativeGUID{0x0000010b, 0, 0, [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
)

func knownShortcutFolder(id *nativeGUID) (string, error) {
	var path *uint16
	result, _, _ := shell32.NewProc("SHGetKnownFolderPath").Call(
		uintptr(unsafe.Pointer(id)), 0x8000, 0, uintptr(unsafe.Pointer(&path))) // KF_FLAG_CREATE, current user
	if path != nil {
		defer ole32.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(path)))
	}
	if int32(result) < 0 || path == nil {
		return "", fmt.Errorf("Windows の保存先を取得できません (HRESULT 0x%08X)．", uint32(result))
	}
	return filepath.EvalSymlinks(nativeWideString(path))
}

func CreateShortcut(destination string) (ShortcutResult, error) {
	var folder *nativeGUID
	switch destination {
	case "start-menu":
		folder = &folderPrograms
	case "desktop":
		folder = &folderDesktop
	default:
		return ShortcutResult{}, errors.New("ショートカットの作成先を選んでください．")
	}
	source, err := os.Executable()
	if err != nil {
		return ShortcutResult{}, fmt.Errorf("起動中の実行ファイルを取得できません: %w", err)
	}
	localAppData, err := knownShortcutFolder(&folderLocalAppData)
	if err != nil {
		return ShortcutResult{}, err
	}
	shortcutDirectory, err := knownShortcutFolder(folder)
	if err != nil {
		return ShortcutResult{}, err
	}
	return createShortcutAt(source, localAppData, shortcutDirectory)
}

// This helper also lets the Windows tests use temporary directories instead
// of modifying the account's Desktop or Start Menu.
func createShortcutAt(source, localAppData, shortcutDirectory string) (ShortcutResult, error) {
	directory, err := shortcutInstallDirectory(localAppData)
	if err != nil {
		return ShortcutResult{}, err
	}
	release, err := acquireNamedLock(directory, `Local\LocalTeXEditorInstall-`)
	if err != nil {
		return ShortcutResult{}, fmt.Errorf("別のウィンドウがショートカットを作成しています: %w", err)
	}
	defer release()
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return ShortcutResult{}, err
	}
	target := filepath.Join(directory, shortcutExecutableName)
	shortcut := filepath.Join(shortcutDirectory, shortcutName)
	existed := false
	if info, err := shortcutRegularFile(shortcut); err == nil {
		if info.Size() > 1<<20 {
			return ShortcutResult{}, fmt.Errorf("同名の別ファイルがあります (%s)．%w", shortcut, ErrShortcutConflict)
		}
		link, err := readShellShortcut(shortcut)
		if err != nil || (!shortcutPathsEqual(link.Target, target) && !shortcutPathsEqual(link.Target, source)) {
			return ShortcutResult{}, fmt.Errorf("同名の別のショートカットがあります (%s)．%w", shortcut, ErrShortcutConflict)
		}
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return ShortcutResult{}, err
	}
	installed, err := installShortcutExecutable(source, directory)
	if err != nil {
		return ShortcutResult{}, err
	}
	icon, iconCreated, err := installShortcutIcon(directory)
	if err != nil {
		return ShortcutResult{}, fmt.Errorf("ショートカットのアイコンを保存できません: %w", err)
	}
	temporary, err := os.CreateTemp(shortcutDirectory, "Local TeX Editor-*.lnk")
	if err != nil {
		return ShortcutResult{}, fmt.Errorf("ショートカットを保存できません: %w", err)
	}
	staged := temporary.Name()
	_ = temporary.Close()
	defer os.Remove(staged)
	if err := writeShellShortcut(staged, installed, icon); err != nil {
		return ShortcutResult{}, err
	}
	if err := commitShortcutFile(staged, shortcut, existed); err != nil {
		return ShortcutResult{}, fmt.Errorf("ショートカットを保存できません (%s): %w", shortcut, err)
	}
	// Refresh this application's entries without restarting Explorer or touching
	// the user's icon-cache database. The content-hashed ICO path changes with
	// the artwork, and rewriting the link also upgrades pre-icon installations.
	notifyShellFileChange(installed, false)
	notifyShellFileChange(icon, iconCreated)
	notifyShellFileChange(shortcut, !existed)
	return ShortcutResult{Executable: installed, Shortcut: shortcut, Message: "次回からこのショートカットで起動できます．"}, nil
}

func notifyShellFileChange(path string, created bool) {
	wide, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	event := uintptr(0x00002000) // SHCNE_UPDATEITEM
	if created {
		event = 0x00000002 // SHCNE_CREATE
	}
	shell32.NewProc("SHChangeNotify").Call(event, 0x0005|0x2000,
		uintptr(unsafe.Pointer(wide)), 0) // SHCNF_PATHW | SHCNF_FLUSHNOWAIT
}

func withShellShortcut(use func(link, persistent *nativeCOMObject) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 2) // COINIT_APARTMENTTHREADED
	if int32(result) < 0 {
		return shortcutCOMError("準備", result)
	}
	defer ole32.NewProc("CoUninitialize").Call()
	var link *nativeCOMObject
	result, _, _ = ole32.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&classShellLink)), 0, 1,
		uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if int32(result) < 0 || link == nil {
		return shortcutCOMError("作成", result)
	}
	defer link.call(2)
	var persistent *nativeCOMObject
	result = link.call(0, uintptr(unsafe.Pointer(&iidPersistFile)), uintptr(unsafe.Pointer(&persistent)))
	if int32(result) < 0 || persistent == nil {
		return shortcutCOMError("準備", result)
	}
	defer persistent.call(2)
	return use(link, persistent)
}

func shortcutCOMError(action string, result uintptr) error {
	return fmt.Errorf("ショートカットを%sできません (HRESULT 0x%08X)．", action, uint32(result))
}

func writeShellShortcut(path, target, iconPath string) error {
	return withShellShortcut(func(link, persistent *nativeCOMObject) error {
		for _, field := range []struct {
			index int
			value string
		}{
			{20, target}, // IShellLinkW::SetPath
			{9, filepath.Dir(target)},
			{11, ""},
			{7, "ブラウザで TeX 原稿を編集する"},
		} {
			wide, err := syscall.UTF16PtrFromString(field.value)
			if err != nil {
				return err
			}
			if result := link.call(field.index, uintptr(unsafe.Pointer(wide))); int32(result) < 0 {
				return shortcutCOMError("設定", result)
			}
		}
		icon, err := syscall.UTF16PtrFromString(iconPath)
		if err != nil {
			return err
		}
		if result := link.call(17, uintptr(unsafe.Pointer(icon)), 0); int32(result) < 0 {
			return shortcutCOMError("設定", result)
		}
		if result := link.call(15, 1); int32(result) < 0 { // SW_SHOWNORMAL
			return shortcutCOMError("設定", result)
		}
		name, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		if result := persistent.call(6, uintptr(unsafe.Pointer(name)), 1); int32(result) < 0 {
			return shortcutCOMError("保存", result)
		}
		return nil
	})
}

type shellShortcut struct {
	Target           string
	WorkingDirectory string
	Arguments        string
	IconPath         string
	IconIndex        int32
}

func readShellShortcut(path string) (shellShortcut, error) {
	var value shellShortcut
	err := withShellShortcut(func(link, persistent *nativeCOMObject) error {
		name, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		if result := persistent.call(5, uintptr(unsafe.Pointer(name)), 0); int32(result) < 0 { // STGM_READ
			return shortcutCOMError("読み取り", result)
		}
		buffer := make([]uint16, 32768)
		if result := link.call(3, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 4); int32(result) < 0 { // SLGP_RAWPATH
			return shortcutCOMError("読み取り", result)
		}
		value.Target = syscall.UTF16ToString(buffer)
		for _, field := range []struct {
			index  int
			target *string
		}{{8, &value.WorkingDirectory}, {10, &value.Arguments}} {
			clear(buffer)
			if result := link.call(field.index, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer))); int32(result) < 0 {
				return shortcutCOMError("読み取り", result)
			}
			*field.target = syscall.UTF16ToString(buffer)
		}
		clear(buffer)
		if result := link.call(16, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)),
			uintptr(unsafe.Pointer(&value.IconIndex))); int32(result) < 0 { // IShellLinkW::GetIconLocation
			return shortcutCOMError("読み取り", result)
		}
		value.IconPath = syscall.UTF16ToString(buffer)
		return nil
	})
	return value, err
}

func commitShortcutFile(source, target string, replace bool) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	flags := uintptr(0x8) // MOVEFILE_WRITE_THROUGH
	if replace {
		flags |= 0x1
	}
	result, _, callErr := kernel32.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), flags)
	if result == 0 {
		return fmt.Errorf("ファイルを配置できません: %w", callErr)
	}
	return nil
}
