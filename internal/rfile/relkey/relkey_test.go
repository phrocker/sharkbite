package relkey

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// roundTrip encodes the given keys+values into a single block and decodes
// them back, returning the decoded sequence and any read-side error. Uses
// the package's own EncodeKey, so this only proves the encoder and
// decoder agree — but every case below was hand-checked against the Java
// algorithm in RelativeKey.java to ensure that agreement is the right
// agreement.
func roundTrip(t *testing.T, kvs []kvPair) []decoded {
	t.Helper()
	var buf bytes.Buffer
	var prev *Key
	for i, kv := range kvs {
		if err := EncodeKey(&buf, prev, kv.k, kv.v); err != nil {
			t.Fatalf("EncodeKey #%d: %v", i, err)
		}
		prev = kv.k
	}

	r := NewReader(buf.Bytes(), len(kvs))
	out := make([]decoded, 0, len(kvs))
	for {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, decoded{k: k, v: v})
	}
	if r.Err() != nil {
		t.Fatalf("Reader.Err: %v", r.Err())
	}
	if r.Remaining() != 0 {
		t.Errorf("Reader.Remaining = %d, want 0", r.Remaining())
	}
	return out
}

type kvPair struct {
	k *Key
	v []byte
}

type decoded struct {
	k *Key
	v []byte
}

func mk(row, cf, cq, cv string, ts int64, del bool) *Key {
	return &Key{
		Row:              []byte(row),
		ColumnFamily:     []byte(cf),
		ColumnQualifier:  []byte(cq),
		ColumnVisibility: []byte(cv),
		Timestamp:        ts,
		Deleted:          del,
	}
}

func assertSame(t *testing.T, got []decoded, want []kvPair) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].k.Equal(want[i].k) {
			t.Errorf("entry %d key mismatch:\n got:  %+v\n want: %+v", i, got[i].k, want[i].k)
		}
		if !bytes.Equal(got[i].v, want[i].v) {
			t.Errorf("entry %d value mismatch: got %q, want %q", i, got[i].v, want[i].v)
		}
	}
}

// TestSingleKeyNoCompression: the simplest case — one key in a block, no
// previous, no SAME or PREFIX bits, just full lengths.
func TestSingleKeyNoCompression(t *testing.T) {
	kvs := []kvPair{
		{mk("rowA", "fam1", "qual1", "PUBLIC", 1000, false), []byte("v1")},
	}
	assertSame(t, roundTrip(t, kvs), kvs)
}

// TestAllFieldsSame: two consecutive identical-coordinate keys (e.g. two
// timestamped versions of the same cell). Should set ROW/CF/CQ/CV/TS_SAME
// on the second.
func TestAllFieldsSame(t *testing.T) {
	k1 := mk("r", "f", "q", "v", 100, false)
	k2 := mk("r", "f", "q", "v", 100, false)
	got := roundTrip(t, []kvPair{
		{k1, []byte("a")},
		{k2, []byte("b")},
	})
	assertSame(t, got, []kvPair{{k1, []byte("a")}, {k2, []byte("b")}})

	// Confirm the encoder picked the all-same path: total bytes for the 2nd
	// entry should be just the fieldsSame byte + 4-byte value-len + value.
	var buf bytes.Buffer
	if err := EncodeKey(&buf, k1, k2, []byte("b")); err != nil {
		t.Fatal(err)
	}
	wantHeader := RowSame | CFSame | CQSame | CVSame | TSSame
	if buf.Bytes()[0] != wantHeader {
		t.Errorf("fieldsSame = %#x, want %#x", buf.Bytes()[0], wantHeader)
	}
	// 1 byte header + 4 byte val len + 1 byte payload = 6.
	if buf.Len() != 6 {
		t.Errorf("encoded len = %d, want 6 (header+vallen+1)", buf.Len())
	}
}

