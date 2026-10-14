package accumulo

import (
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

func streamTestConnector(t *testing.T) *Connector {
	t.Helper()
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	return testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
}

func drainStream(t *testing.T, stream *ResultStream) []KeyValue {
	t.Helper()
	var values []KeyValue
	for stream.Next() {
		values = append(values, stream.Entry())
	}
	return values
}

func rowsOf(values []KeyValue) string {
	rows := make([]string, 0, len(values))
	for _, value := range values {
		rows = append(rows, string(value.Key.Row))
	}
	return strings.Join(rows, ",")
}

func TestStreamPullsOneBatchAtATime(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID: 7,
			Result_: &data.ScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
				More:    true,
			},
		}},
		continues: []*data.ScanResult_{
			{Results: []*data.TKeyValue{testEntry("b", "two")}, More: true},
			{Results: []*data.TKeyValue{testEntry("c", "three")}},
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, err := NewRange([]byte("a"), true, []byte("c"), true)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Next() {
		t.Fatalf("first Next = false, err = %v", stream.Err())
	}
	if string(stream.Entry().Key.Row) != "a" {
		t.Fatalf("first entry row = %q", stream.Entry().Key.Row)
	}
	if adapter.continueCalls != 0 {
		t.Fatalf("continue called %d times before the first batch was drained", adapter.continueCalls)
	}
	values := append([]KeyValue{stream.Entry()}, drainStream(t, stream)...)
	if got := rowsOf(values); got != "a,b,c" {
		t.Fatalf("rows = %q", got)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if adapter.startCalls != 1 || adapter.continueCalls != 2 {
		t.Fatalf("start=%d continue=%d", adapter.startCalls, adapter.continueCalls)
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
}

func TestStreamCloseIsIdempotentAndStopsIteration(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID: 11,
			Result_: &data.ScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one"), testEntry("b", "two")},
				More:    true,
			},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() {
		t.Fatalf("Next = false, err = %v", stream.Err())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
	if stream.Next() {
		t.Fatal("Next after Close returned true")
	}
	if !errors.Is(stream.Err(), ErrStreamClosed) {
		t.Fatalf("Err after Close = %v", stream.Err())
	}
}

func TestStreamCloseUnblocksNextFromAnotherGoroutine(t *testing.T) {
	connector := streamTestConnector(t)
	continueEntered := make(chan struct{})
	continueRelease := make(chan struct{})
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  3,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
		continueEntered: continueEntered,
		continueRelease: continueRelease,
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() {
		t.Fatalf("Next = false, err = %v", stream.Err())
	}

	blocked := make(chan bool, 1)
	go func() { blocked <- stream.Next() }()
	<-continueEntered

	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	select {
	case more := <-blocked:
		if more {
			t.Fatal("Next produced an entry after Close cancelled the RPC")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not return after Close")
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("Err after Close = %v", stream.Err())
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
	close(continueRelease)
}

func TestStreamReportsCleanupFailureAfterDraining(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  5,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}},
		}},
		closeErr: errors.New("close boom"),
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	values := drainStream(t, stream)
	if got := rowsOf(values); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	var cleanup *CleanupError
	if !errors.As(stream.Err(), &cleanup) || cleanup.ScanID != 5 {
		t.Fatalf("Err = %v", stream.Err())
	}
	if err := stream.Close(); !errors.As(err, &cleanup) {
		t.Fatalf("Close = %v", err)
	}
}

func TestStreamStopsOnContinueFailureAndClosesSession(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("continue boom")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  9,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
		continueErr: failure,
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), failure) {
		t.Fatalf("Err = %v", stream.Err())
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
}

func TestStreamDeliversInitialEntriesBeforeAZeroScanIDFailure(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  0,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "zero scan ID") {
		t.Fatalf("Err = %v", err)
	}
}

