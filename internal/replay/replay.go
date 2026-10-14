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

// Package replay is a minimal append-only action-replay ledger over a shoal
// table. It records an ordered stream of actions (tool calls / reasoning
// steps) under a correlation id and replays them in order — the local analogue
// of the veculo platform's replay ledger (replay_ledger.py), pared down to the
// action stream itself.
//
// Steps are immutable cells, so replay is a read: it never mutates state.
// Combined with the engine's timestamped cells, Replay can also reconstruct
// the ledger as it stood at an instant via ReplayOptions.AsOf — a step appended
// after the ceiling is simply not yet visible. DryRun is carried through to the
// report for callers that gate side effects on it; the ledger itself performs
// none.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/internal/embedpb"
)

// RowPrefix namespaces ledger rows. A step lives at
// "replay:<correlationID>:<zero-padded-seq>".
const RowPrefix = "replay:"

const (
	stepCF       = "ledger:"
	cqStep       = "step"
	seqWidth     = 20 // zero-pad seq so lexical row order == numeric order
	defaultLimit = 1 << 20
)

// Store is the subset of the embedstore API the Ledger needs. *embedstore.
// EngineStore satisfies it.
type Store interface {
	Write(ctx context.Context, table string, muts []*embedpb.Mutation) error
	Scan(ctx context.Context, table string, req *embedpb.ScanRequest) ([]*embedpb.Cell, error)
}

// Step is one recorded action in a correlation's stream.
type Step struct {
	Seq         int64             `json:"seq"`
	TimestampMs int64             `json:"ts_ms,omitempty"`
	Action      string            `json:"action"`
	Hash        string            `json:"hash,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

// ReplayOptions tunes a Replay.
type ReplayOptions struct {
	// AsOf, when > 0, reconstructs the ledger as of that cell-timestamp
	// ceiling: steps written after it are excluded.
	AsOf int64
	// DryRun is echoed in the report; the ledger never causes side effects.
	DryRun bool
}

// ReplayReport is the ordered result of a Replay.
type ReplayReport struct {
	CorrelationID string
	Steps         []Step
	DryRun        bool
}

// Ledger appends and replays steps in a single table.
type Ledger struct {
	store Store
	table string
}

// NewLedger binds a Ledger to a store and table.
func NewLedger(store Store, table string) *Ledger {
	return &Ledger{store: store, table: table}
}

func prefixFor(correlationID string) string {
	return fmt.Sprintf("%s%s:", RowPrefix, correlationID)
}

func rowFor(correlationID string, seq int64) []byte {
	return []byte(fmt.Sprintf("%s%0*d", prefixFor(correlationID), seqWidth, seq))
}

// Append records step under correlationID. When step.Seq <= 0 the Ledger
// assigns the next sequence (one past the current maximum). The stored step is
// returned with its final Seq.
func (l *Ledger) Append(ctx context.Context, correlationID string, step Step) (Step, error) {
	if correlationID == "" {
		return Step{}, errors.New("replay: correlationID is required")
	}
	if step.Action == "" {
		return Step{}, errors.New("replay: step.Action is required")
	}
	if step.Seq <= 0 {
		next, err := l.nextSeq(ctx, correlationID)
		if err != nil {
			return Step{}, err
		}
		step.Seq = next
	}
	payload, err := json.Marshal(step)
	if err != nil {
		return Step{}, err
	}
	entry := &embedpb.Entry{ColumnFamily: []byte(stepCF), ColumnQualifier: []byte(cqStep), Value: payload}
	if step.TimestampMs > 0 {
		entry.Timestamp = step.TimestampMs
	}
	muts := []*embedpb.Mutation{{Row: rowFor(correlationID, step.Seq), Entries: []*embedpb.Entry{entry}}}
	if err := l.store.Write(ctx, l.table, muts); err != nil {
		return Step{}, err
	}
	return step, nil
}

// nextSeq returns one past the highest existing seq for correlationID (1 when
// empty).
func (l *Ledger) nextSeq(ctx context.Context, correlationID string) (int64, error) {
	steps, err := l.read(ctx, correlationID, 0)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, s := range steps {
		if s.Seq > max {
			max = s.Seq
		}
	}
	return max + 1, nil
}

// Replay returns correlationID's steps in sequence order, honoring opts.
func (l *Ledger) Replay(ctx context.Context, correlationID string, opts ReplayOptions) (ReplayReport, error) {
	if correlationID == "" {
		return ReplayReport{}, errors.New("replay: correlationID is required")
	}
	steps, err := l.read(ctx, correlationID, opts.AsOf)
	if err != nil {
		return ReplayReport{}, err
	}
	return ReplayReport{CorrelationID: correlationID, Steps: steps, DryRun: opts.DryRun}, nil
}

// read loads and decodes every step for correlationID, ordered by seq. asOf,
// when > 0, bounds the scan to cells at-or-before that timestamp.
func (l *Ledger) read(ctx context.Context, correlationID string, asOf int64) ([]Step, error) {
	cells, err := l.store.Scan(ctx, l.table, &embedpb.ScanRequest{RowPrefix: prefixFor(correlationID), Limit: defaultLimit, AsOf: asOf})
	if err != nil {
		return nil, err
	}
	var steps []Step
	for _, c := range cells {
		if string(c.ColumnFamily) != stepCF || string(c.ColumnQualifier) != cqStep {
			continue
		}
		var s Step
		if err := json.Unmarshal(c.Value, &s); err != nil {
			return nil, fmt.Errorf("replay: decode step at %s: %w", c.Row, err)
		}
		steps = append(steps, s)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Seq < steps[j].Seq })
	return steps, nil
}
