// RFile parity harness — the gating piece for shoal compactors (design
// doc: platform/shoal/docs/compactor-and-wal-reads-design.md, "RFile
// parity harness"). The single biggest risk in shoal compactors is a
// byte-divergent RFile: a Java tserver reading a shoal-written RFile
// must produce identical results to reading a Java-written RFile from
// the same inputs.
//
// This file holds the reusable, JVM-free machinery:
//   - a deterministic cell-stream generator + binary codec, shared
//     verbatim with the Java side (ShoalParityWrite.java reads exactly
//     this wire format),
//   - shoal-writer invocation,
//   - all-cell scan + point-lookup comparison primitives that operate
//     on any *Reader.
//
// The JVM-gated assertions (shoal_rfile vs java_rfile) live in
// parity_harness_test.go; everything here runs unconditionally.
//
// Scope of this first cut: IDENTITY-COMPACTION parity — same cell stream
// in, no user iterators (just the VersioningIterator-passthrough
// equivalent: cells are emitted as-is). Iterator-stack parmeterization
// is a C1+ concern; the ParityConfig.Iterators field is reserved for it.
package rfile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"os"
	"sort"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// cellStreamMagic / cellStreamVersion gate the shared cell-stream wire
// format. ShoalParityWrite.java carries the identical constants — any
// drift here MUST be mirrored there or the Java side rejects the input.
const (
	cellStreamMagic   = 0x53484c43 // "SHLC"
	cellStreamVersion = 1
)

// ParityConfig describes one parity-harness scenario.
type ParityConfig struct {
	// Seed makes the generated cell stream deterministic — two runs with
	// the same Seed produce byte-identical input, which is what lets the
	// shoal and Java sides write "the same input".
	Seed int64

	// Cells is the number of cells in the generated stream.
	Cells int

	// Codec is the data-block compression codec ("none" | "gz" |
	// "snappy"). Passed to both the shoal writer and ShoalParityWrite.
	Codec string

	// BlockSize / IndexBlockSize tune the writers. Zero falls back to the
	// writer defaults. Block boundaries are allowed to differ between the
	// two writers (the design doc permits "block-boundary flexibility");
	// passing the SAME values to both just keeps divergence small and the
	// harness output easy to eyeball.
	BlockSize      int
	IndexBlockSize int

	// Lookups is the number of random point lookups to sample in the
	// cross-reader comparison. The design doc calls for ~10K.
	Lookups int

	// Iterators is the iterator-stack config the compactor applies to the
	// generated cell stream BEFORE write. Each entry is one iterator,
	// "name" or "name:k=v,k=v" (e.g. "versioning:maxVersions=2"). The
	// stack is applied bottom-up: Iterators[0] sits directly on the
	// cell-stream leaf. Empty == identity compaction (cells pass through
	// untouched), the C0 behaviour.
	//
	// Wiring (C1) lives in parity_iter_test.go (external package
	// rfile_test, to avoid an iterrt<->rfile import cycle):
	// applyIteratorStack runs the stack in Go and produces the
	// POST-iterator cell stream. BOTH the shoal writer and the Java
	// writer (ShoalParityWrite) then consume that same post-iterator
	// stream — so the harness asserts shoal-iterators+shoal-writer is
	// semantically identical to the Java writer fed the same logical
	// cells. Shoal-iterator-vs-Java-iterator parity is validated
	// separately by internal/iterrt's unit tests (cross-checked against
	// the Java iterator source); see the report's "Java-side gap" note.
	Iterators []string

	// IteratorScope is the compaction/scan scope the iterator stack runs
	// in: 0=scan, 1=minc, 2=majc (matches iterrt.IteratorScope's order;
	// kept as an int here so this struct stays in package rfile without
	// importing iterrt). Only meaningful when Iterators is non-empty.
	IteratorScope int

	// Authorizations is the auth set handed to a visibility-filter
	// iterator in the stack. Only consulted at scan scope.
	Authorizations [][]byte
}

