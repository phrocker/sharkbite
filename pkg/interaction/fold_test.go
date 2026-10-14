/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package interaction_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func labelResolver(labels map[shoal.ID][]string) interaction.VisibilityResolver {
	return func(id shoal.ID) ([]string, error) {
		set, ok := labels[id]
		if !ok {
			return nil, shoal.NewError(shoal.ErrorUnavailable, "unknown node")
		}
		return set, nil
	}
}

func foldFixture() interaction.Fold {
	return interaction.Fold{
		Members: []interaction.FoldMember{
			{
				SessionID:        "interaction.session_b",
				RetrievedNodeIDs: []shoal.ID{"span-2", "span-3"},
				CitedNodeIDs:     []shoal.ID{"span-3"},
				Visibility:       []string{"ops"},
			},
			{
				SessionID:        "interaction.session_a",
				RetrievedNodeIDs: []shoal.ID{"span-1", "span-2"},
				CitedNodeIDs:     []shoal.ID{"span-1"},
				Visibility:       []string{"secret"},
			},
		},
		SummaryDigest: interaction.Digest("the retry budget was exhausted"),
		FoldedAt:      time.Unix(1700000500, 0).UTC(),
	}
}

// TestFoldIdentityIsContentAddressed pins requirement 4: the same folded input
// always mints the same vertex, regardless of member or node ordering, and a
// different input mints a different one.
func TestFoldIdentityIsContentAddressed(t *testing.T) {
	fold := foldFixture()
	first, err := fold.ID()
	if err != nil {
		t.Fatal(err)
	}

	// Same input, members and node IDs presented in a different order, and a
	// different fold time.
	shuffled := interaction.Fold{
		Members: []interaction.FoldMember{
			{
				SessionID:        "interaction.session_a",
				RetrievedNodeIDs: []shoal.ID{"span-2", "span-1", "span-1"},
				CitedNodeIDs:     []shoal.ID{"span-1"},
				Visibility:       []string{"secret"},
			},
			{
				SessionID:        "interaction.session_b",
				RetrievedNodeIDs: []shoal.ID{"span-3", "span-2"},
				CitedNodeIDs:     []shoal.ID{"span-3"},
				Visibility:       []string{"ops"},
			},
		},
		SummaryDigest: fold.SummaryDigest,
		FoldedAt:      time.Unix(1799999999, 0).UTC(),
	}
	second, err := shuffled.ID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("fold identity is not content addressed: %q != %q", first, second)
	}
	if !strings.HasPrefix(string(first), interaction.KindFold+"_") {
		t.Fatalf("fold identity %q is outside the reserved namespace", first)
	}

	// A different summary digest is a different fold.
	renamed := foldFixture()
	renamed.SummaryDigest = interaction.Digest("a different summary")
	other, err := renamed.ID()
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("folds with different summaries collided")
	}

	// Dropping a cited node is a different fold, because the cited set is
	// part of the folded provenance.
	narrowed := foldFixture()
	narrowed.Members[0].CitedNodeIDs = nil
	third, err := narrowed.ID()
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("folds with different cited sets collided")
	}
}

