package accumulo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

type fakeScannerAdapter struct {
	mu sync.Mutex

	startResults []*data.InitialScan
	startErrors  []error
	continues    []*data.ScanResult_
	continueErr  error
	closeErr     error
	closeErrors  []error

	continueEntered chan<- struct{}
	continueRelease <-chan struct{}
	// continueErrWithResult returns the queued batch alongside continueErr,
	// which is what the pooled client does when the RPC succeeds but releasing
	// its transport lease fails.
	continueErrWithResult      bool
	multiContinueErrWithResult bool

	multiStartResults []*data.InitialMultiScan
	multiStartErrors  []error
	multiContinues    []*data.MultiScanResult_
	multiContinueErr  error
	multiCloseErr     error

	addresses     []string
	startRequests []scanclient.StartRequest
	startCalls    int
	continueCalls int
	closeCalls    int
	onFirstStart  func()
	closeContext  error
	startFunc     func(string, scanclient.StartRequest) (*data.InitialScan, error)
	startEntered  chan<- struct{}
	startRelease  <-chan struct{}
	activeStarts  int
	maxStarts     int

	multiAddresses     []string
	multiRequests      []scanclient.MultiStartRequest
	multiStartCalls    int
	multiContinueCalls int
	multiCloseCalls    int
	multiStartFunc     func(string, scanclient.MultiStartRequest) (*data.InitialMultiScan, error)
}

func (f *fakeScannerAdapter) Start(
	_ context.Context,
	address string,
	req scanclient.StartRequest,
) (*data.InitialScan, error) {
	f.mu.Lock()
	f.addresses = append(f.addresses, address)
	f.startRequests = append(f.startRequests, req)
	index := f.startCalls
	f.startCalls++
	onFirstStart := f.onFirstStart
	startFunc := f.startFunc
	startEntered := f.startEntered
	startRelease := f.startRelease
	var result *data.InitialScan
	if index < len(f.startResults) {
		result = f.startResults[index]
	}
	var err error
	if index < len(f.startErrors) {
		err = f.startErrors[index]
	}
	f.activeStarts++
	if f.activeStarts > f.maxStarts {
		f.maxStarts = f.activeStarts
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.activeStarts--
		f.mu.Unlock()
	}()

	if index == 0 && onFirstStart != nil {
		onFirstStart()
	}
	if startEntered != nil {
		startEntered <- struct{}{}
	}
	if startRelease != nil {
		<-startRelease
	}
	if startFunc != nil {
		return startFunc(address, req)
	}
	return result, err
}

func (f *fakeScannerAdapter) Continue(
	ctx context.Context,
	_ string,
	_ data.ScanID,
	_ int64,
) (*data.ScanResult_, error) {
	f.mu.Lock()
	index := f.continueCalls
	f.continueCalls++
	continueEntered := f.continueEntered
	continueRelease := f.continueRelease
	continueErr := f.continueErr
	withResult := f.continueErrWithResult
	var result *data.ScanResult_
	if index < len(f.continues) {
		result = f.continues[index]
	} else {
		result = &data.ScanResult_{}
	}
	f.mu.Unlock()

	if continueEntered != nil {
		continueEntered <- struct{}{}
	}
	if continueRelease != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-continueRelease:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if continueErr != nil {
		if withResult {
			return result, continueErr
		}
		return nil, continueErr
	}
	return result, nil
}

func (f *fakeScannerAdapter) CloseScan(ctx context.Context, _ string, _ data.ScanID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.closeCalls
	f.closeCalls++
	f.closeContext = ctx.Err()
	if index < len(f.closeErrors) {
		return f.closeErrors[index]
	}
	return f.closeErr
}

