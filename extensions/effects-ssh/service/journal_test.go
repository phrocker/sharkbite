// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/sshclient"
)

var journalEpoch = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

type fakeNow struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeNow) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeNow) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func openTestJournal(t *testing.T, dir string, now *fakeNow, maxEntries int) *Journal {
	t.Helper()
	ensureJournal(t, dir)
	journal, err := OpenJournal(dir, JournalOptions{Retention: time.Hour, MaxEntries: maxEntries, Now: now.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal
}

var fp = Fingerprint("restart-web")

func completed() Finished {
	status := 0
	return Finished{Outcome: OutcomeCompleted, Reference: "exit-0", ExitStatus: &status}
}

func runAndFinish(t *testing.T, j *Journal, key string) {
	t.Helper()
	if begin := j.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatalf("begin %s: %v", key, begin.Kind)
	}
	if entry := j.Finish(key, completed()); entry.Finished == nil {
		t.Fatalf("finish %s not durable", key)
	}
}

func appendRaw(t *testing.T, dir string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dir, JournalFileName), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
}

func line(t *testing.T, r record) []byte {
	t.Helper()
	encoded, err := encodeLine(r)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestTornStartedTailIsDropped: a started record cut off by a crash was
// never durable, so nothing was dialed; it is dropped and the key is new.
func TestTornStartedTailIsDropped(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	runAndFinish(t, j, testKey("a"))
	_ = j.Close()
	full := line(t, record{Op: recordStarted, Key: testKey("b"), At: journalEpoch, Command: "restart-web", Fingerprint: fp})
	appendRaw(t, dir, full[:len(full)-1]) // everything but the LF
	before, _ := os.Stat(filepath.Join(dir, JournalFileName))

	j = openTestJournal(t, dir, now, 10)
	after, _ := os.Stat(filepath.Join(dir, JournalFileName))
	if after.Size() != before.Size()-int64(len(full)-1) {
		t.Fatalf("the torn tail was not truncated: %d -> %d", before.Size(), after.Size())
	}
	if begin := j.Begin(testKey("b"), fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatalf("torn started: %v", begin.Kind)
	}
	if begin := j.Begin(testKey("a"), fp, "restart-web", true, 4); begin.Kind != BeginFinished {
		t.Fatalf("finished key: %v", begin.Kind)
	}
}

// TestTornFinishedTailLeavesUnknown: a finished record cut off by a crash
// leaves the key started, which answers unknown and never runs again.
func TestTornFinishedTailLeavesUnknown(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	key := testKey("a")
	if begin := j.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatal(begin.Kind)
	}
	_ = j.Close()
	status := 0
	full := line(t, record{Op: recordFinished, Key: key, At: journalEpoch,
		Finished: &Finished{Outcome: OutcomeCompleted, Reference: "exit-0", ExitStatus: &status}})
	for _, cut := range []int{1, len(full) / 2, len(full) - 1} {
		copyDir := t.TempDir()
		raw, _ := os.ReadFile(filepath.Join(dir, JournalFileName))
		_ = os.WriteFile(filepath.Join(copyDir, JournalFileName), append(raw, full[:cut]...), 0o600)
		reopened := openTestJournal(t, copyDir, now, 10)
		if begin := reopened.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginUnknown {
			t.Fatalf("cut %d: %v", cut, begin.Kind)
		}
	}
}

