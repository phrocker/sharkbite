//go:build !embed

package cclient

import (
	"bytes"
	"testing"
)

func TestNewRange_Validation(t *testing.T) {
	tests := []struct {
		name           string
		start          []byte
		startInclusive bool
		end            []byte
		endInclusive   bool
		wantErr        string
	}{
		{"ok-bounded", []byte("a"), true, []byte("z"), true, ""},
		{"ok-equal-rows-inclusive", []byte("a"), true, []byte("a"), true, ""},
		{"ok-no-start", nil, true, []byte("z"), true, ""},
		{"ok-no-end", []byte("a"), true, nil, false, ""},
		{"ok-fully-open", nil, true, nil, false, ""},
		{"err-end-before-start", []byte("z"), true, []byte("a"), true, "endRow"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRange(tc.start, tc.startInclusive, tc.end, tc.endInclusive)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err: %v", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestNewRangeRow_EmptyRejected(t *testing.T) {
	if _, err := NewRangeRow(nil); err == nil {
		t.Error("nil row should error")
	}
	if _, err := NewRangeRow([]byte{}); err == nil {
		t.Error("empty row should error")
	}
}

func TestInfiniteRange_ToThriftHasNilKeys(t *testing.T) {
	tr := InfiniteRange().ToThrift()
	if !tr.InfiniteStartKey || !tr.InfiniteStopKey {
		t.Errorf("flags wrong: start=%v stop=%v", tr.InfiniteStartKey, tr.InfiniteStopKey)
	}
	if tr.Start != nil {
		t.Errorf("Start = %+v, want nil", tr.Start)
	}
	if tr.Stop != nil {
		t.Errorf("Stop = %+v, want nil", tr.Stop)
	}
}

func TestRange_ToThrift_InclusiveEndAppendsZeroByte(t *testing.T) {
	// This is the critical correctness gotcha. Inclusive endRow on the
	// wire must become exclusive-with-0x00-appended.
	r, err := NewRange([]byte("a"), true, []byte("zzz"), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := r.ToThrift()
	if tr.Stop == nil {
		t.Fatal("Stop is nil")
	}
	want := append([]byte("zzz"), 0x00)
	if !bytes.Equal(tr.Stop.Row, want) {
		t.Errorf("Stop.Row = %x, want %x", tr.Stop.Row, want)
	}
	if tr.StopKeyInclusive {
		t.Error("StopKeyInclusive should be flipped to false after 0x00 trick")
	}
	if !tr.StartKeyInclusive {
		t.Error("StartKeyInclusive should remain true (flag passes through)")
	}
}

func TestRange_ToThrift_ExclusiveEndIsVerbatim(t *testing.T) {
	r, err := NewRange([]byte("a"), false, []byte("zzz"), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := r.ToThrift()
	if !bytes.Equal(tr.Stop.Row, []byte("zzz")) {
		t.Errorf("Stop.Row = %q, want zzz (no 0x00)", tr.Stop.Row)
	}
	if tr.StopKeyInclusive {
		t.Error("StopKeyInclusive should be false")
	}
	if tr.StartKeyInclusive {
		t.Error("StartKeyInclusive should be false")
	}
}

func TestRange_ToThrift_StartRowPreserved(t *testing.T) {
	r, err := NewRange([]byte("aaa"), true, []byte("zzz"), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := r.ToThrift()
	if tr.Start == nil {
		t.Fatal("nil Start")
	}
	if !bytes.Equal(tr.Start.Row, []byte("aaa")) {
		t.Errorf("Start.Row = %q", tr.Start.Row)
	}
	// Start row never gets the 0x00 transformation.
	if len(tr.Start.Row) != 3 {
		t.Errorf("Start.Row length = %d, want 3", len(tr.Start.Row))
	}
}

func TestRange_ToThrift_FromThrift_Roundtrip(t *testing.T) {
	// Roundtrip with exclusive endRow → byte-for-byte identical.
	r, err := NewRange([]byte("a"), false, []byte("z"), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tr := r.ToThrift()
	back, err := RangeFromThrift(tr)
	if err != nil {
		t.Fatalf("FromThrift: %v", err)
	}
	if !bytes.Equal(back.StartRow(), r.StartRow()) {
		t.Errorf("StartRow: %q vs %q", back.StartRow(), r.StartRow())
	}
	if !bytes.Equal(back.EndRow(), r.EndRow()) {
		t.Errorf("EndRow: %q vs %q", back.EndRow(), r.EndRow())
	}
	if back.StartInclusive() != r.StartInclusive() {
		t.Error("StartInclusive lost")
	}
	if back.EndInclusive() != r.EndInclusive() {
		t.Error("EndInclusive lost")
	}
}

func TestNewRangeRow_Behavior(t *testing.T) {
	r, err := NewRangeRow([]byte("the-row"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !r.StartInclusive() || !r.EndInclusive() {
		t.Error("single-row range should be doubly inclusive")
	}
	if !bytes.Equal(r.StartRow(), r.EndRow()) {
		t.Error("start/end rows should match")
	}
	tr := r.ToThrift()
	// Inclusive end → 0x00 appended.
	if !bytes.Equal(tr.Stop.Row, append([]byte("the-row"), 0x00)) {
		t.Errorf("Stop.Row = %x", tr.Stop.Row)
	}
}
