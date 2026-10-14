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

// Package adjacency implements shoal.adjacency — an optional auxiliary
// meta-block that stores a graph's out-edges in CSR (compressed sparse
// row) form so a "neighbors of node N" query is a binary search plus a
// contiguous slice read, bypassing the merge + versioning + RelativeKey
// decode machinery a normal Scan pays per cell.
//
// It is an ACCELERATION STRUCTURE, not a source of truth: the edge cells
// are still written into the RFile's data blocks exactly as before, so
// Scan, compaction, versioning, and Apache Accumulo parity are all
// unaffected. Stock readers ignore unknown meta-blocks; shoal's reader
// consults this one when present and falls back to Scan when it isn't.
//
// One RFile's index answers neighbors within THAT file only. The engine
// unions per-file indexes plus the memtable's edges and resolves
// newest-timestamp-wins with delete suppression — identical semantics to
// a Scan over (row, edgeCF), but reading compact per-node slices.
//
// Wire format (V1; snappy-compressed as a BCFile meta-block):
//
//	int32 magic        = 'SADJ'
//	int32 version      = 1
//	vint  edgeCFLen, [edgeCF bytes]      — the column family treated as edges
//	int32 nodeCount
//	for each node (sorted by row ascending, for binary search):
//	    vint rowLen, [row bytes]
//	    int32 targetStart   — index into the flattened targets array
//	    int32 targetCount   — number of out-edges for this node
//	int32 targetTotal
//	for each target (CSR values, grouped by node):
//	    vint cqLen,  [cq bytes]          — edge cell column qualifier
//	    vint valLen, [value bytes]
//	    int64 timestamp
//	    bool  deleted
//	    vint visLen, [visibility bytes]
package adjacency

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// MetaBlockName is the BCFile MetaIndex entry under which the adjacency
// index is stored. Mirrors RFile's "RFile.index"/"RFile.blockmeta"
// naming convention.
const MetaBlockName = "shoal.adjacency"

// Magic identifies an adjacency block. ASCII 'SADJ'.
const Magic int32 = 0x53414447

// V1 is the only supported version.
const V1 int32 = 1

// ErrCorrupt indicates malformed bytes in the meta block.
var ErrCorrupt = errors.New("adjacency: corrupt")

// ErrUnsupportedVersion is returned when the version field is unknown.
var ErrUnsupportedVersion = errors.New("adjacency: unsupported version")

// Edge is one out-edge of a node — a full-fidelity mirror of the edge
// cell so Neighbors reproduces exactly what a Scan over (row, edgeCF)
// would return. Row and ColumnFamily are implied (the queried row and
// the index's EdgeCF respectively).
type Edge struct {
	CQ        []byte // column qualifier = the neighbor/target id
	Value     []byte
	Timestamp int64
	Deleted   bool
	Vis       []byte
}

// node is one source row's entry in the CSR directory.
type node struct {
	row   []byte
	start int32
	count int32
}

// Index is a parsed adjacency block: a row-sorted node directory plus a
// flattened targets array. Immutable after Parse/Build; safe to share
// across concurrent readers.
type Index struct {
	EdgeCF  []byte
	nodes   []node
	targets []Edge
}

// EdgeColumnFamily returns the column family this index treats as edges.
func (ix *Index) EdgeColumnFamily() []byte { return ix.EdgeCF }

// Neighbors returns the out-edges of row, or nil if row has none in this
// index. The returned slice aliases the index's storage and MUST NOT be
// mutated. Deleted edges are included (with Deleted=true) so the engine
// can suppress them during a cross-source merge.
func (ix *Index) Neighbors(row []byte) []Edge {
	i := sort.Search(len(ix.nodes), func(i int) bool {
		return bytes.Compare(ix.nodes[i].row, row) >= 0
	})
	if i >= len(ix.nodes) || !bytes.Equal(ix.nodes[i].row, row) {
		return nil
	}
	n := ix.nodes[i]
	return ix.targets[n.start : n.start+n.count]
}

// NodeCount reports how many distinct source rows carry edges.
func (ix *Index) NodeCount() int { return len(ix.nodes) }

