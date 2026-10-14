//go:build !linux

// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"errors"
	"os"
)

func openLockFile(string) (*os.File, error) {
	return nil, errors.New("local service session locking requires Linux")
}
func lockSessionFile(*os.File) error {
	return errors.New("local service session locking requires Linux")
}
