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

package contextpack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestVerifyResultReturnsCompleteEvidenceAndRejectsSnapshotDrift(
	t *testing.T,
) {
	client, request, response, pins := embeddedFixture(t)
	builder := Builder{Reader: client}
	pack, err := builder.Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceIDs := make([]shoal.ID, 0, len(pack.Evidence()))
	for _, anchor := range pack.Evidence() {
		evidenceIDs = append(evidenceIDs, anchor.ID())
	}
	issue, err := inference.NewIssue(
		inference.IssueUnsupported, "input", "reason", evidenceIDs)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewInferenceResult(
		pack, nil, []inference.Issue{issue},
		pack.Snapshot().AsOf().Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &verificationSnapshotReader{
		Explorer: client,
		snapshot: explorer.Snapshot{
			ID: string(pack.Snapshot().ID()), AsOf: pack.Snapshot().AsOf(),
		},
		authorization: pack.Authorization(),
	}
	verified, err := (Builder{Reader: reader}).VerifyResult(
		context.Background(), pack, result)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Anchors()) != len(pack.Evidence()) {
		t.Fatalf(
			"verified anchors = %d, want %d",
			len(verified.Anchors()), len(pack.Evidence()),
		)
	}
	for _, anchor := range verified.Anchors() {
		reference, err := anchor.EvidenceReference()
		if err != nil {
			t.Fatal(err)
		}
		if reference.AnchorID != anchor.Anchor().ID() ||
			len(reference.NodeIDs) == 0 {
			t.Fatalf("evidence reference = %+v", reference)
		}
	}
	otherAuthorization, err := inference.NewAuthPin(
		"different-authorization",
		pack.Authorization().ExpiresAt(),
	)
	if err != nil {
		t.Fatal(err)
	}
	reader.authorization = otherAuthorization
	if _, err := (Builder{Reader: reader}).VerifyResult(
		context.Background(), pack, result,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("authorization drift verification error = %v", err)
	}
	reader.authorization = pack.Authorization()
	if _, err := client.Ingest(context.Background(), explorer.Source{
		URI: "memory://snapshot-drift", MediaType: explorer.MediaTypeText,
		Content: "later publication",
	}); err != nil {
		t.Fatal(err)
	}
	reader.snapshot, err = client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Builder{Reader: reader}).VerifyResult(
		context.Background(), pack, result,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("snapshot drift verification error = %v", err)
	}
	inFlight := &verificationSnapshotReader{
		Explorer: client,
		snapshots: []explorer.Snapshot{{
			ID: string(pack.Snapshot().ID()), AsOf: pack.Snapshot().AsOf(),
		}, reader.snapshot},
		authorization: pack.Authorization(),
	}
	if _, err := (Builder{Reader: inFlight}).VerifyResult(
		context.Background(), pack, result,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("in-flight snapshot drift verification error = %v", err)
	}
}

