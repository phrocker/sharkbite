// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package qwal

import (
	"errors"
	"io"
	"testing"
)

// fakeSegment is an in-memory segmentSource over a fixed slice of entries,
// optionally yielding an error after the entries are drained.
type fakeSegment struct {
	entries []*Entry
	pos     int
	tailErr error
	closed  bool
}

func (f *fakeSegment) Next() (*Entry, error) {
	if f.pos >= len(f.entries) {
		if f.tailErr != nil {
			return nil, f.tailErr
		}
		return nil, io.EOF
	}
	e := f.entries[f.pos]
	f.pos++
	return e, nil
}

func (f *fakeSegment) Close() error {
	f.closed = true
	return nil
}

func mut(event LogEvent, tabletID int32, seq int64) *Entry {
	return &Entry{Key: LogFileKey{Event: event, TabletID: tabletID, Seq: seq}}
}

// drain pulls every entry from a MergedStream and returns the (event,tablet,seq)
// triples in merged order.
func drain(t *testing.T, m *MergedStream) [][3]int64 {
	t.Helper()
	var got [][3]int64
	for {
		e, err := m.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("unexpected Next error: %v", err)
		}
		got = append(got, [3]int64{int64(eventType(e.Key.Event)), int64(e.Key.TabletID), e.Key.Seq})
	}
}

func TestMergedStream_OrdersByEventThenTabletThenSeq(t *testing.T) {
	// Each segment is internally append-ordered; across segments the merge
	// must produce LogFileKey.compareTo order: eventType, tabletId, seq.
	segA := &fakeSegment{entries: []*Entry{
		mut(EventDefineTablet, 5, 0),
		mut(EventMutation, 5, 2),
		mut(EventMutation, 5, 4),
	}}
	segB := &fakeSegment{entries: []*Entry{
		mut(EventMutation, 5, 1),
		mut(EventMutation, 5, 3),
		mut(EventMutation, 5, 5),
	}}

	got := drain(t, NewMergedStream(segA, segB))
	want := [][3]int64{
		{1, 5, 0}, // DEFINE_TABLET sorts before any MUTATION
		{3, 5, 1},
		{3, 5, 2},
		{3, 5, 3},
		{3, 5, 4},
		{3, 5, 5},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMergedStream_TabletIsHigherPriorityThanSeq(t *testing.T) {
	// A low-seq entry on a higher tabletId must still sort AFTER a high-seq
	// entry on a lower tabletId (mirrors LogFileKey.compareTo).
	segA := &fakeSegment{entries: []*Entry{mut(EventMutation, 1, 9)}}
	segB := &fakeSegment{entries: []*Entry{mut(EventMutation, 2, 0)}}

	got := drain(t, NewMergedStream(segA, segB))
	want := [][3]int64{{3, 1, 9}, {3, 2, 0}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMergedStream_EmptyAndSingleSegment(t *testing.T) {
	empty := &fakeSegment{}
	one := &fakeSegment{entries: []*Entry{mut(EventMutation, 7, 1), mut(EventMutation, 7, 2)}}

	got := drain(t, NewMergedStream(empty, one, &fakeSegment{}))
	if len(got) != 2 {
		t.Fatalf("expected 2 entries from the single non-empty segment, got %d", len(got))
	}

	if got := drain(t, NewMergedStream()); got != nil {
		t.Errorf("merge over zero sources should yield nothing, got %v", got)
	}
}

func TestMergedStream_EqualKeysTiebreakBySegmentOrder(t *testing.T) {
	// Two segments carrying the identical (event,tablet,seq) — the heap needs
	// a total order; the earlier segment index wins.
	a := mut(EventMutation, 3, 1)
	b := mut(EventMutation, 3, 1)
	segA := &fakeSegment{entries: []*Entry{a}}
	segB := &fakeSegment{entries: []*Entry{b}}

	m := NewMergedStream(segA, segB)
	first, _ := m.Next()
	if first != a {
		t.Error("equal-key tie should resolve to the earlier segment first")
	}
	second, _ := m.Next()
	if second != b {
		t.Error("second equal-key entry should be the later segment's")
	}
}

func TestMergedStream_SurfacesDecodeError(t *testing.T) {
	boom := errors.New("qwal: truncated WAL entry")
	segA := &fakeSegment{entries: []*Entry{mut(EventMutation, 1, 1)}, tailErr: boom}
	segB := &fakeSegment{entries: []*Entry{mut(EventMutation, 1, 2)}}

	m := NewMergedStream(segA, segB)
	// First entry is fine; the error appears once segA is drained and refilled.
	var lastErr error
	for {
		_, err := m.Next()
		if err != nil {
			lastErr = err
			break
		}
	}
	if !errors.Is(lastErr, boom) {
		t.Errorf("expected the segment decode error to surface, got %v", lastErr)
	}
}

func TestMergedStream_CloseClosesSources(t *testing.T) {
	segA := &fakeSegment{}
	segB := &fakeSegment{}
	m := NewMergedStream(segA, segB)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !segA.closed || !segB.closed {
		t.Error("Close should have closed every io.Closer source")
	}
}
