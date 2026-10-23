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

// Package graphtable reads a graph laid out in any engine table through the
// table's recorded shoal.schema.v1.TableSchema descriptor. The reader never
// opens the table itself: every read is a Scan request, the same request the
// shoal-embed gRPC service takes, so the rows are served by whichever process
// hosts them. The reader holds only the walk's frontier; a neighborhood is
// expanded hop by hop, and each hop's row reads are routed to the store that
// owns each row and run concurrently. A table split by row range across
// several store processes is one Sharded store to the reader.
package graphtable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Store serves scan requests for the rows it hosts. Every implementation
// applies the engine's deleting and versioning stack, so a caller sees the
// newest live cell of each key, exactly as a shoal-embed client does.
type Store interface {
	Scan(ctx context.Context, req *embedpb.ScanRequest) ([]*embedpb.Cell, error)
}

// EngineStore serves scans from an engine open in this process.
type EngineStore struct{ store *embedstore.EngineStore }

// NewEngineStore wraps an open engine.
func NewEngineStore(eng *engine.Engine) *EngineStore {
	return &EngineStore{store: embedstore.New(eng)}
}

// Scan runs the request against the engine.
func (s *EngineStore) Scan(ctx context.Context, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("graphtable: engine store is not open")
	}
	return s.store.Scan(ctx, req.GetTable(), req)
}

// DefaultMaxRemoteCells bounds one remote scan's result unless the caller
// sets another bound: a store that ignores a request's limit cannot make
// the reader hold an unbounded result.
const DefaultMaxRemoteCells = 1 << 16

// RemoteStore serves scans from a shoal-embed process over its gRPC service.
type RemoteStore struct {
	client embedpb.ShoalEmbedClient
	// MaxCells bounds one scan's result; NewRemoteStore sets
	// DefaultMaxRemoteCells, and a negative value removes the bound.
	MaxCells int
}

// NewRemoteStore wraps a connected client.
func NewRemoteStore(client embedpb.ShoalEmbedClient) *RemoteStore {
	return &RemoteStore{client: client, MaxCells: DefaultMaxRemoteCells}
}

// Scan streams the request's cells from the remote store. ScanV2 is used
// because it is the service's complete scan surface (pushdowns included).
func (s *RemoteStore) Scan(ctx context.Context, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("graphtable: remote store has no client")
	}
	stream, err := s.client.ScanV2(ctx, req)
	if err != nil {
		return nil, err
	}
	var cells []*embedpb.Cell
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return cells, nil
		}
		if err != nil {
			return nil, err
		}
		cells = append(cells, resp.GetCells()...)
		if s.MaxCells > 0 && len(cells) > s.MaxCells {
			return nil, fmt.Errorf("graphtable: scan of %q returned more than %d cells", req.GetTable(), s.MaxCells)
		}
	}
}

// Shard is one store and the half-open row range [Start, End) it hosts. An
// empty Start is the beginning of the keyspace and an empty End its end.
type Shard struct {
	Name  string
	Start []byte
	End   []byte
	Store Store
}

func (s Shard) owns(row []byte) bool {
	if len(s.Start) > 0 && bytes.Compare(row, s.Start) < 0 {
		return false
	}
	if len(s.End) > 0 && bytes.Compare(row, s.End) >= 0 {
		return false
	}
	return true
}

// String renders the shard as name=start..end.
func (s Shard) String() string {
	return fmt.Sprintf("%s=%s..%s", s.Name, s.Start, s.End)
}

// Sharded routes each scan to the shards whose ranges meet the request's
// rows and merges their cells in key order. The shards must tile the whole
// keyspace with no gap and no overlap, so every row has exactly one owner.
type Sharded struct{ shards []Shard }

// NewSharded validates the tiling: shards sorted by start, the first
// starting at the beginning of the keyspace, each one ending exactly where
// the next begins, the last ending at the end of the keyspace.
func NewSharded(shards []Shard) (*Sharded, error) {
	if len(shards) == 0 {
		return nil, errors.New("graphtable: at least one shard is required")
	}
	sorted := append([]Shard(nil), shards...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].Start, sorted[j].Start) < 0
	})
	names := map[string]bool{}
	for i, s := range sorted {
		if s.Store == nil {
			return nil, fmt.Errorf("graphtable: shard %q has no store", s.Name)
		}
		if s.Name == "" || names[s.Name] {
			return nil, fmt.Errorf("graphtable: shard name %q is empty or repeated", s.Name)
		}
		names[s.Name] = true
		if len(s.Start) > 0 && len(s.End) > 0 && bytes.Compare(s.Start, s.End) >= 0 {
			return nil, fmt.Errorf("graphtable: shard %s has an empty range", s)
		}
		if i == 0 && len(s.Start) > 0 {
			return nil, fmt.Errorf("graphtable: no shard owns rows before %q", s.Start)
		}
		if i == len(sorted)-1 && len(s.End) > 0 {
			return nil, fmt.Errorf("graphtable: no shard owns rows from %q", s.End)
		}
		if i > 0 {
			prev := sorted[i-1]
			if len(prev.End) == 0 || !bytes.Equal(prev.End, s.Start) {
				return nil, fmt.Errorf("graphtable: shards %s and %s do not meet", prev, s)
			}
		}
	}
	return &Sharded{shards: sorted}, nil
}

