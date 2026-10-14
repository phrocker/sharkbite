//go:build !linux

// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"errors"
	"os"
)

func acquireInquiryLock(string) (*os.File, error) {
	return nil, errors.New("local inquiry session locking requires Linux")
}
