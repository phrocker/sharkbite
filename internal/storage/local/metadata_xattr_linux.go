//go:build linux && !android

package local

import (
	"errors"

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
	size, err := unix.Listxattr(path, nil)
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
	n, err := unix.Listxattr(path, buf)
	if err != nil {
		return nil, err
	}
	return splitXattrNames(buf[:n]), nil
}

func getXattr(path, name string) ([]byte, error) {
	size, err := unix.Getxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func setXattr(path, name string, value []byte, flags int) error {
	return unix.Setxattr(path, name, value, flags)
}

func removeXattr(path, name string) error {
	if err := unix.Removexattr(path, name); err != nil {
		if isMissingXattr(err) {
			return nil
		}
		return err
	}
	return nil
}
