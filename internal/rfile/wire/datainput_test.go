package wire

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestReadInt32(t *testing.T) {
	// Big-endian: 0x12345678
	got, err := ReadInt32(bytes.NewReader([]byte{0x12, 0x34, 0x56, 0x78}))
	if err != nil {
		t.Fatal(err)
	}
	if got != 0x12345678 {
		t.Errorf("got %#x", got)
	}
}

func TestInt32Roundtrip(t *testing.T) {
	cases := []int32{0, 1, -1, math.MaxInt32, math.MinInt32, 0x7eadbeef, -0x12345678}
	for _, v := range cases {
		var buf bytes.Buffer
		if err := WriteInt32(&buf, v); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 4 {
			t.Errorf("WriteInt32(%d) wrote %d bytes, want 4", v, buf.Len())
		}
		got, err := ReadInt32(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Errorf("got %d, want %d", got, v)
		}
	}
}

func TestInt64Roundtrip(t *testing.T) {
	cases := []int64{0, 1, -1, math.MaxInt64, math.MinInt64, 1 << 40}
	for _, v := range cases {
		var buf bytes.Buffer
		if err := WriteInt64(&buf, v); err != nil {
			t.Fatal(err)
		}
		got, err := ReadInt64(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Errorf("got %d, want %d", got, v)
		}
	}
}

func TestBoolRoundtrip(t *testing.T) {
	for _, v := range []bool{false, true} {
		var buf bytes.Buffer
		if err := WriteBool(&buf, v); err != nil {
			t.Fatal(err)
		}
		got, err := ReadBool(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Errorf("got %v, want %v", got, v)
		}
	}

	// Any non-zero byte = true (Java's readBoolean accepts that).
	got, err := ReadBool(bytes.NewReader([]byte{0xff}))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Errorf("0xff should decode true")
	}
}

func TestUTFRoundtrip(t *testing.T) {
	cases := []string{
		"",
		"default",
		"locality-group-name",
		strings.Repeat("a", 1000),
	}
	for _, s := range cases {
		var buf bytes.Buffer
		if err := WriteUTF(&buf, s); err != nil {
			t.Fatal(err)
		}
		got, err := ReadUTF(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != s {
			t.Errorf("got %q (len %d), want %q (len %d)", got, len(got), s, len(s))
		}
	}
}

func TestUTFTooLong(t *testing.T) {
	huge := strings.Repeat("x", 0x10000) // 1 byte over uint16 max
	if err := WriteUTF(&bytes.Buffer{}, huge); err == nil {
		t.Errorf("expected error for too-long string")
	}
}

func TestKeyRoundtrip(t *testing.T) {
	cases := []*Key{
		{Row: []byte("row-1"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
			ColumnVisibility: []byte(""), Timestamp: 12345, Deleted: false},
		{Row: []byte{}, ColumnFamily: []byte{}, ColumnQualifier: []byte{}, ColumnVisibility: []byte{},
			Timestamp: 0, Deleted: false},
		{Row: []byte("\x00\x01\x02"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
			ColumnVisibility: []byte("CONFIDENTIAL"), Timestamp: math.MaxInt64, Deleted: true},
		{Row: bytes.Repeat([]byte{0xff}, 1000), ColumnFamily: nil, ColumnQualifier: nil,
			ColumnVisibility: nil, Timestamp: -1, Deleted: false},
	}
	for i, k := range cases {
		var buf bytes.Buffer
		if err := WriteKey(&buf, k); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got, err := ReadKey(&buf)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		// Empty / nil distinction: writer collapses nil → 0-length, reader
		// reconstructs []byte{}. So compare with bytes.Equal which treats
		// nil and empty equivalently.
		if !bytes.Equal(got.Row, k.Row) ||
			!bytes.Equal(got.ColumnFamily, k.ColumnFamily) ||
			!bytes.Equal(got.ColumnQualifier, k.ColumnQualifier) ||
			!bytes.Equal(got.ColumnVisibility, k.ColumnVisibility) ||
			got.Timestamp != k.Timestamp ||
			got.Deleted != k.Deleted {
			t.Errorf("case %d: roundtrip mismatch\n  got: %+v\n  want: %+v", i, got, k)
		}
	}
}

func TestReadKeyTruncated(t *testing.T) {
	// Write a Key, then truncate at every byte position; each must fail.
	original := &Key{Row: []byte("foo"), ColumnFamily: []byte("bar"), Timestamp: 1, Deleted: true}
	var buf bytes.Buffer
	if err := WriteKey(&buf, original); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	for i := 0; i < len(full); i++ {
		_, err := ReadKey(bytes.NewReader(full[:i]))
		if err == nil {
			t.Errorf("ReadKey on %d-byte truncation succeeded, expected error", i)
		}
	}
	// Full bytes succeed.
	if _, err := ReadKey(bytes.NewReader(full)); err != nil {
		t.Errorf("ReadKey on full bytes: %v", err)
	}
}
