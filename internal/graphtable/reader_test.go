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

package graphtable_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/graphtable"
	"github.com/phrocker/shoal-oss/internal/schemapb"
	"github.com/phrocker/shoal-oss/internal/tableschema"
)

// cell is one fixture cell.
type cell struct {
	row, cf, cq string
	value       []byte
}

func openEngine(t *testing.T, tables ...string) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	for _, table := range tables {
		if err := eng.CreateTable(table, engine.TableOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return eng
}

func write(t *testing.T, eng *engine.Engine, table string, cells []cell) {
	t.Helper()
	byRow := map[string][]cell{}
	var rows []string
	for _, c := range cells {
		if _, ok := byRow[c.row]; !ok {
			rows = append(rows, c.row)
		}
		byRow[c.row] = append(byRow[c.row], c)
	}
	for _, row := range rows {
		m, err := cclient.NewMutation([]byte(row))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range byRow[row] {
			m.Put([]byte(c.cf), []byte(c.cq), nil, cclient.MutationLatestTimestamp, c.value)
		}
		if err := eng.Write(table, []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}
}

// foremanSchema is the descriptor of the layout issue #207 describes:
// ent:<kind>|<id> and evt:<kind>|<id> rows, cf = record type, cq = field,
// edges as cf "edge", cq = <rel>\x00<target row>.
func foremanSchema(version int32) *schemapb.TableSchema {
	return &schemapb.TableSchema{
		Table: "graph", SchemaId: "foreman.graph", Version: version,
		Nodes: []*schemapb.NodeSchema{
			{Name: "entity", RowPrefix: "ent:", KindSep: "|", KindIndex: 0, AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD, AttributeEncoding: schemapb.ValueEncoding_VALUE_ENCODING_TEXT},
			{Name: "event", RowPrefix: "evt:", KindSep: "|", KindIndex: 0, AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD, AttributeEncoding: schemapb.ValueEncoding_VALUE_ENCODING_TEXT},
		},
		Edges: []*schemapb.EdgeSchema{{
			Name: "edge", EdgeCf: "edge", EdgeField: "qualifier", FieldSep: "\x00",
			IdIndex: 1, IdIndexSet: true, RelIndex: 0,
		}},
		Postings: []*schemapb.PostingSchema{{Name: "term", TermRowPrefix: "idx:", IdSource: "qualifier"}},
	}
}

func edgeCQ(rel, target string) string { return rel + "\x00" + target }

// foremanCells is a small graph: a service that owns two hosts, one of
// which observed two events, one event caused by the other, and term
// postings over the service and the hosts.
func foremanCells() []cell {
	return []cell{
		{"ent:service|api", "service", "name", []byte("api")},
		{"ent:service|api", "service", "tier", []byte("gold")},
		{"ent:service|api", "edge", edgeCQ("owns", "ent:host|h1"), nil},
		{"ent:service|api", "edge", edgeCQ("owns", "ent:host|h2"), nil},
		{"ent:host|h1", "host", "name", []byte("h1")},
		{"ent:host|h1", "edge", edgeCQ("observed", "evt:alert|e1"), nil},
		{"ent:host|h1", "edge", edgeCQ("observed", "evt:alert|e2"), nil},
		{"ent:host|h2", "host", "name", []byte("h2")},
		{"ent:host|h2", "edge", edgeCQ("peer", "ent:host|h1"), nil},
		{"evt:alert|e1", "alert", "severity", []byte("high")},
		{"evt:alert|e1", "edge", edgeCQ("caused", "evt:alert|e2"), nil},
		{"evt:alert|e2", "alert", "severity", []byte("low")},
		{"evt:alert|e2", "edge", edgeCQ("on", "ent:host|missing"), nil},
		{"idx:api", "", "ent:service|api", nil},
		{"idx:host", "", "ent:host|h1", nil},
		{"idx:host", "", "ent:host|h2", nil},
	}
}

func describe(t *testing.T, eng *engine.Engine, schema *schemapb.TableSchema) {
	t.Helper()
	if _, err := tableschema.New(eng).Put(schema); err != nil {
		t.Fatal(err)
	}
}

func openReader(t *testing.T, store graphtable.Store, table string) *graphtable.Reader {
	t.Helper()
	reader, err := graphtable.Open(context.Background(), store, table)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func nodeIDs(nodes []graphtable.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func edgeKeys(edges []graphtable.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, fmt.Sprintf("%s -%s/%s-> %s", e.From, e.Family, e.Relationship, e.To))
	}
	return out
}

func TestOpenReadsTheNewestDescriptorThroughTheStore(t *testing.T) {
	eng := openEngine(t, "graph")
	describe(t, eng, foremanSchema(1))
	revised := foremanSchema(2)
	revised.Description = "second"
	describe(t, eng, revised)

	reader := openReader(t, graphtable.NewEngineStore(eng), "graph")
	if reader.Schema().GetVersion() != 2 || reader.Schema().GetDescription() != "second" {
		t.Fatalf("reader opened version %d %q, want the newest", reader.Schema().GetVersion(), reader.Schema().GetDescription())
	}
	if _, err := graphtable.Open(context.Background(), graphtable.NewEngineStore(eng), "other"); !errors.Is(err, tableschema.ErrNotDescribed) {
		t.Fatalf("undescribed table opened: %v", err)
	}
	ontology := reader.Ontology()
	if ontology.SchemaID != "foreman.graph" || len(ontology.Namespaces) != 2 || len(ontology.Edges) != 1 || len(ontology.Postings) != 1 {
		t.Fatalf("ontology = %+v", ontology)
	}
}

func TestNeighborhoodWalksTheForemanLayoutHopByHop(t *testing.T) {
	eng := openEngine(t, "graph")
	describe(t, eng, foremanSchema(1))
	write(t, eng, "graph", foremanCells())
	reader := openReader(t, graphtable.NewEngineStore(eng), "graph")

	one, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nodeIDs(one.Nodes), []string{"ent:service|api", "ent:host|h1", "ent:host|h2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("one hop nodes = %v, want %v", got, want)
	}
	if got, want := edgeKeys(one.Edges), []string{"ent:service|api -edge/owns-> ent:host|h1", "ent:service|api -edge/owns-> ent:host|h2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("one hop edges = %v, want %v", got, want)
	}
	seed := one.Nodes[0]
	if seed.Kind != "service" || seed.Namespace != "entity" || !seed.Found {
		t.Fatalf("seed = %+v", seed)
	}
	if got, want := seed.Attributes, []graphtable.Attribute{{Family: "service", Field: "name", Value: "api"}, {Family: "service", Field: "tier", Value: "gold"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seed attributes = %+v, want %+v", got, want)
	}
	// The hop's leaves are read for their kinds and attributes.
	if leaf := one.Nodes[1]; leaf.Kind != "host" || !leaf.Found || len(leaf.Attributes) != 1 {
		t.Fatalf("leaf = %+v", leaf)
	}

	three, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ent:service|api", "ent:host|h1", "ent:host|h2", "evt:alert|e1", "evt:alert|e2", "ent:host|missing"}
	if got := nodeIDs(three.Nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("three hop nodes = %v, want %v", got, want)
	}
	if three.Truncated {
		t.Fatal("complete walk reported truncated")
	}
	// A dangling target is a node the store holds nothing for, and the
	// edge back to an already-known node is still an edge.
	var missing, peer bool
	for _, n := range three.Nodes {
		if n.ID == "ent:host|missing" {
			missing = !n.Found && n.Kind == "host"
		}
	}
	for _, e := range three.Edges {
		if e.From == "ent:host|h2" && e.To == "ent:host|h1" && e.Relationship == "peer" {
			peer = true
		}
	}
	if !missing || !peer {
		t.Fatalf("missing=%v peer=%v in %v", missing, peer, edgeKeys(three.Edges))
	}

	// Relationship and family filters restrict what is followed.
	owns, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}, Depth: 3, Relationships: []string{"owns"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeIDs(owns.Nodes); !reflect.DeepEqual(got, []string{"ent:service|api", "ent:host|h1", "ent:host|h2"}) {
		t.Fatalf("filtered nodes = %v", got)
	}
	if _, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}, Families: []string{"nope"}}); !errors.Is(err, graphtable.ErrInvalid) {
		t.Fatalf("unknown family accepted: %v", err)
	}

	// Fanout and node budget truncate deterministically.
	fan, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}, Fanout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !fan.Truncated || len(fan.Edges) != 1 || fan.Edges[0].To != "ent:host|h1" {
		t.Fatalf("fanout 1 = %+v", fan)
	}
	budget, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}, Depth: 3, MaxNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !budget.Truncated || len(budget.Nodes) != 2 {
		t.Fatalf("max_nodes 2 = %+v", budget)
	}

	// A seed outside every namespace is refused before any read.
	if _, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"_shoal_schema"}}); !errors.Is(err, graphtable.ErrInvalid) {
		t.Fatalf("foreign row accepted: %v", err)
	}
	absent, err := reader.Nodes(context.Background(), []string{"ent:host|nobody"})
	if err != nil || len(absent) != 1 || absent[0].Found {
		t.Fatalf("absent node = %+v, %v", absent, err)
	}
}

