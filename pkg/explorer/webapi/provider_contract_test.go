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

package webapi

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCitationEnvelopeInteractionEvidenceUsesAcceptedSession(t *testing.T) {
	client, reader, pack, result, policyID := citationWireFixture(t)
	builder, err := reasoning.NewBuilder(reader)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: pack,
		Result:      result,
		Policy:      reasoning.Policy{ID: policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := pack.Snapshot().AsOf().Add(2 * time.Minute)
	session, err := prepared.CaptureMetadata().NewSession(
		interaction.OperationChat, "interaction-evidence", recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := interaction.NewRecorder(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return recordedAt }); err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Capture(context.Background(), recorder, session)
	if err != nil {
		t.Fatal(err)
	}
	envelope := NewCitationEnvelope(response)
	retrieved, cited, err := envelope.InteractionEvidence()
	if err != nil {
		t.Fatal(err)
	}
	accepted := response.RecordedSession()
	if !reflect.DeepEqual(retrieved, sessionRetrievedEvidence(accepted)) ||
		!reflect.DeepEqual(cited, accepted.CitedEvidence) {
		t.Fatal("interaction evidence differs from the accepted session")
	}
	if len(retrieved) == 0 || len(cited) == 0 {
		t.Fatal("fixture did not produce complete interaction evidence")
	}

	retrieved[0].NodeIDs[0] = "mutated"
	cited[0].NodeIDs[0] = "mutated"
	retrievedAgain, citedAgain, err := envelope.InteractionEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retrievedAgain, sessionRetrievedEvidence(accepted)) ||
		!reflect.DeepEqual(citedAgain, accepted.CitedEvidence) {
		t.Fatal("returned interaction evidence aliases the envelope session")
	}
}