// genCells produces the deterministic cell stream for cfg. The stream is
// already in non-decreasing Key order (Append / RFile.Writer both
// require it). Rows are zero-padded so lexical order == numeric order;
// every Nth cell carries a non-empty visibility + an older duplicate
// timestamp so the harness exercises cv-bytes and the
// timestamp-descending tiebreak.
func genCells(cfg ParityConfig) []cell {
	rng := rand.New(rand.NewSource(cfg.Seed))
	cells := make([]cell, 0, cfg.Cells)
	for i := 0; i < cfg.Cells; i++ {
		row := []byte(fmt.Sprintf("row%08d", i))
		cf := []byte("cf")
		cq := []byte(fmt.Sprintf("cq%03d", i%7))
		var cv []byte
		if i%5 == 0 {
			cv = []byte("vis")
		}
		// Random-length but deterministic value.
		vlen := 4 + rng.Intn(60)
		val := make([]byte, vlen)
		for j := range val {
			val[j] = byte('a' + rng.Intn(26))
		}
		cells = append(cells, cell{
			K: &Key{
				Row:              row,
				ColumnFamily:     cf,
				ColumnQualifier:  cq,
				ColumnVisibility: cv,
				Timestamp:        int64(1_000_000 - i),
			},
			V: val,
		})
	}
	return cells
}

