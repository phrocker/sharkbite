// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package tserverrpc adapts Accumulo's manager-facing Thrift services to the
// fenced tablet lifecycle in internal/tserver.
package tserverrpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletmgmt"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

const (
	managementServiceName   = "tablet"
	tabletServerServiceName = "tserver"
)

var (
	ErrInvalidRequest = errors.New("tserverrpc: invalid request")
	ErrUnauthorized   = errors.New("tserverrpc: unauthorized")
	ErrUnsupported    = errors.New("tserverrpc: operation unsupported")
	ErrNotServing     = errors.New("tserverrpc: tablet is not serving")
)

// UnloadGoal preserves the manager's requested metadata outcome for a backend.
type UnloadGoal int

const (
	UnloadUnassigned UnloadGoal = iota
	UnloadSuspended
	UnloadDeleted
)

// Backend owns the actual tablet resources. Host remains the ownership and
// fencing authority; Backend only performs work for an accepted attempt.
type Backend interface {
	Load(context.Context, tserver.Extent) error
	Unload(context.Context, tserver.Extent, UnloadGoal) error
	Flush(context.Context, tserver.Extent) error
}

// AttemptBackend receives the exact assignment attempt so a writable tablet
// can stamp every WAL and metadata operation with the lifecycle fence.
type AttemptBackend interface {
	LoadAssigned(context.Context, tserver.Extent, tserver.Attempt) error
	UnloadAssigned(context.Context, tserver.Extent, tserver.Attempt, UnloadGoal) error
}

// CredentialsValidator authenticates Accumulo system credentials. Instance ID
// matching is enforced by Adapter before this hook is called.
type CredentialsValidator interface {
	Validate(context.Context, *security.TCredentials, string) error
}

// StatusReporter sends the asynchronous load/unload result the manager expects.
type StatusReporter interface {
	Report(context.Context, manager.TabletLoadState, tserver.Extent) error
}

type Config struct {
	Host            *tserver.Host
	Backend         Backend
	Credentials     CredentialsValidator
	Reporter        StatusReporter
	InstanceID      string
	ManagerLockPath string
	Name            string
	Version         string
	Stop            func()
	Now             func() time.Time
	OnError         func(error)
}

type Adapter struct {
	host        *tserver.Host
	backend     Backend
	credentials CredentialsValidator
	reporter    StatusReporter
	instanceID  string
	managerPath string
	name        string
	version     string
	stop        func()
	now         func() time.Time
	onError     func(error)

	ctx         context.Context
	cancel      context.CancelFunc
	reportWake  chan struct{}
	reportMu    sync.Mutex
	reportQueue []statusReport

	mu             sync.Mutex
	closed         bool
	registered     bool
	operations     map[string]*operation
	flushing       map[string]struct{}
	managerEpoch   tserver.LockID
	managerSession uint64
}

type operation struct {
	attempt tserver.Attempt
	cancel  context.CancelFunc
	kind    operationKind
}

type statusReport struct {
	state  manager.TabletLoadState
	extent tserver.Extent
}

type operationKind int

const (
	operationLoad operationKind = iota
	operationUnload
)

