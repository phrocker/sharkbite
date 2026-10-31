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

package webapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphtable"
	"github.com/phrocker/shoal-oss/internal/schemapb"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

type graphTableStub struct {
	last           string
	seeds          []string
	depth          int
	invalid        bool
	cellVisibility bool
	next, after    string
}

type graphVisibilityStore struct{ cells []*embedpb.Cell }

func (s graphVisibilityStore) Scan(_ context.Context, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	if string(req.GetStartRow()) == "ent:service|api" {
		return s.cells, nil
	}
	return nil, nil
}

func TestGraphTableVisibilityUsesCallerGrantsAndCVGrammar(t *testing.T) {
	allowedComponent, _ := auth.EncodeComponent([]byte("domain"))
	deniedComponent, _ := auth.EncodeComponent([]byte("other"))
	allowed := "d:" + allowedComponent
	denied := "d:" + deniedComponent
	row := []byte("ent:service|api")
	store := graphVisibilityStore{cells: []*embedpb.Cell{
		{Row: row, ColumnFamily: []byte("service"), ColumnQualifier: []byte("name"), ColumnVisibility: []byte(allowed), Value: []byte("visible")},
		{Row: row, ColumnFamily: []byte("service"), ColumnQualifier: []byte("secret"), ColumnVisibility: []byte(denied), Value: []byte("hidden")},
		{Row: row, ColumnFamily: []byte("service"), ColumnQualifier: []byte("either"), ColumnVisibility: []byte(allowed + "|" + denied), Value: []byte("or visible")},
		{Row: row, ColumnFamily: []byte("service"), ColumnQualifier: []byte("both"), ColumnVisibility: []byte(allowed + "&" + denied), Value: []byte("and hidden")},
	}}
	reader, err := graphtable.New(&schemapb.TableSchema{Table: "graph", SchemaId: "foreman.graph", Version: 1, CellVisibility: true,
		Nodes: []*schemapb.NodeSchema{{Name: "entity", RowPrefix: "ent:", KindSep: "|", AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD, AttributeEncoding: schemapb.ValueEncoding_VALUE_ENCODING_TEXT}}}, store)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationNeighborhood}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("label-policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "request"})
	if err != nil {
		t.Fatal(err)
	}
	g := &graphTableHTTP{surface: GraphTableSurface{Reader: reader}}
	ctx, err := g.cellVisibilityContext(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := reader.Nodes(ctx, []string{"ent:service|api"})
	if err != nil {
		t.Fatal(err)
	}
	if !nodes[0].Found || len(nodes[0].Attributes) != 2 {
		t.Fatalf("visible cells = %+v", nodes[0])
	}
	for _, attr := range nodes[0].Attributes {
		if attr.Value == "hidden" || attr.Value == "and hidden" {
			t.Fatalf("withheld cell leaked: %+v", attr)
		}
	}
}

func (s *graphTableStub) Ontology() graphtable.Ontology {
	return graphtable.Ontology{Table: "graph", SchemaID: "foreman.graph", Version: 3, CellVisibility: s.cellVisibility,
		Namespaces: []graphtable.OntologyNodeKind{{Name: "entity", RowPrefix: "ent:"}},
		Edges:      []graphtable.OntologyEdge{{Name: "edge", ColumnFamily: "edge", Relationships: []string{"owns"}}}}
}

func (s *graphTableStub) Browse(_ context.Context, req graphtable.BrowseRequest) (graphtable.BrowsePage, error) {
	s.last = "browse"
	s.after = req.After
	if s.invalid {
		return graphtable.BrowsePage{}, graphtable.ErrInvalid
	}
	return graphtable.BrowsePage{Nodes: []graphtable.Node{{ID: "ent:service|api", Key: "service|api", Identifier: "api", Namespace: "entity", Kind: "service", Found: true, Attributes: []graphtable.Attribute{{Family: "service", Field: "name", Value: "API service"}}}}, Next: s.next}, nil
}

func (s *graphTableStub) Nodes(_ context.Context, ids []string) ([]graphtable.Node, error) {
	s.last, s.seeds = "nodes", ids
	return []graphtable.Node{{ID: ids[0], Key: "service|api", Namespace: "entity", Kind: "service", Found: true, Attributes: []graphtable.Attribute{{Family: "service", Field: "name", Value: "api"}}}}, nil
}

func (s *graphTableStub) Neighborhood(_ context.Context, req graphtable.NeighborhoodRequest) (graphtable.Neighborhood, error) {
	s.last, s.seeds, s.depth = "neighborhood", req.Seeds, req.Depth
	if s.invalid {
		return graphtable.Neighborhood{}, graphtable.ErrInvalid
	}
	return graphtable.Neighborhood{
		Nodes:     []graphtable.Node{{ID: req.Seeds[0], Key: "service|api", Namespace: "entity", Kind: "service", Found: true}, {ID: "ent:host|h1", Key: "host|h1", Namespace: "entity", Kind: "host"}},
		Edges:     []graphtable.Edge{{From: req.Seeds[0], To: "ent:host|h1", Family: "edge", Relationship: "owns", Weight: 1}},
		Truncated: true,
	}, nil
}

func (s *graphTableStub) Path(_ context.Context, from, to string, maxDepth, _, _ int) (graphtable.Neighborhood, bool, error) {
	s.last, s.seeds, s.depth = "path", []string{from, to}, maxDepth
	return graphtable.Neighborhood{Nodes: []graphtable.Node{{ID: from}, {ID: to}}, Edges: []graphtable.Edge{{From: from, To: to, Family: "edge"}}}, true, nil
}

func (s *graphTableStub) Terms(_ context.Context, terms []string) ([]graphtable.TermHit, error) {
	s.last, s.seeds = "terms", terms
	return []graphtable.TermHit{{Term: terms[0], NodeIDs: []string{"ent:service|api"}}}, nil
}

func (s *graphTableStub) Vector(_ context.Context, req graphtable.VectorRequest) ([]graphtable.VectorHit, error) {
	s.last = "vector"
	if req.Family != "semantic" || req.TopK != 2 || len(req.Query) != 8 {
		return nil, graphtable.ErrInvalid
	}
	return []graphtable.VectorHit{{NodeID: "ent:service|api", Score: 1}}, nil
}

func graphTableFixture(t *testing.T, operations []auth.Operation, policies [][]byte) (*Handler, *graphTableStub) {
	return graphTableFixtureCV(t, operations, policies, false)
}

func graphTableFixtureCV(t *testing.T, operations []auth.Operation, policies [][]byte, cv bool) (*Handler, *graphTableStub) {
	t.Helper()
	a := auth.NewAuthority()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: operations, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: policies, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(r *http.Request) (auth.Decision, error) {
		if r.Header.Get("Authorization") == "" {
			return auth.Decision{}, authenticationDenied()
		}
		return d, nil
	}), a.Binder(), "example.test")
	if e != nil {
		t.Fatal(e)
	}
	stub := &graphTableStub{cellVisibility: cv}
	if e := h.MountGraphTable(GraphTableSurface{Resolver: a.Resolver(), Reader: stub, Label: "graph", LabelPolicyID: []byte("label-policy"), Stores: []GraphTableStore{{Name: "embedded", Address: "embedded"}}}); e != nil {
		t.Fatal(e)
	}
	return h, stub
}