// writeCellStream serializes cells to the shared binary cell-stream
// format that ShoalParityWrite.java consumes. All integers big-endian
// to match java.io.DataInput.
func writeCellStream(w io.Writer, cells []cell) error {
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[0:4], cellStreamMagic)
	binary.BigEndian.PutUint32(hdr[4:8], cellStreamVersion)
	binary.BigEndian.PutUint64(hdr[8:16], uint64(len(cells)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var num [8]byte
	field := func(b []byte) error {
		binary.BigEndian.PutUint32(num[0:4], uint32(len(b)))
		if _, err := w.Write(num[0:4]); err != nil {
			return err
		}
		_, err := w.Write(b)
		return err
	}
	for _, c := range cells {
		if err := field(c.K.Row); err != nil {
			return err
		}
		if err := field(c.K.ColumnFamily); err != nil {
			return err
		}
		if err := field(c.K.ColumnQualifier); err != nil {
			return err
		}
		if err := field(c.K.ColumnVisibility); err != nil {
			return err
		}
		binary.BigEndian.PutUint64(num[0:8], uint64(c.K.Timestamp))
		if _, err := w.Write(num[0:8]); err != nil {
			return err
		}
		var del byte
		if c.K.Deleted {
			del = 1
		}
		if _, err := w.Write([]byte{del}); err != nil {
			return err
		}
		if err := field(c.V); err != nil {
			return err
		}
	}
	return nil
}

// shoalWriteRFile writes cells through the shoal Writer at path, using
// cfg's codec + block sizing. This is the "shoal_rfile" side of the
// harness.
func shoalWriteRFile(path string, cfg ParityConfig, cells []cell) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	w, err := NewWriter(f, WriterOptions{
		Codec:          cfg.Codec,
		BlockSize:      cfg.BlockSize,
		IndexBlockSize: cfg.IndexBlockSize,
	})
	if err != nil {
		return fmt.Errorf("NewWriter: %w", err)
	}
	for i, c := range cells {
		if err := w.Append(c.K, c.V); err != nil {
			return fmt.Errorf("Append %d: %w", i, err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("Writer.Close: %w", err)
	}
	return nil
}

// openRFile opens an on-disk RFile path through the full shoal stack
// (bcfile + default decompressor). Caller must Close the returned
// Reader. The file bytes are read fully into memory — parity fixtures
// are small by construction.
func openRFile(path string) (*Reader, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		return nil, fmt.Errorf("bcfile.NewReader: %w", err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		return nil, fmt.Errorf("rfile.Open: %w", err)
	}
	return r, nil
}

// scanAll drains every cell from r in iteration order. Returns a
// cloned, owned slice — safe to retain past r's lifetime.
func scanAll(r *Reader) ([]cell, error) {
	if err := r.Seek(nil); err != nil {
		return nil, fmt.Errorf("Seek(nil): %w", err)
	}
	var out []cell
	for {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Next at cell %d: %w", len(out), err)
		}
		vc := make([]byte, len(v))
		copy(vc, v)
		out = append(out, cell{K: k.Clone(), V: vc})
	}
	return out, nil
}

// cellSeqDiff returns "" when a and b are the identical key+value
// sequence, otherwise a human-readable description of the first
// divergence. This is assertion (d) — "all-row scan produces identical
// key sequence + value sequence".
func cellSeqDiff(a, b []cell) string {
	if len(a) != len(b) {
		return fmt.Sprintf("cell count mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !a[i].K.Equal(b[i].K) {
			return fmt.Sprintf("cell %d key mismatch:\n  a: %+v\n  b: %+v", i, a[i].K, b[i].K)
		}
		if !bytes.Equal(a[i].V, b[i].V) {
			return fmt.Sprintf("cell %d value mismatch:\n  a: %q\n  b: %q", i, a[i].V, b[i].V)
		}
	}
	return ""
}

// scanFingerprint is an order-sensitive FNV-1a hash over the full
// key+value sequence. Used for the determinism check (two shoal writes
// of the same input must produce the same scan) without retaining two
// full cell slices.
func scanFingerprint(cells []cell) uint64 {
	h := fnv.New64a()
	var num [8]byte
	mix := func(b []byte) {
		binary.BigEndian.PutUint64(num[:], uint64(len(b)))
		_, _ = h.Write(num[:])
		_, _ = h.Write(b)
	}
	for _, c := range cells {
		mix(c.K.Row)
		mix(c.K.ColumnFamily)
		mix(c.K.ColumnQualifier)
		mix(c.K.ColumnVisibility)
		binary.BigEndian.PutUint64(num[:], uint64(c.K.Timestamp))
		_, _ = h.Write(num[:])
		if c.K.Deleted {
			_, _ = h.Write([]byte{1})
		} else {
			_, _ = h.Write([]byte{0})
		}
		mix(c.V)
	}
	return h.Sum64()
}

// lookupResult is the outcome of a single point lookup: the first cell
// at-or-after the probe key, or notFound.
type lookupResult struct {
	found bool
	key   *Key
	val   []byte
}

func (lr lookupResult) diff(o lookupResult) string {
	if lr.found != o.found {
		return fmt.Sprintf("found mismatch: %v vs %v", lr.found, o.found)
	}
	if !lr.found {
		return ""
	}
	if !lr.key.Equal(o.key) {
		return fmt.Sprintf("key mismatch:\n  a: %+v\n  b: %+v", lr.key, o.key)
	}
	if !bytes.Equal(lr.val, o.val) {
		return fmt.Sprintf("value mismatch:\n  a: %q\n  b: %q", lr.val, o.val)
	}
	return ""
}

// pointLookup seeks r to target and returns the first cell at-or-after
// it. This is the iterator-output primitive for assertion (b): "for a
// random sample of key lookups, every (range, fetched cols, auths)
// tuple produces the same iterator output against both files". The C0
// cut samples the (seek-key) axis; fetched-cols / auths axes attach in
// C1 when the iterator runtime can express them.
func pointLookup(r *Reader, target *Key) (lookupResult, error) {
	if err := r.Seek(target); err != nil {
		return lookupResult{}, fmt.Errorf("Seek: %w", err)
	}
	k, v, err := r.Next()
	if errors.Is(err, io.EOF) {
		return lookupResult{found: false}, nil
	}
	if err != nil {
		return lookupResult{}, fmt.Errorf("Next: %w", err)
	}
	vc := make([]byte, len(v))
	copy(vc, v)
	return lookupResult{found: true, key: k.Clone(), val: vc}, nil
}

// sampleProbeKeys builds n deterministic probe keys for point-lookup
// comparison. Mix of: exact keys present in the stream, prefix-only
// (row) keys, and synthetic between-rows keys that land on no exact
// cell — so the harness exercises both hit and skip-to-next paths.
func sampleProbeKeys(cells []cell, n int, seed int64) []*Key {
	if len(cells) == 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(seed))
	probes := make([]*Key, 0, n)
	for i := 0; i < n; i++ {
		base := cells[rng.Intn(len(cells))]
		switch rng.Intn(3) {
		case 0:
			// Exact key.
			probes = append(probes, base.K.Clone())
		case 1:
			// Row-prefix only — Seek lands on the first cell of the row.
			probes = append(probes, &Key{Row: append([]byte(nil), base.K.Row...)})
		default:
			// Synthetic between-rows key: base row with a trailing byte,
			// which sorts strictly after every cell in base's row.
			probes = append(probes, &Key{Row: append(append([]byte(nil), base.K.Row...), 0xff)})
		}
	}
	return probes
}

