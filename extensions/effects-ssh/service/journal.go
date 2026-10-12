// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// The journal is what makes a command run at most once per idempotency key.
//
// It is an append-only file of records, one per line:
//
//	<crc32c of the JSON, 8 hex digits> SP <JSON record> LF
//
// A key's life is two records: "started", written and fsync'd before any
// connection is opened, and "finished", written and fsync'd after the run
// ends, carrying what is known of how it ended. Every answer about a key is a
// function of what is durable:
//
//	no record           the command has not run; a new key may start it
//	started, finished   the recorded result is replayed, never re-run
//	started only        running in this process: the request attaches to it;
//	                    otherwise (a restart, a finish that could not be
//	                    made durable) the outcome is unknown, forever
//
// Crash windows:
//
//   - Before "started" is durable: nothing was dialed. A torn "started" line
//     at the tail is dropped on open, which is correct because the command
//     was never begun.
//   - After "started" is durable, before "finished" is: the key answers
//     outcome unknown and never runs again. A torn "finished" line at the
//     tail is dropped, which leaves exactly this state.
//   - A complete line anywhere that fails its checksum, or a record that
//     contradicts the ones before it, is corruption: the journal refuses to
//     open and nothing runs until an operator has looked.
//
// Compaction rewrites the live records to a temporary file, fsyncs it,
// renames it over the journal and fsyncs the directory, so a crash leaves
// either the old file or the new one, each complete.
//
// The journal directory is held under an exclusive flock for the life of the
// process: two processes sharing one journal would each believe a key new.

// JournalFileName is the journal inside its directory.
const JournalFileName = "journal.log"

// LockFileName is the lock inside the journal directory.
const LockFileName = "LOCK"

const (
	recordStarted  = "started"
	recordFinished = "finished"
	// recordHeader is the first line of every journal, written once by
	// InitJournal: the journal's identity and creation time.
	recordHeader = "journal"
	// maxRecordLine bounds one line read back. The largest record this code
	// writes, two streams at MaxOutputBytesLimit in base64, is well under it.
	maxRecordLine = 4 << 20
	// MaxJournalEntries bounds -journal-max-entries.
	MaxJournalEntries = 1 << 20
	// MinJournalRetention bounds -journal-retention from below.
	MinJournalRetention = time.Minute
)

// compactSlack is how many dead records may accumulate beyond the live ones
// before the file is rewritten. A variable only so the bound test can reach
// it with a small journal.
var compactSlack = 1024

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// syncFile is every fsync the journal makes, of its file and of its
// directory. It is a variable so tests can observe the order of syncs
// against the dial (TestSyncsPrecedeTheDial); nothing else replaces it.
var syncFile = (*os.File).Sync

// ErrJournalCorrupt is returned by OpenJournal for a journal it will not trust.
var ErrJournalCorrupt = errors.New("journal is corrupt")

// ErrJournalUninitialized is returned by OpenJournal for a directory holding
// no initialized journal. The server never creates one: an empty directory is
// what a dropped volume mount looks like, and treating it as a fresh journal
// would forget every key and re-run a command whose outcome was unknown.
var ErrJournalUninitialized = errors.New("journal is not initialized")

// ErrJournalMismatch is returned by OpenJournal when the journal's identity
// is not the one the operator pinned.
var ErrJournalMismatch = errors.New("journal identity does not match")

// CorruptError says where and why a journal stopped being readable.
type CorruptError struct {
	// Offset is the byte offset of the line, Line its 1-based number.
	Offset int64
	Line   int
	Reason string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("%v: line %d at offset %d: %s", ErrJournalCorrupt, e.Line, e.Offset, e.Reason)
}

// Is makes errors.Is(err, ErrJournalCorrupt) hold.
func (e *CorruptError) Is(target error) bool { return target == ErrJournalCorrupt }

var journalIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidJournalID reports whether id has the form InitJournal writes.
func ValidJournalID(id string) bool { return journalIDPattern.MatchString(id) }

// Outcome is a finished run's outcome, as recorded.
type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeNotRun    Outcome = "not_run"
	OutcomeUnknown   Outcome = "unknown"
)

