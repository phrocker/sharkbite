package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestConnectorRegistryLifecycle(t *testing.T) {
	registry := newConnectorRegistry()
	first := &ownedConnector{}
	second := &ownedConnector{}

	firstID, ok := registry.add(first)
	if !ok || firstID == 0 {
		t.Fatalf("add(first) = %d, %v", firstID, ok)
	}
	secondID, ok := registry.add(second)
	if !ok || secondID == 0 || secondID == firstID {
		t.Fatalf("add(second) = %d, %v; first = %d", secondID, ok, firstID)
	}
	if got, ok := registry.get(firstID); !ok || got != first {
		t.Fatalf("get(first) = %p, %v", got, ok)
	}
	if got, ok := registry.remove(firstID); !ok || got != first {
		t.Fatalf("remove(first) = %p, %v", got, ok)
	}
	if _, ok := registry.get(firstID); ok {
		t.Fatal("removed connector remains registered")
	}
}

type fakeConnectorAPI struct {
	close     func() error
	identity  func(context.Context) (accumulo.InstanceInfo, string, error)
	principal string
}

func (c *fakeConnectorAPI) Close() error {
	if c.close == nil {
		return nil
	}
	return c.close()
}

func (c *fakeConnectorAPI) Principal() string {
	return c.principal
}

func (c *fakeConnectorAPI) capiConnectorIdentity(ctx context.Context) (accumulo.InstanceInfo, string, error) {
	if c.identity != nil {
		return c.identity(ctx)
	}
	return accumulo.InstanceInfo{Name: "test", ID: "test-id"}, c.principal, nil
}

func (c *fakeConnectorAPI) NewScanner(
	accumulo.Table,
	accumulo.ScannerOptions,
) (*accumulo.Scanner, error) {
	return nil, nil
}

func (c *fakeConnectorAPI) NewBatchScanner(
	accumulo.Table,
	accumulo.ScannerOptions,
) (*accumulo.BatchScanner, error) {
	return nil, nil
}

func (c *fakeConnectorAPI) NewBatchWriter(
	accumulo.Table,
	accumulo.BatchWriterOptions,
) (*accumulo.BatchWriter, error) {
	return nil, nil
}

func (c *fakeConnectorAPI) Tables(context.Context) ([]accumulo.Table, error) {
	return nil, nil
}

func (c *fakeConnectorAPI) TableExists(context.Context, string) (bool, error) {
	return false, nil
}

func (c *fakeConnectorAPI) CreateTable(context.Context, string) error {
	return nil
}

func (c *fakeConnectorAPI) DeleteTable(context.Context, string) error {
	return nil
}

func (c *fakeConnectorAPI) RenameTable(context.Context, string, string) error {
	return nil
}

func (c *fakeConnectorAPI) FlushTable(context.Context, string, bool) error {
	return nil
}

func (c *fakeConnectorAPI) SetTableProperty(
	context.Context,
	string,
	string,
	string,
) error {
	return nil
}

func (c *fakeConnectorAPI) RemoveTableProperty(
	context.Context,
	string,
	string,
) error {
	return nil
}

func (c *fakeConnectorAPI) EffectiveTableProperties(
	context.Context,
	string,
) (map[string]string, error) {
	return nil, nil
}

type fakeConnectorInstance struct {
	accumulo.NoTopology
	configuration *accumulo.Configuration
	close         func() error
}

func (i *fakeConnectorInstance) Info() accumulo.InstanceInfo {
	return accumulo.InstanceInfo{Name: "test", ID: "test-id"}
}

func (i *fakeConnectorInstance) Configuration() *accumulo.Configuration {
	if i.configuration == nil {
		i.configuration = accumulo.NewConfiguration()
	}
	return i.configuration
}

func (i *fakeConnectorInstance) Close() error {
	if i.close == nil {
		return nil
	}
	return i.close()
}