func (f *fakeScannerAdapter) StartMulti(
	_ context.Context,
	address string,
	req scanclient.MultiStartRequest,
) (*data.InitialMultiScan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.multiAddresses = append(f.multiAddresses, address)
	f.multiRequests = append(f.multiRequests, req)
	index := f.multiStartCalls
	f.multiStartCalls++
	if f.multiStartFunc != nil {
		return f.multiStartFunc(address, req)
	}
	var result *data.InitialMultiScan
	if index < len(f.multiStartResults) {
		result = f.multiStartResults[index]
	}
	var err error
	if index < len(f.multiStartErrors) {
		err = f.multiStartErrors[index]
	}
	return result, err
}

func (f *fakeScannerAdapter) ContinueMulti(
	_ context.Context,
	_ string,
	_ data.ScanID,
	_ int64,
) (*data.MultiScanResult_, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.multiContinueCalls
	f.multiContinueCalls++
	var result *data.MultiScanResult_
	if index < len(f.multiContinues) {
		result = f.multiContinues[index]
	} else {
		result = &data.MultiScanResult_{}
	}
	if f.multiContinueErr != nil {
		if f.multiContinueErrWithResult {
			return result, f.multiContinueErr
		}
		return nil, f.multiContinueErr
	}
	return result, nil
}

func (f *fakeScannerAdapter) CloseMultiScan(
	_ context.Context,
	_ string,
	_ data.ScanID,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.multiCloseCalls++
	return f.multiCloseErr
}

func (f *fakeScannerAdapter) Close() error { return nil }

func TestRangeWireBoundaries(t *testing.T) {
	scanRange, err := NewRange([]byte("a"), false, []byte("z"), true)
	if err != nil {
		t.Fatal(err)
	}
	wire := scanRange.toThrift()
	if !bytes.Equal(wire.Start.Row, []byte("a")) || wire.StartKeyInclusive {
		t.Fatalf("start = %+v inclusive=%v", wire.Start, wire.StartKeyInclusive)
	}
	if !bytes.Equal(wire.Stop.Row, []byte{'z', 0}) || wire.StopKeyInclusive {
		t.Fatalf("stop = %+v inclusive=%v", wire.Stop, wire.StopKeyInclusive)
	}
	if got := scanRange.routingRow(); !bytes.Equal(got, []byte{'a', 0}) {
		t.Fatalf("routing row = %q", got)
	}
	firstTablet := Tablet{Extent: TabletExtent{EndRow: []byte("k")}}
	exclusiveFollowingRow, _ := NewRange([]byte("a"), true, []byte{'k', 0}, false)
	if !exclusiveFollowingRow.fitsTablet(firstTablet) {
		t.Fatal("exclusive following-row stop should fit the preceding tablet")
	}
	inclusiveFollowingRow, _ := NewRange([]byte("a"), true, []byte{'k', 0}, true)
	if inclusiveFollowingRow.fitsTablet(firstTablet) {
		t.Fatal("inclusive following-row stop should span into the next tablet")
	}
}

func TestScannerContinuationAndCleanup(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID: 7,
			Result_: &data.ScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
				More:    true,
			},
		}},
		continues: []*data.ScanResult_{{
			Results: []*data.TKeyValue{testEntry("b", "two")},
		}},
	}
	connector.scan = adapter
	family := []byte("content")
	qualifier := []byte("body")
	columns := []Column{
		NewColumnFamily(family),
		NewColumn(family, qualifier),
	}
	iteratorOptions := map[string]string{"maxVersions": "3"}
	iterator, err := NewIteratorSetting(
		"versioning",
		"org.apache.accumulo.core.iterators.user.VersioningIterator",
		20,
		iteratorOptions,
	)
	if err != nil {
		t.Fatal(err)
	}
	iterators := []IteratorSetting{iterator}
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{
		BatchSize:      2,
		Authorizations: [][]byte{[]byte("public")},
		Columns:        columns,
		Iterators:      iterators,
	})
	if err != nil {
		t.Fatal(err)
	}
	family[0] = 'X'
	qualifier[0] = 'X'
	columns[0].family[0] = 'X'
	columns[1].qualifier[0] = 'X'
	iteratorOptions["maxVersions"] = "99"
	iterators[0].options["maxVersions"] = "100"
	scanRange, _ := NewRange([]byte("a"), true, []byte("k"), true)
	values, err := scanner.Scan(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || string(values[0].Value) != "one" || string(values[1].Key.Row) != "b" {
		t.Fatalf("values = %+v", values)
	}
	if adapter.startCalls != 1 || adapter.continueCalls != 1 || adapter.closeCalls != 1 {
		t.Fatalf("calls start/continue/close = %d/%d/%d", adapter.startCalls, adapter.continueCalls, adapter.closeCalls)
	}
	req := adapter.startRequests[0]
	if req.BatchSize != 2 || len(req.Authorizations) != 1 || string(req.Extent.Table) != "1" {
		t.Fatalf("start request = %+v", req)
	}
	assertColumns(t, req.Columns, "content", "body")
	assertIterators(t, req.Iterators, req.IteratorOptions)
	values[0].Key.Row[0] = 'z'
	if string(adapter.startResults[0].Result_.Results[0].Key.Row) != "a" {
		t.Fatal("public key mutation leaked into wire result")
	}
}

