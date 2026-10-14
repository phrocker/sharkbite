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
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

// CommitMode selects how a finished Plan is turned into durable metadata.
type CommitMode int

const (
	// ModePlan (the default) emits a machine-readable commit plan and
	// makes no metadata writes. An operator or an Ample-based applier
	// consumes it and performs the conditional mutation inside Accumulo,
	// keeping metadata-write authority where it belongs. This is the
	// conservative path shipped first (design §4.3).
	ModePlan CommitMode = iota
	// ModeDirect writes accumulo.metadata itself via a supplied
	// MetadataCommitter, as a conditional mutation guarded on the current
	// file set. Opt-in only; requires a committer (shoal ships no
	// metadata writer of its own — it is a read fleet).
	ModeDirect
)

func (m CommitMode) String() string {
	switch m {
	case ModePlan:
		return "plan"
	case ModeDirect:
		return "direct"
	default:
		return fmt.Sprintf("CommitMode(%d)", int(m))
	}
}

// ParseCommitMode maps the --commit-mode flag value to a CommitMode.
func ParseCommitMode(s string) (CommitMode, error) {
	switch s {
	case "plan", "":
		return ModePlan, nil
	case "direct":
		return ModeDirect, nil
	default:
		return 0, fmt.Errorf("unknown commit mode %q (want \"plan\" or \"direct\")", s)
	}
}

// FileAdd is the single output RFile a tablet gains. The applier turns
// this into a StoredTabletFile "file:" entry (whole-file range) plus its
// DataFileValue ("Size,NumEntries").
type FileAdd struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	NumEntries int64  `json:"numEntries"`
}

// TabletCommit is the metadata delta for one tablet: drop every input
// "file:" entry (matched byte-exactly by DeleteQualifiers) and add the
// single compacted output. Deletes carry the raw qualifier bytes because
// Accumulo enforces byte-exact qualifier match on mutation.
type TabletCommit struct {
	// EndRow/PrevRow identify the extent. nil EndRow = default (last)
	// tablet; nil PrevRow = first tablet. Encoded as base64 (or null) in
	// JSON via Go's []byte handling.
	EndRow  []byte `json:"endRow"`
	PrevRow []byte `json:"prevRow"`
	// DeleteQualifiers are the exact "file:" column qualifiers to remove
	// (StoredTabletFile JSON bytes as they appear in metadata).
	DeleteQualifiers [][]byte `json:"delete"`
	// Add is the output file to reference.
	Add FileAdd `json:"add"`
}

// CommitPlan is the serialized hand-off for ModePlan. It never deletes
// input RFiles from storage; it only removes their metadata refs and lets
// Accumulo's GC reclaim the bytes (design §4.3, "GC-safe").
type CommitPlan struct {
	TableID string         `json:"tableId"`
	Mode    string         `json:"mode"`
	Tablets []TabletCommit `json:"tablets"`
}

// MetadataCommitter applies one tablet's delta to accumulo.metadata as a
// conditional mutation. It is the ModeDirect seam; shoal ships no default
// implementation (see ErrDirectCommitUnavailable). An implementation must
// guard the mutation on the current file set so a concurrent change (which
// the OFFLINE fence should already preclude) still cannot corrupt metadata.
type MetadataCommitter interface {
	Commit(ctx context.Context, tc TabletCommit) error
}

// ErrDirectCommitUnavailable is returned when ModeDirect is requested but
// no MetadataCommitter was supplied.
var ErrDirectCommitUnavailable = errors.New("direct commit mode requires a MetadataCommitter (none wired; use commit mode \"plan\")")

// BuildCommitPlan projects a finished Plan into a CommitPlan. It fails
// closed if any consumed input lacks a RawQualifier, since an un-deletable
// input would make the emitted plan unapplyable (and silently lossy).
func BuildCommitPlan(p *Plan) (*CommitPlan, error) {
	if p == nil {
		return nil, errors.New("nil plan")
	}
	cp := &CommitPlan{
		TableID: p.TableID,
		Mode:    ModePlan.String(),
		Tablets: make([]TabletCommit, 0, len(p.Results)),
	}
	for _, res := range p.Results {
		dels := make([][]byte, 0, len(res.Inputs))
		for _, in := range res.Inputs {
			if len(in.RawQualifier) == 0 {
				return nil, fmt.Errorf("tablet %s/%s input %q has no RawQualifier; cannot build a byte-exact delete",
					res.Tablet.TableID, metadata.PrintableBytes(res.Tablet.EndRow), in.Path)
			}
			dels = append(dels, append([]byte(nil), in.RawQualifier...))
		}
		cp.Tablets = append(cp.Tablets, TabletCommit{
			EndRow:           cloneBytes(res.Tablet.EndRow),
			PrevRow:          cloneBytes(res.Tablet.PrevRow),
			DeleteQualifiers: dels,
			Add: FileAdd{
				Path:       res.OutputPath,
				Size:       res.OutputSize,
				NumEntries: res.EntriesWritten,
			},
		})
	}
	return cp, nil
}

// MarshalCommitPlan serializes a CommitPlan as indented JSON for the
// operator/applier hand-off.
func MarshalCommitPlan(cp *CommitPlan) ([]byte, error) {
	return json.MarshalIndent(cp, "", "  ")
}

// Commit turns a finished Plan into durable metadata under the OFFLINE
// fence. It re-verifies the fence immediately before any write (the token
// was minted before the — potentially long — compaction), then either
// returns the commit plan (ModePlan) or applies each tablet delta via the
// committer (ModeDirect).
//
// dryRun is the single commit gate: when true, Commit still verifies the
// fence and builds the plan but performs no writes, regardless of mode.
// In ModeDirect a mid-run committer failure leaves already-committed
// tablets in place (each tablet delta is independent and idempotent); the
// error names the failed tablet so the operator can resume.
func Commit(ctx context.Context, p *Plan, fence StateFence, minted FenceToken, mode CommitMode, dryRun bool, committer MetadataCommitter) (*CommitPlan, error) {
	if fence == nil {
		return nil, errors.New("nil fence: refusing to commit without an OFFLINE guard")
	}
	if err := fence.Verify(ctx, minted); err != nil {
		return nil, fmt.Errorf("pre-commit fence check: %w", err)
	}

	cp, err := BuildCommitPlan(p)
	if err != nil {
		return nil, err
	}
	cp.Mode = mode.String()

	// Validate the mode BEFORE the dry-run gate so an invalid CommitMode
	// fails closed in every path — otherwise a dry run would happily emit
	// a plan stamped with a bogus mode and no error.
	switch mode {
	case ModePlan, ModeDirect:
		// recognized
	default:
		return cp, fmt.Errorf("unknown commit mode %v", mode)
	}

	if dryRun {
		return cp, nil
	}

	switch mode {
	case ModePlan:
		// The plan itself is the artifact; oc-cli writes it out.
		return cp, nil
	case ModeDirect:
		if committer == nil {
			return cp, ErrDirectCommitUnavailable
		}
		for _, tc := range cp.Tablets {
			if err := committer.Commit(ctx, tc); err != nil {
				return cp, fmt.Errorf("commit tablet %s/%s: %w",
					cp.TableID, metadata.PrintableBytes(tc.EndRow), err)
			}
		}
		return cp, nil
	default:
		return cp, fmt.Errorf("unknown commit mode %v", mode)
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
