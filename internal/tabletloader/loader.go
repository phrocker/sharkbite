// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

// Package tabletloader builds the immutable specification needed to open one
// manager-assigned tablet. It performs no ingest and owns no manager RPCs.
package tabletloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

var (
	ErrMissingMetadata   = errors.New("tabletloader: tablet metadata missing")
	ErrCorruptMetadata   = errors.New("tabletloader: tablet metadata corrupt")
	ErrMissingConfig     = errors.New("tabletloader: table configuration missing")
	ErrCorruptConfig     = errors.New("tabletloader: table configuration corrupt")
	ErrInvalidReference  = errors.New("tabletloader: invalid storage reference")
	ErrStaleGeneration   = errors.New("tabletloader: stale assignment generation")
	ErrRetriesExhausted  = errors.New("tabletloader: retries exhausted")
	ErrInvalidDependency = errors.New("tabletloader: invalid dependency")
)

// Generation is an opaque assignment fence. Implementations normally use the
// exact serialized ServiceLock identity stored in the metadata future/loc
// column, optionally combined with their local Host attempt identifier.
type Generation string

// Authority captures and revalidates the assignment generation. Validate must
// fail once unload, lock loss, reassignment, or cancellation makes generation
// stale.
type Authority interface {
	Capture(context.Context, tserver.Extent) (Generation, error)
	Validate(context.Context, tserver.Extent, Generation) error
}

// MetadataSource reads exactly one tablet row from the authoritative metadata
// table. Revision is an opaque source revision useful for diagnostics.
type MetadataSource interface {
	ReadTablet(context.Context, tserver.Extent) (MetadataSnapshot, error)
}

type MetadataSnapshot struct {
	Tablet     metadata.TabletInfo
	Revision   string
	Generation Generation
}

// ConfigSource returns the effective, inherited table configuration and the
// table-property generation used to construct it.
type ConfigSource interface {
	ReadTableConfiguration(context.Context, string) (ConfigurationSnapshot, error)
}

type ConfigurationSnapshot struct {
	TableID    string
	Generation int64
	Properties map[string]string
}

// FileResolver validates and canonicalizes one metadata data-file reference.
// Production implementations may also probe the selected storage backend.
type FileResolver interface {
	ResolveDataFile(context.Context, string, metadata.FileEntry) (DataFile, error)
}

// LogResolver validates and canonicalizes one WAL metadata reference.
type LogResolver interface {
	ResolveLog(context.Context, string, metadata.LogEntry) (Log, error)
}

type Property struct {
	Name  string
	Value string
}

type DataFile struct {
	Path         string
	StartRow     string
	EndRow       string
	Size         int64
	NumEntries   int64
	Time         int64
	Embedding    embeddingspace.FileState
	RawQualifier []byte
}

type Log struct {
	UUID         string
	Path         string
	Server       string
	Peers        []string
	RawQualifier []byte
}

// Specification is a validated point-in-time recipe for opening a tablet.
// Every collection is deterministically sorted.
type Specification struct {
	Extent           tserver.Extent
	Generation       Generation
	MetadataRevision string
	ConfigGeneration int64
	Directory        string
	Time             string
	Properties       []Property
	Files            []DataFile
	Logs             []Log
}

type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 250 * time.Millisecond}
}

type Config struct {
	Authority Authority
	Metadata  MetadataSource
	Config    ConfigSource
	Files     FileResolver
	Logs      LogResolver
	Retry     RetryPolicy
}

type Loader struct {
	authority Authority
	metadata  MetadataSource
	config    ConfigSource
	files     FileResolver
	logs      LogResolver
	retry     RetryPolicy
}

func New(cfg Config) (*Loader, error) {
	switch {
	case cfg.Authority == nil:
		return nil, fmt.Errorf("%w: nil authority", ErrInvalidDependency)
	case cfg.Metadata == nil:
		return nil, fmt.Errorf("%w: nil metadata source", ErrInvalidDependency)
	case cfg.Config == nil:
		return nil, fmt.Errorf("%w: nil config source", ErrInvalidDependency)
	case cfg.Files == nil:
		return nil, fmt.Errorf("%w: nil file resolver", ErrInvalidDependency)
	case cfg.Logs == nil:
		return nil, fmt.Errorf("%w: nil log resolver", ErrInvalidDependency)
	}
	if cfg.Retry.MaxAttempts == 0 {
		cfg.Retry = DefaultRetryPolicy()
	}
	if cfg.Retry.MaxAttempts < 1 || cfg.Retry.InitialBackoff < 0 || cfg.Retry.MaxBackoff < 0 {
		return nil, fmt.Errorf("%w: invalid retry policy", ErrInvalidDependency)
	}
	if cfg.Retry.MaxBackoff > 0 && cfg.Retry.InitialBackoff > cfg.Retry.MaxBackoff {
		return nil, fmt.Errorf("%w: initial backoff exceeds maximum", ErrInvalidDependency)
	}
	return &Loader{
		authority: cfg.Authority,
		metadata:  cfg.Metadata,
		config:    cfg.Config,
		files:     cfg.Files,
		logs:      cfg.Logs,
		retry:     cfg.Retry,
	}, nil
}

