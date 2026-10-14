package metadata

import (
	"bytes"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

func kv(row, cf, cq, value string) *data.TKeyValue {
	return &data.TKeyValue{
		Key: &data.TKey{
			Row:          []byte(row),
			ColFamily:    []byte(cf),
			ColQualifier: []byte(cq),
		},
		Value: []byte(value),
	}
}

func TestAggregateRows_ScanRootYieldsMetadataTablets(t *testing.T) {
	// Scanning the root tablet returns rows for the METADATA table's own
	// tablets — row keys are "!0;<endrow>" or "!0<".
	row := "!0<" // single metadata-table tablet: the default/last one
	kvs := []*data.TKeyValue{
		kv(row, CFFile, `{"path":"gs://b/!0/A0001.rf","startRow":"","endRow":""}`, "10240,500"),
		kv(row, CFFile, `{"path":"gs://b/!0/A0002.rf","startRow":"","endRow":""}`, "8192,400,1700000000000"),
		kv(row, CFCurrentLocation, "/lock-path/n$abcd", "tserver-1:9997"),
		kv(row, CFServer, CQDirectory, "default_tablet"),
		kv(row, CFServer, CQTime, "M0"),
		kv(row, CFLog, "-/file:///wal/tserver-1+9997/11111111-1111-1111-1111-111111111111", ""),
		kv(row, CFTabletSection, CQPrevRow, "\x00"),
	}
	out, err := AggregateRows(kvs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d tablets, want 1", len(out))
	}
	got := out[0]
	if got.TableID != "!0" {
		t.Errorf("TableID = %q, want %q", got.TableID, "!0")
	}
	if got.EndRow != nil {
		t.Errorf("EndRow = %v, want nil", got.EndRow)
	}
	if got.PrevRow != nil {
		t.Errorf("PrevRow = %v, want nil for first/only tablet", got.PrevRow)
	}
	if !got.PrevRowSet || got.Directory != "default_tablet" || got.Time != "M0" {
		t.Errorf("required metadata = %+v", got)
	}
	if len(got.Logs) != 1 || got.Logs[0].Server != "tserver-1:9997" {
		t.Errorf("Logs = %+v", got.Logs)
	}
	if got.Location == nil || got.Location.HostPort != "tserver-1:9997" {
		t.Errorf("Location = %+v", got.Location)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(got.Files))
	}
	if got.Files[1].Time != 1700000000000 {
		t.Errorf("Files[1].Time = %d", got.Files[1].Time)
	}
}

func TestAggregateRows_ScanMetadataYieldsUserTabletsForOneTable(t *testing.T) {
	// Scanning a metadata tablet returns rows for user-table tablets —
	// row keys are "<tableId>;<endrow>" or "<tableId><". Here: three
	// tablets covering user table "2k" at boundaries (-inf,"k"], ("k","p"],
	// ("p", +inf).
	kvs := []*data.TKeyValue{
		kv("2k;k", CFFile, `{"path":"gs://b/2k/A.rf","startRow":"","endRow":""}`, "100,10"),
		kv("2k;k", CFCurrentLocation, "/p/n$1", "tserver-1:9997"),
		kv("2k;k", CFTabletSection, CQPrevRow, "\x00"),

		kv("2k;p", CFFile, `{"path":"gs://b/2k/B.rf","startRow":"","endRow":""}`, "200,20"),
		kv("2k;p", CFCurrentLocation, "/p/n$2", "tserver-2:9997"),
		kv("2k;p", CFTabletSection, CQPrevRow, "\x01k"),

		kv("2k<", CFFile, `{"path":"gs://b/2k/C.rf","startRow":"","endRow":""}`, "300,30"),
		kv("2k<", CFCurrentLocation, "/p/n$3", "tserver-3:9997"),
		kv("2k<", CFTabletSection, CQPrevRow, "\x01p"),
	}
	out, err := AggregateRows(kvs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d tablets, want 3", len(out))
	}
	for i, got := range out {
		if got.TableID != "2k" {
			t.Errorf("tablet %d TableID = %q", i, got.TableID)
		}
	}
	if !bytes.Equal(out[0].EndRow, []byte("k")) || out[0].PrevRow != nil {
		t.Errorf("tablet 0: end=%q prev=%v", out[0].EndRow, out[0].PrevRow)
	}
	if !bytes.Equal(out[1].EndRow, []byte("p")) || !bytes.Equal(out[1].PrevRow, []byte("k")) {
		t.Errorf("tablet 1: end=%q prev=%q", out[1].EndRow, out[1].PrevRow)
	}
	if out[2].EndRow != nil || !bytes.Equal(out[2].PrevRow, []byte("p")) {
		t.Errorf("tablet 2: end=%v prev=%q", out[2].EndRow, out[2].PrevRow)
	}
}