// Finished is what a finished record carries.
type Finished struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	// Reference is the exact string the completed answer carries, recorded
	// so a replay is byte-identical whatever this code later becomes.
	Reference       string `json:"reference,omitempty"`
	ExitStatus      *int   `json:"exit_status,omitempty"`
	Signal          string `json:"signal,omitempty"`
	Stdout          []byte `json:"stdout,omitempty"`
	StdoutBytes     int64  `json:"stdout_bytes"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	Stderr          []byte `json:"stderr,omitempty"`
	StderrBytes     int64  `json:"stderr_bytes"`
	StderrTruncated bool   `json:"stderr_truncated"`
	DurationMillis  int64  `json:"duration_ms"`
}

type record struct {
	Op          string    `json:"op"`
	JournalID   string    `json:"journal_id,omitempty"`
	Key         string    `json:"key"`
	At          time.Time `json:"at"`
	Command     string    `json:"command,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Finished    *Finished `json:"finished,omitempty"`
}

// span is where a record's line sits in the current file.
type span struct{ offset, length int64 }

// Entry is a key's state.
type Entry struct {
	Key         string
	Command     string
	Fingerprint string
	StartedAt   time.Time
	FinishedAt  time.Time
	// Finished is nil until the finished record is durable. Output is not
	// held in memory; Read the journal file for it.
	Finished *Finished

	started, finished span
	running           bool
	done              chan struct{}
}

func (e *Entry) last() time.Time {
	if e.FinishedAt.After(e.StartedAt) {
		return e.FinishedAt
	}
	return e.StartedAt
}

// snapshot copies the entry without its process-local state.
func (e *Entry) snapshot() Entry {
	out := Entry{Key: e.Key, Command: e.Command, Fingerprint: e.Fingerprint,
		StartedAt: e.StartedAt, FinishedAt: e.FinishedAt, running: e.running}
	if e.Finished != nil {
		finished := *e.Finished
		finished.Stdout, finished.Stderr = nil, nil
		out.Finished = &finished
	}
	return out
}

// Running reports whether this process is running the key's command.
func (e Entry) Running() bool { return e.running }

// JournalOptions are the declared bounds.
type JournalOptions struct {
	// Retention is how long a key is remembered after its last record. The
	// gateway's -idempotency-retention must not exceed it.
	Retention time.Duration
	// MaxEntries bounds the keys held. A new key beyond it is refused.
	MaxEntries int
	// Now is the wall clock; tests substitute one.
	Now func() time.Time
	// ID, when set, is the journal identity the operator pinned; a journal
	// with another identity is refused, so a different volume cannot be
	// swapped in.
	ID string
}

// Journal is the durable once-only record.
type Journal struct {
	mu      sync.Mutex
	dir     string
	lock    *dirLock
	file    *os.File
	size    int64
	records int
	entries map[string]*Entry
	running int
	opts    JournalOptions
	failed  error
	failCh  chan struct{}
	closed  bool
	// lastPrune is when keys past retention were last forgotten.
	lastPrune time.Time
	// id is the journal's identity, from its header line at header.
	id     string
	header span
}

// ID is the journal's identity.
func (j *Journal) ID() string { return j.id }