// Load resolves one tablet under a single captured assignment generation. A
// retry repeats the full read, never just a suffix, and generation is checked
// between every external operation and before the result is returned.
func (l *Loader) Load(ctx context.Context, extent tserver.Extent) (Specification, error) {
	if err := ctx.Err(); err != nil {
		return Specification{}, err
	}
	if err := extent.Validate(); err != nil {
		return Specification{}, err
	}
	generation, err := l.authority.Capture(ctx, extent)
	if err != nil {
		return Specification{}, normalizeFenceError(err)
	}
	if generation == "" {
		return Specification{}, fmt.Errorf("%w: authority returned an empty generation", ErrStaleGeneration)
	}

	var last error
	backoff := l.retry.InitialBackoff
	for attempt := 1; attempt <= l.retry.MaxAttempts; attempt++ {
		if err := l.check(ctx, extent, generation); err != nil {
			return Specification{}, err
		}
		spec, err := l.loadOnce(ctx, extent, generation)
		if err == nil {
			return spec, nil
		}
		if !retryable(err) {
			return Specification{}, err
		}
		last = err
		if attempt == l.retry.MaxAttempts {
			break
		}
		if err := sleep(ctx, backoff); err != nil {
			return Specification{}, err
		}
		if backoff > 0 {
			backoff *= 2
			if l.retry.MaxBackoff > 0 && backoff > l.retry.MaxBackoff {
				backoff = l.retry.MaxBackoff
			}
		}
	}
	return Specification{}, fmt.Errorf("%w after %d attempts: %w", ErrRetriesExhausted, l.retry.MaxAttempts, last)
}

func (l *Loader) loadOnce(ctx context.Context, extent tserver.Extent, generation Generation) (Specification, error) {
	ms, err := l.metadata.ReadTablet(ctx, extent)
	if err != nil {
		return Specification{}, fmt.Errorf("read tablet metadata: %w", err)
	}
	if err := l.check(ctx, extent, generation); err != nil {
		return Specification{}, err
	}
	if err := validateMetadata(extent, generation, ms); err != nil {
		return Specification{}, err
	}

	cs, err := l.config.ReadTableConfiguration(ctx, extent.TableID)
	if err != nil {
		return Specification{}, fmt.Errorf("read table configuration: %w", err)
	}
	if err := l.check(ctx, extent, generation); err != nil {
		return Specification{}, err
	}
	if err := validateConfig(extent.TableID, cs); err != nil {
		return Specification{}, err
	}

	files := append([]metadata.FileEntry(nil), ms.Tablet.Files...)
	sort.Slice(files, func(i, j int) bool { return compareFileEntries(files[i], files[j]) < 0 })
	resolvedFiles := make([]DataFile, 0, len(files))
	for i, file := range files {
		resolved, err := l.files.ResolveDataFile(ctx, extent.TableID, file)
		if err != nil {
			return Specification{}, fmt.Errorf("resolve data file %d: %w", i, err)
		}
		if err := l.check(ctx, extent, generation); err != nil {
			return Specification{}, err
		}
		resolvedFiles = append(resolvedFiles, cloneDataFile(resolved))
	}
	if err := validateResolvedFiles(resolvedFiles); err != nil {
		return Specification{}, err
	}
	sort.Slice(resolvedFiles, func(i, j int) bool {
		a, b := resolvedFiles[i], resolvedFiles[j]
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c < 0
		}
		if c := strings.Compare(a.StartRow, b.StartRow); c != 0 {
			return c < 0
		}
		return a.EndRow < b.EndRow
	})

	logs := append([]metadata.LogEntry(nil), ms.Tablet.Logs...)
	sort.Slice(logs, func(i, j int) bool { return compareLogEntries(logs[i], logs[j]) < 0 })
	resolvedLogs := make([]Log, 0, len(logs))
	for i, log := range logs {
		resolved, err := l.logs.ResolveLog(ctx, extent.TableID, log)
		if err != nil {
			return Specification{}, fmt.Errorf("resolve WAL %d: %w", i, err)
		}
		if err := l.check(ctx, extent, generation); err != nil {
			return Specification{}, err
		}
		resolvedLogs = append(resolvedLogs, cloneLog(resolved))
	}
	if err := validateResolvedLogs(resolvedLogs); err != nil {
		return Specification{}, err
	}
	sort.Slice(resolvedLogs, func(i, j int) bool {
		if c := strings.Compare(resolvedLogs[i].Path, resolvedLogs[j].Path); c != 0 {
			return c < 0
		}
		return resolvedLogs[i].UUID < resolvedLogs[j].UUID
	})
	if err := l.check(ctx, extent, generation); err != nil {
		return Specification{}, err
	}

	return Specification{
		Extent:           cloneExtent(extent),
		Generation:       generation,
		MetadataRevision: ms.Revision,
		ConfigGeneration: cs.Generation,
		Directory:        ms.Tablet.Directory,
		Time:             ms.Tablet.Time,
		Properties:       sortedProperties(cs.Properties),
		Files:            resolvedFiles,
		Logs:             resolvedLogs,
	}, nil
}

