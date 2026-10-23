// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

//go:build !unix

package main

import "os/exec"

// setProcessGroup is a no-op where process groups are unavailable; the
// context's default kill applies to the trainer process alone.
func setProcessGroup(*exec.Cmd) {}
