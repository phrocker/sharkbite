package accumulo

import (
	"bytes"
	"math"
	"testing"
)

func TestKeyRangePreservesFullBounds(t *testing.T) {
	start := &Key{
		Row:              []byte("row"),
		ColumnFamily:     []byte("cf"),
		ColumnQualifier:  []byte("a"),
		ColumnVisibility: []byte("A"),
		Timestamp:        9,
	}
	end := &Key{
		Row:              []byte("row"),
		ColumnFamily:     []byte("cf"),
		ColumnQualifier:  []byte("z"),
		ColumnVisibility: []byte("B"),
		Timestamp:        3,
	}
	scanRange, err := NewKeyRange(start, false, end, true)
	if err != nil {
		t.Fatal(err)
	}
	start.Row[0] = 'X'
	end.ColumnQualifier[0] = 'X'

	wire := scanRange.toThrift()
	if wire.InfiniteStartKey || wire.InfiniteStopKey {
		t.Fatal("bounded key range was marked infinite")
	}
	if wire.StartKeyInclusive || !wire.StopKeyInclusive {
		t.Fatalf(
			"inclusivity = start %v, stop %v",
			wire.StartKeyInclusive,
			wire.StopKeyInclusive,
		)
	}
	if got := string(wire.Start.Row); got != "row" {
		t.Fatalf("start row = %q, want row", got)
	}
	if got := string(wire.Start.ColQualifier); got != "a" {
		t.Fatalf("start qualifier = %q, want a", got)
	}
	if got := string(wire.Stop.ColQualifier); got != "z" {
		t.Fatalf("stop qualifier = %q, want z", got)
	}
	if wire.Start.Timestamp != 9 || wire.Stop.Timestamp != 3 {
		t.Fatalf("timestamps = %d, %d", wire.Start.Timestamp, wire.Stop.Timestamp)
	}

	startCopy := scanRange.StartKey()
	startCopy.Row[0] = 'Y'
	if got := string(scanRange.StartKey().Row); got != "row" {
		t.Fatalf("range start mutated through accessor: %q", got)
	}
}

func TestKeyRangeUsesAccumuloTimestampOrdering(t *testing.T) {
	newer := &Key{Row: []byte("row"), Timestamp: 10}
	older := &Key{Row: []byte("row"), Timestamp: 1}
	if _, err := NewKeyRange(newer, true, older, true); err != nil {
		t.Fatalf("newer-to-older range rejected: %v", err)
	}
	if _, err := NewKeyRange(older, true, newer, true); err == nil {
		t.Fatal("older-to-newer range accepted despite descending timestamp order")
	}
}

func TestRowRangeInclusiveEndStillIncludesWholeRow(t *testing.T) {
	scanRange, err := NewRange([]byte("a"), true, []byte("z"), true)
	if err != nil {
		t.Fatal(err)
	}
	wire := scanRange.toThrift()
	if wire.StopKeyInclusive {
		t.Fatal("inclusive row endpoint was not normalized to an exclusive successor")
	}
	if !bytes.Equal(wire.Stop.Row, []byte{'z', 0}) {
		t.Fatalf("stop row = %q, want z\\x00", wire.Stop.Row)
	}
}

func TestKeyRangeRejectsReversedFields(t *testing.T) {
	start := &Key{Row: []byte("row"), ColumnFamily: []byte("z")}
	end := &Key{Row: []byte("row"), ColumnFamily: []byte("a")}
	if _, err := NewKeyRange(start, true, end, true); err == nil {
		t.Fatal("reversed full-key range accepted")
	}
}

