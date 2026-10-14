package rfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// KeyValueIterator is the read contract shared by every RFile stream: a
// cursor positioned by Seek that yields entries in key order until the range
// is exhausted. It is the Go equivalent of Sharkbite's KeyValueIterator, whose
// RFile, SequentialRFile and merged multi-file streams all implement the same
// five operations.
//
// The zero position is "no top": Top, TopKey, TopValue and Next report ErrNoTop
// before the first Seek and after exhaustion, where Sharkbite raises
// StopIteration.
type KeyValueIterator interface {
	// Seek positions the cursor at the start of the relocation's range,
	// restricted to its column families.
	Seek(ctx context.Context, target StreamRelocation) error

	// HasTop reports whether an entry is available.
	HasTop() bool

	// Top returns the current entry, including its tombstone flag.
	Top() (Entry, error)

	// TopKey returns the current entry's key.
	TopKey() (accumulo.Key, error)

	// TopValue returns the current entry's value.
	TopValue() ([]byte, error)

	// Next advances past the current entry.
	Next(ctx context.Context) error

	// Close releases the underlying files. It is idempotent.
	Close() error
}

// Reader is a positioned RFile stream over one or more files. Open returns a
// reader that seeks through the file index, OpenSequential returns one already
// positioned at the first entry, and OpenMany returns one that merges several
// files into a single ordered stream.
type Reader struct {
	mu       sync.Mutex
	source   iterrt.SortedKeyValueIterator
	closers  []func() error
	closed   bool
	closeErr error
	seeked   bool
}

var _ KeyValueIterator = (*Reader)(nil)

// Open opens path for random access, mirroring Sharkbite's
// RFileOperations.randomSeek. The reader starts unpositioned: call Seek before
// reading, exactly as Sharkbite's RFile requires.
func Open(ctx context.Context, path string) (*Reader, error) {
	source, closers, err := openSource(ctx, path)
	if err != nil {
		return nil, err
	}
	return &Reader{source: source, closers: closers}, nil
}

// OpenSequential opens path and positions it at the first entry, mirroring
// Sharkbite's RFileOperations.sequentialRead. Seek may still be called
// afterwards to reposition, as Sharkbite's SequentialRFile allows.
func OpenSequential(ctx context.Context, path string) (*Reader, error) {
	reader, err := Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := reader.Seek(ctx, EntireFile()); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return reader, nil
}

// MergeOptions controls how OpenMany combines several RFiles, mirroring the
// arguments of Sharkbite's RFileOperations.openManySequential(paths, versions,
// withDeletes, propogate, maxtimestamp) as declared at
// include/data/constructs/rfile/RFileOperations.h:55.
type MergeOptions struct {
	// Versions keeps at most this many versions of each key. Zero or
	// negative keeps every version, which is what Sharkbite's versions=0
	// does. Sharkbite ignores the number for any other value and always
	// keeps one version (VersioningIterator.cpp next() advances past every
	// further version of the key it just returned), so Versions:1
	// reproduces it and larger values are a Shoal extension.
	Versions int

	// ApplyDeletes runs the merged stream through the delete-aware iterator
	// so a tombstone suppresses the entries it covers. Sharkbite spells this
	// withDeletes: false selects a plain MultiIterator that yields tombstones
	// and the entries under them untouched, true selects DeletingMultiIterator
	// (RFileOperations.h:162-171).
	ApplyDeletes bool

	// Propagate keeps a tombstone in the output stream after it has been
	// applied, which every read except the last compaction of a tablet must
	// do so the next level can suppress against files this stack never saw.
	// It is only consulted when ApplyDeletes is true.
	//
	// Sharkbite's propogate=true instead short-circuits delete handling
	// entirely (DeletingMultiIterator.h:49-51 calls multiNext with no
	// suppression), so its documented default openManySequential(paths, 0,
	// True, True, 0) applies no delete at all. Shoal follows Accumulo's
	// DeletingIterator, which is why this option is described as "keep the
	// tombstone", not "ignore deletes"; see the compatibility matrix row
	// SB-RFILE-004.
	Propagate bool

	// MinTimestamp drops entries stamped before it: an age-off cutoff.
	//
	// Sharkbite spells this maxtimestamp, but wires it to
	// AgeOffCondition.earliest_allowed_timestamp, and its evaluator filters a
	// key when getTimeStamp() < earliest_allowed_timestamp
	// (AgeOffConditions.h:82-88) — a minimum, not a maximum. In the pinned
	// Sharkbite the argument is unusable either way: any non-zero value makes
	// the AgeOffEvaluator constructor throw "Cannot have more than one default
	// condition" (AgeOffConditions.h:52-57), so openManySequential fails.
	// Shoal implements the intended age-off instead of reproducing the throw;
	// zero, the default, keeps every timestamp.
	MinTimestamp int64
}

