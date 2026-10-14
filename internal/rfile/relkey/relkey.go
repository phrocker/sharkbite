// Package relkey decodes and encodes Accumulo's RelativeKey wire format
// (the prefix-compressed Key encoding used inside every RFile data block).
//
// Reference Java source — this is the source of truth, not the C++:
//
//	core/.../file/rfile/RelativeKey.java
//	  - readFields(DataInput):           lines 167-196
//	  - write(DataOutput):               lines 530-587
//	  - getCommonPrefix(prev, cur):      lines 138-160
//
// Wire format for one RelativeKey:
//
//	byte fieldsSame
//	if PREFIX_COMPRESSION_ENABLED bit set:
//	    byte fieldsPrefixed
//	row, cf, cq, cv: each one of
//	    nothing                                  (if *_SAME bit set)
//	    vint prefixLen, vint suffixLen, suffix bytes  (if *_COMMON_PREFIX set)
//	    vint len, bytes                          (otherwise)
//	timestamp:
//	    nothing                                  (if TS_SAME)
//	    vlong delta (added to prev timestamp)    (if TS_DIFF)
//	    vlong absolute                           (otherwise)
//	deleted: derived from DELETED bit in fieldsSame, no separate byte.
//
// V1 (this file): the Reader operates directly over the decompressed
// block []byte via a concrete cursor (not io.Reader/io.ByteReader),
// removes per-byte itab dispatch, and produces zero-copy Key views —
// slice fields point into the block buffer for SAME and NEW cases.
// COMMON_PREFIX still allocates because prefix+suffix aren't contiguous
// on the wire. Filter pushdown via Reader.SetFilter skips value
// materialization for rejected cells.
//
// Lifetime contract: Keys returned by NextView are TRANSIENT — slice
// fields alias the block buffer + are invalidated on the next NextView
// call. Callers that need to retain a Key past the next call must
// Clone(). Backwards-compat Next() Clones internally + copies the
// value, so existing call sites are unaffected.
package relkey