func New(ctx context.Context, cfg Config) (*Adapter, error) {
	switch {
	case ctx == nil:
		return nil, errors.New("tserverrpc: nil context")
	case cfg.Host == nil:
		return nil, errors.New("tserverrpc: nil host")
	case cfg.Backend == nil:
		return nil, fmt.Errorf("%w: tablet backend is unavailable", ErrUnsupported)
	case cfg.Credentials == nil:
		return nil, fmt.Errorf("%w: system credential validation is unavailable", ErrUnsupported)
	case cfg.Reporter == nil:
		return nil, fmt.Errorf("%w: manager status reporting is unavailable", ErrUnsupported)
	case cfg.InstanceID == "":
		return nil, fmt.Errorf("%w: empty instance ID", ErrInvalidRequest)
	case path.Clean(cfg.ManagerLockPath) != "/managers/lock":
		return nil, fmt.Errorf("%w: invalid manager lock path %q", ErrInvalidRequest, cfg.ManagerLockPath)
	case cfg.Name == "":
		return nil, fmt.Errorf("%w: empty server name", ErrInvalidRequest)
	case cfg.Version == "":
		return nil, fmt.Errorf("%w: empty version", ErrInvalidRequest)
	case cfg.Stop == nil:
		return nil, fmt.Errorf("%w: process stop callback is unavailable", ErrUnsupported)
	}

	child, cancel := context.WithCancel(ctx)
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	adapter := &Adapter{
		host:        cfg.Host,
		backend:     cfg.Backend,
		credentials: cfg.Credentials,
		reporter:    cfg.Reporter,
		instanceID:  cfg.InstanceID,
		managerPath: path.Clean(cfg.ManagerLockPath),
		name:        cfg.Name,
		version:     cfg.Version,
		stop:        cfg.Stop,
		now:         now,
		onError:     cfg.OnError,
		ctx:         child,
		cancel:      cancel,
		reportWake:  make(chan struct{}, 1),
		operations:  make(map[string]*operation),
		flushing:    make(map[string]struct{}),
	}
	go adapter.runReporter()
	return adapter, nil
}

// Services is the exact ServiceLock descriptor set backed by this adapter.
// Scan, ingest, and ClientService are intentionally absent.
func (a *Adapter) Services() []tserver.ThriftService {
	return []tserver.ThriftService{
		tserver.ServiceTabletManagement,
		tserver.ServiceTabletServer,
	}
}

func (a *Adapter) LockData(lock *tserver.ServiceLock, address, group string) (tserver.ServiceLockData, error) {
	if lock == nil {
		return tserver.ServiceLockData{}, errors.New("tserverrpc: nil service lock")
	}
	a.mu.Lock()
	registered := a.registered && !a.closed
	a.mu.Unlock()
	if !registered {
		return tserver.ServiceLockData{}, fmt.Errorf(
			"%w: register tablet and tserver processors before advertising them", ErrUnsupported)
	}
	return tserver.TabletServerLockData(lock.UUID(), address, group, a.Services()...)
}

// RegisterProcessors installs only the multiplexed services this adapter
// implements. A process must register these processors before publishing the
// LockData returned above.
func (a *Adapter) RegisterProcessors(mux *thrift.TMultiplexedProcessor) error {
	if mux == nil {
		return errors.New("tserverrpc: nil multiplexed processor")
	}
	mux.RegisterProcessor(managementServiceName,
		tabletmgmt.NewTabletManagementClientServiceProcessor(a))
	mux.RegisterProcessor(tabletServerServiceName,
		tabletserver.NewTabletServerClientServiceProcessor(a))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return context.Canceled
	}
	a.registered = true
	return nil
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.cancel()
	for _, op := range a.operations {
		op.cancel()
	}
	a.operations = make(map[string]*operation)
	a.mu.Unlock()
	a.stop()
	return nil
}

// ReleaseDropped is the release callback for tserver.Participate. It cancels
// work for every tablet Host dropped on lock loss before closing its backend
// resources. No status is reported: once the ServiceLock is gone the manager
// discovers the dead generation through ZooKeeper and performs recovery.
func (a *Adapter) ReleaseDropped(dropped []tserver.Extent) {
	for _, extent := range dropped {
		key := extentKey(extent)
		a.mu.Lock()
		if op, ok := a.operations[key]; ok {
			op.cancel()
			delete(a.operations, key)
		}
		a.mu.Unlock()
		if err := a.backend.Unload(context.WithoutCancel(a.ctx), extent, UnloadUnassigned); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, ErrNotServing) {
			a.emit(fmt.Errorf("release %s after lock loss: %w", extent, err))
		}
	}
}

