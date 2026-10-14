// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

// Package tserverprocess wires validated tablet specifications into the
// manager lifecycle and hosted-only scan surfaces.
package tserverprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/tserverrpc"
)

var (
	ErrNotHosted             = errors.New("tserverprocess: tablet is not hosted")
	ErrWALIntegrationMissing = errors.New("tserverprocess: hosted WAL integration is unavailable")
	ErrUnsupportedConstraint = errors.New("tserverprocess: ingest constraints are unsupported")
)

type SpecificationLoader interface {
	Load(context.Context, tserver.Extent) (tabletloader.Specification, error)
}

// Store is both the manager adapter backend and the scan server's tablet
// locator. A specification becomes visible to scans only after every metadata,
// generation, file, WAL, and iterator check succeeds.
type Store struct {
	loader SpecificationLoader
	opener TabletOpener

	mu      sync.RWMutex
	next    uint64
	loading map[string]uint64
	hosted  map[string]tabletloader.Specification
	ingest  map[string]ingestrouter.HostedTablet
}

type TabletOpener interface {
	Open(context.Context, tabletloader.Specification, tserver.Attempt) (ingestrouter.HostedTablet, error)
}

type tabletCloser interface {
	Close(context.Context) error
}

type tabletFlusher interface {
	Flush(context.Context) error
}

type tabletFiles interface {
	DataFiles() []mincauthority.DataFile
}

func NewStore(loader SpecificationLoader) (*Store, error) {
	if loader == nil {
		return nil, errors.New("tserverprocess: nil specification loader")
	}
	return &Store{
		loader:  loader,
		next:    1,
		loading: make(map[string]uint64),
		hosted:  make(map[string]tabletloader.Specification),
		ingest:  make(map[string]ingestrouter.HostedTablet),
	}, nil
}

func (s *Store) Load(ctx context.Context, extent tserver.Extent) error {
	return s.load(ctx, extent, tserver.Attempt{}, false)
}

func NewWritableStore(loader SpecificationLoader, opener TabletOpener) (*Store, error) {
	if opener == nil {
		return nil, errors.New("tserverprocess: nil tablet opener")
	}
	store, err := NewStore(loader)
	if err != nil {
		return nil, err
	}
	store.opener = opener
	return store, nil
}

func (s *Store) LoadAssigned(ctx context.Context, extent tserver.Extent, attempt tserver.Attempt) error {
	if s.opener == nil || !attempt.Valid() {
		return ErrWALIntegrationMissing
	}
	return s.load(ctx, extent, attempt, true)
}

func (s *Store) load(ctx context.Context, extent tserver.Extent, attempt tserver.Attempt, writable bool) error {
	key := extentKey(extent)
	s.mu.Lock()
	token := s.next
	s.next++
	s.loading[key] = token
	s.mu.Unlock()

	spec, err := s.loader.Load(ctx, extent)
	if err == nil {
		err = validateHostedScanStack(extent.TableID, propertiesMap(spec.Properties))
	}
	if err == nil && len(spec.Logs) > 0 && !writable {
		err = fmt.Errorf("%w: %s references %d WAL segment(s)",
			ErrWALIntegrationMissing, extent, len(spec.Logs))
	}
	var ingestTablet ingestrouter.HostedTablet
	if err == nil && writable {
		ingestTablet, err = s.opener.Open(ctx, spec, attempt)
		if err != nil {
			err = fmt.Errorf("open writable tablet: %w", err)
		}
	}

	s.mu.Lock()
	if current, ok := s.loading[key]; !ok || current != token {
		s.mu.Unlock()
		closeTablet(ingestTablet)
		return context.Canceled
	}
	delete(s.loading, key)
	if err != nil {
		s.mu.Unlock()
		closeTablet(ingestTablet)
		return err
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		closeTablet(ingestTablet)
		return err
	}
	s.hosted[key] = cloneSpecification(spec)
	if ingestTablet != nil {
		s.ingest[key] = ingestTablet
	}
	s.mu.Unlock()
	return nil
}

