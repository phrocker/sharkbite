package blockmeta

// OverlayBuilder is the writer-side interface that aggregates per-block
// state during data-block accumulation and emits an overlay payload at
// flush time. The writer's hot path calls AppendCell for every cell
// appended to the in-flight data block; at block-flush time it calls
// Snapshot to materialize the overlay payload; then Reset for the next
// block.
//
// Build a list of OverlayBuilders once at Writer construction; the
// writer calls them all in registration order per cell, and emits one
// Overlay per builder per block.
//
// Implementations must be cheap on the AppendCell hot path — a builder
// that allocates per cell defeats the relkey-level zero-copy work.
// ZoneMapBuilder is a good shape: two int64 compares.
type OverlayBuilder interface {
	// AppendCell records one cell's coordinate + value into the current
	// block's running state. The fields are passed by reference but
	// MUST NOT be retained — the slices are transient. Builders that
	// need persistence copy what they need into builder-owned storage.
	AppendCell(row, cf, cq, cv []byte, ts int64, value []byte)

	// OverlayType reports the registry type this builder produces.
	OverlayType() OverlayType

	// Snapshot serializes the current block's accumulated state. Called
	// at block-flush. Returning nil suppresses emission for this block
	// (e.g., zone-map for an empty block). Returned bytes are owned by
	// the caller.
	Snapshot() []byte

	// Reset clears state for the next block. Called after Snapshot at
	// every flush, including the final flush at Close.
	Reset()
}

// AppendCell on ZoneMapBuilder fits the OverlayBuilder shape — only the
// timestamp matters, the rest are ignored. Adapter wrapper so the same
// builder can satisfy the interface without type assertions in the
// writer's loop.
func (b *ZoneMapBuilder) AppendCell(_, _, _, _ []byte, ts int64, _ []byte) {
	b.zm.Update(ts)
}