func (a *Adapter) LoadTablet(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	lock string,
	textent *data.TKeyExtent,
) error {
	extent, fence, err := a.validateManagerRequest(ctx, credentials, lock, textent, "loadTablet")
	if err != nil {
		a.emit(fmt.Errorf("loadTablet rejected: %w", err))
		return err
	}

	a.mu.Lock()
	if err := a.checkOpenLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	attempt, err := a.host.Assign(fence, extent)
	if errors.Is(err, tserver.ErrAlreadyAssigned) {
		state := a.host.State(extent)
		a.mu.Unlock()
		if state == tserver.StateLoading || state == tserver.StateHosted {
			return nil
		}
		return err
	}
	if err != nil {
		a.mu.Unlock()
		a.enqueueReport(manager.TabletLoadState_LOAD_FAILURE, extent)
		return err
	}
	key := extentKey(extent)
	opctx, cancel := context.WithCancel(a.ctx)
	a.operations[key] = &operation{attempt: attempt, cancel: cancel, kind: operationLoad}
	a.mu.Unlock()

	go a.runLoad(opctx, key, extent, attempt)
	return nil
}

func (a *Adapter) runLoad(ctx context.Context, key string, extent tserver.Extent, attempt tserver.Attempt) {
	var loadErr error
	if backend, ok := a.backend.(AttemptBackend); ok {
		loadErr = backend.LoadAssigned(ctx, extent, attempt)
	} else {
		loadErr = a.backend.Load(ctx, extent)
	}

	a.mu.Lock()
	op, current := a.operations[key]
	if !current || op.kind != operationLoad || !op.attempt.Equal(attempt) {
		a.mu.Unlock()
		return
	}
	delete(a.operations, key)
	op.cancel()

	var state manager.TabletLoadState
	var transitionErr error
	switch {
	case loadErr == nil:
		transitionErr = a.host.LoadComplete(attempt)
		state = manager.TabletLoadState_LOADED
	case errors.Is(loadErr, context.Canceled):
		a.mu.Unlock()
		return
	default:
		transitionErr = a.host.LoadFailed(attempt)
		state = manager.TabletLoadState_LOAD_FAILURE
	}

	if transitionErr != nil {
		a.mu.Unlock()
		a.emit(fmt.Errorf("complete load for %s: %w", extent, transitionErr))
		return
	}
	a.enqueueReportLocked(state, extent)
	a.mu.Unlock()
	if loadErr != nil {
		a.emit(fmt.Errorf("load %s: %w", extent, loadErr))
	}
}

func (a *Adapter) UnloadTablet(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	lock string,
	textent *data.TKeyExtent,
	goal tabletmgmt.TUnloadTabletGoal,
	_ int64,
) error {
	extent, fence, err := a.validateManagerRequest(ctx, credentials, lock, textent, "unloadTablet")
	if err != nil {
		return err
	}
	backendGoal, err := unloadGoal(goal)
	if err != nil {
		return err
	}

	key := extentKey(extent)
	a.mu.Lock()
	if err := a.checkOpenLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	if existing, ok := a.operations[key]; ok && existing.kind == operationUnload {
		a.mu.Unlock()
		return nil
	}
	if existing, ok := a.operations[key]; ok {
		existing.cancel()
		delete(a.operations, key)
	}
	attempt, err := a.host.Unassign(fence, extent, tserver.UnloadGraceful)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	if !attempt.Valid() {
		a.mu.Unlock()
		return nil
	}
	opctx, cancel := context.WithCancel(a.ctx)
	a.operations[key] = &operation{attempt: attempt, cancel: cancel, kind: operationUnload}
	a.mu.Unlock()

	go a.runUnload(opctx, key, extent, attempt, backendGoal)
	return nil
}