// compareReaders runs the cross-reader assertions (b) + (d) for two
// readers that should be semantically equivalent. shoalR and javaR are
// re-seekable Readers over the two RFiles. Returns "" on full parity,
// else the first divergence found.
//
// Assertion (a) byte-identity and (c) metadata-via-Java-reader are NOT
// here: (a) is checked directly on the file bytes by the caller, and
// (c) requires the JVM and lives in the gated test.
func compareReaders(shoalR, javaR *Reader, probes []*Key) (string, error) {
	shoalCells, err := scanAll(shoalR)
	if err != nil {
		return "", fmt.Errorf("scan shoal rfile: %w", err)
	}
	javaCells, err := scanAll(javaR)
	if err != nil {
		return "", fmt.Errorf("scan java rfile: %w", err)
	}
	if d := cellSeqDiff(shoalCells, javaCells); d != "" {
		return "all-row scan divergence: " + d, nil
	}
	for i, p := range probes {
		sl, err := pointLookup(shoalR, p)
		if err != nil {
			return "", fmt.Errorf("shoal lookup %d: %w", i, err)
		}
		jl, err := pointLookup(javaR, p)
		if err != nil {
			return "", fmt.Errorf("java lookup %d: %w", i, err)
		}
		if d := sl.diff(jl); d != "" {
			return fmt.Sprintf("point-lookup %d (probe %+v) divergence: %s", i, p, d), nil
		}
	}
	return "", nil
}

// metadataSummary is a compact, comparable view of an RFile's
// directory-level metadata: the locality-group shape the Java RFile
// reader also exposes. Used for assertion (c) — when the Java reader is
// available the gated test cross-checks these against PrintInfo output;
// the pure-Go path at least asserts shoal's own writer/reader agree.
type metadataSummary struct {
	NumLocalityGroups int
	DefaultLGEntries  int32 // NumTotalEntries (== leaf data-block count)
	RootIndexLevel    int32
	FirstRow          string
	HasBlockMeta      bool
}

func readMetadataSummary(r *Reader) metadataSummary {
	lg := r.LocalityGroup()
	ms := metadataSummary{
		NumLocalityGroups: len(r.IndexReader().Groups),
		DefaultLGEntries:  lg.NumTotalEntries,
		HasBlockMeta:      r.BlockMeta() != nil,
	}
	if lg.RootIndex != nil {
		ms.RootIndexLevel = lg.RootIndex.Level
	}
	if lg.FirstKey != nil {
		ms.FirstRow = string(lg.FirstKey.Row)
	}
	return ms
}

// sortCellsStable is a defensive helper for callers that build cell
// streams from a map or other unordered source — the writers REQUIRE
// non-decreasing Key order. genCells already emits sorted output, so
// the harness itself doesn't call this; it's exported-adjacent for the
// C1 iterator-port harness, which will merge multiple input files.
func sortCellsStable(cells []cell) {
	sort.SliceStable(cells, func(i, j int) bool {
		return cells[i].K.Compare(cells[j].K) < 0
	})
}
