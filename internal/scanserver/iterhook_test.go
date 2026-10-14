package scanserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/ivfpq"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// TestStartScan_WithIvfPqIterator end-to-end checks the iterator hook:
// write 5 PQ-encoded cells into an RFile, run StartScan with the IVF
// iterator config (top-K=2), confirm the response contains exactly the
// top-2 by score in score-descending order.
func TestStartScan_WithIvfPqIterator(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/ivf.rf"

	// Tiny PQ: M=2 subspaces, Ks=4, dim=4, dsub=2.
	// Centroid magnitudes are picked so every (s0,s1) code yields a
	// distinct score against query=[1,1,1,1] — avoids top-K ties
	// destabilizing the test.
	codebook := [][][]float32{
		{ // subspace 0
			{0, 0}, {1, 0}, {2, 0}, {3, 0},
		},
		{ // subspace 1
			{0, 0}, {0, 1}, {0, 2}, {0, 3},
		},
	}
	pq := mustPQ(t, codebook, 4, 1)
	var pqBuf bytes.Buffer
	if _, err := pq.WriteTo(&pqBuf); err != nil {
		t.Fatal(err)
	}

	// Query — boring all-1s. The score for code (s0,s1) is sum of
	// dot(query[0:2], codebook[0][s0]) + dot(query[2:4], codebook[1][s1]).
	query := []float32{1, 1, 1, 1}
	queryBuf := make([]byte, len(query)*4)
	for i, f := range query {
		binary.BigEndian.PutUint32(queryBuf[i*4:], math.Float32bits(f))
	}

	// 5 cells, each row carrying a 2-byte PQ code as its Value. The
	// rfile.Writer requires sorted rows; pre-sorted by hex prefix.
	cells := []cellSpec{
		{row: "00000001:vA", cf: "V", cq: "_pq", value: string([]byte{0, 0}), ts: 1},
		{row: "00000001:vB", cf: "V", cq: "_pq", value: string([]byte{1, 1}), ts: 2},
		{row: "00000001:vC", cf: "V", cq: "_pq", value: string([]byte{2, 2}), ts: 3},
		{row: "00000001:vD", cf: "V", cq: "_pq", value: string([]byte{3, 3}), ts: 4},
		{row: "00000001:vE", cf: "V", cq: "_pq", value: string([]byte{0, 1}), ts: 5},
	}
	writeRFileToMemory(t, mem, path, cells)

	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"3": {{TableID: "3", Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
	srv, err := NewServer(Options{
		Locator:    loc,
		BlockCache: cache.NewBlockCache(1 << 20),
		Storage:    mem,
	})
	if err != nil {
		t.Fatal(err)
	}

	ssiList := []*data.IterInfo{
		{Priority: 42, ClassName: ivfpq.IteratorClassName, IterName: "ivfPqAdc"},
	}
	ssio := map[string]map[string]string{
		"ivfPqAdc": {
			ivfpq.OptQuery:      base64.StdEncoding.EncodeToString(queryBuf),
			ivfpq.OptPQCodebook: base64.StdEncoding.EncodeToString(pqBuf.Bytes()),
			ivfpq.OptTopK:       "2",
			ivfpq.OptThreshold:  "-1e30",
		},
	}

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("3")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, ssiList, ssio, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Result_.Results); got != 2 {
		t.Fatalf("top-K=2 expected 2 cells, got %d", got)
	}
	// Compute expected scores manually so we know which 2 should win.
	tbl, _ := pq.InnerProductTable(query)
	type scored struct {
		row   string
		score float32
	}
	expected := []scored{}
	for _, c := range cells {
		expected = append(expected, scored{c.row, pq.Dot([]byte(c.value), tbl)})
	}
	// Sort desc.
	for i := range expected {
		for j := i + 1; j < len(expected); j++ {
			if expected[j].score > expected[i].score {
				expected[i], expected[j] = expected[j], expected[i]
			}
		}
	}
	want := map[string]bool{expected[0].row: true, expected[1].row: true}
	for _, r := range resp.Result_.Results {
		row := string(r.Key.Row)
		if !want[row] {
			t.Errorf("unexpected row %q in top-2; expected one of %v", row, want)
		}
		// Verify Value carries the score as 4-byte big-endian float32.
		if len(r.Value) != 4 {
			t.Errorf("expected 4-byte score; got %d bytes for row %q", len(r.Value), row)
		}
	}
	// Output must be in score-descending order.
	s0 := math.Float32frombits(binary.BigEndian.Uint32(resp.Result_.Results[0].Value))
	s1 := math.Float32frombits(binary.BigEndian.Uint32(resp.Result_.Results[1].Value))
	if s1 > s0 {
		t.Errorf("results not score-descending: %f then %f", s0, s1)
	}
}

// TestStartScan_UnknownIteratorErrors confirms shoal refuses to silently
// drop unknown server-side iterators (would produce wrong answers).
func TestStartScan_UnknownIteratorErrors(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/un.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r1", cf: "cf", cq: "cq", value: "v1", ts: 1},
	})
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	_, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0,
		[]*data.IterInfo{{Priority: 10, ClassName: "org.apache.accumulo.core.iterators.user.NoSuchIterator", IterName: "x"}},
		nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err == nil {
		t.Errorf("expected error for unknown iterator class")
	}
}