// TestCorruptionRefusesToOpen: a complete line that fails its checksum, or a
// record contradicting the ones before it, is not trusted.
func TestCorruptionRefusesToOpen(t *testing.T) {
	now := &fakeNow{now: journalEpoch}
	key := testKey("a")
	started := line(t, record{Op: recordStarted, Key: key, At: journalEpoch, Command: "restart-web", Fingerprint: fp})
	status := 0
	finished := line(t, record{Op: recordFinished, Key: key, At: journalEpoch,
		Finished: &Finished{Outcome: OutcomeCompleted, Reference: "exit-0", ExitStatus: &status}})
	flipped := append([]byte(nil), started...)
	flipped[20] ^= 0x01
	// A checksum that does not match a record that still decodes.
	wrongSum := append([]byte(nil), started...)
	if wrongSum[0] == '0' {
		wrongSum[0] = '1'
	} else {
		wrongSum[0] = '0'
	}
	cases := map[string][]byte{
		"flipped byte, last line":  flipped,
		"wrong checksum":           wrongSum,
		"flipped byte, mid file":   append(append([]byte(nil), flipped...), finished...),
		"finished without started": finished,
		"started twice":            append(append([]byte(nil), started...), started...),
		"finished twice":           append(append(append([]byte(nil), started...), finished...), finished...),
		"unknown op":               line(t, record{Op: "deleted", Key: key, At: journalEpoch}),
		"bad key":                  line(t, record{Op: recordStarted, Key: "short", At: journalEpoch, Command: "restart-web", Fingerprint: fp}),
		"no time":                  line(t, record{Op: recordStarted, Key: key, Command: "restart-web", Fingerprint: fp}),
		"completed without reference": append(append([]byte(nil), started...), line(t, record{Op: recordFinished, Key: key,
			At: journalEpoch, Finished: &Finished{Outcome: OutcomeCompleted}})...),
		"unknown with reference": append(append([]byte(nil), started...), line(t, record{Op: recordFinished, Key: key,
			At: journalEpoch, Finished: &Finished{Outcome: OutcomeUnknown, Reference: "exit-0"}})...),
		"unknown field": []byte("00000000 {\"op\":\"started\",\"extra\":1}\n"),
		"no checksum":   []byte("{\"op\":\"started\"}\n"),
		"blank line":    []byte("\n"),
		"header twice":  headerLine(t),
		"record with a journal id": line(t, record{Op: recordStarted, JournalID: "0123456789abcdef0123456789abcdef",
			Key: key, At: journalEpoch, Command: "restart-web", Fingerprint: fp}),
	}
	header := headerLine(t)
	for name, content := range cases {
		content = append(append([]byte(nil), header...), content...)
		cases[name] = content
	}
	// And journals whose first line is not a valid header.
	cases["no header"] = started
	cases["malformed header id"] = line(t, record{Op: recordHeader, JournalID: "ABC", At: journalEpoch})
	cases["header with a key"] = line(t, record{Op: recordHeader, JournalID: "0123456789abcdef0123456789abcdef",
		Key: key, At: journalEpoch})
	for name, content := range cases {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, JournalFileName), content, 0o600)
		j, err := OpenJournal(dir, JournalOptions{Retention: time.Hour, MaxEntries: 10, Now: now.Now})
		if err == nil {
			_ = j.Close()
			t.Errorf("%s: opened", name)
			continue
		}
		if !errors.Is(err, ErrJournalCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
		// A refused open leaves the file as it was, and releases the lock.
		if raw, _ := os.ReadFile(filepath.Join(dir, JournalFileName)); string(raw) != string(content) {
			t.Errorf("%s: the file was changed", name)
		}
		if lock, err := lockDir(dir); err != nil {
			t.Errorf("%s: lock not released: %v", name, err)
		} else {
			_ = lock.release()
		}
	}
}

// TestServeNeverCreatesAJournal: an existing but empty directory (a volume
// that failed to mount, leaving the image's directory) is refused, and
// nothing is created in it. So is a directory holding only a fragment of a
// header, or an empty journal file.
func TestServeNeverCreatesAJournal(t *testing.T) {
	options := JournalOptions{Retention: time.Hour, MaxEntries: 10}
	empty := t.TempDir()
	if _, err := OpenJournal(empty, options); !errors.Is(err, ErrJournalUninitialized) {
		t.Fatalf("empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, JournalFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening an empty directory created a journal")
	}
	header := headerLine(t)
	for name, content := range map[string][]byte{
		"empty file":    {},
		"torn header":   header[:len(header)-1],
		"header prefix": header[:12],
	} {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, JournalFileName), content, 0o600)
		if _, err := OpenJournal(dir, options); !errors.Is(err, ErrJournalUninitialized) {
			t.Errorf("%s: %v", name, err)
		}
		if raw, _ := os.ReadFile(filepath.Join(dir, JournalFileName)); string(raw) != string(content) {
			t.Errorf("%s: the file was changed", name)
		}
	}
}

