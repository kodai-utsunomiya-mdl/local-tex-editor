//go:build !windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

func PrepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func StopProcess(process *os.Process) error {
	if process == nil || process.Pid <= 0 {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func IsReparsePoint(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}

func IsRegularFile(_ string, info os.FileInfo) (bool, error) {
	return info.Mode().IsRegular(), nil
}

func ReplaceFile(src, dst string) error { return os.Rename(src, dst) }

func OpenBrowser(url string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	cmd := exec.Command(command, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func showNativeFolderDialog(_ *folderPickRequest) (string, error) {
	return "", errors.New("この OS ではフォルダーのパスを入力してください．")
}

func ShowError(message string) {}

func DecodeOutput(data []byte) string { return strings.ToValidUTF8(string(data), "\uFFFD") }

func EncodeFileName(name string) []byte { return []byte(name) }

func ShortPath(path string) (string, error) { return path, nil }