// Builder accumulates edge cells during RFile writing and produces an
// Index at Close. Cells may be added in any order; Build sorts.
type Builder struct {
	edgeCF  []byte
	entries []entry
}

type entry struct {
	row  []byte
	edge Edge
}

// NewBuilder returns a Builder that treats cells whose column family
// equals edgeCF as graph edges.
func NewBuilder(edgeCF []byte) *Builder {
	cf := make([]byte, len(edgeCF))
	copy(cf, edgeCF)
	return &Builder{edgeCF: cf}
}

// EdgeCF returns the configured edge column family.
func (b *Builder) EdgeCF() []byte { return b.edgeCF }

// Add records one edge cell. It copies every slice — callers may reuse
// their buffers after the call.
func (b *Builder) Add(row, cq, value, vis []byte, ts int64, deleted bool) {
	b.entries = append(b.entries, entry{
		row: cloneBytes(row),
		edge: Edge{
			CQ:        cloneBytes(cq),
			Value:     cloneBytes(value),
			Timestamp: ts,
			Deleted:   deleted,
			Vis:       cloneBytes(vis),
		},
	})
}

// Len reports the number of edges accumulated so far.
func (b *Builder) Len() int { return len(b.entries) }

// Build assembles the accumulated edges into a row-sorted CSR Index.
// Within a row, edges keep column-qualifier order. Returns nil if no
// edges were added (caller suppresses the meta-block, matching the
// "no index" default).
func (b *Builder) Build() *Index {
	if len(b.entries) == 0 {
		return nil
	}
	// Stable sort by (row, cq) so the CSR groups rows contiguously and
	// preserves deterministic edge order within each node.
	sort.SliceStable(b.entries, func(i, j int) bool {
		if c := bytes.Compare(b.entries[i].row, b.entries[j].row); c != 0 {
			return c < 0
		}
		return bytes.Compare(b.entries[i].edge.CQ, b.entries[j].edge.CQ) < 0
	})

	targets := make([]Edge, len(b.entries))
	var nodes []node
	for i, e := range b.entries {
		targets[i] = e.edge
		if len(nodes) == 0 || !bytes.Equal(nodes[len(nodes)-1].row, e.row) {
			nodes = append(nodes, node{row: e.row, start: int32(i), count: 1})
		} else {
			nodes[len(nodes)-1].count++
		}
	}
	return &Index{EdgeCF: b.edgeCF, nodes: nodes, targets: targets}
}

// Write serializes ix to w. Inverse of Parse.
func Write(w io.Writer, ix *Index) error {
	bw := asByteWriter(w)
	if err := wire.WriteInt32(w, Magic); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, V1); err != nil {
		return err
	}
	if err := writeBytes(w, bw, ix.EdgeCF); err != nil {
		return fmt.Errorf("edgeCF: %w", err)
	}
	if err := wire.WriteInt32(w, int32(len(ix.nodes))); err != nil {
		return err
	}
	for i, n := range ix.nodes {
		if err := writeBytes(w, bw, n.row); err != nil {
			return fmt.Errorf("node[%d] row: %w", i, err)
		}
		if err := wire.WriteInt32(w, n.start); err != nil {
			return fmt.Errorf("node[%d] start: %w", i, err)
		}
		if err := wire.WriteInt32(w, n.count); err != nil {
			return fmt.Errorf("node[%d] count: %w", i, err)
		}
	}
	if err := wire.WriteInt32(w, int32(len(ix.targets))); err != nil {
		return err
	}
	for i := range ix.targets {
		if err := writeEdge(w, bw, &ix.targets[i]); err != nil {
			return fmt.Errorf("target[%d]: %w", i, err)
		}
	}
	return nil
}

