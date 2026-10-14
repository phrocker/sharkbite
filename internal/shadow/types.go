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

// Package shadow is shoal's correctness oracle for compactions. Given a
// set of (inputs, iterator stack, scope) plus the RFile bytes Java
// produced from the same inputs, Compare runs the four-tier parity
// suite (T1 Java cross-read, T2 cell-sequence, T3 random point lookup,
// T4 metadata summary) and returns a structured Report.
//
// This package is pure over byte slices — no I/O, no goroutines. The
// Phase 2 service binary fetches bytes from GCS, calls Compare, then
// uploads the Report. Keeping the oracle pure means Phase 2's poller
// can race compactions in parallel without sharing state.
package shadow

import "github.com/phrocker/shoal-oss/internal/iterrt"

// CompareSpec describes one shadow-comparison unit. Fields mirror
// compaction.Spec — the oracle runs the same iterator pipeline on the
// inputs, then diffs against the supplied Java output bytes.
type CompareSpec struct {
	// Inputs are the RFile byte streams that were the compaction inputs.
	Inputs []InputBlob

	// Stack is the iterator stack the compaction applied. Built from
	// table.iterator.<scope>.* properties in priority order (lowest
	// first), matching Accumulo's IteratorEnvironment semantics.
	Stack []iterrt.IterSpec

	// Scope identifies the compaction context — ScopeMinc or ScopeMajc.
	Scope iterrt.IteratorScope

	// FullMajorCompaction is true when the compaction's output is the
	// tablet's sole remaining file. Threaded into IteratorEnvironment;
	// only matters for delete-aware iterators (DeletingIterator drops
	// tombstones iff this is true at ScopeMajc).
	FullMajorCompaction bool

	// Codec controls the output RFile's block compression. Empty = none.
	// Mismatch between shoal's and Java's codec choice doesn't affect
	// cell-content equivalence — T2 and T3 work after decompression.
	Codec string

	// LookupSamples bounds T3's random point-lookup probe count. Zero
	// uses DefaultLookupSamples (10000). Tests typically set a small
	// value (100) to keep runtime bounded.
	LookupSamples int

	// LookupSeed pins T3's RNG seed so a divergence report can be
	// reproduced from the same inputs. Zero is a valid seed.
	LookupSeed int64
}

// InputBlob is one compaction input. Name is a human-readable handle
// (e.g. the GCS path) used in divergence reports — not parsed by the
// oracle. Bytes is the raw RFile content.
type InputBlob struct {
	Name  string
	Bytes []byte
}

// DefaultLookupSamples is the T3 probe count when CompareSpec.LookupSamples
// is zero. 10K matches the design-doc "random sample of 10K key lookups"
// gate from the C0 parity harness.
const DefaultLookupSamples = 10000

// Report is the structured result of a single Compare call. Tier
// outcomes are independent — a T1 failure does NOT short-circuit T2/T3.
// The service binary in Phase 2 can decide which tier failures alert on.
type Report struct {
	// Shoal-side stats.
	ShoalCells int64
	// Java-side stats.
	JavaCells int64
	// JavaCellsSourced is true iff the caller supplied a non-empty Java
	// output blob. T2/T3 are skipped when false (the oracle ran on the
	// inputs alone — useful for "shoal-only sanity" mode).
	JavaCellsSourced bool

	// T1 — Java reader cross-validation.
	T1 T1Result

	// T2 — cell-sequence equivalence. Skipped when JavaCellsSourced=false.
	T2 T2Result

	// T3 — random point-lookup equivalence. Skipped when JavaCellsSourced=false.
	T3 T3Result

	// T4 — per-file informational summaries. Always populated.
	ShoalSummary RFileSummary
	JavaSummary  RFileSummary

	// Timings (milliseconds) for each phase, useful for service-mode
	// metrics. Phase-1 CLI just logs them.
	ShoalCompactMs int64
	T1Ms           int64
	T2Ms           int64
	T3Ms           int64
}

// T1Result reports whether Java's RFile reader accepts shoal's output.
// Optional — only populated when an external Java validator is wired in
// (env var SHOAL_JAVA_RFILE_VALIDATE; see java_validate.go).
type T1Result struct {
	// Attempted is false when no validator was configured.
	Attempted bool
	// Passed is true iff the Java reader accepted shoal's output.
	Passed bool
	// Error is the validator's stderr/stdout on failure, empty on pass.
	Error string
	// CommandUsed is the shell command line invoked, for diagnostics.
	CommandUsed string
	// elapsedMs is set internally by runJavaValidator; surfaced via
	// Report.T1Ms.
	elapsedMs int64
}

// T2Result reports cell-sequence equivalence between shoal's and Java's
// output. CellSeqDiff is the first divergence found, or "" on full match.
type T2Result struct {
	Attempted   bool
	Passed      bool
	CellSeqDiff string
	// FirstDivergenceIndex is -1 on full match.
	FirstDivergenceIndex int
}

// T3Result reports random point-lookup equivalence. Sampled probes drawn
// from the cell stream; each probed against both shoal and Java output;
// divergence is when seek-at-probe-key returns different (key, value)
// pairs.
type T3Result struct {
	Attempted      bool
	LookupsTotal   int
	LookupsMatched int
	Divergences    []LookupDivergence
}

// LookupDivergence captures one (probe, shoal_result, java_result) tuple
// where the two readers disagreed. ProbeKey is the seek target. Reports
// keep at most 32 divergences to bound memory; the count drives the
// pass/fail decision.
type LookupDivergence struct {
	ProbeKey string // hex-or-printable rendering
	Diff     string // human-readable explanation
}

// RFileSummary is the T4 informational shape — per-file statistics that
// don't gate pass/fail but help diagnose divergences. Populated by a
// single full scan during Compare.
type RFileSummary struct {
	TotalCells int64
	// CellsByCF counts how many cells fell under each column family.
	// Useful when a divergence is concentrated in one CF.
	CellsByCF map[string]int64
	// FirstKey / LastKey rendered as printable strings for the report.
	FirstKey string
	LastKey  string
	// SizeBytes is the original file's byte length (Java) or the
	// generated buffer length (shoal). NOT a content-equivalence signal
	// — block-boundary flexibility is allowed by the C0 contract.
	SizeBytes int64
}
