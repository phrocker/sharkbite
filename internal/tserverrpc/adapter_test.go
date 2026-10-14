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

package tserverrpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	gozk "github.com/go-zookeeper/zk"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletmgmt"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

const (
	testInstance    = "instance-1"
	testManagerPath = "/managers/lock"
	testServerUUID  = "11111111-1111-4111-8111-111111111111"
	testManagerUUID = "22222222-2222-4222-8222-222222222222"
)

type allowCredentials struct{}

func (allowCredentials) Validate(context.Context, *security.TCredentials, string) error {
	return nil
}

type unusedLockConn struct{}

func (unusedLockConn) Create(string, []byte, int32, []gozk.ACL) (string, error) {
	return "", errors.New("unused")
}
func (unusedLockConn) Children(string) ([]string, *gozk.Stat, error) {
	return nil, nil, errors.New("unused")
}
func (unusedLockConn) GetW(string) ([]byte, *gozk.Stat, <-chan gozk.Event, error) {
	return nil, nil, nil, errors.New("unused")
}
func (unusedLockConn) Delete(string, int32) error { return errors.New("unused") }

type fakeBackend struct {
	loadStarted   chan struct{}
	loadRelease   chan error
	loadCanceled  chan struct{}
	unloadStarted chan struct{}
	unloadRelease chan error

	loads   atomic.Int32
	unloads atomic.Int32
	flushes atomic.Int32
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		loadStarted:   make(chan struct{}, 10),
		loadRelease:   make(chan error, 10),
		loadCanceled:  make(chan struct{}, 10),
		unloadStarted: make(chan struct{}, 10),
		unloadRelease: make(chan error, 10),
	}
}

func (b *fakeBackend) Load(ctx context.Context, _ tserver.Extent) error {
	b.loads.Add(1)
	b.loadStarted <- struct{}{}
	select {
	case err := <-b.loadRelease:
		return err
	case <-ctx.Done():
		b.loadCanceled <- struct{}{}
		return ctx.Err()
	}
}

