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

package tableschema_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/schemapb"
	"github.com/phrocker/shoal-oss/internal/tableschema"
)

// foreman is the layout from the issue: rows ent:<kind>|<id> and
// evt:<kind>|<id>, cf = record type, cq = field, text values, and edges as
// cf=edge, cq=<rel>\0<target row>.
func foreman(version int32) *schemapb.TableSchema {
	return &schemapb.TableSchema{
		Table:       "graph",
		SchemaId:    "foreman.graph",
		Version:     version,
		Description: "foreman entities and events",
		Nodes: []*schemapb.NodeSchema{
			{
				Name: "entity", RowPrefix: "ent:", KindSep: "|", KindIndex: 0,
				Kinds:              []string{"person", "host"},
				AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD,
				AttributeEncoding:  schemapb.ValueEncoding_VALUE_ENCODING_TEXT,
			},
			{
				Name: "event", RowPrefix: "evt:", KindSep: "|", KindIndex: 0,
				AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD,
				AttributeEncoding:  schemapb.ValueEncoding_VALUE_ENCODING_TEXT,
			},
		},
		Edges: []*schemapb.EdgeSchema{{
			Name: "edge", EdgeCf: "edge", EdgeField: "qualifier", FieldSep: "\x00",
			IdIndex: 1, IdIndexSet: true, RelIndex: 0,
			Relationships: []string{"owns", "observed"},
		}},
	}
}