// TestEmptyVolumeAfterACrashDoesNotRerun is the scenario L1 names: a key
// started and never finished, then the volume is not mounted and the server
// starts on an empty directory. It refuses to start, so the resend cannot
// run the command again; with the volume back, the key answers unknown.
func TestEmptyVolumeAfterACrashDoesNotRerun(t *testing.T) {
	volume, now := t.TempDir(), &fakeNow{now: journalEpoch}
	id, err := InitJournal(volume, now.Now)
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(volume, JournalOptions{Retention: time.Hour, MaxEntries: 10, Now: now.Now, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("in-flight")
	if begin := j.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatal(begin.Kind)
	}
	_ = j.Close() // the crash
	unmounted := t.TempDir()
	if _, err := OpenJournal(unmounted, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id}); !errors.Is(err, ErrJournalUninitialized) {
		t.Fatalf("unmounted volume: %v", err)
	}
	j, err = OpenJournal(volume, JournalOptions{Retention: time.Hour, MaxEntries: 10, Now: now.Now, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if begin := j.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginUnknown {
		t.Fatalf("remounted: %v", begin.Kind)
	}
}

// TestPinnedJournalIDRefusesAnotherJournal: an initialized journal that is
// not the pinned one (another volume, or a re-init after a wipe) is refused.
func TestPinnedJournalIDRefusesAnotherJournal(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	id, err := InitJournal(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := InitJournal(second, nil)
	if err != nil || other == id || !ValidJournalID(id) || !ValidJournalID(other) {
		t.Fatalf("ids %q %q: %v", id, other, err)
	}
	if _, err := OpenJournal(second, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id}); !errors.Is(err, ErrJournalMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := OpenJournal(second, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: "not-an-id"}); err == nil {
		t.Fatal("a malformed pinned ID was accepted")
	}
	j, err := OpenJournal(first, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id})
	if err != nil || j.ID() != id {
		t.Fatalf("match: %v", err)
	}
	_ = j.Close()
	// Compaction keeps the identity.
	j, err = OpenJournal(first, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.records++ // force a rewrite
	err = j.compact()
	j.mu.Unlock()
	_ = j.Close()
	if err != nil {
		t.Fatal(err)
	}
	if j, err = OpenJournal(first, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id}); err != nil {
		t.Fatalf("after compaction: %v", err)
	}
	_ = j.Close()
}

// TestSymlinkedJournalIsRefused: a journal.log that links to the volume
// would be followed, and compaction's rename would replace the link with a
// file off the volume, leaving a stale journal behind the link. Refused.
func TestSymlinkedJournalIsRefused(t *testing.T) {
	volume, ephemeral := t.TempDir(), t.TempDir()
	id, err := InitJournal(volume, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(volume, JournalFileName), filepath.Join(ephemeral, JournalFileName)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := OpenJournal(ephemeral, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id}); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked journal: %v", err)
	}
	// A symlinked directory is fine: the rename stays inside it.
	linkedDir := filepath.Join(t.TempDir(), "dir")
	if err := os.Symlink(volume, linkedDir); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(linkedDir, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: id})
	if err != nil {
		t.Fatalf("symlinked directory: %v", err)
	}
	_ = j.Close()
	// InitJournal refuses to write through a link too.
	dangling := t.TempDir()
	_ = os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dangling, JournalFileName))
	if _, err := InitJournal(dangling, nil); err == nil {
		t.Fatal("init wrote through a link")
	}
}

// TestMismatchedJournalIsNotTouched: a journal that is not the pinned one
// is refused at its first line, before a torn tail is truncated, and even
// when it is corrupt further on.
func TestMismatchedJournalIsNotTouched(t *testing.T) {
	dir := t.TempDir()
	content := append(headerLine(t), []byte("00000000 {\"op\":\"started\"}\n0123")...)
	_ = os.WriteFile(filepath.Join(dir, JournalFileName), content, 0o600)
	_, err := OpenJournal(dir, JournalOptions{Retention: time.Hour, MaxEntries: 10, ID: "ffffffffffffffffffffffffffffffff"})
	if !errors.Is(err, ErrJournalMismatch) || errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("err %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, JournalFileName)); string(raw) != string(content) {
		t.Fatal("a journal that is not the pinned one was changed")
	}
}

