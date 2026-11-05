//go:build windows

package platform

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type fileAttributeTagInfo struct {
	attributes uint32
	tag        uint32
}

// Cloud Files tags describe the file at this path. A junction or symbolic
// link can redirect the path elsewhere and is still rejected.
func isCloudFileTag(tag uint32) bool {
	return tag & ^uint32(0x0000f000) == 0x9000001a
}

func readFileAttributeTag(path string) (fileAttributeTagInfo, error) {
	var info fileAttributeTagInfo
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return info, err
	}
	info.attributes, err = syscall.GetFileAttributes(name)
	if err != nil || info.attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return info, err
	}
	// OPEN_REPARSE_POINT reads the tag without following a junction or
	// downloading the contents of a OneDrive placeholder.
	handle, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return info, err
	}
	defer syscall.CloseHandle(handle)
	result, _, callErr := kernel32.NewProc("GetFileInformationByHandleEx").Call(uintptr(handle), 9,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)) // FileAttributeTagInfo
	if result == 0 {
		return info, fmt.Errorf("read file attributes: %w", callErr)
	}
	return info, nil
}

// IsReparsePoint reports paths that must not be followed inside a project.
// OneDrive files use Cloud Files tags and remain within their original path.
func IsReparsePoint(path string) (bool, error) {
	info, err := readFileAttributeTag(path)
	if err != nil {
		return false, err
	}
	return info.attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 && !isCloudFileTag(info.tag), nil
}

func IsRegularFile(path string, info os.FileInfo) (bool, error) {
	if info.Mode().IsRegular() {
		return true, nil
	}
	if info.IsDir() || info.Mode()&os.ModeIrregular == 0 {
		return false, nil
	}
	attributes, err := readFileAttributeTag(path)
	if err != nil {
		return false, err
	}
	return attributes.attributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 && isCloudFileTag(attributes.tag), nil
}