func graphTableRequest(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://example.test"+path, nil)
	} else {
		r = httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer token")
	return r
}

func TestGraphTableRoutesServeTheDescribedTable(t *testing.T) {
	h, stub := graphTableFixture(t, []auth.Operation{auth.OperationRead, auth.OperationNeighborhood}, [][]byte{[]byte("policy"), []byte("label-policy")})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodGet, "/api/v1/graph-table/", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("describe = %d %s", w.Code, w.Body)
	}
	var described GraphTableDescription
	if err := json.Unmarshal(w.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Table != "graph" || described.Label != "graph" || described.Version != 3 || len(described.Stores) != 1 || described.Bounds.MaxDepth != graphtable.MaxDepth || len(described.Ontology.Edges) != 1 {
		t.Fatalf("described = %+v", described)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/neighborhood", `{"node_ids":["ent:service|api"],"depth":2,"fanout":5}`))
	if w.Code != http.StatusOK || stub.last != "neighborhood" || stub.depth != 2 {
		t.Fatalf("neighborhood = %d %s (stub %+v)", w.Code, w.Body, stub)
	}
	var neighborhood struct {
		Table        string `json:"table"`
		Truncated    bool   `json:"truncated"`
		Neighborhood struct {
			Nodes []struct {
				ID         string   `json:"id"`
				Kind       string   `json:"kind"`
				Labels     []string `json:"labels"`
				Properties []struct{ Key, Value string }
			} `json:"nodes"`
			Edges []struct {
				ID, From, To, Type string
				Weight             float64
				Properties         []struct{ Key, Value string }
			} `json:"edges"`
		} `json:"neighborhood"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &neighborhood); err != nil {
		t.Fatal(err)
	}
	if neighborhood.Table != "graph" || !neighborhood.Truncated || len(neighborhood.Neighborhood.Nodes) != 2 || len(neighborhood.Neighborhood.Edges) != 1 {
		t.Fatalf("neighborhood body = %s", w.Body)
	}
	decoded := func(entries []struct{ Key, Value string }) map[string]string {
		out := map[string]string{}
		for _, e := range entries {
			k, _ := base64.RawURLEncoding.DecodeString(e.Key)
			v, _ := base64.RawURLEncoding.DecodeString(e.Value)
			out[string(k)] = string(v)
		}
		return out
	}
	if node := neighborhood.Neighborhood.Nodes[0]; node.Kind != "service" || len(node.Labels) != 2 || node.Labels[0] != "service|api" || node.Labels[1] != "entity" || decoded(node.Properties)["found"] != "true" {
		t.Fatalf("node = %+v", node)
	}
	if edge := neighborhood.Neighborhood.Edges[0]; edge.Type != "owns" || edge.From != "ent:service|api" || edge.To != "ent:host|h1" || edge.Weight != 1 || edge.ID == "" || decoded(edge.Properties)["relationship"] != "owns" || decoded(edge.Properties)["family"] != "edge" {
		t.Fatalf("edge = %+v", edge)
	}

	for _, row := range []struct{ path, body, last string }{
		{"/api/v1/graph-table/browse", `{"namespace":"entity","kind":"service","limit":24}`, "browse"},
		{"/api/v1/graph-table/nodes", `{"ids":["ent:service|api"]}`, "nodes"},
		{"/api/v1/graph-table/path", `{"from":"ent:service|api","to":"ent:host|h1","max_depth":3}`, "path"},
		{"/api/v1/graph-table/terms", `{"terms":["api"]}`, "terms"},
		{"/api/v1/graph-table/vector", `{"family":"semantic","query_b64":"AACAPwAAAAA=","top_k":2}`, "vector"},
	} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, graphTableRequest(http.MethodPost, row.path, row.body))
		if w.Code != http.StatusOK || stub.last != row.last {
			t.Fatalf("%s = %d %s (stub %s)", row.path, w.Code, w.Body, stub.last)
		}
	}
	if !strings.Contains(w.Body.String(), `"node_id":"ent:service|api"`) || !strings.Contains(w.Body.String(), `"score":1`) {
		t.Fatalf("vector response = %s", w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/vector", `{"family":"semantic","query_b64":"bad","top_k":2}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid vector query = %d %s", w.Code, w.Body)
	}
	if stub.depth != 3 {
		t.Fatalf("path max_depth not passed: %d", stub.depth)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/browse", `{"namespace":"entity","kind":"service","limit":24}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "API service") || strings.Contains(w.Body.String(), `"labels":["service|api"`) {
		t.Fatalf("browse display = %d %s", w.Code, w.Body)
	}

	// Capability advertised.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodGet, "/api/v1/meta", ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"graph_table":true`) {
		t.Fatalf("meta = %d %s", w.Code, w.Body)
	}
}