// OpenJournal locks dir, reads the journal, drops a torn tail, prunes keys
// past retention and compacts.
func OpenJournal(dir string, opts JournalOptions) (*Journal, error) {
	switch {
	case opts.Retention < MinJournalRetention:
		return nil, fmt.Errorf("journal retention must be at least %s", MinJournalRetention)
	case opts.MaxEntries < 1 || opts.MaxEntries > MaxJournalEntries:
		return nil, fmt.Errorf("journal max entries must be 1 to %d", MaxJournalEntries)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ID != "" && !ValidJournalID(opts.ID) {
		return nil, errors.New("journal ID must be 32 lowercase hex digits, as journal init prints it")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("journal directory must exist; it is not created, " +
			"so a mistyped path cannot start an empty journal")
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, lock: lock, entries: map[string]*Entry{}, opts: opts,
		failCh: make(chan struct{})}
	if err := j.load(); err != nil {
		_ = lock.release()
		return nil, err
	}
	if opts.ID != "" && opts.ID != j.id {
		j.closeFile()
		_ = lock.release()
		return nil, fmt.Errorf("%w: the directory holds journal %s, and %s was pinned",
			ErrJournalMismatch, j.id, opts.ID)
	}
	j.prune()
	j.lastPrune = opts.Now()
	if err := j.compact(); err != nil {
		j.closeFile()
		_ = lock.release()
		return nil, err
	}
	return j, nil
}

func (j *Journal) path() string { return filepath.Join(j.dir, JournalFileName) }

func (j *Journal) closeFile() {
	if j.file != nil {
		_ = j.file.Close()
		j.file = nil
	}
}

// load reads every record, validates the sequence and truncates a torn tail.
func (j *Journal) load() error {
	// Never O_CREATE: only InitJournal makes a journal.
	linked, err := os.Lstat(j.path())
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s has no %s; run `shoal-effects-ssh journal init` "+
			"once, on the volume meant to hold it", ErrJournalUninitialized, j.dir, JournalFileName)
	}
	if err != nil {
		return fmt.Errorf("journal cannot be opened: %w", err)
	}
	if !linked.Mode().IsRegular() {
		// A symlink would be followed here and replaced by compaction's
		// rename, leaving the live journal off the volume and the old one
		// behind the link: the journal directory must itself be the volume.
		return fmt.Errorf("%s is not a regular file; the journal directory must be the "+
			"volume itself, not hold a link to it", j.path())
	}
	file, err := os.OpenFile(j.path(), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("journal cannot be opened: %w", err)
	}
	if opened, err := file.Stat(); err != nil || !os.SameFile(linked, opened) {
		_ = file.Close()
		return fmt.Errorf("%s changed while it was opened", j.path())
	}
	j.file = file
	good, err := readRecords(file, func(r record, at span) error { return j.apply(r, at) })
	if err != nil {
		j.closeFile()
		return err
	}
	if j.id == "" {
		// Empty, or only a fragment of the header: InitJournal never
		// finished. Nothing was ever journaled, but nothing proves this is
		// the journal it should be either, so it is refused, not adopted.
		j.closeFile()
		return fmt.Errorf("%w: %s has no complete header; remove it and run journal init "+
			"only if this volume never held a journal", ErrJournalUninitialized, j.path())
	}
	info, err := file.Stat()
	if err != nil {
		j.closeFile()
		return err
	}
	if info.Size() != good {
		// A torn tail: the last write did not finish. Whatever it was, it was
		// never acknowledged, and dropping it leaves a state the rules above
		// already answer safely.
		if err := file.Truncate(good); err != nil {
			j.closeFile()
			return fmt.Errorf("journal tail cannot be truncated: %w", err)
		}
		if err := syncFile(file); err != nil {
			j.closeFile()
			return fmt.Errorf("journal cannot be synced: %w", err)
		}
	}
	j.size = good
	return nil
}

// readRecords calls visit for each complete record and returns the offset
// after the last one. Only an incomplete final line (no LF) is a torn tail;
// a complete line that fails its checksum is corruption.
//
// On corruption the error is a *CorruptError naming the line; every record
// before it has already been visited.
func readRecords(file io.ReadSeeker, visit func(record, span) error) (int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	var offset int64
	for number := 1; ; number++ {
		corrupt := func(err error) error {
			return &CorruptError{Offset: offset, Line: number, Reason: err.Error()}
		}
		line, err := readLine(reader)
		if errors.Is(err, io.EOF) {
			return offset, nil
		}
		if errors.Is(err, errTorn) {
			return offset, nil
		}
		if errors.Is(err, errLineBound) {
			return offset, corrupt(err)
		}
		if err != nil {
			// A read error is not corruption: the file may be fine.
			return offset, fmt.Errorf("journal cannot be read at offset %d: %w", offset, err)
		}
		parsed, err := parseLine(line)
		if err != nil {
			return offset, corrupt(err)
		}
		at := span{offset: offset, length: int64(len(line))}
		if err := visit(parsed, at); err != nil {
			if errors.Is(err, ErrJournalMismatch) {
				return offset, err
			}
			return offset, corrupt(err)
		}
		offset += int64(len(line))
	}
}

var errTorn = errors.New("torn tail")

var errLineBound = errors.New("record exceeds the line bound")

// readLine returns one line including its LF. A final fragment without LF is
// errTorn; a line longer than maxRecordLine is an error.
func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxRecordLine {
			return nil, errLineBound
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, errTorn
		default:
			return nil, err
		}
	}
}

func encodeLine(r record) ([]byte, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	sum := crc32.Checksum(payload, castagnoli)
	line := make([]byte, 0, len(payload)+10)
	line = fmt.Appendf(line, "%08x ", sum)
	line = append(line, payload...)
	return append(line, '\n'), nil
}