func TestPathAndTerms(t *testing.T) {
	eng := openEngine(t, "graph")
	describe(t, eng, foremanSchema(1))
	write(t, eng, "graph", foremanCells())
	reader := openReader(t, graphtable.NewEngineStore(eng), "graph")

	path, found, err := reader.Path(context.Background(), "ent:service|api", "evt:alert|e2", 0, 0, 0)
	if err != nil || !found {
		t.Fatalf("path: found=%v err=%v", found, err)
	}
	if got, want := edgeKeys(path.Edges), []string{"ent:service|api -edge/owns-> ent:host|h1", "ent:host|h1 -edge/observed-> evt:alert|e2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("path = %v, want %v", got, want)
	}
	if got := nodeIDs(path.Nodes); !reflect.DeepEqual(got, []string{"ent:service|api", "ent:host|h1", "evt:alert|e2"}) {
		t.Fatalf("path nodes = %v", got)
	}
	if path.Nodes[2].Attributes[0].Value != "low" {
		t.Fatalf("path node attributes not read: %+v", path.Nodes[2])
	}
	if _, found, err := reader.Path(context.Background(), "evt:alert|e2", "ent:service|api", 0, 0, 0); err != nil || found {
		t.Fatalf("reverse path found=%v err=%v; edges are directed", found, err)
	}
	if _, found, err := reader.Path(context.Background(), "ent:service|api", "evt:alert|e2", 1, 0, 0); err != nil || found {
		t.Fatalf("path within one hop found=%v err=%v", found, err)
	}

	hits, err := reader.Terms(context.Background(), []string{"host", "api", "none"})
	if err != nil {
		t.Fatal(err)
	}
	want := []graphtable.TermHit{
		{Term: "host", NodeIDs: []string{"ent:host|h1", "ent:host|h2"}},
		{Term: "api", NodeIDs: []string{"ent:service|api"}},
		{Term: "none", NodeIDs: []string{}},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("terms = %+v, want %+v", hits, want)
	}
}

