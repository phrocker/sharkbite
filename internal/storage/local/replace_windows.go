//go:build windows

package local

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

// ReplaceFileW switches the destination and creates its backup in one system
// call. A crash sees either the old target, or the new target plus old backup.
func platformAtomicReplace(temp, target, backup string, hadOld bool) error {
	if !hadOld {
		tempPtr, err := windows.UTF16PtrFromString(temp)
		if err != nil {
			return err
		}
		targetPtr, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return err
		}
		return windows.MoveFileEx(tempPtr, targetPtr, windows.MOVEFILE_WRITE_THROUGH)
	}
	return callReplaceFile(target, temp, backup)
}

func platformAtomicRestore(target, backup string) error {
	err := callReplaceFile(target, backup, "")
	if err != windows.ERROR_FILE_NOT_FOUND {
		return err
	}
	backupPtr, ptrErr := windows.UTF16PtrFromString(backup)
	if ptrErr != nil {
		return ptrErr
	}
	targetPtr, ptrErr := windows.UTF16PtrFromString(target)
	if ptrErr != nil {
		return ptrErr
	}
	return windows.MoveFileEx(backupPtr, targetPtr, windows.MOVEFILE_WRITE_THROUGH)
}

func callReplaceFile(target, replacement, backup string) error {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	replacementPtr, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		return err
	}
	var backupPtr *uint16
	if backup != "" {
		backupPtr, err = windows.UTF16PtrFromString(backup)
		if err != nil {
			return err
		}
	}
	result, _, callErr := replaceFileW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(unsafe.Pointer(replacementPtr)),
		uintptr(unsafe.Pointer(backupPtr)),
		0,
		0,
		0,
	)
	if result != 0 {
		return nil
	}
	if callErr != windows.ERROR_SUCCESS {
		return callErr
	}
	return syscall.EINVAL
}