func (l *Loader) check(ctx context.Context, extent tserver.Extent, generation Generation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.authority.Validate(ctx, extent, generation); err != nil {
		return normalizeFenceError(err)
	}
	return nil
}

func validateMetadata(extent tserver.Extent, generation Generation, snapshot MetadataSnapshot) error {
	info := snapshot.Tablet
	if info.TableID == "" {
		return fmt.Errorf("%w: %s", ErrMissingMetadata, extent)
	}
	got := tserver.Extent{TableID: info.TableID, PrevEndRow: info.PrevRow, EndRow: info.EndRow}
	if err := got.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptMetadata, err)
	}
	if !extent.Equal(got) {
		return fmt.Errorf("%w: requested %s, metadata returned %s", ErrCorruptMetadata, extent, got)
	}
	if !info.PrevRowSet {
		return fmt.Errorf("%w: %s has no ~tab:~pr column", ErrCorruptMetadata, extent)
	}
	if info.Directory == "" {
		return fmt.Errorf("%w: %s has no srv:dir column", ErrCorruptMetadata, extent)
	}
	if info.Time == "" {
		return fmt.Errorf("%w: %s has no srv:time column", ErrCorruptMetadata, extent)
	}
	if !validDirectory(info.Directory) {
		return fmt.Errorf("%w: invalid srv:dir %q", ErrCorruptMetadata, info.Directory)
	}
	if !validMetadataTime(info.Time) {
		return fmt.Errorf("%w: invalid srv:time %q", ErrCorruptMetadata, info.Time)
	}
	if info.Location != nil && info.FutureLocation != nil {
		return fmt.Errorf("%w: %s has both loc and future assignments", ErrCorruptMetadata, extent)
	}
	location := info.FutureLocation
	if location == nil {
		location = info.Location
	}
	if location == nil || location.HostPort == "" || location.Session == "" {
		return fmt.Errorf("%w: %s has no complete loc/future assignment", ErrStaleGeneration, extent)
	}
	if snapshot.Generation == "" || snapshot.Generation != generation {
		return fmt.Errorf("%w: metadata generation %q does not match captured generation %q",
			ErrStaleGeneration, snapshot.Generation, generation)
	}
	if Generation(location.Session) != generation {
		return fmt.Errorf("%w: metadata location session %q does not match captured generation %q",
			ErrStaleGeneration, location.Session, generation)
	}
	return nil
}

func validateConfig(tableID string, snapshot ConfigurationSnapshot) error {
	if snapshot.TableID == "" {
		return fmt.Errorf("%w: table %s", ErrMissingConfig, tableID)
	}
	if snapshot.TableID != tableID {
		return fmt.Errorf("%w: requested table %q, configuration belongs to %q",
			ErrCorruptConfig, tableID, snapshot.TableID)
	}
	if snapshot.Generation < 0 {
		return fmt.Errorf("%w: negative generation %d", ErrCorruptConfig, snapshot.Generation)
	}
	if snapshot.Properties == nil {
		return fmt.Errorf("%w: nil effective properties for table %s", ErrCorruptConfig, tableID)
	}
	for k := range snapshot.Properties {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: empty property name", ErrCorruptConfig)
		}
	}
	return nil
}

