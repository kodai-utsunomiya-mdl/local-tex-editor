//go:build windows

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
)

func PrepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000200}
}

func StopProcess(process *os.Process) error {
	if process == nil || process.Pid <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	cmd := exec.CommandContext(ctx, filepath.Join(systemRoot, "System32", "taskkill.exe"), "/PID", strconv.Itoa(process.Pid), "/T", "/F")
	PrepareCommand(cmd)
	if err := cmd.Run(); err != nil {
		killErr := process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			return nil
		}
		return killErr
	}
	return nil
}

func ReplaceFile(src, dst string) error {
	s, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	d, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	ok, _, callErr := kernel32.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(d)), 0x1|0x8)
	if ok == 0 {
		return fmt.Errorf("replace file: %w", callErr)
	}
	return nil
}

func OpenBrowser(url string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 2)
	if int32(hr) < 0 {
		return fmt.Errorf("browser could not start (HRESULT 0x%x)", hr)
	}
	defer ole32.NewProc("CoUninitialize").Call()
	verb, _ := syscall.UTF16PtrFromString("open")
	address, err := syscall.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	result, _, _ := shell32.NewProc("ShellExecuteW").Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(address)), 0, 0, 1)
	if result <= 32 {
		return fmt.Errorf("could not open the browser (Windows code %d)", result)
	}
	return nil
}

func ShowError(message string) {
	title, _ := syscall.UTF16PtrFromString("Local TeX Editor")
	text, err := syscall.UTF16PtrFromString(message)
	if err != nil {
		return
	}
	user32.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}

func DecodeOutput(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	// TeX and Windows tools may use different encodings in the same log.
	// Decode only the lines that need the system code page.
	var output strings.Builder
	output.Grow(len(data))
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			end = len(data)
		} else {
			end++
		}
		line := data[:end]
		if utf8.Valid(line) {
			output.Write(line)
		} else {
			output.WriteString(decodeSystemCodePage(line))
		}
		data = data[end:]
	}
	return output.String()
}

func decodeSystemCodePage(data []byte) string {
	convert := kernel32.NewProc("MultiByteToWideChar")
	count, _, _ := convert.Call(0, 0, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0, 0)
	if count == 0 {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	wide := make([]uint16, int(count))
	result, _, _ := convert.Call(0, 0, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&wide[0])), count)
	if result == 0 {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return string(utf16.Decode(wide[:int(result)]))
}

// TeX Live's SyncTeX writer uses the Windows ANSI code page for Input names.
// Recreating that spelling lets the application match the recorded name to
// a Unicode name from the project or TeX's recorder file.
func EncodeFileName(name string) []byte {
	wide := utf16.Encode([]rune(name))
	if len(wide) == 0 {
		return nil
	}
	convert := kernel32.NewProc("WideCharToMultiByte")
	count, _, _ := convert.Call(0, 0, uintptr(unsafe.Pointer(&wide[0])), uintptr(len(wide)), 0, 0, 0, 0)
	if count == 0 {
		return []byte(name)
	}
	data := make([]byte, int(count))
	result, _, _ := convert.Call(0, 0, uintptr(unsafe.Pointer(&wide[0])), uintptr(len(wide)), uintptr(unsafe.Pointer(&data[0])), count, 0, 0)
	if result == 0 {
		return []byte(name)
	}
	return data[:int(result)]
}

// TeX's working directory can use Windows' 8.3 spelling even when the
// application started it with the full Unicode path.
func ShortPath(path string) (string, error) {
	input, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	convert := kernel32.NewProc("GetShortPathNameW")
	length, _, callErr := convert.Call(uintptr(unsafe.Pointer(input)), 0, 0)
	if length == 0 || length > 32768 {
		return "", fmt.Errorf("get short path: %w", callErr)
	}
	buffer := make([]uint16, int(length)+1)
	result, _, callErr := convert.Call(uintptr(unsafe.Pointer(input)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if result == 0 || result >= uintptr(len(buffer)) {
		return "", fmt.Errorf("get short path: %w", callErr)
	}
	return syscall.UTF16ToString(buffer[:result]), nil
}