func TestScannerRelocatesUntilNotServingClears(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
		"1": {{TableID: "1", EndRow: []byte("k"), Location: &metadata.Location{HostPort: "old:9997"}}},
	}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{nil, nil, nil, {
			ScanID:  8,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "ok")}},
		}},
		startErrors: []error{
			tabletserver.NewNotServingTabletException(),
			tabletserver.NewNotServingTabletException(),
			tabletserver.NewNotServingTabletException(),
			nil,
		},
	}
	adapter.onFirstStart = func() {
		walker.mu.Lock()
		walker.tablets["1"][0].Location = &metadata.Location{HostPort: "new:9997"}
		walker.mu.Unlock()
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRangeRow([]byte("a"))
	values, err := scanner.Scan(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || adapter.startCalls != 4 {
		t.Fatalf("values=%+v startCalls=%d", values, adapter.startCalls)
	}
	if got := adapter.addresses; len(got) != 4 || got[0] != "old:9997" || got[3] != "new:9997" {
		t.Fatalf("addresses = %v", got)
	}
}

func TestScannerDoesNotDuplicateCleanupErrorWhenInvalidationFails(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	closeErr := errors.New("close failed")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  12,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "ok")}},
		}},
		startErrors: []error{tabletserver.NewNotServingTabletException()},
		closeErr:    closeErr,
	}
	adapter.onFirstStart = func() {
		connector.discovery = nil
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRangeRow([]byte("a"))
	values, err := scanner.Scan(context.Background(), scanRange)
	if len(values) != 1 || !errors.Is(err, closeErr) || !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("values=%+v error=%v", values, err)
	}
	if got := strings.Count(err.Error(), closeErr.Error()); got != 1 {
		t.Fatalf("cleanup error occurrences = %d, want 1: %v", got, err)
	}
}

func TestScannerRangeAndCleanupErrors(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	closeErr := errors.New("close failed")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  9,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "ok")}},
		}},
		closeErr: closeErr,
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wide, _ := NewRange([]byte("a"), true, []byte("z"), true)
	if _, err := scanner.Scan(context.Background(), wide); !errors.Is(err, ErrRangeSpansTablets) {
		t.Fatalf("wide range error = %v", err)
	}
	oneRow, _ := NewRangeRow([]byte("a"))
	values, err := scanner.Scan(context.Background(), oneRow)
	var cleanupErr *CleanupError
	if len(values) != 1 || !errors.As(err, &cleanupErr) || !errors.Is(err, closeErr) {
		t.Fatalf("values=%+v error=%v", values, err)
	}
}

func TestScannerCancellationStillClosesServerScan(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  10,
			Result_: &data.ScanResult_{More: true},
		}},
		onFirstStart: cancel,
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRangeRow([]byte("a"))
	if _, err := scanner.Scan(ctx, scanRange); !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", err)
	}
	if adapter.continueCalls != 1 || adapter.closeCalls != 1 {
		t.Fatalf("continue/close calls = %d/%d, want 1/1", adapter.continueCalls, adapter.closeCalls)
	}
	if adapter.closeContext != nil {
		t.Fatalf("close context error = %v, want active cleanup context", adapter.closeContext)
	}
}

