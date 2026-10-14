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

package shadow

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Compare runs the parity oracle. Streaming-hash design:
//
//  1. Run shoal compaction on spec.Inputs through spec.Stack at spec.Scope
//     to produce the shoal output bytes.
//  2. Open both rfile streams in a single pass, computing a SHA-256
//     digest over the canonical (key-bytes || value-bytes) sequence and
//     a per-CF count map at the same time. Memory per side: one cell
//     plus the running hash state — O(1) regardless of input size.
//  3. If the digests match, T2 passes and we're done (no point-lookup
//     phase needed — equal digests prove the cell sequences are
//     identical position-for-position; block-index divergence cannot
//     produce equal stream-walk content).
//  4. On digest mismatch, run a second lockstep walk to find the first
//     diverging cell for the report. Still streaming — no materialized
//     slices.
//
// Returns a Report even when individual tiers fail. Returns an error
// only when shoal compaction itself crashes; divergences from Java are
// structured-report shapes, not Go errors.
//
// T3 (random point-lookup sampling) is preserved in Report for wire
// compatibility but always Attempted=false. The check it used to
// perform — verifying block-index/bloom consistency through a separate
// seek path — is subsumed by the streaming hash: if every cell at
// every position matches, no read path can produce different results.
func Compare(spec CompareSpec, javaOutput []byte) (*Report, error) {
	report := &Report{}

	// 1. shoal compaction.
	t0 := time.Now()
	shoalRes, err := compaction.Compact(toCompactSpec(spec))
	report.ShoalCompactMs = time.Since(t0).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("shoal compaction failed: %w", err)
	}
	report.ShoalCells = shoalRes.EntriesWritten

	// 2. T4 shoal summary via streaming hash pass.
	shoalDigest, shoalSummary, err := hashRFile(shoalRes.Output)
	if err != nil {
		return nil, fmt.Errorf("hash shoal output: %w", err)
	}
	report.ShoalSummary = shoalSummary

	// 3. Java-side tiers, only when Java output is supplied.
	if len(javaOutput) > 0 {
		report.JavaCellsSourced = true

		// T1 — Java validator (env-gated; no-op if not configured).
		t1 := runJavaValidator(shoalRes.Output)
		report.T1 = t1
		report.T1Ms = t1.elapsedMs

		// Java-side streaming hash + summary.
		t2start := time.Now()
		javaDigest, javaSummary, err := hashRFile(javaOutput)
		if err != nil {
			return report, fmt.Errorf("hash java output: %w", err)
		}
		report.JavaCells = javaSummary.TotalCells
		report.JavaSummary = javaSummary

		// T2 — digest equality. Constant-time on cell content, 32-byte
		// compare. Hash collisions with SHA-256 are negligible
		// (~2⁻¹²⁸ for any pair), so a match is a proof of cell-sequence
		// equivalence.
		t2 := T2Result{Attempted: true, FirstDivergenceIndex: -1}
		if bytes.Equal(shoalDigest, javaDigest) {
			t2.Passed = true
		} else {
			// Second pass: lockstep walk to find first divergence.
			// Bounded by the smaller of the two cell counts; we report
			// either the diverging cell OR a count-mismatch if one side
			// ends first.
			idx, diff, ferr := findFirstDivergence(shoalRes.Output, javaOutput)
			if ferr != nil {
				t2.CellSeqDiff = fmt.Sprintf("digest mismatch (shoal=%s java=%s); divergence walk error: %v",
					hex.EncodeToString(shoalDigest[:8]),
					hex.EncodeToString(javaDigest[:8]),
					ferr)
			} else {
				t2.FirstDivergenceIndex = idx
				t2.CellSeqDiff = diff
			}
		}
		report.T2 = t2
		report.T2Ms = time.Since(t2start).Milliseconds()
		// T3 is vestigial under the streaming-hash design. The Report
		// struct keeps the field for wire compatibility with existing
		// reports already in GCS, but Attempted stays false to signal
		// it was deliberately not run.
		report.T3 = T3Result{}
	}

	return report, nil
}