import (
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Flag bits in the first ("fieldsSame") byte. Mirrors RelativeKey.java:44-51.
const (
	RowSame                  byte = 1 << 0
	CFSame                   byte = 1 << 1
	CQSame                   byte = 1 << 2
	CVSame                   byte = 1 << 3
	TSSame                   byte = 1 << 4
	Deleted                  byte = 1 << 5
	PrefixCompressionEnabled byte = 1 << 7
)

// Flag bits in the second ("fieldsPrefixed") byte. Mirrors
// RelativeKey.java:54-58.
const (
	RowCommonPrefix byte = 1 << 0
	CFCommonPrefix  byte = 1 << 1
	CQCommonPrefix  byte = 1 << 2
	CVCommonPrefix  byte = 1 << 3
	TSDiff          byte = 1 << 4
)

// Key is an alias of wire.Key so callers can treat the relkey package as
// self-contained. We don't redeclare the struct because every other reader
// in shoal will want to consume wire.Key directly.
type Key = wire.Key

// Filter is a caller-provided predicate. It's called inside NextView
// after the Key is fully decoded but BEFORE the value is read. Returning
// false causes the Reader to advance past the value bytes without
// allocating + copying them, then continue to the next cell.
//
// CRITICAL: the Key passed to a Filter is TRANSIENT. Its slice fields
// point into the decompressed block buffer (or a per-Reader scratch
// for COMMON_PREFIX cases) and are invalid after Filter returns.
// Filters must not retain the *Key; they may inspect bytes, copy what
// they need, but must not store the Key itself.
//
// V0 use case: visibility-evaluator dispatch — the filter looks at
// k.ColumnVisibility and decides accept/reject by evaluating the
// scan's authorizations against the cell's CV expression.
type Filter func(k *Key) bool

// Reader decodes RelativeKeys from a single decompressed RFile data
// block. Construct with NewReader, drive with Next or NextView.
//
// Concurrency: a Reader is single-consumer. Don't share across goroutines.
type Reader struct {
	cur     rcursor
	entries int
	read    int

	// prev is the last decoded cell's resolved key. Slice fields point
	// into cur.buf (zero-copy) for SAME and NEW; into a per-Reader
	// alloc for COMMON_PREFIX. The struct is reused across decodes;
	// when we update prev = current after decoding, only slice headers
	// move (no underlying byte copy).
	prev    Key
	hasPrev bool

	// current is the in-flight Key that NextView returns by pointer.
	// Caller treats as read-only + transient.
	current Key

	// Per-field scratch for COMMON_PREFIX results. Reused across decodes
	// instead of allocating a fresh slice per prefixed field — the dominant
	// point-lookup allocation, since a skip-scan decodes every cell before
	// the target and adjacent rows almost always share a common prefix.
	//
	// Safe to reuse in-place: COMMON_PREFIX only reads prev[:prefixLen]
	// (the front of the previous field, which a scratch-backed slice always
	// occupies starting at offset 0), then writes the suffix from
	// prefixLen onward. The prefix bytes we copy are never the bytes we
	// overwrite, so building directly into the buffer prev points at is
	// correct. Consistent with the documented contract that COMMON_PREFIX
	// fields live in per-Reader scratch valid only until the next decode.
	rowScratch []byte
	cfScratch  []byte
	cqScratch  []byte
	cvScratch  []byte

	filter Filter
	err    error
}

// NewReader constructs a Reader over a decompressed block buffer.
// `entries` is the cell count from the parent IndexEntry.NumEntries.
// The buffer must remain alive (no mutation) for as long as any
// returned Key or value is in use; for backwards-compat Next() this
// is automatic since outputs are cloned.
func NewReader(buf []byte, entries int) *Reader {
	return &Reader{cur: rcursor{buf: buf}, entries: entries}
}

// SetFilter installs a per-Reader filter predicate. nil disables
// filtering (every cell accepted). Calling SetFilter mid-iteration is
// safe — the next decode picks up the new predicate.
func (r *Reader) SetFilter(f Filter) { r.filter = f }

// Remaining returns the number of cells the block still owes us
// (entries - cells consumed, including filter-rejected ones).
func (r *Reader) Remaining() int { return r.entries - r.read }

// Err returns the sticky terminal error, if any. Returns nil after a
// clean EOF (Remaining == 0).
func (r *Reader) Err() error {
	if errors.Is(r.err, io.EOF) {
		return nil
	}
	return r.err
}

// Next decodes the next cell, returning a freshly-allocated Key + value
// owned by the caller. Backwards-compat with the V0 API. Internally
// calls NextView and clones the outputs.
func (r *Reader) Next() (*Key, []byte, error) {
	k, v, err := r.NextView()
	if err != nil {
		return nil, nil, err
	}
	clonedV := make([]byte, len(v))
	copy(clonedV, v)
	return k.Clone(), clonedV, nil
}

// NextView decodes the next cell and returns a TRANSIENT view: the Key's
// slice fields and the value slice both point into the block buffer (or,
// for COMMON_PREFIX fields, into a per-Reader scratch alloc kept alive
// until the next NextView call rewrites it). The caller must not retain
// either past the next call without copying.
//
// If a Filter is installed, NextView loops internally over rejected
// cells: it advances past their value bytes without copying, then
// continues. The returned Key/value pair is always for an accepted cell.
func (r *Reader) NextView() (*Key, []byte, error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	for {
		if r.read >= r.entries {
			r.err = io.EOF
			return nil, nil, io.EOF
		}
		if err := r.decodeKey(&r.current); err != nil {
			r.err = err
			return nil, nil, err
		}
		valLen, ok := r.cur.int32be()
		if !ok {
			r.err = io.ErrUnexpectedEOF
			return nil, nil, r.err
		}
		if valLen < 0 {
			r.err = fmt.Errorf("relkey: negative value length %d", valLen)
			return nil, nil, r.err
		}
		// Update prev BEFORE the filter check — the next cell's prefix-
		// decompression depends on this cell's resolved Key regardless
		// of accept/reject. Struct copy moves the slice headers (no
		// underlying byte copy); the slices themselves point at the
		// same memory r.current's slices do.
		r.prev = r.current
		r.hasPrev = true
		r.read++

		if r.filter != nil && !r.filter(&r.current) {
			if !r.cur.skip(int(valLen)) {
				r.err = io.ErrUnexpectedEOF
				return nil, nil, r.err
			}
			continue
		}

		val, ok := r.cur.sliceN(int(valLen))
		if !ok {
			r.err = io.ErrUnexpectedEOF
			return nil, nil, r.err
		}
		return &r.current, val, nil
	}
}

// decodeKey reads one RelativeKey, resolves it against r.prev, and
// fills *out. SAME fields share slice headers with r.prev (no copy);
// NEW fields slice into cur.buf (no copy); COMMON_PREFIX fields
// allocate fresh (prefix from prev + suffix from cur.buf).
func (r *Reader) decodeKey(out *Key) error {
	fieldsSame, ok := r.cur.byteAt()
	if !ok {
		return io.ErrUnexpectedEOF
	}
	var fieldsPrefixed byte
	if fieldsSame&PrefixCompressionEnabled != 0 {
		fieldsPrefixed, ok = r.cur.byteAt()
		if !ok {
			return io.ErrUnexpectedEOF
		}
	}
	if !r.hasPrev &&
		(fieldsSame&(RowSame|CFSame|CQSame|CVSame|TSSame) != 0 ||
			fieldsPrefixed&(RowCommonPrefix|CFCommonPrefix|CQCommonPrefix|CVCommonPrefix|TSDiff) != 0) {
		return errors.New("relkey: first key in block has same/prefix bits set")
	}

	row, err := r.field(fieldsSame, fieldsPrefixed, RowSame, RowCommonPrefix, r.prev.Row, &r.rowScratch)
	if err != nil {
		return fmt.Errorf("row: %w", err)
	}
	cf, err := r.field(fieldsSame, fieldsPrefixed, CFSame, CFCommonPrefix, r.prev.ColumnFamily, &r.cfScratch)
	if err != nil {
		return fmt.Errorf("cf: %w", err)
	}
	cq, err := r.field(fieldsSame, fieldsPrefixed, CQSame, CQCommonPrefix, r.prev.ColumnQualifier, &r.cqScratch)
	if err != nil {
		return fmt.Errorf("cq: %w", err)
	}
	cv, err := r.field(fieldsSame, fieldsPrefixed, CVSame, CVCommonPrefix, r.prev.ColumnVisibility, &r.cvScratch)
	if err != nil {
		return fmt.Errorf("cv: %w", err)
	}

	var ts int64
	switch {
	case fieldsSame&TSSame != 0:
		ts = r.prev.Timestamp
	case fieldsPrefixed&TSDiff != 0:
		delta, ok := r.cur.vlong()
		if !ok {
			return io.ErrUnexpectedEOF
		}
		ts = delta + r.prev.Timestamp
	default:
		v, ok := r.cur.vlong()
		if !ok {
			return io.ErrUnexpectedEOF
		}
		ts = v
	}

	out.Row = row
	out.ColumnFamily = cf
	out.ColumnQualifier = cq
	out.ColumnVisibility = cv
	out.Timestamp = ts
	out.Deleted = fieldsSame&Deleted != 0
	return nil
}

// field decodes one of (Row, CF, CQ, CV) per the SAME / COMMON_PREFIX /
// NEW dispatch. Allocation behaviour:
//
//	SAME:           share slice header with prev — zero alloc.
//	NEW:            slice into cur.buf — zero alloc.
//	COMMON_PREFIX:  build prefix(from prev) + suffix(from cur.buf) into
//	                the per-field reuse buffer *scratch — zero alloc in
//	                steady state (only grows when a field gets longer).
//	                Building in-place is safe because the prefix is read
//	                from the front of prev and the suffix is written after
//	                it, so bytes we copy are never bytes we overwrite.
func (r *Reader) field(fieldsSame, fieldsPrefixed, sameBit, prefixBit byte, prev []byte, scratch *[]byte) ([]byte, error) {
	if fieldsSame&sameBit != 0 {
		// Zero-copy: share prev's slice header. Safe because prev's slice
		// points at memory that's alive for the rest of this block (either
		// cur.buf, or a COMMON_PREFIX scratch kept reachable through prev).
		return prev, nil
	}
	if fieldsPrefixed&prefixBit != 0 {
		prefixLen, ok := r.cur.vint()
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		if prefixLen < 0 || int(prefixLen) > len(prev) {
			return nil, fmt.Errorf("prefix len %d out of range (prev=%d bytes)", prefixLen, len(prev))
		}
		suffixLen, ok := r.cur.vint()
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		if suffixLen < 0 {
			return nil, fmt.Errorf("negative suffix len %d", suffixLen)
		}
		suffix, ok := r.cur.sliceN(int(suffixLen))
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		need := int(prefixLen) + int(suffixLen)
		buf := *scratch
		if cap(buf) < need {
			// Grow. Copy the prefix from prev into the fresh buffer before
			// we drop the old one — prev may alias the old scratch.
			nb := make([]byte, need)
			copy(nb, prev[:prefixLen])
			buf = nb
		} else {
			buf = buf[:need]
			// In-place: copy(buf[:prefixLen], prev[:prefixLen]) is a no-op
			// when prev already backs buf at offset 0, and a safe forward
			// copy otherwise.
			copy(buf[:prefixLen], prev[:prefixLen])
		}
		copy(buf[prefixLen:], suffix)
		*scratch = buf
		return buf, nil
	}
	n, ok := r.cur.vint()
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	if n < 0 {
		return nil, fmt.Errorf("negative field length %d", n)
	}
	out, ok := r.cur.sliceN(int(n))
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return out, nil
}
