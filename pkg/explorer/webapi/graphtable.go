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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/graphtable"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// GraphTableRoute is the collection root of the described-table graph
// routes: a graph laid out in any engine table and read through the
// table's recorded descriptor rather than the workspace's own records.
const GraphTableRoute = "/api/v1/graph-table"

// MaxGraphTableRequestBytes bounds one request body.
const MaxGraphTableRequestBytes = 64 << 10

// GraphTableReader is what the routes need from a described table.
type GraphTableReader interface {
	Ontology() graphtable.Ontology
	Browse(context.Context, graphtable.BrowseRequest) (graphtable.BrowsePage, error)
	Nodes(context.Context, []string) ([]graphtable.Node, error)
	Neighborhood(context.Context, graphtable.NeighborhoodRequest) (graphtable.Neighborhood, error)
	Path(ctx context.Context, from, to string, maxDepth, fanout, maxNodes int) (graphtable.Neighborhood, bool, error)
	Terms(context.Context, []string) ([]graphtable.TermHit, error)
}

// GraphTableStore describes one process the table's rows are read from.
type GraphTableStore struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Start   string `json:"start,omitempty"`
	End     string `json:"end,omitempty"`
}

// GraphTableSurface is the host composition of the described-table routes.
// A described table is trusted ingest exposed under one label: a decision
// that does not hold the label's policy is told no table is mounted.
type GraphTableSurface struct {
	Resolver auth.Resolver
	Reader   GraphTableReader
	// Label is the one label every cell is exposed under and LabelPolicyID
	// the policy identity a decision must hold to see it.
	Label         string
	LabelPolicyID []byte
	// Stores is where the rows are served from, reported to the UI.
	Stores []GraphTableStore
}

func (s GraphTableSurface) validate() error {
	if isAbsentInterface(s.Resolver) || isAbsentInterface(s.Reader) || s.Label == "" || len(s.LabelPolicyID) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "complete graph table surface, label, and trusted resolver required")
	}
	return nil
}

// MountGraphTable mounts the described-table routes behind the host's
// authentication gate and advertises the graph_table capability only to
// decisions that hold the label and may read neighborhoods.
func (h *Handler) MountGraphTable(surface GraphTableSurface) error {
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "graph table surface requires authenticated transport")
	}
	if err := surface.validate(); err != nil {
		return err
	}
	handler := &graphTableHTTP{surface: surface}
	if err := h.MountAuthenticated(GraphTableRoute+"/", handler); err != nil {
		return err
	}
	h.decisionCapabilities.GraphTable = true
	h.graphTable = handler
	return nil
}

type graphTableHTTP struct{ surface GraphTableSurface }

func (g *graphTableHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	decision, err := g.resolve(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	// The label gate comes first and conceals the mount, as an
	// unregistered node is concealed by the workspace: a reader without
	// the label gets the response an unmounted route gives, whatever else
	// the decision allows.
	if !graphTableLabelHeld(decision, g.surface.LabelPolicyID) {
		writeError(w, shoal.NewError(shoal.ErrorNotFound, "no such graph table route"))
		return
	}
	if !graphTableReadAllowed(decision) {
		writeError(w, shoal.NewError(shoal.ErrorUnauthorized, "graph reads are not permitted"))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, GraphTableRoute+"/")
	if r.URL.RawQuery != "" || strings.Contains(rest, "/") {
		writeError(w, shoal.NewError(shoal.ErrorNotFound, "no such graph table route"))
		return
	}
	methodNotAllowed := func(allow string) {
		w.Header().Set("Allow", allow)
		writeResponse(w, http.StatusMethodNotAllowed, struct {
			Code    shoal.ErrorCode `json:"code"`
			Message string          `json:"message"`
		}{shoal.ErrorInvalidArgument, "method not allowed"})
	}
	switch {
	case rest == "":
		if r.Method != http.MethodGet {
			methodNotAllowed("GET")
			return
		}
		writeResponse(w, http.StatusOK, g.describe())
	case rest == "browse" || rest == "nodes" || rest == "neighborhood" || rest == "path" || rest == "terms":
		if r.Method != http.MethodPost {
			methodNotAllowed("POST")
			return
		}
		body, err := readGraphTableBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		value, err := g.dispatch(r.Context(), rest, body)
		if err != nil {
			writeError(w, graphTableError(err))
			return
		}
		writeResponse(w, http.StatusOK, value)
	default:
		writeError(w, shoal.NewError(shoal.ErrorNotFound, "no such graph table route"))
	}
}

func (g *graphTableHTTP) resolve(ctx context.Context) (auth.Decision, error) {
	if ctx.Err() != nil {
		return auth.Decision{}, authenticationDenied()
	}
	d, err := g.surface.Resolver.Resolve(ctx)
	if err != nil || ctx.Err() != nil || !time.Now().Before(d.AuthenticationExpires()) {
		return auth.Decision{}, authenticationDenied()
	}
	return d, nil
}

func graphTableReadAllowed(d auth.Decision) bool {
	for _, op := range d.AllowedOperations() {
		if op == auth.OperationNeighborhood {
			return true
		}
	}
	return false
}

func graphTableLabelHeld(d auth.Decision, policyID []byte) bool {
	for _, held := range d.PermittedPolicyIDs() {
		if bytes.Equal(held, policyID) {
			return true
		}
	}
	return false
}

func readGraphTableBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "graph table request body is required")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxGraphTableRequestBytes+1))
	if err != nil || len(raw) > MaxGraphTableRequestBytes || !utf8.Valid(raw) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "graph table request body size or encoding")
	}
	return raw, nil
}