// OpenMany opens every path and merges them into one ordered stream,
// mirroring Sharkbite's RFileOperations.openManySequential. The returned
// reader is already positioned at the first entry, and Close closes every
// file the call opened.
func OpenMany(ctx context.Context, paths []string, opts MergeOptions) (*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: at least one path is required", ErrInvalidPath)
	}

	sources := make([]iterrt.SortedKeyValueIterator, 0, len(paths))
	closers := make([]func() error, 0, len(paths))
	fail := func(err error) (*Reader, error) {
		for _, closer := range closers {
			_ = closer()
		}
		return nil, err
	}
	for _, path := range paths {
		source, fileClosers, err := openSource(ctx, path)
		if err != nil {
			return fail(err)
		}
		closers = append(closers, fileClosers...)
		if opts.MinTimestamp > 0 {
			// Sharkbite ages off per file, before the merge
			// (RFileOperations.h:113 setAgeOff on each stream).
			source = newAgeOffFilter(source, opts.MinTimestamp)
		}
		sources = append(sources, source)
	}

	merged := iterrt.NewMergingIterator(sources...)
	if err := merged.Init(nil, nil, readEnvironment()); err != nil {
		return fail(fmt.Errorf("rfile: merge %d files: %w", len(paths), err))
	}

	var stream iterrt.SortedKeyValueIterator = merged
	if opts.Versions > 0 {
		versioning := iterrt.NewVersioningIterator()
		options := map[string]string{iterrt.VersioningOption: strconv.Itoa(opts.Versions)}
		if err := versioning.Init(stream, options, readEnvironment()); err != nil {
			return fail(fmt.Errorf("rfile: limit to %d versions: %w", opts.Versions, err))
		}
		stream = versioning
	}
	if opts.ApplyDeletes {
		deleting := iterrt.NewDeletingIterator()
		options := map[string]string{
			iterrt.DeletingOptionPropagate: strconv.FormatBool(opts.Propagate),
		}
		if err := deleting.Init(stream, options, readEnvironment()); err != nil {
			return fail(fmt.Errorf("rfile: apply deletes: %w", err))
		}
		stream = deleting
	}

	reader := &Reader{source: stream, closers: closers}
	if err := reader.Seek(ctx, EntireFile()); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return reader, nil
}

// readEnvironment is the iterator environment for a client-side read: scan
// scope, never a full major compaction, so delete-aware iterators keep the
// conservative behavior a client must have.
func readEnvironment() iterrt.IteratorEnvironment {
	return iterrt.IteratorEnvironment{Scope: iterrt.ScopeScan}
}

