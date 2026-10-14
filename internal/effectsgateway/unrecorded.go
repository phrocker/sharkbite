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
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/internal/dirlock"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// The unrecorded log (#514) holds lost-fence reports the explorer did not
// record: refused, not found, or indeterminate after the client's resend. A
// co-tenant in the same execute scope can deny the ambiguity route by
// spending the per-record report budget or evicting the holder from the
// claim history, and a gateway's own crash-looping replicas can evict their
// earlier holders, so the deployment that performed an effect can lose its
// recourse on the record. This log is the mitigation that holds: a report the
// explorer would not take is never "nothing to report".
//
// It is JSON lines, rewritten whole on every change through a temporary file
// that is fsync'd and renamed over the log, with the directory fsync'd after,
// so a crash leaves either the old log or the new one. A torn trailing line —
// which only a writer that did not follow this protocol can leave — is
// tolerated on read; any other unreadable line refuses the open, because
// skipping it would drop a report silently.
//
// It is bounded at UnrecordedMaxEntries entries and UnrecordedMaxBytes
// bytes. The bound is kept by reservation: the worker reserves room for one
// entry before each claim, since a claim can produce at most one report, and
// stops claiming (and goes not-ready) when no reservation is left. A report
// that arrives therefore always fits.
//
// An entry leaves the log in exactly two ways: a retry the explorer accepts
// (an identical report is a replay, #542, and is accepted too), or an
// explicit Ack by the operator. Nothing else — not a restart, not age —
// removes one.
//
// The directory also holds the gateway's lock file, flock'd for the life of
// the log: one gateway per surface, and a second instance refuses to start.

const (
	// UnrecordedMaxEntries and UnrecordedMaxBytes bound the log.
	UnrecordedMaxEntries = 1024
	UnrecordedMaxBytes   = 1 << 20
	// UnrecordedMaxEntryBytes bounds one encoded entry, newline included. A
	// test pins that the largest entry the field bounds admit fits.
	UnrecordedMaxEntryBytes = 16 << 10
	// UnrecordedFileName and LockFileName are the two files in the
	// directory.
	UnrecordedFileName = "unrecorded.jsonl"
	LockFileName       = "effects-gateway.lock"
	unrecordedTemp     = UnrecordedFileName + ".tmp"
)

// ErrGatewayLocked says another gateway instance holds the directory.
var ErrGatewayLocked = errors.New("another effects gateway holds this directory's lock")

// ErrUnrecordedFull says an entry did not fit. With reservations it cannot
// happen to a report the worker claimed room for.
var ErrUnrecordedFull = errors.New("unrecorded log is full")

// UnrecordedEntry is one report the explorer did not record. Every field is
// one the logging policy permits, or the bounded reference and target the
// report itself carries, or the record's correlation, which a retry must
// send: no input, no filled path, no body, no header, no error text.
type UnrecordedEntry struct {
	ActionID      []byte
	Fence         uint64
	ClaimNonce    ClaimNonce
	RouteAction   string
	Method        string
	PathTemplate  string
	Outcome       dispatchapi.AmbiguityOutcome
	Target        string
	Reference     string
	CorrelationID []byte
	// Status is the explorer's HTTP status on the last refusal, 0 when the
	// answer was lost.
	Status int
	// DispatchError is the client's closed kind for the last failure.
	DispatchError DispatchErrorKind
	FirstAt       time.Time
	LastAt        time.Time
	Attempts      int
}

// Report is the ambiguity report this entry holds.
func (e UnrecordedEntry) Report(request RequestContext) AmbiguityReport {
	request.CorrelationID = append([]byte(nil), e.CorrelationID...)
	return AmbiguityReport{
		Context: request, ClaimFence: e.Fence, Outcome: e.Outcome,
		Target: e.Target, Reference: e.Reference,
	}
}

type unrecordedWire struct {
	ActionID      string    `json:"action_id"`
	Fence         uint64    `json:"fence"`
	ClaimNonce    string    `json:"claim_nonce,omitempty"`
	RouteAction   string    `json:"route_action"`
	Method        string    `json:"method"`
	PathTemplate  string    `json:"path_template"`
	Outcome       string    `json:"outcome"`
	Target        string    `json:"target,omitempty"`
	Reference     string    `json:"reference,omitempty"`
	CorrelationID string    `json:"correlation_id"`
	Status        int       `json:"status,omitempty"`
	DispatchError string    `json:"dispatch_error,omitempty"`
	FirstAt       time.Time `json:"first_at"`
	LastAt        time.Time `json:"last_at"`
	Attempts      int       `json:"attempts"`
}