func TestWholeRowProcessorUsesAccumuloEncoding(t *testing.T) {
	processor := &wholeRowProcessor{}
	processor.offer(&wire.Key{
		Row: []byte("r1"), ColumnFamily: []byte("cf"),
		ColumnQualifier: []byte("cq1"), Timestamp: 7,
	}, []byte("v1"))
	processor.offer(&wire.Key{
		Row: []byte("r1"), ColumnFamily: []byte("cf"),
		ColumnQualifier: []byte("cq2"), ColumnVisibility: []byte("A"), Timestamp: 6,
	}, []byte("v2"))
	processor.offer(&wire.Key{Row: []byte("r2"), Timestamp: 5}, []byte("v3"))

	got := processor.drain()
	if len(got) != 2 || string(got[0].Key.Row) != "r1" ||
		got[0].Key.Timestamp != math.MaxInt64 || string(got[1].Key.Row) != "r2" {
		t.Fatalf("drain = %#v", got)
	}
	in := bytes.NewReader(got[0].Value)
	var count int32
	if err := binary.Read(in, binary.BigEndian, &count); err != nil || count != 2 {
		t.Fatalf("encoded count = %d, %v", count, err)
	}
	readField := func() []byte {
		t.Helper()
		var length int32
		if err := binary.Read(in, binary.BigEndian, &length); err != nil {
			t.Fatal(err)
		}
		value := make([]byte, length)
		if _, err := in.Read(value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if string(readField()) != "cf" || string(readField()) != "cq1" ||
		len(readField()) != 0 {
		t.Fatal("first encoded key fields do not match")
	}
	var timestamp int64
	if err := binary.Read(in, binary.BigEndian, &timestamp); err != nil ||
		timestamp != 7 || string(readField()) != "v1" {
		t.Fatal("first encoded timestamp/value do not match")
	}
}

func TestGrepProcessorMatchesConfiguredFields(t *testing.T) {
	processor, err := buildPostProcessor(
		[]*data.IterInfo{{
			Priority: 40, ClassName: grepIteratorClassName, IterName: "grep",
		}},
		map[string]map[string]string{"grep": {
			"term": "table-7", "matchRow": "false", "matchColumnFamily": "false",
			"matchColumnQualifier": "false", "matchValue": "true",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	processor.offer(&wire.Key{
		Row: []byte("table-7-row"), ColumnFamily: []byte("cf"),
		ColumnQualifier: []byte("cq"), Timestamp: 2,
	}, []byte("other"))
	processor.offer(&wire.Key{
		Row: []byte("other-row"), ColumnFamily: []byte("cf"),
		ColumnQualifier: []byte("cq"), Timestamp: 1,
	}, []byte("hdfs://accumulo/tables/table-7/F.rf"))

	got := processor.drain()
	if len(got) != 1 || string(got[0].Key.Row) != "other-row" {
		t.Fatalf("drain = %#v, want value match only", got)
	}
}

func TestGrepProcessorRequiresTerm(t *testing.T) {
	_, err := buildPostProcessor(
		[]*data.IterInfo{{
			Priority: 40, ClassName: grepIteratorClassName, IterName: "grep",
		}},
		map[string]map[string]string{"grep": {}},
	)
	if err == nil {
		t.Fatal("expected missing term error")
	}
}

func TestWholeRowProcessorEnforcesMaxBufferSize(t *testing.T) {
	processor, err := buildPostProcessor(
		[]*data.IterInfo{{
			Priority: 10, ClassName: wholeRowIteratorClassName, IterName: "whole",
		}},
		map[string]map[string]string{"whole": {"maxBufferSize": "128B"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	processor.offer(&wire.Key{
		Row: []byte("row"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
	}, []byte("value"))
	if processor.err() == nil {
		t.Fatal("expected row buffer overflow")
	}
}

func TestRowFateStatusFilter(t *testing.T) {
	processor, err := buildPostProcessor(
		[]*data.IterInfo{{
			Priority: 100, ClassName: rowFateStatusFilterClassName, IterName: "statuses",
		}},
		map[string]map[string]string{"statuses": {"statuses": "NEW,IN_PROGRESS"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	processor.offer(&wire.Key{
		Row: []byte("accepted"), ColumnFamily: []byte("txadmin"),
		ColumnQualifier: []byte("status"), Timestamp: 2,
	}, []byte("IN_PROGRESS"))
	processor.offer(&wire.Key{
		Row: []byte("accepted"), ColumnFamily: []byte("tx"),
		ColumnQualifier: []byte("ctime"), Timestamp: 1,
	}, []byte("1"))
	processor.offer(&wire.Key{
		Row: []byte("rejected"), ColumnFamily: []byte("txadmin"),
		ColumnQualifier: []byte("status"), Timestamp: 1,
	}, []byte("SUCCESSFUL"))

	got := processor.drain()
	if len(got) != 1 || string(got[0].Key.Row) != "accepted" {
		t.Fatalf("drain = %#v, want only accepted row", got)
	}
}

func TestWholeRowProcessorParsesAccumuloMemory(t *testing.T) {
	for input, want := range map[string]int64{
		"1": 1, "2B": 2, "3k": 3 << 10, "4M": 4 << 20, "5g": 5 << 30,
	} {
		got, err := parseAccumuloMemory(input)
		if err != nil || got != want {
			t.Fatalf("parseAccumuloMemory(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1", " 1K", "9223372036854775807G"} {
		if _, err := parseAccumuloMemory(input); err == nil {
			t.Fatalf("parseAccumuloMemory(%q) unexpectedly succeeded", input)
		}
	}
}

// mustPQ wraps a codebook into a VectorPQ via the package's exported
// FromBytes round-trip so tests don't depend on private fields. (It
// also confirms the WriteTo/FromBytes pair is internally consistent
// from the consumer's perspective.)
func mustPQ(t *testing.T, codebook [][][]float32, dim int, version int32) *ivfpq.VectorPQ {
	t.Helper()
	// Use ivfpq's own writer + reader via a tiny shim: WriteTo a
	// constructed PQ struct → bytes → FromBytes. We don't have a
	// public constructor; round-trip via the bytes API.
	tmp := buildAndWrite(t, codebook, dim, version)
	pq, err := ivfpq.FromBytes(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return pq
}

// buildAndWrite serializes a codebook directly in Java's wire format
// (since we don't expose an ivfpq constructor for tests in this
// package). Header layout matches VectorPQ.writeTo exactly.
func buildAndWrite(t *testing.T, codebook [][][]float32, dim int, version int32) []byte {
	t.Helper()
	m := int32(len(codebook))
	ks := int32(len(codebook[0]))
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, ivfpq.FormatVersion)
	binary.Write(&buf, binary.BigEndian, version)
	binary.Write(&buf, binary.BigEndian, m)
	binary.Write(&buf, binary.BigEndian, ks)
	binary.Write(&buf, binary.BigEndian, int32(dim))
	for s := 0; s < int(m); s++ {
		for c := 0; c < int(ks); c++ {
			for _, f := range codebook[s][c] {
				binary.Write(&buf, binary.BigEndian, f)
			}
		}
	}
	return buf.Bytes()
}