func TestStreamRetriesAStaleTabletBeforeDeliveringEntries(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startErrors: []error{tabletserver.NewNotServingTabletException(), nil},
		startResults: []*data.InitialScan{nil, {
			ScanID:  4,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if adapter.startCalls != 2 {
		t.Fatalf("start called %d times", adapter.startCalls)
	}
}

func TestStreamValidatesArguments(t *testing.T) {
	connector := streamTestConnector(t)
	connector.scan = &fakeScannerAdapter{}
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Stream(context.Background(), nil); err == nil {
		t.Fatal("nil range accepted")
	}
	if _, err := batch.Stream(context.Background(), nil); err == nil {
		t.Fatal("empty range list accepted")
	}
	spanning, _ := NewRange([]byte("a"), true, []byte("z"), true)
	if _, err := scanner.Stream(context.Background(), spanning); !errors.Is(err, ErrRangeSpansTablets) {
		t.Fatalf("spanning range error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	single, _ := NewRange([]byte("a"), true, []byte("c"), true)
	if _, err := scanner.Stream(cancelled, single); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v", err)
	}
	if _, err := batch.Stream(cancelled, []*Range{single}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch context error = %v", err)
	}
}

func TestStreamCancellationStopsIterationAndReleasesTheSession(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  21,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(ctx, scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() {
		t.Fatalf("Next = false, err = %v", stream.Err())
	}
	cancel()
	if stream.Next() {
		t.Fatal("Next kept iterating after cancellation")
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("Err = %v", stream.Err())
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
	if adapter.closeContext != nil {
		t.Fatalf("close ran with a cancelled context: %v", adapter.closeContext)
	}
}

func TestBatchScannerStreamReadsTabletsInOrderOneAtATime(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 1, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			{ScanID: 2, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("m", "two")}}},
			{ScanID: 3, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("z", "three")}}},
		},
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	spanning, _ := NewRange([]byte("a"), true, []byte("z"), true)
	stream, err := batch.Stream(context.Background(), []*Range{spanning})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a,m,z" {
		t.Fatalf("rows = %q", got)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if adapter.maxStarts != 1 {
		t.Fatalf("streamed %d concurrent scans", adapter.maxStarts)
	}
	if got := strings.Join(adapter.addresses, ","); got != "ts1:9997,ts2:9997,ts3:9997" {
		t.Fatalf("addresses = %q", got)
	}
	if adapter.closeCalls != 3 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
}

func TestBatchScannerStreamStopsAtTheFailingTablet(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("second tablet down")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 1, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			nil,
		},
		startErrors: []error{nil, failure},
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spanning, _ := NewRange([]byte("a"), true, []byte("z"), true)
	stream, err := batch.Stream(context.Background(), []*Range{spanning})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), failure) {
		t.Fatalf("Err = %v", stream.Err())
	}
}

func TestBatchScannerStreamMultiScanReplansFailedRanges(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 34, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("m", "two")}}},
		},
	}
	adapter.multiStartFunc = func(
		address string,
		req scanclient.MultiStartRequest,
	) (*data.InitialMultiScan, error) {
		switch address {
		case "ts1:9997":
			return &data.InitialMultiScan{ScanID: 31, Result_: &data.MultiScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
			}}, nil
		case "ts2:9997":
			// The server reports the whole group as failed, exactly as it does
			// when the tablet moved between planning and the scan.
			return &data.InitialMultiScan{ScanID: 32, Result_: &data.MultiScanResult_{
				Failures: req.Batch,
			}}, nil
		default:
			return &data.InitialMultiScan{ScanID: 33, Result_: &data.MultiScanResult_{
				Results: []*data.TKeyValue{testEntry("z", "three")},
			}}, nil
		}
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{UseMultiScan: true})
	if err != nil {
		t.Fatal(err)
	}
	spanning, _ := NewRange([]byte("a"), true, []byte("z"), true)
	stream, err := batch.Stream(context.Background(), []*Range{spanning})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	rows := rowsOf(drainStream(t, stream))
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if rows != "a,m,z" {
		t.Fatalf("rows = %q", rows)
	}
	if adapter.multiStartCalls != 3 || adapter.startCalls != 1 {
		t.Fatalf("multiStart=%d start=%d", adapter.multiStartCalls, adapter.startCalls)
	}
	if adapter.multiCloseCalls != 3 || adapter.closeCalls != 1 {
		t.Fatalf("multiClose=%d close=%d", adapter.multiCloseCalls, adapter.closeCalls)
	}
}

func TestBatchScannerStreamRejectsAdaptersWithoutMultiScan(t *testing.T) {
	connector := streamTestConnector(t)
	connector.scan = singleOnlyScanAdapter{}
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{UseMultiScan: true})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	if _, err := batch.Stream(context.Background(), []*Range{scanRange}); err == nil ||
		!strings.Contains(err.Error(), "multi-scan") {
		t.Fatalf("Stream error = %v", err)
	}
}

func TestStreamEntriesAreOwnedByTheCaller(t *testing.T) {
	connector := streamTestConnector(t)
	entry := testEntry("a", "one")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  41,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{entry}},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Next() {
		t.Fatalf("Next = false, err = %v", stream.Err())
	}
	value := stream.Entry()
	value.Key.Row[0] = 'X'
	value.Value[0] = 'X'
	if string(entry.Key.Row) != "a" || string(entry.Value) != "one" {
		t.Fatalf("wire entry mutated: row=%q value=%q", entry.Key.Row, entry.Value)
	}
}