// openSource opens one RFile and wraps it as a leaf iterator. An RFile may
// hold several locality groups, each sorted within itself, so every group is
// opened and merged: a reader that walked the default group alone would
// silently omit the cells a named group holds.
func openSource(ctx context.Context, path string) (iterrt.SortedKeyValueIterator, []func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if path == "" {
		return nil, nil, fmt.Errorf("%w: path is required", ErrInvalidPath)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("rfile: open %s: %w", path, err)
	}
	fail := func(err error) (iterrt.SortedKeyValueIterator, []func() error, error) {
		_ = file.Close()
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("rfile: stat %s: %w", path, err))
	}
	bc, err := bcfile.NewReader(file, info.Size())
	if err != nil {
		return fail(fmt.Errorf("rfile: read %s: %w", path, err))
	}
	groups, err := rfile.OpenAll(bc, block.Default())
	if err != nil {
		return fail(fmt.Errorf("rfile: read %s: %w", path, err))
	}
	if len(groups) == 0 {
		return fail(fmt.Errorf("rfile: read %s: file has no locality groups", path))
	}

	closers := make([]func() error, 0, len(groups)+1)
	sources := make([]iterrt.SortedKeyValueIterator, 0, len(groups))
	for _, group := range groups {
		closers = append(closers, group.Close)
		source := iterrt.NewRFileSource(group, nil)
		if err := source.Init(nil, nil, readEnvironment()); err != nil {
			for _, closer := range closers {
				_ = closer()
			}
			return fail(fmt.Errorf("rfile: read %s: %w", path, err))
		}
		sources = append(sources, source)
	}
	closers = append(closers, file.Close)

	if len(sources) == 1 {
		return sources[0], closers, nil
	}
	merged := iterrt.NewMergingIterator(sources...)
	if err := merged.Init(nil, nil, readEnvironment()); err != nil {
		for _, closer := range closers {
			_ = closer()
		}
		return nil, nil, fmt.Errorf("rfile: read %s: merge %d locality groups: %w", path, len(sources), err)
	}
	return merged, closers, nil
}

// Seek positions the reader, mirroring Sharkbite's RFile.seek,
// SequentialRFile.seek and KeyValueIterator.seek, all of which take a
// StreamRelocation.
func (r *Reader) Seek(ctx context.Context, target StreamRelocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("%w: relocation is required", ErrInvalidSeekable)
	}
	keyRange := target.Range()
	if keyRange == nil {
		return fmt.Errorf("%w: range is required", ErrInvalidSeekable)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if err := r.source.Seek(internalRange(keyRange), target.ColumnFamilies(), target.Inclusive()); err != nil {
		return fmt.Errorf("rfile: seek: %w", err)
	}
	r.seeked = true
	return nil
}

// HasTop reports whether an entry is available.
func (r *Reader) HasTop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.seeked {
		return false
	}
	return r.source.HasTop()
}

// Top returns the current entry, mirroring Sharkbite's getTop. The key and
// value are copies, so the entry stays valid after Next.
func (r *Reader) Top() (Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkTop(); err != nil {
		return Entry{}, err
	}
	top := r.source.GetTopKey()
	return Entry{
		Key:     publicKey(top),
		Value:   append([]byte(nil), r.source.GetTopValue()...),
		Deleted: top != nil && top.Deleted,
	}, nil
}

// TopKey returns the current entry's key, mirroring Sharkbite's getTopKey.
// accumulo.Key has no tombstone flag; use Top when the flag matters.
func (r *Reader) TopKey() (accumulo.Key, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkTop(); err != nil {
		return accumulo.Key{}, err
	}
	return publicKey(r.source.GetTopKey()), nil
}

// TopValue returns the current entry's value, mirroring Sharkbite's
// getTopValue. The returned slice is a copy, so it stays valid after Next.
func (r *Reader) TopValue() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkTop(); err != nil {
		return nil, err
	}
	return append([]byte(nil), r.source.GetTopValue()...), nil
}

// Next advances past the current entry. It reports ErrNoTop at exhaustion,
// which is where Sharkbite raises StopIteration.
func (r *Reader) Next(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkTop(); err != nil {
		return err
	}
	if err := r.source.Next(); err != nil {
		return fmt.Errorf("rfile: next: %w", err)
	}
	return nil
}