func TestOwnedConnectorCloseCancelsAndJoinsActiveCalls(t *testing.T) {
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	connector := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			return nil
		}},
		&fakeConnectorInstance{close: func() error {
			instanceCloses.Add(1)
			return nil
		}},
	)
	ctx, done, err := connector.begin(0)
	if err != nil {
		t.Fatal(err)
	}

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- connector.close()
	}()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("close did not cancel active connector call")
	}
	select {
	case <-closeReturned:
		t.Fatal("close returned before active connector call completed")
	default:
	}

	done()
	select {
	case err := <-closeReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not join completed connector call")
	}
	if _, _, err := connector.begin(0); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("begin after close error = %v, want connector closed", err)
	}
	if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		t.Fatalf("close counts = connector:%d instance:%d, want 1/1",
			connectorCloses.Load(), instanceCloses.Load())
	}
}

func TestOwnedConnectorCloseBoundedWait(t *testing.T) {
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	connector := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			return nil
		}},
		&fakeConnectorInstance{close: func() error {
			instanceCloses.Add(1)
			return nil
		}},
	)
	ctx, done, err := connector.begin(0)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := connector.closeBounded(20 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeBounded error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("closeBounded exceeded deadline bound: %v", elapsed)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("active context error = %v, want canceled", ctx.Err())
	}
	done()

	deadline := time.After(time.Second)
	for connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		select {
		case <-deadline:
			t.Fatalf("background close counts = connector:%d instance:%d, want 1/1",
				connectorCloses.Load(), instanceCloses.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := connector.closeBounded(time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated closeBounded error = %v, want sticky deadline", err)
	}
}

func TestOwnedConnectorCloseBoundedIncludesFinalClose(t *testing.T) {
	connectorCloseStarted := make(chan struct{})
	unblockConnectorClose := make(chan struct{})
	instanceCloseStarted := make(chan struct{})
	unblockInstanceClose := make(chan struct{})
	instanceCloseFinished := make(chan struct{})
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	connector := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			close(connectorCloseStarted)
			<-unblockConnectorClose
			return nil
		}},
		&fakeConnectorInstance{close: func() error {
			instanceCloses.Add(1)
			close(instanceCloseStarted)
			<-unblockInstanceClose
			close(instanceCloseFinished)
			return nil
		}},
	)

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- connector.closeBounded(20 * time.Millisecond)
	}()
	select {
	case <-connectorCloseStarted:
	case <-time.After(time.Second):
		t.Fatal("connector close did not start")
	}
	select {
	case err := <-closeReturned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("closeBounded error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closeBounded remained blocked in connector close")
	}
	if instanceCloses.Load() != 0 {
		t.Fatalf("instance close count = %d before connector close completed, want 0", instanceCloses.Load())
	}

	close(unblockConnectorClose)
	select {
	case <-instanceCloseStarted:
	case <-time.After(time.Second):
		t.Fatal("instance close did not start after connector close completed")
	}
	if err := connector.closeBounded(time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated closeBounded error = %v, want sticky deadline", err)
	}
	close(unblockInstanceClose)
	select {
	case <-instanceCloseFinished:
	case <-time.After(time.Second):
		t.Fatal("final close did not finish after being unblocked")
	}
	if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		t.Fatalf("close counts = connector:%d instance:%d, want 1/1",
			connectorCloses.Load(), instanceCloses.Load())
	}
}