func TestScannerPreservesInitialBatchWithLeaseCleanupError(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	leaseErr := errors.New("release transport")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  11,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "ok")}},
		}},
		startErrors: []error{leaseErr},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRangeRow([]byte("a"))
	values, err := scanner.Scan(context.Background(), scanRange)
	if len(values) != 1 || !errors.Is(err, leaseErr) {
		t.Fatalf("values=%+v error=%v", values, err)
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", adapter.closeCalls)
	}
}

func TestBatchScannerSplitsRangeAcrossTablets(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 20, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			{ScanID: 21, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("m", "two")}}},
			{ScanID: 22, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("z", "three")}}},
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("z"), true)
	values, err := scanner.Scan(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 {
		t.Fatalf("values = %+v", values)
	}
	if got := adapter.addresses; len(got) != 3 ||
		got[0] != "ts1:9997" || got[1] != "ts2:9997" || got[2] != "ts3:9997" {
		t.Fatalf("addresses = %v", got)
	}
	assertWireRange(t, adapter.startRequests[0].Range, []byte("a"), true, []byte{'k', 0})
	assertWireRange(t, adapter.startRequests[1].Range, []byte("k"), false, []byte{'p', 0})
	assertWireRange(t, adapter.startRequests[2].Range, []byte("p"), false, []byte{'z', 0})
}

func TestBatchScannerContinuesAfterCleanupError(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	closeErr := errors.New("close first tablet")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 23, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			{ScanID: 24, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("n", "two")}}},
		},
		closeErrors: []error{closeErr, nil},
	}
	connector.scan = adapter
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("n"), true)
	values, err := scanner.Scan(context.Background(), []*Range{scanRange})
	if len(values) != 2 || adapter.startCalls != 2 || !errors.Is(err, closeErr) {
		t.Fatalf("values=%+v startCalls=%d error=%v", values, adapter.startCalls, err)
	}
}

func TestBatchScannerSupportsUnboundedAndMultipleRanges(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 25, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			{ScanID: 26, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("m", "two")}}},
			{ScanID: 27, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("z", "three")}}},
			{ScanID: 28, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("b", "four")}}},
			{ScanID: 29, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("n", "five")}}},
			{ScanID: 30, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("q", "six")}}},
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oneRow, _ := NewRangeRow([]byte("b"))
	spanning, _ := NewRange([]byte("n"), true, []byte("q"), true)
	values, err := scanner.Scan(context.Background(), []*Range{InfiniteRange(), oneRow, spanning})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 6 || adapter.startCalls != 6 {
		t.Fatalf("values=%+v startCalls=%d", values, adapter.startCalls)
	}
	if !adapter.startRequests[0].Range.InfiniteStartKey {
		t.Fatal("first infinite-range segment should have an unbounded start")
	}
	if !adapter.startRequests[2].Range.InfiniteStopKey {
		t.Fatal("last infinite-range segment should have an unbounded stop")
	}
	if got := adapter.addresses; len(got) != 6 ||
		got[3] != "ts1:9997" || got[4] != "ts2:9997" || got[5] != "ts3:9997" {
		t.Fatalf("addresses = %v", got)
	}
}

