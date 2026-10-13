package index

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// keyRow is a one-shot helper for IndexEntries — most tests only care
// about the Row component for ordering.
func keyRow(row string) *wire.Key {
	return &wire.Key{
		Row: []byte(row),
		// Other fields zero — that's fine for ordering tests.
	}
}

// buildIndexBlock packs a slice of entries into an IndexBlock at the
// given level. Offsets[] are populated by re-encoding each entry into
// Data; entries don't move once written.
func buildIndexBlock(t *testing.T, level int32, entries []*IndexEntry) *IndexBlock {
	t.Helper()
	var data bytes.Buffer
	offsets := make([]int32, 0, len(entries))
	for _, e := range entries {
		offsets = append(offsets, int32(data.Len()))
		if err := WriteIndexEntry(&data, e); err != nil {
			t.Fatal(err)
		}
	}
	return &IndexBlock{
		Level:   level,
		Offset:  0,
		HasNext: false,
		Offsets: offsets,
		Data:    data.Bytes(),
	}
}

func TestEntries_AtAndKeyAt(t *testing.T) {
	entries := []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 100, CompressedSize: 50, RawSize: 200},
		{Key: keyRow("m"), NumEntries: 2, Offset: 200, CompressedSize: 75, RawSize: 300},
		{Key: keyRow("z"), NumEntries: 3, Offset: 400, CompressedSize: 90, RawSize: 400},
	}
	block := buildIndexBlock(t, 0, entries)
	view := EntriesOf(block)

	if view.Len() != 3 {
		t.Errorf("Len = %d, want 3", view.Len())
	}
	for i, want := range entries {
		got, err := view.At(i)
		if err != nil {
			t.Fatalf("At(%d): %v", i, err)
		}
		if !got.Key.Equal(want.Key) || got.Offset != want.Offset {
			t.Errorf("At(%d): got %+v, want %+v", i, got, want)
		}
		k, err := view.KeyAt(i)
		if err != nil {
			t.Fatalf("KeyAt(%d): %v", i, err)
		}
		if !k.Equal(want.Key) {
			t.Errorf("KeyAt(%d): got %+v, want %+v", i, k, want.Key)
		}
	}
}

func TestEntries_AtBoundsError(t *testing.T) {
	view := EntriesOf(buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 0, CompressedSize: 1, RawSize: 1},
	}))
	if _, err := view.At(-1); err == nil {
		t.Errorf("At(-1) succeeded, want error")
	}
	if _, err := view.At(99); err == nil {
		t.Errorf("At(99) succeeded, want error")
	}
}

func TestEntries_AtRequiresExactDeclaredSegment(t *testing.T) {
	entry := &IndexEntry{
		Key: sampleKey("a"), NumEntries: 1, Offset: 1, CompressedSize: 1, RawSize: 1,
	}
	var encoded bytes.Buffer
	if err := WriteIndexEntry(&encoded, entry); err != nil {
		t.Fatal(err)
	}

	tests := map[string]*IndexBlock{
		"trailing bytes": {
			Offsets: []int32{0},
			Data:    append(append([]byte(nil), encoded.Bytes()...), 0),
		},
		"entry crosses next offset": {
			Offsets: []int32{0, int32(encoded.Len() - 1)},
			Data:    append(append([]byte(nil), encoded.Bytes()...), encoded.Bytes()...),
		},
	}
	for name, block := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := EntriesOf(block).At(0); err == nil {
				t.Fatal("At accepted an entry outside its exact declared segment")
			}
		})
	}
}

func TestEntries_NilBlock(t *testing.T) {
	view := EntriesOf(nil)
	if view.Len() != 0 {
		t.Errorf("nil-block Len = %d, want 0", view.Len())
	}
}