func TestOwnedConnectorCloseBoundedIncludesInstanceClose(t *testing.T) {
	instanceCloseStarted := make(chan struct{})
	unblockInstanceClose := make(chan struct{})
	instanceCloseFinished := make(chan struct{})
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	connector := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			return nil
		}},
		&fakeConnectorInstance{close: func() error {
			instanceCloses.Add(1)
			close(instanceCloseStarted)
			<-unblockInstanceClose
			close(instanceCloseFinished)
			return nil
		}},
	)

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- connector.closeBounded(20 * time.Millisecond)
	}()
	select {
	case <-instanceCloseStarted:
	case <-time.After(time.Second):
		t.Fatal("instance close did not start")
	}
	select {
	case err := <-closeReturned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("closeBounded error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closeBounded remained blocked in instance close")
	}
	if err := connector.closeBounded(time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated closeBounded error = %v, want sticky deadline", err)
	}
	close(unblockInstanceClose)
	select {
	case <-instanceCloseFinished:
	case <-time.After(time.Second):
		t.Fatal("instance close did not finish after being unblocked")
	}
	if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		t.Fatalf("close counts = connector:%d instance:%d, want 1/1",
			connectorCloses.Load(), instanceCloses.Load())
	}
}

func TestOwnedConnectorCloseRecoversResourcePanics(t *testing.T) {
	tests := []struct {
		name           string
		connectorPanic bool
		instancePanic  bool
		wantErrors     []string
	}{
		{
			name:           "connector panic",
			connectorPanic: true,
			wantErrors:     []string{"shoal: internal panic closing connector: connector exploded"},
		},
		{
			name:          "instance panic",
			instancePanic: true,
			wantErrors:    []string{"shoal: internal panic closing instance: instance exploded"},
		},
		{
			name:           "both panic",
			connectorPanic: true,
			instancePanic:  true,
			wantErrors: []string{
				"shoal: internal panic closing connector: connector exploded",
				"shoal: internal panic closing instance: instance exploded",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var connectorCloses atomic.Int32
			var instanceCloses atomic.Int32
			connector := newOwnedConnector(
				&fakeConnectorAPI{close: func() error {
					connectorCloses.Add(1)
					if test.connectorPanic {
						panic("connector exploded")
					}
					return nil
				}},
				&fakeConnectorInstance{close: func() error {
					instanceCloses.Add(1)
					if test.instancePanic {
						panic("instance exploded")
					}
					return nil
				}},
			)

			err := connector.close()
			if err == nil {
				t.Fatal("close error = nil, want recovered panic")
			}
			for _, want := range test.wantErrors {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("close error = %q, want %q", err, want)
				}
			}
			if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
				t.Fatalf("close counts = connector:%d instance:%d, want 1/1",
					connectorCloses.Load(), instanceCloses.Load())
			}

			stickyErr := connector.close()
			if stickyErr == nil || stickyErr.Error() != err.Error() {
				t.Fatalf("repeated close error = %v, want sticky %v", stickyErr, err)
			}
			if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
				t.Fatalf("repeated close counts = connector:%d instance:%d, want 1/1",
					connectorCloses.Load(), instanceCloses.Load())
			}
		})
	}
}

func TestOwnedConnectorCloseBoundedBackgroundCleanupWaitsAndRecoversPanics(t *testing.T) {
	originalFreeTimeout := connectorFreeTimeout
	connectorFreeTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		connectorFreeTimeout = originalFreeTimeout
	})

	connectorCloseStarted := make(chan struct{})
	unblockConnectorClose := make(chan struct{})
	backgroundCloseFinished := make(chan struct{})
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	connector := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			close(connectorCloseStarted)
			<-unblockConnectorClose
			panic("connector exploded")
		}},
		&fakeConnectorInstance{close: func() error {
			defer close(backgroundCloseFinished)
			instanceCloses.Add(1)
			panic("instance exploded")
		}},
	)
	_, operationDone, err := connector.begin(0)
	if err != nil {
		t.Fatal(err)
	}

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- connector.closeBounded(connectorFreeTimeout)
	}()
	select {
	case err := <-closeReturned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("closeBounded error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closeBounded remained blocked waiting for the active operation")
	}
	select {
	case <-connectorCloseStarted:
		t.Fatal("background close started before the active operation completed")
	default:
	}
	if err := connector.closeBounded(time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated closeBounded error = %v, want sticky deadline", err)
	}

	time.Sleep(2 * connectorFreeTimeout)
	operationDone()
	select {
	case <-connectorCloseStarted:
	case <-time.After(time.Second):
		t.Fatal("background connector close did not start")
	}

	close(unblockConnectorClose)
	select {
	case <-backgroundCloseFinished:
	case <-time.After(time.Second):
		t.Fatal("background close did not continue to the instance after connector panic")
	}
	if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		t.Fatalf("background close counts = connector:%d instance:%d, want 1/1",
			connectorCloses.Load(), instanceCloses.Load())
	}
}

