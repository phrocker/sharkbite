// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build linux

package gatewaycmd

import (
	"bytes"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
)

// TestAckIsOneRewrite: an ack of several entries replaces the log once. Every
// rewrite renames a fresh file over the log, so the renames the kernel reports
// on the directory are the rewrites; one per entry would let a failure part
// way leave some reports cleared and others not.
func TestAckIsOneRewrite(t *testing.T) {
	dir := seedLog(t, entry("act-1", 1), entry("act-2", 1), entry("act-3", 1), entry("act-4", 1))
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	defer unix.Close(fd)
	// Both halves of the rename are watched: without IN_MOVED_FROM the
	// kernel issues no rename cookie, and it coalesces identical queued
	// IN_MOVED_TO events into one.
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_MOVED_FROM|unix.IN_MOVED_TO); err != nil {
		t.Skipf("inotify watch unavailable: %v", err)
	}
	code, _, stderr := operator("ack", "-unrecorded-dir", dir,
		b64([]byte("act-1")), b64([]byte("act-2"))+":1", b64([]byte("act-3")))
	if code != ExitOK {
		t.Fatalf("ack: exit %d %s", code, stderr)
	}
	renames := 0
	buffer := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buffer)
		if err != nil || n <= 0 {
			break
		}
		for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buffer[offset]))
			name := buffer[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+int(event.Len)]
			if event.Mask&unix.IN_MOVED_TO != 0 &&
				string(bytes.TrimRight(name, "\x00")) == effectsgateway.UnrecordedFileName {
				renames++
			}
			offset += unix.SizeofInotifyEvent + int(event.Len)
		}
	}
	if renames != 1 {
		t.Fatalf("the ack rewrote the log %d times, want once", renames)
	}
	if got := held(t, dir); len(got) != 1 || string(got[0].ActionID) != "act-4" {
		t.Fatalf("after ack: %+v", got)
	}
}
