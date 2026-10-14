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

// Package provenance implements a lightweight, deterministic provenance stamp
// that can be attached to any shoal cell row.
//
// It is the local, prudent counterpart to the veculo platform's Phase 2/3
// rationale + PCA write stamps (api/app/folds.py, api/app/authority_chain.py).
// Where the platform mints a cryptographic, narrowable authority chain, the
// local stamp records only the audit-grade essentials: who wrote the cell, which
// sources it derives from, a short rationale, and a PCA-style hop/op chain. The
// stamp is rendered as ordinary cells under a reserved provenance column family,
// so it survives RFile export/import and incremental sync like any other data,
// and it can later be promoted to the full platform authority chain without a
// schema change.
//
// The package depends only on the generated embedpb types; it has no knowledge
// of folds, agentmem, or the engine. Folds are its first consumer.
package provenance

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/embedpb"
)

// CFName is the reserved column family that holds provenance property cells.
// It is namespaced like the graphschema column families so locality-group
// skipping can ignore provenance when a scan does not need it.
const CFName = "prov:"

// Column qualifiers for the individual provenance properties. They mirror the
// platform's _actor / _pca_* / _rationale_* property names so a local stamp can
// be lifted into the platform vocabulary one-to-one.
const (
	cqActor     = "actor"
	cqSources   = "sources"
	cqRationale = "rationale"
	cqHop       = "pca_hop"
	cqOps       = "pca_ops"
	cqCreatedAt = "created_at"
)

// sourceSep joins multiple source ids in the single sources cell. Newline is
// safe because vertex/event ids in shoal never contain it.
const sourceSep = "\n"

// CF returns the provenance column family bytes. Callers can pass it directly to
// scan/filter requests.
func CF() []byte { return []byte(CFName) }

// Stamp is a deterministic, audit-grade provenance record for a single write.
// The zero value is a valid empty stamp that renders no cells.
type Stamp struct {
	// Actor is the agent or system identity that produced the write.
	Actor string
	// Sources are the ids (event rows, fold ids, vertex ids) this write derives
	// from. Order is preserved; callers that want a canonical set should sort
	// before constructing the stamp.
	Sources []string
	// Rationale is a short, human-readable reason for the write.
	Rationale string
	// Hop is the PCA-style hop count from the originating authority.
	Hop int
	// Ops is the PCA-style operation chain. It is stored as a sorted, canonical
	// comma list to match the platform's _pca_ops encoding.
	Ops []string
	// CreatedAt is the stamp time. When zero, Entries substitutes the timestamp
	// passed to it so the rendered cells still carry a creation time.
	CreatedAt time.Time
}

// IsZero reports whether the stamp carries no provenance information and would
// render no cells.
func (s Stamp) IsZero() bool {
	return s.Actor == "" && len(s.Sources) == 0 && s.Rationale == "" &&
		s.Hop == 0 && len(s.Ops) == 0 && s.CreatedAt.IsZero()
}

// Entries renders the stamp into provenance cells under CFName. The ts argument
// is the cell timestamp (unix millis) and is also used as the created_at value
// when Stamp.CreatedAt is zero. Only set fields produce cells, and the cells are
// emitted in a fixed order so identical stamps render byte-identically.
func (s Stamp) Entries(ts int64) []*embedpb.Entry {
	if s.IsZero() {
		return nil
	}
	cf := CF()
	var out []*embedpb.Entry
	add := func(cq, val string) {
		out = append(out, &embedpb.Entry{
			ColumnFamily:    cf,
			ColumnQualifier: []byte(cq),
			Timestamp:       ts,
			Value:           []byte(val),
		})
	}
	if s.Actor != "" {
		add(cqActor, s.Actor)
	}
	if len(s.Sources) > 0 {
		add(cqSources, strings.Join(s.Sources, sourceSep))
	}
	if s.Rationale != "" {
		add(cqRationale, s.Rationale)
	}
	if s.Hop != 0 {
		add(cqHop, strconv.Itoa(s.Hop))
	}
	if len(s.Ops) > 0 {
		ops := append([]string(nil), s.Ops...)
		sort.Strings(ops)
		add(cqOps, strings.Join(ops, ","))
	}
	created := s.CreatedAt
	if created.IsZero() {
		created = time.UnixMilli(ts).UTC()
	}
	add(cqCreatedAt, created.UTC().Format(time.RFC3339))
	return out
}

// Parse reconstructs a Stamp from a slice of cells, ignoring any cell whose
// column family is not CFName. It is the inverse of Entries for the round-trip
// that fold recall relies on.
func Parse(cells []*embedpb.Cell) Stamp {
	var s Stamp
	for _, c := range cells {
		if string(c.ColumnFamily) != CFName {
			continue
		}
		switch string(c.ColumnQualifier) {
		case cqActor:
			s.Actor = string(c.Value)
		case cqSources:
			if len(c.Value) > 0 {
				s.Sources = strings.Split(string(c.Value), sourceSep)
			}
		case cqRationale:
			s.Rationale = string(c.Value)
		case cqHop:
			s.Hop, _ = strconv.Atoi(string(c.Value))
		case cqOps:
			if len(c.Value) > 0 {
				s.Ops = strings.Split(string(c.Value), ",")
			}
		case cqCreatedAt:
			if t, err := time.Parse(time.RFC3339, string(c.Value)); err == nil {
				s.CreatedAt = t.UTC()
			}
		}
	}
	return s
}

// MarshalJSON renders the stamp as a compact, canonical JSON object for callers
// that surface provenance over an API. Empty fields are omitted.
func (s Stamp) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if s.Actor != "" {
		m["actor"] = s.Actor
	}
	if len(s.Sources) > 0 {
		m["sources"] = s.Sources
	}
	if s.Rationale != "" {
		m["rationale"] = s.Rationale
	}
	if s.Hop != 0 {
		m["pca_hop"] = s.Hop
	}
	if len(s.Ops) > 0 {
		ops := append([]string(nil), s.Ops...)
		sort.Strings(ops)
		m["pca_ops"] = ops
	}
	if !s.CreatedAt.IsZero() {
		m["created_at"] = s.CreatedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(m)
}
