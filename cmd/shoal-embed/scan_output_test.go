package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/parquetfile"
)

type sliceScanCursor struct {
	cells []struct {
		key   iterrt.Key
		value []byte
	}
	index      int
	advanceErr error
}

func (c *sliceScanCursor) Next() bool       { return c.index < len(c.cells) }
func (c *sliceScanCursor) Key() *iterrt.Key { return &c.cells[c.index].key }
func (c *sliceScanCursor) Value() []byte    { return c.cells[c.index].value }
func (c *sliceScanCursor) Advance() error {
	c.index++
	return c.advanceErr
}

func TestWriteScanParquetRoundTrip(t *testing.T) {
	cursor := &sliceScanCursor{cells: []struct {
		key   iterrt.Key
		value []byte
	}{
		{
			key: iterrt.Key{
				Row:              []byte{0x00, 0xff},
				ColumnFamily:     []byte("cf"),
				ColumnQualifier:  []byte("cq"),
				ColumnVisibility: []byte("private"),
				Timestamp:        42,
			},
			value: []byte{0xfe, 0x00},
		},
		{
			key:   iterrt.Key{Row: []byte("second"), Timestamp: 7},
			value: []byte("value"),
		},
	}}

	var buffer bytes.Buffer
	output, err := newScanOutput("parquet", &buffer)
	if err != nil {
		t.Fatalf("newScanOutput: %v", err)
	}
	count, err := writeScan(cursor, output, 0)
	if err != nil {
		t.Fatalf("writeScan: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	rows, err := parquet.Read[parquetfile.Cell](bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	if err != nil {
		t.Fatalf("read parquet: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if !bytes.Equal(rows[0].Row, []byte{0x00, 0xff}) ||
		!bytes.Equal(rows[0].CF, []byte("cf")) ||
		!bytes.Equal(rows[0].CQ, []byte("cq")) ||
		!bytes.Equal(rows[0].CV, []byte("private")) ||
		rows[0].Timestamp != 42 ||
		!bytes.Equal(rows[0].Value, []byte{0xfe, 0x00}) {
		t.Fatalf("first row = %+v", rows[0])
	}
	if !bytes.Equal(rows[1].Row, []byte("second")) ||
		rows[1].Timestamp != 7 ||
		!bytes.Equal(rows[1].Value, []byte("value")) {
		t.Fatalf("second row = %+v", rows[1])
	}
}

func TestWriteScanHonorsLimit(t *testing.T) {
	cursor := &sliceScanCursor{cells: []struct {
		key   iterrt.Key
		value []byte
	}{
		{key: iterrt.Key{Row: []byte("one")}},
		{key: iterrt.Key{Row: []byte("two")}},
	}}
	var buffer bytes.Buffer
	output, err := newScanOutput("json", &buffer)
	if err != nil {
		t.Fatalf("newScanOutput: %v", err)
	}
	count, err := writeScan(cursor, output, 1)
	if err != nil {
		t.Fatalf("writeScan: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if got, want := buffer.String(), "{\"row\":\"one\",\"cf\":\"\",\"cq\":\"\",\"ts\":0,\"value\":\"\"}\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestWriteScanReportsAdvanceError(t *testing.T) {
	cursor := &sliceScanCursor{
		cells: []struct {
			key   iterrt.Key
			value []byte
		}{{key: iterrt.Key{Row: []byte("one")}}},
		advanceErr: errors.New("failed"),
	}
	var buffer bytes.Buffer
	output, err := newScanOutput("parquet", &buffer)
	if err != nil {
		t.Fatalf("newScanOutput: %v", err)
	}
	if _, err := writeScan(cursor, output, 0); err == nil {
		t.Fatal("writeScan succeeded, want advance error")
	}
}

func TestNewScanOutputRejectsUnknownFormat(t *testing.T) {
	if _, err := newScanOutput("csv", &bytes.Buffer{}); err == nil {
		t.Fatal("newScanOutput succeeded, want error")
	}
}