func TestReadsSeeOnlyTheNewestLiveCell(t *testing.T) {
	eng := openEngine(t, "graph")
	describe(t, eng, foremanSchema(1))
	write(t, eng, "graph", foremanCells())
	// Rewrite one attribute and delete one edge.
	write(t, eng, "graph", []cell{{"ent:service|api", "service", "tier", []byte("silver")}})
	m, _ := cclient.NewMutation([]byte("ent:service|api"))
	m.Delete([]byte("edge"), []byte(edgeCQ("owns", "ent:host|h2")), nil, cclient.MutationLatestTimestamp)
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	reader := openReader(t, graphtable.NewEngineStore(eng), "graph")
	got, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Edges) != 1 || got.Edges[0].To != "ent:host|h1" {
		t.Fatalf("deleted edge still read: %v", edgeKeys(got.Edges))
	}
	if tier := got.Nodes[0].Attributes[1]; tier.Field != "tier" || tier.Value != "silver" {
		t.Fatalf("stale attribute read: %+v", got.Nodes[0].Attributes)
	}
}

// graphschemaCells lays the same graph out by docs/graph-schema.md: event
// and entity rows, attr: and content: families, and four edge views with
// the neighbor id in the qualifier and a float32 weight.
func graphschemaCells() []cell {
	ev := func(id string) string { return string(graphschema.EventRow(id)) }
	en := func(name string) string { return string(graphschema.EntityRow(name)) }
	w := func(v float32) []byte { return graphschema.PackWeight(v) }
	return []cell{
		{ev("01A"), graphschema.ContentCFName, "text", []byte("deploy started")},
		{ev("01A"), graphschema.AttributeCFName, "actor", []byte("ci")},
		{ev("01A"), graphschema.TemporalEdgeCFName, "01B", w(1)},
		{ev("01A"), graphschema.EntityEdgeCFName, "api", w(0.5)},
		{ev("01B"), graphschema.ContentCFName, "text", []byte("alert fired")},
		{ev("01B"), graphschema.TemporalEdgeCFName, "01C", w(1)},
		{ev("01B"), graphschema.CausalEdgeCFName, "01C", w(0.9)},
		{ev("01C"), graphschema.ContentCFName, "text", []byte("rollback")},
		{ev("01C"), graphschema.SemanticEdgeCFName, "01A", w(0.3)},
		{en("api"), graphschema.AttributeCFName, "tier", []byte("gold")},
		{en("api"), graphschema.EntityEdgeCFName, "h1", nil},
		{en("h1"), graphschema.AttributeCFName, "role", []byte("host")},
		{string(graphschema.TermRow("deploy")), "p", "01A", nil},
		{string(graphschema.TermRow("alert")), "p", "01B", nil},
	}
}

