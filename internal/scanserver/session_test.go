package scanserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

func TestStartScan_ContinuationAtExactByteBoundary(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-exact.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)

	fullSrv := newSessionTestServer(t, loc, mem, 1<<20, time.Minute)
	full := startWholeTableScan(t, fullSrv, "1")
	fullPairs := pairsOf(full.Result_.Results)
	if got := len(full.Result_.Results); got != 3 {
		t.Fatalf("full scan returned %d results, want 3", got)
	}
	capBytes := approxKVSize(full.Result_.Results[0]) + approxKVSize(full.Result_.Results[1])

	srv := newSessionTestServer(t, loc, mem, capBytes, time.Minute)
	initial := startWholeTableScan(t, srv, "1")
	if !initial.Result_.More {
		t.Fatal("initial result More=false, want continuation")
	}
	if initial.ScanID == 0 {
		t.Fatal("initial ScanID=0, want opaque continuation ID")
	}
	if got := pairsOf(initial.Result_.Results); !equalStrings(got, fullPairs[:2]) {
		t.Fatalf("initial page = %v, want %v", got, fullPairs[:2])
	}

	got := collectSingleScan(t, srv, initial)
	if !equalStrings(got, fullPairs) {
		t.Fatalf("collected pairs = %v, want %v", got, fullPairs)
	}
	exhausted, err := srv.ContinueScan(context.Background(), nil, initial.ScanID, 0)
	if err != nil || exhausted.More || len(exhausted.Results) != 0 {
		t.Fatalf("ContinueScan after exhaustion = %+v, %v; want empty no-op", exhausted, err)
	}
	if err := srv.CloseScan(context.Background(), nil, initial.ScanID); err != nil {
		t.Fatalf("CloseScan after exhaustion: %v", err)
	}
	if got := srv.scans.count(); got != 0 {
		t.Fatalf("session count = %d, want 0", got)
	}
}

func TestStartScan_ContinuationAfterByteOvershoot(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-overshoot.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)

	fullSrv := newSessionTestServer(t, loc, mem, 1<<20, time.Minute)
	full := startWholeTableScan(t, fullSrv, "1")
	fullPairs := pairsOf(full.Result_.Results)
	capBytes := approxKVSize(full.Result_.Results[0]) + approxKVSize(full.Result_.Results[1]) - 1

	srv := newSessionTestServer(t, loc, mem, capBytes, time.Minute)
	initial := startWholeTableScan(t, srv, "1")
	if !initial.Result_.More {
		t.Fatal("initial result More=false, want continuation")
	}
	if got := pairsOf(initial.Result_.Results); !equalStrings(got, fullPairs[:2]) {
		t.Fatalf("initial page = %v, want %v", got, fullPairs[:2])
	}

	got := collectSingleScan(t, srv, initial)
	if !equalStrings(got, fullPairs) {
		t.Fatalf("collected pairs = %v, want %v", got, fullPairs)
	}
}

func TestCloseScan_ReleasesContinuationState(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-close.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)

	initial := startWholeTableScan(t, srv, "1")
	if initial.ScanID == 0 || !initial.Result_.More {
		t.Fatalf("initial = %+v, want retained continuation state", initial)
	}
	if err := srv.CloseScan(context.Background(), nil, initial.ScanID); err != nil {
		t.Fatalf("CloseScan: %v", err)
	}
	if got := srv.scans.count(); got != 0 {
		t.Fatalf("session count = %d, want 0", got)
	}
	if _, err := srv.ContinueScan(context.Background(), nil, initial.ScanID, 0); !isNoSuchScanID(err) {
		t.Fatalf("ContinueScan after close error = %v, want NoSuchScanIDException", err)
	}
}

func TestScanSessionExpiryRemovesContinuationState(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-expiry.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)

	initial := startWholeTableScan(t, srv, "1")
	srv.scans.mu.Lock()
	srv.scans.sessions[initial.ScanID].expiresAt = time.Now().Add(-time.Second)
	srv.scans.mu.Unlock()

	if _, err := srv.ContinueScan(context.Background(), nil, initial.ScanID, 0); !isNoSuchScanID(err) {
		t.Fatalf("ContinueScan after expiry error = %v, want NoSuchScanIDException", err)
	}
	if err := srv.CloseScan(context.Background(), nil, initial.ScanID); !isNoSuchScanID(err) {
		t.Fatalf("CloseScan after expiry error = %v, want NoSuchScanIDException", err)
	}
}

func TestScanSessionUnknownIDReturnsNoSuchScanID(t *testing.T) {
	srv, err := NewServer(Options{Locator: &stubLocator{}, Storage: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ContinueScan(context.Background(), nil, 77, 0); !isNoSuchScanID(err) {
		t.Fatalf("ContinueScan error = %v, want NoSuchScanIDException", err)
	}
	if err := srv.CloseScan(context.Background(), nil, 77); !isNoSuchScanID(err) {
		t.Fatalf("CloseScan error = %v, want NoSuchScanIDException", err)
	}
}

func TestContinueScan_CanceledRequestDoesNotConsumePage(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-cancel.rf"
	loc := newSessionTestData(t, mem, path, "1", []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	})
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)
	initial := startWholeTableScan(t, srv, "1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.ContinueScan(ctx, nil, initial.ScanID, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ContinueScan canceled error = %v, want context.Canceled", err)
	}
	got := collectSingleScan(t, srv, initial)
	if want := []string{"row01=value-01", "row02=value-02", "row03=value-03"}; !equalStrings(got, want) {
		t.Fatalf("retry after cancellation = %v, want %v", got, want)
	}
}