func parseLine(line []byte) (record, error) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	if len(line) < 10 || line[8] != ' ' {
		return record{}, errors.New("malformed record")
	}
	want, err := hex.DecodeString(string(line[:8]))
	if err != nil {
		return record{}, errors.New("malformed checksum")
	}
	payload := line[9:]
	sum := crc32.Checksum(payload, castagnoli)
	if uint32(want[0])<<24|uint32(want[1])<<16|uint32(want[2])<<8|uint32(want[3]) != sum {
		return record{}, errors.New("checksum mismatch")
	}
	var r record
	if err := decodeStrict(payload, &r); err != nil {
		return record{}, errors.New("record does not decode")
	}
	return r, nil
}

// apply folds one record into the state, refusing contradictions.
func (j *Journal) apply(r record, at span) error {
	if r.Op == recordHeader {
		if at.offset != 0 || j.id != "" {
			return errors.New("a journal header appears after the first line")
		}
		if !ValidJournalID(r.JournalID) || r.At.IsZero() || r.Key != "" ||
			r.Command != "" || r.Fingerprint != "" || r.Finished != nil {
			return errors.New("malformed journal header")
		}
		if j.opts.ID != "" && j.opts.ID != r.JournalID {
			// Checked here, at the first line, so nothing of a journal that is
			// not the pinned one is read further or truncated.
			return fmt.Errorf("%w: the directory holds journal %s, and %s was pinned",
				ErrJournalMismatch, r.JournalID, j.opts.ID)
		}
		j.id, j.header = r.JournalID, at
		return nil
	}
	if j.id == "" {
		return errors.New("the first line is not a journal header")
	}
	if r.JournalID != "" {
		return errors.New("a record carries a journal ID")
	}
	if !validJournalKey(r.Key) || r.At.IsZero() {
		return errors.New("record has no valid key or time")
	}
	switch r.Op {
	case recordStarted:
		if r.Finished != nil || !ValidCommandName(r.Command) || len(r.Fingerprint) != 64 {
			return errors.New("malformed started record")
		}
		if previous, exists := j.entries[r.Key]; exists {
			// The writer appends a second started record for a key only
			// after a prune forgot it, and the earlier records stay until a
			// compaction. The file's order is authoritative: the later
			// record supersedes. The prune was decided under that process's
			// retention and clock, so neither is re-judged here; only an
			// exact repeat of the same record is corruption.
			if previous.StartedAt.Equal(r.At) {
				return errors.New("a key is started twice")
			}
		}
		j.entries[r.Key] = &Entry{Key: r.Key, Command: r.Command,
			Fingerprint: r.Fingerprint, StartedAt: r.At, started: at}
	case recordFinished:
		entry, exists := j.entries[r.Key]
		if !exists || entry.Finished != nil {
			return errors.New("a finished record has no started record, or repeats one")
		}
		if r.Finished == nil || r.Command != "" || r.Fingerprint != "" || !validFinished(r.Finished) {
			return errors.New("malformed finished record")
		}
		finished := *r.Finished
		finished.Stdout, finished.Stderr = nil, nil
		entry.Finished, entry.FinishedAt, entry.finished = &finished, r.At, at
	default:
		return errors.New("unknown record type")
	}
	j.records++
	return nil
}

func validFinished(f *Finished) bool {
	switch f.Outcome {
	case OutcomeCompleted:
		return f.Reference != "" && validReference(f.Reference)
	case OutcomeNotRun, OutcomeUnknown:
		return f.Reference == ""
	}
	return false
}

// BeginKind is what Begin decided.
type BeginKind int

const (
	// BeginRun: the key is new and its started record is durable. The caller
	// must run the command and then call Finish exactly once.
	BeginRun BeginKind = iota + 1
	// BeginFinished: the key has a durable result; replay it.
	BeginFinished
	// BeginRunning: this process is running the key; wait on Done.
	BeginRunning
	// BeginUnknown: the key started and has no durable result, and nothing in
	// this process is running it. It never runs again.
	BeginUnknown
	// BeginConflict: the key is known under a different request.
	BeginConflict
	// BeginUnregistered: the key is new and the command is not registered.
	BeginUnregistered
	// BeginBusy: the key is new and the concurrency bound is reached.
	BeginBusy
	// BeginFull: the key is new and the journal holds MaxEntries live keys.
	BeginFull
	// BeginUnavailable: the key is new and the journal cannot record it.
	BeginUnavailable
)

