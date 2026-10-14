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

// Test-only bridge: re-exports the package-internal parity-harness
// helpers so the iterator-aware harness in package rfile_test
// (parity_iter_test.go) can drive them. rfile_test must live in a
// separate package because it imports internal/iterrt, and iterrt
// imports internal/rfile — an in-package test importing iterrt would
// be an import cycle.
package rfile

import (
	"errors"
	"io"
)

func isEOF(err error) bool { return errors.Is(err, io.EOF) }

// HarnessCell is one (Key, Value) pair flowing through the parity
// harness. Re-export of the unexported cell type.
type HarnessCell struct {
	K *Key
	V []byte
}

func toHarnessCells(cs []cell) []HarnessCell {
	out := make([]HarnessCell, len(cs))
	for i, c := range cs {
		out[i] = HarnessCell{K: c.K, V: c.V}
	}
	return out
}

func fromHarnessCells(hs []HarnessCell) []cell {
	out := make([]cell, len(hs))
	for i, h := range hs {
		out[i] = cell{K: h.K, V: h.V}
	}
	return out
}

// HarnessGenCells generates the deterministic parity cell stream.
func HarnessGenCells(cfg ParityConfig) []HarnessCell {
	return toHarnessCells(genCells(cfg))
}

// HarnessWriteRFile writes cells through the shoal RFile writer at path.
func HarnessWriteRFile(path string, cfg ParityConfig, cells []HarnessCell) error {
	return shoalWriteRFile(path, cfg, fromHarnessCells(cells))
}

// HarnessWriteCellStream serializes cells to the shared cell-stream wire
// format that ShoalParityWrite.java consumes. w is any byte sink.
func HarnessWriteCellStream(w interface{ Write([]byte) (int, error) }, cells []HarnessCell) error {
	return writeCellStream(w, fromHarnessCells(cells))
}

// HarnessOpenRFile opens an on-disk RFile through the full shoal stack.
func HarnessOpenRFile(path string) (*Reader, error) { return openRFile(path) }

// HarnessScanAll drains every cell from r in iteration order.
func HarnessScanAll(r *Reader) ([]HarnessCell, error) {
	cs, err := scanAll(r)
	if err != nil {
		return nil, err
	}
	return toHarnessCells(cs), nil
}

// HarnessCellSeqDiff returns "" when a and b are the identical key+value
// sequence, else a human-readable first divergence.
func HarnessCellSeqDiff(a, b []HarnessCell) string {
	return cellSeqDiff(fromHarnessCells(a), fromHarnessCells(b))
}

// HarnessSampleProbeKeys builds n deterministic point-lookup probe keys.
func HarnessSampleProbeKeys(cells []HarnessCell, n int, seed int64) []*Key {
	return sampleProbeKeys(fromHarnessCells(cells), n, seed)
}

// HarnessProbeResult is the outcome of one point lookup: the first cell
// at-or-after the probe (within an optional column-family restriction).
type HarnessProbeResult struct {
	Found bool
	Key   *Key
	Val   []byte
}

// HarnessPointLookupFiltered seeks r to target and returns the first cell
// at-or-after it whose column family is in fetchCFs (empty fetchCFs ==
// no restriction). This extends the C0 seek-key-only probe with the
// fetched-columns axis the design doc's parity spec calls for.
func HarnessPointLookupFiltered(r *Reader, target *Key, fetchCFs [][]byte) (HarnessProbeResult, error) {
	if err := r.Seek(target); err != nil {
		return HarnessProbeResult{}, err
	}
	for {
		k, v, err := r.Next()
		if err != nil {
			if isEOF(err) {
				return HarnessProbeResult{Found: false}, nil
			}
			return HarnessProbeResult{}, err
		}
		if !cfInSet(k.ColumnFamily, fetchCFs) {
			continue
		}
		vc := make([]byte, len(v))
		copy(vc, v)
		return HarnessProbeResult{Found: true, Key: k.Clone(), Val: vc}, nil
	}
}

// cfInSet reports whether cf is in families; empty families == match all.
func cfInSet(cf []byte, families [][]byte) bool {
	if len(families) == 0 {
		return true
	}
	for _, f := range families {
		if string(f) == string(cf) {
			return true
		}
	}
	return false
}
