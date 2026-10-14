// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

//go:build !linux

package main

import "syscall"

// childProcAttr: only Linux has a parent-death signal; elsewhere a child is
// killed by the test's cleanup alone.
func childProcAttr() *syscall.SysProcAttr { return nil }