func TestVerifyResultProjectsGraphAdditionAssertionsAndVisibility(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	pack, err := (Builder{Reader: client}).Build(
		context.Background(), InitialRequest{
			Request: request, Response: response, Pins: pins,
			Selection: EvidenceSelection{Documents: true},
		})
	if err != nil {
		t.Fatal(err)
	}
	citation := response.Results[0].Evidence[0].Citation
	quote := response.Results[0].Evidence[0].Quote
	evidence, err := ontology.NewEvidenceRef(citation, quote, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"graph-node", "Graph Node", "A graph node", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"supports", "Supports", "Supports another graph node",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue("graph-target")
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"graph-source", relationship.ID(), value, ontology.AssertionInferred, 0.9,
		[]ontology.EvidenceRef{evidence}, provenance,
		shoal.Metadata{"shoal.graph.edge_id": "graph-edge"},
	)
	if err != nil {
		t.Fatal(err)
	}
	path := graph.Path{
		Nodes: []graph.Node{
			{ID: "graph-source"},
			{ID: "graph-target"},
		},
		Edges: []graph.Edge{{
			ID: "graph-edge", From: "graph-source", To: "graph-target",
			Type: string(relationship.ID()), Weight: 0.9,
			Properties: shoal.Metadata{
				interaction.PropertyVisibility: "restricted",
			},
		}},
	}
	reference := interaction.AssertionReference{
		AssertionID: assertion.ID(),
		EdgeID:      "graph-edge",
		Origin:      ontology.AssertionInferred,
	}
	addition, err := inference.NewGraphAnchorWithAssertions(
		path, []interaction.AssertionReference{reference})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := inference.NewIssue(
		inference.IssueUnsupported, "input", "reason",
		[]shoal.ID{addition.ID()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewExtendedInferenceResult(
		pack, []inference.EvidenceAnchor{addition}, nil,
		[]inference.Issue{issue}, pack.Snapshot().AsOf().Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &verificationGraphReader{
		verificationSnapshotReader: verificationSnapshotReader{
			Explorer: client,
			snapshot: explorer.Snapshot{
				ID: string(pack.Snapshot().ID()), AsOf: pack.Snapshot().AsOf(),
			},
			authorization: pack.Authorization(),
		},
		neighborhood: explorer.Neighborhood{
			Nodes: path.Nodes, Edges: path.Edges,
			Assertions: []ontology.Assertion{assertion},
		},
	}
	verified, err := (Builder{Reader: reader}).VerifyResult(
		context.Background(), pack, result)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, anchor := range verified.Anchors() {
		if anchor.Anchor().ID() != addition.ID() {
			continue
		}
		found = true
		if !anchor.Addition() ||
			len(anchor.Assertions()) != 1 ||
			anchor.Assertions()[0].AssertionID() != assertion.ID() ||
			anchor.Assertions()[0].EdgeID() != "graph-edge" ||
			anchor.Assertions()[0].Origin() != ontology.AssertionInferred ||
			!containsString(anchor.Visibility(), "restricted") {
			t.Fatalf("verified graph addition = %+v", anchor)
		}
		projected, err := anchor.EvidenceReference()
		if err != nil {
			t.Fatal(err)
		}
		if len(projected.EdgeIDs) != 1 ||
			projected.EdgeIDs[0] != "graph-edge" ||
			len(projected.Assertions) != 1 ||
			projected.Assertions[0] != reference {
			t.Fatalf("projected graph evidence = %+v", projected)
		}
	}
	if !found {
		t.Fatal("verified result omitted graph evidence addition")
	}

	digestOnlyPath := path
	digestOnlyPath.Edges = append([]graph.Edge(nil), path.Edges...)
	digestOnlyPath.Edges[0].Properties = shoal.Metadata{
		interaction.PropertyVisibilityDigest: strings.Repeat("0", 64),
		interaction.PropertyVisibilityCount:  "1",
	}
	digestOnly, err := inference.NewGraphAnchorWithAssertions(
		digestOnlyPath, []interaction.AssertionReference{reference})
	if err != nil {
		t.Fatal(err)
	}
	digestIssue, err := inference.NewIssue(
		inference.IssueUnsupported, "input", "reason",
		[]shoal.ID{digestOnly.ID()})
	if err != nil {
		t.Fatal(err)
	}
	digestResult, err := inference.NewExtendedInferenceResult(
		pack, []inference.EvidenceAnchor{digestOnly}, nil,
		[]inference.Issue{digestIssue},
		pack.Snapshot().AsOf().Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader.neighborhood.Edges = digestOnlyPath.Edges
	if _, err := (Builder{Reader: reader}).VerifyResult(
		context.Background(), pack, digestResult,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("digest-only edge visibility error = %v", err)
	}
}

func TestVerifiedDocumentReferenceRetainsResolvedSourceRoles(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	pack, err := (Builder{Reader: client}).Build(
		context.Background(), InitialRequest{
			Request: request, Response: response, Pins: pins,
			Selection: EvidenceSelection{Documents: true},
		})
	if err != nil {
		t.Fatal(err)
	}
	citation := response.Results[0].Evidence[0].Citation
	quote := response.Results[0].Evidence[0].Quote
	resolvedSectionID := citation.SectionID
	resolvedSpanID := citation.SpanID
	citation.SpanID = ""
	addition, err := inference.NewDocumentAnchor(citation, quote)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := inference.NewIssue(
		inference.IssueUnsupported, "input", "reason",
		[]shoal.ID{addition.ID()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewExtendedInferenceResult(
		pack, []inference.EvidenceAnchor{addition}, nil,
		[]inference.Issue{issue}, pack.Snapshot().AsOf().Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &verificationSnapshotReader{
		Explorer: client,
		snapshot: explorer.Snapshot{
			ID: string(pack.Snapshot().ID()), AsOf: pack.Snapshot().AsOf(),
		},
		authorization: pack.Authorization(),
	}
	verified, err := (Builder{Reader: reader}).VerifyResult(
		context.Background(), pack, result)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range verified.Anchors() {
		if anchor.Anchor().ID() != addition.ID() {
			continue
		}
		reference, err := anchor.EvidenceReference()
		if err != nil {
			t.Fatal(err)
		}
		if reference.Citation.SpanID != "" ||
			len(reference.NodeIDs) != 3 ||
			!testContainsID(reference.NodeIDs, resolvedSectionID) ||
			!testContainsID(reference.NodeIDs, resolvedSpanID) {
			t.Fatalf("resolved source role projection = %+v", reference)
		}
		return
	}
	t.Fatal("verified result omitted document evidence addition")
}

type verificationSnapshotReader struct {
	*explorer.Explorer
	snapshot      explorer.Snapshot
	snapshots     []explorer.Snapshot
	snapshotCalls int
	authorization inference.AuthPin
}

func (r *verificationSnapshotReader) Snapshot(
	context.Context,
) (explorer.Snapshot, error) {
	if len(r.snapshots) > 0 {
		index := r.snapshotCalls
		if index >= len(r.snapshots) {
			index = len(r.snapshots) - 1
		}
		r.snapshotCalls++
		return r.snapshots[index], nil
	}
	return r.snapshot, nil
}

func (r *verificationSnapshotReader) ValidateAuthorization(
	_ context.Context,
	pin inference.AuthPin,
) error {
	if pin.Fingerprint() != r.authorization.Fingerprint() ||
		!pin.ExpiresAt().Equal(r.authorization.ExpiresAt()) {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"authorization pin does not match verification reader",
		)
	}
	return nil
}

type verificationGraphReader struct {
	verificationSnapshotReader
	neighborhood explorer.Neighborhood
}

func (r *verificationGraphReader) Neighborhood(
	context.Context,
	explorer.NeighborhoodRequest,
) (explorer.Neighborhood, error) {
	return r.neighborhood, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func testContainsID(values []shoal.ID, target shoal.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