// pushdownNeighbors is the engine's own one-hop answer for an anchor over
// one edge family: EdgeExpand with the graphschema parameters from
// docs/graph-schema.md, returning the neighbor rows.
func pushdownNeighbors(t *testing.T, eng *engine.Engine, anchor string, cf []byte, primaryPrefix string) []string {
	t.Helper()
	cells, err := embedstore.New(eng).Scan(context.Background(), "memory", &embedpb.ScanRequest{
		Table: "memory",
		EdgeExpand: &embedpb.EdgeExpand{
			AnchorRows: [][]byte{[]byte(anchor)}, EdgeCf: cf, EdgeField: "qualifier",
			PrimaryPrefix: []byte(primaryPrefix), MaxHops: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var rows []string
	for _, c := range cells {
		if row := string(c.GetRow()); !seen[row] {
			seen[row] = true
			rows = append(rows, row)
		}
	}
	sort.Strings(rows)
	return rows
}

func TestDescriptorReadAgreesWithGraphschemaPushdown(t *testing.T) {
	eng := openEngine(t, "memory")
	describe(t, eng, tableschema.ForGraphSchema("memory", "", 1))
	write(t, eng, "memory", graphschemaCells())
	reader := openReader(t, graphtable.NewEngineStore(eng), "memory")

	type view struct {
		name, prefix string
		cf           []byte
	}
	views := []view{
		{"temporal", graphschema.EventRowPrefix, graphschema.TemporalEdgeCF()},
		{"causal", graphschema.EventRowPrefix, graphschema.CausalEdgeCF()},
		{"semantic", graphschema.EventRowPrefix, graphschema.SemanticEdgeCF()},
		{"entity_edge", graphschema.EntityRowPrefix, graphschema.EntityEdgeCF()},
	}
	anchors := []string{"evt:01A", "evt:01B", "evt:01C", "ent:api", "ent:h1"}
	for _, v := range views {
		for _, anchor := range anchors {
			// The documented EdgeExpand mapping stores ids without the
			// namespace, so primary_prefix supplies it: event ids for the
			// event views and entity names for the entity view. The
			// descriptor has to say the same thing, or the two readings
			// differ.
			want := pushdownNeighbors(t, eng, anchor, v.cf, v.prefix)
			got, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{anchor}, Families: []string{v.name}})
			if err != nil {
				t.Fatal(err)
			}
			var rows []string
			for _, n := range got.Nodes[1:] {
				if n.Found {
					rows = append(rows, n.ID)
				}
			}
			sort.Strings(rows)
			if want == nil {
				want = []string{}
			}
			if rows == nil {
				rows = []string{}
			}
			if !reflect.DeepEqual(rows, want) {
				t.Fatalf("%s from %s: descriptor read %v, pushdown %v", v.name, anchor, rows, want)
			}
		}
	}
	// Weights and content come through the descriptor as the doc says.
	got, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"evt:01B"}})
	if err != nil {
		t.Fatal(err)
	}
	var causal bool
	for _, e := range got.Edges {
		if e.Family == "causal" && e.To == "evt:01C" && e.Weighted && e.Weight == 0.9 {
			causal = true
		}
	}
	if !causal {
		t.Fatalf("weighted causal edge not read: %+v", got.Edges)
	}
	if attrs := got.Nodes[0].Attributes; len(attrs) != 1 || attrs[0].Family != graphschema.ContentCFName || attrs[0].Value != "alert fired" {
		t.Fatalf("event content not read as an attribute: %+v", attrs)
	}
	if got.Nodes[0].Kind != "event" {
		t.Fatalf("kind = %q; a layout without a kind in the row reports the namespace", got.Nodes[0].Kind)
	}
	hits, err := reader.Terms(context.Background(), []string{"deploy"})
	if err != nil || !reflect.DeepEqual(hits[0].NodeIDs, []string{"evt:01A"}) {
		t.Fatalf("terms = %+v, %v", hits, err)
	}
}