func TestOwnedConnectorCloseIsConcurrentAndIdempotent(t *testing.T) {
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	connector := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			return nil
		}},
		&fakeConnectorInstance{close: func() error {
			instanceCloses.Add(1)
			return nil
		}},
	)
	const callers = 16
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			if err := connector.close(); err != nil {
				t.Errorf("close error = %v", err)
			}
		}()
	}
	wait.Wait()
	if connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		t.Fatalf("close counts = connector:%d instance:%d, want 1/1",
			connectorCloses.Load(), instanceCloses.Load())
	}
}

func TestOwnedScannerCloseCancelsAndJoinsActiveCalls(t *testing.T) {
	scanner := newOwnedScanner(nil, nil, nil)
	ctx, done, err := scanner.begin(0)
	if err != nil {
		t.Fatal(err)
	}

	closeReturned := make(chan struct{})
	go func() {
		scanner.close()
		close(closeReturned)
	}()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("close did not cancel active call")
	}
	select {
	case <-closeReturned:
		t.Fatal("close returned before active call completed")
	default:
	}

	done()
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("close did not join completed call")
	}
	if _, _, err := scanner.begin(0); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("begin after close error = %v, want connector closed", err)
	}
}

func TestOwnedScannerCloseIsConcurrentAndIdempotent(t *testing.T) {
	scanner := newOwnedScanner(nil, nil, nil)
	const callers = 16
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			scanner.close()
		}()
	}
	wait.Wait()
}

func TestOwnedScannerDeadline(t *testing.T) {
	scanner := newOwnedScanner(nil, nil, nil)
	ctx, done, err := scanner.begin(time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("operation context did not reach deadline")
	}
}

