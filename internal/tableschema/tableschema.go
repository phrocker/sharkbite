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

// Package tableschema records and reads well-known table schema descriptors
// (proto/schema.proto, shoal.schema.v1.TableSchema) in a store.
//
// A descriptor says how one table's cells lay out a graph so a reader does
// not have to be told out of band. The engine stores descriptors and never
// interprets them; this package is a consumer library in the same sense as
// graphschema.
//
// Layout of the reserved table:
//
//	table  _shoal_schema
//	row    <described table name>
//	cf     schema
//	cq     <version, zero-padded to 20 digits so versions sort>
//	value  the TableSchema in proto3 JSON with proto field names, compact
//
// Writes are append-only and versions are monotonic: a put must carry a
// version greater than the newest recorded one, or the same version with a
// byte-identical document (which is a no-op that repairs the manifest
// pointer). The described table's manifest carries a pointer to the newest
// version so the engine's status listing can report described tables; the
// pointer is an index and the reserved table holds the document.
package tableschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/schemapb"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

const (
	// Table is the reserved table holding descriptors. It is created on the
	// first put, never by init.
	Table = "_shoal_schema"
	// SchemaCF is the column family of descriptor cells.
	SchemaCF = "schema"

	// qualifierWidth zero-pads versions so lexical order is numeric order.
	// int64 has at most 19 digits.
	qualifierWidth = 20

	// MaxDescriptorBytes bounds one recorded document.
	MaxDescriptorBytes = 1 << 20
)

// ErrNotDescribed reports that no descriptor is recorded for a table.
var ErrNotDescribed = errors.New("tableschema: table is not described")

// ErrNotNewer reports a put whose version does not advance the record.
var ErrNotNewer = errors.New("tableschema: version is not newer than the recorded one")

// ErrConflict reports a put that re-uses a recorded version with a different
// document.
var ErrConflict = errors.New("tableschema: recorded version carries a different document")

// Qualifier returns the column qualifier under which a version is recorded.
func Qualifier(version int64) []byte {
	return []byte(fmt.Sprintf("%0*d", qualifierWidth, version))
}

// ParseQualifier reads a version back from its qualifier.
func ParseQualifier(cq []byte) (int64, error) {
	if len(cq) != qualifierWidth {
		return 0, fmt.Errorf("tableschema: qualifier %q is not %d digits", cq, qualifierWidth)
	}
	version, err := strconv.ParseInt(string(cq), 10, 64)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("tableschema: qualifier %q is not a positive version", cq)
	}
	return version, nil
}