func TestForGraphSchemaNamesTheTargetNamespaceOfEachView(t *testing.T) {
	// The graphschema qualifier holds an id, not a row, so the parity above
	// depends on ForGraphSchema carrying the documented primary prefix of
	// each view.
	schema := tableschema.ForGraphSchema("memory", "", 1)
	for _, edge := range schema.GetEdges() {
		want := graphschema.EventRowPrefix
		if edge.GetName() == "entity_edge" {
			want = graphschema.EntityRowPrefix
		}
		if edge.GetPrimaryPrefix() != want {
			t.Fatalf("%s primary_prefix = %q, want %q", edge.GetName(), edge.GetPrimaryPrefix(), want)
		}
	}
}

// split writes each row of the fixture to the engine owning it.
func split(t *testing.T, cells []cell, schema *schemapb.TableSchema, boundary string) (low, high *engine.Engine) {
	t.Helper()
	low, high = openEngine(t, "graph"), openEngine(t, "graph")
	describe(t, low, schema)
	describe(t, high, schema)
	var lows, highs []cell
	for _, c := range cells {
		if c.row < boundary {
			lows = append(lows, c)
		} else {
			highs = append(highs, c)
		}
	}
	write(t, low, "graph", lows)
	write(t, high, "graph", highs)
	return low, high
}

