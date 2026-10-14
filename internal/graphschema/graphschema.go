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

// Package graphschema defines the Veculo agentic-memory multi-graph cell-schema
// conventions for modeling an agentic memory graph in shoal's generic sorted
// key/value cells.
//
// The package is intentionally only conventions and byte helpers. Shoal's
// engine and iterator runtime remain schema-agnostic: consumers pass the row
// prefixes and column-family bytes from this package into generic pushdown
// requests such as VectorSearch, EdgeExpand, and TermFilter.
//
// The schema keeps each graph view in its own column family. That column-family
// namespacing lets shoal's sorted-key layout and locality-group skipping avoid
// unrelated cells when a scan needs only content, embeddings, attributes, or one
// specific edge view.
package graphschema

import (
	"bytes"
	"encoding/binary"
	"math"
)

// Row-key prefixes for the Veculo agentic-memory node and posting rows.
const (
	// EventRowPrefix prefixes event node rows. Event ids should be lexically and
	// temporally sortable, such as ULIDs, so a row-range scan over evt: also acts
	// as a temporal backbone scan.
	EventRowPrefix = "evt:"

	// EntityRowPrefix prefixes entity node rows. The suffix is the canonical
	// entity name used by the consumer.
	EntityRowPrefix = "ent:"

	// TermRowPrefix prefixes inverted-index posting rows. TermFilter requests
	// can use TermRow(term) directly as posting rows.
	TermRowPrefix = "idx:"
)

// Reserved column-family names for the Veculo agentic-memory schema.
const (
	// ContentCFName marks an event text-content cell.
	ContentCFName = "content:"

	// VectorCFName marks an embedding cell whose value is a sequence of
	// big-endian float32 values, matching proto VectorSearch.query and
	// VectorSearch.embedding_cf conventions.
	VectorCFName = "vec:"

	// AttributeCFName marks metadata attribute cells. The column qualifier is
	// the attribute key and the value is consumer-defined.
	AttributeCFName = "attr:"

	// TemporalEdgeCFName marks temporal graph edges. The column qualifier is the
	// neighbor or target id; the value may be empty or a big-endian float32
	// weight.
	TemporalEdgeCFName = "edge.temp:"

	// CausalEdgeCFName marks causal graph edges. The column qualifier is the
	// neighbor or target id; the value may be empty or a big-endian float32
	// weight.
	CausalEdgeCFName = "edge.causal:"

	// SemanticEdgeCFName marks semantic graph edges. The column qualifier is the
	// neighbor or target id; the value may be empty or a big-endian float32
	// weight.
	SemanticEdgeCFName = "edge.sem:"

	// EntityEdgeCFName marks event/entity relationship edges. The column
	// qualifier is the neighbor or target id; the value may be empty or a
	// big-endian float32 weight.
	EntityEdgeCFName = "edge.ent:"
)

// EdgeType identifies one of the four orthogonal graph views.
type EdgeType uint8

const (
	// Temporal edges form the time-adjacent event backbone.
	Temporal EdgeType = iota
	// Causal edges connect events by inferred or explicit cause/effect.
	Causal
	// Semantic edges connect events or entities by embedding/meaning affinity.
	Semantic
	// Entity edges connect events to mentioned entities or entities to related
	// events/entities.
	Entity
)

// EventRow returns the row key for an event node id.
func EventRow(id string) []byte {
	return append([]byte(EventRowPrefix), id...)
}

// EntityRow returns the row key for an entity node name.
func EntityRow(name string) []byte {
	return append([]byte(EntityRowPrefix), name...)
}

// TermRow returns the row key for an inverted-index posting term.
func TermRow(term string) []byte {
	return append([]byte(TermRowPrefix), term...)
}

// ContentCF returns the column family for event text content cells.
func ContentCF() []byte { return []byte(ContentCFName) }

// VectorCF returns the column family for embedding cells. The bytes can be
// supplied directly as VectorSearch.embedding_cf.
func VectorCF() []byte { return []byte(VectorCFName) }

// AttributeCF returns the column family for metadata attribute cells.
func AttributeCF() []byte { return []byte(AttributeCFName) }

// TemporalEdgeCF returns the column family for temporal edge cells.
func TemporalEdgeCF() []byte { return []byte(TemporalEdgeCFName) }

// CausalEdgeCF returns the column family for causal edge cells.
func CausalEdgeCF() []byte { return []byte(CausalEdgeCFName) }

// SemanticEdgeCF returns the column family for semantic edge cells.
func SemanticEdgeCF() []byte { return []byte(SemanticEdgeCFName) }

// EntityEdgeCF returns the column family for entity edge cells.
func EntityEdgeCF() []byte { return []byte(EntityEdgeCFName) }

// EdgeCF returns the column family for the requested graph view. The returned
// bytes can be supplied directly as EdgeExpand.edge_cf. Unknown EdgeType values
// return nil.
func EdgeCF(t EdgeType) []byte {
	switch t {
	case Temporal:
		return TemporalEdgeCF()
	case Causal:
		return CausalEdgeCF()
	case Semantic:
		return SemanticEdgeCF()
	case Entity:
		return EntityEdgeCF()
	default:
		return nil
	}
}

// EdgeTypeFromCF maps a graph edge column family back to its EdgeType.
func EdgeTypeFromCF(cf []byte) (EdgeType, bool) {
	switch {
	case bytes.Equal(cf, []byte(TemporalEdgeCFName)):
		return Temporal, true
	case bytes.Equal(cf, []byte(CausalEdgeCFName)):
		return Causal, true
	case bytes.Equal(cf, []byte(SemanticEdgeCFName)):
		return Semantic, true
	case bytes.Equal(cf, []byte(EntityEdgeCFName)):
		return Entity, true
	default:
		return 0, false
	}
}

// PackWeight packs an edge weight as a single big-endian float32. This matches
// the float encoding used by shoal's vector search and IVF-PQ iterator
// conventions.
func PackWeight(weight float32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, math.Float32bits(weight))
	return out
}

// UnpackWeight unpacks a single big-endian float32 edge weight. It returns
// false unless the input is exactly four bytes.
func UnpackWeight(raw []byte) (float32, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	return math.Float32frombits(binary.BigEndian.Uint32(raw)), true
}

// ParseEventID extracts the event id suffix from an evt: row key.
func ParseEventID(row []byte) (id string, ok bool) {
	return parseSuffix(row, EventRowPrefix)
}

// ParseEntityName extracts the entity name suffix from an ent: row key.
func ParseEntityName(row []byte) (name string, ok bool) {
	return parseSuffix(row, EntityRowPrefix)
}

// ParseTerm extracts the term suffix from an idx: posting row key.
func ParseTerm(row []byte) (term string, ok bool) {
	return parseSuffix(row, TermRowPrefix)
}

func parseSuffix(row []byte, prefix string) (string, bool) {
	if !bytes.HasPrefix(row, []byte(prefix)) {
		return "", false
	}
	return string(row[len(prefix):]), true
}