// TestCompactionDoesNotWriteThroughALeftoverLink: a planted
// journal.log.compact link is removed, not followed.
func TestCompactionDoesNotWriteThroughALeftoverLink(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	runAndFinish(t, j, testKey("kept"))
	victim := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(victim, []byte("precious"), 0o600)
	if err := os.Symlink(victim, filepath.Join(dir, JournalFileName+".compact")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	j.mu.Lock()
	j.records++
	err := j.compact()
	j.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(victim); string(raw) != "precious" {
		t.Fatal("compaction wrote through a link")
	}
	if begin := j.Begin(testKey("kept"), fp, "restart-web", true, 4); begin.Kind != BeginFinished {
		t.Fatal(begin.Kind)
	}
}

// TestReadJournalOnAnUninitializedFile: the operator's view says what the
// server will do with it.
func TestReadJournalOnAnUninitializedFile(t *testing.T) {
	header := headerLine(t)
	for name, content := range map[string][]byte{"empty": {}, "torn header": header[:len(header)-1]} {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, JournalFileName), content, 0o600)
		if _, _, err := ReadJournal(dir); !errors.Is(err, ErrJournalUninitialized) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestInitJournalOnlyOnce: init refuses a directory that already holds a
// journal file, even an empty one, and a missing directory.
func TestInitJournalOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitJournal(dir, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, JournalFileName))
	if _, err := InitJournal(dir, nil); err == nil {
		t.Fatal("init replaced a journal")
	}
	if after, _ := os.ReadFile(filepath.Join(dir, JournalFileName)); string(after) != string(before) {
		t.Fatal("the second init changed the journal")
	}
	emptyFile := t.TempDir()
	_ = os.WriteFile(filepath.Join(emptyFile, JournalFileName), nil, 0o600)
	if _, err := InitJournal(emptyFile, nil); err == nil {
		t.Fatal("init adopted an existing empty journal file")
	}
	if _, err := InitJournal(filepath.Join(dir, "missing"), nil); err == nil {
		t.Fatal("init created a directory")
	}
	// A locked directory (a running server) is refused too.
	j := openTestJournal(t, t.TempDir(), &fakeNow{now: journalEpoch}, 10)
	_ = os.Remove(filepath.Join(j.dir, JournalFileName))
	if _, err := InitJournal(j.dir, nil); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("locked: %v", err)
	}
}

// TestReadJournalReportsWhereItStopped: on corruption the operator's view
// has every record before the bad line, and the line, offset and reason.
func TestReadJournalReportsWhereItStopped(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	runAndFinish(t, j, testKey("one"))
	_ = j.Close()
	before, _ := os.ReadFile(filepath.Join(dir, JournalFileName))
	appendRaw(t, dir, []byte("00000000 {\"op\":\"started\"}\n"))
	runAndFinishLine := line(t, record{Op: recordStarted, Key: testKey("two"), At: journalEpoch,
		Command: "restart-web", Fingerprint: fp})
	appendRaw(t, dir, runAndFinishLine)
	records, _, err := ReadJournal(dir)
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) || !errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("err %v", err)
	}
	if len(records) != 3 || records[0].Op != recordHeader || records[0].JournalID == "" ||
		records[2].Op != recordFinished {
		t.Fatalf("records %+v", records)
	}
	if corrupt.Line != 4 || corrupt.Offset != int64(len(before)) || corrupt.Reason != "checksum mismatch" {
		t.Fatalf("corrupt %+v (want line 4 at %d)", corrupt, len(before))
	}
	// A contradiction is reported the same way, by the rules OpenJournal uses.
	dup := t.TempDir()
	started := line(t, record{Op: recordStarted, Key: testKey("d"), At: journalEpoch, Command: "restart-web", Fingerprint: fp})
	_ = os.WriteFile(filepath.Join(dup, JournalFileName), append(append(headerLine(t), started...), started...), 0o600)
	records, _, err = ReadJournal(dup)
	if !errors.As(err, &corrupt) || len(records) != 2 || corrupt.Line != 3 || corrupt.Reason != "a key is started twice" {
		t.Fatalf("duplicate: %+v %v", records, err)
	}
}