func (e UnrecordedEntry) wire() unrecordedWire {
	w := unrecordedWire{
		ActionID: base64.RawURLEncoding.EncodeToString(e.ActionID), Fence: e.Fence,
		RouteAction: e.RouteAction, Method: e.Method, PathTemplate: e.PathTemplate,
		Outcome: string(e.Outcome), Target: e.Target, Reference: e.Reference,
		CorrelationID: base64.RawURLEncoding.EncodeToString(e.CorrelationID),
		Status:        e.Status, DispatchError: string(e.DispatchError),
		FirstAt: e.FirstAt.UTC(), LastAt: e.LastAt.UTC(), Attempts: e.Attempts,
	}
	if e.ClaimNonce != (ClaimNonce{}) {
		w.ClaimNonce = hex.EncodeToString(e.ClaimNonce[:])
	}
	return w
}

func (w unrecordedWire) entry() (UnrecordedEntry, error) {
	id, err := base64.RawURLEncoding.Strict().DecodeString(w.ActionID)
	if err != nil {
		return UnrecordedEntry{}, errors.New("action_id is not unpadded base64url")
	}
	correlation, err := base64.RawURLEncoding.Strict().DecodeString(w.CorrelationID)
	if err != nil {
		return UnrecordedEntry{}, errors.New("correlation_id is not unpadded base64url")
	}
	entry := UnrecordedEntry{
		ActionID: id, Fence: w.Fence, RouteAction: w.RouteAction, Method: w.Method,
		PathTemplate: w.PathTemplate, Outcome: dispatchapi.AmbiguityOutcome(w.Outcome),
		Target: w.Target, Reference: w.Reference, CorrelationID: correlation,
		Status: w.Status, DispatchError: DispatchErrorKind(w.DispatchError),
		FirstAt: w.FirstAt, LastAt: w.LastAt, Attempts: w.Attempts,
	}
	if w.ClaimNonce != "" {
		nonce, err := hex.DecodeString(w.ClaimNonce)
		if err != nil || len(nonce) != ClaimNonceBytes {
			return UnrecordedEntry{}, errors.New("claim_nonce is not 16 hex-encoded bytes")
		}
		copy(entry.ClaimNonce[:], nonce)
	}
	return entry, entry.check()
}

// check holds an entry to the bounds a report and the logging policy set, so
// a hand-edited log cannot carry text a report could not.
func (e UnrecordedEntry) check() error {
	switch {
	case len(e.ActionID) == 0 || len(e.ActionID) > dispatchapi.MaxActionIDBytes:
		return errors.New("action ID is outside its bound")
	case e.Fence == 0:
		return errors.New("fence is required")
	case !validAmbiguity[e.Outcome]:
		return errors.New("outcome is not in the closed vocabulary")
	case CheckCorrelationID(e.CorrelationID) != nil:
		return errors.New("correlation ID is not one the explorer accepts")
	case len(e.Target) > dispatchapi.MaxAmbiguityTargetBytes || !printableAmbiguityText(e.Target):
		return errors.New("target is outside its bound")
	case len(e.Reference) > dispatchapi.MaxAmbiguityReferenceBytes || !printableAmbiguityText(e.Reference):
		return errors.New("reference is outside its bound")
	case len(e.RouteAction) > 256 || len(e.Method) > 16 || len(e.PathTemplate) > maxPathTemplateBytes:
		return errors.New("route fields are outside their bounds")
	case e.DispatchError != "" && !validDispatchErrors[e.DispatchError]:
		return errors.New("dispatch error is not in the closed vocabulary")
	case e.Status < 0 || e.Status > 999 || e.Attempts < 0:
		return errors.New("status or attempts is outside its bound")
	}
	return nil
}

func encodeEntry(entry UnrecordedEntry) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(entry.wire()); err != nil { // Encode appends '\n'
		return nil, err
	}
	if buffer.Len() > UnrecordedMaxEntryBytes {
		return nil, errors.New("unrecorded entry exceeds its encoded bound")
	}
	return buffer.Bytes(), nil
}

// fileSyncer is the fsync seam. Production calls (*os.File).Sync; a test
// counts the calls, which is how a missing fsync is caught.
var fileSyncer = func(file *os.File) error { return file.Sync() }