// TestSeek_SingleLevelExactMatch: leaf-only tree, target == an existing
// IndexEntry's key. Java's binary search returns the exact index.
func TestSeek_SingleLevelExactMatch(t *testing.T) {
	entries := []*IndexEntry{
		{Key: keyRow("apple"), NumEntries: 1, Offset: 100, CompressedSize: 10, RawSize: 50},
		{Key: keyRow("mango"), NumEntries: 2, Offset: 200, CompressedSize: 20, RawSize: 60},
		{Key: keyRow("zebra"), NumEntries: 3, Offset: 300, CompressedSize: 30, RawSize: 70},
	}
	root := buildIndexBlock(t, 0, entries)
	w := NewWalker(root, nil)

	for _, want := range entries {
		got, err := w.Seek(want.Key)
		if err != nil {
			t.Fatalf("Seek(%q): %v", want.Key.Row, err)
		}
		if !got.Key.Equal(want.Key) {
			t.Errorf("Seek(%q): got %+v, want %+v", want.Key.Row, got.Key, want.Key)
		}
	}
}

// TestSeek_SingleLevelInsertionPoint: target falls between two entries.
// Walker should return the first entry whose key >= target.
func TestSeek_SingleLevelInsertionPoint(t *testing.T) {
	entries := []*IndexEntry{
		{Key: keyRow("apple"), NumEntries: 1, Offset: 100},
		{Key: keyRow("mango"), NumEntries: 1, Offset: 200},
		{Key: keyRow("zebra"), NumEntries: 1, Offset: 300},
	}
	root := buildIndexBlock(t, 0, entries)
	w := NewWalker(root, nil)

	cases := []struct {
		target string
		want   string // expected entry's row
	}{
		{"aardvark", "apple"}, // before first
		{"banana", "mango"},   // between first and second
		{"narwhal", "zebra"},  // between second and third
		{"apple", "apple"},    // exact
		{"zebra", "zebra"},    // exact, last
	}
	for _, c := range cases {
		got, err := w.Seek(keyRow(c.target))
		if err != nil {
			t.Fatalf("Seek(%q): %v", c.target, err)
		}
		if string(got.Key.Row) != c.want {
			t.Errorf("Seek(%q): got %q, want %q", c.target, got.Key.Row, c.want)
		}
	}
}

func TestSeek_PastEnd(t *testing.T) {
	root := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 100},
		{Key: keyRow("b"), NumEntries: 1, Offset: 200},
	})
	w := NewWalker(root, nil)

	_, err := w.Seek(keyRow("zzz"))
	if !errors.Is(err, ErrPastEnd) {
		t.Errorf("err = %v, want ErrPastEnd", err)
	}
}

// fakeLevelReader satisfies LevelReader by mapping (offset, compSize)
// pairs to pre-built IndexBlocks. Tracks call count so tests can assert
// "no extra fetches happened."
type fakeLevelReader struct {
	blocks map[int64]*IndexBlock // keyed by IndexEntry.Offset
	calls  int64
	err    error
}

func (f *fakeLevelReader) ReadIndexBlock(region bcfile.BlockRegion) (*IndexBlock, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.blocks[region.Offset]
	if !ok {
		return nil, fmt.Errorf("fakeLevelReader: no block at offset %d", region.Offset)
	}
	return b, nil
}