func (b *fakeBackend) Unload(ctx context.Context, _ tserver.Extent, _ UnloadGoal) error {
	b.unloads.Add(1)
	b.unloadStarted <- struct{}{}
	select {
	case err := <-b.unloadRelease:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *fakeBackend) Flush(context.Context, tserver.Extent) error {
	b.flushes.Add(1)
	return nil
}

type report struct {
	state  manager.TabletLoadState
	extent tserver.Extent
}

type fakeReporter struct {
	reports chan report
}

func (r *fakeReporter) Report(
	_ context.Context,
	state manager.TabletLoadState,
	extent tserver.Extent,
) error {
	r.reports <- report{state: state, extent: extent}
	return nil
}

type blockingReporter struct {
	calls        chan manager.TabletLoadState
	releaseFirst chan struct{}
	once         sync.Once
}

func (r *blockingReporter) Report(
	_ context.Context,
	state manager.TabletLoadState,
	_ tserver.Extent,
) error {
	r.calls <- state
	r.once.Do(func() { <-r.releaseFirst })
	return nil
}

type fixture struct {
	adapter     *Adapter
	host        *tserver.Host
	backend     *fakeBackend
	reporter    *fakeReporter
	serverLock  tserver.LockID
	managerLock tserver.LockID
	credentials *security.TCredentials
	serialized  string
	extent      tserver.Extent
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	host := tserver.NewHost()
	serverLock := tserver.LockID{UUID: testServerUUID, Sequence: 7}
	managerLock := tserver.LockID{UUID: testManagerUUID, Sequence: 11}
	if err := host.AdoptLock(serverLock); err != nil {
		t.Fatalf("AdoptLock: %v", err)
	}
	if err := host.ObserveManagerLock(managerLock); err != nil {
		t.Fatalf("ObserveManagerLock: %v", err)
	}
	backend := newFakeBackend()
	reporter := &fakeReporter{reports: make(chan report, 20)}
	adapter, err := New(context.Background(), Config{
		Host:            host,
		Backend:         backend,
		Credentials:     allowCredentials{},
		Reporter:        reporter,
		InstanceID:      testInstance,
		ManagerLockPath: testManagerPath,
		Name:            "shoal:9997",
		Version:         "4.0.0-SNAPSHOT",
		Stop:            func() {},
		Now:             func() time.Time { return time.UnixMilli(1234) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	return fixture{
		adapter:     adapter,
		host:        host,
		backend:     backend,
		reporter:    reporter,
		serverLock:  serverLock,
		managerLock: managerLock,
		credentials: &security.TCredentials{InstanceId: testInstance, Principal: "!SYSTEM"},
		serialized:  serializedManagerLock(managerLock, 0xabc),
		extent:      tserver.Extent{TableID: "2", EndRow: []byte("m")},
	}
}

func serializedManagerLock(lock tserver.LockID, session uint64) string {
	return fmt.Sprintf("%s/%s$%x", testManagerPath, lock.String(), session)
}

func waitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitReport(t *testing.T, reports <-chan report, state manager.TabletLoadState) report {
	t.Helper()
	select {
	case got := <-reports:
		if got.state != state {
			t.Fatalf("report state = %s, want %s", got.state, state)
		}
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", state)
		return report{}
	}
}

func TestAssignmentStatusAndDuplicateAreIdempotent(t *testing.T) {
	f := newFixture(t)
	extent := toThriftExtent(f.extent)

	if err := f.adapter.LoadTablet(context.Background(), nil, f.credentials, f.serialized, extent); err != nil {
		t.Fatalf("LoadTablet: %v", err)
	}
	waitSignal(t, f.backend.loadStarted, "load start")

	if err := f.adapter.LoadTablet(context.Background(), nil, f.credentials, f.serialized, extent); err != nil {
		t.Fatalf("duplicate LoadTablet: %v", err)
	}
	if got := f.backend.loads.Load(); got != 1 {
		t.Fatalf("backend loads = %d, want 1", got)
	}

	f.backend.loadRelease <- nil
	waitReport(t, f.reporter.reports, manager.TabletLoadState_LOADED)
	if state := f.host.State(f.extent); state != tserver.StateHosted {
		t.Fatalf("host state = %s, want HOSTED", state)
	}

	status, err := f.adapter.GetTabletServerStatus(context.Background(), nil, f.credentials)
	if err != nil {
		t.Fatalf("GetTabletServerStatus: %v", err)
	}
	if status.LastContact != 1234 || status.Name != "shoal:9997" ||
		status.TableMap["2"].Tablets != 1 || status.TableMap["2"].OnlineTablets != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestDuplicateUnloadStartsOneDrain(t *testing.T) {
	f := newFixture(t)
	hostTablet(t, f)

	extent := toThriftExtent(f.extent)
	if err := f.adapter.UnloadTablet(
		context.Background(), nil, f.credentials, f.serialized, extent,
		tabletmgmt.TUnloadTabletGoal_UNASSIGNED, 1,
	); err != nil {
		t.Fatalf("UnloadTablet: %v", err)
	}
	waitSignal(t, f.backend.unloadStarted, "unload start")
	if err := f.adapter.UnloadTablet(
		context.Background(), nil, f.credentials, f.serialized, extent,
		tabletmgmt.TUnloadTabletGoal_UNASSIGNED, 1,
	); err != nil {
		t.Fatalf("duplicate UnloadTablet: %v", err)
	}
	if got := f.backend.unloads.Load(); got != 1 {
		t.Fatalf("backend unloads = %d, want 1", got)
	}
	f.backend.unloadRelease <- nil
	waitReport(t, f.reporter.reports, manager.TabletLoadState_UNLOADED)
	if state := f.host.State(f.extent); state != tserver.StateUnassigned {
		t.Fatalf("host state = %s, want UNASSIGNED", state)
	}
}

func TestBackendFailuresAreReportedAccurately(t *testing.T) {
	t.Run("load failure", func(t *testing.T) {
		f := newFixture(t)
		if err := f.adapter.LoadTablet(
			context.Background(), nil, f.credentials, f.serialized, toThriftExtent(f.extent),
		); err != nil {
			t.Fatalf("LoadTablet: %v", err)
		}
		waitSignal(t, f.backend.loadStarted, "load start")
		f.backend.loadRelease <- errors.New("metadata unavailable")
		waitReport(t, f.reporter.reports, manager.TabletLoadState_LOAD_FAILURE)
		if state := f.host.State(f.extent); state != tserver.StateUnassigned {
			t.Fatalf("host state = %s, want UNASSIGNED", state)
		}
	})

	t.Run("unload error", func(t *testing.T) {
		f := newFixture(t)
		hostTablet(t, f)
		if err := f.adapter.UnloadTablet(
			context.Background(), nil, f.credentials, f.serialized, toThriftExtent(f.extent),
			tabletmgmt.TUnloadTabletGoal_UNASSIGNED, 1,
		); err != nil {
			t.Fatalf("UnloadTablet: %v", err)
		}
		waitSignal(t, f.backend.unloadStarted, "unload start")
		f.backend.unloadRelease <- errors.New("close failed")
		waitReport(t, f.reporter.reports, manager.TabletLoadState_UNLOAD_ERROR)
		if state := f.host.State(f.extent); state != tserver.StateUnloading {
			t.Fatalf("host state = %s, want UNLOADING for retry", state)
		}
	})

	t.Run("not serving", func(t *testing.T) {
		f := newFixture(t)
		hostTablet(t, f)
		if err := f.adapter.UnloadTablet(
			context.Background(), nil, f.credentials, f.serialized, toThriftExtent(f.extent),
			tabletmgmt.TUnloadTabletGoal_DELETED, 1,
		); err != nil {
			t.Fatalf("UnloadTablet: %v", err)
		}
		waitSignal(t, f.backend.unloadStarted, "unload start")
		f.backend.unloadRelease <- ErrNotServing
		waitReport(t, f.reporter.reports, manager.TabletLoadState_UNLOAD_FAILURE_NOT_SERVING)
		if state := f.host.State(f.extent); state != tserver.StateUnassigned {
			t.Fatalf("host state = %s, want UNASSIGNED", state)
		}
	})
}

func TestUnloadCancelsInFlightLoad(t *testing.T) {
	f := newFixture(t)
	extent := toThriftExtent(f.extent)
	if err := f.adapter.LoadTablet(context.Background(), nil, f.credentials, f.serialized, extent); err != nil {
		t.Fatalf("LoadTablet: %v", err)
	}
	waitSignal(t, f.backend.loadStarted, "load start")

	if err := f.adapter.UnloadTablet(
		context.Background(), nil, f.credentials, f.serialized, extent,
		tabletmgmt.TUnloadTabletGoal_SUSPENDED, 1,
	); err != nil {
		t.Fatalf("UnloadTablet: %v", err)
	}
	waitSignal(t, f.backend.loadCanceled, "load cancellation")
	waitSignal(t, f.backend.unloadStarted, "unload start")
	f.backend.unloadRelease <- nil
	waitReport(t, f.reporter.reports, manager.TabletLoadState_UNLOADED)
}

func TestLockLossCancelsWorkAndClosesDroppedTablet(t *testing.T) {
	f := newFixture(t)
	if err := f.adapter.LoadTablet(
		context.Background(), nil, f.credentials, f.serialized, toThriftExtent(f.extent),
	); err != nil {
		t.Fatalf("LoadTablet: %v", err)
	}
	waitSignal(t, f.backend.loadStarted, "load start")
	dropped := f.host.LoseLock(f.serverLock)
	if len(dropped) != 1 {
		t.Fatalf("LoseLock dropped %d tablets, want 1", len(dropped))
	}

	done := make(chan struct{})
	go func() {
		f.adapter.ReleaseDropped(dropped)
		close(done)
	}()
	waitSignal(t, f.backend.loadCanceled, "load cancellation")
	waitSignal(t, f.backend.unloadStarted, "backend release")
	f.backend.unloadRelease <- ErrNotServing
	waitSignal(t, done, "release callback")
}

func TestStaleManagerEpochAndSessionFailClosed(t *testing.T) {
	f := newFixture(t)
	newer := tserver.LockID{UUID: "33333333-3333-4333-8333-333333333333", Sequence: 12}
	if err := f.host.ObserveManagerLock(newer); err != nil {
		t.Fatalf("ObserveManagerLock: %v", err)
	}
	if err := f.adapter.LoadTablet(
		context.Background(), nil, f.credentials, f.serialized, toThriftExtent(f.extent),
	); !errors.Is(err, tserver.ErrStaleManagerLock) {
		t.Fatalf("stale epoch error = %v, want ErrStaleManagerLock", err)
	}
	if f.host.State(f.extent) != tserver.StateUnassigned {
		t.Fatal("stale epoch changed hosting state")
	}

	fresh := newFixture(t)
	if err := fresh.adapter.LoadTablet(
		context.Background(), nil, fresh.credentials, fresh.serialized, toThriftExtent(fresh.extent),
	); err != nil {
		t.Fatalf("first request: %v", err)
	}
	waitSignal(t, fresh.backend.loadStarted, "load start")
	otherSession := serializedManagerLock(fresh.managerLock, 0xdef)
	if err := fresh.adapter.LoadTablet(
		context.Background(), nil, fresh.credentials, otherSession, toThriftExtent(fresh.extent),
	); !errors.Is(err, tserver.ErrStaleManagerLock) {
		t.Fatalf("changed session error = %v, want ErrStaleManagerLock", err)
	}
}

func TestConcurrentDuplicateAssignmentAndUnload(t *testing.T) {
	f := newFixture(t)
	extent := toThriftExtent(f.extent)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.adapter.LoadTablet(context.Background(), nil, f.credentials, f.serialized, extent)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent assignment: %v", err)
		}
	}
	waitSignal(t, f.backend.loadStarted, "load start")
	if got := f.backend.loads.Load(); got != 1 {
		t.Fatalf("backend loads = %d, want 1", got)
	}
	f.backend.loadRelease <- nil
	waitReport(t, f.reporter.reports, manager.TabletLoadState_LOADED)

	errs = make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.adapter.UnloadTablet(
				context.Background(), nil, f.credentials, f.serialized, extent,
				tabletmgmt.TUnloadTabletGoal_UNASSIGNED, 1,
			)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent unload: %v", err)
		}
	}
	waitSignal(t, f.backend.unloadStarted, "unload start")
	if got := f.backend.unloads.Load(); got != 1 {
		t.Fatalf("backend unloads = %d, want 1", got)
	}
	f.backend.unloadRelease <- nil
	waitReport(t, f.reporter.reports, manager.TabletLoadState_UNLOADED)
}

func TestValidationAndAdvertisementBoundary(t *testing.T) {
	f := newFixture(t)
	badCredentials := &security.TCredentials{InstanceId: "other"}
	if err := f.adapter.LoadTablet(
		context.Background(), nil, badCredentials, f.serialized, toThriftExtent(f.extent),
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong instance error = %v, want ErrUnauthorized", err)
	}
	if err := f.adapter.LoadTablet(
		context.Background(), nil, f.credentials,
		"/accumulo/other/managers/lock/"+f.managerLock.String()+"$abc",
		toThriftExtent(f.extent),
	); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("wrong lock path error = %v, want ErrInvalidRequest", err)
	}
	services := f.adapter.Services()
	if len(services) != 2 ||
		services[0] != tserver.ServiceTabletManagement ||
		services[1] != tserver.ServiceTabletServer {
		t.Fatalf("advertised services = %v", services)
	}
	lock, err := tserver.NewServiceLock(unusedLockConn{}, tserver.ServiceLockOptions{
		Path: "/accumulo/instance-1/tservers/default/shoal:9997",
	})
	if err != nil {
		t.Fatalf("NewServiceLock: %v", err)
	}
	if _, err := f.adapter.LockData(lock, "shoal:9997", "default"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("LockData before processors = %v, want ErrUnsupported", err)
	}
	mux := thrift.NewTMultiplexedProcessor()
	if err := f.adapter.RegisterProcessors(mux); err != nil {
		t.Fatalf("RegisterProcessors: %v", err)
	}
	lockData, err := f.adapter.LockData(lock, "shoal:9997", "default")
	if err != nil {
		t.Fatalf("LockData: %v", err)
	}
	if len(lockData.Descriptors) != 2 {
		t.Fatalf("LockData descriptors = %d, want 2", len(lockData.Descriptors))
	}
}

