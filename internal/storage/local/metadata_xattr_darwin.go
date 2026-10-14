//go:build darwin && !ios

package local

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

func platformXattrOperations() xattrOperations {
	return xattrOperations{
		list:   listXattrs,
		get:    getXattr,
		set:    setXattr,
		remove: removeXattr,
	}
}

func listXattrs(path string) ([]string, error) {
	size, err := darwinListxattr(path, nil, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := darwinListxattr(path, buf, 0)
	if err != nil {
		return nil, err
	}
	return splitXattrNames(buf[:n]), nil
}

func getXattr(path, name string) ([]byte, error) {
	size, err := darwinGetxattr(path, name, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	n, err := darwinGetxattr(path, name, buf, 0, 0)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func setXattr(path, name string, value []byte, flags int) error {
	return darwinSetxattr(path, name, value, 0, flags)
}

func removeXattr(path, name string) error {
	if err := darwinRemovexattr(path, name, 0); err != nil {
		if isMissingXattr(err) {
			return nil
		}
		return err
	}
	return nil
}

func darwinListxattr(path string, dest []byte, options int) (int, error) {
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	var destPtr *byte
	if len(dest) > 0 {
		destPtr = &dest[0]
	}
	size, _, errno := unix.Syscall6(
		unix.SYS_LISTXATTR,
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(destPtr)),
		uintptr(len(dest)),
		uintptr(options),
		0,
		0,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(size), nil
}

func darwinGetxattr(path, name string, dest []byte, position uint32, options int) (int, error) {
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	namePtr, err := unix.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	var destPtr *byte
	if len(dest) > 0 {
		destPtr = &dest[0]
	}
	size, _, errno := unix.Syscall6(
		unix.SYS_GETXATTR,
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(destPtr)),
		uintptr(len(dest)),
		uintptr(position),
		uintptr(options),
	)
	if errno != 0 {
		return 0, errno
	}
	return int(size), nil
}

func darwinSetxattr(path, name string, value []byte, position uint32, options int) error {
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	namePtr, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var valuePtr *byte
	if len(value) > 0 {
		valuePtr = &value[0]
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_SETXATTR,
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(valuePtr)),
		uintptr(len(value)),
		uintptr(position),
		uintptr(options),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func darwinRemovexattr(path, name string, options int) error {
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	namePtr, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall(
		unix.SYS_REMOVEXATTR,
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(options),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