func TestGraphTableRoutesRefuseWhatTheyShould(t *testing.T) {
	h, stub := graphTableFixture(t, []auth.Operation{auth.OperationRead, auth.OperationNeighborhood}, [][]byte{[]byte("policy"), []byte("label-policy")})
	for _, row := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"unknown route", http.MethodPost, "/api/v1/graph-table/vectors", `{}`, http.StatusNotFound},
		{"nested path", http.MethodPost, "/api/v1/graph-table/nodes/x", `{}`, http.StatusNotFound},
		{"query string", http.MethodGet, "/api/v1/graph-table/?x=1", "", http.StatusNotFound},
		{"get on a read", http.MethodGet, "/api/v1/graph-table/neighborhood", "", http.StatusMethodNotAllowed},
		{"post on describe", http.MethodPost, "/api/v1/graph-table/", `{}`, http.StatusMethodNotAllowed},
		{"unknown field", http.MethodPost, "/api/v1/graph-table/neighborhood", `{"node_ids":["a"],"cursor":"x"}`, http.StatusBadRequest},
		{"two objects", http.MethodPost, "/api/v1/graph-table/terms", `{"terms":["a"]}{}`, http.StatusBadRequest},
		{"not json", http.MethodPost, "/api/v1/graph-table/terms", `nope`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, graphTableRequest(row.method, row.path, row.body))
		if w.Code != row.status {
			t.Fatalf("%s = %d %s, want %d", row.name, w.Code, w.Body, row.status)
		}
	}
	stub.invalid = true
	w := httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/neighborhood", `{"node_ids":["a"]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reader refusal = %d %s", w.Code, w.Body)
	}

	// No credential at all.
	w = httptest.NewRecorder()
	r := graphTableRequest(http.MethodGet, "/api/v1/graph-table/", "")
	r.Header.Del("Authorization")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d %s", w.Code, w.Body)
	}

	// A decision without the neighborhood operation may not read.
	noRead, _ := graphTableFixture(t, []auth.Operation{auth.OperationRead}, [][]byte{[]byte("label-policy")})
	w = httptest.NewRecorder()
	noRead.ServeHTTP(w, graphTableRequest(http.MethodGet, "/api/v1/graph-table/", ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no neighborhood operation = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	noRead.ServeHTTP(w, graphTableRequest(http.MethodGet, "/api/v1/meta", ""))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"graph_table":true`) {
		t.Fatalf("no-read meta = %d %s", w.Code, w.Body)
	}
	// Without the label, the operation is not even considered: the mount
	// stays concealed.
	neither, _ := graphTableFixture(t, []auth.Operation{auth.OperationRead}, [][]byte{[]byte("policy")})
	w = httptest.NewRecorder()
	neither.ServeHTTP(w, graphTableRequest(http.MethodGet, "/api/v1/graph-table/", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("neither label nor operation = %d %s, want the unmounted route's 404", w.Code, w.Body)
	}

	// A decision without the label's policy is told nothing is mounted,
	// including in the metadata capability list.
	noLabel, _ := graphTableFixture(t, []auth.Operation{auth.OperationRead, auth.OperationNeighborhood}, [][]byte{[]byte("policy")})
	for _, path := range []string{"/api/v1/graph-table/", "/api/v1/graph-table/neighborhood", "/api/v1/graph-table/browse"} {
		w = httptest.NewRecorder()
		method, body := http.MethodGet, ""
		if strings.HasSuffix(path, "neighborhood") || strings.HasSuffix(path, "browse") {
			method, body = http.MethodPost, `{"namespace":"entity","limit":24}`
		}
		noLabel.ServeHTTP(w, graphTableRequest(method, path, body))
		if w.Code != http.StatusNotFound {
			t.Fatalf("without label %s = %d %s", path, w.Code, w.Body)
		}
	}
	w = httptest.NewRecorder()
	noLabel.ServeHTTP(w, graphTableRequest(http.MethodGet, "/api/v1/meta", ""))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"graph_table":true`) {
		t.Fatalf("unlabeled meta = %d %s", w.Code, w.Body)
	}
}