func TestShardedReadAgreesWithOneStore(t *testing.T) {
	whole := openEngine(t, "graph")
	describe(t, whole, foremanSchema(1))
	write(t, whole, "graph", foremanCells())
	one := openReader(t, graphtable.NewEngineStore(whole), "graph")

	// The boundary falls inside the entity namespace, so a hop crosses
	// shards in both directions.
	low, high := split(t, foremanCells(), foremanSchema(1), "ent:host|h2")
	sharded, err := graphtable.NewSharded([]graphtable.Shard{
		{Name: "high", Start: []byte("ent:host|h2"), Store: graphtable.NewEngineStore(high)},
		{Name: "low", End: []byte("ent:host|h2"), Store: graphtable.NewEngineStore(low)},
	})
	if err != nil {
		t.Fatal(err)
	}
	two := openReader(t, sharded, "graph")

	for _, seed := range []string{"ent:service|api", "ent:host|h2", "evt:alert|e1"} {
		for depth := 1; depth <= 3; depth++ {
			req := graphtable.NeighborhoodRequest{Seeds: []string{seed}, Depth: depth}
			a, err := one.Neighborhood(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			b, err := two.Neighborhood(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("seed %s depth %d: one store %+v, sharded %+v", seed, depth, a, b)
			}
		}
	}
	a, _, err := one.Path(context.Background(), "ent:service|api", "evt:alert|e2", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, found, err := two.Path(context.Background(), "ent:service|api", "evt:alert|e2", 0, 0, 0)
	if err != nil || !found || !reflect.DeepEqual(a, b) {
		t.Fatalf("sharded path = %+v (found %v, err %v), one store %+v", b, found, err, a)
	}
	ta, _ := one.Terms(context.Background(), []string{"host", "api"})
	tb, err := two.Terms(context.Background(), []string{"host", "api"})
	if err != nil || !reflect.DeepEqual(ta, tb) {
		t.Fatalf("sharded terms = %+v, %v; one store %+v", tb, err, ta)
	}

	// Shards must agree on the descriptor.
	drift := openEngine(t, "graph")
	describe(t, drift, foremanSchema(2))
	disagree, err := graphtable.NewSharded([]graphtable.Shard{
		{Name: "a", End: []byte("m"), Store: graphtable.NewEngineStore(low)},
		{Name: "b", Start: []byte("m"), Store: graphtable.NewEngineStore(drift)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graphtable.Open(context.Background(), disagree, "graph"); err == nil || !strings.Contains(err.Error(), "every shard must carry the same descriptor") {
		t.Fatalf("disagreeing shards opened: %v", err)
	}
}

func TestShardTilingIsValidated(t *testing.T) {
	store := graphtable.NewEngineStore(openEngine(t))
	cases := map[string][]graphtable.Shard{
		"gap":       {{Name: "a", End: []byte("f"), Store: store}, {Name: "b", Start: []byte("m"), Store: store}},
		"overlap":   {{Name: "a", End: []byte("m"), Store: store}, {Name: "b", Start: []byte("f"), Store: store}},
		"open low":  {{Name: "a", Start: []byte("b"), Store: store}},
		"open high": {{Name: "a", End: []byte("y"), Store: store}},
		"no store":  {{Name: "a"}},
		"dup name":  {{Name: "a", End: []byte("m"), Store: store}, {Name: "a", Start: []byte("m"), Store: store}},
		"empty":     {{Name: "a", Start: []byte("m"), End: []byte("m"), Store: store}},
	}
	for name, shards := range cases {
		if _, err := graphtable.NewSharded(shards); err == nil {
			t.Fatalf("%s tiling accepted", name)
		}
	}
	ok, err := graphtable.NewSharded([]graphtable.Shard{{Name: "a", End: []byte("m"), Store: store}, {Name: "b", Start: []byte("m"), Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ok.Scan(context.Background(), &embedpb.ScanRequest{Table: "graph", EdgeExpand: &embedpb.EdgeExpand{}}); err == nil {
		t.Fatal("pushdown routed across shards")
	}
}

func TestParseShardSpecs(t *testing.T) {
	specs, err := graphtable.ParseShardSpecs("10.0.0.1:9100=..m, 10.0.0.2:9100=m..")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Address != "10.0.0.1:9100" || string(specs[0].End) != "m" || string(specs[1].Start) != "m" || specs[1].End != nil {
		t.Fatalf("specs = %+v", specs)
	}
	single, err := graphtable.ParseShardSpecs("localhost:9100")
	if err != nil || len(single) != 1 || single[0].Start != nil || single[0].End != nil {
		t.Fatalf("single = %+v, %v", single, err)
	}
	for _, bad := range []string{"", "a:1,b:2", "a:1=m", "a:1=m..a", "=..m"} {
		if _, err := graphtable.ParseShardSpecs(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// embedServer is the smallest ShoalEmbed service: scans over one engine,
// as cmd/shoal-embed serves them.
type embedServer struct {
	embedpb.UnimplementedShoalEmbedServer
	store *embedstore.EngineStore
}

func (s *embedServer) ScanV2(req *embedpb.ScanRequest, stream embedpb.ShoalEmbed_ScanV2Server) error {
	cells, err := s.store.Scan(stream.Context(), req.GetTable(), req)
	if err != nil {
		return err
	}
	for len(cells) > 0 {
		n := 3
		if n > len(cells) {
			n = len(cells)
		}
		if err := stream.Send(&embedpb.ScanResponse{Cells: cells[:n]}); err != nil {
			return err
		}
		cells = cells[n:]
	}
	return nil
}

func serve(t *testing.T, eng *engine.Engine) embedpb.ShoalEmbedClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	embedpb.RegisterShoalEmbedServer(server, &embedServer{store: embedstore.New(eng)})
	go func() { _ = server.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close() })
	return embedpb.NewShoalEmbedClient(conn)
}

func TestRemoteStoresServeTheSameWalk(t *testing.T) {
	whole := openEngine(t, "graph")
	describe(t, whole, foremanSchema(1))
	write(t, whole, "graph", foremanCells())
	local := openReader(t, graphtable.NewEngineStore(whole), "graph")

	low, high := split(t, foremanCells(), foremanSchema(1), "evt:")
	sharded, err := graphtable.NewSharded([]graphtable.Shard{
		{Name: "entities", End: []byte("evt:"), Store: graphtable.NewRemoteStore(serve(t, low))},
		{Name: "events", Start: []byte("evt:"), Store: graphtable.NewRemoteStore(serve(t, high))},
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := openReader(t, sharded, "graph")
	if remote.Schema().GetTable() != local.Schema().GetTable() {
		t.Fatal("descriptor differs")
	}
	req := graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|api"}, Depth: 3}
	a, err := local.Neighborhood(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := remote.Neighborhood(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("remote %+v, local %+v", b, a)
	}
	bounded := graphtable.NewRemoteStore(serve(t, whole))
	bounded.MaxCells = 2
	if _, err := bounded.Scan(context.Background(), &embedpb.ScanRequest{Table: "graph", RowPrefix: "ent:"}); err == nil {
		t.Fatal("remote cell bound not enforced")
	}
}

// foremanPushdownNodes is the engine's own answer for the Foreman layout:
// EdgeExpand with the descriptor's parameters, N hops, returning the rows
// it reached that exist.
func foremanPushdownNodes(t *testing.T, eng *engine.Engine, anchor string, hops int) []string {
	t.Helper()
	cells, err := embedstore.New(eng).Scan(context.Background(), "graph", &embedpb.ScanRequest{
		Table: "graph",
		EdgeExpand: &embedpb.EdgeExpand{
			AnchorRows: [][]byte{[]byte(anchor)}, EdgeCf: []byte("edge"), EdgeField: "qualifier",
			FieldSep: []byte("\x00"), IdIndex: 1, IdIndexSet: true, MaxHops: int32(hops),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var rows []string
	for _, c := range cells {
		if row := string(c.GetRow()); !seen[row] {
			seen[row] = true
			rows = append(rows, row)
		}
	}
	sort.Strings(rows)
	if rows == nil {
		rows = []string{}
	}
	return rows
}

func TestForemanWalkAgreesWithEdgeExpandAndReportsWhatItDrops(t *testing.T) {
	eng := openEngine(t, "graph")
	describe(t, eng, foremanSchema(1))
	cells := foremanCells()
	// One edge names a target row that is not UTF-8, one a posting row
	// outside every node namespace, and one a key past the row bound. The
	// engine follows all three; the reader names none and says so.
	cells = append(cells,
		cell{"ent:host|h2", "edge", edgeCQ("binary", "ent:host|\xff"), nil},
		cell{"ent:host|\xff", "host", "name", []byte("binary")},
		cell{"ent:host|h2", "edge", edgeCQ("posted", "idx:host"), nil},
		cell{"ent:host|h2", "edge", edgeCQ("long", "ent:host|"+strings.Repeat("L", graphtable.MaxRowBytes)), nil},
	)
	write(t, eng, "graph", cells)
	reader := openReader(t, graphtable.NewEngineStore(eng), "graph")

	for _, anchor := range []string{"ent:service|api", "ent:host|h2", "evt:alert|e1"} {
		for hops := 1; hops <= 3; hops++ {
			engineRows := foremanPushdownNodes(t, eng, anchor, hops)
			got, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{anchor}, Depth: hops})
			if err != nil {
				t.Fatal(err)
			}
			var readerRows []string
			for _, n := range got.Nodes[1:] {
				if n.Found {
					readerRows = append(readerRows, n.ID)
				}
			}
			sort.Strings(readerRows)
			// The engine reaches the binary row and the posting row; the
			// reader cannot name either and reports the dropped edges.
			var expected []string
			for _, row := range engineRows {
				nameable := utf8.ValidString(row) && len(row) <= graphtable.MaxRowBytes && (strings.HasPrefix(row, "ent:") || strings.HasPrefix(row, "evt:"))
				if nameable {
					expected = append(expected, row)
				}
			}
			if expected == nil {
				expected = []string{}
			}
			if readerRows == nil {
				readerRows = []string{}
			}
			if !reflect.DeepEqual(readerRows, expected) {
				t.Fatalf("%s %d hops: reader %v, engine %v", anchor, hops, readerRows, engineRows)
			}
			// h2's edges are read when h2 is the seed, or from the service
			// at two hops or more; the alert never reaches h2.
			reachesH2 := anchor == "ent:host|h2" || (anchor == "ent:service|api" && hops >= 2)
			if reachesH2 && got.DroppedEdges != 3 {
				t.Fatalf("%s %d hops: dropped %d edges, want the binary, posting, and long targets", anchor, hops, got.DroppedEdges)
			}
			for _, n := range got.Nodes {
				if n.Namespace == "" || !utf8.ValidString(n.ID) || len(n.ID) > graphtable.MaxRowBytes {
					t.Fatalf("%s %d hops: walk produced an unnameable node %+v", anchor, hops, n)
				}
			}
		}
	}
	path, found, err := reader.Path(context.Background(), "ent:service|api", "evt:alert|e2", 0, 0, 0)
	if err != nil || !found || path.DroppedEdges != 3 {
		t.Fatalf("path dropped %d (found %v, err %v)", path.DroppedEdges, found, err)
	}
}

// hubStore serves one row with more cells than the reader's row bound and
// records the limit of every scan it was asked for.
type hubStore struct {
	inner  graphtable.Store
	mu     sync.Mutex
	limits []int32
}

func (h *hubStore) Scan(ctx context.Context, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	h.mu.Lock()
	h.limits = append(h.limits, req.GetLimit())
	h.mu.Unlock()
	return h.inner.Scan(ctx, req)
}

func TestARowPastTheCellBoundIsReadUpToItAndReportedTruncated(t *testing.T) {
	eng := openEngine(t, "graph")
	describe(t, eng, foremanSchema(1))
	var cells []cell
	for i := 0; i < graphtable.MaxRowCells+50; i++ {
		cells = append(cells, cell{"ent:service|hub", "edge", edgeCQ("owns", fmt.Sprintf("ent:host|%06d", i)), nil})
	}
	cells = append(cells, cell{"ent:host|000000", "host", "name", []byte("first")})
	write(t, eng, "graph", cells)
	hub := &hubStore{inner: graphtable.NewEngineStore(eng)}
	reader := openReader(t, hub, "graph")
	got, err := reader.Neighborhood(context.Background(), graphtable.NeighborhoodRequest{Seeds: []string{"ent:service|hub"}, Fanout: 2, MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Edges) != 2 || len(got.Nodes) != 3 {
		t.Fatalf("hub expansion = truncated %v, %d edges, %d nodes", got.Truncated, len(got.Edges), len(got.Nodes))
	}
	for _, limit := range hub.limits[1:] { // the first scan read the descriptor
		if limit != graphtable.MaxRowCells+1 {
			t.Fatalf("a row scan carried limit %d, want %d", limit, graphtable.MaxRowCells+1)
		}
	}
	remote := graphtable.NewRemoteStore(nil)
	if remote.MaxCells != graphtable.DefaultMaxRemoteCells {
		t.Fatalf("remote store default bound = %d", remote.MaxCells)
	}
}
