package managerclient

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

func TestPooledExecuteLifecycleAndReuse(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	rpc := &fakeFateRPC{id: fateID{Type: 1, UUID: "abc"}}
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		want := transportpool.Key{
			Address:         "manager:9997",
			Service:         "fate",
			InstanceID:      "uuid-1",
			ProtocolVersion: "4.0.0-SNAPSHOT",
		}
		if key != want {
			t.Fatalf("key = %+v, want %+v", key, want)
		}
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	for range 2 {
		if err := pooled.Execute(context.Background(), "manager:9997", createRequest("events")); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
	if rpc.begin.Load() != 2 || rpc.execute.Load() != 2 || rpc.waitCalls.Load() != 2 || rpc.finish.Load() != 2 {
		t.Fatalf("calls begin/execute/wait/finish = %d/%d/%d/%d",
			rpc.begin.Load(), rpc.execute.Load(), rpc.waitCalls.Load(), rpc.finish.Load())
	}
	if got := string(rpc.request.Arguments[0]); got != "events" {
		t.Fatalf("table argument = %q", got)
	}
	if rpc.instance != FateUser {
		t.Fatalf("FATE instance = %v, want user", rpc.instance)
	}
}

func TestPooledExecuteBulkImportLifecycle(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeFateRPC{id: fateID{Type: 1, UUID: "bulk-1"}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	req := Request{
		Operation: TableBulkImport,
		Instance:  FateUser,
		Arguments: [][]byte{[]byte("t-1"), []byte("hdfs://nn/bulk/events"), []byte("true")},
		Options:   map[string]string{},
	}
	if err := pooled.Execute(context.Background(), "manager:9997", req); err != nil {
		t.Fatal(err)
	}
	if rpc.begin.Load() != 1 || rpc.execute.Load() != 1 || rpc.waitCalls.Load() != 1 || rpc.finish.Load() != 1 {
		t.Fatalf("calls begin/execute/wait/finish = %d/%d/%d/%d",
			rpc.begin.Load(), rpc.execute.Load(), rpc.waitCalls.Load(), rpc.finish.Load())
	}
	if rpc.request.Operation != TableBulkImport {
		t.Fatalf("operation = %v, want TableBulkImport", rpc.request.Operation)
	}
	wantArgs := []string{"t-1", "hdfs://nn/bulk/events", "true"}
	if len(rpc.request.Arguments) != len(wantArgs) {
		t.Fatalf("arguments = %#v, want %v", rpc.request.Arguments, wantArgs)
	}
	for i, want := range wantArgs {
		if got := string(rpc.request.Arguments[i]); got != want {
			t.Fatalf("argument %d = %q, want %q", i, got, want)
		}
	}
}

func TestPooledDurableFateLifecycleDoesNotFinishBeforeCaller(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeFateRPC{id: fateID{Type: 1, UUID: "durable-1"}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport
	req := Request{
		Operation: TableBulkImport, Instance: FateUser,
		Arguments: [][]byte{[]byte("t-1"), []byte("hdfs://nn/bulk/events"), []byte("false")},
		Options:   map[string]string{},
	}
	id, err := pooled.BeginFate(context.Background(), "manager:9997", FateUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := pooled.ExecuteFate(context.Background(), "manager:9997", id, req); err != nil {
		t.Fatal(err)
	}
	if _, err := pooled.WaitFate(context.Background(), "manager:9997", id); err != nil {
		t.Fatal(err)
	}
	if rpc.finish.Load() != 0 {
		t.Fatalf("finish calls before durable record = %d, want 0", rpc.finish.Load())
	}
	if err := pooled.FinishFate(context.Background(), "manager:9997", id); err != nil {
		t.Fatal(err)
	}
	if rpc.finish.Load() != 1 {
		t.Fatalf("finish calls = %d, want 1", rpc.finish.Load())
	}
}

func TestPooledFinishesAfterOperationFailure(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeFateRPC{
		id: fateID{Type: 1, UUID: "abc"},
		executeErr: &clientgen.ThriftTableOperationException{
			Type:      clientgen.TableOperationExceptionType_EXISTS,
			TableName: "events",
		},
	}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	err := pooled.Execute(context.Background(), "manager:9997", createRequest("events"))
	var managerErr *Error
	if !errors.As(err, &managerErr) || managerErr.Kind != ErrorTableExists {
		t.Fatalf("error = %#v, want ErrorTableExists", err)
	}
	if rpc.finish.Load() != 1 {
		t.Fatalf("finish calls = %d, want 1", rpc.finish.Load())
	}
}

func TestPooledExecuteStatusContinuesWaitingAfterExecuteCleanupFailure(t *testing.T) {
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

	cleanupErr := thrift.NewTTransportExceptionFromError(errors.New("execute close failed"))
	rpc := &fakeFateRPC{
		id:         fateID{Type: 1, UUID: "abc"},
		waitStatus: "COMMITTED",
	}
	var dials atomic.Int32
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		if dials.Add(1) == 2 {
			return &fakeTransport{rpc: rpc, closeErr: cleanupErr}, nil
		}
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	status, err := pooled.ExecuteStatus(context.Background(), "manager:9997", createRequest("events"))
	if status != "COMMITTED" {
		t.Fatalf("status = %q, want wait status", status)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want execute cleanup preserved", err)
	}
	if rpc.begin.Load() != 1 || rpc.execute.Load() != 1 || rpc.waitCalls.Load() != 1 || rpc.finish.Load() != 1 {
		t.Fatalf("calls begin/execute/wait/finish = %d/%d/%d/%d",
			rpc.begin.Load(), rpc.execute.Load(), rpc.waitCalls.Load(), rpc.finish.Load())
	}
}

func TestPooledFinishesWithCancelledContext(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	rpc := &fakeFateRPC{id: fateID{Type: 1, UUID: "abc"}, wait: func() error {
		cancel()
		return context.Canceled
	}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	if err := pooled.Execute(ctx, "manager:9997", createRequest("events")); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if rpc.finish.Load() != 1 || rpc.finishContextErr != nil {
		t.Fatalf("finish calls/context = %d/%v", rpc.finish.Load(), rpc.finishContextErr)
	}
}

func TestPooledBoundsFinishCleanup(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()
	pooled.finishTimeout = 20 * time.Millisecond

	rpc := &fakeFateRPC{
		id: fateID{Type: 1, UUID: "abc"},
		finishFunc: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	started := time.Now()
	err := pooled.Execute(context.Background(), "manager:9997", createRequest("events"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded finish took %v", elapsed)
	}
}

func TestPooledInvalidatesWireFailureAndCloseRace(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	var first *fakeTransport
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		n := dials.Add(1)
		rpc := &fakeFateRPC{id: fateID{Type: 1, UUID: "abc"}}
		if n == 1 {
			rpc.beginErr = thrift.NewTTransportExceptionFromError(errors.New("reset"))
		}
		transport := &fakeTransport{rpc: rpc}
		if n == 1 {
			first = transport
		}
		return transport, nil
	}
	pooled.newClient = clientFromFakeTransport

	if err := pooled.Execute(context.Background(), "manager:9997", createRequest("events")); err == nil {
		t.Fatal("expected wire error")
	}
	if first.closes.Load() != 1 {
		t.Fatalf("invalidated closes = %d, want 1", first.closes.Load())
	}
	if err := pooled.Execute(context.Background(), "manager:9997", createRequest("events")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pooled.Close()
		}()
	}
	wg.Wait()
	if err := pooled.Execute(context.Background(), "manager:9997", createRequest("events")); err == nil {
		t.Fatal("expected closed error")
	}
}

func TestPooledConcurrentOperationsUseExclusiveTransports(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var active atomic.Int32
	var maxActive atomic.Int32
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(8)
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{rpc: &fakeFateRPC{
			id: fateID{Type: 1, UUID: "abc"},
			beginHook: func() {
				now := active.Add(1)
				for {
					max := maxActive.Load()
					if now <= max || maxActive.CompareAndSwap(max, now) {
						break
					}
				}
				started.Done()
				<-release
			},
			finishHook: func() {
				active.Add(-1)
			},
		}}, nil
	}
	pooled.newClient = clientFromFakeTransport

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pooled.Execute(context.Background(), "manager:9997", createRequest("events")); err != nil {
				t.Errorf("Execute: %v", err)
			}
		}()
	}
	started.Wait()
	close(release)
	wg.Wait()
	if maxActive.Load() < 2 {
		t.Fatalf("max concurrent transports = %d, want at least 2", maxActive.Load())
	}
}

func TestPooledTablePropertyMutationsUseManagerServiceAndReuse(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	rpc := &fakeManagerRPC{}
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		want := transportpool.Key{
			Address:         "manager:9997",
			Service:         "mgr",
			InstanceID:      "uuid-1",
			ProtocolVersion: "4.0.0-SNAPSHOT",
		}
		if key != want {
			t.Fatalf("key = %+v, want %+v", key, want)
		}
		return &fakeTransport{manager: rpc}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	if err := pooled.SetTableProperty(
		context.Background(),
		"manager:9997",
		"events",
		"table.file.compress.type",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := pooled.RemoveTableProperty(
		context.Background(),
		"manager:9997",
		"events",
		"table.file.compress.type",
	); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
	if rpc.setCalls.Load() != 1 || rpc.removeCalls.Load() != 1 {
		t.Fatalf("set/remove calls = %d/%d, want 1/1", rpc.setCalls.Load(), rpc.removeCalls.Load())
	}
	if rpc.tableName != "events" || rpc.property != "table.file.compress.type" || rpc.value != "" {
		t.Fatalf("set request = %q/%q/%q", rpc.tableName, rpc.property, rpc.value)
	}
}

func TestPooledTablePropertyWireFailureEvictsTransport(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	var first *fakeTransport
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		n := dials.Add(1)
		rpc := &fakeManagerRPC{}
		if n == 1 {
			rpc.setErr = thrift.NewTTransportExceptionFromError(errors.New("reset"))
		}
		transport := &fakeTransport{manager: rpc}
		if n == 1 {
			first = transport
		}
		return transport, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	if err := pooled.SetTableProperty(
		context.Background(),
		"manager:9997",
		"events",
		"table.file.compress.type",
		"gz",
	); err == nil {
		t.Fatal("expected wire error")
	}
	if first.closes.Load() != 1 {
		t.Fatalf("invalidated closes = %d, want 1", first.closes.Load())
	}
	if err := pooled.RemoveTableProperty(
		context.Background(),
		"manager:9997",
		"events",
		"table.file.compress.type",
	); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}
}

func TestPooledTablePropertyCancellationAndClose(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeTransport{manager: &fakeManagerRPC{}}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pooled.SetTableProperty(
		ctx,
		"manager:9997",
		"events",
		"table.file.compress.type",
		"gz",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("canceled operation dials = %d, want 0", dials.Load())
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pooled.RemoveTableProperty(
		context.Background(),
		"manager:9997",
		"events",
		"table.file.compress.type",
	); err == nil {
		t.Fatal("expected closed error")
	}
}

func TestPooledFlushTableLifecycleAndWaitModes(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	rpc := &fakeManagerRPC{flushID: 41}
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		if key.Service != managerServiceName {
			t.Fatalf("service = %q, want %q", key.Service, managerServiceName)
		}
		return &fakeTransport{manager: rpc}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	if err := pooled.FlushTable(context.Background(), "manager:9997", "1", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := pooled.FlushTable(context.Background(), "manager:9997", "1", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
	if rpc.initiateCalls.Load() != 2 || rpc.flushWaitCalls.Load() != 2 {
		t.Fatalf(
			"initiate/wait calls = %d/%d, want 2/2",
			rpc.initiateCalls.Load(),
			rpc.flushWaitCalls.Load(),
		)
	}
	if rpc.flushTableID != "1" || rpc.waitFlushID != 41 {
		t.Fatalf("flush table/ID = %q/%d", rpc.flushTableID, rpc.waitFlushID)
	}
	if !slices.Equal(rpc.maxLoops, []int64{noWaitFlushMaxLoops, waitForFlushMaxLoops}) {
		t.Fatalf("max loops = %v", rpc.maxLoops)
	}
}

func TestPooledFlushTableStopsAfterInitiateFailure(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeManagerRPC{
		initiateErr: &clientgen.ThriftTableOperationException{
			Type:    clientgen.TableOperationExceptionType_NOTFOUND,
			TableId: "1",
		},
	}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{manager: rpc}, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	err := pooled.FlushTable(context.Background(), "manager:9997", "1", nil, nil, true)
	var managerErr *Error
	if !errors.As(err, &managerErr) ||
		managerErr.Kind != ErrorTableNotFound ||
		managerErr.TableID != "1" {
		t.Fatalf("error = %#v, want table-not-found for ID 1", err)
	}
	if rpc.flushWaitCalls.Load() != 0 {
		t.Fatalf("wait calls = %d, want 0", rpc.flushWaitCalls.Load())
	}
}

func TestPooledFlushCancellationEvictsTransport(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var dials atomic.Int32
	var first *fakeTransport
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		n := dials.Add(1)
		rpc := &fakeManagerRPC{flushID: 41}
		if n == 1 {
			rpc.waitFlush = func(context.Context) error {
				cancel()
				return context.Canceled
			}
		}
		transport := &fakeTransport{manager: rpc}
		if n == 1 {
			first = transport
		}
		return transport, nil
	}
	pooled.newManagerClient = managerFromFakeTransport

	if err := pooled.FlushTable(ctx, "manager:9997", "1", nil, nil, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush error = %v, want context canceled", err)
	}
	if first.closes.Load() != 1 {
		t.Fatalf("invalidated closes = %d, want 1", first.closes.Load())
	}
	if err := pooled.SetTableProperty(
		context.Background(),
		"manager:9997",
		"events",
		"table.file.compress.type",
		"gz",
	); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}
}

func TestWaitForFlushNilRowsAreAbsentOnWire(t *testing.T) {
	nilRows := manager.NewManagerClientServiceWaitForFlushArgs()
	nilRows.Tinfo = &clientgen.TInfo{}
	nilRows.Credentials = &security.TCredentials{}
	nilRows.TableName = "1"
	nilRows.FlushID = 41
	nilRows.MaxLoops = 1
	if got, want := thriftFieldIDs(t, nilRows), []int16{1, 2, 3, 6, 7}; !slices.Equal(got, want) {
		t.Fatalf("nil-row field IDs = %v, want %v", got, want)
	}

	emptyRows := manager.NewManagerClientServiceWaitForFlushArgs()
	emptyRows.Tinfo = &clientgen.TInfo{}
	emptyRows.Credentials = &security.TCredentials{}
	emptyRows.TableName = "1"
	emptyRows.StartRow = []byte{}
	emptyRows.EndRow = []byte{}
	emptyRows.FlushID = 41
	emptyRows.MaxLoops = 1
	if got, want := thriftFieldIDs(t, emptyRows), []int16{1, 2, 3, 4, 5, 6, 7}; !slices.Equal(got, want) {
		t.Fatalf("empty-row field IDs = %v, want %v", got, want)
	}
}

func TestThriftOperationMapping(t *testing.T) {
	tests := []struct {
		op   Operation
		want manager.TFateOperation
	}{
		{TableCreate, manager.TFateOperation_TABLE_CREATE},
		{TableDelete, manager.TFateOperation_TABLE_DELETE},
		{TableRename, manager.TFateOperation_TABLE_RENAME},
		{TableBulkImport, manager.TFateOperation_TABLE_BULK_IMPORT2},
	}
	for _, tt := range tests {
		if got := thriftOperation(tt.op); got != tt.want {
			t.Fatalf("thriftOperation(%v) = %v, want %v", tt.op, got, tt.want)
		}
	}
}

func TestThriftOperationPanicsOnUnknownOperation(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("thriftOperation did not panic on an unknown operation")
		}
	}()
	thriftOperation(Operation(-1))
}

func TestValidateRequestArgumentCounts(t *testing.T) {
	args := func(n int) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = []byte("x")
		}
		return out
	}
	tests := []struct {
		name    string
		req     Request
		wantErr bool
	}{
		{"create exact", Request{Operation: TableCreate, Instance: FateUser, Arguments: args(5)}, false},
		{"create short", Request{Operation: TableCreate, Instance: FateUser, Arguments: args(4)}, true},
		{"delete exact", Request{Operation: TableDelete, Instance: FateUser, Arguments: args(1)}, false},
		{"delete extra", Request{Operation: TableDelete, Instance: FateUser, Arguments: args(2)}, true},
		{"rename exact", Request{Operation: TableRename, Instance: FateUser, Arguments: args(2)}, false},
		{"rename short", Request{Operation: TableRename, Instance: FateUser, Arguments: args(1)}, true},
		{"bulk import exact", Request{Operation: TableBulkImport, Instance: FateUser, Arguments: args(3)}, false},
		{"bulk import short", Request{Operation: TableBulkImport, Instance: FateUser, Arguments: args(2)}, true},
		{"bulk import extra", Request{Operation: TableBulkImport, Instance: FateMeta, Arguments: args(4)}, true},
		{"unknown operation", Request{Operation: Operation(99), Instance: FateUser, Arguments: args(3)}, true},
		{"unknown instance", Request{Operation: TableBulkImport, Instance: FateInstance(99), Arguments: args(3)}, true},
		{"nil argument", Request{Operation: TableBulkImport, Instance: FateUser, Arguments: [][]byte{[]byte("id"), nil, []byte("true")}}, true},
	}
	for _, tt := range tests {
		err := validateRequest(tt.req)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: validateRequest error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestMapRPCError(t *testing.T) {
	tests := []struct {
		err  error
		kind ErrorKind
	}{
		{&clientgen.ThriftTableOperationException{
			Type: clientgen.TableOperationExceptionType_EXISTS,
		}, ErrorTableExists},
		{&clientgen.ThriftTableOperationException{
			Type: clientgen.TableOperationExceptionType_NOTFOUND,
		}, ErrorTableNotFound},
		{&clientgen.ThriftTableOperationException{
			Type: clientgen.TableOperationExceptionType_NAMESPACE_NOTFOUND,
		}, ErrorNamespaceNotFound},
		{&clientgen.ThriftTableOperationException{
			Type: clientgen.TableOperationExceptionType_INVALID_NAME,
		}, ErrorInvalidName},
		{&manager.ThriftPropertyException{
			Property:    "table.invalid",
			Value:       "x",
			Description: "property is not valid",
		}, ErrorInvalidProperty},
		{&clientgen.ThriftSecurityException{
			Code: clientgen.SecurityErrorCode_PERMISSION_DENIED,
		}, ErrorSecurity},
		{&clientgen.ThriftSecurityException{
			Code: clientgen.SecurityErrorCode_TABLE_DOESNT_EXIST,
		}, ErrorTableNotFound},
		{&clientgen.ThriftSecurityException{
			Code: clientgen.SecurityErrorCode_NAMESPACE_DOESNT_EXIST,
		}, ErrorNamespaceNotFound},
		{&clientgen.ThriftNotActiveServiceException{}, ErrorNotActive},
	}
	for _, tt := range tests {
		var got *Error
		if err := mapRPCError(tt.err); !errors.As(err, &got) || got.Kind != tt.kind {
			t.Fatalf("mapRPCError(%T) = %#v, want kind %d", tt.err, err, tt.kind)
		}
	}
	securityErr := mapRPCError(&clientgen.ThriftSecurityException{
		User: "root",
		Code: clientgen.SecurityErrorCode_PERMISSION_DENIED,
	})
	if got := securityErr.Error(); got != "managerclient: PERMISSION_DENIED" {
		t.Fatalf("security error = %q, want code without principal", got)
	}
	var propertyErr *Error
	if err := mapRPCError(&manager.ThriftPropertyException{
		Property:    "table.invalid",
		Value:       "x",
		Description: "property is not valid",
	}); !errors.As(err, &propertyErr) ||
		propertyErr.Property != "table.invalid" ||
		propertyErr.Value != "x" ||
		propertyErr.Description != "property is not valid" {
		t.Fatalf("property error = %#v", err)
	}
}

func TestPooledGetTableConfigurationReuseEmptyValuesAndCopyIsolation(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	rpc := &fakeClientRPC{properties: map[string]string{
		"table.custom.empty": "",
		"table.file.max":     "15",
	}}
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		want := transportpool.Key{
			Address:         "tablet:9997",
			Service:         "client",
			InstanceID:      "uuid-1",
			ProtocolVersion: "4.0.0-SNAPSHOT",
		}
		if key != want {
			t.Fatalf("key = %+v, want %+v", key, want)
		}
		return &fakeTransport{service: rpc}, nil
	}
	pooled.newServiceClient = serviceFromFakeTransport

	properties, err := pooled.GetTableConfiguration(context.Background(), "tablet:9997", "events")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := properties["table.custom.empty"]; !ok || value != "" {
		t.Fatalf("empty property = %q/%v, want present empty value", value, ok)
	}
	properties["table.file.max"] = "mutated"
	delete(properties, "table.custom.empty")

	again, err := pooled.GetTableConfiguration(context.Background(), "tablet:9997", "events")
	if err != nil {
		t.Fatal(err)
	}
	if again["table.file.max"] != "15" {
		t.Fatalf("caller mutation leaked into subsequent result: %#v", again)
	}
	if value, ok := again["table.custom.empty"]; !ok || value != "" {
		t.Fatalf("empty property after reuse = %q/%v", value, ok)
	}
	if dials.Load() != 1 || rpc.calls.Load() != 2 || rpc.tableName != "events" {
		t.Fatalf("dials/calls/table = %d/%d/%q", dials.Load(), rpc.calls.Load(), rpc.tableName)
	}
}