// build2LevelTree builds:
//
//	root (Level 1):  IE("g", offset=10), IE("p", offset=20), IE("z", offset=30)
//	leaf @ offset 10: ["a", "g"]
//	leaf @ offset 20: ["m", "p"]
//	leaf @ offset 30: ["x", "z"]
//
// Each leaf entry's IndexEntry.key is the LAST key in the leaf, so the
// root's "g" entry points at the leaf containing keys ≤ "g".
func build2LevelTree(t *testing.T) (*IndexBlock, *fakeLevelReader) {
	t.Helper()
	leafGOffset := int64(10)
	leafPOffset := int64(20)
	leafZOffset := int64(30)

	leafG := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 1000, CompressedSize: 50, RawSize: 200},
		{Key: keyRow("g"), NumEntries: 1, Offset: 1100, CompressedSize: 50, RawSize: 200},
	})
	leafP := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("m"), NumEntries: 1, Offset: 1200, CompressedSize: 50, RawSize: 200},
		{Key: keyRow("p"), NumEntries: 1, Offset: 1300, CompressedSize: 50, RawSize: 200},
	})
	leafZ := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("x"), NumEntries: 1, Offset: 1400, CompressedSize: 50, RawSize: 200},
		{Key: keyRow("z"), NumEntries: 1, Offset: 1500, CompressedSize: 50, RawSize: 200},
	})

	root := buildIndexBlock(t, 1, []*IndexEntry{
		{Key: keyRow("g"), NumEntries: 2, Offset: leafGOffset, CompressedSize: 100, RawSize: 200},
		{Key: keyRow("p"), NumEntries: 2, Offset: leafPOffset, CompressedSize: 100, RawSize: 200},
		{Key: keyRow("z"), NumEntries: 2, Offset: leafZOffset, CompressedSize: 100, RawSize: 200},
	})

	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{
		leafGOffset: leafG,
		leafPOffset: leafP,
		leafZOffset: leafZ,
	}}
	return root, lr
}

func TestSeek_TwoLevelDescent(t *testing.T) {
	root, lr := build2LevelTree(t)
	w := NewWalker(root, lr)

	cases := []struct {
		target   string
		want     string // expected leaf entry's row
		expCalls int64
	}{
		{"a", "a", 1}, // descend through "g" entry, find "a" in leafG
		{"g", "g", 1}, // descend through "g" entry, find "g" exact
		{"h", "m", 1}, // root's "p" entry, leafP's "m" (smallest >= "h")
		{"p", "p", 1}, // root's "p" entry, leafP's "p" exact
		{"q", "x", 1}, // root's "z" entry, leafZ's "x"
		{"z", "z", 1}, // root's "z" entry, leafZ's "z"
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			atomic.StoreInt64(&lr.calls, 0)
			got, err := w.Seek(keyRow(c.target))
			if err != nil {
				t.Fatalf("Seek(%q): %v", c.target, err)
			}
			if string(got.Key.Row) != c.want {
				t.Errorf("Seek(%q): got %q, want %q", c.target, got.Key.Row, c.want)
			}
			if got := atomic.LoadInt64(&lr.calls); got != c.expCalls {
				t.Errorf("Seek(%q): %d level fetches, want %d", c.target, got, c.expCalls)
			}
		})
	}
}

func TestSeek_TwoLevelPastEnd(t *testing.T) {
	root, lr := build2LevelTree(t)
	w := NewWalker(root, lr)

	_, err := w.Seek(keyRow("zzz"))
	if !errors.Is(err, ErrPastEnd) {
		t.Errorf("err = %v, want ErrPastEnd", err)
	}
}

func TestSeek_MultiLevelRequiresLevelReader(t *testing.T) {
	root := buildIndexBlock(t, 1, []*IndexEntry{
		{Key: keyRow("g"), Offset: 10, CompressedSize: 1, RawSize: 1},
	})
	w := NewWalker(root, nil)
	_, err := w.Seek(keyRow("a"))
	if err == nil {
		t.Errorf("expected error when LevelReader is nil for multi-level tree")
	}
}

func TestSeek_LevelReaderErrorPropagates(t *testing.T) {
	root, lr := build2LevelTree(t)
	want := errors.New("disk on fire")
	lr.err = want
	w := NewWalker(root, lr)

	_, err := w.Seek(keyRow("a"))
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want chain to %v", err, want)
	}
}

func TestSeek_RejectsInvalidLevelTransition(t *testing.T) {
	root := buildIndexBlock(t, 1, []*IndexEntry{
		{Key: keyRow("z"), Offset: 10, CompressedSize: 1, RawSize: 1},
	})
	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{
		10: buildIndexBlock(t, 1, []*IndexEntry{
			{Key: keyRow("z"), Offset: 10, CompressedSize: 1, RawSize: 1},
		}),
	}}
	if _, err := NewWalker(root, lr).Seek(keyRow("a")); err == nil ||
		!strings.Contains(err.Error(), "child block level 1, want 0") {
		t.Fatalf("Seek error = %v, want invalid level transition", err)
	}
}