func (a *Adapter) runUnload(
	ctx context.Context,
	key string,
	extent tserver.Extent,
	attempt tserver.Attempt,
	goal UnloadGoal,
) {
	var unloadErr error
	if backend, ok := a.backend.(AttemptBackend); ok {
		unloadErr = backend.UnloadAssigned(ctx, extent, attempt, goal)
	} else {
		unloadErr = a.backend.Unload(ctx, extent, goal)
	}

	a.mu.Lock()
	op, current := a.operations[key]
	if !current || op.kind != operationUnload || !op.attempt.Equal(attempt) {
		a.mu.Unlock()
		return
	}
	delete(a.operations, key)
	op.cancel()

	state := manager.TabletLoadState_UNLOADED
	var transitionErr error
	switch {
	case unloadErr == nil:
		transitionErr = a.host.UnloadComplete(attempt)
	case errors.Is(unloadErr, ErrNotServing):
		transitionErr = a.host.UnloadComplete(attempt)
		state = manager.TabletLoadState_UNLOAD_FAILURE_NOT_SERVING
	case errors.Is(unloadErr, context.Canceled):
		a.mu.Unlock()
		return
	default:
		state = manager.TabletLoadState_UNLOAD_ERROR
	}

	if transitionErr != nil {
		a.mu.Unlock()
		a.emit(fmt.Errorf("complete unload for %s: %w", extent, transitionErr))
		return
	}
	a.enqueueReportLocked(state, extent)
	a.mu.Unlock()
	if unloadErr != nil {
		a.emit(fmt.Errorf("unload %s: %w", extent, unloadErr))
	}
}

func (a *Adapter) FlushTablet(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	lock string,
	textent *data.TKeyExtent,
) error {
	extent, _, err := a.validateManagerRequest(ctx, credentials, lock, textent, "flushTablet")
	if err != nil {
		return err
	}
	if a.host.State(extent) != tserver.StateHosted {
		return fmt.Errorf("%w: %s", ErrNotServing, extent)
	}
	return a.backend.Flush(ctx, extent)
}

func (a *Adapter) GetTabletServerStatus(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
) (*manager.TabletServerStatus, error) {
	if err := a.validateCredentials(ctx, credentials, "getTabletServerStatus"); err != nil {
		return nil, err
	}
	if _, ok := a.host.Lock(); !ok {
		return nil, tserver.ErrNoLock
	}
	tableMap := make(map[string]*manager.TableInfo)
	for _, tablet := range a.host.Tablets() {
		info := tableMap[tablet.Extent.TableID]
		if info == nil {
			info = &manager.TableInfo{
				Minors: &manager.Compacting{},
				Scans:  &manager.Compacting{},
			}
			tableMap[tablet.Extent.TableID] = info
		}
		info.Tablets++
		if tablet.State == tserver.StateHosted {
			info.OnlineTablets++
		}
	}
	return &manager.TabletServerStatus{
		TableMap:    tableMap,
		LastContact: a.now().UnixMilli(),
		Name:        a.name,
		LogSorts:    []*manager.RecoveryStatus{},
		Version:     a.version,
	}, nil
}

func (a *Adapter) Halt(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	lock string,
) error {
	if _, _, err := a.validateManagerRequest(ctx, credentials, lock, nil, "halt"); err != nil {
		return err
	}
	a.stop()
	return nil
}

func (a *Adapter) FastHalt(
	ctx context.Context,
	tinfo *client.TInfo,
	credentials *security.TCredentials,
	lock string,
) error {
	return a.Halt(ctx, tinfo, credentials, lock)
}