// Begin is the result of Journal.Begin.
type Begin struct {
	Kind  BeginKind
	Entry Entry
	Done  <-chan struct{}
}

// Begin decides what a request for key does. For a known key the journal
// alone decides, whatever the command's registration is now: a command
// removed from the configuration after it ran still answers its result. For
// a new key, registered says whether the command exists and maxRunning
// bounds concurrent runs; the started record is durable before BeginRun is
// returned.
func (j *Journal) Begin(key, fingerprint, command string, registered bool, maxRunning int) Begin {
	j.mu.Lock()
	defer j.mu.Unlock()
	if entry, known := j.entries[key]; known {
		switch {
		case entry.Fingerprint != fingerprint:
			return Begin{Kind: BeginConflict}
		case entry.Finished != nil:
			return Begin{Kind: BeginFinished, Entry: entry.snapshot()}
		case entry.running:
			return Begin{Kind: BeginRunning, Entry: entry.snapshot(), Done: entry.done}
		default:
			return Begin{Kind: BeginUnknown, Entry: entry.snapshot()}
		}
	}
	switch {
	case !registered:
		return Begin{Kind: BeginUnregistered}
	case j.failed != nil || j.closed:
		return Begin{Kind: BeginUnavailable}
	case j.running >= maxRunning:
		return Begin{Kind: BeginBusy}
	}
	if len(j.entries) >= j.opts.MaxEntries {
		j.prune()
		j.compactIfDead()
		if j.failed != nil {
			return Begin{Kind: BeginUnavailable}
		}
		if len(j.entries) >= j.opts.MaxEntries {
			return Begin{Kind: BeginFull}
		}
	}
	now := j.opts.Now().UTC()
	at, err := j.append(record{Op: recordStarted, Key: key, At: now,
		Command: command, Fingerprint: fingerprint})
	if err != nil {
		// Not durable, or not known to be: nothing is dialed. Whether a line
		// reached the disk is unknown, and if it did the key answers outcome
		// unknown after a restart, which overstates and never re-runs.
		return Begin{Kind: BeginUnavailable}
	}
	entry := &Entry{Key: key, Command: command, Fingerprint: fingerprint,
		StartedAt: now, started: at, running: true, done: make(chan struct{})}
	j.entries[key] = entry
	j.running++
	return Begin{Kind: BeginRun, Entry: entry.snapshot(), Done: entry.done}
}

// Finish records how a run ended and releases its waiters. It returns the
// entry as it now stands: with Finished set when the record is durable, and
// without when it is not, in which case the key answers outcome unknown from
// here on and the journal is failed.
func (j *Journal) Finish(key string, finished Finished) Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[key]
	if !ok || !entry.running {
		return Entry{}
	}
	defer func() {
		entry.running = false
		j.running--
		close(entry.done)
	}()
	if j.failed != nil || j.closed {
		return entry.snapshot()
	}
	now := j.opts.Now().UTC()
	at, err := j.append(record{Op: recordFinished, Key: key, At: now, Finished: &finished})
	if err != nil {
		return entry.snapshot()
	}
	kept := finished
	kept.Stdout, kept.Stderr = nil, nil
	entry.Finished, entry.FinishedAt, entry.finished = &kept, now, at
	j.maybeCompact()
	return entry.snapshot()
}

// Lookup returns a key's entry.
func (j *Journal) Lookup(key string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[key]
	if !ok {
		return Entry{}, false
	}
	return entry.snapshot(), true
}

// Len is the number of keys held.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}

// Failed is closed when the journal stops accepting new keys because a write
// failed. The process should drain and exit; a restart re-reads the file.
func (j *Journal) Failed() <-chan struct{} { return j.failCh }

// Err is the failure, if any.
func (j *Journal) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failed
}

// Close releases the file and the lock. Runs still in flight finish as
// unrecorded, which their keys answer as outcome unknown after a restart.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	j.closeFile()
	return j.lock.release()
}