func TestStartScan_SessionCapacityReturnsBusy(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-capacity.rf"
	loc := newSessionTestData(t, mem, path, "1", []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
	})
	srv, err := NewServer(Options{
		Locator:             loc,
		Storage:             mem,
		ScanResultBytesCap:  1,
		ScanSessionTTL:      time.Minute,
		ScanSessionCapacity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := startWholeTableScan(t, srv, "1")
	if first.ScanID == 0 {
		t.Fatal("first scan did not retain continuation")
	}
	if _, err := startWholeTableScanErr(srv, "1"); !isScanServerBusy(err) {
		t.Fatalf("second StartScan error = %v, want ScanServerBusyException", err)
	}
	if err := srv.CloseScan(context.Background(), nil, first.ScanID); err != nil {
		t.Fatal(err)
	}
	if next := startWholeTableScan(t, srv, "1"); next.ScanID == 0 {
		t.Fatal("capacity was not reclaimed after close")
	}
}

func TestStartScan_SessionByteCapacityReturnsBusyWithoutRetention(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-byte-capacity.rf"
	loc := newSessionTestData(t, mem, path, "1", []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
	})
	srv, err := NewServer(Options{
		Locator:                  loc,
		Storage:                  mem,
		ScanResultBytesCap:       1,
		ScanSessionBytesCapacity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startWholeTableScanErr(srv, "1"); !isScanServerBusy(err) {
		t.Fatalf("StartScan error = %v, want ScanServerBusyException", err)
	}
	if srv.scans.count() != 0 || srv.scans.bytes != 0 {
		t.Fatalf("rejected session retained count=%d bytes=%d", srv.scans.count(), srv.scans.bytes)
	}
}

func TestScanSessionsSupportConcurrentAccess(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/session-concurrent.rf"
	cells := make([]cellSpec, 0, 12)
	want := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		row := fmt.Sprintf("row%02d", i)
		value := fmt.Sprintf("value-%02d", i)
		cells = append(cells, cellSpec{row: row, cf: "cf", cq: "cq", value: value, ts: int64(i + 1)})
		want = append(want, row+"="+value)
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	srv := newSessionTestServer(t, loc, mem, 128, time.Minute)

	const workers = 16
	errCh := make(chan error, workers)
	idCh := make(chan data.ScanID, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			initial, err := startWholeTableScanErr(srv, "1")
			if err != nil {
				errCh <- err
				return
			}
			if initial.ScanID == 0 || !initial.Result_.More {
				errCh <- fmt.Errorf("initial = %+v, want continuation state", initial)
				return
			}
			idCh <- initial.ScanID
			got, err := collectSingleScanErr(srv, initial)
			if err != nil {
				errCh <- err
				return
			}
			if !equalStrings(got, want) {
				errCh <- fmt.Errorf("collected pairs = %v, want %v", got, want)
				return
			}
			if err := srv.CloseScan(context.Background(), nil, initial.ScanID); err != nil {
				errCh <- fmt.Errorf("CloseScan(%d): %w", initial.ScanID, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	close(idCh)

	for err := range errCh {
		if err != nil {
			t.Error(err)
		}
	}
	seen := make(map[data.ScanID]struct{}, workers)
	for id := range idCh {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate scan ID %d generated under concurrency", id)
		}
		seen[id] = struct{}{}
	}
	if got := len(seen); got != workers {
		t.Fatalf("got %d scan IDs, want %d", got, workers)
	}
	if got := srv.scans.count(); got != 0 {
		t.Fatalf("session count = %d, want 0", got)
	}
}

func newSessionTestData(t *testing.T, mem *memory.Backend, path, table string, cells []cellSpec) *stubLocator {
	t.Helper()
	writeRFileToMemory(t, mem, path, cells)
	return &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			table: {{TableID: table, Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
}

func newSessionTestServer(t *testing.T, loc *stubLocator, mem *memory.Backend, pageCap int, ttl time.Duration) *Server {
	t.Helper()
	srv, err := NewServer(Options{
		Locator:            loc,
		Storage:            mem,
		ScanResultBytesCap: pageCap,
		ScanSessionTTL:     ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func startWholeTableScan(t *testing.T, srv *Server, table string) *data.InitialScan {
	t.Helper()
	resp, err := startWholeTableScanErr(srv, table)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func startWholeTableScanErr(srv *Server, table string) (*data.InitialScan, error) {
	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte(table)},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Result_ == nil {
		return nil, fmt.Errorf("StartScan returned nil response: %+v", resp)
	}
	return resp, nil
}

func collectSingleScan(t *testing.T, srv *Server, initial *data.InitialScan) []string {
	t.Helper()
	out, err := collectSingleScanErr(srv, initial)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func collectSingleScanErr(srv *Server, initial *data.InitialScan) ([]string, error) {
	out := append([]string(nil), pairsOf(initial.Result_.Results)...)
	for more := initial.Result_.More; more; {
		resp, err := srv.ContinueScan(context.Background(), nil, initial.ScanID, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, pairsOf(resp.Results)...)
		more = resp.More
	}
	return out, nil
}

func pairsOf(results []*data.TKeyValue) []string {
	out := make([]string, 0, len(results))
	for _, kv := range results {
		out = append(out, string(kv.Key.Row)+"="+string(kv.Value))
	}
	return out
}

func isNoSuchScanID(err error) bool {
	var target *tabletserver.NoSuchScanIDException
	return errors.As(err, &target)
}