func (a *Adapter) Flush(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	lock, tableID string,
	startRow, endRow []byte,
) error {
	if _, _, err := a.validateManagerRequest(ctx, credentials, lock, nil, "flush"); err != nil {
		return err
	}
	for _, tablet := range a.host.Tablets() {
		if tablet.Extent.TableID != tableID || tablet.State != tserver.StateHosted {
			continue
		}
		if !extentIntersects(tablet.Extent, startRow, endRow) {
			continue
		}
		key := extentKey(tablet.Extent)
		a.mu.Lock()
		if _, ok := a.flushing[key]; ok {
			a.mu.Unlock()
			continue
		}
		a.flushing[key] = struct{}{}
		a.mu.Unlock()
		go func(extent tserver.Extent, key string) {
			defer func() {
				a.mu.Lock()
				delete(a.flushing, key)
				a.mu.Unlock()
			}()
			if err := a.backend.Flush(a.ctx, extent); err != nil && a.onError != nil {
				a.onError(fmt.Errorf("flush %s: %w", extent, err))
			}
		}(tablet.Extent, key)
	}
	return nil
}

func (a *Adapter) GetTabletStats(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	tableID string,
) ([]*tabletserver.TabletStats, error) {
	if err := a.validateCredentials(ctx, credentials, "getTabletStats"); err != nil {
		return nil, err
	}
	var result []*tabletserver.TabletStats
	for _, tablet := range a.host.Tablets() {
		if tablet.Extent.TableID == tableID && tablet.State == tserver.StateHosted {
			result = append(result, &tabletserver.TabletStats{Extent: toThriftExtent(tablet.Extent)})
		}
	}
	return result, nil
}

func (a *Adapter) GetHistoricalStats(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
) (*tabletserver.TabletStats, error) {
	if err := a.validateCredentials(ctx, credentials, "getHistoricalStats"); err != nil {
		return nil, err
	}
	return &tabletserver.TabletStats{}, nil
}

func (a *Adapter) GetActiveCompactions(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
) ([]*tabletserver.ActiveCompaction, error) {
	if err := a.validateCredentials(ctx, credentials, "getActiveCompactions"); err != nil {
		return nil, err
	}
	return []*tabletserver.ActiveCompaction{}, nil
}

func (a *Adapter) RemoveLogs(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	_ []string,
) error {
	if err := a.validateCredentials(ctx, credentials, "removeLogs"); err != nil {
		return err
	}
	return fmt.Errorf("%w: removeLogs requires a WAL implementation", ErrUnsupported)
}

func (a *Adapter) GetActiveLogs(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
) ([]string, error) {
	if err := a.validateCredentials(ctx, credentials, "getActiveLogs"); err != nil {
		return nil, err
	}
	return []string{}, nil
}

func (a *Adapter) StartGetSummaries(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	_ *data.TSummaryRequest,
) (*data.TSummaries, error) {
	if err := a.validateCredentials(ctx, credentials, "startGetSummaries"); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: summaries", ErrUnsupported)
}

func (a *Adapter) StartGetSummariesForPartition(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	_ *data.TSummaryRequest,
	_, _ int32,
) (*data.TSummaries, error) {
	if err := a.validateCredentials(ctx, credentials, "startGetSummariesForPartition"); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: summaries", ErrUnsupported)
}

func (a *Adapter) StartGetSummariesFromFiles(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	_ *data.TSummaryRequest,
	_ map[string][]*data.TRowRange,
) (*data.TSummaries, error) {
	if err := a.validateCredentials(ctx, credentials, "startGetSummariesFromFiles"); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: summaries", ErrUnsupported)
}

func (a *Adapter) ContiuneGetSummaries(
	context.Context, *client.TInfo, int64,
) (*data.TSummaries, error) {
	return nil, fmt.Errorf("%w: summaries", ErrUnsupported)
}

func (a *Adapter) RefreshTablets(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	tablets []*data.TKeyExtent,
) ([]*data.TKeyExtent, error) {
	if err := a.validateCredentials(ctx, credentials, "refreshTablets"); err != nil {
		return nil, err
	}
	return append([]*data.TKeyExtent(nil), tablets...), nil
}

