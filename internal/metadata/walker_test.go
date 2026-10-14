package metadata

import (
	"bytes"
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/zk"
)

func TestRootTabletExtent(t *testing.T) {
	e := rootTabletExtent()
	if !bytes.Equal(e.Table, []byte(RootTableID)) {
		t.Errorf("Table = %q, want %q", e.Table, RootTableID)
	}
	if e.EndRow != nil {
		t.Errorf("EndRow = %v, want nil", e.EndRow)
	}
	if e.PrevEndRow != nil {
		t.Errorf("PrevEndRow = %v, want nil", e.PrevEndRow)
	}
}

func TestTabletExtent(t *testing.T) {
	in := TabletInfo{
		TableID: "2k",
		EndRow:  []byte("k"),
		PrevRow: nil,
	}
	e := tabletExtent(in)
	if !bytes.Equal(e.Table, []byte("2k")) {
		t.Errorf("Table = %q", e.Table)
	}
	if !bytes.Equal(e.EndRow, []byte("k")) {
		t.Errorf("EndRow = %q", e.EndRow)
	}
	if e.PrevEndRow != nil {
		t.Errorf("PrevEndRow = %v, want nil", e.PrevEndRow)
	}
}

func TestTabletExtent_LastTablet(t *testing.T) {
	in := TabletInfo{TableID: "2k", EndRow: nil, PrevRow: []byte("p")}
	e := tabletExtent(in)
	if e.EndRow != nil {
		t.Errorf("EndRow = %v, want nil for default tablet", e.EndRow)
	}
	if !bytes.Equal(e.PrevEndRow, []byte("p")) {
		t.Errorf("PrevEndRow = %q", e.PrevEndRow)
	}
}

func TestFullRange(t *testing.T) {
	r := fullRange()
	if !r.InfiniteStartKey || !r.InfiniteStopKey {
		t.Errorf("expected both ends infinite, got %+v", r)
	}
	if r.Start != nil || r.Stop != nil {
		t.Errorf("expected nil Start/Stop, got %+v / %+v", r.Start, r.Stop)
	}
}

type fakeWalkerLocator struct{}

func (fakeWalkerLocator) InstanceID() string { return "uuid-1" }
func (fakeWalkerLocator) RootTabletLocation(context.Context) (*zk.Location, error) {
	return &zk.Location{HostPort: "root:9997", Session: "abc"}, nil
}

type fakeWalkerLifecycle struct {
	address string
	starts  int
	closes  int
	results []*data.TKeyValue
}

func (f *fakeWalkerLifecycle) Start(
	_ context.Context,
	address string,
	req scanclient.StartRequest,
) (*data.InitialScan, error) {
	f.address = address
	f.starts++
	if string(req.Extent.Table) != RootTableID {
		return nil, context.Canceled
	}
	return &data.InitialScan{
		ScanID: 7, Result_: &data.ScanResult_{Results: f.results},
	}, nil
}

func TestLocateMetadataTableDoesNotRequireHostedMetadataTablets(t *testing.T) {
	lifecycle := &fakeWalkerLifecycle{results: []*data.TKeyValue{{
		Key: &data.TKey{
			Row: []byte(MetadataTableID + "<"), ColFamily: []byte(CFTabletSection),
			ColQualifier: []byte(CQPrevRow), Timestamp: 1,
		},
		Value: []byte{0},
	}}}
	walker := NewWalkerWithLifecycle(fakeWalkerLocator{}, lifecycle)
	tablets, err := walker.LocateTable(context.Background(), MetadataTableID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tablets) != 1 || tablets[0].TableID != MetadataTableID ||
		tablets[0].Location != nil {
		t.Fatalf("metadata tablets = %#v", tablets)
	}
	if lifecycle.starts != 1 {
		t.Fatalf("scan starts = %d, want root scan only", lifecycle.starts)
	}
}

func (*fakeWalkerLifecycle) Continue(context.Context, string, data.ScanID, int64) (*data.ScanResult_, error) {
	return nil, nil
}

func (f *fakeWalkerLifecycle) CloseScan(context.Context, string, data.ScanID) error {
	f.closes++
	return nil
}

func TestWalkerUsesInjectedLocatorAndLifecycle(t *testing.T) {
	lifecycle := &fakeWalkerLifecycle{}
	walker := NewWalkerWithLifecycle(fakeWalkerLocator{}, lifecycle)
	tablets, err := walker.ScanRootTablet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tablets) != 0 {
		t.Fatalf("tablets = %v, want empty", tablets)
	}
	if lifecycle.address != "root:9997" || lifecycle.starts != 1 || lifecycle.closes != 1 {
		t.Fatalf("lifecycle address=%q starts=%d closes=%d", lifecycle.address, lifecycle.starts, lifecycle.closes)
	}
}