func TestSeek_RejectsExcessiveRootLevel(t *testing.T) {
	root := buildIndexBlock(t, maxTraversalDepth+1, []*IndexEntry{
		{Key: keyRow("z"), Offset: 10, CompressedSize: 1, RawSize: 1},
	})
	if _, err := NewWalker(root, &fakeLevelReader{}).Seek(keyRow("a")); err == nil ||
		!strings.Contains(err.Error(), "exceeds maximum traversal depth") {
		t.Fatalf("Seek error = %v, want traversal depth rejection", err)
	}
}

func TestIterateLeaves_SingleLevel(t *testing.T) {
	entries := []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 1},
		{Key: keyRow("b"), NumEntries: 1, Offset: 2},
		{Key: keyRow("c"), NumEntries: 1, Offset: 3},
	}
	w := NewWalker(buildIndexBlock(t, 0, entries), nil)

	var seen []string
	err := w.IterateLeaves(func(e *IndexEntry) error {
		seen = append(seen, string(e.Key.Row))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != "a" || seen[1] != "b" || seen[2] != "c" {
		t.Errorf("seen = %v, want [a b c]", seen)
	}
}

func TestIterateLeaves_TwoLevel(t *testing.T) {
	root, lr := build2LevelTree(t)
	w := NewWalker(root, lr)

	var seen []string
	err := w.IterateLeaves(func(e *IndexEntry) error {
		seen = append(seen, string(e.Key.Row))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "g", "m", "p", "x", "z"}
	if len(seen) != len(want) {
		t.Fatalf("seen %d, want %d", len(seen), len(want))
	}
	for i, w := range want {
		if seen[i] != w {
			t.Errorf("seen[%d] = %q, want %q", i, seen[i], w)
		}
	}
	// Descended into 3 child blocks (one per root entry).
	if got := atomic.LoadInt64(&lr.calls); got != 3 {
		t.Errorf("level fetches = %d, want 3", got)
	}
}

func TestIterateLeaves_RejectsRepeatedChildRegion(t *testing.T) {
	leaf := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("z"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
	})
	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{10: leaf}}
	region := bcfile.BlockRegion{Offset: 10, CompressedSize: 1, RawSize: 1}
	visited := make(map[blockRegionKey]struct{})
	if _, err := readChildBlock(lr, 1, region, nil, keyRow("z"), visited); err != nil {
		t.Fatalf("first readChildBlock: %v", err)
	}
	_, err := readChildBlock(lr, 1, region, nil, keyRow("z"), visited)
	if err == nil || !strings.Contains(err.Error(), "repeated child block region") {
		t.Fatalf("readChildBlock error = %v, want repeated region rejection", err)
	}
}

func TestIterateLeaves_RejectsRepeatedLeafRegion(t *testing.T) {
	root := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("m"), NumEntries: 1, Offset: 10, CompressedSize: 2, RawSize: 3},
		{Key: keyRow("z"), NumEntries: 1, Offset: 10, CompressedSize: 2, RawSize: 4},
	})
	callbacks := 0
	err := NewWalker(root, nil).IterateLeaves(func(*IndexEntry) error {
		callbacks++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "repeated leaf block region") {
		t.Fatalf("IterateLeaves error = %v, want repeated leaf region rejection", err)
	}
	if callbacks != 1 {
		t.Fatalf("callback count = %d, want 1 before repeated region rejection", callbacks)
	}
}

func TestIterateLeaves_RejectsUnorderedEntries(t *testing.T) {
	root := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("z"), NumEntries: 1, Offset: 10, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("a"), NumEntries: 1, Offset: 20, CompressedSize: 1, RawSize: 1},
	})
	callbacks := 0
	err := NewWalker(root, nil).IterateLeaves(func(*IndexEntry) error {
		callbacks++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "sorts before") {
		t.Fatalf("IterateLeaves error = %v, want key-order rejection", err)
	}
	if callbacks != 0 {
		t.Fatalf("callback count = %d, want 0", callbacks)
	}
}