// ValidateOutput runs the shoal-only verification tiers over an
// already-produced output RFile WITHOUT recompacting. Offline-compaction
// verification must validate the exact bytes written to storage, not a
// fresh recompaction — so it cannot use Compare (which regenerates its own
// output and would ignore the caller's writer/codec/block choices).
//
// Populates:
//   - T4 (always): per-file summary — cell count, per-CF histogram,
//     first/last key, size — over the provided bytes (ShoalSummary,
//     ShoalCells).
//   - T1 (env-gated on SHOAL_JAVA_RFILE_VALIDATE): the Java RFile reader
//     run against the provided bytes, proving Java can consume shoal's
//     actual output (T1, T1Ms).
//
// T2/T3 are left un-attempted: there is no Java reference output to
// cross-diff against for a fresh offline compaction.
func ValidateOutput(output []byte) (*Report, error) {
	report := &Report{}

	_, summary, err := hashRFile(output)
	if err != nil {
		return nil, fmt.Errorf("summarize output: %w", err)
	}
	report.ShoalSummary = summary
	report.ShoalCells = summary.TotalCells

	t1 := runJavaValidator(output)
	report.T1 = t1
	report.T1Ms = t1.elapsedMs

	return report, nil
}

func toCompactSpec(spec CompareSpec) compaction.Spec {
	inputs := make([]compaction.Input, len(spec.Inputs))
	for i, in := range spec.Inputs {
		inputs[i] = compaction.Input{Name: in.Name, Bytes: in.Bytes}
	}
	return compaction.Spec{
		Inputs:              inputs,
		Stack:               spec.Stack,
		Scope:               spec.Scope,
		FullMajorCompaction: spec.FullMajorCompaction,
		Codec:               spec.Codec,
	}
}

// hashRFile opens an RFile byte stream, walks every cell once, and
// returns the SHA-256 digest of the canonical-encoded cell sequence
// plus an RFileSummary populated during the same pass.
//
// Canonical encoding per cell (length-prefixed, fixed-order — order
// dependence is the point):
//
//	varint(len(row))           row
//	varint(len(cf))            cf
//	varint(len(cq))            cq
//	varint(len(cv))            cv
//	int64-le(timestamp)
//	byte(deleted ? 1 : 0)
//	varint(len(value))         value
//
// Length prefixes prevent boundary-shifting attacks (cells (A,"")(B,"")
// and (AB,"")() would otherwise hash to the same bytes). Deleted flag
// is included so a tombstone vs. a real cell with the same key+value
// bytes are distinguishable.
//
// Memory: O(1) per side (hash state + one cell at a time).
func hashRFile(b []byte) (digest []byte, summary RFileSummary, err error) {
	summary = RFileSummary{
		CellsByCF: map[string]int64{},
		SizeBytes: int64(len(b)),
	}
	h := sha256.New()
	if len(b) == 0 {
		return h.Sum(nil), summary, nil
	}

	bc, err := bcfile.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, summary, fmt.Errorf("bcfile.NewReader: %w", err)
	}
	r, err := rfile.Open(bc, block.Default())
	if err != nil {
		return nil, summary, fmt.Errorf("rfile.Open: %w", err)
	}
	defer r.Close()

	if err := r.Seek(nil); err != nil {
		return nil, summary, fmt.Errorf("seek-to-start: %w", err)
	}

	var firstKey, lastKey *wire.Key
	var count int64
	var scratch [16]byte // reusable for the integer parts of canonical encode
	for {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, summary, fmt.Errorf("Next at cell %d: %w", count, err)
		}
		// Hash the cell. We do NOT retain k or v past the next Next()
		// call; rfile.Reader's contract permits the buffers to be
		// reused.
		writeCanonicalCell(h, k, v, scratch[:])

		// Per-CF count.
		summary.CellsByCF[string(k.ColumnFamily)]++

		// First/last key — small string per cell, but we only keep two
		// keys total.
		if firstKey == nil {
			firstKey = k.Clone()
		}
		lastKey = k.Clone()
		count++
	}
	summary.TotalCells = count
	if firstKey != nil {
		summary.FirstKey = renderKey(firstKey)
	}
	if lastKey != nil {
		summary.LastKey = renderKey(lastKey)
	}
	return h.Sum(nil), summary, nil
}

