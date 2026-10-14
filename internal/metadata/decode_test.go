package metadata

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeLogEntry(t *testing.T) {
	const qualifier = "-/file:///accumulo/wal/host.example+9997/11111111-1111-1111-1111-111111111111"
	got, err := DecodeLogEntry([]byte(qualifier))
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != "11111111-1111-1111-1111-111111111111" ||
		got.Server != "host.example:9997" ||
		got.Path != "file:///accumulo/wal/host.example+9997/11111111-1111-1111-1111-111111111111" {
		t.Fatalf("decoded log = %+v", got)
	}
	got.RawQualifier[0] = 'x'
	if qualifier[0] != '-' {
		t.Fatal("test qualifier unexpectedly mutable")
	}
}

func TestDecodeLogEntryRejectsMalformed(t *testing.T) {
	for _, input := range []string{"", "no-slash", "-/short", "-/x/host:9/11111111-1111-1111-1111-111111111111", "-/x/host+9/not-a-uuid"} {
		t.Run(input, func(t *testing.T) {
			if _, err := DecodeLogEntry([]byte(input)); err == nil {
				t.Fatalf("DecodeLogEntry(%q) succeeded", input)
			}
		})
	}
}

func TestDecodeTabletRow_DefaultTablet(t *testing.T) {
	tableID, endRow, err := DecodeTabletRow([]byte("+r<"))
	if err != nil {
		t.Fatal(err)
	}
	if tableID != "+r" {
		t.Errorf("tableID = %q, want %q", tableID, "+r")
	}
	if endRow != nil {
		t.Errorf("endRow = %v, want nil", endRow)
	}
}

func TestDecodeTabletRow_IntermediateTablet(t *testing.T) {
	tableID, endRow, err := DecodeTabletRow([]byte("!0;abc"))
	if err != nil {
		t.Fatal(err)
	}
	if tableID != "!0" {
		t.Errorf("tableID = %q, want %q", tableID, "!0")
	}
	if !bytes.Equal(endRow, []byte("abc")) {
		t.Errorf("endRow = %q, want %q", endRow, "abc")
	}
}

func TestDecodeTabletRow_UserTable(t *testing.T) {
	tableID, endRow, err := DecodeTabletRow([]byte("2k;some/key/here"))
	if err != nil {
		t.Fatal(err)
	}
	if tableID != "2k" {
		t.Errorf("tableID = %q", tableID)
	}
	if !bytes.Equal(endRow, []byte("some/key/here")) {
		t.Errorf("endRow = %q", endRow)
	}
}

func TestDecodeTabletRow_Errors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "empty"},
		{";abc", "empty tableID"},
		{"<", "empty tableID"},
		{"+r", "missing ';' or '<'"},
		{"+r;", "';' without endRow"},
		{"+r<extra", "'<' must be last byte"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, _, err := DecodeTabletRow([]byte(c.in))
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestDecodeDataFileValue_TwoFields(t *testing.T) {
	size, ne, time, err := DecodeDataFileValue([]byte("12345,678"))
	if err != nil {
		t.Fatal(err)
	}
	if size != 12345 || ne != 678 || time != -1 {
		t.Errorf("got size=%d ne=%d time=%d, want 12345 678 -1", size, ne, time)
	}
}

func TestDecodeDataFileValue_ThreeFields(t *testing.T) {
	size, ne, time, err := DecodeDataFileValue([]byte("100,5,1672531200000"))
	if err != nil {
		t.Fatal(err)
	}
	if size != 100 || ne != 5 || time != 1672531200000 {
		t.Errorf("got size=%d ne=%d time=%d", size, ne, time)
	}
}

func TestDecodeDataFileValue_Errors(t *testing.T) {
	cases := []string{
		"",
		"only-one-field",
		"1,2,3,4",
		"abc,123",
		"123,xyz",
		"123,456,bad-time",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, _, _, err := DecodeDataFileValue([]byte(c))
			if err == nil {
				t.Errorf("expected error for %q", c)
			}
		})
	}
}

func TestDecodeLockID_Happy(t *testing.T) {
	in := []byte("/accumulo/abc/tservers/tserver-3:9997/lock-0000000123$deadbeef")
	got, err := DecodeLockID(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/accumulo/abc/tservers/tserver-3:9997" {
		t.Errorf("Path = %q", got.Path)
	}
	if got.Node != "lock-0000000123" {
		t.Errorf("Node = %q", got.Node)
	}
	if got.EID != 0xdeadbeef {
		t.Errorf("EID = 0x%x, want 0xdeadbeef", got.EID)
	}
}

func TestDecodeLockID_LargeUnsignedEID(t *testing.T) {
	// Signed 64-bit overflow value — Java uses parseUnsignedLong, so
	// "ffffffffffffffff" is valid and equals math.MaxUint64.
	got, err := DecodeLockID([]byte("/p/n$ffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	if got.EID != ^uint64(0) {
		t.Errorf("EID = 0x%x, want max uint64", got.EID)
	}
}

func TestDecodeLockID_Errors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "empty"},
		{"nodollar", "missing '$'"},
		{"/p/n$", "empty eid"},
		{"$abc", "missing path/node split"},
		{"/onlyroot$abc", "missing path/node split"},
		{"/p/n$notHex!", "bad eid"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, err := DecodeLockID([]byte(c.in))
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}