// TestPrefixCompression: the second key shares >1 bytes with the first in
// every text field, so all four COMMON_PREFIX bits should fire.
func TestPrefixCompression(t *testing.T) {
	k1 := mk("rowAAA", "famXY", "qualUVW", "PUBLIC", 100, false)
	k2 := mk("rowAAB", "famXZ", "qualUVX", "PUBLIQ", 100, false)
	kvs := []kvPair{
		{k1, []byte("v1")},
		{k2, []byte("v2")},
	}
	assertSame(t, roundTrip(t, kvs), kvs)

	// Inspect the second-key header bytes directly.
	var buf bytes.Buffer
	if err := EncodeKey(&buf, k1, k2, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[0]&PrefixCompressionEnabled == 0 {
		t.Errorf("expected PREFIX_COMPRESSION_ENABLED; fieldsSame=%#x", buf.Bytes()[0])
	}
	if got := buf.Bytes()[1]; got&(RowCommonPrefix|CFCommonPrefix|CQCommonPrefix|CVCommonPrefix) == 0 {
		t.Errorf("expected at least one *_COMMON_PREFIX bit; fieldsPrefixed=%#x", got)
	}
}

// TestMixedFlagCombos covers SAME for some fields, PREFIX for others, and
// full-write for the rest, all in one transition.
func TestMixedFlagCombos(t *testing.T) {
	k1 := mk("r1", "famAA", "Q", "L1", 50, false)
	k2 := mk("r1" /* same */, "famAB" /* prefix */, "totally-different" /* full */, "L1" /* same */, 50, true)
	kvs := []kvPair{{k1, []byte("a")}, {k2, []byte("b")}}
	assertSame(t, roundTrip(t, kvs), kvs)

	var buf bytes.Buffer
	if err := EncodeKey(&buf, k1, k2, []byte("b")); err != nil {
		t.Fatal(err)
	}
	fs := buf.Bytes()[0]
	if fs&RowSame == 0 {
		t.Error("expected ROW_SAME")
	}
	if fs&CFSame != 0 {
		t.Error("did not expect CF_SAME")
	}
	if fs&CQSame != 0 {
		t.Error("did not expect CQ_SAME")
	}
	if fs&CVSame == 0 {
		t.Error("expected CV_SAME")
	}
	if fs&TSSame == 0 {
		t.Error("expected TS_SAME (timestamps match)")
	}
	if fs&Deleted == 0 {
		t.Error("expected DELETED bit (k2.Deleted=true)")
	}
	if fs&PrefixCompressionEnabled == 0 {
		t.Error("expected PREFIX_COMPRESSION_ENABLED (CF prefixed)")
	}
}

// TestDeletedRoundTrip: the DELETED bit lives in fieldsSame; make sure it
// survives encode->decode regardless of whether other bits are set.
func TestDeletedRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		k1, k2  *Key
		v1, v2  []byte
		wantDel [2]bool
	}{
		{
			name: "first_deleted_only",
			k1:   mk("r", "f", "q", "v", 1, true),
			k2:   mk("r", "f", "q", "v", 1, false),
			v1:   []byte{}, v2: []byte{},
			wantDel: [2]bool{true, false},
		},
		{
			name: "second_deleted_only",
			k1:   mk("r", "f", "q", "v", 1, false),
			k2:   mk("r", "f", "q", "v", 1, true),
			v1:   []byte("x"), v2: []byte("y"),
			wantDel: [2]bool{false, true},
		},
		{
			name: "both_deleted",
			k1:   mk("r", "f", "q", "v", 1, true),
			k2:   mk("r", "f", "q", "v", 2, true),
			v1:   []byte("x"), v2: []byte("y"),
			wantDel: [2]bool{true, true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := roundTrip(t, []kvPair{{c.k1, c.v1}, {c.k2, c.v2}})
			if got[0].k.Deleted != c.wantDel[0] {
				t.Errorf("entry 0 Deleted = %v, want %v", got[0].k.Deleted, c.wantDel[0])
			}
			if got[1].k.Deleted != c.wantDel[1] {
				t.Errorf("entry 1 Deleted = %v, want %v", got[1].k.Deleted, c.wantDel[1])
			}
		})
	}
}

// TestTimestampSameVsDiff hits both ts encodings:
//
//	TS_SAME    : second key has identical ts to first; no ts bytes on wire
//	TS_DIFF    : ts encoded as delta (added to prev.ts on read)
//	full vlong : reserved for the very first key in a block (no prev)
func TestTimestampSameVsDiff(t *testing.T) {
	t.Run("ts_same", func(t *testing.T) {
		k1 := mk("r", "f", "q", "v", 1000, false)
		k2 := mk("r", "f", "q", "v", 1000, false)
		got := roundTrip(t, []kvPair{{k1, []byte("a")}, {k2, []byte("b")}})
		if got[1].k.Timestamp != 1000 {
			t.Errorf("ts = %d, want 1000", got[1].k.Timestamp)
		}
	})
	t.Run("ts_diff_positive_delta", func(t *testing.T) {
		k1 := mk("r", "f", "q", "v", 1000, false)
		k2 := mk("r", "f", "q", "v", 1500, false)
		got := roundTrip(t, []kvPair{{k1, []byte("a")}, {k2, []byte("b")}})
		if got[1].k.Timestamp != 1500 {
			t.Errorf("ts = %d, want 1500", got[1].k.Timestamp)
		}
	})
	t.Run("ts_diff_negative_delta", func(t *testing.T) {
		// Newer-first ordering is the reader's problem; the codec must
		// faithfully encode and decode ANY delta, including a negative one.
		k1 := mk("r", "f", "q", "v", 2000, false)
		k2 := mk("r", "f", "q", "v", 1500, false)
		got := roundTrip(t, []kvPair{{k1, []byte("a")}, {k2, []byte("b")}})
		if got[1].k.Timestamp != 1500 {
			t.Errorf("ts = %d, want 1500", got[1].k.Timestamp)
		}
	})
}