func TestCheckJournalBounds(t *testing.T) {
	if err := CheckJournalBounds(10000, DefaultMaxOutputBytes); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
	for _, c := range [][2]int{{MaxJournalEntries, 1}, {10000, MaxOutputBytesLimit}, {3000, 64 << 10 * 2}} {
		if err := CheckJournalBounds(c[0], c[1]); err == nil {
			t.Errorf("%d entries x %d bytes accepted", c[0], c[1])
		}
	}
}

func TestJournalLockIsExclusive(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	if _, err := OpenJournal(dir, JournalOptions{Retention: time.Hour, MaxEntries: 10}); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("second open: %v", err)
	}
	_ = j.Close()
	again := openTestJournal(t, dir, now, 10)
	_ = again.Close()
}

func TestJournalOpenRefusals(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenJournal(missing, JournalOptions{Retention: time.Hour, MaxEntries: 10}); err == nil {
		t.Fatal("opened a missing directory")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the missing directory was created")
	}
	for name, options := range map[string]JournalOptions{
		"short retention": {Retention: time.Second, MaxEntries: 10},
		"no entries":      {Retention: time.Hour},
		"too many":        {Retention: time.Hour, MaxEntries: MaxJournalEntries + 1},
	} {
		if _, err := OpenJournal(t.TempDir(), options); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

// TestRetentionPrunesOnlyExpiredIdleKeys: a key is forgotten only after its
// last record is older than retention, and never while it runs.
func TestRetentionPrunesOnlyExpiredIdleKeys(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	runAndFinish(t, j, testKey("old"))
	// Still running when its started record passes retention.
	if begin := j.Begin(testKey("long"), fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatal(begin.Kind)
	}
	now.Advance(30 * time.Minute)
	runAndFinish(t, j, testKey("young"))
	if begin := j.Begin(testKey("running"), fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatal(begin.Kind)
	}
	now.Advance(31 * time.Minute)
	j.mu.Lock()
	j.prune()
	j.mu.Unlock()
	if _, ok := j.Lookup(testKey("old")); ok {
		t.Fatal("an expired key was kept")
	}
	if _, ok := j.Lookup(testKey("young")); !ok {
		t.Fatal("a key within retention was pruned")
	}
	if _, ok := j.Lookup(testKey("running")); !ok {
		t.Fatal("a running key was pruned")
	}
	if _, ok := j.Lookup(testKey("long")); !ok {
		t.Fatal("a running key past retention was pruned")
	}
	_ = j.Close()
	// On reopen the same rule applies to what is on disk: the running key's
	// started record is 31 minutes old, within retention, and answers unknown.
	j = openTestJournal(t, dir, now, 10)
	if j.Len() != 2 {
		t.Fatalf("%d keys after reopen", j.Len())
	}
	if begin := j.Begin(testKey("running"), fp, "restart-web", true, 4); begin.Kind != BeginUnknown {
		t.Fatalf("running key after reopen: %v", begin.Kind)
	}
}

// TestKeyReusedAfterRetentionStillOpens: a key the running process forgot
// (past retention) and then saw again has two started records on disk until
// a compaction; the journal must still open, holding the later one.
func TestKeyReusedAfterRetentionStillOpens(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	key := testKey("reused")
	runAndFinish(t, j, key)
	now.Advance(2 * time.Hour)
	// Another key's finish prunes the expired one in memory; its records
	// stay on disk (too few dead records to compact).
	runAndFinish(t, j, testKey("other"))
	if _, known := j.Lookup(key); known {
		t.Fatal("the expired key was not pruned")
	}
	runAndFinish(t, j, key)
	_ = j.Close()
	j = openTestJournal(t, dir, now, 10)
	if begin := j.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginFinished {
		t.Fatalf("reused key after reopen: %v", begin.Kind)
	}
	_ = j.Close()
	// Raising the retention, or a clock set back, does not re-judge the
	// earlier prune: the journal still opens.
	for name, options := range map[string]JournalOptions{
		"retention raised": {Retention: 48 * time.Hour, MaxEntries: 10, Now: now.Now},
		"clock set back":   {Retention: time.Hour, MaxEntries: 10, Now: func() time.Time { return journalEpoch }},
	} {
		reopened, err := OpenJournal(dir, options)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if begin := reopened.Begin(key, fp, "restart-web", true, 4); begin.Kind != BeginFinished {
			t.Fatalf("%s: %v", name, begin.Kind)
		}
		_ = reopened.Close()
	}
}

// TestJournalStaysWithinItsDeclaredBound: generations of keys pruned by a
// full Begin, faster than any Finish-time prune, still leave the file within
// JournalBound's record count.
func TestJournalStaysWithinItsDeclaredBound(t *testing.T) {
	saved := compactSlack
	compactSlack = 4
	t.Cleanup(func() { compactSlack = saved })
	const entries = 20
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, entries)
	for generation := 0; generation < 4; generation++ {
		for i := 0; i < entries; i++ {
			runAndFinish(t, j, testKey(string(rune('a'+generation))+string(rune('A'+i))))
		}
		now.Advance(2 * time.Hour)
		// Keep Finish-time pruning out of it: only the full Begin prunes.
		j.mu.Lock()
		j.lastPrune = now.Now()
		j.mu.Unlock()
		records, _, err := ReadJournal(dir)
		if err != nil {
			t.Fatal(err)
		}
		if limit := 2 * (2*entries + compactSlack); len(records) > limit {
			t.Fatalf("generation %d: %d records, bound %d", generation, len(records), limit)
		}
	}
}

// TestCompactionKeepsEveryLiveRecordByteForByte: after compaction the file
// holds exactly the live records, and a reopen answers every key as before.
func TestCompactionKeepsEveryLiveRecordByteForByte(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 5000)
	for i := 0; i < compactSlack+200; i++ {
		runAndFinish(t, j, testKey("dead"+string(rune(i))))
	}
	now.Advance(2 * time.Hour)
	unfinished := testKey("unfinished")
	if begin := j.Begin(unfinished, fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatal(begin.Kind)
	}
	for i := 0; i < 5; i++ {
		runAndFinish(t, j, testKey("live"+string(rune(i))))
	}
	records, torn, err := ReadJournal(dir)
	if err != nil || torn {
		t.Fatal(err, torn)
	}
	// The dead keys expired, so the finishes after them compacted the file.
	if len(records) != 1+1+10 {
		t.Fatalf("%d records after compaction", len(records))
	}
	if _, err := os.Stat(filepath.Join(dir, JournalFileName+".compact")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the compaction file was left behind")
	}
	// The running key finishes after the compaction moved its record.
	if entry := j.Finish(unfinished, completed()); entry.Finished == nil {
		t.Fatal("finish after compaction")
	}
	_ = j.Close()
	j = openTestJournal(t, dir, now, 5000)
	for i := 0; i < 5; i++ {
		if begin := j.Begin(testKey("live"+string(rune(i))), fp, "restart-web", true, 4); begin.Kind != BeginFinished {
			t.Fatalf("live %d: %v", i, begin.Kind)
		}
	}
	if begin := j.Begin(unfinished, fp, "restart-web", true, 4); begin.Kind != BeginFinished {
		t.Fatalf("unfinished: %v", begin.Kind)
	}
}