// Parse reads a serialized adjacency block, starting at the magic int32.
func Parse(r wire.ByteAndReader) (*Index, error) {
	magic, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("magic: %w", err)
	}
	if magic != Magic {
		return nil, fmt.Errorf("%w: bad magic 0x%08x (want 0x%08x)", ErrCorrupt, magic, Magic)
	}
	version, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}
	if version != V1 {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
	}
	edgeCF, err := readBytes(r)
	if err != nil {
		return nil, fmt.Errorf("edgeCF: %w", err)
	}
	nodeCount, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("nodeCount: %w", err)
	}
	if nodeCount < 0 {
		return nil, fmt.Errorf("%w: negative nodeCount %d", ErrCorrupt, nodeCount)
	}
	nodes := make([]node, nodeCount)
	for i := int32(0); i < nodeCount; i++ {
		row, err := readBytes(r)
		if err != nil {
			return nil, fmt.Errorf("node[%d] row: %w", i, err)
		}
		start, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("node[%d] start: %w", i, err)
		}
		count, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("node[%d] count: %w", i, err)
		}
		if start < 0 || count < 0 {
			return nil, fmt.Errorf("%w: node[%d] bad start/count %d/%d", ErrCorrupt, i, start, count)
		}
		nodes[i] = node{row: row, start: start, count: count}
	}
	targetTotal, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("targetTotal: %w", err)
	}
	if targetTotal < 0 {
		return nil, fmt.Errorf("%w: negative targetTotal %d", ErrCorrupt, targetTotal)
	}
	targets := make([]Edge, targetTotal)
	for i := int32(0); i < targetTotal; i++ {
		e, err := readEdge(r)
		if err != nil {
			return nil, fmt.Errorf("target[%d]: %w", i, err)
		}
		targets[i] = e
	}
	// Validate node ranges fall within the targets array.
	for i, n := range nodes {
		if int64(n.start)+int64(n.count) > int64(len(targets)) {
			return nil, fmt.Errorf("%w: node[%d] range [%d,%d) exceeds targets %d",
				ErrCorrupt, i, n.start, n.start+n.count, len(targets))
		}
	}
	return &Index{EdgeCF: edgeCF, nodes: nodes, targets: targets}, nil
}

func writeEdge(w io.Writer, bw io.ByteWriter, e *Edge) error {
	if err := writeBytes(w, bw, e.CQ); err != nil {
		return fmt.Errorf("cq: %w", err)
	}
	if err := writeBytes(w, bw, e.Value); err != nil {
		return fmt.Errorf("value: %w", err)
	}
	if err := wire.WriteInt64(w, e.Timestamp); err != nil {
		return fmt.Errorf("timestamp: %w", err)
	}
	if err := wire.WriteBool(w, e.Deleted); err != nil {
		return fmt.Errorf("deleted: %w", err)
	}
	if err := writeBytes(w, bw, e.Vis); err != nil {
		return fmt.Errorf("vis: %w", err)
	}
	return nil
}

func readEdge(r wire.ByteAndReader) (Edge, error) {
	cq, err := readBytes(r)
	if err != nil {
		return Edge{}, fmt.Errorf("cq: %w", err)
	}
	value, err := readBytes(r)
	if err != nil {
		return Edge{}, fmt.Errorf("value: %w", err)
	}
	ts, err := wire.ReadInt64(r)
	if err != nil {
		return Edge{}, fmt.Errorf("timestamp: %w", err)
	}
	deleted, err := wire.ReadBool(r)
	if err != nil {
		return Edge{}, fmt.Errorf("deleted: %w", err)
	}
	vis, err := readBytes(r)
	if err != nil {
		return Edge{}, fmt.Errorf("vis: %w", err)
	}
	return Edge{CQ: cq, Value: value, Timestamp: ts, Deleted: deleted, Vis: vis}, nil
}

// writeBytes emits a vint length prefix followed by the bytes.
func writeBytes(w io.Writer, bw io.ByteWriter, b []byte) error {
	if _, err := wire.WriteVInt(bw, int32(len(b))); err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	_, err := w.Write(b)
	return err
}

// readBytes reads a vint-length-prefixed byte slice.
func readBytes(r wire.ByteAndReader) ([]byte, error) {
	n, _, err := wire.ReadVInt(r)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("%w: negative length %d", ErrCorrupt, n)
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func asByteWriter(w io.Writer) io.ByteWriter {
	if bw, ok := w.(io.ByteWriter); ok {
		return bw
	}
	return byteWriterShim{w: w}
}

type byteWriterShim struct{ w io.Writer }

func (s byteWriterShim) WriteByte(b byte) error {
	_, err := s.w.Write([]byte{b})
	return err
}
