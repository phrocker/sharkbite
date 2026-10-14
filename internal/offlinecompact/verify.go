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

package offlinecompact

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/shadow"
)

// CellMismatch pins the first position where the written output diverged
// from an independent re-derivation of the compaction stream.
type CellMismatch struct {
	Index  int64
	Reason string
}

func (m *CellMismatch) String() string {
	if m == nil {
		return "<none>"
	}
	return fmt.Sprintf("cell %d: %s", m.Index, m.Reason)
}

// VerifyReport is the outcome of verifying one tablet's output against the
// three independent checks from the design (§5).
type VerifyReport struct {
	// SelfConsistent is check #1: the output RFile's cell sequence is
	// byte-identical to a fresh, independent re-run of the same iterator
	// stack over the same inputs.
	SelfConsistent bool
	// CellsCompared is how many cells the self-consistency walk matched.
	CellsCompared int64
	// Mismatch is the first divergence found by check #1, or nil.
	Mismatch *CellMismatch
	// Shadow carries check #2 (T4 per-file summary — always populated)
	// and check #3 (T1 Java cross-read — attempted only when
	// SHOAL_JAVA_RFILE_VALIDATE is configured). Nil only if the shadow
	// pass itself could not run.
	Shadow *shadow.Report
}

// OK reports whether every applicable check passed. T4 is informational
// and never fails the gate; T1 fails the gate only when it was actually
// attempted (i.e. a Java validator is configured) and rejected the output.
func (r *VerifyReport) OK() bool {
	if r == nil || !r.SelfConsistent || r.Mismatch != nil {
		return false
	}
	if r.Shadow != nil && r.Shadow.T1.Attempted && !r.Shadow.T1.Passed {
		return false
	}
	return true
}

// errStopVerify unwinds StreamCells once the self-consistency walk has its
// answer (a mismatch); it is not a real error.
var errStopVerify = errors.New("offlinecompact: verify stop")

// VerifyTablet runs the §5 verification for one tablet: self-consistency
// (primary gate) plus the shadow oracle's shoal-only tiers (T4 summary and
// the env-gated T1 Java cross-read). spec must be the exact spec used to
// produce output; entriesWritten is the orchestrator's reported cell count.
func VerifyTablet(spec compaction.Spec, output []byte, entriesWritten int64) (*VerifyReport, error) {
	rep := &VerifyReport{}

	mism, compared, err := selfConsistent(spec, output)
	if err != nil {
		return nil, err
	}
	rep.CellsCompared = compared
	rep.Mismatch = mism
	if rep.Mismatch == nil && compared != entriesWritten {
		rep.Mismatch = &CellMismatch{
			Index:  compared,
			Reason: fmt.Sprintf("re-derived %d cells but orchestrator reported %d written", compared, entriesWritten),
		}
	}
	rep.SelfConsistent = rep.Mismatch == nil

	shReport, err := shadow.ValidateOutput(output)
	if err != nil {
		return rep, fmt.Errorf("offlinecompact: shadow verify: %w", err)
	}
	rep.Shadow = shReport

	return rep, nil
}

// selfConsistent walks a fresh re-derivation of the compaction stream
// (StreamCells) in lockstep with the written output RFile, comparing each
// cell byte-for-byte. Returns the first divergence (or nil) and the number
// of cells compared.
func selfConsistent(spec compaction.Spec, output []byte) (*CellMismatch, int64, error) {
	r, err := openRFile(output)
	if err != nil {
		return nil, 0, fmt.Errorf("offlinecompact: open output rfile: %w", err)
	}
	defer r.Close()

	var idx int64
	var mism *CellMismatch
	streamErr := compaction.StreamCells(spec, func(ek *wire.Key, ev []byte) error {
		ak, av, err := r.Next()
		if errors.Is(err, io.EOF) {
			mism = &CellMismatch{Index: idx, Reason: "output RFile ended before the re-derived stream"}
			return errStopVerify
		}
		if err != nil {
			return fmt.Errorf("read output cell %d: %w", idx, err)
		}
		if !ek.Equal(ak) || !bytes.Equal(ev, av) {
			mism = &CellMismatch{
				Index:  idx,
				Reason: fmt.Sprintf("output %s != re-derived %s", renderCell(ak, av), renderCell(ek, ev)),
			}
			return errStopVerify
		}
		idx++
		return nil
	})
	if streamErr != nil && !errors.Is(streamErr, errStopVerify) {
		return nil, idx, fmt.Errorf("re-derive compaction stream: %w", streamErr)
	}
	if mism != nil {
		return mism, idx, nil
	}

	// The re-derived stream is exhausted; the output must be too.
	if _, _, err := r.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			return &CellMismatch{Index: idx, Reason: "output RFile has more cells than the re-derived stream"}, idx, nil
		}
		return nil, idx, fmt.Errorf("check output tail: %w", err)
	}
	return nil, idx, nil
}

func openRFile(image []byte) (*rfile.Reader, error) {
	bc, err := bcfile.NewReader(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		return nil, fmt.Errorf("bcfile open: %w", err)
	}
	r, err := rfile.Open(bc, block.Default())
	if err != nil {
		return nil, fmt.Errorf("rfile open: %w", err)
	}
	return r, nil
}

func renderCell(k *wire.Key, v []byte) string {
	return fmt.Sprintf("(%s:%s:%s@%d del=%t val=%s)",
		truncBytes(k.Row),
		truncBytes(k.ColumnFamily),
		truncBytes(k.ColumnQualifier),
		k.Timestamp, k.Deleted, truncBytes(v))
}

// maxRenderBytes bounds how many bytes of any single cell field are
// rendered into a mismatch reason, so a divergence on a multi-megabyte
// value cannot explode log size or dump full (possibly sensitive) cell
// payloads. The comparison itself is still byte-exact; only the message
// is truncated.
const maxRenderBytes = 64

func truncBytes(b []byte) string {
	if len(b) <= maxRenderBytes {
		return metadata.PrintableBytes(b)
	}
	return fmt.Sprintf("%s…(+%d bytes)", metadata.PrintableBytes(b[:maxRenderBytes]), len(b)-maxRenderBytes)
}