func open(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestEncodeIsCompactDeterministicAndRoundTrips(t *testing.T) {
	first, err := tableschema.Encode(foreman(1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := tableschema.Encode(foreman(1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("encode is not deterministic:\n%s\n%s", first, second)
	}
	if bytes.ContainsAny(first, "\n\t") || bytes.Contains(first, []byte(": ")) {
		t.Fatalf("encode is not compact: %s", first)
	}
	var generic map[string]any
	if err := json.Unmarshal(first, &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"table", "schema_id", "version", "nodes", "edges"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("wire form lacks proto field name %q: %s", key, first)
		}
	}
	if !bytes.Contains(first, []byte(`"field_sep":"\u0000"`)) {
		t.Fatalf("NUL separator is not readable in the wire form: %s", first)
	}
	decoded, err := tableschema.Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decoded, foreman(1)) {
		t.Fatalf("round trip changed the descriptor: %v", decoded)
	}
}

func TestDecodeRefusesUnknownFieldsAndInvalidDocuments(t *testing.T) {
	data, err := tableschema.Encode(foreman(1))
	if err != nil {
		t.Fatal(err)
	}
	widened := bytes.Replace(data, []byte(`"table":"graph"`), []byte(`"table":"graph","later_field":1`), 1)
	if _, err := tableschema.Decode(widened); err == nil || !strings.Contains(err.Error(), "later_field") {
		t.Fatalf("unknown field accepted: %v", err)
	}
	if _, err := tableschema.Decode([]byte(`{"table":"graph"}`)); err == nil {
		t.Fatal("document without schema_id accepted")
	}
	if _, err := tableschema.Decode([]byte(`not json`)); err == nil {
		t.Fatal("malformed document accepted")
	}
}

func TestValidate(t *testing.T) {
	mutate := func(f func(s *schemapb.TableSchema)) *schemapb.TableSchema {
		s := foreman(1)
		f(s)
		return s
	}
	cases := []struct {
		name   string
		schema *schemapb.TableSchema
		want   string
	}{
		{"nil", nil, "nil descriptor"},
		{"reserved table", mutate(func(s *schemapb.TableSchema) { s.Table = tableschema.Table }), "reserved"},
		{"bad table name", mutate(func(s *schemapb.TableSchema) { s.Table = "a/b" }), "invalid table name"},
		{"no id", mutate(func(s *schemapb.TableSchema) { s.SchemaId = "" }), "schema_id is required"},
		{"zero version", mutate(func(s *schemapb.TableSchema) { s.Version = 0 }), "not positive"},
		{"no nodes", mutate(func(s *schemapb.TableSchema) { s.Nodes = nil }), "at least one node"},
		{"node without prefix", mutate(func(s *schemapb.TableSchema) { s.Nodes[0].RowPrefix = "" }), "no row_prefix"},
		{"duplicate names across kinds", mutate(func(s *schemapb.TableSchema) { s.Edges[0].Name = "entity" }), "reuses the name"},
		{"overlapping node prefixes", mutate(func(s *schemapb.TableSchema) { s.Nodes[1].RowPrefix = "ent:x" }), "overlap"},
		{"posting prefix overlaps node", mutate(func(s *schemapb.TableSchema) {
			s.Postings = []*schemapb.PostingSchema{{Name: "term", TermRowPrefix: "e"}}
		}), "overlap"},
		{"kind index without sep", mutate(func(s *schemapb.TableSchema) { s.Nodes[0].KindSep = ""; s.Nodes[0].KindIndex = 1 }), "without kind_sep"},
		{"kinds without sep", mutate(func(s *schemapb.TableSchema) { s.Nodes[0].KindSep = "" }), "without kind_sep"},
		{"duplicate kinds", mutate(func(s *schemapb.TableSchema) { s.Nodes[0].Kinds = []string{"a", "a"} }), "repeat"},
		{"edge without cf", mutate(func(s *schemapb.TableSchema) { s.Edges[0].EdgeCf = "" }), "no edge_cf"},
		{"edge field", mutate(func(s *schemapb.TableSchema) { s.Edges[0].EdgeField = "row" }), "edge_field"},
		{"relationships without sep", mutate(func(s *schemapb.TableSchema) {
			s.Edges[0].FieldSep = ""
			s.Edges[0].IdIndexSet = false
			s.Edges[0].IdIndex = 0
		}), "without field_sep"},
		{"id and rel index collide", mutate(func(s *schemapb.TableSchema) { s.Edges[0].IdIndex = 0 }), "both 0"},
		{"edge cf claimed twice", mutate(func(s *schemapb.TableSchema) { s.Nodes[0].AttributeCfs = []string{"edge"} }), "both claim column family"},
		{"vector without space", mutate(func(s *schemapb.TableSchema) {
			s.Vectors = []*schemapb.VectorSchema{{Name: "vec", EmbeddingCf: "vec"}}
		}), "no embedding_space"},
		{"vector metric", mutate(func(s *schemapb.TableSchema) {
			s.Vectors = []*schemapb.VectorSchema{{Name: "vec", EmbeddingCf: "vec", EmbeddingSpace: "e", Metric: "hamming"}}
		}), "metric"},
		{"posting id source", mutate(func(s *schemapb.TableSchema) {
			s.Postings = []*schemapb.PostingSchema{{Name: "term", TermRowPrefix: "idx:", IdSource: "row"}}
		}), "id_source"},
		{"unknown enum", mutate(func(s *schemapb.TableSchema) { s.Nodes[0].AttributeEncoding = 99 }), "unknown attribute_encoding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tableschema.Validate(tc.schema)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := tableschema.Validate(foreman(1)); err != nil {
		t.Fatal(err)
	}
	if err := tableschema.Validate(tableschema.ForGraphSchema("graph", "space:v1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := tableschema.Validate(tableschema.ForGraphSchema("graph", "", 1)); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetAndPointer(t *testing.T) {
	eng := open(t)
	store := tableschema.New(eng)

	if _, err := store.Get("graph"); !errors.Is(err, tableschema.ErrNotDescribed) {
		t.Fatalf("get before any put: %v", err)
	}
	if entries, err := store.Described(); err != nil || len(entries) != 0 {
		t.Fatalf("described before any put = %v, %v", entries, err)
	}
	for _, name := range eng.TableNames() {
		if name == tableschema.Table {
			t.Fatal("reserved table exists before the first put")
		}
	}

	entry, err := store.Put(foreman(1))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != 1 || entry.Table != "graph" {
		t.Fatalf("entry = %+v", entry)
	}
	got, err := store.Get("graph")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got.Schema, foreman(1)) || !bytes.Equal(got.Bytes, entry.Bytes) {
		t.Fatalf("get = %+v", got)
	}
	pointer, ok, err := eng.TableSchema("graph")
	if err != nil || !ok || pointer != (engine.SchemaPointer{ID: "foreman.graph", Version: 1}) {
		t.Fatalf("pointer = %+v, %v, %v", pointer, ok, err)
	}

	// The document is readable as plain cells of the reserved table.
	sc, err := eng.Scan(tableschema.Table, iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cells := 0
	for sc.Next() {
		key := sc.Key()
		if string(key.Row) != "graph" || string(key.ColumnFamily) != tableschema.SchemaCF || string(key.ColumnQualifier) != "00000000000000000001" {
			t.Fatalf("unexpected cell %q %q %q", key.Row, key.ColumnFamily, key.ColumnQualifier)
		}
		if !bytes.Equal(sc.Value(), entry.Bytes) {
			t.Fatalf("cell value = %s", sc.Value())
		}
		cells++
		sc.Advance()
	}
	sc.Close()
	if cells != 1 {
		t.Fatalf("cells = %d", cells)
	}
}

func TestPutIsMonotonicAndAppendOnly(t *testing.T) {
	eng := open(t)
	store := tableschema.New(eng)
	if _, err := store.Put(foreman(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(foreman(1)); !errors.Is(err, tableschema.ErrNotNewer) {
		t.Fatalf("older version accepted: %v", err)
	}
	changed := foreman(2)
	changed.Description = "revised without a version bump"
	if _, err := store.Put(changed); !errors.Is(err, tableschema.ErrConflict) {
		t.Fatalf("same version with different bytes accepted: %v", err)
	}
	// Identical re-put is a no-op that leaves one cell.
	if _, err := store.Put(foreman(2)); err != nil {
		t.Fatal(err)
	}
	if versions, err := store.Versions("graph"); err != nil || len(versions) != 1 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
	if _, err := store.Put(foreman(5)); err != nil {
		t.Fatal(err)
	}
	versions, err := store.Versions("graph")
	if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 5 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
	if got, err := store.Get("graph"); err != nil || got.Version != 5 {
		t.Fatalf("latest = %+v, %v", got, err)
	}
	if got, err := store.GetVersion("graph", 2); err != nil || got.Version != 2 {
		t.Fatalf("version 2 = %+v, %v", got, err)
	}
	if _, err := store.GetVersion("graph", 3); err == nil || !strings.Contains(err.Error(), "no version 3") {
		t.Fatalf("missing version: %v", err)
	}
	pointer, _, _ := eng.TableSchema("graph")
	if pointer.Version != 5 {
		t.Fatalf("pointer = %+v", pointer)
	}
}

func TestPutRequiresTheDescribedTable(t *testing.T) {
	eng := open(t)
	store := tableschema.New(eng)
	missing := foreman(1)
	missing.Table = "absent"
	if _, err := store.Put(missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("put for an absent table: %v", err)
	}
	for _, name := range eng.TableNames() {
		if name == tableschema.Table {
			t.Fatal("refused put created the reserved table")
		}
	}
	if _, err := store.Get("absent"); !errors.Is(err, tableschema.ErrNotDescribed) {
		t.Fatalf("get for an absent table: %v", err)
	}
}

func TestDescribedListsNewestPerTableAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"graph", "events"} {
		if err := eng.CreateTable(name, engine.TableOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	store := tableschema.New(eng)
	events := tableschema.ForGraphSchema("events", "space:v1", 3)
	for _, s := range []*schemapb.TableSchema{foreman(1), foreman(2), events} {
		if _, err := store.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store = tableschema.New(eng)
	described, err := store.Described()
	if err != nil {
		t.Fatal(err)
	}
	if len(described) != 2 || described[0].Table != "events" || described[0].Version != 3 ||
		described[1].Table != "graph" || described[1].Version != 2 {
		t.Fatalf("described = %+v", described)
	}
	if !proto.Equal(described[0].Schema, events) {
		t.Fatalf("events descriptor changed across reopen: %v", described[0].Schema)
	}
	stats := eng.Stats()
	byName := map[string]engine.TableStat{}
	for _, st := range stats {
		byName[st.Name] = st
	}
	if st := byName["graph"]; st.Schema == nil || *st.Schema != (engine.SchemaPointer{ID: "foreman.graph", Version: 2}) {
		t.Fatalf("graph stat = %+v", st)
	}
	if st := byName["events"]; st.Schema == nil || st.Schema.ID != tableschema.GraphSchemaID || st.Schema.Version != 3 {
		t.Fatalf("events stat = %+v", st)
	}
	if st := byName[tableschema.Table]; st.Schema != nil {
		t.Fatalf("reserved table is described: %+v", st)
	}
}

func TestQualifierRoundTrip(t *testing.T) {
	for _, v := range []int64{1, 42, 1 << 40} {
		got, err := tableschema.ParseQualifier(tableschema.Qualifier(v))
		if err != nil || got != v {
			t.Fatalf("qualifier %d -> %d, %v", v, got, err)
		}
	}
	for _, bad := range []string{"", "1", "00000000000000000000", "0000000000000000000a"} {
		if _, err := tableschema.ParseQualifier([]byte(bad)); err == nil {
			t.Fatalf("qualifier %q accepted", bad)
		}
	}
}

// Only the newest cell of a version counts, and a version whose newest cell
// is a delete marker is gone: a tombstone or a duplicate timestamp written
// behind the Store's back neither wedges reads nor lets a stale document pass
// as the recorded one.
func TestReadsSeeOnlyTheNewestCellAndHonourDeletes(t *testing.T) {
	eng := open(t)
	store := tableschema.New(eng)
	if _, err := store.Put(foreman(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(foreman(2)); err != nil {
		t.Fatal(err)
	}
	// A raw second cell for version 2 with a newer timestamp and other bytes.
	revised := foreman(2)
	revised.Description = "written behind the store's back"
	revisedBytes, err := tableschema.Encode(revised)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("graph"))
	m.Put([]byte(tableschema.SchemaCF), tableschema.Qualifier(2), nil, cclient.MutationLatestTimestamp, revisedBytes)
	if err := eng.Write(tableschema.Table, []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("graph")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, revisedBytes) {
		t.Fatalf("get returned the older cell: %s", got.Bytes)
	}
	if versions, err := store.Versions("graph"); err != nil || len(versions) != 2 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
	// The stale document is no longer "identical" to the recorded version.
	if _, err := store.Put(foreman(2)); !errors.Is(err, tableschema.ErrConflict) {
		t.Fatalf("stale re-put accepted: %v", err)
	}
	// Deleting version 2 leaves version 1 current, and a later put is again
	// possible; nothing is wedged.
	d, _ := cclient.NewMutation([]byte("graph"))
	d.DeleteLatest([]byte(tableschema.SchemaCF), tableschema.Qualifier(2), nil)
	if err := eng.Write(tableschema.Table, []*cclient.Mutation{d}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get("graph"); err != nil || got.Version != 1 {
		t.Fatalf("after delete: %+v, %v", got, err)
	}
	if described, err := store.Described(); err != nil || len(described) != 1 || described[0].Version != 1 {
		t.Fatalf("described after delete = %v, %v", described, err)
	}
	if _, err := store.Put(foreman(3)); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get("graph"); err != nil || got.Version != 3 {
		t.Fatalf("after re-put: %+v, %v", got, err)
	}
}

// Recording a version is a conditional write: a cell that appeared between
// the Store's read and its write is detected, and the put is refused unless
// that cell carries the same document.
func TestPutIsConditionalOnTheVersionBeingAbsent(t *testing.T) {
	eng := open(t)
	store := tableschema.New(eng)
	if _, err := store.Put(foreman(1)); err != nil {
		t.Fatal(err)
	}
	other := foreman(2)
	other.Description = "recorded by another process"
	otherBytes, _ := tableschema.Encode(other)
	m, _ := cclient.NewMutation([]byte("graph"))
	m.Put([]byte(tableschema.SchemaCF), tableschema.Qualifier(2), nil, cclient.MutationLatestTimestamp, otherBytes)
	if err := eng.Write(tableschema.Table, []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(foreman(2)); !errors.Is(err, tableschema.ErrConflict) {
		t.Fatalf("conflicting concurrent version accepted: %v", err)
	}
	if _, err := store.Put(other); err != nil {
		t.Fatalf("identical concurrent version refused: %v", err)
	}
	if versions, _ := store.Versions("graph"); len(versions) != 2 {
		t.Fatalf("versions = %v", versions)
	}
}

func TestValidateRefusesTheQuietHoles(t *testing.T) {
	mutate := func(f func(s *schemapb.TableSchema)) *schemapb.TableSchema {
		s := foreman(1)
		f(s)
		return s
	}
	for name, tc := range map[string]struct {
		schema *schemapb.TableSchema
		want   string
	}{
		"edge token in value plus payload": {mutate(func(s *schemapb.TableSchema) {
			s.Edges[0].EdgeField = "value"
			s.Edges[0].ValueEncoding = schemapb.ValueEncoding_VALUE_ENCODING_FLOAT32_BE
		}), "cannot also carry"},
		"id_index without id_index_set": {mutate(func(s *schemapb.TableSchema) { s.Edges[0].IdIndexSet = false; s.Edges[0].IdIndex = 3 }), "without id_index_set"},
		"blank schema_id":               {mutate(func(s *schemapb.TableSchema) { s.SchemaId = "  " }), "schema_id"},
		"blank name":                    {mutate(func(s *schemapb.TableSchema) { s.Nodes[0].Name = "\t" }), "name is required"},
		"table with newline":            {mutate(func(s *schemapb.TableSchema) { s.Table = "gra\nph" }), "invalid table name"},
		"attribute family is an edge family": {mutate(func(s *schemapb.TableSchema) {
			s.Nodes[0].AttributeCfs = []string{"edge"}
		}), "both claim column family"},
	} {
		if err := tableschema.Validate(tc.schema); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
	}
	// Two namespaces may share an attribute family.
	shared := mutate(func(s *schemapb.TableSchema) {
		s.Nodes[0].AttributeCfs = []string{"attr:"}
		s.Nodes[1].AttributeCfs = []string{"attr:", "content:"}
	})
	if err := tableschema.Validate(shared); err != nil {
		t.Fatal(err)
	}
}

// The graphschema descriptor says what docs/graph-schema.md says: attr: on
// every node row, content: on events, vec: on both namespaces, four edge
// views, term postings.
func TestForGraphSchemaMatchesTheDocumentedLayout(t *testing.T) {
	s := tableschema.ForGraphSchema("graph", "space:v1", 1)
	if err := tableschema.Validate(s); err != nil {
		t.Fatal(err)
	}
	byName := map[string]*schemapb.NodeSchema{}
	for _, n := range s.Nodes {
		byName[n.Name] = n
	}
	if got := byName["event"].AttributeCfs; len(got) != 2 || got[0] != "content:" || got[1] != "attr:" {
		t.Fatalf("event attribute families = %v", got)
	}
	if got := byName["entity"].AttributeCfs; len(got) != 1 || got[0] != "attr:" {
		t.Fatalf("entity attribute families = %v", got)
	}
	if len(s.Edges) != 4 || len(s.Vectors) != 1 || len(s.Postings) != 1 {
		t.Fatalf("%v", s)
	}
}