// UnrecordedLog is the durable log and the directory lock that comes with it.
type UnrecordedLog struct {
	dir  string
	lock *dirlock.Lock
	now  Clock

	mu       sync.Mutex
	entries  []UnrecordedEntry
	lines    [][]byte
	size     int
	reserved int
	closed   bool
}

// OpenUnrecordedLog takes the directory's lock and reads the log. A second
// open of the same directory, in this process or another, is
// ErrGatewayLocked.
func OpenUnrecordedLog(dir string, now Clock) (*UnrecordedLog, error) {
	return openUnrecordedLog(dir, now, true)
}

// ErrUnrecordedDirMissing says the directory OpenExistingUnrecordedLog was
// given does not exist, or is not a directory.
var ErrUnrecordedDirMissing = errors.New("unrecorded log directory does not exist")

// OpenExistingUnrecordedLog is OpenUnrecordedLog for an operator's command:
// it never creates the directory. A mistyped path is ErrUnrecordedDirMissing,
// not an empty log that says nothing awaits reconciliation. Nothing on this
// path creates a directory, so there is no check-then-create window: the
// lock file is created inside the directory only if the directory is there.
func OpenExistingUnrecordedLog(dir string, now Clock) (*UnrecordedLog, error) {
	return openUnrecordedLog(dir, now, false)
}

func openUnrecordedLog(dir string, now Clock, create bool) (*UnrecordedLog, error) {
	if dir == "" {
		return nil, errors.New("unrecorded log directory is required")
	}
	if now == nil {
		now = time.Now
	}
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, errors.New("unrecorded log directory cannot be created")
		}
	} else if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, ErrUnrecordedDirMissing
	}
	acquire := dirlock.Acquire
	if !create {
		acquire = dirlock.AcquireExisting
	}
	lock, err := acquire(dir, LockFileName)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return nil, ErrGatewayLocked
		}
		if !create && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)) {
			return nil, ErrUnrecordedDirMissing
		}
		return nil, errors.New("unrecorded log lock cannot be taken")
	}
	log := &UnrecordedLog{dir: dir, lock: lock, now: now}
	if err := log.load(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	// A temporary file is a write that never reached its rename: the log it
	// would have replaced is still the log.
	_ = os.Remove(filepath.Join(dir, unrecordedTemp))
	return log, nil
}

func (l *UnrecordedLog) load() error {
	file, err := os.Open(filepath.Join(l.dir, UnrecordedFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("unrecorded log cannot be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, UnrecordedMaxBytes+UnrecordedMaxEntryBytes+1))
	if err != nil {
		return errors.New("unrecorded log cannot be read")
	}
	if len(data) > UnrecordedMaxBytes+UnrecordedMaxEntryBytes {
		return errors.New("unrecorded log exceeds its bound")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), UnrecordedMaxBytes+UnrecordedMaxEntryBytes)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var wire unrecordedWire
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&wire)
		var entry UnrecordedEntry
		if decodeErr == nil {
			entry, decodeErr = wire.entry()
		}
		if decodeErr != nil {
			// Torn: the last line, with no newline after it.
			if !bytes.HasSuffix(data, []byte("\n")) &&
				bytes.HasSuffix(data, raw) && lineNumber == countLines(data) {
				break
			}
			return fmt.Errorf("unrecorded log line %d is unreadable; refusing to start "+
				"rather than drop a report", lineNumber)
		}
		if err := l.add(entry); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		return errors.New("unrecorded log cannot be read")
	}
	return nil
}

func countLines(data []byte) int {
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

// add appends or replaces in memory; the caller persists.
func (l *UnrecordedLog) add(entry UnrecordedEntry) error {
	line, err := encodeEntry(entry)
	if err != nil {
		return err
	}
	if index := l.find(entry.ActionID, entry.Fence); index >= 0 {
		l.size += len(line) - len(l.lines[index])
		l.entries[index], l.lines[index] = entry, line
		return nil
	}
	l.entries = append(l.entries, entry)
	l.lines = append(l.lines, line)
	l.size += len(line)
	return nil
}

func (l *UnrecordedLog) find(actionID []byte, fence uint64) int {
	for i, entry := range l.entries {
		if entry.Fence == fence && bytes.Equal(entry.ActionID, actionID) {
			return i
		}
	}
	return -1
}

// persist writes the whole log through a synced temporary file and a rename.
func (l *UnrecordedLog) persist() error {
	path := filepath.Join(l.dir, UnrecordedFileName)
	temp := filepath.Join(l.dir, unrecordedTemp)
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("unrecorded log: temporary file cannot be created")
	}
	for _, line := range l.lines {
		if _, err := file.Write(line); err != nil {
			_ = file.Close()
			_ = os.Remove(temp)
			return errors.New("unrecorded log: write failed")
		}
	}
	if err := fileSyncer(file); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return errors.New("unrecorded log: fsync failed")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return errors.New("unrecorded log: close failed")
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return errors.New("unrecorded log: rename failed")
	}
	directory, err := os.Open(l.dir)
	if err != nil {
		return errors.New("unrecorded log: directory cannot be synced")
	}
	defer directory.Close()
	if err := fileSyncer(directory); err != nil {
		return errors.New("unrecorded log: directory fsync failed")
	}
	return nil
}