func TestNewRefusesMissingProcessDependencies(t *testing.T) {
	base := Config{
		Host:            tserver.NewHost(),
		Backend:         newFakeBackend(),
		Credentials:     allowCredentials{},
		Reporter:        &fakeReporter{reports: make(chan report, 1)},
		InstanceID:      testInstance,
		ManagerLockPath: testManagerPath,
		Name:            "shoal:9997",
		Version:         "4.0.0-SNAPSHOT",
		Stop:            func() {},
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   error
	}{
		{"backend", func(c *Config) { c.Backend = nil }, ErrUnsupported},
		{"credentials", func(c *Config) { c.Credentials = nil }, ErrUnsupported},
		{"reporter", func(c *Config) { c.Reporter = nil }, ErrUnsupported},
		{"stop", func(c *Config) { c.Stop = nil }, ErrUnsupported},
		{"manager path for another instance", func(c *Config) {
			c.ManagerLockPath = "/accumulo/other/managers/lock"
		}, ErrInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			if _, err := New(context.Background(), cfg); !errors.Is(err, test.want) {
				t.Fatalf("New error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestHaltRequiresCurrentManagerFence(t *testing.T) {
	f := newFixture(t)
	var stops atomic.Int32
	f.adapter.stop = func() { stops.Add(1) }
	if err := f.adapter.Halt(context.Background(), nil, f.credentials, f.serialized); err != nil {
		t.Fatalf("Halt: %v", err)
	}
	if stops.Load() != 1 {
		t.Fatalf("stop calls = %d, want 1", stops.Load())
	}
	stale := tserver.LockID{UUID: "44444444-4444-4444-8444-444444444444", Sequence: 10}
	if err := f.adapter.FastHalt(
		context.Background(), nil, f.credentials, serializedManagerLock(stale, 0xabc),
	); !errors.Is(err, tserver.ErrStaleManagerLock) {
		t.Fatalf("FastHalt stale error = %v, want ErrStaleManagerLock", err)
	}
	if stops.Load() != 1 {
		t.Fatalf("stale control changed stop calls to %d", stops.Load())
	}
}

func TestLifecycleReportsRemainOrderedDuringReconnect(t *testing.T) {
	host := tserver.NewHost()
	reporter := &blockingReporter{
		calls:        make(chan manager.TabletLoadState, 2),
		releaseFirst: make(chan struct{}),
	}
	adapter, err := New(context.Background(), Config{
		Host:            host,
		Backend:         newFakeBackend(),
		Credentials:     allowCredentials{},
		Reporter:        reporter,
		InstanceID:      testInstance,
		ManagerLockPath: testManagerPath,
		Name:            "shoal:9997",
		Version:         "4.0.0-SNAPSHOT",
		Stop:            func() {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer adapter.Close()

	adapter.enqueueReport(manager.TabletLoadState_LOADED, tserver.Extent{TableID: "2"})
	if got := <-reporter.calls; got != manager.TabletLoadState_LOADED {
		t.Fatalf("first report = %s, want LOADED", got)
	}
	adapter.enqueueReport(manager.TabletLoadState_UNLOADED, tserver.Extent{TableID: "2"})
	select {
	case got := <-reporter.calls:
		t.Fatalf("second report %s overtook the blocked first report", got)
	case <-time.After(20 * time.Millisecond):
	}
	close(reporter.releaseFirst)
	select {
	case got := <-reporter.calls:
		if got != manager.TabletLoadState_UNLOADED {
			t.Fatalf("second report = %s, want UNLOADED", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordered reporter did not continue")
	}
}

func hostTablet(t *testing.T, f fixture) {
	t.Helper()
	if err := f.adapter.LoadTablet(
		context.Background(), nil, f.credentials, f.serialized, toThriftExtent(f.extent),
	); err != nil {
		t.Fatalf("LoadTablet: %v", err)
	}
	waitSignal(t, f.backend.loadStarted, "load start")
	f.backend.loadRelease <- nil
	waitReport(t, f.reporter.reports, manager.TabletLoadState_LOADED)
}