// Shards returns the tiling in row order.
func (s *Sharded) Shards() []Shard { return append([]Shard(nil), s.shards...) }

// Scan sends the request to every shard whose range meets the request's
// rows and merges the results by key. A request carrying a pushdown is
// refused: a pushdown resolves rows inside one store, and the rows it
// resolves to may be owned by another.
func (s *Sharded) Scan(ctx context.Context, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	if s == nil || len(s.shards) == 0 {
		return nil, errors.New("graphtable: sharded store has no shards")
	}
	if req.GetEdgeExpand() != nil || req.GetTermFilter() != nil || req.GetVectorSearch() != nil || req.GetScoreFilter() != nil {
		return nil, errors.New("graphtable: a pushdown cannot be routed across shards")
	}
	r := embedstore.ScanRange(req)
	var targets []Shard
	for _, shard := range s.shards {
		if rangeMeetsShard(r, shard) {
			targets = append(targets, shard)
		}
	}
	if len(targets) == 1 {
		return targets[0].Store.Scan(ctx, req)
	}
	results := make([][]*embedpb.Cell, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, shard := range targets {
		wg.Add(1)
		go func(i int, shard Shard) {
			defer wg.Done()
			results[i], errs[i] = shard.Store.Scan(ctx, req)
		}(i, shard)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("graphtable: shard %s: %w", targets[i].Name, err)
		}
	}
	var merged []*embedpb.Cell
	for _, cells := range results {
		merged = append(merged, cells...)
	}
	sort.SliceStable(merged, func(i, j int) bool { return compareCells(merged[i], merged[j]) < 0 })
	if limit := int(req.GetLimit()); limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// rangeMeetsShard reports whether a key range can hold a row the shard owns.
func rangeMeetsShard(r iterrt.Range, shard Shard) bool {
	if len(shard.End) > 0 && !r.InfiniteStart && r.Start != nil && bytes.Compare(r.Start.Row, shard.End) >= 0 {
		return false
	}
	if len(shard.Start) > 0 && !r.InfiniteEnd && r.End != nil {
		c := bytes.Compare(r.End.Row, shard.Start)
		if c < 0 || (c == 0 && !r.EndInclusive && len(r.End.ColumnFamily) == 0 && len(r.End.ColumnQualifier) == 0) {
			return false
		}
	}
	return true
}

func compareCells(a, b *embedpb.Cell) int {
	ka := wire.Key{Row: a.Row, ColumnFamily: a.ColumnFamily, ColumnQualifier: a.ColumnQualifier, ColumnVisibility: a.ColumnVisibility, Timestamp: a.Timestamp}
	kb := wire.Key{Row: b.Row, ColumnFamily: b.ColumnFamily, ColumnQualifier: b.ColumnQualifier, ColumnVisibility: b.ColumnVisibility, Timestamp: b.Timestamp}
	return ka.Compare(&kb)
}

// ShardSpec is one parsed -graph-stores entry: an address and the row range
// it owns.
type ShardSpec struct {
	Address string
	Start   []byte
	End     []byte
}

// ParseShardSpecs reads a comma-separated list of address[=start..end]
// entries. An entry without a range owns the whole keyspace and must be the
// only entry. Ranges are half-open and either bound may be empty.
func ParseShardSpecs(spec string) ([]ShardSpec, error) {
	var out []ShardSpec
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		address, rng, hasRange := strings.Cut(item, "=")
		address = strings.TrimSpace(address)
		if address == "" {
			return nil, fmt.Errorf("graph store entry %q has no address", item)
		}
		entry := ShardSpec{Address: address}
		if hasRange {
			start, end, ok := strings.Cut(rng, "..")
			if !ok {
				return nil, fmt.Errorf("graph store entry %q: range must be start..end", item)
			}
			if start != "" {
				entry.Start = []byte(start)
			}
			if end != "" {
				entry.End = []byte(end)
			}
			if len(entry.Start) > 0 && len(entry.End) > 0 && bytes.Compare(entry.Start, entry.End) >= 0 {
				return nil, fmt.Errorf("graph store entry %q: range is empty", item)
			}
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, errors.New("graph stores: at least one address is required")
	}
	if len(out) > 1 {
		for _, entry := range out {
			if len(entry.Start) == 0 && len(entry.End) == 0 {
				return nil, fmt.Errorf("graph store %s owns the whole keyspace; it must be the only entry", entry.Address)
			}
		}
	}
	return out, nil
}