func TestIterateLeaves_RejectsNonPositiveLeafCount(t *testing.T) {
	for _, count := range []int32{0, -1} {
		t.Run(fmt.Sprintf("count_%d", count), func(t *testing.T) {
			root := buildIndexBlock(t, 0, []*IndexEntry{{
				Key: keyRow("a"), NumEntries: count, Offset: 10, CompressedSize: 1, RawSize: 1,
			}})
			err := NewWalker(root, nil).IterateLeaves(func(*IndexEntry) error {
				t.Fatal("callback invoked for invalid leaf count")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "non-positive cell count") {
				t.Fatalf("IterateLeaves error = %v, want leaf-count rejection", err)
			}
		})
	}
}

func TestIterateLeaves_RejectsParentKeyMismatch(t *testing.T) {
	leaf := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 200, CompressedSize: 1, RawSize: 1},
	})
	root := buildIndexBlock(t, 1, []*IndexEntry{{
		Key: keyRow("b"), Offset: 10, CompressedSize: 1, RawSize: 1,
	}})
	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{10: leaf}}
	err := NewWalker(root, lr).IterateLeaves(func(*IndexEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "does not match parent upper bound") {
		t.Fatalf("IterateLeaves error = %v, want parent-bound rejection", err)
	}
}

func TestSeek_RejectsParentKeyMismatch(t *testing.T) {
	leaf := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 200, CompressedSize: 1, RawSize: 1},
	})
	root := buildIndexBlock(t, 1, []*IndexEntry{{
		Key: keyRow("z"), Offset: 10, CompressedSize: 1, RawSize: 1,
	}})
	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{10: leaf}}
	_, err := NewWalker(root, lr).Seek(keyRow("m"))
	if err == nil || !strings.Contains(err.Error(), "does not match parent upper bound") {
		t.Fatalf("Seek error = %v, want parent-bound rejection", err)
	}
}

func TestIterateLeaves_RejectsOverlappingChildKeyRanges(t *testing.T) {
	first := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 200, CompressedSize: 1, RawSize: 1},
	})
	second := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("b"), NumEntries: 1, Offset: 300, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("z"), NumEntries: 1, Offset: 400, CompressedSize: 1, RawSize: 1},
	})
	root := buildIndexBlock(t, 1, []*IndexEntry{
		{Key: keyRow("m"), Offset: 10, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("z"), Offset: 20, CompressedSize: 1, RawSize: 1},
	})
	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{10: first, 20: second}}
	err := NewWalker(root, lr).IterateLeaves(func(*IndexEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "sorts before parent lower bound") {
		t.Fatalf("IterateLeaves error = %v, want overlapping-range rejection", err)
	}
}

func TestIterateLeaves_FnEarlyAbort(t *testing.T) {
	root, lr := build2LevelTree(t)
	w := NewWalker(root, lr)

	stop := errors.New("stop here")
	count := 0
	err := w.IterateLeaves(func(e *IndexEntry) error {
		count++
		if count == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Errorf("err = %v, want %v", err, stop)
	}
	if count != 3 {
		t.Errorf("called fn %d times, want 3", count)
	}
}

// build3LevelTree builds a 3-level tree to exercise deeper descent:
//
//	root (Level 2): one entry pointing at the only level-1 block
//	level-1 block (Level 1): 2 entries pointing at 2 leaf blocks
//	leaf A (Level 0): keys "aa", "ab"
//	leaf B (Level 0): keys "ba", "bb"
//
// Smallest realistic 3-level shape, lets us test the full descent path.
func build3LevelTree(t *testing.T) (*IndexBlock, *fakeLevelReader) {
	t.Helper()
	leafA := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("aa"), NumEntries: 1, Offset: 1000, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("ab"), NumEntries: 1, Offset: 1100, CompressedSize: 1, RawSize: 1},
	})
	leafB := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("ba"), NumEntries: 1, Offset: 1200, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("bb"), NumEntries: 1, Offset: 1300, CompressedSize: 1, RawSize: 1},
	})
	level1 := buildIndexBlock(t, 1, []*IndexEntry{
		{Key: keyRow("ab"), Offset: 100, CompressedSize: 50, RawSize: 100}, // points at leafA
		{Key: keyRow("bb"), Offset: 200, CompressedSize: 50, RawSize: 100}, // points at leafB
	})
	root := buildIndexBlock(t, 2, []*IndexEntry{
		{Key: keyRow("bb"), Offset: 50, CompressedSize: 50, RawSize: 100}, // points at level1
	})
	lr := &fakeLevelReader{blocks: map[int64]*IndexBlock{
		50:  level1,
		100: leafA,
		200: leafB,
	}}
	return root, lr
}