// TestWriteFailureFailsClosed: a failed append is never a run, the journal
// refuses new keys from then on, and a run whose finish could not be written
// answers unknown.
func TestWriteFailureFailsClosed(t *testing.T) {
	dir, now := t.TempDir(), &fakeNow{now: journalEpoch}
	j := openTestJournal(t, dir, now, 10)
	runAndFinish(t, j, testKey("done"))
	running := testKey("running")
	if begin := j.Begin(running, fp, "restart-web", true, 4); begin.Kind != BeginRun {
		t.Fatal(begin.Kind)
	}
	j.mu.Lock()
	_ = j.file.Close() // every later write fails
	j.mu.Unlock()
	if entry := j.Finish(running, completed()); entry.Finished != nil {
		t.Fatal("a finish that failed to write is reported durable")
	}
	select {
	case <-j.Failed():
	default:
		t.Fatal("the journal did not report the failure")
	}
	if begin := j.Begin(running, fp, "restart-web", true, 4); begin.Kind != BeginUnknown {
		t.Fatalf("unrecorded finish: %v", begin.Kind)
	}
	if begin := j.Begin(testKey("new"), fp, "restart-web", true, 4); begin.Kind != BeginUnavailable {
		t.Fatalf("new key on a failed journal: %v", begin.Kind)
	}
	if begin := j.Begin(testKey("done"), fp, "restart-web", true, 4); begin.Kind != BeginFinished {
		t.Fatalf("known key on a failed journal: %v", begin.Kind)
	}
}

