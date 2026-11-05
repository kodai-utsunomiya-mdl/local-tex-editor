//go:build windows

package platform

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

func AcquireAppLock(configDir string) (func(), error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}
	return acquireNamedLock(configDir, `Local\LocalTeXEditorApp-`)
}

func AcquireProjectLock(root string) (func(), error) {
	return acquireNamedLock(root, `Local\LocalTeXEditorProject-`)
}

func acquireNamedLock(folder, prefix string) (func(), error) {
	absolute, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	identity := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(canonical))))
	name, err := syscall.UTF16PtrFromString(fmt.Sprintf("%s%x", prefix, identity))
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	threadLocked := true
	defer func() {
		if threadLocked {
			runtime.UnlockOSThread()
		}
	}()
	handle, _, callErr := kernel32.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return nil, fmt.Errorf("could not create the application lock: %w", callErr)
	}
	result, _, waitErr := kernel32.NewProc("WaitForSingleObject").Call(handle, 0)
	switch result {
	case 0, 0x80: // WAIT_OBJECT_0 and WAIT_ABANDONED both grant ownership.
		threadLocked = false
		var once sync.Once
		return func() {
			once.Do(func() {
				kernel32.NewProc("ReleaseMutex").Call(handle)
				kernel32.NewProc("CloseHandle").Call(handle)
				runtime.UnlockOSThread()
			})
		}, nil
	case 0x102: // WAIT_TIMEOUT: another thread owns the named mutex.
		kernel32.NewProc("CloseHandle").Call(handle)
		return nil, fmt.Errorf("%s: %w", folder, ErrLocked)
	default:
		kernel32.NewProc("CloseHandle").Call(handle)
		return nil, fmt.Errorf("could not acquire the application lock: %w", waitErr)
	}
}
