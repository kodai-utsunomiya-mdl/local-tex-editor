//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func AcquireAppLock(configDir string) (func(), error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}
	return acquireFileLock(filepath.Join(configDir, "app.lock"))
}

func AcquireProjectLock(root string) (func(), error) {
	return acquireFileLock(filepath.Join(root, ".local-tex-editor", "compile.lock"))
}

func acquireFileLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%s: %w", path, ErrLocked)
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			file.Close()
		})
	}, nil
}