func TestSeek_ThreeLevel(t *testing.T) {
	root, lr := build3LevelTree(t)
	w := NewWalker(root, lr)

	cases := []struct {
		target string
		want   string
		// 2 level-fetches: level1 + leaf
		expCalls int64
	}{
		{"a", "aa", 2},
		{"aa", "aa", 2},
		{"ab", "ab", 2},
		{"ac", "ba", 2}, // jumps from leafA to leafB via level1's "bb" entry
		{"bb", "bb", 2},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			atomic.StoreInt64(&lr.calls, 0)
			got, err := w.Seek(keyRow(c.target))
			if err != nil {
				t.Fatalf("Seek(%q): %v", c.target, err)
			}
			if string(got.Key.Row) != c.want {
				t.Errorf("Seek(%q): got %q, want %q", c.target, got.Key.Row, c.want)
			}
			if got := atomic.LoadInt64(&lr.calls); got != c.expCalls {
				t.Errorf("Seek(%q): %d fetches, want %d", c.target, got, c.expCalls)
			}
		})
	}
}

func TestIterateLeaves_ThreeLevel(t *testing.T) {
	root, lr := build3LevelTree(t)
	w := NewWalker(root, lr)

	var seen []string
	err := w.IterateLeaves(func(e *IndexEntry) error {
		seen = append(seen, string(e.Key.Row))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aa", "ab", "ba", "bb"}
	if len(seen) != len(want) {
		t.Fatalf("seen %d, want %d", len(seen), len(want))
	}
	for i, w := range want {
		if seen[i] != w {
			t.Errorf("seen[%d] = %q, want %q", i, seen[i], w)
		}
	}
}

func TestSeek_EmptyRootIsPastEnd(t *testing.T) {
	root := &IndexBlock{Level: 0, Offsets: []int32{}, Data: []byte{}}
	w := NewWalker(root, nil)
	_, err := w.Seek(keyRow("anything"))
	if !errors.Is(err, ErrPastEnd) {
		t.Errorf("err = %v, want ErrPastEnd", err)
	}
}

// TestBinarySearchKey_LargeIndex verifies the binary search picks the
// right insertion point for a non-trivial-size index. 1000 entries with
// row keys "key0000".."key0999".
func TestBinarySearchKey_LargeIndex(t *testing.T) {
	const N = 1000
	entries := make([]*IndexEntry, N)
	for i := 0; i < N; i++ {
		entries[i] = &IndexEntry{
			Key: keyRow(fmt.Sprintf("key%04d", i)),
		}
	}
	view := EntriesOf(buildIndexBlock(t, 0, entries))

	// Exact matches at every position.
	for i := 0; i < N; i++ {
		idx, err := binarySearchKey(view, keyRow(fmt.Sprintf("key%04d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if idx != i {
			t.Errorf("Search(key%04d) = %d, want %d", i, idx, i)
		}
	}

	// Insertion-point cases.
	idx, _ := binarySearchKey(view, keyRow("k")) // before first
	if idx != 0 {
		t.Errorf("before-first idx = %d, want 0", idx)
	}
	idx, _ = binarySearchKey(view, keyRow("zzz")) // past last
	if idx != N {
		t.Errorf("past-last idx = %d, want %d", idx, N)
	}
}