func TestAggregateRows_CapturesFutureLocation(t *testing.T) {
	// During tablet assignment, future carries the generation the loader
	// must fence against before the tablet is promoted to loc.
	kvs := []*data.TKeyValue{
		kv("2k;k", CFFile, `{"path":"gs://x/A.rf","startRow":"","endRow":""}`, "1,1"),
		kv("2k;k", CFFutureLocation, "/p/n$1", "tserver-future:9997"),
		kv("2k;k", CFTabletSection, CQPrevRow, "\x00"),
	}
	out, err := AggregateRows(kvs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d", len(out))
	}
	if out[0].Location != nil {
		t.Errorf("expected nil Location, got %+v", out[0].Location)
	}
	if len(out[0].Files) != 1 {
		t.Errorf("Files = %d, want 1", len(out[0].Files))
	}
	if out[0].FutureLocation == nil || out[0].FutureLocation.HostPort != "tserver-future:9997" {
		t.Errorf("FutureLocation = %+v", out[0].FutureLocation)
	}
}

func TestAggregateRows_RejectsMultipleLoc(t *testing.T) {
	kvs := []*data.TKeyValue{
		kv("2k;k", CFCurrentLocation, "/p/n$1", "tserver-1:9997"),
		kv("2k;k", CFCurrentLocation, "/p/n$2", "tserver-2:9997"),
	}
	_, err := AggregateRows(kvs)
	if err == nil {
		t.Fatal("expected error on multiple loc entries")
	}
}

func TestAggregateRows_RejectsNilKey(t *testing.T) {
	_, err := AggregateRows([]*data.TKeyValue{{Key: nil, Value: []byte("v")}})
	if err == nil {
		t.Fatal("expected error on nil key")
	}
}

func TestAggregateRows_EmptyInput(t *testing.T) {
	out, err := AggregateRows(nil)
	if err != nil || out != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", out, err)
	}
}

func TestAggregateRows_PropagatesBadFile(t *testing.T) {
	kvs := []*data.TKeyValue{
		kv("2k;k", CFFile, `{"path":"gs://x/A.rf","startRow":"","endRow":""}`, "garbage"),
	}
	_, err := AggregateRows(kvs)
	if err == nil {
		t.Fatal("expected DataFileValue parse error")
	}
}

func TestDecodePrevRow(t *testing.T) {
	// Java encoding (MetadataSchema.encodePrevEndRow):
	//   null prev → [0x00]
	//   present prev (any bytes, incl empty) → [0x01, ...bytes]
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"absent sentinel", []byte{0x00}, nil},
		{"present empty", []byte{0x01}, []byte{}},
		{"present with bytes", []byte{0x01, 'k', 'e', 'y'}, []byte("key")},
		{"present with tilde (default tablet prev)", []byte{0x01, '~'}, []byte("~")},
		{"empty input", nil, nil},
		// Per Java: any non-zero first byte means present. Unknown
		// non-zero markers decode the rest as the prev row.
		{"non-zero non-1 marker", []byte{0x42, 'a'}, []byte("a")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodePrevRow(c.in)
			if !bytes.Equal(got, c.want) {
				t.Errorf("got %v want %v", got, c.want)
			}
			// distinguish nil from empty slice
			if (got == nil) != (c.want == nil) {
				t.Errorf("nil-ness mismatch: got nil=%v want nil=%v", got == nil, c.want == nil)
			}
		})
	}
}
