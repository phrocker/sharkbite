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

package effectsgateway

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func heldEntry(id string, fence uint64) UnrecordedEntry {
	return UnrecordedEntry{
		ActionID: []byte(id), Fence: fence, ClaimNonce: ClaimNonce{1, 2, 3},
		RouteAction: "charge", Method: "POST", PathTemplate: "/v1/customers/{customer}/charges",
		Outcome: dispatchapi.AmbiguityOutcomeUnknown, Target: testSurface, Reference: "ch_1",
		CorrelationID: []byte("trace-" + id), Status: 400,
		DispatchError: DispatchAmbiguityUnrecorded,
	}
}

func openLog(t *testing.T, dir string) *UnrecordedLog {
	t.Helper()
	log, err := OpenUnrecordedLog(dir, newTimerClock().Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// TestUnrecordedLogIsDurableAndReplacesPerFence: entries survive a reopen;
// a second entry for the same action and fence replaces the first, keeping
// its first time and counting attempts.
func TestUnrecordedLogIsDurableAndReplacesPerFence(t *testing.T) {
	dir := t.TempDir()
	log := openLog(t, dir)
	if err := log.Append(heldEntry("a1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(heldEntry("a2", 1)); err != nil {
		t.Fatal(err)
	}
	again := heldEntry("a1", 1)
	again.Status = 404
	if err := log.Append(again); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 2 {
		t.Fatalf("%d entries", log.Len())
	}
	_ = log.Close()
	reopened := openLog(t, dir)
	entries := reopened.Entries()
	if len(entries) != 2 || entries[0].Attempts != 2 || entries[0].Status != 404 ||
		entries[0].ClaimNonce != (ClaimNonce{1, 2, 3}) || entries[0].FirstAt.IsZero() ||
		string(entries[0].CorrelationID) != "trace-a1" {
		t.Fatalf("entries = %+v", entries)
	}
	if removed, err := reopened.Ack([]byte("a1"), 1); err != nil || !removed {
		t.Fatalf("ack = %v %v", removed, err)
	}
	if removed, _ := reopened.Ack([]byte("a1"), 1); removed {
		t.Fatal("acked twice")
	}
	_ = reopened.Close()
	if final := openLog(t, dir); final.Len() != 1 || string(final.Entries()[0].ActionID) != "a2" {
		t.Fatalf("after ack: %+v", final.Entries())
	}
}

// TestUnrecordedLogFsyncsEveryChange: each write syncs the file before the
// rename and the directory after it.
func TestUnrecordedLogFsyncsEveryChange(t *testing.T) {
	var mu sync.Mutex
	var synced []string
	previous := fileSyncer
	fileSyncer = func(file *os.File) error {
		mu.Lock()
		synced = append(synced, filepath.Base(file.Name()))
		mu.Unlock()
		return file.Sync()
	}
	t.Cleanup(func() { fileSyncer = previous })
	dir := t.TempDir()
	log := openLog(t, dir)
	if err := log.Append(heldEntry("a1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Ack([]byte("a1"), 1); err != nil {
		t.Fatal(err)
	}
	want := []string{unrecordedTemp, filepath.Base(dir), unrecordedTemp, filepath.Base(dir)}
	if strings.Join(synced, ",") != strings.Join(want, ",") {
		t.Fatalf("synced %v, want %v", synced, want)
	}
	// A failed fsync is a failed append: nothing is claimed durable.
	fileSyncer = func(*os.File) error { return errors.New("disk") }
	if err := log.Append(heldEntry("a2", 1)); err == nil || log.Len() != 0 {
		t.Fatalf("append after a failed fsync = %v, %d entries", err, log.Len())
	}
}

// TestUnrecordedLogToleratesOnlyATornTrailingLine.
func TestUnrecordedLogToleratesOnlyATornTrailingLine(t *testing.T) {
	first, err := encodeEntry(heldEntry("a1", 1))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := encodeEntry(heldEntry("a2", 2))
	torn := second[:len(second)/2]

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UnrecordedFileName), append(append([]byte{}, first...), torn...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, unrecordedTemp), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := openLog(t, dir)
	if log.Len() != 1 {
		t.Fatalf("%d entries from a log with a torn tail", log.Len())
	}
	if _, err := os.Stat(filepath.Join(dir, unrecordedTemp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stale temporary file survived the open")
	}
	_ = log.Close()

	corrupt := t.TempDir()
	if err := os.WriteFile(filepath.Join(corrupt, UnrecordedFileName),
		append(append(append([]byte{}, torn...), '\n'), first...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUnrecordedLog(corrupt, nil); err == nil {
		t.Fatal("an unreadable line before the tail was skipped")
	}
}

// TestUnrecordedLogReservesItsBound: room is reserved before each claim, so
// no report can arrive to a full log; when no reservation is left the log
// is full.
func TestUnrecordedLogReservesItsBound(t *testing.T) {
	dir := t.TempDir()
	fillUnrecorded(t, dir, UnrecordedMaxEntries-2)
	log := openLog(t, dir)
	if log.Full() || !log.Reserve() || !log.Reserve() {
		t.Fatal("two reservations refused with room for two")
	}
	if !log.Full() || log.Reserve() {
		t.Fatal("a third reservation granted")
	}
	if err := log.Append(heldEntry("r1", 1)); err != nil {
		t.Fatal(err)
	}
	log.Release()
	if err := log.Append(heldEntry("r2", 1)); err != nil {
		t.Fatal(err)
	}
	log.Release()
	if log.Len() != UnrecordedMaxEntries || !log.Full() {
		t.Fatalf("%d entries, full %v", log.Len(), log.Full())
	}
	if err := log.Append(heldEntry("over", 1)); !errors.Is(err, ErrUnrecordedFull) {
		t.Fatalf("append past the bound = %v", err)
	}

	// The byte bound holds too: entries near the maximum size.
	bytesDir := t.TempDir()
	big := openLog(t, bytesDir)
	reserved := 0
	for big.Reserve() {
		reserved++
		entry := maximalEntry(t, reserved)
		if err := big.Append(entry); err != nil {
			t.Fatalf("append %d: %v", reserved, err)
		}
		big.Release()
	}
	if big.Size() > UnrecordedMaxBytes || big.Len() >= UnrecordedMaxEntries {
		t.Fatalf("%d bytes in %d entries", big.Size(), big.Len())
	}
}

func maximalEntry(t *testing.T, n int) UnrecordedEntry {
	t.Helper()
	id := []byte(strings.Repeat("i", dispatchapi.MaxActionIDBytes-8) + jsonNumber(100000 + n)[:6])
	return UnrecordedEntry{
		ActionID: id, Fence: ^uint64(0), ClaimNonce: ClaimNonce{0xff},
		RouteAction: strings.Repeat("a", 256), Method: "DELETE",
		PathTemplate:  strings.Repeat("p", maxPathTemplateBytes),
		Outcome:       dispatchapi.AmbiguityOutcomeUnknown,
		Target:        strings.Repeat("t", dispatchapi.MaxAmbiguityTargetBytes),
		Reference:     strings.Repeat("r", dispatchapi.MaxAmbiguityReferenceBytes),
		CorrelationID: []byte(strings.Repeat("c", shoal.MaxIDBytes)), Status: 999,
		DispatchError: DispatchAmbiguityUnrecorded, Attempts: 1 << 30,
	}
}

// TestTheLargestEntryFitsItsBound: every field at its bound still encodes
// within UnrecordedMaxEntryBytes, so the reservation arithmetic is sound.
func TestTheLargestEntryFitsItsBound(t *testing.T) {
	entry := maximalEntry(t, 1)
	if err := entry.check(); err != nil {
		t.Fatal(err)
	}
	line, err := encodeEntry(entry)
	if err != nil || len(line) > UnrecordedMaxEntryBytes {
		t.Fatalf("%d bytes, %v", len(line), err)
	}
}

// TestUnrecordedLogRefusesTextAReportCouldNotCarry: an entry is held to the
// report's own bounds.
func TestUnrecordedLogRefusesTextAReportCouldNotCarry(t *testing.T) {
	log := openLog(t, t.TempDir())
	for name, mutate := range map[string]func(*UnrecordedEntry){
		"no fence":          func(e *UnrecordedEntry) { e.Fence = 0 },
		"open outcome":      func(e *UnrecordedEntry) { e.Outcome = "done" },
		"control reference": func(e *UnrecordedEntry) { e.Reference = "ch\x00" },
		"long target":       func(e *UnrecordedEntry) { e.Target = strings.Repeat("t", 257) },
		"bad correlation":   func(e *UnrecordedEntry) { e.CorrelationID = []byte("has space") },
		"open kind":         func(e *UnrecordedEntry) { e.DispatchError = "free text" },
	} {
		entry := heldEntry("a1", 1)
		mutate(&entry)
		if err := log.Append(entry); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if log.Len() != 0 {
		t.Fatal("a refused entry was kept")
	}
}

// TestASecondGatewayIsRefusedByTheLock: one gateway per directory, released
// on close.
func TestASecondGatewayIsRefusedByTheLock(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUnrecordedLog(dir, nil); !errors.Is(err, ErrGatewayLocked) {
		t.Fatalf("second open = %v, want ErrGatewayLocked", err)
	}
	if _, err := os.Stat(filepath.Join(dir, LockFileName)); err != nil {
		t.Fatalf("no lock file: %v", err)
	}
	_ = first.Close()
	second, err := OpenUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatalf("open after close = %v", err)
	}
	_ = second.Close()
	_ = bytes.MinRead
}

// TestAckAllIsOneRewriteOrNone: AckAll clears every named entry with one
// durable rewrite; a key that names nothing, or a write that fails, leaves
// every entry in place, in memory and on disk.
func TestAckAllIsOneRewriteOrNone(t *testing.T) {
	keys := []UnrecordedKey{{[]byte("a1"), 1}, {[]byte("a2"), 1}, {[]byte("a3"), 2}}
	seed := func(t *testing.T) (string, *UnrecordedLog) {
		dir := t.TempDir()
		log, err := OpenUnrecordedLog(dir, newTimerClock().Now)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range append(keys, UnrecordedKey{[]byte("kept"), 1}) {
			if err := log.Append(heldEntry(string(key.ActionID), key.Fence)); err != nil {
				t.Fatal(err)
			}
		}
		return dir, log
	}
	onDisk := func(t *testing.T, dir string) int {
		t.Helper()
		log := openLog(t, dir)
		defer log.Close()
		return log.Len()
	}
	previous := fileSyncer
	t.Cleanup(func() { fileSyncer = previous })

	t.Run("a failed write removes nothing", func(t *testing.T) {
		dir, log := seed(t)
		fileSyncer = func(*os.File) error { return errors.New("disk") }
		err := log.AckAll(keys)
		fileSyncer = previous
		if err == nil || log.Len() != 4 {
			t.Fatalf("ack after a failed fsync = %v, %d entries left", err, log.Len())
		}
		_ = log.Close()
		if n := onDisk(t, dir); n != 4 {
			t.Fatalf("%d entries on disk after a failed ack", n)
		}
	})
	t.Run("an unknown key removes nothing", func(t *testing.T) {
		dir, log := seed(t)
		if err := log.AckAll(append(keys[:2:2], UnrecordedKey{[]byte("a9"), 1})); err == nil || log.Len() != 4 {
			t.Fatalf("ack naming an unknown entry = %v, %d entries left", err, log.Len())
		}
		_ = log.Close()
		if n := onDisk(t, dir); n != 4 {
			t.Fatalf("%d entries on disk", n)
		}
	})
	t.Run("one rewrite", func(t *testing.T) {
		dir, log := seed(t)
		writes := 0
		fileSyncer = func(file *os.File) error {
			if filepath.Base(file.Name()) == unrecordedTemp {
				writes++
			}
			return file.Sync()
		}
		err := log.AckAll(keys)
		fileSyncer = previous
		if err != nil || log.Len() != 1 || writes != 1 {
			t.Fatalf("ack = %v, %d left, %d rewrites (want 1)", err, log.Len(), writes)
		}
		_ = log.Close()
		if n := onDisk(t, dir); n != 1 {
			t.Fatalf("%d entries on disk", n)
		}
	})
}

// TestOpenExistingNeverCreatesTheDirectory: the operator's open refuses a
// missing directory, or a file, and creates nothing; it opens an existing
// one, as the gateway's own open does.
func TestOpenExistingNeverCreatesTheDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenExistingUnrecordedLog(missing, nil); !errors.Is(err, ErrUnrecordedDirMissing) {
		t.Fatalf("missing directory: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the operator's open created the directory: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExistingUnrecordedLog(file, nil); !errors.Is(err, ErrUnrecordedDirMissing) {
		t.Fatalf("a file: %v", err)
	}
	dir := t.TempDir()
	log, err := OpenExistingUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExistingUnrecordedLog(dir, nil); !errors.Is(err, ErrGatewayLocked) {
		t.Fatalf("a second open: %v", err)
	}
	_ = log.Close()
}