// Encode validates a descriptor and returns its wire form: proto3 JSON with
// proto field names, insignificant whitespace removed. The bytes are
// deterministic for a given descriptor, which is what makes a re-put of the
// same version checkable.
func Encode(schema *schemapb.TableSchema) ([]byte, error) {
	if err := Validate(schema); err != nil {
		return nil, err
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("tableschema: encode: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("tableschema: encode: %w", err)
	}
	if compact.Len() > MaxDescriptorBytes {
		return nil, fmt.Errorf("tableschema: descriptor is %d bytes; the limit is %d", compact.Len(), MaxDescriptorBytes)
	}
	return compact.Bytes(), nil
}

// Decode parses a wire-form descriptor and validates it. Unknown fields are
// refused: a document a newer writer produced is not silently narrowed.
func Decode(data []byte) (*schemapb.TableSchema, error) {
	if len(data) > MaxDescriptorBytes {
		return nil, fmt.Errorf("tableschema: descriptor is %d bytes; the limit is %d", len(data), MaxDescriptorBytes)
	}
	var schema schemapb.TableSchema
	if err := (protojson.UnmarshalOptions{}).Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("tableschema: decode: %w", err)
	}
	if err := Validate(&schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

// Validate checks a descriptor's internal consistency: names and namespaces
// are present and distinct, the pushdown parameters it carries are ones the
// engine accepts, and no two entries claim the same cells.
func Validate(schema *schemapb.TableSchema) error {
	if schema == nil {
		return errors.New("tableschema: nil descriptor")
	}
	if err := validateTableName(schema.GetTable()); err != nil {
		return err
	}
	if schema.GetSchemaId() == "" {
		return errors.New("tableschema: schema_id is required")
	}
	if schema.GetVersion() < 1 {
		return fmt.Errorf("tableschema: version %d is not positive", schema.GetVersion())
	}
	if len(schema.GetNodes()) == 0 {
		return errors.New("tableschema: at least one node namespace is required")
	}
	names := map[string]string{}
	if strings.TrimSpace(schema.GetSchemaId()) == "" || hasControl(schema.GetSchemaId()) {
		return errors.New("tableschema: schema_id must be text")
	}
	claim := func(kind, name string) error {
		if strings.TrimSpace(name) == "" || hasControl(name) {
			return fmt.Errorf("tableschema: %s name is required", kind)
		}
		if prior, dup := names[name]; dup {
			return fmt.Errorf("tableschema: %s %q reuses the name of a %s", kind, name, prior)
		}
		names[name] = kind
		return nil
	}
	var rowPrefixes []namedPrefix
	families := map[string]string{}
	attributeFamilies := map[string][]string{}
	claimFamily := func(owner, cf string) error {
		if prior, dup := families[cf]; dup {
			return fmt.Errorf("tableschema: %s and %s both claim column family %q", prior, owner, cf)
		}
		families[cf] = owner
		return nil
	}
	for _, node := range schema.GetNodes() {
		if err := claim("node", node.GetName()); err != nil {
			return err
		}
		if node.GetRowPrefix() == "" {
			return fmt.Errorf("tableschema: node %q has no row_prefix", node.GetName())
		}
		rowPrefixes = append(rowPrefixes, namedPrefix{"node " + node.GetName(), node.GetRowPrefix()})
		if node.GetKindSep() == "" {
			if node.GetKindIndex() != 0 {
				return fmt.Errorf("tableschema: node %q sets kind_index without kind_sep", node.GetName())
			}
			if len(node.GetKinds()) != 0 {
				return fmt.Errorf("tableschema: node %q lists kinds without kind_sep", node.GetName())
			}
		} else if node.GetKindIndex() < 0 {
			return fmt.Errorf("tableschema: node %q kind_index %d is negative", node.GetName(), node.GetKindIndex())
		}
		if err := distinct("node "+node.GetName()+" kinds", node.GetKinds()); err != nil {
			return err
		}
		// Attribute families are claimed per namespace: two namespaces may
		// share one (the graphschema layout puts attr: on every node row),
		// but no attribute family may also be an edge, vector, or posting
		// family.
		if err := distinct("node "+node.GetName()+" attribute_cfs", node.GetAttributeCfs()); err != nil {
			return err
		}
		for _, cf := range node.GetAttributeCfs() {
			attributeFamilies[cf] = append(attributeFamilies[cf], "node "+node.GetName())
		}
		if _, ok := schemapb.QualifierRole_name[int32(node.GetAttributeQualifier())]; !ok {
			return fmt.Errorf("tableschema: node %q has unknown attribute_qualifier %d", node.GetName(), node.GetAttributeQualifier())
		}
		if _, ok := schemapb.ValueEncoding_name[int32(node.GetAttributeEncoding())]; !ok {
			return fmt.Errorf("tableschema: node %q has unknown attribute_encoding %d", node.GetName(), node.GetAttributeEncoding())
		}
	}
	for _, edge := range schema.GetEdges() {
		if err := claim("edge", edge.GetName()); err != nil {
			return err
		}
		owner := "edge " + edge.GetName()
		if edge.GetEdgeCf() == "" {
			return fmt.Errorf("tableschema: %s has no edge_cf", owner)
		}
		if err := claimFamily(owner, edge.GetEdgeCf()); err != nil {
			return err
		}
		switch edge.GetEdgeField() {
		case "", "qualifier":
		case "value":
			if edge.GetValueEncoding() != schemapb.ValueEncoding_VALUE_ENCODING_UNSPECIFIED {
				return fmt.Errorf("tableschema: %s reads the edge token from the value, so the value cannot also carry an encoded payload", owner)
			}
		default:
			return fmt.Errorf("tableschema: %s edge_field %q is not \"qualifier\" or \"value\"", owner, edge.GetEdgeField())
		}
		if !edge.GetIdIndexSet() && edge.GetIdIndex() != 0 {
			return fmt.Errorf("tableschema: %s sets id_index %d without id_index_set; a reader would ignore it", owner, edge.GetIdIndex())
		}
		if edge.GetFieldSep() == "" {
			switch {
			case edge.GetIdIndexSet() || edge.GetIdIndex() != 0:
				return fmt.Errorf("tableschema: %s sets id_index without field_sep", owner)
			case edge.GetRelIndex() != 0:
				return fmt.Errorf("tableschema: %s sets rel_index without field_sep", owner)
			case len(edge.GetRelationships()) != 0:
				return fmt.Errorf("tableschema: %s lists relationships without field_sep", owner)
			}
		} else {
			if edge.GetIdIndexSet() && edge.GetIdIndex() < 0 {
				return fmt.Errorf("tableschema: %s id_index %d is invalid", owner, edge.GetIdIndex())
			}
			if edge.GetRelIndex() < 0 {
				return fmt.Errorf("tableschema: %s rel_index %d is negative", owner, edge.GetRelIndex())
			}
			if edge.GetIdIndexSet() && edge.GetIdIndex() == edge.GetRelIndex() {
				return fmt.Errorf("tableschema: %s id_index and rel_index are both %d", owner, edge.GetRelIndex())
			}
		}
		if err := distinct(owner+" relationships", edge.GetRelationships()); err != nil {
			return err
		}
		if _, ok := schemapb.ValueEncoding_name[int32(edge.GetValueEncoding())]; !ok {
			return fmt.Errorf("tableschema: %s has unknown value_encoding %d", owner, edge.GetValueEncoding())
		}
	}
	for _, vector := range schema.GetVectors() {
		if err := claim("vector", vector.GetName()); err != nil {
			return err
		}
		owner := "vector " + vector.GetName()
		if vector.GetEmbeddingCf() == "" {
			return fmt.Errorf("tableschema: %s has no embedding_cf", owner)
		}
		if err := claimFamily(owner, vector.GetEmbeddingCf()); err != nil {
			return err
		}
		if vector.GetEmbeddingSpace() == "" {
			return fmt.Errorf("tableschema: %s has no embedding_space", owner)
		}
		switch vector.GetMetric() {
		case "", "cosine", "dot", "l2":
		default:
			return fmt.Errorf("tableschema: %s metric %q is not cosine, dot, or l2", owner, vector.GetMetric())
		}
	}
	for _, posting := range schema.GetPostings() {
		if err := claim("posting", posting.GetName()); err != nil {
			return err
		}
		owner := "posting " + posting.GetName()
		if posting.GetTermRowPrefix() == "" {
			return fmt.Errorf("tableschema: %s has no term_row_prefix", owner)
		}
		rowPrefixes = append(rowPrefixes, namedPrefix{owner, posting.GetTermRowPrefix()})
		switch posting.GetIdSource() {
		case "", "qualifier", "value":
		default:
			return fmt.Errorf("tableschema: %s id_source %q is not \"qualifier\" or \"value\"", owner, posting.GetIdSource())
		}
	}
	for cf, owners := range attributeFamilies {
		if prior, dup := families[cf]; dup {
			return fmt.Errorf("tableschema: %s and %s both claim column family %q", prior, owners[0], cf)
		}
	}
	for i, a := range rowPrefixes {
		for _, b := range rowPrefixes[i+1:] {
			if strings.HasPrefix(a.prefix, b.prefix) || strings.HasPrefix(b.prefix, a.prefix) {
				return fmt.Errorf("tableschema: row prefixes of %s (%q) and %s (%q) overlap", a.owner, a.prefix, b.owner, b.prefix)
			}
		}
	}
	return nil
}

type namedPrefix struct{ owner, prefix string }

func distinct(what string, values []string) error {
	seen := map[string]struct{}{}
	for _, v := range values {
		if v == "" {
			return fmt.Errorf("tableschema: %s contain an empty entry", what)
		}
		if _, dup := seen[v]; dup {
			return fmt.Errorf("tableschema: %s repeat %q", what, v)
		}
		seen[v] = struct{}{}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateTableName(name string) error {
	if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || hasControl(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("tableschema: invalid table name %q", name)
	}
	if name == Table {
		return fmt.Errorf("tableschema: %s is the reserved descriptor table and cannot be described", Table)
	}
	return nil
}

// Entry is one recorded descriptor version.
type Entry struct {
	Table   string
	Version int64
	Schema  *schemapb.TableSchema
	// Bytes is the recorded wire form.
	Bytes []byte
}

// Store records and reads descriptors in one engine. The version cell is
// written with a conditional mutation that requires it absent, so two
// Stores, or two processes, racing to record one version cannot both land a
// cell: the loser re-reads and is refused unless its document is the one
// already recorded.
type Store struct {
	eng *engine.Engine
	mu  sync.Mutex
}

// New returns a Store over the engine.
func New(eng *engine.Engine) *Store { return &Store{eng: eng} }

// Put records a descriptor as the newest version for its table and updates
// the described table's manifest pointer. The described table must exist.
// The reserved table is created on the first put.
func (s *Store) Put(schema *schemapb.TableSchema) (Entry, error) {
	data, err := Encode(schema)
	if err != nil {
		return Entry{}, err
	}
	table := schema.GetTable()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasTable(table) {
		return Entry{}, fmt.Errorf("tableschema: table %q does not exist in this store", table)
	}
	if err := s.ensureReserved(); err != nil {
		return Entry{}, err
	}
	latest, err := s.latest(table)
	if err != nil && !errors.Is(err, ErrNotDescribed) {
		return Entry{}, err
	}
	version := int64(schema.GetVersion())
	switch {
	case err == nil && version < latest.Version:
		return Entry{}, fmt.Errorf("%w: table %q has version %d, put carries %d", ErrNotNewer, table, latest.Version, version)
	case err == nil && version == latest.Version:
		if !bytes.Equal(latest.Bytes, data) {
			return Entry{}, fmt.Errorf("%w: table %q version %d", ErrConflict, table, version)
		}
		// Identical re-put: only the pointer can be behind.
	default:
		mutation, err := cclient.NewMutation([]byte(table))
		if err != nil {
			return Entry{}, fmt.Errorf("tableschema: %w", err)
		}
		mutation.Put([]byte(SchemaCF), Qualifier(version), nil, cclient.MutationLatestTimestamp, data)
		accepted, err := s.eng.ConditionalWrite(Table, []engine.ConditionalMutation{{
			Mutation:   mutation,
			Conditions: []tablet.Condition{{ColumnFamily: []byte(SchemaCF), ColumnQualifier: Qualifier(version), Kind: engine.ConditionAbsent}},
		}})
		if err != nil {
			return Entry{}, fmt.Errorf("tableschema: record %q version %d: %w", table, version, err)
		}
		if len(accepted) != 1 || !accepted[0] {
			// Someone else recorded this version between our read and our
			// write. It is ours only if it carries our bytes.
			recorded, err := s.versionEntry(table, version)
			if err != nil {
				return Entry{}, err
			}
			if !bytes.Equal(recorded.Bytes, data) {
				return Entry{}, fmt.Errorf("%w: table %q version %d was recorded concurrently", ErrConflict, table, version)
			}
		}
	}
	pointer := engine.SchemaPointer{ID: schema.GetSchemaId(), Version: version}
	if err := s.eng.SetTableSchema(table, pointer); err != nil {
		return Entry{}, fmt.Errorf("tableschema: point %q at version %d: %w", table, version, err)
	}
	return Entry{Table: table, Version: version, Schema: proto.Clone(schema).(*schemapb.TableSchema), Bytes: data}, nil
}

// Get returns the newest recorded descriptor for a table.
func (s *Store) Get(table string) (Entry, error) {
	if err := validateTableName(table); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasTable(Table) {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotDescribed, table)
	}
	return s.latest(table)
}

// GetVersion returns one recorded version of a table's descriptor.
func (s *Store) GetVersion(table string, version int64) (Entry, error) {
	if err := validateTableName(table); err != nil {
		return Entry{}, err
	}
	if version < 1 {
		return Entry{}, fmt.Errorf("tableschema: version %d is not positive", version)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasTable(Table) {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotDescribed, table)
	}
	entries, err := s.versions(table)
	if err != nil {
		return Entry{}, err
	}
	for _, entry := range entries {
		if entry.Version == version {
			return entry, nil
		}
	}
	if len(entries) == 0 {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotDescribed, table)
	}
	return Entry{}, fmt.Errorf("tableschema: table %q has no version %d", table, version)
}

// Versions returns every recorded version of a table's descriptor, oldest
// first.
func (s *Store) Versions(table string) ([]Entry, error) {
	if err := validateTableName(table); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasTable(Table) {
		return nil, fmt.Errorf("%w: %q", ErrNotDescribed, table)
	}
	entries, err := s.versions(table)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNotDescribed, table)
	}
	return entries, nil
}