func TestGraphTableVisibilityBrowseCursorConcealsRow(t *testing.T) {
	h, stub := graphTableFixtureCV(t, []auth.Operation{auth.OperationRead, auth.OperationNeighborhood}, [][]byte{[]byte("label-policy")}, true)
	stub.next = "ent:hidden|record"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/browse", `{"namespace":"entity","limit":2}`))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), stub.next) {
		t.Fatalf("unconcealed cursor = %d %s", w.Code, w.Body)
	}
	var page struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Next == "" {
		t.Fatalf("cursor response = %s, %v", w.Body, err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/browse", `{"namespace":"entity","limit":2,"after":"`+page.Next+`"}`))
	if w.Code != http.StatusOK || stub.after != stub.next {
		t.Fatalf("cursor roundtrip = %d %s, after %q", w.Code, w.Body, stub.after)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, graphTableRequest(http.MethodPost, "/api/v1/graph-table/browse", `{"namespace":"entity","limit":2,"after":"tampered"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor = %d %s", w.Code, w.Body)
	}
}

func TestGraphTableMountNeedsACompleteSurface(t *testing.T) {
	a := auth.NewAuthority()
	h, err := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(*http.Request) (auth.Decision, error) { return auth.Decision{}, authenticationDenied() }), a.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	for name, surface := range map[string]GraphTableSurface{
		"no reader":   {Resolver: a.Resolver(), Label: "graph", LabelPolicyID: []byte("p")},
		"no label":    {Resolver: a.Resolver(), Reader: &graphTableStub{}, LabelPolicyID: []byte("p")},
		"no policy":   {Resolver: a.Resolver(), Reader: &graphTableStub{}, Label: "graph"},
		"no resolver": {Reader: &graphTableStub{}, Label: "graph", LabelPolicyID: []byte("p")},
	} {
		if err := h.MountGraphTable(surface); err == nil {
			t.Fatalf("%s mounted", name)
		}
	}
	if h.decisionCapabilities.GraphTable {
		t.Fatal("capability advertised without a mount")
	}
}
