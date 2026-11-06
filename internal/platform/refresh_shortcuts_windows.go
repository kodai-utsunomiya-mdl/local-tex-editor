//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// RefreshInstalledShortcuts updates a previous installation when the user opens
// a newly downloaded executable. It does not create a new installation or link.
func RefreshInstalledShortcuts() error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	localAppData, err := existingShortcutFolder(&folderLocalAppData)
	if err != nil {
		return err
	}
	var directories []string
	for _, id := range []*nativeGUID{&folderPrograms, &folderDesktop} {
		directory, err := existingShortcutFolder(id)
		if err == nil {
			directories = append(directories, directory)
		}
	}
	if err := refreshInstalledShortcutsAt(source, localAppData, directories); err != nil {
		return fmt.Errorf("ショートカットから起動するアプリを更新できませんでした．設定でショートカットをもう一度作成してください．%w", err)
	}
	return nil
}

func existingShortcutFolder(id *nativeGUID) (string, error) {
	var path *uint16
	result, _, _ := shell32.NewProc("SHGetKnownFolderPath").Call(
		uintptr(unsafe.Pointer(id)), 0, 0, uintptr(unsafe.Pointer(&path)))
	if path != nil {
		defer ole32.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(path)))
	}
	if int32(result) < 0 || path == nil {
		return "", fmt.Errorf("Windows の保存先を取得できません (HRESULT 0x%08X)．", uint32(result))
	}
	return filepath.EvalSymlinks(nativeWideString(path))
}

func refreshInstalledShortcutsAt(source, localAppData string, directories []string) error {
	directory := filepath.Join(localAppData, "Programs", "LocalTeXEditor")
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Check the existing directories with the same rules used by installation.
	if _, err := shortcutInstallDirectory(localAppData); err != nil {
		return err
	}
	manifest, err := readShortcutInstallation(filepath.Join(directory, shortcutManifestName))
	if err != nil || manifest == nil {
		return err
	}
	release, err := acquireNamedLock(directory, `Local\LocalTeXEditorInstall-`)
	if err != nil {
		return err
	}
	installed, err := refreshInstalledExecutable(source, directory)
	release()
	if err != nil {
		return err
	}
	for _, folder := range directories {
		shortcut := filepath.Join(folder, shortcutName)
		info, err := shortcutRegularFile(shortcut)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Size() > 1<<20 {
			continue
		}
		link, err := readShellShortcut(shortcut)
		if err != nil || !shortcutSameFile(link.Target, installed) {
			continue
		}
		// A user may add a project argument or choose another working folder.
		// The executable is already updated; leave those link settings intact.
		if link.Arguments != "" || !shortcutSameFile(link.WorkingDirectory, filepath.Dir(installed)) {
			continue
		}
		if _, err := createShortcutAt(source, localAppData, folder); err != nil {
			return err
		}
	}
	notifyShellFileChange(installed, false)
	return nil
}

func refreshInstalledExecutable(source, directory string) (string, error) {
	const errorSharingViolation = syscall.Errno(32)
	deadline := time.Now().Add(2 * time.Second)
	for {
		installed, err := installShortcutExecutable(source, directory)
		if err == nil || time.Now().After(deadline) ||
			(!errors.Is(err, syscall.ERROR_ACCESS_DENIED) && !errors.Is(err, errorSharingViolation)) {
			return installed, err
		}
		// The previous process may have released its application mutex just
		// before Windows releases the executable's file mapping.
		time.Sleep(100 * time.Millisecond)
	}
}