func TestBatchScannerBoundsParallelismAndPreservesOrder(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	adapter := &fakeScannerAdapter{
		startEntered: entered,
		startRelease: release,
		startFunc: func(address string, _ scanclient.StartRequest) (*data.InitialScan, error) {
			rows := map[string]string{
				"ts1:9997": "a",
				"ts2:9997": "m",
				"ts3:9997": "z",
			}
			return &data.InitialScan{
				ScanID:  31,
				Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry(rows[address], address)}},
			}, nil
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{
		Parallelism: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("z"), true)
	type scanResult struct {
		values []KeyValue
		err    error
	}
	done := make(chan scanResult, 1)
	go func() {
		values, err := scanner.Scan(context.Background(), []*Range{scanRange})
		done <- scanResult{values: values, err: err}
	}()

	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("two tablet scans did not start concurrently")
		}
	}
	close(release)

	var result scanResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("batch scan did not complete")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.values) != 3 ||
		string(result.values[0].Key.Row) != "a" ||
		string(result.values[1].Key.Row) != "m" ||
		string(result.values[2].Key.Row) != "z" {
		t.Fatalf("values = %+v", result.values)
	}
	adapter.mu.Lock()
	maxStarts := adapter.maxStarts
	adapter.mu.Unlock()
	if maxStarts != 2 {
		t.Fatalf("maximum concurrent starts = %d, want 2", maxStarts)
	}
}

func TestBatchScannerGroupsMultiScansByServer(t *testing.T) {
	tablets := discoveryTablets()
	tablets[0].Location.HostPort = "shared:9997"
	tablets[1].Location.HostPort = "shared:9997"
	tablets[2].Location.HostPort = "other:9997"
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": tablets}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		multiStartResults: []*data.InitialMultiScan{
			{
				ScanID: 40,
				Result_: &data.MultiScanResult_{
					Results: []*data.TKeyValue{testEntry("a", "one"), testEntry("m", "two")},
				},
			},
			{
				ScanID: 41,
				Result_: &data.MultiScanResult_{
					Results: []*data.TKeyValue{testEntry("z", "three")},
				},
			},
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{
		UseMultiScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("z"), true)
	values, err := scanner.Scan(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || adapter.multiStartCalls != 2 || adapter.startCalls != 0 {
		t.Fatalf(
			"values=%+v multiStartCalls=%d startCalls=%d",
			values,
			adapter.multiStartCalls,
			adapter.startCalls,
		)
	}
	if got := adapter.multiAddresses; len(got) != 2 ||
		got[0] != "shared:9997" || got[1] != "other:9997" {
		t.Fatalf("multi addresses = %v", got)
	}
	if len(adapter.multiRequests[0].Batch) != 2 || len(adapter.multiRequests[1].Batch) != 1 {
		t.Fatalf(
			"batch extent counts = %d/%d, want 2/1",
			len(adapter.multiRequests[0].Batch),
			len(adapter.multiRequests[1].Batch),
		)
	}
}

func TestMultiScanExtentIdentityPreservesNilBoundaries(t *testing.T) {
	scanRange, _ := NewRangeRow([]byte("a"))
	tablets := []Tablet{
		{
			Extent: TabletExtent{TableID: "1", EndRow: []byte{}},
			Server: &TabletServer{HostPort: "shared:9997"},
		},
		{
			Extent: TabletExtent{TableID: "1", PrevRow: []byte{}, EndRow: []byte{}},
			Server: &TabletServer{HostPort: "shared:9997"},
		},
		{
			Extent: TabletExtent{TableID: "1"},
			Server: &TabletServer{HostPort: "shared:9997"},
		},
	}
	segments := make([]batchScanSegment, len(tablets))
	for index, tablet := range tablets {
		segments[index] = batchScanSegment{tablet: tablet, scanRange: scanRange}
	}
	groups := groupBatchSegments(segments)
	if len(groups) != 1 || len(groups[0].batch) != 3 {
		t.Fatalf("groups=%d extents=%d, want 1/3", len(groups), len(groups[0].batch))
	}
	nilBoundary := tabletExtentToThrift(tablets[0])
	emptyBoundary := tabletExtentToThrift(tablets[1])
	if thriftExtentEqual(nilBoundary, emptyBoundary) {
		t.Fatal("nil and empty previous-row boundaries must remain distinct")
	}
}

func TestBatchScannerContinuesMultiScanAndFallsBackFailures(t *testing.T) {
	tablets := discoveryTablets()
	for index := range tablets {
		tablets[index].Location.HostPort = "shared:9997"
	}
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": tablets}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  43,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("m", "fallback")}},
		}},
		multiContinues: []*data.MultiScanResult_{{
			Results: []*data.TKeyValue{testEntry("z", "continued")},
		}},
	}
	adapter.multiStartFunc = func(
		_ string,
		req scanclient.MultiStartRequest,
	) (*data.InitialMultiScan, error) {
		var failedExtent *data.TKeyExtent
		var failedRange *data.TRange
		for extent, ranges := range req.Batch {
			if bytes.Equal(extent.EndRow, []byte("p")) {
				failedExtent = extent
				failedRange = ranges[0]
				break
			}
		}
		if failedExtent == nil || failedRange == nil {
			return nil, errors.New("test did not find middle tablet failure")
		}
		walker.mu.Lock()
		walker.tablets["1"][1].Location = &metadata.Location{HostPort: "moved:9997"}
		walker.mu.Unlock()
		return &data.InitialMultiScan{
			ScanID: 42,
			Result_: &data.MultiScanResult_{
				Results:  []*data.TKeyValue{testEntry("a", "initial")},
				Failures: data.ScanBatch{failedExtent: []*data.TRange{failedRange}},
				More:     true,
			},
		}, nil
	}
	connector.scan = adapter
	iterator, err := NewIteratorSetting(
		"ageoff",
		"org.apache.accumulo.core.iterators.user.AgeOffFilter",
		30,
		map[string]string{"ttl": "60000"},
	)
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{
		UseMultiScan: true,
		Columns: []Column{
			NewColumnFamily([]byte("content")),
			NewColumn([]byte("meta"), []byte("type")),
		},
		Iterators: []IteratorSetting{iterator},
	})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("z"), true)
	values, err := scanner.Scan(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 ||
		adapter.multiStartCalls != 1 ||
		adapter.multiContinueCalls != 1 ||
		adapter.multiCloseCalls != 1 ||
		adapter.startCalls != 1 {
		t.Fatalf(
			"values=%+v multi=%d/%d/%d fallback starts=%d",
			values,
			adapter.multiStartCalls,
			adapter.multiContinueCalls,
			adapter.multiCloseCalls,
			adapter.startCalls,
		)
	}
	if got := adapter.addresses[0]; got != "moved:9997" {
		t.Fatalf("fallback address = %q, want moved:9997", got)
	}
	if got := string(adapter.startRequests[0].Extent.EndRow); got != "p" {
		t.Fatalf("fallback extent end = %q, want p", got)
	}
	assertColumns(t, adapter.multiRequests[0].Columns, "meta", "type")
	assertColumns(t, adapter.startRequests[0].Columns, "meta", "type")
	assertIterators(t, adapter.multiRequests[0].Iterators, adapter.multiRequests[0].IteratorOptions)
	assertIterators(t, adapter.startRequests[0].Iterators, adapter.startRequests[0].IteratorOptions)
}