func TestFoldRetainsAndConjoinsExactSourceEdges(t *testing.T) {
	fold := interaction.Fold{
		Members: []interaction.FoldMember{{
			SessionID:        interaction.DerivedID("session", "edge-fold"),
			RetrievedNodeIDs: []shoal.ID{"source-a", "source-b"},
			TouchedEdgeIDs:   []shoal.ID{"source-edge"},
		}},
		FoldedAt: time.Unix(1700000000, 0).UTC(),
	}
	if _, err := fold.Subgraph(func(shoal.ID) ([]string, error) {
		return nil, nil
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("fold without edge resolver = %v", err)
	}
	subgraph, err := fold.SubgraphWithEvidence(
		func(shoal.ID) ([]string, error) { return []string{"node-label"}, nil },
		func(id shoal.ID) ([]string, error) {
			if id != "source-edge" {
				t.Fatalf("resolved edge = %q", id)
			}
			return []string{"edge-label"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := interaction.Expression(subgraph.Visibility); got != "edge-label&node-label" {
		t.Fatalf("fold visibility = %q", got)
	}
	if !reflect.DeepEqual(subgraph.TouchedEdgeIDs, []shoal.ID{"source-edge"}) {
		t.Fatalf("fold subgraph edges = %v", subgraph.TouchedEdgeIDs)
	}
	canonical, err := fold.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.Members[0].TouchedEdgeIDs) != 1 ||
		canonical.Members[0].TouchedEdgeIDs[0] != "source-edge" {
		t.Fatalf("canonical fold lost touched edges: %+v", canonical.Members[0])
	}
	withEdgesID, err := canonical.ID()
	if err != nil {
		t.Fatal(err)
	}
	canonical.Members[0].TouchedEdgeIDs = nil
	legacyID, err := canonical.ID()
	if err != nil {
		t.Fatal(err)
	}
	if withEdgesID == legacyID {
		t.Fatal("edge-bearing fold identity omitted exact source edges")
	}
}

func TestFoldEdgeIdentityEncodingSeparatesOpaqueListValues(t *testing.T) {
	sessionID := interaction.DerivedID("session", "opaque-edge-boundary")
	summary := interaction.Digest("opaque edge boundary")
	legacy := interaction.Fold{
		Members: []interaction.FoldMember{{
			SessionID:    sessionID,
			CitedNodeIDs: []shoal.ID{"edges", "z"},
		}},
		SummaryDigest: summary,
		FoldedAt:      time.Unix(1700000000, 0).UTC(),
	}
	withEdges := interaction.Fold{
		Members: []interaction.FoldMember{{
			SessionID:      sessionID,
			TouchedEdgeIDs: []shoal.ID{"z"},
		}},
		SummaryDigest: summary,
		FoldedAt:      legacy.FoldedAt,
	}
	legacyID, err := legacy.ID()
	if err != nil {
		t.Fatal(err)
	}
	edgeID, err := withEdges.ID()
	if err != nil {
		t.Fatal(err)
	}
	if legacyID == edgeID {
		t.Fatal("opaque cited IDs collided with typed edge provenance")
	}
}

// TestFoldVisibilityIsConjunctionOfEverythingFolded pins requirement 2:
// summarizing never widens visibility. The fold requires every label of every
// folded session and every label of every source node those sessions touched.
func TestFoldVisibilityIsConjunctionOfEverythingFolded(t *testing.T) {
	fold := foldFixture()
	subgraph, err := fold.Subgraph(labelResolver(map[shoal.ID][]string{
		"span-1": {"incident"},
		"span-2": nil,
		"span-3": {"ops", "pii"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := interaction.Expression(subgraph.Visibility)
	const want = "incident&ops&pii&secret"
	if got != want {
		t.Fatalf("fold visibility = %q, want %q", got, want)
	}
	if property := subgraph.Nodes[0].Properties[interaction.PropertyVisibility]; property != want {
		t.Fatalf("fold node visibility = %q, want %q", property, want)
	}
	// Every member's own visibility must be implied by the fold's.
	for _, member := range fold.Members {
		for _, label := range member.Visibility {
			if !strings.Contains(got, label) {
				t.Fatalf("fold visibility %q dropped member label %q", got, label)
			}
		}
	}
}

// TestFoldFailsClosedOnUnresolvableNode pins that an unresolvable touched node
// fails the whole fold rather than producing an understated visibility.
func TestFoldFailsClosedOnUnresolvableNode(t *testing.T) {
	fold := foldFixture()
	_, err := fold.Subgraph(labelResolver(map[shoal.ID][]string{
		"span-1": {"incident"},
		"span-2": nil,
	}))
	if err == nil {
		t.Fatal("expected a fold over an unresolvable node to fail")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

// TestFoldSubgraphKeepsRetrievedAndCitedDistinct pins requirement 3 at the
// graph level: the materialized fold still distinguishes what was shown from
// what was cited, and TouchedNodes recovers both sets exactly.
func TestFoldSubgraphKeepsRetrievedAndCitedDistinct(t *testing.T) {
	fold := foldFixture()
	subgraph, err := fold.Subgraph(labelResolver(map[shoal.ID][]string{
		"span-1": nil, "span-2": nil, "span-3": nil,
	}))
	if err != nil {
		t.Fatal(err)
	}
	touched := interaction.TouchedNodes(subgraph.Nodes, subgraph.Edges)
	assertIDs(t, "retrieved", touched.RetrievedNodeIDs,
		[]shoal.ID{"span-1", "span-2", "span-3"})
	assertIDs(t, "cited", touched.CitedNodeIDs, []shoal.ID{"span-1", "span-3"})

	// span-2 was shown to both sessions and cited by neither. Losing that
	// distinction is what would make the visibility conjunction unsound.
	for _, id := range touched.CitedNodeIDs {
		if id == "span-2" {
			t.Fatal("a retrieved-only node was reported as cited")
		}
	}
	var folds int
	for _, edge := range subgraph.Edges {
		if edge.Type == interaction.EdgeFolds {
			folds++
		}
	}
	if folds != len(fold.Members) {
		t.Fatalf("fold has %d membership edges, want %d", folds, len(fold.Members))
	}
}

// TestTouchedNodesIgnoresSubgraphInternalEdges pins that structural edges
// inside an interaction record are never mistaken for source provenance.
func TestTouchedNodesIgnoresSubgraphInternalEdges(t *testing.T) {
	session := interaction.Session{
		ID:          interaction.DerivedID("session", "internal"),
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		SeedNodeIDs: []shoal.ID{"span-1"},
		Turns: []interaction.Turn{{
			Index:    0,
			Decision: "retrieve",
			ToolCall: &interaction.ToolCall{
				Kind:             "retrieve",
				RetrievedNodeIDs: []shoal.ID{"span-2"},
			},
		}},
		CitedNodeIDs: []shoal.ID{"span-2"},
	}
	subgraph, err := session.Subgraph(labelResolver(map[shoal.ID][]string{
		"span-1": nil, "span-2": nil,
	}))
	if err != nil {
		t.Fatal(err)
	}
	touched := interaction.TouchedNodes(subgraph.Nodes, subgraph.Edges)
	assertIDs(t, "retrieved", touched.RetrievedNodeIDs,
		[]shoal.ID{"span-1", "span-2"})
	assertIDs(t, "cited", touched.CitedNodeIDs, []shoal.ID{"span-2"})
}

// TestFoldRejectsInteractionNodeAsSourceEvidence pins requirement 1 in the
// contract package: a fold cannot claim a derived node as source evidence.
func TestFoldRejectsInteractionNodeAsSourceEvidence(t *testing.T) {
	fold := foldFixture()
	fold.Members[0].CitedNodeIDs = append(
		fold.Members[0].CitedNodeIDs,
		interaction.DerivedID("session", "an-earlier-session"),
	)
	if _, err := fold.ID(); err == nil {
		t.Fatal("expected a fold citing an interaction node to be refused")
	}
	if _, err := fold.Subgraph(labelResolver(nil)); err == nil {
		t.Fatal("expected a fold citing an interaction node to be refused")
	}
}

// TestFoldRejectsNonDigestSummary pins the redaction discipline: the summary
// reference must be a SHA-256 digest, so the field cannot be used to smuggle
// prompt, answer, or evidence text into a node payload.
func TestFoldRejectsNonDigestSummary(t *testing.T) {
	for _, summary := range []string{
		"the retry budget was exhausted during the outage",
		"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789",
		"zz00000000000000000000000000000000000000000000000000000000000000",
		"abc123",
	} {
		fold := foldFixture()
		fold.SummaryDigest = summary
		if err := fold.Validate(); err == nil {
			t.Fatalf("summary %q was accepted", summary)
		}
	}
	fold := foldFixture()
	fold.SummaryDigest = ""
	if err := fold.Validate(); err != nil {
		t.Fatalf("a structural fold with no summary must be valid: %v", err)
	}
}

// TestFoldNodeCarriesNoText pins that a fold node's payload is identities,
// digests, counts, and labels only.
func TestFoldNodeCarriesNoText(t *testing.T) {
	fold := foldFixture()
	subgraph, err := fold.Subgraph(labelResolver(map[shoal.ID][]string{
		"span-1": {"incident"}, "span-2": nil, "span-3": {"ops"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	node := subgraph.Nodes[0]
	if node.Kind != interaction.KindFold {
		t.Fatalf("fold node kind = %q", node.Kind)
	}
	allowed := map[string]struct{}{
		interaction.PropertyFoldedAt:      {},
		interaction.PropertyFoldedCount:   {},
		interaction.PropertyRetrieved:     {},
		interaction.PropertyCited:         {},
		interaction.PropertySummaryDigest: {},
		interaction.PropertyVisibility:    {},
	}
	for key := range node.Properties {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("fold node carries unexpected property %q", key)
		}
	}
	if node.Properties[interaction.PropertySummaryDigest] != fold.SummaryDigest {
		t.Fatal("fold node lost its summary digest")
	}
}

// TestFoldRequiresAtLeastOneMember pins the static bound.
func TestFoldRequiresAtLeastOneMember(t *testing.T) {
	if err := (interaction.Fold{FoldedAt: time.Now()}).Validate(); err == nil {
		t.Fatal("expected an empty fold to be refused")
	}
	duplicated := interaction.Fold{
		Members: []interaction.FoldMember{
			{SessionID: "interaction.session_a"}, {SessionID: "interaction.session_a"},
		},
		FoldedAt: time.Now(),
	}
	if _, err := duplicated.Canonical(); err == nil {
		t.Fatal("expected a fold with a duplicate member to be refused")
	}
}

func assertIDs(t *testing.T, name string, got, want []shoal.ID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
	for index := range got {
		if got[index] != want[index] {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}