// fits reports whether one more entry fits beside extra reserved ones.
func (l *UnrecordedLog) fits(extra int) bool {
	return len(l.entries)+extra <= UnrecordedMaxEntries &&
		l.size+extra*UnrecordedMaxEntryBytes <= UnrecordedMaxBytes
}

// Reserve claims room for one future entry, and is false when the log
// cannot promise it — the worker's signal to stop claiming. Every true
// return is paired with exactly one Release.
func (l *UnrecordedLog) Reserve() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.fits(l.reserved+1) {
		return false
	}
	l.reserved++
	return true
}

// Release returns a reservation, whether or not an entry used its room.
func (l *UnrecordedLog) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reserved > 0 {
		l.reserved--
	}
}

// Full reports whether no further reservation can be made.
func (l *UnrecordedLog) Full() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.fits(l.reserved + 1)
}

// Append writes an entry durably before returning. An entry for the same
// action and fence replaces the earlier one (there is at most one report per
// fence), keeping its FirstAt and counting the attempt.
//
// The caller holds a reservation for a new entry; a replacement needs none.
func (l *UnrecordedLog) Append(entry UnrecordedEntry) error {
	if err := entry.check(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("unrecorded log is closed")
	}
	now := l.now().UTC()
	if entry.LastAt.IsZero() {
		entry.LastAt = now
	}
	if entry.FirstAt.IsZero() {
		entry.FirstAt = entry.LastAt
	}
	if entry.Attempts == 0 {
		entry.Attempts = 1
	}
	previousEntries := append([]UnrecordedEntry(nil), l.entries...)
	previousLines := append([][]byte(nil), l.lines...)
	previousSize := l.size
	if index := l.find(entry.ActionID, entry.Fence); index >= 0 {
		entry.FirstAt = l.entries[index].FirstAt
		entry.Attempts += l.entries[index].Attempts
	} else if len(l.entries)+1 > UnrecordedMaxEntries {
		return ErrUnrecordedFull
	}
	if err := l.add(entry); err != nil {
		return err
	}
	if l.size > UnrecordedMaxBytes {
		l.entries, l.lines, l.size = previousEntries, previousLines, previousSize
		return ErrUnrecordedFull
	}
	if err := l.persist(); err != nil {
		l.entries, l.lines, l.size = previousEntries, previousLines, previousSize
		return err
	}
	return nil
}