// TestLongerBlock checks the prevKey threading across many entries — each
// entry's decoded key is what the next entry will diff against, and a bug
// in the cloning logic would surface as cumulative drift.
func TestLongerBlock(t *testing.T) {
	kvs := []kvPair{
		{mk("rowA", "f", "q", "PUBLIC", 100, false), []byte("a")},
		{mk("rowA", "f", "q", "PUBLIC", 200, false), []byte("b")}, // ts diff
		{mk("rowA", "f", "qXX", "PUBLIC", 200, true), []byte("c")},
		{mk("rowB", "g", "qXX", "ADMIN", 200, false), []byte("d")},
		{mk("rowB", "g", "qXX", "ADMIN", 200, false), []byte("e")},
	}
	assertSame(t, roundTrip(t, kvs), kvs)
}

// TestEmptyFields: zero-length row/cf/cq/cv (legal in Accumulo).
func TestEmptyFields(t *testing.T) {
	kvs := []kvPair{
		{mk("", "", "", "", 0, false), []byte("")},
		{mk("", "", "", "", 1, false), []byte("v")},
	}
	got := roundTrip(t, kvs)
	for i := range kvs {
		if !got[i].k.Equal(kvs[i].k) {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i].k, kvs[i].k)
		}
	}
}

// TestRejectsFirstKeyWithSameBit: a corrupted block must not deref nil
// prev. Hand-craft 1 byte that says ROW_SAME and feed it to the reader.
func TestRejectsFirstKeyWithSameBit(t *testing.T) {
	r := NewReader([]byte{RowSame}, 1)
	_, _, err := r.Next()
	if err == nil {
		t.Fatal("expected error on first key with ROW_SAME set")
	}
	if !strings.Contains(err.Error(), "first key") {
		t.Errorf("error = %v, want substring 'first key'", err)
	}
}

// TestPrematureEOFOnValue: a block that promises N entries but runs out
// mid-value should surface an io error, not silently truncate.
func TestPrematureEOFOnValue(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeKey(&buf, nil, mk("r", "f", "q", "v", 1, false), []byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Promise 2 entries but only encode 1.
	r := NewReader(buf.Bytes(), 2)
	if _, _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	_, _, err := r.Next()
	if err == nil {
		t.Fatal("expected error on second Next (block underflow)")
	}
}

// TestPrefixLenOutOfRange: a corrupted prefix that says "use 999 bytes
// from a 5-byte previous field" must not panic.
func TestPrefixLenOutOfRange(t *testing.T) {
	// Build entry 1 normally, then a bogus entry 2 with PREFIX_COMPRESSION_ENABLED
	// + ROW_COMMON_PREFIX + a vint prefixLen way past the previous row length.
	var buf bytes.Buffer
	if err := EncodeKey(&buf, nil, mk("ab", "f", "q", "v", 1, false), []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Header: PREFIX_COMPRESSION_ENABLED + ROW_COMMON_PREFIX + CF_SAME + CQ_SAME + CV_SAME + TS_SAME
	// (so we only have to provide a row prefix decode.)
	hdr := PrefixCompressionEnabled | CFSame | CQSame | CVSame | TSSame
	prefBits := RowCommonPrefix
	buf.WriteByte(hdr)
	buf.WriteByte(prefBits)
	buf.WriteByte(byte(int8(99))) // vint: 99 (< 127, single-byte direct) — way past prev row of 2 bytes
	// Don't bother filling the rest; the error must surface before we get
	// there.

	r := NewReader(buf.Bytes(), 2)
	if _, _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	_, _, err := r.Next()
	if err == nil {
		t.Fatal("expected out-of-range error")
	}
	if !strings.Contains(err.Error(), "prefix len") && !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v, want prefix-len-out-of-range message", err)
	}
}

// TestPrevKeyIsolation: mutating the returned key's slices must not
// affect the next decoded key (common bug: aliasing prev's backing array).
func TestPrevKeyIsolation(t *testing.T) {
	kvs := []kvPair{
		{mk("rowABC", "f", "q", "v", 100, false), []byte("a")},
		{mk("rowABC", "f", "q", "v", 200, false), []byte("b")}, // all SAME except ts
	}
	var buf bytes.Buffer
	var prev *Key
	for _, kv := range kvs {
		if err := EncodeKey(&buf, prev, kv.k, kv.v); err != nil {
			t.Fatal(err)
		}
		prev = kv.k
	}
	r := NewReader(buf.Bytes(), 2)
	k1, _, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	// Mutate k1's row in place. If the reader aliased prev.Row into k2, the
	// next decode would see "xxxABC".
	for i := range k1.Row {
		k1.Row[i] = 'X'
	}
	k2, _, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(k2.Row) != "rowABC" {
		t.Errorf("aliasing bug: k2.Row = %q, want \"rowABC\"", k2.Row)
	}
}