// TestKeyBoundsResolvesRowBoundsToAbsoluteKeys pins the conversion a
// key-ordered reader needs: a row bound covers every key in the row, and every
// such key sorts after the key that spells the row alone.
func TestKeyBoundsResolvesRowBoundsToAbsoluteKeys(t *testing.T) {
	exclusiveStart, err := NewRange([]byte("row2"), false, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	start, startInclusive, end, _ := exclusiveStart.KeyBounds()
	if start == nil || !bytes.Equal(start.Row, []byte("row2\x00")) || !startInclusive {
		t.Fatalf("exclusive row start = %+v inclusive=%t, want row2+NUL inclusive", start, startInclusive)
	}
	if start.Timestamp != math.MaxInt64 {
		t.Fatalf("start timestamp = %d, want the maximum: timestamps sort descending, so the first key of a row carries it", start.Timestamp)
	}
	if end != nil {
		t.Fatalf("unbounded end = %+v, want nil", end)
	}

	inclusiveEnd, err := NewRange(nil, true, []byte("row2"), true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, end, endInclusive := inclusiveEnd.KeyBounds()
	if end == nil || !bytes.Equal(end.Row, []byte("row2\x00")) || endInclusive || end.Timestamp != math.MaxInt64 {
		t.Fatalf("inclusive row end = %+v inclusive=%t, want row2+NUL exclusive at the maximum timestamp", end, endInclusive)
	}

	exclusiveEnd, err := NewRange(nil, true, []byte("row2"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, end, endInclusive = exclusiveEnd.KeyBounds()
	if end == nil || !bytes.Equal(end.Row, []byte("row2")) || endInclusive || end.Timestamp != math.MaxInt64 {
		t.Fatalf("exclusive row end = %+v inclusive=%t, want row2 exclusive at the maximum timestamp", end, endInclusive)
	}

	inclusiveStart, err := NewRange([]byte("row2"), true, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	start, startInclusive, _, _ = inclusiveStart.KeyBounds()
	if start == nil || !bytes.Equal(start.Row, []byte("row2")) || !startInclusive || start.Timestamp != math.MaxInt64 {
		t.Fatalf("inclusive row start = %+v inclusive=%t, want row2 inclusive at the maximum timestamp", start, startInclusive)
	}
}

// TestKeyBoundsLeavesKeyRangesAlone pins that a range built from full keys is
// returned as it was given: only row bounds carry the row-covering convention.
func TestKeyBoundsLeavesKeyRangesAlone(t *testing.T) {
	start := &Key{Row: []byte("row2"), ColumnFamily: []byte("cf"), Timestamp: 7}
	end := &Key{Row: []byte("row9"), ColumnFamily: []byte("cf"), Timestamp: 3}
	keyRange, err := NewKeyRange(start, false, end, true)
	if err != nil {
		t.Fatal(err)
	}
	gotStart, startInclusive, gotEnd, endInclusive := keyRange.KeyBounds()
	if !bytes.Equal(gotStart.Row, start.Row) || !bytes.Equal(gotStart.ColumnFamily, start.ColumnFamily) || startInclusive {
		t.Fatalf("start = %+v inclusive=%t, want the key it was built with, exclusive", gotStart, startInclusive)
	}
	if !bytes.Equal(gotEnd.Row, end.Row) || !endInclusive {
		t.Fatalf("end = %+v inclusive=%t, want the key it was built with, inclusive", gotEnd, endInclusive)
	}
	gotStart.Row[0] = 'X'
	if again, _, _, _ := keyRange.KeyBounds(); !bytes.Equal(again.Row, []byte("row2")) {
		t.Fatalf("KeyBounds returned an aliased key: %q", again.Row)
	}
}

// TestKeyBoundsOnNilRangeIsUnbounded pins the nil-receiver contract the other
// accessors already have.
func TestKeyBoundsOnNilRangeIsUnbounded(t *testing.T) {
	var nilRange *Range
	start, startInclusive, end, endInclusive := nilRange.KeyBounds()
	if start != nil || end != nil || !startInclusive || endInclusive {
		t.Fatalf("nil range bounds = %+v/%t %+v/%t, want unbounded", start, startInclusive, end, endInclusive)
	}
}
