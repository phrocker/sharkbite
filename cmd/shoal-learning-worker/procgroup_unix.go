// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the trainer in its own process group and kills the
// whole group when the worker's context ends, so a trainer's children do
// not outlive the worker or hold its output pipe open.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
