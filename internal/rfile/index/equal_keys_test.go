package index

import (
	"strings"
	"testing"
)

// Writers before #690 could give consecutive blocks the same last key. The
// walker accepts equal adjacent keys (never decreasing ones) and seeks to
// the first entry of an equal run.

// equalRunTree is a two-level tree whose leaves are a, m, m | m, m, z: the
// run of "m" blocks straddles the two child index blocks, so the second
// child's first key equals the root's previous entry.
func equalRunTree(t *testing.T) (*IndexBlock, *fakeLevelReader) {
	t.Helper()
	first := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("a"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 200, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 300, CompressedSize: 1, RawSize: 1},
	})
	second := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("m"), NumEntries: 1, Offset: 400, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 500, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("z"), NumEntries: 1, Offset: 600, CompressedSize: 1, RawSize: 1},
	})
	root := buildIndexBlock(t, 1, []*IndexEntry{
		{Key: keyRow("m"), Offset: 10, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("z"), Offset: 20, CompressedSize: 1, RawSize: 1},
	})
	return root, &fakeLevelReader{blocks: map[int64]*IndexBlock{10: first, 20: second}}
}

func TestValidateBlock_AcceptsEqualAdjacentKeys(t *testing.T) {
	block := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("m"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 200, CompressedSize: 1, RawSize: 1},
	})
	if err := ValidateBlock(block); err != nil {
		t.Fatalf("ValidateBlock(equal keys) = %v, want nil", err)
	}
}

func TestValidateBlock_RejectsDecreasingKeyAfterEqualRun(t *testing.T) {
	block := buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("m"), NumEntries: 1, Offset: 100, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("m"), NumEntries: 1, Offset: 200, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("l"), NumEntries: 1, Offset: 300, CompressedSize: 1, RawSize: 1},
	})
	err := ValidateBlock(block)
	if err == nil || !strings.Contains(err.Error(), "entry 2") || !strings.Contains(err.Error(), "sorts before") {
		t.Fatalf("ValidateBlock(decreasing) = %v, want entry 2 rejected", err)
	}
}

func TestIterateLeaves_AcceptsEqualRunAcrossChildren(t *testing.T) {
	root, lr := equalRunTree(t)
	var offsets []int64
	err := NewWalker(root, lr).IterateLeaves(func(e *IndexEntry) error {
		offsets = append(offsets, e.Offset)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateLeaves = %v", err)
	}
	want := []int64{100, 200, 300, 400, 500, 600}
	if len(offsets) != len(want) {
		t.Fatalf("leaves = %v, want %v", offsets, want)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Fatalf("leaves = %v, want %v", offsets, want)
		}
	}
}

func TestSeek_EqualRunLandsOnFirstEntry(t *testing.T) {
	root, lr := equalRunTree(t)
	w := NewWalker(root, lr)
	for target, wantOffset := range map[string]int64{
		"a": 100, "b": 200, "m": 200, "m\x00": 600, "z": 600,
	} {
		entry, err := w.Seek(keyRow(target))
		if err != nil {
			t.Fatalf("Seek(%q) = %v", target, err)
		}
		if entry.Offset != wantOffset {
			t.Fatalf("Seek(%q) landed on block %d, want %d", target, entry.Offset, wantOffset)
		}
	}
}

func TestIterateLeaves_RejectsChildStartingBeforeEqualLowerBound(t *testing.T) {
	root, lr := equalRunTree(t)
	// The second child now starts at "l", before the "m" its parent's
	// previous entry promised.
	lr.blocks[20] = buildIndexBlock(t, 0, []*IndexEntry{
		{Key: keyRow("l"), NumEntries: 1, Offset: 400, CompressedSize: 1, RawSize: 1},
		{Key: keyRow("z"), NumEntries: 1, Offset: 600, CompressedSize: 1, RawSize: 1},
	})
	err := NewWalker(root, lr).IterateLeaves(func(*IndexEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "sorts before parent lower bound") {
		t.Fatalf("IterateLeaves = %v, want lower-bound rejection", err)
	}
}
