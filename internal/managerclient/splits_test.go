package managerclient

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"

	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

// UpdateTabletMergeability keeps the shared fakeManagerRPC
// (managerclient_test.go) satisfying managerRPC after split support widened
// the interface. Split tests use fakeSplitRPC below instead.
func (r *fakeManagerRPC) UpdateTabletMergeability(
	context.Context,
	*security.TCredentials,
	string,
	[]MergeabilityUpdate,
) ([]TabletExtent, error) {
	return nil, nil
}

// fakeSplitFateRPC is a fateRPC whose wait status is scriptable, so split
// tests can distinguish "SPLIT_SUCCEEDED" from the empty status Accumulo
// returns when the requested extent no longer exists.
type fakeSplitFateRPC struct {
	id         fateID
	waitStatus string
	wait       func() error
	finishErr  error
	request    Request
	instance   FateInstance

	begin            atomic.Int32
	execute          atomic.Int32
	waitCalls        atomic.Int32
	finish           atomic.Int32
	finishContextErr error
}

func (r *fakeSplitFateRPC) Begin(
	_ context.Context,
	_ *security.TCredentials,
	instance FateInstance,
) (fateID, error) {
	r.begin.Add(1)
	r.instance = instance
	return r.id, nil
}

func (r *fakeSplitFateRPC) Execute(
	_ context.Context,
	_ *security.TCredentials,
	_ fateID,
	req Request,
) error {
	r.execute.Add(1)
	r.request = req
	return nil
}

func (r *fakeSplitFateRPC) Wait(
	context.Context,
	*security.TCredentials,
	fateID,
) (string, error) {
	r.waitCalls.Add(1)
	if r.wait != nil {
		return "", r.wait()
	}
	return r.waitStatus, nil
}

func (r *fakeSplitFateRPC) Finish(ctx context.Context, _ *security.TCredentials, _ fateID) error {
	r.finish.Add(1)
	r.finishContextErr = ctx.Err()
	return r.finishErr
}

func splitFateFromFakeTransport(transport io.Closer) (fateRPC, error) {
	return transport.(*fakeTransport).rpc, nil
}

type splitCloseErrorTransport struct {
	rpc      fateRPC
	closeErr error
	closes   atomic.Int32
}

func (t *splitCloseErrorTransport) Close() error {
	t.closes.Add(1)
	return t.closeErr
}

type fakeSplitRPC struct {
	fakeManagerRPC

	mu        sync.Mutex
	tableName string
	updates   []MergeabilityUpdate
	result    []TabletExtent
	err       error
	calls     atomic.Int32
}

func (r *fakeSplitRPC) UpdateTabletMergeability(
	_ context.Context,
	_ *security.TCredentials,
	tableName string,
	updates []MergeabilityUpdate,
) ([]TabletExtent, error) {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tableName = tableName
	r.updates = updates
	return r.result, r.err
}

func splitRequest(payloads ...string) Request {
	arguments := [][]byte{[]byte("1"), []byte("p"), []byte("k")}
	for _, payload := range payloads {
		arguments = append(arguments, []byte(payload))
	}
	return Request{
		Operation: TableSplit,
		Instance:  FateUser,
		Arguments: arguments,
		Options:   map[string]string{},
	}
}

func TestThriftOperationMapsTableSplit(t *testing.T) {
	if got := thriftOperation(TableSplit); got != manager.TFateOperation_TABLE_SPLIT {
		t.Fatalf("thriftOperation(TableSplit) = %v, want TABLE_SPLIT", got)
	}
}

