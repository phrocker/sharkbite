package cclient

import (
	"bytes"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

func TestNewMutation_RejectsEmptyRow(t *testing.T) {
	if _, err := NewMutation(nil); err == nil {
		t.Error("nil row should error")
	}
	if _, err := NewMutation([]byte{}); err == nil {
		t.Error("empty row should error")
	}
}

func TestNewMutation_DefensiveRowCopy(t *testing.T) {
	row := []byte("row-1")
	m, err := NewMutation(row)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	row[0] = 'X'
	if !bytes.Equal(m.Row(), []byte("row-1")) {
		t.Errorf("row not defensively copied: %q", m.Row())
	}
}

func TestMutation_PutAndDelete(t *testing.T) {
	m, _ := NewMutation([]byte("row"))
	m.Put([]byte("cf"), []byte("cq"), []byte("cv"), 1234, []byte("value"))
	m.PutLatest([]byte("cf"), []byte("cq2"), nil, []byte("v2"))
	m.Delete([]byte("cf"), []byte("cq"), nil, 5678)
	m.DeleteLatest([]byte("cf"), []byte("cq3"), nil)

	if m.Size() != 4 {
		t.Fatalf("Size = %d, want 4", m.Size())
	}
	es := m.Entries()
	// Put #1
	if es[0].Deleted || !bytes.Equal(es[0].Value, []byte("value")) || es[0].Timestamp != 1234 {
		t.Errorf("entry[0] = %+v", es[0])
	}
	// PutLatest
	if es[1].Deleted || es[1].Timestamp != MutationLatestTimestamp || !bytes.Equal(es[1].Value, []byte("v2")) {
		t.Errorf("entry[1] = %+v", es[1])
	}
	// Delete (no value)
	if !es[2].Deleted || es[2].Value != nil || es[2].Timestamp != 5678 {
		t.Errorf("entry[2] = %+v", es[2])
	}
	// DeleteLatest
	if !es[3].Deleted || es[3].Timestamp != MutationLatestTimestamp {
		t.Errorf("entry[3] = %+v", es[3])
	}
}

func TestMutation_LatestTimestampIsLongMax(t *testing.T) {
	if MutationLatestTimestamp != 9223372036854775807 {
		t.Errorf("MutationLatestTimestamp = %d, want 9223372036854775807", MutationLatestTimestamp)
	}
}

func TestMutation_PreservesExplicitLongMaxTimestampOnWire(t *testing.T) {
	m, _ := NewMutation([]byte("row"))
	m.Put([]byte("cf"), []byte("cq"), nil, MutationLatestTimestamp, []byte("v"))
	m.entries[0].HasTimestamp = true
	wireMutation, err := m.ToThrift()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := FromThrift(wireMutation)
	if err != nil {
		t.Fatal(err)
	}
	entry := decoded.Entries()[0]
	if !entry.HasTimestamp || entry.Timestamp != MutationLatestTimestamp {
		t.Fatalf("decoded explicit timestamp = %+v", entry)
	}
}

func TestMutation_SerializeAccumulo4Encoding(t *testing.T) {
	m, _ := NewMutation([]byte("row"))
	m.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("v"))
	m.Put([]byte("f"), []byte("q"), []byte("A"), 128, []byte("value"))
	m.DeleteLatest([]byte("d"), []byte("x"), nil)

	got, err := m.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x02, 'c', 'f', 0x02, 'c', 'q', 0x00, 0x00, 0x00, 0x01, 'v',
		0x01, 'f', 0x01, 'q', 0x01, 'A', 0x01, 0x8f, 0x80, 0x00, 0x05, 'v', 'a', 'l', 'u', 'e',
		0x01, 'd', 0x01, 'x', 0x00, 0x00, 0x01, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Serialize() = %x, want %x", got, want)
	}
}

func TestMutation_ToThriftSeparatesLargeValues(t *testing.T) {
	m, _ := NewMutation([]byte("row"))
	large := bytes.Repeat([]byte{0xab}, mutationValueCopyCutoff)
	m.PutLatest([]byte("cf"), []byte("cq"), nil, large)

	wireMutation, err := m.ToThrift()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wireMutation.Row, []byte("row")) {
		t.Fatalf("Row = %q", wireMutation.Row)
	}
	if wireMutation.Entries != 1 {
		t.Fatalf("Entries = %d, want 1", wireMutation.Entries)
	}
	wantData := []byte{0x02, 'c', 'f', 0x02, 'c', 'q', 0x00, 0x00, 0x00, 0xff}
	if !bytes.Equal(wireMutation.Data, wantData) {
		t.Fatalf("Data = %x, want %x", wireMutation.Data, wantData)
	}
	if len(wireMutation.Values) != 1 || !bytes.Equal(wireMutation.Values[0], large) {
		t.Fatalf("Values did not contain the large value")
	}

	large[0] = 0
	if wireMutation.Values[0][0] != 0xab {
		t.Fatal("large value was not defensively copied")
	}
}

func TestMutation_FromThriftRoundTrip(t *testing.T) {
	original, _ := NewMutation([]byte("row"))
	original.PutLatest([]byte("cf"), []byte("cq"), []byte("A&B"), []byte("value"))
	original.Delete([]byte("cf"), []byte("gone"), nil, 42)
	original.Put([]byte("large"), nil, nil, -7, bytes.Repeat([]byte{0xab}, mutationValueCopyCutoff))
	wireMutation, err := original.ToThrift()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := FromThrift(wireMutation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Row(), original.Row()) || len(decoded.Entries()) != len(original.Entries()) {
		t.Fatalf("decoded mutation = %#v", decoded)
	}
	for i, want := range original.Entries() {
		got := decoded.Entries()[i]
		if !bytes.Equal(got.ColFamily, want.ColFamily) ||
			!bytes.Equal(got.ColQualifier, want.ColQualifier) ||
			!bytes.Equal(got.ColVisibility, want.ColVisibility) ||
			got.Timestamp != want.Timestamp || got.Deleted != want.Deleted ||
			!bytes.Equal(got.Value, want.Value) {
			t.Fatalf("entry %d = %#v, want %#v", i, got, want)
		}
	}
}

func TestMutation_FromThriftRejectsMalformedEncoding(t *testing.T) {
	tests := []*data.TMutation{
		nil,
		{Row: []byte("row"), Entries: -1},
		{Row: []byte("row"), Entries: 1, Data: []byte{2, 'c'}},
		{Row: []byte("row"), Entries: 0, Data: []byte{0}},
		{Row: []byte("row"), Entries: 1, Data: []byte{0, 0, 0, 2}},
	}
	for i, mutation := range tests {
		if _, err := FromThrift(mutation); err == nil {
			t.Fatalf("case %d unexpectedly decoded", i)
		}
	}
}

func TestMutation_PutDefensivelyCopiesInputs(t *testing.T) {
	cf := []byte("cf")
	cq := []byte("cq")
	cv := []byte("cv")
	value := []byte("value")
	m, _ := NewMutation([]byte("row"))
	m.PutLatest(cf, cq, cv, value)

	cf[0], cq[0], cv[0], value[0] = 'x', 'x', 'x', 'x'
	entry := m.Entries()[0]
	if string(entry.ColFamily) != "cf" ||
		string(entry.ColQualifier) != "cq" ||
		string(entry.ColVisibility) != "cv" ||
		string(entry.Value) != "value" {
		t.Fatalf("entry changed through caller-owned input: %+v", entry)
	}
}