func validateResolvedFiles(files []DataFile) error {
	seen := make(map[string]struct{}, len(files))
	for i, file := range files {
		if file.Embedding == (embeddingspace.FileState{}) {
			// A resolved file that carries no decoded embedding column
			// is unknown, never no_embeddings: this is a read path and
			// it has been told nothing.
			//
			// Only the exact zero value counts as "no column". A partial
			// state is malformed resolver output and must reach the
			// Validate below and be refused, not be quietly replaced.
			file.Embedding = embeddingspace.Unknown()
			files[i].Embedding = file.Embedding
		}
		if file.Path == "" || len(file.RawQualifier) == 0 ||
			file.Size < 0 || file.NumEntries < 0 || file.Time < -1 ||
			file.Embedding.Validate() != nil {
			return fmt.Errorf("%w: data file %d has invalid path or statistics", ErrInvalidReference, i)
		}
		key := file.Path + "\x00" + file.StartRow + "\x00" + file.EndRow
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%w: duplicate data file %q", ErrCorruptMetadata, file.Path)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateResolvedLogs(logs []Log) error {
	seen := make(map[string]struct{}, len(logs))
	for i, log := range logs {
		if log.UUID == "" || log.Path == "" || log.Server == "" || len(log.RawQualifier) == 0 {
			return fmt.Errorf("%w: WAL %d is incomplete", ErrInvalidReference, i)
		}
		if _, ok := seen[log.UUID]; ok {
			return fmt.Errorf("%w: duplicate WAL UUID %q", ErrCorruptMetadata, log.UUID)
		}
		seen[log.UUID] = struct{}{}
	}
	return nil
}

func validDirectory(dir string) bool {
	if dir == "" {
		return false
	}
	for _, r := range dir {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') &&
			(r < 'a' || r > 'z') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validMetadataTime(value string) bool {
	if len(value) < 2 || (value[0] != 'M' && value[0] != 'L') {
		return false
	}
	_, err := strconv.ParseInt(value[1:], 10, 64)
	return err == nil
}

func sortedProperties(props map[string]string) []Property {
	out := make([]Property, 0, len(props))
	for k, v := range props {
		out = append(out, Property{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func compareFileEntries(a, b metadata.FileEntry) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	if c := strings.Compare(a.StartRow, b.StartRow); c != 0 {
		return c
	}
	return strings.Compare(a.EndRow, b.EndRow)
}

func compareLogEntries(a, b metadata.LogEntry) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	return strings.Compare(a.UUID, b.UUID)
}

func normalizeFenceError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrStaleGeneration) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStaleGeneration, err)
}

type temporary interface{ Temporary() bool }

func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrMissingMetadata) || errors.Is(err, ErrCorruptMetadata) ||
		errors.Is(err, ErrMissingConfig) || errors.Is(err, ErrCorruptConfig) ||
		errors.Is(err, ErrInvalidReference) || errors.Is(err, ErrStaleGeneration) {
		return false
	}
	var temp temporary
	return errors.As(err, &temp) && temp.Temporary()
}

func sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func cloneExtent(e tserver.Extent) tserver.Extent {
	return tserver.Extent{
		TableID:    e.TableID,
		PrevEndRow: append([]byte(nil), e.PrevEndRow...),
		EndRow:     append([]byte(nil), e.EndRow...),
	}
}

func cloneDataFile(f DataFile) DataFile {
	f.RawQualifier = append([]byte(nil), f.RawQualifier...)
	return f
}

func cloneLog(l Log) Log {
	l.Peers = append([]string(nil), l.Peers...)
	sort.Strings(l.Peers)
	l.RawQualifier = append([]byte(nil), l.RawQualifier...)
	return l
}

// Equal compares specifications without depending on map iteration order.
func (s Specification) Equal(other Specification) bool {
	if !s.Extent.Equal(other.Extent) || s.Generation != other.Generation ||
		s.MetadataRevision != other.MetadataRevision ||
		s.ConfigGeneration != other.ConfigGeneration || s.Directory != other.Directory ||
		s.Time != other.Time || len(s.Properties) != len(other.Properties) ||
		len(s.Files) != len(other.Files) || len(s.Logs) != len(other.Logs) {
		return false
	}
	for i := range s.Properties {
		if s.Properties[i] != other.Properties[i] {
			return false
		}
	}
	for i := range s.Files {
		a, b := s.Files[i], other.Files[i]
		if a.Path != b.Path || a.StartRow != b.StartRow || a.EndRow != b.EndRow ||
			a.Size != b.Size || a.NumEntries != b.NumEntries || a.Time != b.Time ||
			a.Embedding != b.Embedding || !bytes.Equal(a.RawQualifier, b.RawQualifier) {
			return false
		}
	}
	for i := range s.Logs {
		a, b := s.Logs[i], other.Logs[i]
		if a.UUID != b.UUID || a.Path != b.Path || a.Server != b.Server ||
			!equalStrings(a.Peers, b.Peers) || !bytes.Equal(a.RawQualifier, b.RawQualifier) {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