func TestBatchScannerReturnsMultiScanCleanupErrorWithResults(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	closeErr := errors.New("close multi-scan")
	adapter := &fakeScannerAdapter{
		multiStartResults: []*data.InitialMultiScan{{
			ScanID: 44,
			Result_: &data.MultiScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
			},
		}},
		multiCloseErr: closeErr,
	}
	connector.scan = adapter
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{
		UseMultiScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRangeRow([]byte("a"))
	values, err := scanner.Scan(context.Background(), []*Range{scanRange})
	var cleanupErr *CleanupError
	if len(values) != 1 ||
		!errors.Is(err, closeErr) ||
		!errors.As(err, &cleanupErr) ||
		cleanupErr.ScanID != 44 {
		t.Fatalf("values=%+v error=%v", values, err)
	}
}

func TestBatchScannerValidatesRanges(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	connector.scan = &fakeScannerAdapter{}
	scanner, err := connector.NewBatchScanner(Table{ID: "1", Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(context.Background(), nil); err == nil {
		t.Fatal("empty ranges should fail")
	}
	oneRow, _ := NewRangeRow([]byte("a"))
	if _, err := scanner.Scan(context.Background(), []*Range{oneRow, nil}); err == nil {
		t.Fatal("nil range should fail")
	}
	if _, err := connector.NewBatchScanner(
		Table{ID: "1", Name: "events"},
		ScannerOptions{Parallelism: -1},
	); err == nil {
		t.Fatal("negative parallelism should fail")
	}
	if _, err := NewIteratorSetting("", "example.Iterator", 10, nil); err == nil {
		t.Fatal("empty iterator name should fail")
	}
	if _, err := NewIteratorSetting("example", "", 10, nil); err == nil {
		t.Fatal("empty iterator class should fail")
	}
	if _, err := NewIteratorSetting("example", "example.Iterator", 0, nil); err == nil {
		t.Fatal("zero iterator priority should fail")
	}
	first, _ := NewIteratorSetting("first", "example.First", 10, nil)
	second, _ := NewIteratorSetting("second", "example.Second", 10, nil)
	if _, err := connector.NewScanner(
		Table{ID: "1", Name: "events"},
		ScannerOptions{Iterators: []IteratorSetting{first, second}},
	); err == nil {
		t.Fatal("duplicate iterator priority should fail")
	}
	second, _ = NewIteratorSetting("first", "example.Second", 20, nil)
	if _, err := connector.NewScanner(
		Table{ID: "1", Name: "events"},
		ScannerOptions{Iterators: []IteratorSetting{first, second}},
	); err == nil {
		t.Fatal("duplicate iterator name should fail")
	}
}

func assertWireRange(t *testing.T, scanRange *data.TRange, start []byte, startInclusive bool, stop []byte) {
	t.Helper()
	if !bytes.Equal(scanRange.Start.Row, start) || scanRange.StartKeyInclusive != startInclusive {
		t.Fatalf("start=%q inclusive=%v, want %q/%v", scanRange.Start.Row, scanRange.StartKeyInclusive, start, startInclusive)
	}
	if !bytes.Equal(scanRange.Stop.Row, stop) || scanRange.StopKeyInclusive {
		t.Fatalf("stop=%q inclusive=%v, want %q/false", scanRange.Stop.Row, scanRange.StopKeyInclusive, stop)
	}
}

func testEntry(row, value string) *data.TKeyValue {
	return &data.TKeyValue{
		Key: &data.TKey{
			Row:          []byte(row),
			ColFamily:    []byte("cf"),
			ColQualifier: []byte("cq"),
			Timestamp:    17,
		},
		Value: []byte(value),
	}
}

func assertColumns(t *testing.T, columns []*data.TColumn, exactFamily, exactQualifier string) {
	t.Helper()
	if len(columns) != 2 ||
		string(columns[0].ColumnFamily) != "content" ||
		columns[0].ColumnQualifier != nil ||
		string(columns[1].ColumnFamily) != exactFamily ||
		string(columns[1].ColumnQualifier) != exactQualifier {
		t.Fatalf("columns = %+v", columns)
	}
}

func assertIterators(
	t *testing.T,
	iterators []*data.IterInfo,
	options map[string]map[string]string,
) {
	t.Helper()
	if len(iterators) != 1 {
		t.Fatalf("iterators = %+v", iterators)
	}
	iterator := iterators[0]
	switch iterator.IterName {
	case "versioning":
		if iterator.Priority != 20 ||
			iterator.ClassName != "org.apache.accumulo.core.iterators.user.VersioningIterator" ||
			options["versioning"]["maxVersions"] != "3" {
			t.Fatalf("iterator/options = %+v/%+v", iterator, options)
		}
	case "ageoff":
		if iterator.Priority != 30 ||
			iterator.ClassName != "org.apache.accumulo.core.iterators.user.AgeOffFilter" ||
			options["ageoff"]["ttl"] != "60000" {
			t.Fatalf("iterator/options = %+v/%+v", iterator, options)
		}
	default:
		t.Fatalf("iterator/options = %+v/%+v", iterator, options)
	}
}