// recordAttempts notes another failed retry on entries already held, and
// persists once for all of them. Entries no longer held are ignored.
func (l *UnrecordedLog) recordAttempts(updates []UnrecordedEntry) error {
	if len(updates) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("unrecorded log is closed")
	}
	previousEntries := append([]UnrecordedEntry(nil), l.entries...)
	previousLines := append([][]byte(nil), l.lines...)
	previousSize := l.size
	now := l.now().UTC()
	changed := false
	for _, update := range updates {
		index := l.find(update.ActionID, update.Fence)
		if index < 0 {
			continue
		}
		entry := l.entries[index]
		entry.Status, entry.DispatchError = update.Status, update.DispatchError
		entry.LastAt, entry.Attempts = now, entry.Attempts+1
		if err := l.add(entry); err != nil {
			l.entries, l.lines, l.size = previousEntries, previousLines, previousSize
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if l.size > UnrecordedMaxBytes+UnrecordedMaxEntryBytes {
		l.entries, l.lines, l.size = previousEntries, previousLines, previousSize
		return ErrUnrecordedFull
	}
	if err := l.persist(); err != nil {
		l.entries, l.lines, l.size = previousEntries, previousLines, previousSize
		return err
	}
	return nil
}

// AppendAll appends several entries with one durable rewrite. Each is
// treated as Append treats it.
func (l *UnrecordedLog) AppendAll(entries []UnrecordedEntry) error {
	if len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		if err := entry.check(); err != nil {
			return err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("unrecorded log is closed")
	}
	previousEntries := append([]UnrecordedEntry(nil), l.entries...)
	previousLines := append([][]byte(nil), l.lines...)
	previousSize := l.size
	restore := func() { l.entries, l.lines, l.size = previousEntries, previousLines, previousSize }
	now := l.now().UTC()
	for _, entry := range entries {
		if entry.LastAt.IsZero() {
			entry.LastAt = now
		}
		if entry.FirstAt.IsZero() {
			entry.FirstAt = entry.LastAt
		}
		if entry.Attempts == 0 {
			entry.Attempts = 1
		}
		if index := l.find(entry.ActionID, entry.Fence); index >= 0 {
			entry.FirstAt = l.entries[index].FirstAt
			entry.Attempts += l.entries[index].Attempts
		} else if len(l.entries)+1 > UnrecordedMaxEntries {
			restore()
			return ErrUnrecordedFull
		}
		if err := l.add(entry); err != nil {
			restore()
			return err
		}
	}
	if l.size > UnrecordedMaxBytes {
		restore()
		return ErrUnrecordedFull
	}
	if err := l.persist(); err != nil {
		restore()
		return err
	}
	return nil
}

// Ack removes the entry for an action and fence: the operator has reconciled
// it. It is the explicit clearing API (`shoal-gateway unrecorded ack`), and
// reports whether an entry was removed.
func (l *UnrecordedLog) Ack(actionID []byte, fence uint64) (bool, error) {
	return l.remove(actionID, fence)
}

// UnrecordedKey names one entry: an action and the fence its report is for.
type UnrecordedKey struct {
	ActionID []byte
	Fence    uint64
}

// AckAll removes several entries with one durable rewrite, or none: every
// key must name a held entry, and a failed write restores the log as it was.
// It is what `shoal-gateway unrecorded ack` calls, so an ack that fails part
// way never leaves some of the named reports cleared and others not.
func (l *UnrecordedLog) AckAll(keys []UnrecordedKey) error {
	if len(keys) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("unrecorded log is closed")
	}
	previousEntries := append([]UnrecordedEntry(nil), l.entries...)
	previousLines := append([][]byte(nil), l.lines...)
	previousSize := l.size
	restore := func() { l.entries, l.lines, l.size = previousEntries, previousLines, previousSize }
	for _, key := range keys {
		index := l.find(key.ActionID, key.Fence)
		if index < 0 {
			restore()
			return fmt.Errorf("no held report for action %s fence %d",
				base64.RawURLEncoding.EncodeToString(key.ActionID), key.Fence)
		}
		l.size -= len(l.lines[index])
		l.entries = append(l.entries[:index:index], l.entries[index+1:]...)
		l.lines = append(l.lines[:index:index], l.lines[index+1:]...)
	}
	if err := l.persist(); err != nil {
		restore()
		return err
	}
	return nil
}

// cleared removes an entry the explorer has now recorded.
func (l *UnrecordedLog) cleared(actionID []byte, fence uint64) (bool, error) {
	return l.remove(actionID, fence)
}

func (l *UnrecordedLog) remove(actionID []byte, fence uint64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false, errors.New("unrecorded log is closed")
	}
	index := l.find(actionID, fence)
	if index < 0 {
		return false, nil
	}
	previousEntries := append([]UnrecordedEntry(nil), l.entries...)
	previousLines := append([][]byte(nil), l.lines...)
	previousSize := l.size
	l.size -= len(l.lines[index])
	l.entries = append(l.entries[:index:index], l.entries[index+1:]...)
	l.lines = append(l.lines[:index:index], l.lines[index+1:]...)
	if err := l.persist(); err != nil {
		l.entries, l.lines, l.size = previousEntries, previousLines, previousSize
		return false, err
	}
	return true, nil
}

// Entries returns a copy of the held entries, oldest first.
func (l *UnrecordedLog) Entries() []UnrecordedEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]UnrecordedEntry, len(l.entries))
	for i, entry := range l.entries {
		entry.ActionID = append([]byte(nil), entry.ActionID...)
		entry.CorrelationID = append([]byte(nil), entry.CorrelationID...)
		out[i] = entry
	}
	return out
}

// Len is the gauge: how many reports await reconciliation.
func (l *UnrecordedLog) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Size is the encoded size of the log in bytes.
func (l *UnrecordedLog) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
}

// Close releases the directory lock. The log is already durable.
func (l *UnrecordedLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	return l.lock.Close()
}