func TestStreamAndScanReturnTheSameEntries(t *testing.T) {
	newAdapter := func() *fakeScannerAdapter {
		return &fakeScannerAdapter{
			startResults: []*data.InitialScan{{
				ScanID: 51,
				Result_: &data.ScanResult_{
					Results: []*data.TKeyValue{testEntry("a", "one")},
					More:    true,
				},
			}},
			continues: []*data.ScanResult_{
				{Results: []*data.TKeyValue{testEntry("b", "two"), testEntry("c", "three")}},
			},
		}
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)

	connector := streamTestConnector(t)
	connector.scan = newAdapter()
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := scanner.Scan(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}

	streamConnector := streamTestConnector(t)
	streamConnector.scan = newAdapter()
	streamScanner, err := streamConnector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := streamScanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	streamed := drainStream(t, stream)
	if rowsOf(scanned) != rowsOf(streamed) {
		t.Fatalf("scan rows %q != stream rows %q", rowsOf(scanned), rowsOf(streamed))
	}
}

func TestScannerStreamsAreIndependentCursors(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{
			{ScanID: 61, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			{ScanID: 62, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	first, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(drainStream(t, first)); got != "a" {
		t.Fatalf("first rows = %q", got)
	}
	second, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(drainStream(t, second)); got != "a" {
		t.Fatalf("restarted rows = %q", got)
	}
	if err := errors.Join(first.Close(), second.Close()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if adapter.startCalls != 2 {
		t.Fatalf("start called %d times", adapter.startCalls)
	}
}

func TestStreamConcurrentCloseAndIterationIsRaceFree(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  71,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
		continues: []*data.ScanResult_{
			{Results: []*data.TKeyValue{testEntry("b", "two")}, More: true},
			{Results: []*data.TKeyValue{testEntry("c", "three")}, More: true},
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for stream.Next() {
			_ = stream.Entry()
		}
	}()
	go func() {
		defer workers.Done()
		_ = stream.Close()
	}()
	workers.Wait()
	_ = stream.Err()
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestStreamDeliversInitialEntriesBeforeAStartFailure(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("partial start")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  81,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
		startErrors: []error{failure},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), failure) {
		t.Fatalf("Err = %v", stream.Err())
	}
	if adapter.closeCalls != 1 {
		t.Fatalf("close called %d times", adapter.closeCalls)
	}
}

func TestBatchScannerStreamDeliversMultiScanEntriesBeforeAStartFailure(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("partial multi start")
	adapter := &fakeScannerAdapter{
		multiStartResults: []*data.InitialMultiScan{{
			ScanID: 91,
			Result_: &data.MultiScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
				More:    true,
			},
		}},
		multiStartErrors: []error{failure},
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{UseMultiScan: true})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := batch.Stream(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), failure) {
		t.Fatalf("Err = %v", stream.Err())
	}
	if adapter.multiCloseCalls != 1 {
		t.Fatalf("multi close called %d times", adapter.multiCloseCalls)
	}
}

func TestStreamDeliversContinueEntriesThatArriveWithAFailure(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("lease release failed")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  101,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
		continues: []*data.ScanResult_{
			{Results: []*data.TKeyValue{testEntry("b", "two")}, More: true},
		},
		continueErr:           failure,
		continueErrWithResult: true,
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a,b" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), failure) {
		t.Fatalf("Err = %v", stream.Err())
	}
}

func TestBatchScannerStreamDeliversMultiContinueEntriesThatArriveWithAFailure(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("multi lease release failed")
	adapter := &fakeScannerAdapter{
		multiStartResults: []*data.InitialMultiScan{{
			ScanID: 111,
			Result_: &data.MultiScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
				More:    true,
			},
		}},
		multiContinues: []*data.MultiScanResult_{
			{Results: []*data.TKeyValue{testEntry("b", "two")}, More: true},
		},
		multiContinueErr:           failure,
		multiContinueErrWithResult: true,
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{UseMultiScan: true})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := batch.Stream(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a,b" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), failure) {
		t.Fatalf("Err = %v", stream.Err())
	}
}

func TestBatchScannerStreamRejectsMultiScanContinuationWithoutAScanID(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		multiStartResults: []*data.InitialMultiScan{{
			ScanID: 0,
			Result_: &data.MultiScanResult_{
				Results: []*data.TKeyValue{testEntry("a", "one")},
				More:    true,
			},
		}},
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{UseMultiScan: true})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := batch.Stream(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	err = stream.Err()
	if err == nil || !strings.Contains(err.Error(), "multi-scan has zero scan ID") {
		t.Fatalf("Err = %v", err)
	}
	if adapter.multiContinueCalls != 0 {
		t.Fatalf("continued a multi-scan without a scan ID %d times", adapter.multiContinueCalls)
	}
}