func TestCitationEnvelopeInteractionEvidenceIsNotWireReconstructed(t *testing.T) {
	client, reader, pack, result, policyID := citationWireFixture(t)
	builder, err := reasoning.NewBuilder(reader)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: pack,
		Result:      result,
		Policy:      reasoning.Policy{ID: policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := pack.Snapshot().AsOf().Add(2 * time.Minute)
	session, err := prepared.CaptureMetadata().NewSession(
		interaction.OperationChat, "wire-evidence", recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := interaction.NewRecorder(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return recordedAt }); err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Capture(context.Background(), recorder, session)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(NewCitationEnvelope(response))
	if err != nil {
		t.Fatal(err)
	}
	var decoded CitationEnvelope
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err := decoded.InteractionEvidence(); err == nil {
		t.Fatal("wire envelope reconstructed trusted interaction evidence")
	}
}

func TestCitationEvidenceProjectionPreservesCompleteDomainEvidence(t *testing.T) {
	client, reader, pack, result, policyID := citationWireFixture(t)
	builder, err := reasoning.NewBuilder(reader)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: pack,
		Result:      result,
		Policy: reasoning.Policy{
			ID: policyID, ExtraOutputVisibility: []string{"api"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := pack.Snapshot().AsOf().Add(2 * time.Minute)
	session, err := prepared.CaptureMetadata().NewSession(
		interaction.OperationChat, "projection-correlation", recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := interaction.NewRecorder(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return recordedAt }); err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Capture(context.Background(), recorder, session)
	if err != nil {
		t.Fatal(err)
	}
	envelope := NewCitationEnvelope(response)
	envelope.OutputVisibility = "api&internal"
	projection, err := envelope.EvidenceProjection()
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.RetrievedSourceIDs) != len(envelope.RetrievedSourceIDs) ||
		len(projection.CitedSourceIDs) != len(envelope.CitedSourceIDs) ||
		len(projection.Anchors) != len(envelope.Evidence) {
		t.Fatalf("projection lost evidence: %+v", projection)
	}
	if projection.OutputVisibility != envelope.OutputVisibility {
		t.Fatalf("output visibility = %q", projection.OutputVisibility)
	}
	for index, anchor := range projection.Anchors {
		source := envelope.Evidence[index]
		if anchor.AnchorID != source.AnchorID ||
			len(anchor.SourceIDs) != len(source.SourceIDs) ||
			len(anchor.Assertions) != len(source.Assertions) {
			t.Fatalf("anchor %d changed: %+v != %+v", index, anchor, source)
		}
		if source.Citation != nil &&
			(anchor.Citation == nil || *anchor.Citation != *source.Citation) {
			t.Fatalf("anchor %d lost exact citation", index)
		}
		if source.Path != nil &&
			len(anchor.EdgeIDs) != len(source.Path.Edges) {
			t.Fatalf("anchor %d lost graph edges", index)
		}
	}
	if len(projection.EmbeddingSpaceIDs) != len(envelope.EmbeddingSpaceIDs) {
		t.Fatal("projection changed embedding constituent cardinality")
	}
}

func TestCitationEvidenceProjectionDoesNotAliasEnvelope(t *testing.T) {
	client, reader, pack, result, policyID := citationWireFixture(t)
	builder, err := reasoning.NewBuilder(reader)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: pack, Result: result, Policy: reasoning.Policy{ID: policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := pack.Snapshot().AsOf().Add(time.Minute)
	session, err := prepared.CaptureMetadata().NewSession(
		interaction.OperationChat, "projection-copy", recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := interaction.NewRecorder(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return recordedAt }); err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Capture(context.Background(), recorder, session)
	if err != nil {
		t.Fatal(err)
	}
	envelope := NewCitationEnvelope(response)
	projection, err := envelope.EvidenceProjection()
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.RetrievedSourceIDs) == 0 || len(projection.Anchors) == 0 {
		t.Fatal("fixture did not produce evidence")
	}
	projection.RetrievedSourceIDs[0] = "changed"
	projection.Anchors[0].SourceIDs[0] = "changed"
	if envelope.RetrievedSourceIDs[0] == "changed" ||
		envelope.Evidence[0].SourceIDs[0] == "changed" {
		t.Fatal("projection aliases the citation envelope")
	}
}

func TestCitationEvidenceProjectionIncludesGraphAssertionsAndExactRange(t *testing.T) {
	citation := document.Citation{
		DocumentID: "\x00document",
		RevisionID: "\xffrevision",
		SectionID:  "section",
		SpanID:     "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 7, Page: 2},
			End:   document.SourcePosition{Offset: 19, Page: 3},
		},
	}
	envelope := CitationEnvelope{
		RetrievedSourceIDs:  []shoal.ID{"node-b", "node-a"},
		CitedSourceIDs:      []shoal.ID{"node-b"},
		EffectiveVisibility: []string{"restricted", "team"},
		OutputVisibility:    "restricted&team",
		EmbeddingSpaceIDs:   []shoal.ID{"space-b", "space-a"},
		Evidence: []CitationEvidence{
			{
				AnchorID:   "graph-anchor",
				Status:     reasoning.VerificationVerified,
				SourceIDs:  []shoal.ID{"node-b", "node-a"},
				Visibility: []string{"restricted"},
				Path: &graph.Path{Edges: []graph.Edge{
					{ID: "edge-b"}, {ID: "edge-a"},
				}},
				Assertions: []CitationAssertion{{
					AssertionID: "assertion-a",
					EdgeID:      "edge-b",
					Origin:      ontology.AssertionExplicit,
				}},
			},
			{
				AnchorID:   "document-anchor",
				Status:     reasoning.VerificationVerified,
				SourceIDs:  []shoal.ID{citation.DocumentID},
				Visibility: []string{"team"},
				Citation:   &citation,
				SourceURI:  "memory://source",
			},
		},
	}
	projection := projectCitationEvidence(envelope)
	if len(projection.EdgeIDs) != 2 ||
		projection.EdgeIDs[0] != "edge-a" ||
		projection.EdgeIDs[1] != "edge-b" {
		t.Fatalf("edge IDs = %q", projection.EdgeIDs)
	}
	if len(projection.AssertionIDs) != 1 ||
		projection.AssertionIDs[0] != "assertion-a" {
		t.Fatalf("assertion IDs = %q", projection.AssertionIDs)
	}
	if len(projection.Anchors) != 2 ||
		projection.Anchors[0].AnchorID != "document-anchor" ||
		projection.Anchors[0].Citation == nil ||
		*projection.Anchors[0].Citation != citation ||
		projection.Anchors[0].SourceURI != "memory://source" {
		t.Fatalf("document anchor changed: %+v", projection.Anchors)
	}
	graphAnchor := projection.Anchors[1]
	if len(graphAnchor.Assertions) != 1 ||
		graphAnchor.Assertions[0].AssertionID != "assertion-a" ||
		graphAnchor.Assertions[0].EdgeID != "edge-b" ||
		graphAnchor.Assertions[0].Origin != ontology.AssertionExplicit {
		t.Fatalf("graph assertions changed: %+v", graphAnchor.Assertions)
	}
	projection.Anchors[0].Citation.DocumentID = "changed"
	if envelope.Evidence[1].Citation.DocumentID == "changed" {
		t.Fatal("projected citation aliases the envelope")
	}
}

func TestInteractionProviderSemantics(t *testing.T) {
	tests := []struct {
		method   InteractionProviderMethod
		readOnly bool
		mutating bool
	}{
		{InteractionMethodList, true, false},
		{InteractionMethodInspect, true, false},
		{InteractionMethodFold, false, true},
		{InteractionMethodUnfold, true, false},
	}
	for _, test := range tests {
		semantics, err := InteractionSemantics(test.method)
		if err != nil {
			t.Fatal(err)
		}
		if semantics.Operation != auth.OperationRead ||
			semantics.ReadOnly != test.readOnly ||
			semantics.Mutating != test.mutating ||
			!semantics.Idempotent {
			t.Fatalf("%s semantics = %+v", test.method, semantics)
		}
	}
	if _, err := InteractionSemantics("unknown"); err == nil {
		t.Fatal("unknown interaction method was accepted")
	}
}
