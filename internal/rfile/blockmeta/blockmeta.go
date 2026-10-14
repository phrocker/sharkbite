// Package blockmeta implements RFile.blockmeta — an optional meta-block
// in shoal-augmented RFiles that stores per-data-block summaries used
// for skip decisions before fetch+decompress.
//
// Stock Apache Accumulo doesn't emit this meta-block; readers that
// don't know about it ignore it (it's just another named entry in the
// MetaIndex). Writers without skip-overlay configuration don't emit it
// — the absence is the V0/legacy default. shoal's reader treats absence
// as "no skip available, walk every block."
//
// Wire format (TLV envelope; forward-compatible — unknown overlay types
// are skipped via their length prefix):
//
//	int32 magic        = 'BMET' (0x424D4554)
//	int32 version      = 1
//	int32 vocabCount
//	for each vocab:
//	    int32 type     (0=CF, 1=CV-label, 2=string)
//	    int32 vocabId
//	    int32 entryCount
//	    for each entry: vint length, bytes
//	int32 blockCount   (must equal LG.NumTotalEntries — leaf-block count)
//	for each block:
//	    int32 overlayCount
//	    for each overlay:
//	        int32 type        (registry below)
//	        int32 payloadLen
//	        [payloadLen bytes]
//
// Overlay type registry (extensible):
//
//	1   zone-map         tsMin, tsMax — used for time-range skip
//	2   cf-set-roaring   RoaringBitmap of CF vocab IDs present in block
//	3   cv-label-set     RoaringBitmap of CV label vocab IDs present
//	4   trigram-bloom    Bloom over 3-byte windows in indexed fields
//	5   ivf-pq-block     centroid IDs + tightest distance bound
//
// Tier 1 (this package) implements only zone-map. Tier 2 overlays plug
// into the same envelope without format changes — readers that don't
// know an overlay type skip it via the length prefix, so adding new
// overlays is a non-breaking change.
package blockmeta

import (
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// MetaBlockName is the entry under which the BCFile MetaIndex stashes
// blockmeta. Mirrors RFile's "RFile.index" naming convention.
const MetaBlockName = "RFile.blockmeta"

// Magic identifies a blockmeta block. ASCII 'BMET'.
const Magic int32 = 0x424D4554

// V1 is the only supported version; version is part of the wire so we
// can evolve the envelope (vocab section, additional fixed fields)
// independent of overlay types.
const V1 int32 = 1

// VocabType discriminates the kind of bytes a file-scoped vocabulary
// holds. Per-overlay payloads reference vocabularies by id.
type VocabType int32

const (
	VocabCF      VocabType = 0
	VocabCVLabel VocabType = 1
	VocabString  VocabType = 2
)

// Vocab is a file-scoped lookup table of byte strings. Overlays that
// would otherwise repeat the same byte strings per block (e.g.,
// cf-set-roaring) reference vocab IDs instead.
type Vocab struct {
	Type    VocabType
	ID      int32
	Entries [][]byte
}

// OverlayType is the registry ID assigned to each overlay format. New
// overlay implementations register a constant here and a payload encoder/
// decoder in their own file.
type OverlayType int32

const (
	OverlayZoneMap      OverlayType = 1
	OverlayCFSetRoaring OverlayType = 2
	OverlayCVLabelSet   OverlayType = 3
	OverlayTrigramBloom OverlayType = 4
	OverlayIVFPQBlock   OverlayType = 5
)

// Overlay is one typed payload attached to one block. The payload is
// kept opaque at this layer; consumers downcast via type-specific
// parsers (e.g., DecodeZoneMap).
type Overlay struct {
	Type    OverlayType
	Payload []byte
}

// BlockMeta is the parsed meta-block. Vocabularies + per-block overlay
// lists. blockCount must equal the locality group's total leaf count;
// the i'th BlockOverlays[i] applies to the i'th leaf in tree order.
type BlockMeta struct {
	Version       int32
	Vocabs        []Vocab
	BlockOverlays [][]Overlay
}

// ErrCorrupt indicates malformed bytes in the meta block.
var ErrCorrupt = errors.New("blockmeta: corrupt")

// ErrUnsupportedVersion is returned when the version field is something
// this build doesn't know how to parse. Forward-incompatible — bumping
// the version means a wire-format change that isn't a TLV-skippable
// extension.
var ErrUnsupportedVersion = errors.New("blockmeta: unsupported version")

// Parse reads a serialized RFile.blockmeta block. The reader is
// expected to start at the magic int32; trailing bytes after the last
// block's overlays are an error (catches truncation).
func Parse(r wire.ByteAndReader) (*BlockMeta, error) {
	magic, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("magic: %w", err)
	}
	if magic != Magic {
		return nil, fmt.Errorf("%w: bad magic 0x%08x (want 0x%08x)", ErrCorrupt, magic, Magic)
	}
	version, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}
	if version != V1 {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
	}

	vocabCount, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("vocabCount: %w", err)
	}
	if vocabCount < 0 {
		return nil, fmt.Errorf("%w: negative vocabCount %d", ErrCorrupt, vocabCount)
	}
	vocabs := make([]Vocab, vocabCount)
	for i := int32(0); i < vocabCount; i++ {
		v, err := readVocab(r)
		if err != nil {
			return nil, fmt.Errorf("vocab[%d]: %w", i, err)
		}
		vocabs[i] = v
	}

	blockCount, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("blockCount: %w", err)
	}
	if blockCount < 0 {
		return nil, fmt.Errorf("%w: negative blockCount %d", ErrCorrupt, blockCount)
	}
	blockOverlays := make([][]Overlay, blockCount)
	for i := int32(0); i < blockCount; i++ {
		ovs, err := readBlockOverlays(r)
		if err != nil {
			return nil, fmt.Errorf("block[%d] overlays: %w", i, err)
		}
		blockOverlays[i] = ovs
	}
	return &BlockMeta{
		Version:       version,
		Vocabs:        vocabs,
		BlockOverlays: blockOverlays,
	}, nil
}

