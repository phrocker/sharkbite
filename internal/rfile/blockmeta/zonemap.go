package blockmeta

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ZoneMap is the Tier-1 overlay: per-block timestamp range. 16 bytes
// per block (2× int64 BE). Skip predicate at scan time:
//
//	if asOf < zm.TsMin → skip (no cell young enough; everything is older)
//	if floorTs > zm.TsMax → skip (no cell old enough)
//
// Note Accumulo timestamps DESCEND in sort order: a fresher cell has a
// LARGER timestamp value. The natural skip predicates flip from what
// you'd write for ascending stores; document this clearly at the
// caller.
type ZoneMap struct {
	TsMin int64
	TsMax int64
}

// NewZoneMap constructs an empty ZoneMap with sentinel min/max so the
// first Update widens it correctly.
func NewZoneMap() *ZoneMap {
	return &ZoneMap{TsMin: math.MaxInt64, TsMax: math.MinInt64}
}

// Update widens the range to include ts. Cheap: two int64 compares.
func (z *ZoneMap) Update(ts int64) {
	if ts < z.TsMin {
		z.TsMin = ts
	}
	if ts > z.TsMax {
		z.TsMax = ts
	}
}

// Empty reports whether no Update calls have been made (sentinel state).
// An empty ZoneMap should not be emitted; callers check this and skip.
func (z *ZoneMap) Empty() bool {
	return z.TsMin == math.MaxInt64 && z.TsMax == math.MinInt64
}

// Encode serializes a ZoneMap into a fixed 16-byte payload. Big-endian
// int64s; the same byte order DataInput.readLong / writeLong uses, so
// porting this to Java is a straight copy.
func (z *ZoneMap) Encode() []byte {
	out := make([]byte, 16)
	binary.BigEndian.PutUint64(out[0:8], uint64(z.TsMin))
	binary.BigEndian.PutUint64(out[8:16], uint64(z.TsMax))
	return out
}

// DecodeZoneMap parses a 16-byte zone-map payload. Returns ErrCorrupt
// if the payload isn't exactly 16 bytes.
func DecodeZoneMap(payload []byte) (*ZoneMap, error) {
	if len(payload) != 16 {
		return nil, fmt.Errorf("%w: zone-map payload is %d bytes (want 16)", ErrCorrupt, len(payload))
	}
	return &ZoneMap{
		TsMin: int64(binary.BigEndian.Uint64(payload[0:8])),
		TsMax: int64(binary.BigEndian.Uint64(payload[8:16])),
	}, nil
}

// ZoneMapBuilder implements OverlayBuilder for the writer side. The
// writer calls AppendCell for each cell appended to the in-flight data
// block, then Snapshot at flush time to produce the overlay payload,
// then Reset for the next block.
type ZoneMapBuilder struct {
	zm *ZoneMap
}

// NewZoneMapBuilder returns a fresh ZoneMapBuilder ready for the first
// data block.
func NewZoneMapBuilder() *ZoneMapBuilder {
	return &ZoneMapBuilder{zm: NewZoneMap()}
}

// AppendCell on ZoneMapBuilder satisfies OverlayBuilder; defined in
// builder.go so the OverlayBuilder shape stays in one place.

// OverlayType reports which overlay-registry slot this builder produces.
func (b *ZoneMapBuilder) OverlayType() OverlayType { return OverlayZoneMap }

// Snapshot serializes the current block's zone-map. Returns nil if no
// cells have been appended (the caller should not emit an overlay for
// an empty block, though that situation shouldn't arise in practice).
func (b *ZoneMapBuilder) Snapshot() []byte {
	if b.zm.Empty() {
		return nil
	}
	return b.zm.Encode()
}

// Reset clears state for the next block.
func (b *ZoneMapBuilder) Reset() {
	b.zm = NewZoneMap()
}