func (a *Adapter) AllocateTimestamps(
	ctx context.Context,
	_ *client.TInfo,
	credentials *security.TCredentials,
	_ []*data.TKeyExtent,
) (map[*data.TKeyExtent]int64, error) {
	if err := a.validateCredentials(ctx, credentials, "allocateTimestamps"); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: timestamp allocation requires the ingest data plane", ErrUnsupported)
}

func (a *Adapter) validateManagerRequest(
	ctx context.Context,
	credentials *security.TCredentials,
	serializedLock string,
	textent *data.TKeyExtent,
	operation string,
) (tserver.Extent, tserver.Fence, error) {
	if err := a.validateCredentials(ctx, credentials, operation); err != nil {
		return tserver.Extent{}, tserver.Fence{}, err
	}
	managerLock, session, err := parseSerializedLock(serializedLock, a.managerPath)
	if err != nil {
		return tserver.Extent{}, tserver.Fence{}, err
	}
	serverLock, ok := a.host.Lock()
	if !ok {
		return tserver.Extent{}, tserver.Fence{}, tserver.ErrNoLock
	}
	observed, ok := a.host.ManagerLock()
	if !ok || !managerLock.Equal(observed) {
		return tserver.Extent{}, tserver.Fence{}, fmt.Errorf(
			"%w: request carries %s, observed %s", tserver.ErrStaleManagerLock, managerLock, observed)
	}

	a.mu.Lock()
	switch {
	case !a.managerEpoch.Valid() || managerLock.Supersedes(a.managerEpoch):
		a.managerEpoch = managerLock
		a.managerSession = session
	case !managerLock.Equal(a.managerEpoch):
		a.mu.Unlock()
		return tserver.Extent{}, tserver.Fence{}, fmt.Errorf(
			"%w: request carries %s, adapter already served %s",
			tserver.ErrStaleManagerLock, managerLock, a.managerEpoch)
	case session != a.managerSession:
		a.mu.Unlock()
		return tserver.Extent{}, tserver.Fence{}, fmt.Errorf(
			"%w: manager session changed for %s", tserver.ErrStaleManagerLock, managerLock)
	}
	a.mu.Unlock()

	var extent tserver.Extent
	if textent != nil {
		extent, err = fromThriftExtent(textent)
		if err != nil {
			return tserver.Extent{}, tserver.Fence{}, err
		}
	}
	return extent, tserver.Fence{Server: serverLock, Manager: managerLock}, nil
}

func (a *Adapter) validateCredentials(
	ctx context.Context,
	credentials *security.TCredentials,
	operation string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return context.Canceled
	}
	if credentials == nil {
		return fmt.Errorf("%w: missing credentials", ErrUnauthorized)
	}
	if credentials.InstanceId != a.instanceID {
		return fmt.Errorf("%w: credentials name instance %q, want %q",
			ErrUnauthorized, credentials.InstanceId, a.instanceID)
	}
	if err := a.credentials.Validate(ctx, credentials, operation); err != nil {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	return nil
}

func (a *Adapter) checkOpenLocked() error {
	if a.closed {
		return context.Canceled
	}
	return nil
}

func (a *Adapter) enqueueReport(state manager.TabletLoadState, extent tserver.Extent) {
	a.reportMu.Lock()
	a.reportQueue = append(a.reportQueue, statusReport{state: state, extent: extent})
	a.reportMu.Unlock()
	select {
	case a.reportWake <- struct{}{}:
	default:
	}
}

func (a *Adapter) enqueueReportLocked(state manager.TabletLoadState, extent tserver.Extent) {
	a.enqueueReport(state, extent)
}

func (a *Adapter) runReporter() {
	for {
		a.reportMu.Lock()
		if len(a.reportQueue) > 0 {
			report := a.reportQueue[0]
			a.reportQueue[0] = statusReport{}
			a.reportQueue = a.reportQueue[1:]
			a.reportMu.Unlock()
			if err := a.reporter.Report(a.ctx, report.state, report.extent); err != nil &&
				!errors.Is(err, context.Canceled) {
				a.emit(fmt.Errorf("report %s for %s: %w", report.state, report.extent, err))
			}
			continue
		}
		a.reportMu.Unlock()
		select {
		case <-a.ctx.Done():
			return
		case <-a.reportWake:
		}
	}
}