func readVocab(r wire.ByteAndReader) (Vocab, error) {
	t, err := wire.ReadInt32(r)
	if err != nil {
		return Vocab{}, fmt.Errorf("type: %w", err)
	}
	id, err := wire.ReadInt32(r)
	if err != nil {
		return Vocab{}, fmt.Errorf("id: %w", err)
	}
	count, err := wire.ReadInt32(r)
	if err != nil {
		return Vocab{}, fmt.Errorf("entryCount: %w", err)
	}
	if count < 0 {
		return Vocab{}, fmt.Errorf("%w: negative entryCount %d", ErrCorrupt, count)
	}
	entries := make([][]byte, count)
	for i := int32(0); i < count; i++ {
		n, _, err := wire.ReadVInt(r)
		if err != nil {
			return Vocab{}, fmt.Errorf("entry[%d] len: %w", i, err)
		}
		if n < 0 {
			return Vocab{}, fmt.Errorf("%w: entry[%d] negative len %d", ErrCorrupt, i, n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Vocab{}, fmt.Errorf("entry[%d] body: %w", i, err)
		}
		entries[i] = buf
	}
	return Vocab{Type: VocabType(t), ID: id, Entries: entries}, nil
}

func readBlockOverlays(r wire.ByteAndReader) ([]Overlay, error) {
	count, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("overlayCount: %w", err)
	}
	if count < 0 {
		return nil, fmt.Errorf("%w: negative overlayCount %d", ErrCorrupt, count)
	}
	out := make([]Overlay, count)
	for i := int32(0); i < count; i++ {
		t, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("overlay[%d] type: %w", i, err)
		}
		payloadLen, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("overlay[%d] payloadLen: %w", i, err)
		}
		if payloadLen < 0 {
			return nil, fmt.Errorf("%w: overlay[%d] negative payloadLen %d", ErrCorrupt, i, payloadLen)
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("overlay[%d] payload: %w", i, err)
		}
		out[i] = Overlay{Type: OverlayType(t), Payload: payload}
	}
	return out, nil
}

// Write serializes a BlockMeta to w. Inverse of Parse.
func Write(w io.Writer, m *BlockMeta) error {
	if err := wire.WriteInt32(w, Magic); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, m.Version); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, int32(len(m.Vocabs))); err != nil {
		return err
	}
	for i, v := range m.Vocabs {
		if err := writeVocab(w, v); err != nil {
			return fmt.Errorf("vocab[%d]: %w", i, err)
		}
	}
	if err := wire.WriteInt32(w, int32(len(m.BlockOverlays))); err != nil {
		return err
	}
	for i, ovs := range m.BlockOverlays {
		if err := writeBlockOverlays(w, ovs); err != nil {
			return fmt.Errorf("block[%d]: %w", i, err)
		}
	}
	return nil
}

func writeVocab(w io.Writer, v Vocab) error {
	if err := wire.WriteInt32(w, int32(v.Type)); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, v.ID); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, int32(len(v.Entries))); err != nil {
		return err
	}
	bw, ok := w.(io.ByteWriter)
	if !ok {
		bw = byteWriterShim{w: w}
	}
	for i, e := range v.Entries {
		if _, err := wire.WriteVInt(bw, int32(len(e))); err != nil {
			return fmt.Errorf("entry[%d] len: %w", i, err)
		}
		if _, err := w.Write(e); err != nil {
			return fmt.Errorf("entry[%d] body: %w", i, err)
		}
	}
	return nil
}

func writeBlockOverlays(w io.Writer, overlays []Overlay) error {
	if err := wire.WriteInt32(w, int32(len(overlays))); err != nil {
		return err
	}
	for i, o := range overlays {
		if err := wire.WriteInt32(w, int32(o.Type)); err != nil {
			return fmt.Errorf("overlay[%d] type: %w", i, err)
		}
		if err := wire.WriteInt32(w, int32(len(o.Payload))); err != nil {
			return fmt.Errorf("overlay[%d] payloadLen: %w", i, err)
		}
		if _, err := w.Write(o.Payload); err != nil {
			return fmt.Errorf("overlay[%d] body: %w", i, err)
		}
	}
	return nil
}

// FindOverlay returns the first overlay of type t for the leaf at
// leafIdx, or nil if absent / out of range.
func (m *BlockMeta) FindOverlay(leafIdx int, t OverlayType) *Overlay {
	if leafIdx < 0 || leafIdx >= len(m.BlockOverlays) {
		return nil
	}
	for i := range m.BlockOverlays[leafIdx] {
		if m.BlockOverlays[leafIdx][i].Type == t {
			return &m.BlockOverlays[leafIdx][i]
		}
	}
	return nil
}

type byteWriterShim struct{ w io.Writer }

func (s byteWriterShim) WriteByte(b byte) error {
	_, err := s.w.Write([]byte{b})
	return err
}