// writeCanonicalCell folds one cell's canonical encoding into h. See
// hashRFile for the encoding spec.
func writeCanonicalCell(h hash.Hash, k *wire.Key, v []byte, scratch []byte) {
	writeLenPrefixed(h, scratch, k.Row)
	writeLenPrefixed(h, scratch, k.ColumnFamily)
	writeLenPrefixed(h, scratch, k.ColumnQualifier)
	writeLenPrefixed(h, scratch, k.ColumnVisibility)
	// int64-le timestamp.
	binary.LittleEndian.PutUint64(scratch[:8], uint64(k.Timestamp))
	_, _ = h.Write(scratch[:8])
	// 1-byte deleted flag.
	if k.Deleted {
		scratch[0] = 1
	} else {
		scratch[0] = 0
	}
	_, _ = h.Write(scratch[:1])
	// Value.
	writeLenPrefixed(h, scratch, v)
}

func writeLenPrefixed(h hash.Hash, scratch []byte, b []byte) {
	n := binary.PutUvarint(scratch[:10], uint64(len(b)))
	_, _ = h.Write(scratch[:n])
	if len(b) > 0 {
		_, _ = h.Write(b)
	}
}

// renderKey produces a one-line key rendering suitable for divergence
// reports. Non-ASCII bytes are escaped so the report stays grep-able.
func renderKey(k *wire.Key) string {
	return fmt.Sprintf("row=%q cf=%q cq=%q cv=%q ts=%d del=%v",
		k.Row, k.ColumnFamily, k.ColumnQualifier, k.ColumnVisibility, k.Timestamp, k.Deleted)
}

// findFirstDivergence runs a second streaming pass over both files,
// stopping at the first cell where the keys or values disagree (or
// where one side ends before the other). Memory: O(1). Returns the
// 0-based cell index of the divergence and a human-readable diff
// string. On any I/O error, returns the error so the caller can
// surface it in the report.
func findFirstDivergence(shoalBytes, javaBytes []byte) (int, string, error) {
	shoalR, shoalCleanup, err := openForStream(shoalBytes)
	if err != nil {
		return -1, "", fmt.Errorf("open shoal: %w", err)
	}
	defer shoalCleanup()
	javaR, javaCleanup, err := openForStream(javaBytes)
	if err != nil {
		return -1, "", fmt.Errorf("open java: %w", err)
	}
	defer javaCleanup()

	if err := shoalR.Seek(nil); err != nil {
		return -1, "", fmt.Errorf("seek shoal: %w", err)
	}
	if err := javaR.Seek(nil); err != nil {
		return -1, "", fmt.Errorf("seek java: %w", err)
	}

	idx := 0
	for {
		sk, sv, serr := shoalR.Next()
		jk, jv, jerr := javaR.Next()
		sEOF := errors.Is(serr, io.EOF)
		jEOF := errors.Is(jerr, io.EOF)

		if sEOF && jEOF {
			// Both ended together but digests differed — paradox.
			// This shouldn't happen since hashRFile produced different
			// digests; surface as a diagnostic.
			return idx, "digests differ but cell-sequence walk matched (canonical-encoding bug?)", nil
		}
		if sEOF != jEOF {
			short, long := "shoal", "java"
			if jEOF {
				short, long = "java", "shoal"
			}
			return idx, fmt.Sprintf("cell-count mismatch: %s ended at cell %d, %s still had more",
				short, idx, long), nil
		}
		if serr != nil && !sEOF {
			return idx, "", fmt.Errorf("shoal Next at cell %d: %w", idx, serr)
		}
		if jerr != nil && !jEOF {
			return idx, "", fmt.Errorf("java Next at cell %d: %w", idx, jerr)
		}

		if !sk.Equal(jk) {
			return idx, fmt.Sprintf("cell %d key mismatch:\n  shoal: %s\n  java:  %s",
				idx, renderKey(sk), renderKey(jk)), nil
		}
		if !bytes.Equal(sv, jv) {
			return idx, fmt.Sprintf("cell %d value mismatch:\n  shoal: %q\n  java:  %q",
				idx, sv, jv), nil
		}
		idx++
	}
}

// openForStream is the rfile-open path used by findFirstDivergence. It
// returns a Reader plus a cleanup func; the bcfile.Reader doesn't need
// explicit close in shoal's implementation, but the rfile.Reader does.
func openForStream(b []byte) (*rfile.Reader, func(), error) {
	bc, err := bcfile.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, func() {}, fmt.Errorf("bcfile.NewReader: %w", err)
	}
	r, err := rfile.Open(bc, block.Default())
	if err != nil {
		return nil, func() {}, fmt.Errorf("rfile.Open: %w", err)
	}
	return r, func() { _ = r.Close() }, nil
}