func closeTablet(tablet ingestrouter.HostedTablet) {
	if closer, ok := tablet.(tabletCloser); ok {
		_ = closer.Close(context.Background())
	}
}

func (s *Store) Unload(ctx context.Context, extent tserver.Extent, goal tserverrpc.UnloadGoal) error {
	if goal != tserverrpc.UnloadUnassigned {
		return tserverrpc.ErrUnsupported
	}
	return s.unload(ctx, extent)
}

func (s *Store) UnloadAssigned(
	ctx context.Context,
	extent tserver.Extent,
	_ tserver.Attempt,
	goal tserverrpc.UnloadGoal,
) error {
	if goal != tserverrpc.UnloadUnassigned {
		return tserverrpc.ErrUnsupported
	}
	return s.unload(ctx, extent)
}

func (s *Store) unload(ctx context.Context, extent tserver.Extent) error {
	key := extentKey(extent)
	s.mu.Lock()
	delete(s.loading, key)
	if _, ok := s.hosted[key]; !ok {
		s.mu.Unlock()
		return tserverrpc.ErrNotServing
	}
	tablet := s.ingest[key]
	s.mu.Unlock()
	if closer, ok := tablet.(tabletCloser); ok {
		if err := closer.Close(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	delete(s.hosted, key)
	delete(s.ingest, key)
	s.mu.Unlock()
	return nil
}

// Flush is a no-op for a read-only hosted tablet. No mutation, memtable, or
// WAL is accepted by this process, so there is no local state to flush.
func (s *Store) Flush(ctx context.Context, extent tserver.Extent) error {
	s.mu.RLock()
	if _, ok := s.hosted[extentKey(extent)]; !ok {
		s.mu.RUnlock()
		return tserverrpc.ErrNotServing
	}
	tablet := s.ingest[extentKey(extent)]
	s.mu.RUnlock()
	if flusher, ok := tablet.(tabletFlusher); ok {
		return flusher.Flush(ctx)
	}
	return nil
}

func (s *Store) Lookup(ctx context.Context, extent ingestrouter.Extent) (ingestrouter.HostedTablet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := extentKey(tserver.Extent{
		TableID: extent.TableID, PrevEndRow: extent.PrevEndRow, EndRow: extent.EndRow,
	})
	s.mu.RLock()
	tablet := s.ingest[key]
	s.mu.RUnlock()
	if tablet == nil {
		return nil, ingestrouter.ErrNotHosted
	}
	return tablet, nil
}

// LocateTable exposes only successfully hosted tablets. Stateful scan
// continuation data is materialized by scanserver before this snapshot can be
// removed, so an unload cannot invalidate an already-issued continuation.
func (s *Store) LocateTable(ctx context.Context, tableID string) ([]metadata.TabletInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	result := make([]metadata.TabletInfo, 0)
	for key, spec := range s.hosted {
		if spec.Extent.TableID == tableID {
			if files, ok := s.ingest[key].(tabletFiles); ok {
				spec = withRuntimeFiles(spec, files.DataFiles())
			}
			result = append(result, tabletInfo(spec))
		}
	}
	s.mu.RUnlock()
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: table %s", ErrNotHosted, tableID)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].EndRow == nil {
			return false
		}
		if result[j].EndRow == nil {
			return true
		}
		return bytes.Compare(result[i].EndRow, result[j].EndRow) < 0
	})
	return result, nil
}

func (s *Store) Hosted() []tabletloader.Specification {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]tabletloader.Specification, 0, len(s.hosted))
	for _, spec := range s.hosted {
		out = append(out, cloneSpecification(spec))
	}
	sort.Slice(out, func(i, j int) bool {
		return extentKey(out[i].Extent) < extentKey(out[j].Extent)
	})
	return out
}

func propertiesMap(properties []tabletloader.Property) map[string]string {
	out := make(map[string]string, len(properties))
	for _, property := range properties {
		out[property.Name] = property.Value
	}

	return out
}

