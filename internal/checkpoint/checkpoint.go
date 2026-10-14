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

// Package checkpoint provides lightweight named checkpoints over a shoal
// table. A checkpoint is NOT a copy of state — it is just a label bound to a
// timestamp watermark. Because the engine keeps immutable, timestamped cells,
// "restoring" a checkpoint is reading the table as-of its watermark via an
// as-of scan (ScanRequest.AsOf), which the Manager wires for you.
//
// This mirrors the cluster's temporal-query replay model (read as-of) rather
// than a full snapshot/restore: cheap to record, and the historical state is
// reconstructed on demand from data that already exists.
//
// The watermark unit is the caller's responsibility: it must match the unit of
// the cell timestamps being filtered (e.g. the same Unix-millis clock agentmem
// stamps events with). The Manager stores and applies the int64 verbatim.
package checkpoint

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/embedpb"
)

// RowPrefix namespaces checkpoint metadata rows within a table. It is distinct
// from the graphschema data prefixes (evt:/ent:/idx:/term:) so checkpoints and
// data coexist in one table without colliding.
const RowPrefix = "ckpt:"

const (
	metaCF       = "meta:"
	cqTimestamp  = "ts"
	cqCreatedAt  = "created_at"
	defaultLimit = 1 << 20
)

// Store is the subset of the embedstore API the Manager needs. *embedstore.
// EngineStore satisfies it.
type Store interface {
	Write(ctx context.Context, table string, muts []*embedpb.Mutation) error
	Scan(ctx context.Context, table string, req *embedpb.ScanRequest) ([]*embedpb.Cell, error)
}

// Checkpoint is a named timestamp watermark.
type Checkpoint struct {
	Label     string
	Timestamp int64 // as-of watermark; the table is read at-or-before this
	CreatedAt int64 // wall-clock the checkpoint was recorded (caller's unit)
}

// Manager records and resolves checkpoints in a single table.
type Manager struct {
	store Store
	table string
}

// NewManager binds a Manager to a store and table.
func NewManager(store Store, table string) *Manager {
	return &Manager{store: store, table: table}
}

func rowFor(label string) []byte { return []byte(RowPrefix + label) }

// Save records (or overwrites) the checkpoint label at the given timestamp
// watermark and createdAt. Both are stored verbatim. A non-positive ts is
// rejected: an as-of scan with ceiling <= 0 means "no ceiling", which would
// silently behave as a live read.
func (m *Manager) Save(ctx context.Context, label string, ts, createdAt int64) (Checkpoint, error) {
	if label == "" {
		return Checkpoint{}, errors.New("checkpoint: label is required")
	}
	if ts <= 0 {
		return Checkpoint{}, errors.New("checkpoint: timestamp watermark must be positive")
	}
	muts := []*embedpb.Mutation{{Row: rowFor(label), Entries: []*embedpb.Entry{
		{ColumnFamily: []byte(metaCF), ColumnQualifier: []byte(cqTimestamp), Value: []byte(strconv.FormatInt(ts, 10))},
		{ColumnFamily: []byte(metaCF), ColumnQualifier: []byte(cqCreatedAt), Value: []byte(strconv.FormatInt(createdAt, 10))},
	}}}
	if err := m.store.Write(ctx, m.table, muts); err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Label: label, Timestamp: ts, CreatedAt: createdAt}, nil
}

// Get returns the checkpoint for label. The bool is false when absent.
func (m *Manager) Get(ctx context.Context, label string) (Checkpoint, bool, error) {
	row := string(rowFor(label))
	cells, err := m.store.Scan(ctx, m.table, &embedpb.ScanRequest{StartRow: rowFor(label), StartInclusive: true, EndRow: rowFor(label), EndInclusive: true, Limit: defaultLimit})
	if err != nil {
		return Checkpoint{}, false, err
	}
	cp := Checkpoint{Label: label}
	found := false
	for _, c := range cells {
		if string(c.Row) != row || string(c.ColumnFamily) != metaCF {
			continue
		}
		found = true
		switch string(c.ColumnQualifier) {
		case cqTimestamp:
			cp.Timestamp, _ = strconv.ParseInt(string(c.Value), 10, 64)
		case cqCreatedAt:
			cp.CreatedAt, _ = strconv.ParseInt(string(c.Value), 10, 64)
		}
	}
	if !found {
		return Checkpoint{}, false, nil
	}
	return cp, true, nil
}

// List returns all checkpoints in the table, ordered by label.
func (m *Manager) List(ctx context.Context) ([]Checkpoint, error) {
	cells, err := m.store.Scan(ctx, m.table, &embedpb.ScanRequest{RowPrefix: RowPrefix, Limit: defaultLimit})
	if err != nil {
		return nil, err
	}
	byLabel := map[string]*Checkpoint{}
	for _, c := range cells {
		if string(c.ColumnFamily) != metaCF || len(c.Row) <= len(RowPrefix) {
			continue
		}
		label := string(c.Row[len(RowPrefix):])
		cp := byLabel[label]
		if cp == nil {
			cp = &Checkpoint{Label: label}
			byLabel[label] = cp
		}
		switch string(c.ColumnQualifier) {
		case cqTimestamp:
			cp.Timestamp, _ = strconv.ParseInt(string(c.Value), 10, 64)
		case cqCreatedAt:
			cp.CreatedAt, _ = strconv.ParseInt(string(c.Value), 10, 64)
		}
	}
	out := make([]Checkpoint, 0, len(byLabel))
	for _, cp := range byLabel {
		out = append(out, *cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// Delete removes the checkpoint label by writing tombstones at each of its
// metadata cells. Absent labels are a no-op.
func (m *Manager) Delete(ctx context.Context, label string) error {
	row := string(rowFor(label))
	cells, err := m.store.Scan(ctx, m.table, &embedpb.ScanRequest{StartRow: rowFor(label), StartInclusive: true, EndRow: rowFor(label), EndInclusive: true, Limit: defaultLimit})
	if err != nil {
		return err
	}
	var dels []*embedpb.Entry
	for _, c := range cells {
		if string(c.Row) != row {
			continue
		}
		dels = append(dels, &embedpb.Entry{ColumnFamily: c.ColumnFamily, ColumnQualifier: c.ColumnQualifier, ColumnVisibility: c.ColumnVisibility, Timestamp: c.Timestamp, Delete: true})
	}
	if len(dels) == 0 {
		return nil
	}
	return m.store.Write(ctx, m.table, []*embedpb.Mutation{{Row: rowFor(label), Entries: dels}})
}

// ScanAsOf resolves label to its watermark and runs req as an as-of scan at
// that instant. The caller supplies the data bounds (RowPrefix / Start / End)
// and ScanAsOf overrides req.AsOf with the checkpoint's timestamp. It errors if
// the label is unknown.
func (m *Manager) ScanAsOf(ctx context.Context, label string, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	cp, ok, err := m.Get(ctx, label)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("checkpoint: unknown label " + label)
	}
	if req == nil {
		req = &embedpb.ScanRequest{}
	}
	req.AsOf = cp.Timestamp
	return m.store.Scan(ctx, m.table, req)
}