func decodeGraphTableBody(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "graph table request: "+err.Error())
	}
	if decoder.More() {
		return shoal.NewError(shoal.ErrorInvalidArgument, "graph table request: one JSON object expected")
	}
	return nil
}

func graphTableError(err error) error {
	if errors.Is(err, graphtable.ErrInvalid) {
		return shoal.NewError(shoal.ErrorInvalidArgument, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return shoal.NewError(shoal.ErrorCanceled, "graph table request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return shoal.NewError(shoal.ErrorDeadline, "graph table request deadline exceeded")
	}
	var typed *shoal.Error
	if errors.As(err, &typed) {
		return err
	}
	return shoal.NewError(shoal.ErrorUnavailable, "graph table store: "+err.Error())
}

// GraphTableDescription is the GET response: what is mounted and the
// bounds a request may ask for.
type GraphTableDescription struct {
	Table    string              `json:"table"`
	SchemaID string              `json:"schema_id"`
	Version  int32               `json:"version"`
	Label    string              `json:"label"`
	Stores   []GraphTableStore   `json:"stores"`
	Ontology graphtable.Ontology `json:"ontology"`
	Bounds   GraphTableBounds    `json:"bounds"`
}

// GraphTableBounds are the request limits the reader enforces.
type GraphTableBounds struct {
	MaxDepth  int `json:"max_depth"`
	MaxFanout int `json:"max_fanout"`
	MaxNodes  int `json:"max_nodes"`
	MaxSeeds  int `json:"max_seeds"`
	MaxTerms  int `json:"max_terms"`
}

func (g *graphTableHTTP) describe() GraphTableDescription {
	ontology := g.surface.Reader.Ontology()
	stores := g.surface.Stores
	if stores == nil {
		stores = []GraphTableStore{}
	}
	return GraphTableDescription{
		Table: ontology.Table, SchemaID: ontology.SchemaID, Version: ontology.Version,
		Label: g.surface.Label, Stores: stores, Ontology: ontology,
		Bounds: GraphTableBounds{MaxDepth: graphtable.MaxDepth, MaxFanout: graphtable.MaxFanout, MaxNodes: graphtable.MaxNodes, MaxSeeds: graphtable.MaxSeeds, MaxTerms: graphtable.MaxTerms},
	}
}

type graphTableNodesRequest struct {
	IDs []string `json:"ids"`
}

type graphTableBrowseRequest struct {
	Namespace        string `json:"namespace"`
	Kind             string `json:"kind"`
	IdentifierPrefix string `json:"identifier_prefix"`
	After            string `json:"after"`
	Limit            int    `json:"limit"`
}

type wireGraphTableBrowse struct {
	Table string     `json:"table"`
	Nodes []wireNode `json:"nodes"`
	Next  string     `json:"next,omitempty"`
}

type graphTableNeighborhoodRequest struct {
	NodeIDs       []string `json:"node_ids"`
	Depth         int      `json:"depth"`
	Fanout        int      `json:"fanout"`
	MaxNodes      int      `json:"max_nodes"`
	Families      []string `json:"families"`
	Relationships []string `json:"relationships"`
}

type graphTablePathRequest struct {
	From     string `json:"from"`
	To       string `json:"to"`
	MaxDepth int    `json:"max_depth"`
	Fanout   int    `json:"fanout"`
	MaxNodes int    `json:"max_nodes"`
}

type graphTableTermsRequest struct {
	Terms []string `json:"terms"`
}

type wireGraphTableNeighborhood struct {
	Table        string           `json:"table"`
	Neighborhood wireNeighborhood `json:"neighborhood"`
	Truncated    bool             `json:"truncated"`
	DroppedEdges int              `json:"dropped_edges"`
}

type wireGraphTablePath struct {
	Table string   `json:"table"`
	Found bool     `json:"found"`
	Path  wirePath `json:"path"`
}

type wireGraphTableNodes struct {
	Table string     `json:"table"`
	Nodes []wireNode `json:"nodes"`
}

type wireGraphTableTerms struct {
	Table string               `json:"table"`
	Hits  []graphtable.TermHit `json:"hits"`
}

func (g *graphTableHTTP) dispatch(ctx context.Context, route string, raw []byte) (any, error) {
	table := g.surface.Reader.Ontology().Table
	switch route {
	case "browse":
		var req graphTableBrowseRequest
		if err := decodeGraphTableBody(raw, &req); err != nil {
			return nil, err
		}
		page, err := g.surface.Reader.Browse(ctx, graphtable.BrowseRequest{Namespace: req.Namespace, Kind: req.Kind, IdentifierPrefix: req.IdentifierPrefix, After: req.After, Limit: req.Limit})
		if err != nil {
			return nil, err
		}
		items := make([]wireNode, 0, len(page.Nodes))
		for _, node := range page.Nodes {
			items = append(items, wireNode{ID: node.ID, Kind: node.Kind, Labels: []string{graphTableDisplayName(node), node.Namespace}})
		}
		return wireGraphTableBrowse{Table: table, Nodes: items, Next: page.Next}, nil
	case "nodes":
		var req graphTableNodesRequest
		if err := decodeGraphTableBody(raw, &req); err != nil {
			return nil, err
		}
		nodes, err := g.surface.Reader.Nodes(ctx, req.IDs)
		if err != nil {
			return nil, err
		}
		return wireGraphTableNodes{Table: table, Nodes: wireGraphTableNodeList(nodes)}, nil
	case "neighborhood":
		var req graphTableNeighborhoodRequest
		if err := decodeGraphTableBody(raw, &req); err != nil {
			return nil, err
		}
		got, err := g.surface.Reader.Neighborhood(ctx, graphtable.NeighborhoodRequest{
			Seeds: req.NodeIDs, Depth: req.Depth, Fanout: req.Fanout, MaxNodes: req.MaxNodes,
			Families: req.Families, Relationships: req.Relationships,
		})
		if err != nil {
			return nil, err
		}
		return wireGraphTableNeighborhood{Table: table, Neighborhood: wireNeighborhood{Nodes: wireGraphTableNodeList(got.Nodes), Edges: wireGraphTableEdgeList(got.Edges)}, Truncated: got.Truncated, DroppedEdges: got.DroppedEdges}, nil
	case "path":
		var req graphTablePathRequest
		if err := decodeGraphTableBody(raw, &req); err != nil {
			return nil, err
		}
		got, found, err := g.surface.Reader.Path(ctx, req.From, req.To, req.MaxDepth, req.Fanout, req.MaxNodes)
		if err != nil {
			return nil, err
		}
		return wireGraphTablePath{Table: table, Found: found, Path: wirePath{Nodes: wireGraphTableNodeList(got.Nodes), Edges: wireGraphTableEdgeList(got.Edges)}}, nil
	case "terms":
		var req graphTableTermsRequest
		if err := decodeGraphTableBody(raw, &req); err != nil {
			return nil, err
		}
		hits, err := g.surface.Reader.Terms(ctx, req.Terms)
		if err != nil {
			return nil, err
		}
		if hits == nil {
			hits = []graphtable.TermHit{}
		}
		return wireGraphTableTerms{Table: table, Hits: hits}, nil
	}
	return nil, shoal.NewError(shoal.ErrorNotFound, "no such graph table route")
}

// wireGraphTableNodeList renders nodes in the shape the workspace graph
// uses: the key within the namespace and the namespace as labels, the kind
// as the kind, the attributes as properties. Properties are encoded as
// every workspace node's and edge's are (wireMetadataValue), so the UI's
// one decoder reads them.
func wireGraphTableNodeList(nodes []graphtable.Node) []wireNode {
	out := make([]wireNode, 0, len(nodes))
	for _, n := range nodes {
		label := graphTableDisplayName(n)
		node := wireNode{ID: n.ID, Kind: n.Kind, Labels: []string{label, n.Namespace}}
		properties := shoal.Metadata{"found": strconv.FormatBool(n.Found)}
		for _, a := range n.Attributes {
			key := a.Family
			if a.Field != "" {
				key = a.Family + "/" + a.Field
			}
			properties[key] = a.Value
		}
		node.Properties = wireMetadataValue(properties)
		out = append(out, node)
	}
	return out
}

func graphTableDisplayName(n graphtable.Node) string {
	for _, wanted := range []string{"name", "display_name", "title", "summary", "subject"} {
		for _, a := range n.Attributes {
			if strings.EqualFold(a.Field, wanted) && strings.TrimSpace(a.Value) != "" {
				value := strings.TrimSpace(strings.SplitN(a.Value, "\n", 2)[0])
				if len([]rune(value)) > 100 {
					value = string([]rune(value)[:100]) + "…"
				}
				return value
			}
		}
	}
	if n.Identifier != "" {
		return n.Identifier
	}
	if n.Key != "" {
		return n.Key
	}
	return n.Kind
}

func wireGraphTableEdgeList(edges []graphtable.Edge) []wireEdge {
	out := make([]wireEdge, 0, len(edges))
	for _, e := range edges {
		kind := e.Family
		if e.Relationship != "" {
			kind = e.Relationship
		}
		edge := wireEdge{ID: e.ID(), From: e.From, To: e.To, Type: kind, Weight: shoal.Score(e.Weight)}
		properties := shoal.Metadata{"family": e.Family}
		if e.Relationship != "" {
			properties["relationship"] = e.Relationship
		}
		if e.Weighted {
			properties["weighted"] = "true"
		}
		edge.Properties = wireMetadataValue(properties)
		out = append(out, edge)
	}
	return out
}