func TestPooledGetTableConfigurationMapsErrorsAndInvalidatesCancellation(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	securityRPC := &fakeClientRPC{err: &clientgen.ThriftSecurityException{
		Code: clientgen.SecurityErrorCode_PERMISSION_DENIED,
	}}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{service: securityRPC}, nil
	}
	pooled.newServiceClient = serviceFromFakeTransport
	_, err := pooled.GetTableConfiguration(context.Background(), "tablet:9997", "events")
	var managerErr *Error
	if !errors.As(err, &managerErr) || managerErr.Kind != ErrorSecurity {
		t.Fatalf("security error = %#v, want ErrorSecurity", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	cancelRPC := &fakeClientRPC{call: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	var transport *fakeTransport
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		transport = &fakeTransport{service: cancelRPC}
		return transport, nil
	}
	pooled.newServiceClient = serviceFromFakeTransport

	// The security response left its healthy transport idle under the same key.
	// Use another address so cancellation exercises a newly leased transport.
	result := make(chan error, 1)
	go func() {
		_, err := pooled.GetTableConfiguration(ctx, "scan:9997", "events")
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
	if transport.closes.Load() != 1 {
		t.Fatalf("canceled transport closes = %d, want 1", transport.closes.Load())
	}
}

func TestPooledGetTableConfigurationValidationAndRetryClassification(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	if _, err := pooled.GetTableConfiguration(context.Background(), "tablet:9997", ""); err == nil {
		t.Fatal("expected empty table name error")
	}
	if IsRetryableEndpointError(&Error{Kind: ErrorSecurity}) {
		t.Fatal("application error must not be retried")
	}
	if IsRetryableEndpointError(context.Canceled) {
		t.Fatal("context cancellation must not be retried")
	}
	wireErr := thrift.NewTTransportExceptionFromError(errors.New("reset"))
	if !IsRetryableEndpointError(wireErr) {
		t.Fatal("transport error should be retried")
	}
}

func TestPooledSecuritySuccessPreservesPostResponseCleanupFailure(t *testing.T) {
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
	rpc := &recordingSecurityRPC{}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{service: rpc, closeErr: cleanupErr}, nil
	}
	pooled.newServiceClient = serviceFromFakeTransport

	err = pooled.ChangeLocalUserPassword(
		context.Background(),
		"server:9997",
		"root",
		[]byte("new-secret"),
	)
	var postResponseErr *PostResponseCleanupError
	if !errors.As(err, &postResponseErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %#v, want post-response cleanup error", err)
	}
	if IsRetryableEndpointError(err) {
		t.Fatal("post-response cleanup error must not be retried")
	}
	if !slices.Equal(rpc.operations, []string{"changeLocalUserPassword"}) {
		t.Fatalf("operations = %v", rpc.operations)
	}
}

func TestMapRPCErrorPreservesJoinedCleanupCause(t *testing.T) {
	cleanupErr := thrift.NewTTransportExceptionFromError(errors.New("close failed"))

	err := mapRPCError(errors.Join(
		&clientgen.ThriftTableOperationException{
			Type:      clientgen.TableOperationExceptionType_OFFLINE,
			TableName: "events",
		},
		cleanupErr,
	))

	var managerErr *Error
	if !errors.As(err, &managerErr) || managerErr.Kind != ErrorTableOffline {
		t.Fatalf("error = %#v, want ErrorTableOffline", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want cleanup cause preserved", err)
	}
}

func TestPooledSecurityRPCSelectionArgumentsAndCopies(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &recordingSecurityRPC{authResult: [][]byte{[]byte("server")}, boolResult: true}
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		if key.Service != clientServiceName {
			t.Fatalf("service = %q, want %q", key.Service, clientServiceName)
		}
		return &fakeTransport{service: rpc}, nil
	}
	pooled.newServiceClient = serviceFromFakeTransport
	ctx := context.Background()
	password := []byte("secret")
	auths := [][]byte{[]byte("alpha"), []byte("beta")}

	if err := pooled.CreateLocalUser(ctx, "server:9997", "alice", password); err != nil {
		t.Fatal(err)
	}
	if err := pooled.DropLocalUser(ctx, "server:9997", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := pooled.ChangeLocalUserPassword(ctx, "server:9997", "alice", password); err != nil {
		t.Fatal(err)
	}
	if err := pooled.ChangeUserAuthorizations(ctx, "server:9997", "alice", auths); err != nil {
		t.Fatal(err)
	}
	gotAuths, err := pooled.GetUserAuthorizations(ctx, "server:9997", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pooled.HasSystemPermission(ctx, "server:9997", "alice", 4); err != nil || !ok {
		t.Fatalf("has system = %v, %v", ok, err)
	}
	if ok, err := pooled.HasTablePermission(ctx, "server:9997", "alice", "events", 6); err != nil || !ok {
		t.Fatalf("has table = %v, %v", ok, err)
	}
	if ok, err := pooled.HasNamespacePermission(ctx, "server:9997", "alice", "analytics", 3); err != nil || !ok {
		t.Fatalf("has namespace = %v, %v", ok, err)
	}
	for _, call := range []func() error{
		func() error { return pooled.GrantSystemPermission(ctx, "server:9997", "alice", 1) },
		func() error { return pooled.RevokeSystemPermission(ctx, "server:9997", "alice", 2) },
		func() error { return pooled.GrantTablePermission(ctx, "server:9997", "alice", "events", 3) },
		func() error { return pooled.RevokeTablePermission(ctx, "server:9997", "alice", "events", 4) },
		func() error {
			return pooled.GrantNamespacePermission(ctx, "server:9997", "alice", "analytics", 5)
		},
		func() error {
			return pooled.RevokeNamespacePermission(ctx, "server:9997", "alice", "analytics", 6)
		},
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}

	wantOps := []string{
		"createLocalUser", "dropLocalUser", "changeLocalUserPassword",
		"changeAuthorizations", "getUserAuthorizations", "hasSystemPermission",
		"hasTablePermission", "hasNamespacePermission", "grantSystemPermission",
		"revokeSystemPermission", "grantTablePermission", "revokeTablePermission",
		"grantNamespacePermission", "revokeNamespacePermission",
	}
	if !slices.Equal(rpc.operations, wantOps) {
		t.Fatalf("operations = %v, want %v", rpc.operations, wantOps)
	}
	if rpc.principal != "alice" || rpc.tableName != "events" || rpc.namespace != "analytics" {
		t.Fatalf("arguments principal/table/namespace = %q/%q/%q",
			rpc.principal, rpc.tableName, rpc.namespace)
	}
	if !slices.Equal(rpc.password, []byte("secret")) ||
		len(rpc.authorizations) != 2 ||
		!slices.Equal(rpc.authorizations[0], []byte("alpha")) {
		t.Fatalf("password/auth arguments = %q/%q", rpc.password, rpc.authorizations)
	}
	password[0] = 'X'
	auths[0][0] = 'X'
	gotAuths[0][0] = 'X'
	if rpc.password[0] != 's' || rpc.authorizations[0][0] != 'a' || rpc.authResult[0][0] != 's' {
		t.Fatal("caller mutation leaked into RPC-owned values")
	}
}

func TestPooledUpdateCredentialsCopiesReplacementForNextRPCAndRejectsClosed(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &recordingSecurityRPC{}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeTransport{service: rpc}, nil
	}
	pooled.newServiceClient = serviceFromFakeTransport
	replacement := &security.TCredentials{
		Principal:      "root",
		TokenClassName: "PasswordToken",
		Token:          []byte("replacement"),
		InstanceId:     "uuid-1",
	}
	if err := pooled.UpdateCredentials(replacement); err != nil {
		t.Fatal(err)
	}
	replacement.Token[0] = 'X'
	if err := pooled.DropLocalUser(context.Background(), "server:9997", "alice"); err != nil {
		t.Fatal(err)
	}
	if got := rpc.credentialToken(); string(got) != "replacement" || string(got) == "secret" {
		t.Fatalf("RPC token = %q, want isolated replacement", got)
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pooled.UpdateCredentials(&security.TCredentials{Token: []byte("later")}); err == nil ||
		err.Error() != "managerclient: pooled client is closed" {
		t.Fatalf("closed update error = %v", err)
	}
}

func TestThriftSecurityRPCMethodNamesAndGeneratedArgumentOrder(t *testing.T) {
	capture := &captureThriftClient{}
	rpc := thriftClientRPC{raw: clientgen.NewClientServiceClient(capture)}
	ctx := context.Background()
	credentials := &security.TCredentials{Principal: "root"}

	calls := []func() error{
		func() error { return rpc.CreateLocalUser(ctx, credentials, "alice", []byte("pw")) },
		func() error { return rpc.DropLocalUser(ctx, credentials, "alice") },
		func() error { return rpc.ChangeLocalUserPassword(ctx, credentials, "alice", []byte("new")) },
		func() error {
			return rpc.ChangeUserAuthorizations(ctx, credentials, "alice", [][]byte{[]byte("a")})
		},
		func() error {
			_, err := rpc.GetUserAuthorizations(ctx, credentials, "alice")
			return err
		},
		func() error {
			_, err := rpc.HasSystemPermission(ctx, credentials, "alice", 4)
			return err
		},
		func() error {
			_, err := rpc.HasTablePermission(ctx, credentials, "alice", "events", 6)
			return err
		},
		func() error {
			_, err := rpc.HasNamespacePermission(ctx, credentials, "alice", "analytics", 3)
			return err
		},
		func() error { return rpc.GrantSystemPermission(ctx, credentials, "alice", 1) },
		func() error { return rpc.RevokeSystemPermission(ctx, credentials, "alice", 2) },
		func() error { return rpc.GrantTablePermission(ctx, credentials, "alice", "events", 3) },
		func() error { return rpc.RevokeTablePermission(ctx, credentials, "alice", "events", 4) },
		func() error {
			return rpc.GrantNamespacePermission(ctx, credentials, "alice", "analytics", 5)
		},
		func() error {
			return rpc.RevokeNamespacePermission(ctx, credentials, "alice", "analytics", 6)
		},
	}
	for _, call := range calls {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	wantMethods := []string{
		"createLocalUser", "dropLocalUser", "changeLocalUserPassword",
		"changeAuthorizations", "getUserAuthorizations", "hasSystemPermission",
		"hasTablePermission", "hasNamespacePermission", "grantSystemPermission",
		"revokeSystemPermission", "grantTablePermission", "revokeTablePermission",
		"grantNamespacePermission", "revokeNamespacePermission",
	}
	if !slices.Equal(capture.methods, wantMethods) {
		t.Fatalf("methods = %v, want %v", capture.methods, wantMethods)
	}
	create := capture.args[0].(*clientgen.ClientServiceCreateLocalUserArgs)
	if create.Tinfo == nil || create.Credentials != credentials ||
		create.Principal != "alice" || !slices.Equal(create.Password, []byte("pw")) {
		t.Fatalf("create args = %#v", create)
	}
	changeAuths := capture.args[3].(*clientgen.ClientServiceChangeAuthorizationsArgs)
	if changeAuths.Principal != "alice" || len(changeAuths.Authorizations) != 1 ||
		!slices.Equal(changeAuths.Authorizations[0], []byte("a")) {
		t.Fatalf("change auth args = %#v", changeAuths)
	}
	hasTable := capture.args[6].(*clientgen.ClientServiceHasTablePermissionArgs)
	if hasTable.Principal != "alice" || hasTable.TableName != "events" || hasTable.TblPerm != 6 {
		t.Fatalf("has table args = %#v", hasTable)
	}
	grantNamespace := capture.args[12].(*clientgen.ClientServiceGrantNamespacePermissionArgs)
	if grantNamespace.Principal != "alice" || grantNamespace.Ns != "analytics" ||
		grantNamespace.Permission != 5 {
		t.Fatalf("grant namespace args = %#v", grantNamespace)
	}
}

func newTestPooled(t *testing.T) (*Pooled, *transportpool.Pool) {
	t.Helper()
	pool, err := transportpool.New(transportpool.Config{MaxIdlePerEndpoint: 1})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := NewPooled(pool, "uuid-1", "4.0.0-SNAPSHOT", &security.TCredentials{
		Principal:      "root",
		TokenClassName: "PasswordToken",
		Token:          []byte("secret"),
		InstanceId:     "uuid-1",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return pooled, pool
}

func createRequest(name string) Request {
	return Request{
		Operation: TableCreate,
		Instance:  FateUser,
		Arguments: [][]byte{[]byte(name), []byte("MILLIS"), []byte("ONLINE"), []byte("HOSTED"), []byte("0")},
		Options:   map[string]string{},
	}
}

type fakeTransport struct {
	rpc      fateRPC
	manager  managerRPC
	service  clientRPC
	closes   atomic.Int32
	closeErr error
}

func (t *fakeTransport) Close() error {
	t.closes.Add(1)
	return t.closeErr
}

func TestNamespaceFATEOperationEncoding(t *testing.T) {
	tests := []struct {
		operation Operation
		want      manager.TFateOperation
		args      [][]byte
	}{
		{NamespaceCreate, manager.TFateOperation_NAMESPACE_CREATE, [][]byte{[]byte("analytics")}},
		{NamespaceDelete, manager.TFateOperation_NAMESPACE_DELETE, [][]byte{[]byte("")}},
		{NamespaceRename, manager.TFateOperation_NAMESPACE_RENAME, [][]byte{[]byte("analytics"), []byte("reporting")}},
	}
	for _, tt := range tests {
		request := Request{
			Operation: tt.operation,
			Instance:  FateUser,
			Arguments: tt.args,
			Options:   map[string]string{},
		}
		if err := validateRequest(request); err != nil {
			t.Fatalf("%v validation: %v", tt.operation, err)
		}
		if got := thriftOperation(tt.operation); got != tt.want {
			t.Fatalf("%v thrift operation = %v, want %v", tt.operation, got, tt.want)
		}
	}
}

func TestPooledNamespacePropertyRPCsAndReads(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	managerRPC := &fakeManagerRPC{}
	clientRPC := &fakeClientRPC{
		properties: map[string]string{"table.file.max": "15"},
		versioned: VersionedProperties{
			Version:    9,
			Properties: map[string]string{"table.file.max": "15"},
		},
	}
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		switch key.Service {
		case managerServiceName:
			return &fakeTransport{manager: managerRPC}, nil
		case clientServiceName:
			return &fakeTransport{service: clientRPC}, nil
		default:
			t.Fatalf("service = %q", key.Service)
			return nil, nil
		}
	}
	pooled.newManagerClient = managerFromFakeTransport
	pooled.newServiceClient = serviceFromFakeTransport

	if err := pooled.SetNamespaceProperty(
		context.Background(),
		"manager:9997",
		"",
		"table.file.max",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := pooled.RemoveNamespaceProperty(
		context.Background(),
		"manager:9997",
		"analytics",
		"table.file.max",
	); err != nil {
		t.Fatal(err)
	}
	if managerRPC.setCalls.Load() != 1 || managerRPC.removeCalls.Load() != 1 {
		t.Fatalf("set/remove calls = %d/%d", managerRPC.setCalls.Load(), managerRPC.removeCalls.Load())
	}

	effective, err := pooled.GetNamespaceConfiguration(context.Background(), "scan:9997", "")
	if err != nil {
		t.Fatal(err)
	}
	local, err := pooled.GetNamespaceProperties(context.Background(), "scan:9997", "analytics")
	if err != nil {
		t.Fatal(err)
	}
	versioned, err := pooled.GetVersionedNamespaceProperties(
		context.Background(),
		"scan:9997",
		"analytics",
	)
	if err != nil {
		t.Fatal(err)
	}
	effective["table.file.max"] = "effective mutation"
	local["table.file.max"] = "local mutation"
	versioned.Properties["table.file.max"] = "versioned mutation"
	if clientRPC.properties["table.file.max"] != "15" ||
		clientRPC.versioned.Properties["table.file.max"] != "15" ||
		versioned.Version != 9 {
		t.Fatalf("namespace property copies/version = %#v/%#v", clientRPC, versioned)
	}
}

func clientFromFakeTransport(transport io.Closer) (fateRPC, error) {
	return transport.(*fakeTransport).rpc, nil
}

func managerFromFakeTransport(transport io.Closer) (managerRPC, error) {
	return transport.(*fakeTransport).manager, nil
}

func serviceFromFakeTransport(transport io.Closer) (clientRPC, error) {
	return transport.(*fakeTransport).service, nil
}

type fakeClientRPC struct {
	noopClientRPC
	properties map[string]string
	versioned  VersionedProperties
	err        error
	call       func(context.Context) error
	tableName  string
	calls      atomic.Int32
}

type captureThriftClient struct {
	methods []string
	args    []thrift.TStruct
}

func (c *captureThriftClient) Call(
	_ context.Context,
	method string,
	args, _ thrift.TStruct,
) (thrift.ResponseMeta, error) {
	c.methods = append(c.methods, method)
	c.args = append(c.args, args)
	return thrift.ResponseMeta{}, nil
}

type noopClientRPC struct{}

func (noopClientRPC) GetTableConfiguration(
	context.Context, *security.TCredentials, string,
) (map[string]string, error) {
	return nil, nil
}
func (noopClientRPC) GetNamespaceConfiguration(
	context.Context, *security.TCredentials, string,
) (map[string]string, error) {
	return nil, nil
}
func (noopClientRPC) GetNamespaceProperties(
	context.Context, *security.TCredentials, string,
) (map[string]string, error) {
	return nil, nil
}
func (noopClientRPC) GetVersionedNamespaceProperties(
	context.Context, *security.TCredentials, string,
) (VersionedProperties, error) {
	return VersionedProperties{}, nil
}
func (noopClientRPC) GetVersionedTableProperties(
	context.Context, *security.TCredentials, string,
) (VersionedProperties, error) {
	return VersionedProperties{}, nil
}
func (noopClientRPC) CreateLocalUser(
	context.Context, *security.TCredentials, string, []byte,
) error {
	return nil
}
func (noopClientRPC) DropLocalUser(context.Context, *security.TCredentials, string) error {
	return nil
}
func (noopClientRPC) ChangeLocalUserPassword(
	context.Context, *security.TCredentials, string, []byte,
) error {
	return nil
}
func (noopClientRPC) ChangeUserAuthorizations(
	context.Context, *security.TCredentials, string, [][]byte,
) error {
	return nil
}
func (noopClientRPC) GetUserAuthorizations(
	context.Context, *security.TCredentials, string,
) ([][]byte, error) {
	return nil, nil
}
func (noopClientRPC) HasSystemPermission(
	context.Context, *security.TCredentials, string, int8,
) (bool, error) {
	return false, nil
}
func (noopClientRPC) HasTablePermission(
	context.Context, *security.TCredentials, string, string, int8,
) (bool, error) {
	return false, nil
}
func (noopClientRPC) HasNamespacePermission(
	context.Context, *security.TCredentials, string, string, int8,
) (bool, error) {
	return false, nil
}
func (noopClientRPC) GrantSystemPermission(
	context.Context, *security.TCredentials, string, int8,
) error {
	return nil
}
func (noopClientRPC) RevokeSystemPermission(
	context.Context, *security.TCredentials, string, int8,
) error {
	return nil
}
func (noopClientRPC) GrantTablePermission(
	context.Context, *security.TCredentials, string, string, int8,
) error {
	return nil
}
func (noopClientRPC) RevokeTablePermission(
	context.Context, *security.TCredentials, string, string, int8,
) error {
	return nil
}
func (noopClientRPC) GrantNamespacePermission(
	context.Context, *security.TCredentials, string, string, int8,
) error {
	return nil
}
func (noopClientRPC) RevokeNamespacePermission(
	context.Context, *security.TCredentials, string, string, int8,
) error {
	return nil
}

type recordingSecurityRPC struct {
	noopClientRPC
	operations     []string
	principal      string
	tableName      string
	namespace      string
	permission     int8
	password       []byte
	authorizations [][]byte
	authResult     [][]byte
	boolResult     bool
	credentials    *security.TCredentials
}

func (r *recordingSecurityRPC) record(operation, principal string) {
	r.operations = append(r.operations, operation)
	r.principal = principal
}

func (r *recordingSecurityRPC) CreateLocalUser(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
	password []byte,
) error {
	r.record("createLocalUser", principal)
	r.password = append([]byte(nil), password...)
	return nil
}

func (r *recordingSecurityRPC) DropLocalUser(
	_ context.Context,
	credentials *security.TCredentials,
	principal string,
) error {
	r.record("dropLocalUser", principal)
	r.credentials = cloneCredentials(credentials)
	return nil
}

func (r *recordingSecurityRPC) credentialToken() []byte {
	if r.credentials == nil {
		return nil
	}
	return append([]byte(nil), r.credentials.Token...)
}

func (r *recordingSecurityRPC) ChangeLocalUserPassword(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
	password []byte,
) error {
	r.record("changeLocalUserPassword", principal)
	r.password = append([]byte(nil), password...)
	return nil
}

func (r *recordingSecurityRPC) ChangeUserAuthorizations(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
	authorizations [][]byte,
) error {
	r.record("changeAuthorizations", principal)
	r.authorizations = cloneArguments(authorizations)
	return nil
}

func (r *recordingSecurityRPC) GetUserAuthorizations(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
) ([][]byte, error) {
	r.record("getUserAuthorizations", principal)
	return r.authResult, nil
}

func (r *recordingSecurityRPC) HasSystemPermission(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
	permission int8,
) (bool, error) {
	r.record("hasSystemPermission", principal)
	r.permission = permission
	return r.boolResult, nil
}

func (r *recordingSecurityRPC) HasTablePermission(
	_ context.Context,
	_ *security.TCredentials,
	principal, tableName string,
	permission int8,
) (bool, error) {
	r.record("hasTablePermission", principal)
	r.tableName, r.permission = tableName, permission
	return r.boolResult, nil
}

func (r *recordingSecurityRPC) HasNamespacePermission(
	_ context.Context,
	_ *security.TCredentials,
	principal, namespace string,
	permission int8,
) (bool, error) {
	r.record("hasNamespacePermission", principal)
	r.namespace, r.permission = namespace, permission
	return r.boolResult, nil
}

func (r *recordingSecurityRPC) GrantSystemPermission(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
	permission int8,
) error {
	r.record("grantSystemPermission", principal)
	r.permission = permission
	return nil
}

func (r *recordingSecurityRPC) RevokeSystemPermission(
	_ context.Context,
	_ *security.TCredentials,
	principal string,
	permission int8,
) error {
	r.record("revokeSystemPermission", principal)
	r.permission = permission
	return nil
}

func (r *recordingSecurityRPC) GrantTablePermission(
	_ context.Context,
	_ *security.TCredentials,
	principal, tableName string,
	permission int8,
) error {
	r.record("grantTablePermission", principal)
	r.tableName, r.permission = tableName, permission
	return nil
}

func (r *recordingSecurityRPC) RevokeTablePermission(
	_ context.Context,
	_ *security.TCredentials,
	principal, tableName string,
	permission int8,
) error {
	r.record("revokeTablePermission", principal)
	r.tableName, r.permission = tableName, permission
	return nil
}

func (r *recordingSecurityRPC) GrantNamespacePermission(
	_ context.Context,
	_ *security.TCredentials,
	principal, namespace string,
	permission int8,
) error {
	r.record("grantNamespacePermission", principal)
	r.namespace, r.permission = namespace, permission
	return nil
}

func (r *recordingSecurityRPC) RevokeNamespacePermission(
	_ context.Context,
	_ *security.TCredentials,
	principal, namespace string,
	permission int8,
) error {
	r.record("revokeNamespacePermission", principal)
	r.namespace, r.permission = namespace, permission
	return nil
}

func (r *fakeClientRPC) GetTableConfiguration(
	ctx context.Context,
	_ *security.TCredentials,
	tableName string,
) (map[string]string, error) {
	r.calls.Add(1)
	r.tableName = tableName
	if r.call != nil {
		if err := r.call(ctx); err != nil {
			return nil, err
		}
	}
	return r.properties, r.err
}

func (r *fakeClientRPC) GetNamespaceConfiguration(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace string,
) (map[string]string, error) {
	return r.GetTableConfiguration(ctx, credentials, namespace)
}

func (r *fakeClientRPC) GetNamespaceProperties(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace string,
) (map[string]string, error) {
	return r.GetTableConfiguration(ctx, credentials, namespace)
}

func (r *fakeClientRPC) GetVersionedTableProperties(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName string,
) (VersionedProperties, error) {
	return r.GetVersionedNamespaceProperties(ctx, credentials, tableName)
}

func (r *fakeClientRPC) GetVersionedNamespaceProperties(
	ctx context.Context,
	_ *security.TCredentials,
	namespace string,
) (VersionedProperties, error) {
	r.calls.Add(1)
	r.tableName = namespace
	if r.call != nil {
		if err := r.call(ctx); err != nil {
			return VersionedProperties{}, err
		}
	}
	return r.versioned, r.err
}

type fakeManagerRPC struct {
	initiateErr        error
	waitFlushErr       error
	setErr             error
	removeErr          error
	waitFlush          func(context.Context) error
	tableName          string
	property           string
	value              string
	flushTableID       string
	modifiedTable      string
	modifiedProperties VersionedProperties
	modifyErr          error
	flushStartRow      []byte
	flushEndRow        []byte
	flushID            int64
	waitFlushID        int64
	maxLoops           []int64
	initiateCalls      atomic.Int32
	flushWaitCalls     atomic.Int32
	setCalls           atomic.Int32
	removeCalls        atomic.Int32
}

func (r *fakeManagerRPC) InitiateFlush(
	_ context.Context,
	_ *security.TCredentials,
	tableID string,
) (int64, error) {
	r.initiateCalls.Add(1)
	r.flushTableID = tableID
	return r.flushID, r.initiateErr
}

func (r *fakeManagerRPC) WaitForFlush(
	ctx context.Context,
	_ *security.TCredentials,
	tableID string,
	startRow, endRow []byte,
	flushID, maxLoops int64,
) error {
	r.flushWaitCalls.Add(1)
	r.flushTableID = tableID
	r.flushStartRow = append([]byte(nil), startRow...)
	r.flushEndRow = append([]byte(nil), endRow...)
	r.waitFlushID = flushID
	r.maxLoops = append(r.maxLoops, maxLoops)
	if r.waitFlush != nil {
		return r.waitFlush(ctx)
	}
	return r.waitFlushErr
}

func (r *fakeManagerRPC) ModifyTableProperties(
	_ context.Context,
	_ *security.TCredentials,
	tableName string,
	properties VersionedProperties,
) error {
	r.modifiedTable = tableName
	r.modifiedProperties = properties
	return r.modifyErr
}

func (r *fakeManagerRPC) SetTableProperty(
	_ context.Context,
	_ *security.TCredentials,
	tableName, property, value string,
) error {
	r.setCalls.Add(1)
	r.tableName = tableName
	r.property = property
	r.value = value
	return r.setErr
}

func (r *fakeManagerRPC) RemoveTableProperty(
	_ context.Context,
	_ *security.TCredentials,
	tableName, property string,
) error {
	r.removeCalls.Add(1)
	r.tableName = tableName
	r.property = property
	return r.removeErr
}

func (r *fakeManagerRPC) SetNamespaceProperty(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace, property, value string,
) error {
	return r.SetTableProperty(ctx, credentials, namespace, property, value)
}

func (r *fakeManagerRPC) RemoveNamespaceProperty(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace, property string,
) error {
	return r.RemoveTableProperty(ctx, credentials, namespace, property)
}

func thriftFieldIDs(t *testing.T, value thrift.TStruct) []int16 {
	t.Helper()
	ctx := context.Background()
	buffer := thrift.NewTMemoryBuffer()
	writer := thrift.NewTCompactProtocol(buffer)
	if err := value.Write(ctx, writer); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	reader := thrift.NewTCompactProtocol(buffer)
	if _, err := reader.ReadStructBegin(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []int16
	for {
		_, fieldType, fieldID, err := reader.ReadFieldBegin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if fieldType == thrift.STOP {
			break
		}
		ids = append(ids, fieldID)
		if err := reader.Skip(ctx, fieldType); err != nil {
			t.Fatal(err)
		}
		if err := reader.ReadFieldEnd(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := reader.ReadStructEnd(ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}

type fakeFateRPC struct {
	id         fateID
	beginErr   error
	executeErr error
	waitStatus string
	wait       func() error
	finishErr  error
	finishFunc func(context.Context) error
	request    Request
	instance   FateInstance
	beginHook  func()
	finishHook func()

	begin            atomic.Int32
	execute          atomic.Int32
	waitCalls        atomic.Int32
	finish           atomic.Int32
	finishContextErr error
}

func (r *fakeFateRPC) Begin(
	_ context.Context,
	_ *security.TCredentials,
	instance FateInstance,
) (fateID, error) {
	r.begin.Add(1)
	r.instance = instance
	if r.beginHook != nil {
		r.beginHook()
	}
	return r.id, r.beginErr
}

func (r *fakeFateRPC) Execute(_ context.Context, _ *security.TCredentials, _ fateID, req Request) error {
	r.execute.Add(1)
	r.request = req
	return r.executeErr
}

func (r *fakeFateRPC) Wait(context.Context, *security.TCredentials, fateID) (string, error) {
	r.waitCalls.Add(1)
	if r.wait != nil {
		return "", r.wait()
	}
	return r.waitStatus, nil
}

func (r *fakeFateRPC) Finish(ctx context.Context, _ *security.TCredentials, _ fateID) error {
	r.finish.Add(1)
	r.finishContextErr = ctx.Err()
	if r.finishHook != nil {
		r.finishHook()
	}
	if r.finishFunc != nil {
		return r.finishFunc(ctx)
	}
	return r.finishErr
}