// TestStartedIsDurableBeforeTheRun: when the command is dialed, its started
// record is already on disk.
func TestStartedIsDurableBeforeTheRun(t *testing.T) {
	var onDisk bool
	e := newEnv(t, func(o *Options, _ *JournalOptions) {
		o.Run = func(ctx context.Context, command *Command) sshclient.Result {
			records, _, err := ReadJournal(o.Journal.dir)
			onDisk = err == nil && len(records) == 2 && records[1].Op == recordStarted
			return runOverSSH(ctx, command)
		}
	})
	e.post("restart-web", testKey("durable")).is(t, loadAnswers(t), "completed")
	if !onDisk {
		t.Fatal("the run began before its started record was on disk")
	}
}

// TestSyncsPrecedeTheDial: the journal's directory entry is synced when the
// journal opens, the file is synced after the started record and before the
// run, and again after the finished record.
func TestSyncsPrecedeTheDial(t *testing.T) {
	var mu sync.Mutex
	var events []string
	note := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	original := syncFile
	syncFile = func(f *os.File) error {
		info, err := f.Stat()
		if err == nil && info.IsDir() {
			note("sync dir")
		} else {
			note("sync " + filepath.Base(f.Name()))
		}
		return original(f)
	}
	t.Cleanup(func() { syncFile = original })
	e := newEnv(t, func(o *Options, _ *JournalOptions) {
		o.Run = func(ctx context.Context, command *Command) sshclient.Result {
			note("dial")
			return runOverSSH(ctx, command)
		}
	})
	e.post("restart-web", testKey("order")).is(t, loadAnswers(t), "completed")
	mu.Lock()
	defer mu.Unlock()
	// The first two are journal init (file, then directory); open syncs nothing.
	want := []string{"sync " + JournalFileName, "sync dir", "sync " + JournalFileName, "dial", "sync " + JournalFileName}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events %q, want %q", events, want)
	}
}

// TestSyncFailureBeforeTheDialRunsNothing: a started record whose fsync
// fails is not a run.
func TestSyncFailureBeforeTheDialRunsNothing(t *testing.T) {
	e := newEnv(t)
	original := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	t.Cleanup(func() { syncFile = original })
	e.post("restart-web", testKey("fsync")).is(t, loadAnswers(t), "refused_journal_unavailable")
	if e.host.ssh.Accepts() != 0 {
		t.Fatal("dialed after a failed fsync")
	}
	select {
	case <-e.journal.Failed():
	default:
		t.Fatal("the journal did not fail")
	}
}

// TestOneFailedSyncFailsTheJournalForGood: Linux may report an fsync error
// once and then let the next fsync succeed over lost data, so after one
// failure no new key is ever started, even when the disk seems to recover.
func TestOneFailedSyncFailsTheJournalForGood(t *testing.T) {
	e := newEnv(t)
	original := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	e.post("restart-web", testKey("first")).is(t, loadAnswers(t), "refused_journal_unavailable")
	syncFile = original
	t.Cleanup(func() { syncFile = original })
	e.post("restart-web", testKey("second")).is(t, loadAnswers(t), "refused_journal_unavailable")
	if e.host.ssh.Accepts() != 0 {
		t.Fatal("dialed on a failed journal")
	}
}

// TestBeginUnavailableDialsNothing: a started record that cannot be written
// means no run.
func TestBeginUnavailableDialsNothing(t *testing.T) {
	e := newEnv(t)
	e.journal.mu.Lock()
	_ = e.journal.file.Close()
	e.journal.mu.Unlock()
	e.post("restart-web", testKey("x")).is(t, loadAnswers(t), "refused_journal_unavailable")
	if e.host.ssh.Accepts() != 0 {
		t.Fatal("dialed with no durable started record")
	}
}