// append writes one record and fsyncs it. Any failure fails the journal for
// good: after a failed write or fsync the file's state is not known, and
// Linux may report an fsync error once and then forget it.
func (j *Journal) append(r record) (span, error) {
	line, err := encodeLine(r)
	if err != nil {
		return span{}, err
	}
	if len(line) > maxRecordLine {
		return span{}, j.fail(errors.New("record exceeds the line bound"))
	}
	n, err := j.file.WriteAt(line, j.size)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = syncFile(j.file)
	}
	if err != nil {
		return span{}, j.fail(fmt.Errorf("journal write: %w", err))
	}
	at := span{offset: j.size, length: int64(len(line))}
	j.size += int64(len(line))
	j.records++
	return at, nil
}

func (j *Journal) fail(err error) error {
	if j.failed == nil {
		j.failed = err
		close(j.failCh)
	}
	return err
}

// prune forgets keys whose last record is older than retention. A key being
// run is never pruned.
func (j *Journal) prune() {
	cutoff := j.opts.Now().Add(-j.opts.Retention)
	for key, entry := range j.entries {
		if !entry.running && entry.last().Before(cutoff) {
			delete(j.entries, key)
		}
	}
}

func (j *Journal) live() int {
	n := 0
	for _, entry := range j.entries {
		n++
		if entry.Finished != nil {
			n++
		}
	}
	return n
}

// pruneInterval bounds how often a finish walks the keys. Records only die
// by pruning, so this is also how often compaction is considered.
const pruneInterval = time.Minute

func (j *Journal) maybeCompact() {
	now := j.opts.Now()
	if since := now.Sub(j.lastPrune); since >= 0 && since < pruneInterval {
		return
	}
	j.lastPrune = now
	j.prune()
	j.compactIfDead()
}

// compactIfDead rewrites the file once dead records outnumber the live ones
// by compactSlack; every prune is followed by it, which is what bounds the
// file (JournalBound).
func (j *Journal) compactIfDead() {
	if live := j.live(); j.records-live <= live+compactSlack {
		return
	}
	if err := j.compact(); err != nil {
		_ = j.fail(fmt.Errorf("journal compaction: %w", err))
	}
}

// compact rewrites the file with only the live records, copied byte for byte
// from the current file, through a synced temporary file and a rename.
func (j *Journal) compact() error {
	if j.records == j.live() {
		return nil
	}
	temporary := j.path() + ".compact"
	// A leftover temporary (a crash mid-compaction) is removed, and the new
	// one is created exclusively, so a planted link cannot redirect the write.
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(temporary, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func(err error) error {
		_ = out.Close()
		_ = os.Remove(temporary)
		return err
	}
	type move struct {
		entry *Entry
		which *span
	}
	var moves []move
	for _, entry := range j.entries {
		moves = append(moves, move{entry, &entry.started})
		if entry.Finished != nil {
			moves = append(moves, move{entry, &entry.finished})
		}
	}
	// Order by original offset, so every started record precedes its
	// finished record in the new file as it did in the old.
	sort.Slice(moves, func(a, b int) bool { return moves[a].which.offset < moves[b].which.offset })
	// The header is always the first line, copied as it is.
	header := make([]byte, j.header.length)
	if _, err := j.file.ReadAt(header, j.header.offset); err != nil {
		return cleanup(err)
	}
	if parsed, err := parseLine(header); err != nil || parsed.Op != recordHeader || parsed.JournalID != j.id {
		return cleanup(fmt.Errorf("%w: the journal header no longer verifies", ErrJournalCorrupt))
	}
	if _, err := out.WriteAt(header, 0); err != nil {
		return cleanup(err)
	}
	offset := int64(len(header))
	newSpans := make([]span, len(moves))
	for index, m := range moves {
		buffer := make([]byte, m.which.length)
		if _, err := j.file.ReadAt(buffer, m.which.offset); err != nil {
			return cleanup(err)
		}
		if _, err := parseLine(buffer); err != nil {
			return cleanup(fmt.Errorf("%w: a live record no longer verifies", ErrJournalCorrupt))
		}
		if _, err := out.WriteAt(buffer, offset); err != nil {
			return cleanup(err)
		}
		newSpans[index] = span{offset: offset, length: m.which.length}
		offset += m.which.length
	}
	if err := syncFile(out); err != nil {
		return cleanup(err)
	}
	if err := os.Rename(temporary, j.path()); err != nil {
		return cleanup(err)
	}
	if err := syncDir(j.dir); err != nil {
		// The rename may or may not be durable; either file is complete, but
		// the spans below would be wrong for the old one. Fail.
		_ = out.Close()
		return err
	}
	j.closeFile()
	j.file = out
	j.header = span{offset: 0, length: int64(len(header))}
	for index, m := range moves {
		*m.which = newSpans[index]
	}
	j.size, j.records = offset, len(moves)
	return nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("journal directory cannot be opened: %w", err)
	}
	defer handle.Close()
	if err := syncFile(handle); err != nil {
		return fmt.Errorf("journal directory cannot be synced: %w", err)
	}
	return nil
}

