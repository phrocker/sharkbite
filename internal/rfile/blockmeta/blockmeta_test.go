package blockmeta

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestParse_RoundtripEmpty(t *testing.T) {
	in := &BlockMeta{Version: V1}
	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	got, err := Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != V1 {
		t.Errorf("Version = %d; want %d", got.Version, V1)
	}
	if len(got.Vocabs) != 0 {
		t.Errorf("Vocabs = %v; want empty", got.Vocabs)
	}
	if len(got.BlockOverlays) != 0 {
		t.Errorf("BlockOverlays = %v; want empty", got.BlockOverlays)
	}
}

func TestParse_RoundtripWithVocabsAndOverlays(t *testing.T) {
	in := &BlockMeta{
		Version: V1,
		Vocabs: []Vocab{
			{
				Type:    VocabCF,
				ID:      0,
				Entries: [][]byte{[]byte("V"), []byte("E"), []byte("M")},
			},
		},
		BlockOverlays: [][]Overlay{
			{
				{Type: OverlayZoneMap, Payload: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
					0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05}},
			},
			{
				{Type: OverlayZoneMap, Payload: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x06,
					0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x09}},
				{Type: OverlayType(99), Payload: []byte("future overlay; reader skips via length")},
			},
		},
	}
	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	got, err := Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vocabs) != 1 || len(got.Vocabs[0].Entries) != 3 {
		t.Errorf("vocab roundtrip mismatch: %+v", got.Vocabs)
	}
	if len(got.BlockOverlays) != 2 {
		t.Fatalf("BlockOverlays count = %d; want 2", len(got.BlockOverlays))
	}
	if len(got.BlockOverlays[0]) != 1 {
		t.Errorf("block 0 overlay count = %d; want 1", len(got.BlockOverlays[0]))
	}
	if len(got.BlockOverlays[1]) != 2 {
		t.Errorf("block 1 overlay count = %d; want 2", len(got.BlockOverlays[1]))
	}
	if got.BlockOverlays[1][1].Type != 99 {
		t.Errorf("future overlay type lost: got %d", got.BlockOverlays[1][1].Type)
	}
}

func TestParse_RejectsBadMagic(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}))
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v; want ErrCorrupt", err)
	}
}

func TestParse_RejectsUnsupportedVersion(t *testing.T) {
	var buf bytes.Buffer
	// magic + version=99
	buf.Write([]byte{0x42, 0x4D, 0x45, 0x54, 0, 0, 0, 99})
	_, err := Parse(bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("err = %v; want ErrUnsupportedVersion", err)
	}
}

func TestZoneMap_RoundtripAndSkip(t *testing.T) {
	z := NewZoneMap()
	if !z.Empty() {
		t.Errorf("freshly-constructed zone map should be empty")
	}
	for _, ts := range []int64{100, 50, 200, 75, 30} {
		z.Update(ts)
	}
	if z.TsMin != 30 || z.TsMax != 200 {
		t.Errorf("ZoneMap{min=%d, max=%d}; want {30, 200}", z.TsMin, z.TsMax)
	}
	got, err := DecodeZoneMap(z.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.TsMin != 30 || got.TsMax != 200 {
		t.Errorf("decoded ZoneMap{min=%d, max=%d}; want {30, 200}", got.TsMin, got.TsMax)
	}
}

func TestZoneMap_NegativeBoundary(t *testing.T) {
	// Accumulo allows int64 timestamps spanning the full signed range.
	z := NewZoneMap()
	z.Update(math.MinInt64 + 1)
	z.Update(math.MaxInt64 - 1)
	got, _ := DecodeZoneMap(z.Encode())
	if got.TsMin != math.MinInt64+1 || got.TsMax != math.MaxInt64-1 {
		t.Errorf("ZoneMap edge values lost: %+v", got)
	}
}

func TestZoneMap_DecodeShortPayload(t *testing.T) {
	_, err := DecodeZoneMap([]byte{0, 1, 2, 3})
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v; want ErrCorrupt", err)
	}
}

func TestBlockMeta_FindOverlay(t *testing.T) {
	m := &BlockMeta{
		BlockOverlays: [][]Overlay{
			{
				{Type: OverlayZoneMap, Payload: []byte("zm payload")},
				{Type: OverlayCFSetRoaring, Payload: []byte("roaring payload")},
			},
			{},
		},
	}
	if got := m.FindOverlay(0, OverlayZoneMap); got == nil || string(got.Payload) != "zm payload" {
		t.Errorf("FindOverlay(0, ZoneMap) = %v", got)
	}
	if got := m.FindOverlay(0, OverlayTrigramBloom); got != nil {
		t.Errorf("FindOverlay(0, Trigram) should be nil (not present)")
	}
	if got := m.FindOverlay(1, OverlayZoneMap); got != nil {
		t.Errorf("FindOverlay(1, ZoneMap) should be nil (block has no overlays)")
	}
	if got := m.FindOverlay(7, OverlayZoneMap); got != nil {
		t.Errorf("FindOverlay(7, ...) should be nil (out of range)")
	}
}

func TestZoneMapBuilder_BuildEmit(t *testing.T) {
	b := NewZoneMapBuilder()
	if b.OverlayType() != OverlayZoneMap {
		t.Errorf("OverlayType = %d; want %d", b.OverlayType(), OverlayZoneMap)
	}
	// First block: 3 cells with timestamps 10, 50, 30.
	b.AppendCell(nil, nil, nil, nil, 10, nil)
	b.AppendCell(nil, nil, nil, nil, 50, nil)
	b.AppendCell(nil, nil, nil, nil, 30, nil)
	snap := b.Snapshot()
	zm, err := DecodeZoneMap(snap)
	if err != nil {
		t.Fatal(err)
	}
	if zm.TsMin != 10 || zm.TsMax != 50 {
		t.Errorf("block 1 ZoneMap{%d, %d}; want {10, 50}", zm.TsMin, zm.TsMax)
	}
	// Reset, second block with different range.
	b.Reset()
	b.AppendCell(nil, nil, nil, nil, 1000, nil)
	b.AppendCell(nil, nil, nil, nil, 2000, nil)
	zm2, _ := DecodeZoneMap(b.Snapshot())
	if zm2.TsMin != 1000 || zm2.TsMax != 2000 {
		t.Errorf("block 2 ZoneMap{%d, %d}; want {1000, 2000}", zm2.TsMin, zm2.TsMax)
	}
}

func TestZoneMapBuilder_EmptyBlockSuppresses(t *testing.T) {
	b := NewZoneMapBuilder()
	if got := b.Snapshot(); got != nil {
		t.Errorf("empty-block Snapshot should be nil; got %v", got)
	}
}
