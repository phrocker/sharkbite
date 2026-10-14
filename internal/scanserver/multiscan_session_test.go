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
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
)

func TestStartMultiScan_ContinuationAtExactByteBoundary(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-exact.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
		{row: "row10", cf: "cf", cq: "cq", value: "value-10", ts: 4},
		{row: "row11", cf: "cf", cq: "cq", value: "value-11", ts: 5},
		{row: "row20", cf: "cf", cq: "cq", value: "value-20", ts: 6},
		{row: "row21", cf: "cf", cq: "cq", value: "value-21", ts: 7},
		{row: "row22", cf: "cf", cq: "cq", value: "value-22", ts: 8},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	batch := multiBatch("1",
		rowRange("row01", "row04"),
		rowRange("row10", "row12"),
		rowRange("row20", "row23"),
	)

	fullSrv := newSessionTestServer(t, loc, mem, 1<<20, time.Minute)
	full := startMultiScan(t, fullSrv, batch)
	if full.Result_.More {
		t.Fatal("single-page multiscan unexpectedly requires continuation")
	}
	if full.ScanID == 0 {
		t.Fatal("single-page multiscan returned scan ID 0")
	}
	if err := fullSrv.CloseMultiScan(context.Background(), nil, full.ScanID); err != nil {
		t.Fatalf("CloseMultiScan after single-page result: %v", err)
	}
	fullPairs := pairsOf(full.Result_.Results)
	capBytes := approxKVSize(full.Result_.Results[0]) + approxKVSize(full.Result_.Results[1])

	srv := newSessionTestServer(t, loc, mem, capBytes, time.Minute)
	initial := startMultiScan(t, srv, batch)
	if !initial.Result_.More {
		t.Fatal("initial result More=false, want continuation")
	}
	if initial.ScanID == 0 {
		t.Fatal("initial ScanID=0, want opaque continuation ID")
	}
	if got := pairsOf(initial.Result_.Results); !equalStrings(got, fullPairs[:2]) {
		t.Fatalf("initial page = %v, want %v", got, fullPairs[:2])
	}

	got := collectMultiScan(t, srv, initial)
	if !equalStrings(got, fullPairs) {
		t.Fatalf("collected pairs = %v, want %v", got, fullPairs)
	}
	exhausted, err := srv.ContinueMultiScan(context.Background(), nil, initial.ScanID, 0)
	if err != nil || exhausted.More || len(exhausted.Results) != 0 {
		t.Fatalf("ContinueMultiScan after exhaustion = %+v, %v; want empty no-op", exhausted, err)
	}
	if err := srv.CloseMultiScan(context.Background(), nil, initial.ScanID); err != nil {
		t.Fatalf("CloseMultiScan after exhaustion: %v", err)
	}
	if got := srv.multiScans.count(); got != 0 {
		t.Fatalf("session count = %d, want 0", got)
	}
}

func TestStartMultiScan_ContinuationAfterByteOvershoot(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-overshoot.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
		{row: "row04", cf: "cf", cq: "cq", value: "value-04", ts: 4},
		{row: "row05", cf: "cf", cq: "cq", value: "value-05", ts: 5},
		{row: "row06", cf: "cf", cq: "cq", value: "value-06", ts: 6},
		{row: "row07", cf: "cf", cq: "cq", value: "value-07", ts: 7},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	batch := multiBatch("1",
		rowRange("row01", "row05"),
		rowRange("row03", "row08"),
	)

	fullSrv := newSessionTestServer(t, loc, mem, 1<<20, time.Minute)
	full := startMultiScan(t, fullSrv, batch)
	fullPairs := pairsOf(full.Result_.Results)
	capBytes := approxKVSize(full.Result_.Results[0]) + approxKVSize(full.Result_.Results[1]) - 1

	srv := newSessionTestServer(t, loc, mem, capBytes, time.Minute)
	initial := startMultiScan(t, srv, batch)
	if !initial.Result_.More {
		t.Fatal("initial result More=false, want continuation")
	}
	if got := pairsOf(initial.Result_.Results); !equalStrings(got, fullPairs[:2]) {
		t.Fatalf("initial page = %v, want %v", got, fullPairs[:2])
	}

	got := collectMultiScan(t, srv, initial)
	if !equalStrings(got, fullPairs) {
		t.Fatalf("collected pairs = %v, want %v", got, fullPairs)
	}
}