func TestValidateRequestSplitArgumentCounts(t *testing.T) {
	tests := []struct {
		name    string
		req     Request
		wantErr bool
	}{
		{"minimum", splitRequest(`{"split":"YQ==","never":true}`), false},
		{"many splits", splitRequest("a", "b", "c"), false},
		{
			"too few",
			Request{Operation: TableSplit, Instance: FateUser, Arguments: [][]byte{
				[]byte("1"), []byte("p"), []byte("k"),
			}},
			true,
		},
		{
			"nil argument",
			Request{Operation: TableSplit, Instance: FateUser, Arguments: [][]byte{
				[]byte("1"), nil, []byte("k"), []byte("payload"),
			}},
			true,
		},
		{
			"unbounded boundaries",
			Request{Operation: TableSplit, Instance: FateMeta, Arguments: [][]byte{
				[]byte("+a"), {}, {}, []byte("payload"),
			}},
			false,
		},
	}
	for _, tt := range tests {
		err := validateRequest(tt.req)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: validateRequest error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestPooledExecuteStatusReturnsFateStatusAndFinishes(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeSplitFateRPC{id: fateID{Type: 1, UUID: "split-1"}, waitStatus: SplitSucceeded}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = splitFateFromFakeTransport

	req := splitRequest(`{"split":"YQ==","never":true}`)
	status, err := pooled.ExecuteStatus(context.Background(), "manager:9997", req)
	if err != nil {
		t.Fatal(err)
	}
	if status != SplitSucceeded {
		t.Fatalf("status = %q, want %q", status, SplitSucceeded)
	}
	if rpc.begin.Load() != 1 || rpc.execute.Load() != 1 ||
		rpc.waitCalls.Load() != 1 || rpc.finish.Load() != 1 {
		t.Fatalf("calls begin/execute/wait/finish = %d/%d/%d/%d",
			rpc.begin.Load(), rpc.execute.Load(), rpc.waitCalls.Load(), rpc.finish.Load())
	}
	if rpc.request.Operation != TableSplit {
		t.Fatalf("operation = %v, want TableSplit", rpc.request.Operation)
	}
	want := []string{"1", "p", "k", `{"split":"YQ==","never":true}`}
	if len(rpc.request.Arguments) != len(want) {
		t.Fatalf("arguments = %q, want %q", rpc.request.Arguments, want)
	}
	for i, argument := range want {
		if got := string(rpc.request.Arguments[i]); got != argument {
			t.Fatalf("argument %d = %q, want %q", i, got, argument)
		}
	}
}

func TestPooledExecuteStatusReportsEmptyStatusWithoutError(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeSplitFateRPC{id: fateID{Type: 1, UUID: "split-2"}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = splitFateFromFakeTransport

	status, err := pooled.ExecuteStatus(context.Background(), "manager:9997", splitRequest("payload"))
	if err != nil || status != "" {
		t.Fatalf("status/err = %q/%v, want empty status and no error", status, err)
	}
	if rpc.finish.Load() != 1 {
		t.Fatalf("finish calls = %d, want 1", rpc.finish.Load())
	}
}

func TestPooledExecuteStatusKeepsStatusWhenCleanupFails(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	cleanup := errors.New("finish exploded")
	rpc := &fakeSplitFateRPC{
		id:         fateID{Type: 1, UUID: "split-3"},
		waitStatus: SplitSucceeded,
		finishErr:  cleanup,
	}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = splitFateFromFakeTransport

	status, err := pooled.ExecuteStatus(context.Background(), "manager:9997", splitRequest("payload"))
	if !errors.Is(err, cleanup) {
		t.Fatalf("error = %v, want the joined cleanup failure", err)
	}
	if status != SplitSucceeded {
		t.Fatalf("status = %q, want the split status to survive cleanup failure", status)
	}
}

func TestPooledExecuteStatusKeepsWaitStatusWhenLeaseCleanupFails(t *testing.T) {
	pool, err := transportpool.New(transportpool.Config{MaxIdlePerEndpoint: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pooled, err := NewPooled(pool, "uuid-1", "4.0.0-SNAPSHOT", &security.TCredentials{
		Principal:      "root",
		TokenClassName: "PasswordToken",
		Token:          []byte("secret"),
		InstanceId:     "uuid-1",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	rpc := &fakeSplitFateRPC{id: fateID{Type: 1, UUID: "split-cleanup"}, waitStatus: SplitSucceeded}
	cleanup := thrift.NewTTransportExceptionFromError(errors.New("wait transport close failed"))
	var dials atomic.Int32
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		transport := &splitCloseErrorTransport{rpc: rpc}
		if dials.Add(1) == 3 {
			transport.closeErr = cleanup
		}
		return transport, nil
	}
	pooled.newClient = func(transport io.Closer) (fateRPC, error) {
		return transport.(*splitCloseErrorTransport).rpc, nil
	}

	status, err := pooled.ExecuteStatus(
		context.Background(),
		"manager:9997",
		splitRequest("payload"),
	)
	if status != SplitSucceeded {
		t.Fatalf("status = %q, want %q despite lease cleanup failure", status, SplitSucceeded)
	}
	if !errors.Is(err, cleanup) {
		t.Fatalf("error = %v, want wait lease cleanup failure", err)
	}
	if !IsRetryableEndpointError(err) {
		t.Fatalf("cleanup error = %v, want existing endpoint retry classification", err)
	}
	if dials.Load() != 4 {
		t.Fatalf("dials = %d, want begin/execute/wait/finish", dials.Load())
	}
	if rpc.begin.Load() != 1 || rpc.execute.Load() != 1 ||
		rpc.waitCalls.Load() != 1 || rpc.finish.Load() != 1 {
		t.Fatalf("calls begin/execute/wait/finish = %d/%d/%d/%d",
			rpc.begin.Load(), rpc.execute.Load(), rpc.waitCalls.Load(), rpc.finish.Load())
	}
}

func TestPooledExecuteStatusFinishesWithCancelledContext(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	rpc := &fakeSplitFateRPC{id: fateID{Type: 1, UUID: "split-4"}, wait: func() error {
		cancel()
		return context.Canceled
	}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = splitFateFromFakeTransport

	status, err := pooled.ExecuteStatus(ctx, "manager:9997", splitRequest("payload"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if status != "" {
		t.Fatalf("status = %q, want empty", status)
	}
	if rpc.finish.Load() != 1 || rpc.finishContextErr != nil {
		t.Fatalf("finish calls/context = %d/%v", rpc.finish.Load(), rpc.finishContextErr)
	}
}

func TestPooledExecuteStatusRejectsShortSplitRequests(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		t.Fatal("invalid request reached the wire")
		return nil, nil
	}

	if _, err := pooled.ExecuteStatus(context.Background(), "manager:9997", Request{
		Operation: TableSplit,
		Instance:  FateUser,
		Arguments: [][]byte{[]byte("1"), {}, {}},
	}); err == nil {
		t.Fatal("expected a validation error for a split request without payloads")
	}
}

func TestPooledUpdateTabletMergeabilityPassesNameAndUpdates(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeSplitRPC{result: []TabletExtent{
		{TableID: "1", EndRow: []byte("p"), PrevEndRow: []byte("k")},
		{TableID: "1"},
	}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{manager: rpc}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	updates := []MergeabilityUpdate{{
		Extent:       TabletExtent{TableID: "1", EndRow: []byte("p"), PrevEndRow: []byte("k")},
		Mergeability: NeverMergeable(),
	}}
	updated, err := pooled.UpdateTabletMergeability(
		context.Background(),
		"manager:9997",
		"events",
		updates,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 2 {
		t.Fatalf("updated = %#v, want both accepted extents", updated)
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if rpc.tableName != "events" {
		t.Fatalf("table name = %q, want the qualified table name", rpc.tableName)
	}
	if len(rpc.updates) != 1 {
		t.Fatalf("updates = %#v", rpc.updates)
	}
	if !rpc.updates[0].Mergeability.Never || rpc.updates[0].Mergeability.DelayNanos != -1 {
		t.Fatalf("mergeability = %+v, want never with delay -1", rpc.updates[0].Mergeability)
	}
}

func TestPooledUpdateTabletMergeabilityKeepsConfirmedUpdatesOnCleanupFailure(t *testing.T) {
	pool, err := transportpool.New(transportpool.Config{MaxIdlePerEndpoint: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	pooled, err := NewPooled(pool, "uuid-1", "4.0.0-SNAPSHOT", &security.TCredentials{
		Principal:      "root",
		TokenClassName: "PasswordToken",
		Token:          []byte("secret"),
		InstanceId:     "uuid-1",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	cleanupErr := thrift.NewTTransportExceptionFromError(errors.New("close failed"))
	rpc := &fakeSplitRPC{result: []TabletExtent{
		{TableID: "1", EndRow: []byte("p"), PrevEndRow: []byte("k")},
	}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{manager: rpc, closeErr: cleanupErr}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	updated, err := pooled.UpdateTabletMergeability(
		context.Background(),
		"manager:9997",
		"events",
		[]MergeabilityUpdate{{
			Extent:       TabletExtent{TableID: "1", EndRow: []byte("p"), PrevEndRow: []byte("k")},
			Mergeability: NeverMergeable(),
		}},
	)
	if len(updated) != 1 || updated[0].TableID != "1" {
		t.Fatalf("updated = %#v, want the confirmed extent", updated)
	}
	var postResponseErr *PostResponseCleanupError
	if !errors.As(err, &postResponseErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %#v, want post-response cleanup error", err)
	}
}

func TestPooledUpdateTabletMergeabilityValidatesAndMapsErrors(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeSplitRPC{err: &clientgen.ThriftTableOperationException{
		Type:      clientgen.TableOperationExceptionType_OFFLINE,
		TableName: "events",
	}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{manager: rpc}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	valid := []MergeabilityUpdate{{
		Extent:       TabletExtent{TableID: "1", EndRow: []byte("p")},
		Mergeability: NeverMergeable(),
	}}
	if _, err := pooled.UpdateTabletMergeability(
		context.Background(),
		"manager:9997",
		"",
		valid,
	); err == nil {
		t.Fatal("expected an empty table name to be rejected")
	}
	if _, err := pooled.UpdateTabletMergeability(
		context.Background(),
		"manager:9997",
		"events",
		nil,
	); err == nil {
		t.Fatal("expected an empty update set to be rejected")
	}
	if _, err := pooled.UpdateTabletMergeability(
		context.Background(),
		"manager:9997",
		"events",
		[]MergeabilityUpdate{{Mergeability: NeverMergeable()}},
	); err == nil {
		t.Fatal("expected an update without a table ID to be rejected")
	}
	if rpc.calls.Load() != 0 {
		t.Fatalf("rejected updates reached the wire: %d", rpc.calls.Load())
	}

	_, err := pooled.UpdateTabletMergeability(
		context.Background(),
		"manager:9997",
		"events",
		valid,
	)
	var managerErr *Error
	if !errors.As(err, &managerErr) || managerErr.Kind != ErrorTableOffline {
		t.Fatalf("error = %#v, want ErrorTableOffline", err)
	}
}

func TestThriftKeyExtentOmitsUnboundedBoundariesOnTheWire(t *testing.T) {
	bounded := thriftKeyExtent(TabletExtent{
		TableID:    "1",
		EndRow:     []byte("p"),
		PrevEndRow: []byte("k"),
	})
	if got, want := thriftFieldIDs(t, bounded), []int16{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("bounded field IDs = %v, want %v", got, want)
	}
	unbounded := thriftKeyExtent(TabletExtent{TableID: "1"})
	if got, want := thriftFieldIDs(t, unbounded), []int16{1}; !slices.Equal(got, want) {
		t.Fatalf("unbounded field IDs = %v, want %v", got, want)
	}
	empty := thriftKeyExtent(TabletExtent{TableID: "1", EndRow: []byte{}, PrevEndRow: []byte{}})
	if got, want := thriftFieldIDs(t, empty), []int16{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("empty-boundary field IDs = %v, want %v", got, want)
	}
	if string(bounded.Table) != "1" {
		t.Fatalf("table = %q, want the canonical table ID", bounded.Table)
	}
}

func TestThriftKeyExtentCopiesRows(t *testing.T) {
	row := []byte("p")
	extent := thriftKeyExtent(TabletExtent{TableID: "1", EndRow: row})
	row[0] = 'z'
	if string(extent.EndRow) != "p" {
		t.Fatalf("end row aliased the caller slice: %q", extent.EndRow)
	}
}

func TestNeverMergeableMatchesAccumuloThriftEncoding(t *testing.T) {
	mergeability := NeverMergeable()
	if !mergeability.Never || mergeability.DelayNanos != -1 {
		t.Fatalf("NeverMergeable = %+v, want {true -1}", mergeability)
	}
	wire := &manager.TTabletMergeability{
		Never: mergeability.Never,
		Delay: mergeability.DelayNanos,
	}
	if got, want := thriftFieldIDs(t, wire), []int16{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("mergeability field IDs = %v, want %v", got, want)
	}
}

func TestMapRPCErrorMapsOfflineTables(t *testing.T) {
	err := mapRPCError(&clientgen.ThriftTableOperationException{
		Type:      clientgen.TableOperationExceptionType_OFFLINE,
		TableName: "events",
	})
	var managerErr *Error
	if !errors.As(err, &managerErr) || managerErr.Kind != ErrorTableOffline {
		t.Fatalf("error = %#v, want ErrorTableOffline", err)
	}
	if managerErr.TableName != "events" {
		t.Fatalf("table name = %q", managerErr.TableName)
	}
}