func validateHostedScanStack(tableID string, properties map[string]string) error {
	resolved, err := itercfg.ResolveProperties(tableID, iterrt.ScopeScan, properties)
	if err != nil {
		return err
	}
	for _, iterator := range resolved.Stack {
		switch iterator.Name {
		case iterrt.IterVersioning:
			if value := iterator.Options["maxVersions"]; value != "" && value != "1" {
				return fmt.Errorf(
					"tserverprocess: table %s versioning maxVersions=%s is not implemented by hosted scans",
					tableID, value)
			}
		case iterrt.IterDeleting, iterrt.IterVisibility:
			if len(iterator.Options) != 0 {
				return fmt.Errorf(
					"tserverprocess: table %s iterator %s options are not implemented by hosted scans",
					tableID, iterator.Name)
			}
		default:
			return fmt.Errorf(
				"tserverprocess: table %s iterator %s is admitted by the registry but not installed in hosted scans",
				tableID, iterator.Name)
		}
	}
	return nil
}

func tabletInfo(spec tabletloader.Specification) metadata.TabletInfo {
	info := metadata.TabletInfo{
		TableID:    spec.Extent.TableID,
		EndRow:     append([]byte(nil), spec.Extent.EndRow...),
		PrevRow:    append([]byte(nil), spec.Extent.PrevEndRow...),
		PrevRowSet: true,
		Directory:  spec.Directory,
		Time:       spec.Time,
	}
	for _, file := range spec.Files {
		info.Files = append(info.Files, metadata.FileEntry{
			Path: file.Path, StartRow: file.StartRow, EndRow: file.EndRow,
			Size: file.Size, NumEntries: file.NumEntries, Time: file.Time,
			RawQualifier: append([]byte(nil), file.RawQualifier...),
		})
	}
	for _, log := range spec.Logs {
		info.Logs = append(info.Logs, metadata.LogEntry{
			UUID: log.UUID, Path: log.Path, WALPath: log.Path, Server: log.Server,
			Peers:        append([]string(nil), log.Peers...),
			RawQualifier: append([]byte(nil), log.RawQualifier...),
		})
	}
	return info
}

func extentKey(extent tserver.Extent) string {
	return extent.TableID + "\x00" + string(extent.PrevEndRow) + "\x00" + string(extent.EndRow)
}

func cloneSpecification(spec tabletloader.Specification) tabletloader.Specification {
	out := spec
	out.Extent = tserver.Extent{
		TableID:    spec.Extent.TableID,
		PrevEndRow: append([]byte(nil), spec.Extent.PrevEndRow...),
		EndRow:     append([]byte(nil), spec.Extent.EndRow...),
	}
	out.Properties = append([]tabletloader.Property(nil), spec.Properties...)
	out.Files = append([]tabletloader.DataFile(nil), spec.Files...)
	for i := range out.Files {
		out.Files[i].RawQualifier = append([]byte(nil), spec.Files[i].RawQualifier...)
	}
	out.Logs = append([]tabletloader.Log(nil), spec.Logs...)
	for i := range out.Logs {
		out.Logs[i].Peers = append([]string(nil), spec.Logs[i].Peers...)
		out.Logs[i].RawQualifier = append([]byte(nil), spec.Logs[i].RawQualifier...)
	}
	return out
}

func withRuntimeFiles(
	spec tabletloader.Specification,
	runtimeFiles []mincauthority.DataFile,
) tabletloader.Specification {
	out := cloneSpecification(spec)
	known := make(map[string]struct{}, len(out.Files))
	for _, file := range out.Files {
		known[file.Path] = struct{}{}
	}
	for _, file := range runtimeFiles {
		if _, ok := known[file.Path]; ok {
			continue
		}
		out.Files = append(out.Files, tabletloader.DataFile{
			Path: file.Path, Size: file.Size, NumEntries: file.Entries,
		})
		known[file.Path] = struct{}{}
	}
	return out
}

var _ tserverrpc.Backend = (*Store)(nil)
var _ tserverrpc.AttemptBackend = (*Store)(nil)
var _ ingestrouter.Directory = (*Store)(nil)