// ReadJournal reads a journal file without locking or changing it, for the
// operator's journal command. Records are returned in file order with their
// output, and a torn tail is reported, not dropped.
//
// The records are checked by the same rules OpenJournal applies. When the
// file is corrupt, every record before the bad line is returned together with
// a *CorruptError giving its line, offset and reason: in that state nothing
// runs until an operator has looked, and this is what they look with.
func ReadJournal(dir string) ([]JournalRecord, bool, error) {
	file, err := os.Open(filepath.Join(dir, JournalFileName))
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	var out []JournalRecord
	check := &Journal{entries: map[string]*Entry{}}
	good, err := readRecords(file, func(r record, at span) error {
		if err := check.apply(r, at); err != nil {
			return err
		}
		out = append(out, JournalRecord{Op: r.Op, JournalID: r.JournalID, Key: r.Key,
			At: r.At, Command: r.Command, Finished: r.Finished})
		return nil
	})
	if err != nil {
		return out, false, err
	}
	if check.id == "" {
		return nil, false, fmt.Errorf("%w: %s has no complete header; the server refuses it",
			ErrJournalUninitialized, filepath.Join(dir, JournalFileName))
	}
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	return out, info.Size() != good, nil
}

// MaxRecordBytes bounds the two records of one key when no stream keeps more
// than maxOutputBytes: the started record, and the finished record with both
// streams in base64.
func MaxRecordBytes(maxOutputBytes int) int64 {
	encoded := int64(base64.StdEncoding.EncodedLen(maxOutputBytes))
	return 2*encoded + 2048
}

// JournalBound bounds the journal file: at most maxEntries live keys, and at
// most as many dead records again plus compactSlack before a compaction
// rewrites the file.
func JournalBound(maxEntries, maxOutputBytes int) int64 {
	return int64(2*maxEntries+compactSlack) * MaxRecordBytes(maxOutputBytes)
}

// JournalRecord is one record as ReadJournal returns it.
type JournalRecord struct {
	Op        string    `json:"op"`
	JournalID string    `json:"journal_id,omitempty"`
	Key       string    `json:"key,omitempty"`
	At        time.Time `json:"at"`
	Command   string    `json:"command,omitempty"`
	Finished  *Finished `json:"finished,omitempty"`
}

// InitJournal creates a journal in dir, which must exist and hold none, and
// returns its identity. It is the only way a journal comes to exist: the
// server refuses a directory without one (ErrJournalUninitialized), so a
// volume that failed to mount presents as an error, not as an empty journal.
// The header is fsync'd, and so is the directory entry, before it returns.
func InitJournal(dir string, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("journal directory must exist")
	}
	lock, err := lockDir(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.release() }()
	path := filepath.Join(dir, JournalFileName)
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists; a journal is initialized once, and an "+
			"existing one is never replaced", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	line, err := encodeLine(record{Op: recordHeader, JournalID: id, At: now().UTC()})
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := syncFile(file); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return id, nil
}

// MaxCompactionBytes bounds what a compaction may copy. Compaction holds the
// journal lock while it copies the live records, so every request waits for
// it; bounding the live records bounds that stall to a copy of at most this
// much (a second or so on a local SSD). The gateway sees only latency, and
// retries or records unknown, never a wrong answer.
const MaxCompactionBytes = 512 << 20

// CheckJournalBounds refuses an entry bound and output bound whose live
// records could exceed MaxCompactionBytes.
func CheckJournalBounds(maxEntries, maxOutputBytes int) error {
	if live := int64(maxEntries) * MaxRecordBytes(maxOutputBytes); live > MaxCompactionBytes {
		return fmt.Errorf("-journal-max-entries %d with a largest max_output_bytes of %d "+
			"allows %d bytes of live records, above the %d a compaction may copy while "+
			"requests wait; lower one of them", maxEntries, maxOutputBytes, live, MaxCompactionBytes)
	}
	return nil
}