func TestCloseMultiScan_ReleasesContinuationState(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-close.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)

	initial := startMultiScan(t, srv, multiBatch("1", rowRange("row01", "row04")))
	if initial.ScanID == 0 || !initial.Result_.More {
		t.Fatalf("initial = %+v, want retained continuation state", initial)
	}
	if err := srv.CloseMultiScan(context.Background(), nil, initial.ScanID); err != nil {
		t.Fatalf("CloseMultiScan: %v", err)
	}
	if got := srv.multiScans.count(); got != 0 {
		t.Fatalf("session count = %d, want 0", got)
	}
	if _, err := srv.ContinueMultiScan(context.Background(), nil, initial.ScanID, 0); !isNoSuchScanID(err) {
		t.Fatalf("ContinueMultiScan after close error = %v, want NoSuchScanIDException", err)
	}
}

func TestMultiScanSessionExpiryRemovesContinuationState(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-expiry.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)

	initial := startMultiScan(t, srv, multiBatch("1", rowRange("row01", "row04")))
	srv.multiScans.mu.Lock()
	srv.multiScans.sessions[initial.ScanID].expiresAt = time.Now().Add(-time.Second)
	srv.multiScans.mu.Unlock()

	if _, err := srv.ContinueMultiScan(context.Background(), nil, initial.ScanID, 0); !isNoSuchScanID(err) {
		t.Fatalf("ContinueMultiScan after expiry error = %v, want NoSuchScanIDException", err)
	}
	if err := srv.CloseMultiScan(context.Background(), nil, initial.ScanID); !isNoSuchScanID(err) {
		t.Fatalf("CloseMultiScan after expiry error = %v, want NoSuchScanIDException", err)
	}
}