// Described lists the newest descriptor of every described table, by table
// name. It reads the reserved table, not the manifests.
func (s *Store) Described() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasTable(Table) {
		return nil, nil
	}
	entries, err := s.scan(iterrt.InfiniteRange())
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, entry := range entries {
		if n := len(out); n > 0 && out[n-1].Table == entry.Table {
			out[n-1] = entry
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func (s *Store) hasTable(name string) bool {
	for _, table := range s.eng.TableNames() {
		if table == name {
			return true
		}
	}
	return false
}

func (s *Store) ensureReserved() error {
	if s.hasTable(Table) {
		return nil
	}
	if err := s.eng.CreateTable(Table, engine.TableOptions{}); err != nil {
		return fmt.Errorf("tableschema: create %s: %w", Table, err)
	}
	return nil
}

func (s *Store) versionEntry(table string, version int64) (Entry, error) {
	entries, err := s.versions(table)
	if err != nil {
		return Entry{}, err
	}
	for _, entry := range entries {
		if entry.Version == version {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("tableschema: table %q version %d vanished after a conditional write was refused", table, version)
}

func (s *Store) latest(table string) (Entry, error) {
	entries, err := s.versions(table)
	if err != nil {
		return Entry{}, err
	}
	if len(entries) == 0 {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotDescribed, table)
	}
	return entries[len(entries)-1], nil
}

func (s *Store) versions(table string) ([]Entry, error) {
	row := []byte(table)
	end := make([]byte, len(row)+1)
	copy(end, row)
	return s.scan(iterrt.Range{
		Start:          &iterrt.Key{Row: row},
		StartInclusive: true,
		End:            &iterrt.Key{Row: end},
		EndInclusive:   false,
	})
}

// scan reads descriptor cells in key order: by table, then by version. The
// engine delivers every timestamp of a key and every delete marker; only the
// newest cell of each qualifier counts, and a newest cell that is a delete
// marker means the version is gone. A cell that is not a descriptor of the
// row's table at the qualifier's version is an error, not skipped: the
// reserved table is the record, and a stray cell in it is a fault to surface.
func (s *Store) scan(r iterrt.Range) ([]Entry, error) {
	sc, err := s.eng.Scan(Table, r, engine.ScanOptions{
		ColumnFamilies:          [][]byte{[]byte(SchemaCF)},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return nil, fmt.Errorf("tableschema: scan %s: %w", Table, err)
	}
	defer sc.Close()
	var out []Entry
	var lastRow, lastQualifier []byte
	seen := false
	for ; sc.Next(); func() {
		if err := sc.Advance(); err != nil {
			out, err = nil, fmt.Errorf("tableschema: scan %s: %w", Table, err)
		}
	}() {
		key := sc.Key()
		if !bytes.Equal(key.ColumnFamily, []byte(SchemaCF)) {
			continue
		}
		if seen && bytes.Equal(key.Row, lastRow) && bytes.Equal(key.ColumnQualifier, lastQualifier) {
			// An older timestamp of a qualifier already decided.
			continue
		}
		seen, lastRow, lastQualifier = true, append([]byte(nil), key.Row...), append([]byte(nil), key.ColumnQualifier...)
		if key.Deleted {
			continue
		}
		version, err := ParseQualifier(key.ColumnQualifier)
		if err != nil {
			return nil, fmt.Errorf("tableschema: %s row %q: %w", Table, key.Row, err)
		}
		data := append([]byte(nil), sc.Value()...)
		schema, err := Decode(data)
		if err != nil {
			return nil, fmt.Errorf("tableschema: %s row %q version %d: %w", Table, key.Row, version, err)
		}
		if schema.GetTable() != string(key.Row) {
			return nil, fmt.Errorf("tableschema: %s row %q version %d describes table %q", Table, key.Row, version, schema.GetTable())
		}
		if int64(schema.GetVersion()) != version {
			return nil, fmt.Errorf("tableschema: %s row %q version %d carries version %d", Table, key.Row, version, schema.GetVersion())
		}
		out = append(out, Entry{Table: string(key.Row), Version: version, Schema: schema, Bytes: data})
	}
	return out, nil
}

// Indent returns the wire form re-indented for display.
func Indent(data []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