func TestStreamCloseReportsCleanupFailuresAfterAFatalError(t *testing.T) {
	connector := streamTestConnector(t)
	failure := errors.New("continue boom")
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  121,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
		continueErr: failure,
		closeErr:    errors.New("close boom"),
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	var cleanup *CleanupError
	if !errors.Is(stream.Err(), failure) || !errors.As(stream.Err(), &cleanup) {
		t.Fatalf("Err = %v", stream.Err())
	}
	closeErr := stream.Close()
	if !errors.As(closeErr, &cleanup) || cleanup.ScanID != 121 {
		t.Fatalf("Close = %v", closeErr)
	}
	if errors.Is(closeErr, failure) {
		t.Fatalf("Close reported the iteration failure: %v", closeErr)
	}
}

func TestStreamCloseRecordsErrStreamClosedImmediately(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  131,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}, More: true},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() {
		t.Fatalf("Next = false, err = %v", stream.Err())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !errors.Is(stream.Err(), ErrStreamClosed) {
		t.Fatalf("Err right after Close = %v", stream.Err())
	}
	if stream.Next() {
		t.Fatal("Next after Close returned true")
	}
}

func TestStreamCloseAfterDrainingReportsNoError(t *testing.T) {
	connector := streamTestConnector(t)
	adapter := &fakeScannerAdapter{
		startResults: []*data.InitialScan{{
			ScanID:  141,
			Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}},
		}},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(drainStream(t, stream)); got != "a" {
		t.Fatalf("rows = %q", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err after a clean drain = %v", err)
	}
}

func splitTablets() []metadata.TabletInfo {
	return []metadata.TabletInfo{
		{TableID: "1", EndRow: []byte("b"), Location: &metadata.Location{HostPort: "ts1:9997", Session: "a"}},
		{TableID: "1", PrevRow: []byte("b"), EndRow: []byte("k"), Location: &metadata.Location{HostPort: "ts2:9997", Session: "b"}},
		{TableID: "1", PrevRow: []byte("k"), EndRow: []byte("p"), Location: &metadata.Location{HostPort: "ts2:9997", Session: "b"}},
		{TableID: "1", PrevRow: []byte("p"), Location: &metadata.Location{HostPort: "ts3:9997", Session: "c"}},
	}
}

func TestBatchScannerStreamReplansASegmentWhoseTabletSplit(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startErrors: []error{tabletserver.NewNotServingTabletException()},
		startResults: []*data.InitialScan{
			nil,
			{ScanID: 201, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("a", "one")}}},
			{ScanID: 202, Result_: &data.ScanResult_{Results: []*data.TKeyValue{testEntry("c", "two")}}},
		},
		onFirstStart: func() {
			walker.mu.Lock()
			walker.tablets["1"] = splitTablets()
			walker.mu.Unlock()
		},
	}
	connector.scan = adapter
	batch, err := connector.NewBatchScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := batch.Stream(context.Background(), []*Range{scanRange})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	rows := rowsOf(drainStream(t, stream))
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if rows != "a,c" {
		t.Fatalf("rows = %q", rows)
	}
	if got := strings.Join(adapter.addresses, ","); got != "ts1:9997,ts1:9997,ts2:9997" {
		t.Fatalf("addresses = %q", got)
	}
}

func TestStreamRejectsARangeWhoseTabletSplitDuringRetry(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})
	adapter := &fakeScannerAdapter{
		startErrors:  []error{tabletserver.NewNotServingTabletException()},
		startResults: []*data.InitialScan{nil},
		onFirstStart: func() {
			walker.mu.Lock()
			walker.tablets["1"] = splitTablets()
			walker.mu.Unlock()
		},
	}
	connector.scan = adapter
	scanner, err := connector.NewScanner(Table{Name: "events"}, ScannerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scanRange, _ := NewRange([]byte("a"), true, []byte("c"), true)
	stream, err := scanner.Stream(context.Background(), scanRange)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if got := rowsOf(drainStream(t, stream)); got != "" {
		t.Fatalf("rows = %q", got)
	}
	if !errors.Is(stream.Err(), ErrRangeSpansTablets) {
		t.Fatalf("Err = %v", stream.Err())
	}
	if adapter.startCalls != 1 {
		t.Fatalf("start called %d times", adapter.startCalls)
	}
}

type singleOnlyScanAdapter struct{}

func (singleOnlyScanAdapter) Start(
	context.Context,
	string,
	scanclient.StartRequest,
) (*data.InitialScan, error) {
	return nil, errors.New("unused")
}

func (singleOnlyScanAdapter) Continue(
	context.Context,
	string,
	data.ScanID,
	int64,
) (*data.ScanResult_, error) {
	return nil, errors.New("unused")
}

func (singleOnlyScanAdapter) CloseScan(context.Context, string, data.ScanID) error { return nil }

func (singleOnlyScanAdapter) Close() error { return nil }