// Close releases every file this reader opened. It is idempotent, and later
// operations report ErrClosed. A failed close is remembered, so every later
// Close reports the same error rather than a misleading success.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	var errs []error
	for _, closer := range r.closers {
		if err := closer(); err != nil {
			errs = append(errs, err)
		}
	}
	r.closers = nil
	if len(errs) > 0 {
		r.closeErr = fmt.Errorf("rfile: close: %w", errors.Join(errs...))
	}
	return r.closeErr
}

func (r *Reader) checkTop() error {
	if r.closed {
		return ErrClosed
	}
	if !r.seeked || !r.source.HasTop() {
		return ErrNoTop
	}
	return nil
}

// ageOffFilter drops entries stamped before a cutoff, implementing the age-off
// Sharkbite's fifth openManySequential argument configures.
type ageOffFilter struct {
	source       iterrt.SortedKeyValueIterator
	minTimestamp int64
}

func newAgeOffFilter(source iterrt.SortedKeyValueIterator, minTimestamp int64) *ageOffFilter {
	return &ageOffFilter{source: source, minTimestamp: minTimestamp}
}

func (f *ageOffFilter) Init(source iterrt.SortedKeyValueIterator, _ map[string]string, _ iterrt.IteratorEnvironment) error {
	if source != nil {
		f.source = source
	}
	return nil
}

func (f *ageOffFilter) Seek(r iterrt.Range, columnFamilies [][]byte, inclusive bool) error {
	if err := f.source.Seek(r, columnFamilies, inclusive); err != nil {
		return err
	}
	return f.skipAgedOff()
}

func (f *ageOffFilter) Next() error {
	if err := f.source.Next(); err != nil {
		return err
	}
	return f.skipAgedOff()
}

func (f *ageOffFilter) skipAgedOff() error {
	for f.source.HasTop() && f.source.GetTopKey().Timestamp < f.minTimestamp {
		if err := f.source.Next(); err != nil {
			return err
		}
	}
	return nil
}

func (f *ageOffFilter) HasTop() bool           { return f.source.HasTop() }
func (f *ageOffFilter) GetTopKey() *iterrt.Key { return f.source.GetTopKey() }
func (f *ageOffFilter) GetTopValue() []byte    { return f.source.GetTopValue() }
func (f *ageOffFilter) DeepCopy(env iterrt.IteratorEnvironment) iterrt.SortedKeyValueIterator {
	return &ageOffFilter{source: f.source.DeepCopy(env), minTimestamp: f.minTimestamp}
}

// internalRange converts the public range into the iterator runtime's form,
// resolving row bounds into the absolute key bounds a key-ordered reader needs.
func internalRange(r *accumulo.Range) iterrt.Range {
	start, startInclusive, end, endInclusive := r.KeyBounds()
	converted := iterrt.Range{
		StartInclusive: startInclusive,
		EndInclusive:   endInclusive,
	}
	if start != nil {
		converted.Start = internalKey(*start)
	} else {
		converted.InfiniteStart = true
	}
	if end != nil {
		converted.End = internalKey(*end)
	} else {
		converted.InfiniteEnd = true
	}
	return converted
}

func internalKey(key accumulo.Key) *wire.Key {
	return &wire.Key{
		Row:              append([]byte(nil), key.Row...),
		ColumnFamily:     append([]byte(nil), key.ColumnFamily...),
		ColumnQualifier:  append([]byte(nil), key.ColumnQualifier...),
		ColumnVisibility: append([]byte(nil), key.ColumnVisibility...),
		Timestamp:        key.Timestamp,
		Deleted:          key.Deleted,
	}
}

func publicKey(key *wire.Key) accumulo.Key {
	if key == nil {
		return accumulo.Key{}
	}
	return accumulo.Key{
		Row:              append([]byte(nil), key.Row...),
		ColumnFamily:     append([]byte(nil), key.ColumnFamily...),
		ColumnQualifier:  append([]byte(nil), key.ColumnQualifier...),
		ColumnVisibility: append([]byte(nil), key.ColumnVisibility...),
		Timestamp:        key.Timestamp,
		Deleted:          key.Deleted,
	}
}