func TestMultiScanSessionUnknownIDReturnsNoSuchScanID(t *testing.T) {
	srv, err := NewServer(Options{Locator: &stubLocator{}, Storage: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ContinueMultiScan(context.Background(), nil, 77, 0); !isNoSuchScanID(err) {
		t.Fatalf("ContinueMultiScan error = %v, want NoSuchScanIDException", err)
	}
	if err := srv.CloseMultiScan(context.Background(), nil, 77); !isNoSuchScanID(err) {
		t.Fatalf("CloseMultiScan error = %v, want NoSuchScanIDException", err)
	}
}

func TestContinueMultiScan_CanceledRequestDoesNotConsumePage(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-cancel.rf"
	loc := newSessionTestData(t, mem, path, "1", []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	})
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)
	initial := startMultiScan(t, srv, multiBatch("1", rowRange("row01", "row04")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.ContinueMultiScan(ctx, nil, initial.ScanID, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ContinueMultiScan canceled error = %v, want context.Canceled", err)
	}
	got := collectMultiScan(t, srv, initial)
	if want := []string{"row01=value-01", "row02=value-02", "row03=value-03"}; !equalStrings(got, want) {
		t.Fatalf("retry after cancellation = %v, want %v", got, want)
	}
}

func TestStartMultiScan_SessionCapacityReturnsBusy(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-capacity.rf"
	cells := []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "value-01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "value-02", ts: 2},
		{row: "row03", cf: "cf", cq: "cq", value: "value-03", ts: 3},
	}
	loc := newSessionTestData(t, mem, path, "1", cells)
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

	initial := startMultiScan(t, srv, multiBatch("1", rowRange("row01", "row04")))
	if initial.ScanID == 0 || !initial.Result_.More {
		t.Fatalf("initial = %+v, want open continuation state", initial)
	}

	if _, err := startMultiScanErr(srv, multiBatch("1", rowRange("row01", "row04"))); !isScanServerBusy(err) {
		t.Fatalf("second StartMultiScan error = %v, want ScanServerBusyException", err)
	}
}

func TestMultiScanSessionsSupportConcurrentAccess(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multisession-concurrent.rf"
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
	batch := multiBatch("1", rowRange("row00", "row12"))

	const workers = 16
	errCh := make(chan error, workers)
	idCh := make(chan data.ScanID, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			initial, err := startMultiScanErr(srv, batch)
			if err != nil {
				errCh <- err
				return
			}
			if initial.ScanID == 0 || !initial.Result_.More {
				errCh <- fmt.Errorf("initial = %+v, want continuation state", initial)
				return
			}
			idCh <- initial.ScanID
			got, err := collectMultiScanErr(srv, initial)
			if err != nil {
				errCh <- err
				return
			}
			if !equalStrings(got, want) {
				errCh <- fmt.Errorf("collected pairs = %v, want %v", got, want)
				return
			}
			if err := srv.CloseMultiScan(context.Background(), nil, initial.ScanID); err != nil {
				errCh <- fmt.Errorf("CloseMultiScan(%d): %w", initial.ScanID, err)
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
	if got := srv.multiScans.count(); got != 0 {
		t.Fatalf("session count = %d, want 0", got)
	}
}

func TestSplitMultiScanResult_PreservesFailuresAndTailState(t *testing.T) {
	extent := &data.TKeyExtent{Table: []byte("1"), EndRow: []byte("row09")}
	failureRange := rowRange("row03", "row04")
	full := &data.MultiScanResult_{
		Results: []*data.TKeyValue{
			multiScanKV("row01", "value-01"),
			multiScanKV("row02", "value-02"),
			multiScanKV("row03", "value-03"),
		},
		Failures: data.ScanBatch{
			extent: []*data.TRange{failureRange},
		},
		FullScans: []*data.TKeyExtent{extent},
		PartScan:  extent,
		PartNextKey: &data.TKey{
			Row:       []byte("row03"),
			Timestamp: 1<<63 - 1,
		},
		PartNextKeyInclusive: true,
	}

	capBytes := approxKVSize(full.Results[0]) + approxKVSize(full.Results[1])

	page := splitMultiScanResult(full, capBytes)
	if !page.result.More {
		t.Fatal("initial page More=false, want continuation")
	}
	if page.result.PartScan != nil || page.result.PartNextKey != nil || page.result.PartNextKeyInclusive {
		t.Fatalf("initial page leaked terminal part state: %+v", page.result)
	}
	if len(page.result.Failures) != 1 || len(page.result.FullScans) != 0 {
		t.Fatalf("initial page metadata = %+v, want failures only", page.result)
	}

	reg := newMultiScanSessionRegistry(time.Minute, 1, 0)
	scanID, err := reg.create(time.Now(), page.remaining, page.tail)
	if err != nil {
		t.Fatal(err)
	}
	final, err := reg.continueMultiScan(time.Now(), scanID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if final.More {
		t.Fatalf("final More=%v, want false", final.More)
	}
	if got := pairsOf(final.Results); !equalStrings(got, []string{"row03=value-03"}) {
		t.Fatalf("final results = %v, want [row03=value-03]", got)
	}
	if final.PartScan == nil || string(final.PartScan.EndRow) != "row09" {
		t.Fatalf("final PartScan = %+v, want endRow row09", final.PartScan)
	}
	if final.PartNextKey == nil || string(final.PartNextKey.Row) != "row03" || !final.PartNextKeyInclusive {
		t.Fatalf("final part next key = %+v inclusive=%v", final.PartNextKey, final.PartNextKeyInclusive)
	}
	if len(final.Failures) != 0 || len(final.FullScans) != 1 {
		t.Fatalf("final page metadata = %+v, want terminal full scan only", final)
	}
}

func TestStartMultiScan_MultiTabletContinuationAndFailureRetry(t *testing.T) {
	mem := memory.New()
	pathA := "gs://test/multisession-tablet-a.rf"
	pathB := "gs://test/multisession-tablet-b.rf"
	writeRFileToMemory(t, mem, pathA, []cellSpec{
		{row: "row01", cf: "cf", cq: "cq", value: "a01", ts: 1},
		{row: "row02", cf: "cf", cq: "cq", value: "a02", ts: 2},
	})
	writeRFileToMemory(t, mem, pathB, []cellSpec{
		{row: "row11", cf: "cf", cq: "cq", value: "b11", ts: 1},
		{row: "row12", cf: "cf", cq: "cq", value: "b12", ts: 2},
	})
	extentA := &data.TKeyExtent{Table: []byte("1"), EndRow: []byte("row09")}
	extentB := &data.TKeyExtent{Table: []byte("1"), PrevEndRow: []byte("row09")}
	missing := &data.TKeyExtent{Table: []byte("2")}
	loc := &stubLocator{tablets: map[string][]metadata.TabletInfo{
		"1": {
			{TableID: "1", EndRow: []byte("row09"), Files: []metadata.FileEntry{{Path: pathA}}},
			{TableID: "1", PrevRow: []byte("row09"), Files: []metadata.FileEntry{{Path: pathB}}},
		},
	}}
	srv := newSessionTestServer(t, loc, mem, 1, time.Minute)
	batch := data.ScanBatch{
		extentA: {rowRange("row01", "row03")},
		extentB: {rowRange("row11", "row13")},
		missing: {rowRange("row20", "row21")},
	}

	initial := startMultiScan(t, srv, batch)
	if initial.ScanID == 0 || !initial.Result_.More {
		t.Fatalf("initial = %+v, want paged multi-tablet result", initial)
	}
	if len(initial.Result_.Failures) != 1 {
		t.Fatalf("failures = %v, want one retryable tablet", initial.Result_.Failures)
	}
	if len(initial.Result_.FullScans) != 0 {
		t.Fatalf("initial full scans = %d, want none before result drain", len(initial.Result_.FullScans))
	}
	got := append([]string(nil), pairsOf(initial.Result_.Results)...)
	var terminal *data.MultiScanResult_
	for more := initial.Result_.More; more; {
		next, err := srv.ContinueMultiScan(context.Background(), nil, initial.ScanID, 0)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, pairsOf(next.Results)...)
		terminal = next
		more = next.More
	}
	want := []string{"row01=a01", "row02=a02", "row11=b11", "row12=b12"}
	if !equalStrings(got, want) {
		t.Fatalf("continued results = %v, want %v", got, want)
	}
	if terminal == nil || len(terminal.FullScans) != 2 {
		t.Fatalf("terminal full scans = %+v, want 2", terminal)
	}

	const pathC = "gs://test/multisession-tablet-c.rf"
	writeRFileToMemory(t, mem, pathC, []cellSpec{
		{row: "row20", cf: "cf", cq: "cq", value: "c20", ts: 1},
	})
	loc.tablets["2"] = []metadata.TabletInfo{{TableID: "2", Files: []metadata.FileEntry{{Path: pathC}}}}
	retry := startMultiScan(t, srv, initial.Result_.Failures)
	if len(retry.Result_.Failures) != 0 {
		t.Fatalf("retry failures = %v, want none", retry.Result_.Failures)
	}
	if got := collectMultiScan(t, srv, retry); !equalStrings(got, []string{"row20=c20"}) {
		t.Fatalf("retry results = %v, want [row20=c20]", got)
	}
}

func startMultiScan(t *testing.T, srv *Server, batch data.ScanBatch) *data.InitialMultiScan {
	t.Helper()
	resp, err := startMultiScanErr(srv, batch)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func startMultiScanErr(srv *Server, batch data.ScanBatch) (*data.InitialMultiScan, error) {
	resp, err := srv.StartMultiScan(context.Background(), nil, nil,
		batch,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Result_ == nil {
		return nil, fmt.Errorf("StartMultiScan returned nil response: %+v", resp)
	}
	return resp, nil
}

func collectMultiScan(t *testing.T, srv *Server, initial *data.InitialMultiScan) []string {
	t.Helper()
	out, err := collectMultiScanErr(srv, initial)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func collectMultiScanErr(srv *Server, initial *data.InitialMultiScan) ([]string, error) {
	out := append([]string(nil), pairsOf(initial.Result_.Results)...)
	for more := initial.Result_.More; more; {
		resp, err := srv.ContinueMultiScan(context.Background(), nil, initial.ScanID, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, pairsOf(resp.Results)...)
		more = resp.More
	}
	return out, nil
}

func multiBatch(table string, ranges ...*data.TRange) data.ScanBatch {
	return data.ScanBatch{
		&data.TKeyExtent{Table: []byte(table)}: ranges,
	}
}

func multiScanKV(row, value string) *data.TKeyValue {
	return &data.TKeyValue{
		Key: &data.TKey{
			Row:       []byte(row),
			Timestamp: 1<<63 - 1,
		},
		Value: []byte(value),
	}
}

func isScanServerBusy(err error) bool {
	var target *tabletscan.ScanServerBusyException
	return errors.As(err, &target)
}