func TestOwnedScannerOwnerCloseBoundedRejectsNewCallsAndWaitsForInflightScans(t *testing.T) {
	originalFreeTimeout := connectorFreeTimeout
	connectorFreeTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		connectorFreeTimeout = originalFreeTimeout
	})

	connectorCloseStarted := make(chan struct{})
	var connectorCloses atomic.Int32
	var instanceCloses atomic.Int32
	owner := newOwnedConnector(
		&fakeConnectorAPI{close: func() error {
			connectorCloses.Add(1)
			close(connectorCloseStarted)
			return nil
		}},
		&fakeConnectorInstance{close: func() error {
			instanceCloses.Add(1)
			return nil
		}},
	)
	adminCtx, adminDone, err := owner.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	scanner := newOwnedScanner(nil, nil, owner)
	batchScanner := newOwnedScanner(nil, nil, owner)
	scannerCtx, scannerDone, err := scanner.begin(0)
	if err != nil {
		t.Fatal(err)
	}

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- owner.closeBounded(connectorFreeTimeout)
	}()

	select {
	case err := <-closeReturned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("closeBounded error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closeBounded remained blocked waiting for active admin call")
	}
	if !errors.Is(adminCtx.Err(), context.Canceled) {
		t.Fatalf("admin context error = %v, want canceled", adminCtx.Err())
	}
	select {
	case <-scannerCtx.Done():
		t.Fatalf("scanner context error = %v, want in-flight scan to continue", scannerCtx.Err())
	default:
	}
	if _, _, err := scanner.begin(0); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("scanner begin after owner close error = %v, want connector closed", err)
	}
	if _, _, err := batchScanner.begin(0); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("batch scanner begin after owner close error = %v, want connector closed", err)
	}

	adminDone()
	select {
	case <-connectorCloseStarted:
		t.Fatal("connector close started before in-flight scanner completed")
	default:
	}

	scannerDone()
	select {
	case <-connectorCloseStarted:
	case <-time.After(time.Second):
		t.Fatal("connector close did not start after scanner completed")
	}
	deadline := time.After(time.Second)
	for connectorCloses.Load() != 1 || instanceCloses.Load() != 1 {
		select {
		case <-deadline:
			t.Fatalf(
				"close counts = connector:%d instance:%d, want 1/1",
				connectorCloses.Load(),
				instanceCloses.Load(),
			)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type fakeCAPIWriter struct {
	add   func(context.Context, *accumulo.Mutation) error
	flush func(context.Context) error
	close func(context.Context) error
}

func (w *fakeCAPIWriter) Add(ctx context.Context, mutation *accumulo.Mutation) error {
	return w.add(ctx, mutation)
}

func (w *fakeCAPIWriter) Flush(ctx context.Context) error {
	return w.flush(ctx)
}

func (w *fakeCAPIWriter) Close(ctx context.Context) error {
	return w.close(ctx)
}

func TestOwnedBatchWriterCloseCancelsAndJoinsActiveCalls(t *testing.T) {
	writer := newOwnedBatchWriter(&fakeCAPIWriter{
		add:   func(context.Context, *accumulo.Mutation) error { return nil },
		flush: func(context.Context) error { return nil },
		close: func(context.Context) error { return nil },
	}, nil)
	ctx, done, err := writer.begin(0)
	if err != nil {
		t.Fatal(err)
	}

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- writer.close(time.Second)
	}()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("close did not cancel active writer call")
	}
	select {
	case <-closeReturned:
		t.Fatal("close returned before active writer call completed")
	default:
	}

	done()
	select {
	case err := <-closeReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not join active writer call")
	}
	if _, _, err := writer.begin(0); !errors.Is(err, accumulo.ErrBatchWriterClosed) {
		t.Fatalf("begin after close error = %v, want batch writer closed", err)
	}
}

func TestOwnedBatchWriterCloseDeadlineBoundsActiveWait(t *testing.T) {
	writer := newOwnedBatchWriter(&fakeCAPIWriter{
		add:   func(context.Context, *accumulo.Mutation) error { return nil },
		flush: func(context.Context) error { return nil },
		close: func(context.Context) error { return nil },
	}, nil)
	ctx, done, err := writer.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	defer done()

	start := time.Now()
	if err := writer.close(20 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("close exceeded deadline bound: %v", elapsed)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("active context error = %v, want canceled", ctx.Err())
	}
	done()
	if err := writer.close(time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated close error = %v, want sticky deadline", err)
	}
}

func TestOwnedBatchWriterRejectsOperationsAfterConnectorClose(t *testing.T) {
	owner := &ownedConnector{}
	owner.closed.Store(true)
	writer := newOwnedBatchWriter(&fakeCAPIWriter{
		add:   func(context.Context, *accumulo.Mutation) error { return nil },
		flush: func(context.Context) error { return nil },
		close: func(context.Context) error { return nil },
	}, owner)
	if _, _, err := writer.begin(0); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("begin error = %v, want connector closed", err)
	}
}

func TestMutationRegistryLifecycle(t *testing.T) {
	registry := newMutationRegistry()
	mutation, err := accumulo.NewMutation([]byte("row"))
	if err != nil {
		t.Fatal(err)
	}
	id, ok := registry.add(mutation)
	if !ok || id == 0 {
		t.Fatalf("add = %d, %v", id, ok)
	}
	if got, ok := registry.get(id); !ok || got != mutation {
		t.Fatalf("get = %p, %v", got, ok)
	}
	if got, ok := registry.remove(id); !ok || got != mutation {
		t.Fatalf("remove = %p, %v", got, ok)
	}
	if _, ok := registry.get(id); ok {
		t.Fatal("removed mutation remains registered")
	}
}