func (a *Adapter) emit(err error) {
	if err != nil && a.onError != nil {
		a.onError(err)
	}
}

func parseSerializedLock(serialized, expectedPath string) (tserver.LockID, uint64, error) {
	parts := strings.Split(serialized, "$")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return tserver.LockID{}, 0, fmt.Errorf("%w: malformed manager lock", ErrInvalidRequest)
	}
	slash := strings.LastIndex(parts[0], "/")
	if slash <= 0 || slash == len(parts[0])-1 {
		return tserver.LockID{}, 0, fmt.Errorf("%w: malformed manager lock path", ErrInvalidRequest)
	}
	if got := path.Clean(parts[0][:slash]); got != path.Clean(expectedPath) {
		return tserver.LockID{}, 0, fmt.Errorf(
			"%w: manager lock path %q, want %q", ErrInvalidRequest, got, expectedPath)
	}
	lock, ok := tserver.ParseLockNode(parts[0][slash+1:])
	if !ok {
		return tserver.LockID{}, 0, fmt.Errorf("%w: malformed manager lock node", ErrInvalidRequest)
	}
	session, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil || session == 0 {
		return tserver.LockID{}, 0, fmt.Errorf("%w: malformed manager session", ErrInvalidRequest)
	}
	return lock, session, nil
}

func fromThriftExtent(extent *data.TKeyExtent) (tserver.Extent, error) {
	if extent == nil {
		return tserver.Extent{}, fmt.Errorf("%w: nil extent", ErrInvalidRequest)
	}
	converted := tserver.Extent{
		TableID:    string(extent.Table),
		EndRow:     append([]byte(nil), extent.EndRow...),
		PrevEndRow: append([]byte(nil), extent.PrevEndRow...),
	}
	if err := converted.Validate(); err != nil {
		return tserver.Extent{}, err
	}
	return converted, nil
}

func toThriftExtent(extent tserver.Extent) *data.TKeyExtent {
	return &data.TKeyExtent{
		Table:      []byte(extent.TableID),
		EndRow:     append([]byte(nil), extent.EndRow...),
		PrevEndRow: append([]byte(nil), extent.PrevEndRow...),
	}
}

func unloadGoal(goal tabletmgmt.TUnloadTabletGoal) (UnloadGoal, error) {
	switch goal {
	case tabletmgmt.TUnloadTabletGoal_UNASSIGNED:
		return UnloadUnassigned, nil
	case tabletmgmt.TUnloadTabletGoal_SUSPENDED:
		return UnloadSuspended, nil
	case tabletmgmt.TUnloadTabletGoal_DELETED:
		return UnloadDeleted, nil
	default:
		return 0, fmt.Errorf("%w: unload goal %s", ErrInvalidRequest, goal)
	}
}

func extentKey(extent tserver.Extent) string {
	return extent.TableID + ":" +
		base64.RawStdEncoding.EncodeToString(extent.PrevEndRow) + ":" +
		base64.RawStdEncoding.EncodeToString(extent.EndRow)
}

func extentIntersects(extent tserver.Extent, start, end []byte) bool {
	if len(start) > 0 && len(extent.EndRow) > 0 && bytes.Compare(extent.EndRow, start) <= 0 {
		return false
	}
	if len(end) > 0 && len(extent.PrevEndRow) > 0 && bytes.Compare(extent.PrevEndRow, end) >= 0 {
		return false
	}
	return true
}

var (
	_ tabletmgmt.TabletManagementClientService = (*Adapter)(nil)
	_ tabletserver.TabletServerClientService   = (*Adapter)(nil)
)
