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

package reasoning_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var fixtureSequence atomic.Uint64

func TestMultipleClaimsCanCiteSameVerifiedAnchor(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Shared\n\nTwo claims use the same verified quote.\n",
	}}, false, "policy", "request")
	model, prompt := provenance(t)
	anchorID := fixture.documentAnchors[0].ID()
	claims := make([]inference.Claim, 0, 2)
	for _, subject := range []shoal.ID{"first", "second"} {
		value, err := ontology.NewStringValue(string(subject) + " result")
		if err != nil {
			t.Fatal(err)
		}
		claim, err := inference.NewClaim(
			subject, "predicate", value, 0.9, []shoal.ID{anchorID},
			inference.ClaimInferred, model, prompt, nil)
		if err != nil {
			t.Fatal(err)
		}
		claims = append(claims, claim)
	}
	result, err := inference.NewInferenceResult(
		fixture.pack, claims, nil, fixture.generatedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := reasoning.NewBuilder(fixture.reader())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack,
		Result:      result,
		Policy:      reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := capture(t, prepared, &captureSink{})
	if len(response.Claims()) != 2 {
		t.Fatalf("claim count = %d; want 2", len(response.Claims()))
	}
	if got := response.RecordedSession().CitedEvidence; len(got) != 1 ||
		got[0].AnchorID != anchorID {
		t.Fatalf("canonical cited evidence = %+v", got)
	}
}

func TestVerifiedEnvelopeUsesAllTouchedVisibilityAndIsDeterministic(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{
		{content: "# Public\n\nPublic grounded evidence.\n"},
		{content: "# Restricted\n\nRestricted retrieved evidence.\n", visibility: "secret"},
	}, false, "\xffpolicy", "\xferequest")
	result := fixture.result(t, []shoal.ID{fixture.documentAnchors[0].ID()}, true)
	builder, err := reasoning.NewBuilder(fixture.reader())
	if err != nil {
		t.Fatal(err)
	}
	left, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack,
		Result:      result,
		Policy: reasoning.Policy{
			ID: fixture.policyID, ExtraOutputVisibility: []string{"tenant", "ops"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack,
		Result:      result,
		Policy: reasoning.Policy{
			ID: fixture.policyID, ExtraOutputVisibility: []string{"ops", "tenant"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	leftResponse := capture(t, left, &captureSink{})
	rightResponse := capture(t, right, &captureSink{})
	if leftResponse.ID() != rightResponse.ID() {
		t.Fatalf("response IDs differ: %q != %q", leftResponse.ID(), rightResponse.ID())
	}
	assertStrings(t, leftResponse.EffectiveOutputVisibility(),
		[]string{"ops", "secret", "tenant"})
	if len(leftResponse.RetrievedSourceIDs()) != 6 {
		t.Fatalf("retrieved source count = %d", len(leftResponse.RetrievedSourceIDs()))
	}
	if len(leftResponse.CitedSourceIDs()) != 3 {
		t.Fatalf("cited source count = %d", len(leftResponse.CitedSourceIDs()))
	}
	if len(leftResponse.Claims()) != 1 ||
		len(leftResponse.Claims()[0].Citations()) != 1 {
		t.Fatal("verified citation-backed claim was not preserved")
	}
	originalCitation, _, _ := fixture.documentAnchors[0].Document()
	recordedCitation := leftResponse.RecordedSession().CitedEvidence
	if len(recordedCitation) != 1 ||
		recordedCitation[0].AnchorID != fixture.documentAnchors[0].ID() ||
		recordedCitation[0].Citation != originalCitation {
		t.Fatalf("recorded exact citation = %+v", recordedCitation)
	}
	if got := leftResponse.Claims()[0].Citations()[0].Status(); got != reasoning.VerificationVerified {
		t.Fatalf("citation status = %q", got)
	}
	if len(leftResponse.Issues()) != 1 ||
		leftResponse.Issues()[0].Kind() != reasoning.IssueUnsupported {
		t.Fatal("partial unsupported outcome was not preserved")
	}

	visibility := leftResponse.EffectiveOutputVisibility()
	visibility[0] = "mutated"
	sources := leftResponse.Sources()
	sources[0] = reasoning.SourceReference{}
	if got := leftResponse.EffectiveOutputVisibility()[0]; got != "ops" {
		t.Fatalf("response leaked visibility mutation: %q", got)
	}
}

func TestGraphPathIsDerivedNotCitationByDefault(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Graph\n\nGraph explanation remains separate.\n",
	}}, true, "policy", "request")
	if fixture.graphAnchor.ID() == "" {
		t.Fatal("fixture has no graph anchor")
	}
	result := fixture.result(t, []shoal.ID{fixture.graphAnchor.ID()}, false)
	builder, err := reasoning.NewBuilder(fixture.reader())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := capture(t, prepared, &captureSink{})
	if len(response.Claims()) != 0 {
		t.Fatal("path-only claim became a successful cited claim")
	}
	if len(response.CitedSourceIDs()) != 0 {
		t.Fatal("path nodes were laundered into cited source identities")
	}
	if len(response.Issues()) != 1 ||
		response.Issues()[0].Kind() != reasoning.IssueUnverified {
		t.Fatal("path-only claim did not become an unverified issue")
	}
	if response.Issues()[0].OutcomeType() != reasoning.IssueOutcomeClaim {
		t.Fatal("path-only claim lost its outcome discriminator")
	}
	rejectedClaim, ok := response.Issues()[0].Claim()
	if !ok || rejectedClaim.ID() != result.Claims()[0].ID() {
		t.Fatal("path-only claim payload was not retained")
	}
	evidence := response.Issues()[0].Evidence()
	if len(evidence) != 1 ||
		evidence[0].Use() != reasoning.EvidenceDerived ||
		evidence[0].Origin() != reasoning.OriginSource {
		t.Fatal("path evidence lost its derived-use/source-origin distinction")
	}
}

func TestSourceOnlyRejectsAuthoritativeInferredGraphMaterial(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{
		{content: "# One\n\nFirst endpoint.\n"},
		{content: "# Two\n\nSecond endpoint.\n"},
	}, false, "policy", "request")
	firstCitation, firstQuote, _ := fixture.documentAnchors[0].Document()
	secondCitation, secondQuote, _ := fixture.documentAnchors[1].Document()
	evidenceRef, err := ontology.NewEvidenceRef(
		firstCitation, firstQuote, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"test", "extractor-model", "v1", "prompt", "v1",
		"extractor", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"endpoint", "Endpoint", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"related", "Related", "",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	object, err := ontology.NewReferenceValue(secondCitation.SpanID)
	if err != nil {
		t.Fatal(err)
	}
	edgeID := shoal.ID("extracted-relation-edge")
	assertion, err := ontology.NewAssertion(
		firstCitation.SpanID, relationship.ID(), object,
		ontology.AssertionInferred, 0.9,
		[]ontology.EvidenceRef{evidenceRef}, provenance,
		shoal.Metadata{"shoal.graph.edge_id": string(edgeID)})
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID: edgeID, From: firstCitation.SpanID, To: secondCitation.SpanID,
		Type: "related", Weight: 0.9,
		Properties: shoal.Metadata{
			"ontology_relationship_id": string(relationship.ID()),
		},
	}
	if err := fixture.client.Connect(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	reader := assertionReader{
		AuthorizationReader: fixture.reader(), assertion: assertion,
	}
	neighborhood, err := reader.Neighborhood(
		context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{firstCitation.SpanID, secondCitation.SpanID},
			Depth:   1,
		})
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[shoal.ID]graph.Node)
	for _, node := range neighborhood.Nodes {
		nodes[node.ID] = node
	}
	path := graph.Path{
		Nodes: []graph.Node{
			nodes[firstCitation.SpanID],
			nodes[secondCitation.SpanID],
		},
		Edges: []graph.Edge{edge},
	}
	snapshot, err := fixture.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request, err := (retrieval.Request{
		Text: "derived relation", TopK: 2,
		Modes: []retrieval.Mode{retrieval.ModeGraph}, AsOf: snapshot.AsOf,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	response := retrieval.Response{
		RequestID: "derived-request",
		Results: []retrieval.Result{
			{
				ID: firstCitation.DocumentID, Score: 1,
				Evidence: []retrieval.Evidence{{
					Citation: firstCitation, Quote: firstQuote,
					Path: path, Score: 1,
				}},
			},
			{
				ID: secondCitation.DocumentID, Score: 0.9,
				Evidence: []retrieval.Evidence{{
					Citation: secondCitation, Quote: secondQuote, Score: 0.9,
				}},
			},
		},
	}
	snapshotPin, err := inference.NewSnapshotPin(
		shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	authPin, err := inference.NewAuthPin(
		"derived-auth", snapshot.AsOf.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	input := contextpack.InitialRequest{
		Request: request, Response: response,
		Pins: contextpack.Pins{
			Snapshot: snapshotPin, Authorization: authPin,
			PolicyID: fixture.policyID,
		},
	}
	if _, err := (contextpack.Builder{Reader: fixture.client}).Build(
		context.Background(), input,
	); err != nil {
		t.Fatalf("source graph without authoritative assertion = %v", err)
	}
	_, err = (contextpack.Builder{Reader: reader}).Build(
		context.Background(), input)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("inferred assertion-backed source evidence error = %v", err)
	}
}

func TestSnapshotChangeFailsVerification(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Snapshot\n\nPinned evidence.\n",
	}}, false, "policy", "request")
	result := fixture.result(t, []shoal.ID{fixture.documentAnchors[0].ID()}, false)
	if _, err := fixture.client.Ingest(
		context.Background(), explorer.Source{
			URI: "memory://changed", Title: "Changed",
			MediaType: explorer.MediaTypeMarkdown,
			Content:   "# Changed\n\nThe corpus moved.\n",
		}); err != nil {
		t.Fatal(err)
	}
	builder, _ := reasoning.NewBuilder(fixture.reader())
	_, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("snapshot mismatch error = %v", err)
	}
}

func TestForgedEmbeddingSpaceSetFailsClosed(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Vector\n\nVerified source evidence.\n",
	}}, false, "policy", "request")
	result := fixture.result(
		t, []shoal.ID{fixture.documentAnchors[0].ID()}, false)
	builder, _ := reasoning.NewBuilder(fixture.reader())
	if _, err := builder.Build(
		context.Background(), reasoning.BuildInput{
			ContextPack: fixture.pack, Result: result,
			Policy: reasoning.Policy{ID: fixture.policyID},
			EmbeddingSpaces: interaction.EmbeddingSpaceSet{
				Identities: []string{"space-a"},
				Digest:     "forged",
			},
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("forged embedding space set error = %v", err)
	}
}

func TestEmbeddingSpaceSetIsCapturedFromExplicitInput(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Vector\n\nVerified source evidence.\n",
	}}, false, "policy", "request")
	spaces, err := interaction.NewEmbeddingSpaceSet(
		[]string{"space-b", "space-a", "space-b"})
	if err != nil {
		t.Fatal(err)
	}
	builder, err := reasoning.NewBuilder(fixture.reader())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(
		context.Background(), reasoning.BuildInput{
			ContextPack: fixture.pack,
			Result: fixture.result(
				t, []shoal.ID{fixture.documentAnchors[0].ID()}, false),
			Policy:          reasoning.Policy{ID: fixture.policyID},
			EmbeddingSpaces: spaces,
		})
	if err != nil {
		t.Fatal(err)
	}
	metadata := prepared.CaptureMetadata()
	if got := metadata.EmbeddingSpaces(); !reflect.DeepEqual(got, spaces) {
		t.Fatalf("embedding spaces = %+v, want %+v", got, spaces)
	}
	exposed := metadata.EmbeddingSpaces()
	exposed.Identities[0] = "mutated"
	if got := metadata.EmbeddingSpaces(); !reflect.DeepEqual(got, spaces) {
		t.Fatalf("embedding spaces mutated through accessor: %+v", got)
	}
	session := captureSession(t, prepared)
	if !reflect.DeepEqual(session.EmbeddingSpaces, spaces) {
		t.Fatalf(
			"session embedding spaces = %+v, want %+v",
			session.EmbeddingSpaces, spaces,
		)
	}
	response := capture(t, prepared, &captureSink{})
	if got := response.EmbeddingSpaces(); !reflect.DeepEqual(got, spaces) {
		t.Fatalf("response embedding spaces = %+v, want %+v", got, spaces)
	}
}

func TestForgedWrongQuoteSpanAndRevisionFailVerification(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Unicode\n\nnaïve café ☕ evidence.\n",
	}}, false, "policy", "request")
	base := fixture.documentAnchors[0]
	citation, quote, ok := base.Document()
	if !ok {
		t.Fatal("fixture anchor is not a document citation")
	}
	tests := map[string]struct {
		citation document.Citation
		quote    string
	}{
		"quote": {
			citation: citation,
			quote:    string(bytes.Repeat([]byte{'x'}, len(quote))),
		},
		"span": {
			citation: func() document.Citation {
				value := citation
				value.SpanID = "model-produced-span"
				return value
			}(),
			quote: quote,
		},
		"revision": {
			citation: func() document.Citation {
				value := citation
				value.RevisionID = "model-produced-revision"
				return value
			}(),
			quote: quote,
		},
	}
	builder, err := reasoning.NewBuilder(fixture.reader())
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			forged, err := inference.NewDocumentAnchor(test.citation, test.quote)
			if err != nil {
				t.Fatal(err)
			}
			result := fixture.extendedResult(t, forged)
			_, err = builder.Build(context.Background(), reasoning.BuildInput{
				ContextPack: fixture.pack, Result: result,
				Policy: reasoning.Policy{ID: fixture.policyID},
			})
			if err == nil {
				t.Fatal("forged citation was accepted")
			}
		})
	}
}

func TestUnicodeExactQuoteOffsetsAreReverified(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Unicode\n\nnaïve café ☕ evidence.\n",
	}}, false, "policy", "request")
	full, _, _ := fixture.documentAnchors[0].Document()
	view, err := fixture.client.Document(
		context.Background(), full.DocumentID, full.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	span := firstSpan(t, view.Root)
	needle := "café ☕"
	relative := bytes.Index([]byte(span.Text), []byte(needle))
	if relative < 0 {
		t.Fatalf("quote %q not found in span %q", needle, span.Text)
	}
	start := span.Range.Start.Offset + int64(relative)
	citation := document.Citation{
		DocumentID: span.DocumentID, RevisionID: span.RevisionID,
		SectionID: span.SectionID, SpanID: span.ID,
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: start},
			End:   document.SourcePosition{Offset: start + int64(len(needle))},
		},
	}
	anchor, err := inference.NewDocumentAnchor(citation, needle)
	if err != nil {
		t.Fatal(err)
	}
	result := fixture.extendedResult(t, anchor)
	builder, _ := reasoning.NewBuilder(fixture.reader())
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := prepared.CaptureMetadata()
	if len(metadata.SeedEvidence()) != len(fixture.pack.Evidence()) ||
		len(metadata.AdditionEvidence()) != 1 {
		t.Fatal("capture metadata lost original/addition evidence partition")
	}
	withoutTool, err := metadata.NewSession(
		interaction.OperationChat,
		"citation-addition-without-tool",
		metadata.GeneratedAt(),
	)
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return metadata.GeneratedAt().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Capture(
		context.Background(), recorder, withoutTool); err == nil {
		t.Fatal("addition capture without a retrieval tool turn succeeded")
	}
	if sink.records != 0 {
		t.Fatal("invalid addition capture reached durable storage")
	}
	response := capture(t, prepared, &captureSink{})
	found := false
	for _, evidence := range response.Evidence() {
		got, quote, ok := evidence.Anchor().Document()
		if ok && got == citation && quote == needle {
			found = true
		}
	}
	if !found {
		t.Fatal("Unicode exact quote was not retained")
	}
}

func TestPartialDocumentCitationsCaptureResolvedProvenance(t *testing.T) {
	for _, omitted := range []string{"section", "span"} {
		t.Run(omitted, func(t *testing.T) {
			fixture := newFixture(t, []sourceFixture{{
				content: "# Partial\n\nResolved source roles remain authoritative.\n",
			}}, false, "policy", "request")
			citation, quote, ok := fixture.documentAnchors[0].Document()
			if !ok {
				t.Fatal("fixture anchor is not document evidence")
			}
			resolvedSectionID := citation.SectionID
			resolvedSpanID := citation.SpanID
			if omitted == "section" {
				citation.SectionID = ""
			} else {
				citation.SpanID = ""
			}
			addition, err := inference.NewDocumentAnchor(citation, quote)
			if err != nil {
				t.Fatal(err)
			}
			result := fixture.extendedResult(t, addition)
			builder, err := reasoning.NewBuilder(fixture.reader())
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := builder.Build(
				context.Background(), reasoning.BuildInput{
					ContextPack: fixture.pack,
					Result:      result,
					Policy: reasoning.Policy{
						ID: fixture.policyID,
					},
				})
			if err != nil {
				t.Fatal(err)
			}
			additions := prepared.CaptureMetadata().AdditionEvidence()
			if len(additions) != 1 ||
				additions[0].Citation != citation ||
				len(additions[0].NodeIDs) != 3 ||
				!testContainsID(additions[0].NodeIDs, resolvedSectionID) ||
				!testContainsID(additions[0].NodeIDs, resolvedSpanID) {
				t.Fatalf("resolved addition evidence = %+v", additions)
			}
			recorder, err := interaction.NewRecorder(
				context.Background(), fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			if err := recorder.SetClock(func() time.Time {
				return fixture.generatedAt.Add(time.Minute)
			}); err != nil {
				t.Fatal(err)
			}
			response, err := prepared.Capture(
				context.Background(), recorder, captureSession(t, prepared))
			if err != nil {
				t.Fatal(err)
			}
			recorded := response.RecordedSession()
			if len(recorded.Turns) != 1 ||
				recorded.Turns[0].ToolCall == nil ||
				len(recorded.Turns[0].ToolCall.RetrievedEvidence) != 1 {
				t.Fatalf("recorded partial citation = %+v", recorded)
			}
			reference := recorded.Turns[0].ToolCall.RetrievedEvidence[0]
			if reference.Citation != citation ||
				!testContainsID(reference.NodeIDs, resolvedSectionID) ||
				!testContainsID(reference.NodeIDs, resolvedSpanID) {
				t.Fatalf("recorded resolved roles = %+v", reference)
			}
		})
	}
}

func TestCitationAndRetrievedIdentitiesAreNotClippedAtTwenty(t *testing.T) {
	sources := make([]sourceFixture, 21)
	for index := range sources {
		sources[index].content = fmt.Sprintf(
			"# Source %02d\n\nGrounded evidence %02d.\n", index, index)
	}
	fixture := newFixture(t, sources, false, "policy", "request")
	ids := make([]shoal.ID, 0, len(fixture.documentAnchors))
	for _, anchor := range fixture.documentAnchors {
		ids = append(ids, anchor.ID())
	}
	result := fixture.result(t, ids, false)
	builder, _ := reasoning.NewBuilder(fixture.reader())
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	truncated := captureSession(t, prepared)
	truncated.CitedNodeIDs = truncated.CitedNodeIDs[:20]
	truncatedSink := &captureSink{}
	truncatedRecorder, err := interaction.NewRecorder(
		context.Background(), truncatedSink)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Capture(
		context.Background(), truncatedRecorder, truncated,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("truncated citation capture error = %v", err)
	}
	if truncatedSink.records != 0 {
		t.Fatal("truncated citation provenance reached the durable sink")
	}
	response := capture(t, prepared, &captureSink{})
	if len(response.RetrievedSourceIDs()) != 63 {
		t.Fatalf("retrieved source count = %d, want 63",
			len(response.RetrievedSourceIDs()))
	}
	if len(response.CitedSourceIDs()) != 63 {
		t.Fatalf("cited source count = %d, want 63",
			len(response.CitedSourceIDs()))
	}
	if got := len(response.Claims()[0].Citations()); got != 21 {
		t.Fatalf("claim citations = %d, want 21", got)
	}
}

func TestAuthorizedHiddenEvidenceFailsClosed(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Hidden\n\nAuthorized evidence.\n", visibility: "secret",
	}}, false, "policy", "request")
	result := fixture.result(t, []shoal.ID{fixture.documentAnchors[0].ID()}, false)
	citation, _, _ := fixture.documentAnchors[0].Document()
	reader := hiddenReader{AuthorizationReader: fixture.reader(), hidden: citation.DocumentID}
	builder, err := reasoning.NewBuilder(reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("hidden evidence error = %v", err)
	}
}

func TestNoResponseBeforeDurableCaptureSucceeds(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Capture\n\nDurable evidence.\n", visibility: "secret",
	}}, false, "policy", "request")
	result := fixture.result(t, []shoal.ID{fixture.documentAnchors[0].ID()}, false)
	builder, _ := reasoning.NewBuilder(fixture.reader())
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("durable write failed")
	sink := &captureSink{recordErr: failure}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return prepared.CaptureMetadata().GeneratedAt().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Capture(
		context.Background(), recorder, captureSession(t, prepared))
	if !errors.Is(err, failure) {
		t.Fatalf("capture error = %v", err)
	}
	if response.ID() != "" {
		t.Fatal("response escaped a failed durable capture")
	}
	if sink.records != 1 {
		t.Fatalf("record attempts = %d", sink.records)
	}
	trustedActor := interaction.ActorContext{
		SubjectID: "trusted-subject", ActorID: "trusted-actor",
	}
	success := &captureSink{enrich: func(session interaction.Session) interaction.Session {
		trustedActor.OnBehalfOf = []shoal.ID{"trusted-delegate"}
		session.Actor = trustedActor
		return session
	}}
	recorder, err = interaction.NewRecorder(context.Background(), success)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return prepared.CaptureMetadata().GeneratedAt().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	session := captureSession(t, prepared)
	session.Turns = []interaction.Turn{{
		Index: 0,
		ToolCall: &interaction.ToolCall{
			Kind: "retrieve", RetrievedNodeIDs: session.SeedNodeIDs,
			RetrievedEvidence: session.SeedEvidence,
		},
	}}
	captured, err := prepared.Capture(context.Background(), recorder, session)
	if err != nil {
		t.Fatal(err)
	}
	if success.records != 1 {
		t.Fatalf("record count = %d", success.records)
	}
	if captured.RecordedSession().Actor.SubjectID != trustedActor.SubjectID ||
		captured.RecordedSession().Actor.ActorID != trustedActor.ActorID {
		t.Fatal("captured response did not retain trusted persisted enrichment")
	}
	success.session.Actor.OnBehalfOf[0] = "mutated"
	success.session.SeedNodeIDs[0] = "mutated"
	success.session.SeedEvidence[0].NodeIDs[0] = "mutated"
	success.session.Turns[0].ToolCall.RetrievedNodeIDs[0] = "mutated"
	success.session.Turns[0].ToolCall.RetrievedEvidence[0].NodeIDs[0] = "mutated"
	recorded := captured.RecordedSession()
	if recorded.Actor.OnBehalfOf[0] != "trusted-delegate" ||
		recorded.SeedNodeIDs[0] == "mutated" ||
		recorded.SeedEvidence[0].NodeIDs[0] == "mutated" ||
		recorded.Turns[0].ToolCall.RetrievedNodeIDs[0] == "mutated" ||
		recorded.Turns[0].ToolCall.RetrievedEvidence[0].NodeIDs[0] == "mutated" {
		t.Fatal("captured response retained sink-owned mutable slices")
	}

	invalidPersisted := &captureSink{
		enrich: func(session interaction.Session) interaction.Session {
			session.ContextPackID = "wrong-pack"
			return session
		},
	}
	recorder, err = interaction.NewRecorder(
		context.Background(), invalidPersisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return prepared.CaptureMetadata().GeneratedAt().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = prepared.Capture(
		context.Background(), recorder, captureSession(t, prepared))
	if !explorer.IsCommittedInteraction(err) {
		t.Fatalf("post-persistence validation error is not committed: %v", err)
	}
	if invalidPersisted.records != 1 {
		t.Fatalf("post-persistence validation writes = %d", invalidPersisted.records)
	}
}

func TestCaptureUsesRecorderTimeAndRejectsInvalidPersistedChronology(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Time\n\nChronology is verified.\n",
	}}, false, "policy", "request")
	result := fixture.result(t, []shoal.ID{fixture.documentAnchors[0].ID()}, false)
	builder, _ := reasoning.NewBuilder(fixture.reader())
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := captureSession(t, prepared)
	acceptedAt := result.GeneratedAt().Add(time.Minute)
	var acceptedIDs []shoal.ID
	for name, callerTime := range map[string]time.Time{
		"backdated": fixture.pack.Snapshot().AsOf().Add(-time.Hour),
		"future":    fixture.pack.Authorization().ExpiresAt().Add(time.Hour),
	} {
		t.Run("caller "+name, func(t *testing.T) {
			session := valid
			session.RecordedAt = callerTime
			response, err := prepared.Capture(
				context.Background(),
				recorderFunc(func(
					_ context.Context, session interaction.Session,
				) (interaction.Session, error) {
					if !session.RecordedAt.IsZero() {
						t.Fatal("caller timestamp reached trusted recorder")
					}
					session.RecordedAt = acceptedAt
					return session, nil
				}),
				session,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !response.RecordedAt().Equal(acceptedAt) {
				t.Fatalf("recorded time = %v", response.RecordedAt())
			}
			acceptedIDs = append(acceptedIDs, response.ID())
		})
	}
	if acceptedIDs[0] != acceptedIDs[1] {
		t.Fatal("caller timestamps changed response identity")
	}

	for name, recordedAt := range map[string]time.Time{
		"before snapshot":   fixture.pack.Snapshot().AsOf().Add(-time.Nanosecond),
		"before generation": result.GeneratedAt().Add(-time.Nanosecond),
		"at expiry":         fixture.pack.Authorization().ExpiresAt(),
		"future":            fixture.pack.Authorization().ExpiresAt().Add(time.Hour),
	} {
		t.Run("sink "+name, func(t *testing.T) {
			_, err := prepared.Capture(
				context.Background(),
				recorderFunc(func(
					_ context.Context, session interaction.Session,
				) (interaction.Session, error) {
					session.RecordedAt = recordedAt
					return session, nil
				}),
				valid,
			)
			if err == nil {
				t.Fatal("invalid persisted chronology was accepted")
			}
		})
	}

	expiredSink := &captureSink{}
	expiredRecorder, err := interaction.NewRecorder(
		context.Background(), expiredSink)
	if err != nil {
		t.Fatal(err)
	}
	if err := expiredRecorder.SetClock(func() time.Time {
		return fixture.pack.Authorization().ExpiresAt()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Capture(
		context.Background(), expiredRecorder, valid,
	); err == nil {
		t.Fatal("capture after authorization expiry succeeded by backdating")
	}
	if expiredSink.records != 0 {
		t.Fatal("expired capture reached durable sink")
	}

	laterAt := acceptedAt.Add(time.Second)
	later, err := prepared.Capture(
		context.Background(),
		recorderFunc(func(
			_ context.Context, session interaction.Session,
		) (interaction.Session, error) {
			session.RecordedAt = laterAt
			return session, nil
		}),
		valid,
	)
	if err != nil {
		t.Fatal(err)
	}
	if later.ID() == acceptedIDs[0] {
		t.Fatal("accepted recorder timestamp did not affect response identity")
	}
}

func TestCaptureRequiresEmptyPinnedRequestIDToRemainEmpty(t *testing.T) {
	fixture := newFixture(t, []sourceFixture{{
		content: "# Request\n\nNo request identifier was pinned.\n",
	}}, false, "policy", "")
	result := fixture.result(t, []shoal.ID{fixture.documentAnchors[0].ID()}, false)
	builder, _ := reasoning.NewBuilder(fixture.reader())
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: fixture.pack, Result: result,
		Policy: reasoning.Policy{ID: fixture.policyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := captureSession(t, prepared)
	session.RequestID = "injected-request"
	sink := &captureSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Capture(
		context.Background(), recorder, session,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("injected request ID error = %v", err)
	}
	if sink.records != 0 {
		t.Fatal("injected request ID reached durable capture")
	}
	session = captureSession(t, prepared)
	if _, err := prepared.Capture(
		context.Background(),
		recorderFunc(func(
			_ context.Context, session interaction.Session,
		) (interaction.Session, error) {
			session.RequestID = "sink-injected-request"
			return session, nil
		}),
		session,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("sink-injected request ID error = %v", err)
	}
}

type sourceFixture struct {
	content    string
	visibility string
}

type fixture struct {
	client          *explorer.Explorer
	pack            inference.ContextPack
	policyID        shoal.ID
	documentAnchors []inference.EvidenceAnchor
	graphAnchor     inference.EvidenceAnchor
	generatedAt     time.Time
}

func (f fixture) reader() contextpack.AuthorizationReader {
	return testAuthorizationReader{
		SnapshotReader: f.client,
		authorization:  f.pack.Authorization(),
	}
}

type testAuthorizationReader struct {
	contextpack.SnapshotReader
	authorization inference.AuthPin
}

func (r testAuthorizationReader) ValidateAuthorization(
	_ context.Context,
	pin inference.AuthPin,
) error {
	if pin.Fingerprint() != r.authorization.Fingerprint() ||
		!pin.ExpiresAt().Equal(r.authorization.ExpiresAt()) {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"authorization pin does not match test reader",
		)
	}
	return nil
}

func newFixture(
	t *testing.T,
	sources []sourceFixture,
	includePath bool,
	policyID shoal.ID,
	requestID shoal.ID,
) fixture {
	t.Helper()
	path := filepath.Join(
		"testdata",
		fmt.Sprintf("response-%d-%d", os.Getpid(), fixtureSequence.Add(1)),
	)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := explorer.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close fixture: %v", err)
		}
		if err := os.RemoveAll(path); err != nil {
			t.Errorf("remove fixture: %v", err)
		}
	})

	views := make([]explorer.DocumentView, 0, len(sources))
	response := retrieval.Response{RequestID: requestID}
	for index, source := range sources {
		metadata := shoal.Metadata(nil)
		if source.visibility != "" {
			metadata = shoal.Metadata{
				interaction.PropertyVisibility: source.visibility,
			}
		}
		receipt, err := client.IngestWithOptions(
			context.Background(),
			explorer.Source{
				URI:       fmt.Sprintf("memory://citation/%d", index),
				Title:     fmt.Sprintf("Source %d", index),
				MediaType: explorer.MediaTypeMarkdown,
				Content:   source.content, Metadata: metadata,
			},
			explorer.IngestOptions{
				CreatedAt: time.Date(2026, 9, 5, 12, index, 0, 0, time.UTC),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		view, err := client.Document(
			context.Background(), receipt.Document.ID, receipt.Revision.ID)
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, view)
		span := firstSpan(t, view.Root)
		evidence := retrieval.Evidence{
			Citation: document.Citation{
				DocumentID: span.DocumentID, RevisionID: span.RevisionID,
				SectionID: span.SectionID, SpanID: span.ID, Range: span.Range,
			},
			Quote: span.Text,
			Score: 1,
		}
		if includePath && index == 0 {
			neighborhood, err := client.Neighborhood(
				context.Background(),
				explorer.NeighborhoodRequest{
					NodeIDs: []shoal.ID{span.ID}, Depth: 1,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range neighborhood.Nodes {
				if node.ID == span.ID {
					evidence.Path = graph.Path{Nodes: []graph.Node{node}}
					break
				}
			}
			if len(evidence.Path.Nodes) == 0 {
				t.Fatal("span graph node was not found")
			}
		}
		response.Results = append(response.Results, retrieval.Result{
			ID: receipt.Document.ID, Score: 1,
			Evidence: []retrieval.Evidence{evidence},
		})
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request, err := (retrieval.Request{
		Text: "grounded evidence", TopK: uint32(len(response.Results)),
		Modes: []retrieval.Mode{retrieval.ModeLexical},
		AsOf:  snapshot.AsOf,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	snapshotPin, err := inference.NewSnapshotPin(
		shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	authPin, err := inference.NewAuthPin(
		"\xfdauth", snapshot.AsOf.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := (contextpack.Builder{Reader: client}).Build(
		context.Background(), contextpack.InitialRequest{
			Request: request, Response: response, Documents: views,
			Pins: contextpack.Pins{
				Snapshot: snapshotPin, Authorization: authPin, PolicyID: policyID,
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	result := fixture{
		client: client, pack: pack, policyID: policyID,
		generatedAt: snapshot.AsOf.Add(time.Minute),
	}
	for _, anchor := range pack.Evidence() {
		switch anchor.Kind() {
		case inference.AnchorDocument:
			result.documentAnchors = append(result.documentAnchors, anchor)
		case inference.AnchorGraph:
			result.graphAnchor = anchor
		}
	}
	sort.Slice(result.documentAnchors, func(i, j int) bool {
		left, _, _ := result.documentAnchors[i].Document()
		right, _, _ := result.documentAnchors[j].Document()
		return shoal.CompareID(left.DocumentID, right.DocumentID) < 0
	})
	return result
}

func (f fixture) result(
	t *testing.T,
	evidenceIDs []shoal.ID,
	withUnsupported bool,
) inference.InferenceResult {
	t.Helper()
	model, prompt := provenance(t)
	value, err := ontology.NewStringValue("grounded result")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := inference.NewClaim(
		"subject", "predicate", value, 0.9, evidenceIDs,
		inference.ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	var issues []inference.Issue
	if withUnsupported {
		issue, err := inference.NewIssue(
			inference.IssueUnsupported,
			"unsupported request",
			"not supported by the selected model",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		issues = append(issues, issue)
	}
	result, err := inference.NewInferenceResult(
		f.pack, []inference.Claim{claim}, issues, f.generatedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f fixture) extendedResult(
	t *testing.T,
	addition inference.EvidenceAnchor,
) inference.InferenceResult {
	t.Helper()
	model, prompt := provenance(t)
	value, err := ontology.NewStringValue("generated result")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := inference.NewClaim(
		"subject", "predicate", value, 0.9,
		[]shoal.ID{addition.ID()}, inference.ClaimInferred,
		model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewExtendedInferenceResult(
		f.pack, []inference.EvidenceAnchor{addition},
		[]inference.Claim{claim}, nil, f.generatedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func provenance(
	t *testing.T,
) (inference.ModelProvenance, inference.PromptProvenance) {
	t.Helper()
	model, err := inference.NewModelProvenance(
		"test", "model", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := inference.NewPromptProvenance(
		"citation-test", "v1",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
	)
	if err != nil {
		t.Fatal(err)
	}
	return model, prompt
}

func firstSpan(t *testing.T, root explorer.SectionView) document.Span {
	t.Helper()
	for _, span := range root.Spans {
		if span.Text != "" {
			return span
		}
	}
	for _, child := range root.Children {
		if span, ok := findSpan(child); ok {
			return span
		}
	}
	t.Fatal("document has no nonempty span")
	return document.Span{}
}

func findSpan(root explorer.SectionView) (document.Span, bool) {
	for _, span := range root.Spans {
		if span.Text != "" {
			return span, true
		}
	}
	for _, child := range root.Children {
		if span, ok := findSpan(child); ok {
			return span, true
		}
	}
	return document.Span{}, false
}

type captureSink struct {
	ensureErr error
	recordErr error
	records   int
	session   interaction.Session
	enrich    func(interaction.Session) interaction.Session
}

func (s *captureSink) EnsureInteractionSink(context.Context) error {
	return s.ensureErr
}

func (s *captureSink) RecordInteraction(
	ctx context.Context,
	session interaction.Session,
) error {
	_, err := s.RecordInteractionResult(ctx, session)
	return err
}

func (s *captureSink) RecordInteractionResult(
	_ context.Context,
	session interaction.Session,
) (interaction.Session, error) {
	s.records++
	if s.recordErr != nil {
		return interaction.Session{}, s.recordErr
	}
	if s.enrich != nil {
		session = s.enrich(session)
	}
	s.session = session
	return session, nil
}

func capture(
	t *testing.T,
	prepared reasoning.PreparedResponse,
	sink *captureSink,
) reasoning.Response {
	t.Helper()
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return prepared.CaptureMetadata().GeneratedAt().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	response, err := prepared.Capture(
		context.Background(), recorder, captureSession(t, prepared))
	if err != nil {
		t.Fatal(err)
	}
	if sink.records != 1 {
		t.Fatalf("record count = %d", sink.records)
	}
	return response
}

func captureSession(
	t *testing.T,
	prepared reasoning.PreparedResponse,
) interaction.Session {
	t.Helper()
	metadata := prepared.CaptureMetadata()
	recordedAt := metadata.Snapshot().AsOf().Add(2 * time.Minute)
	session, err := metadata.NewSession(
		interaction.OperationChat, "citation-correlation", recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	if additions := metadata.AdditionEvidence(); len(additions) > 0 {
		session.Turns = []interaction.Turn{{
			Index: 0,
			ToolCall: &interaction.ToolCall{
				Kind:              "retrieve",
				RetrievedNodeIDs:  metadata.AdditionSourceIDs(),
				RetrievedEvidence: additions,
			},
		}}
	}
	return session
}

type recorderFunc func(
	context.Context, interaction.Session,
) (interaction.Session, error)

func (f recorderFunc) Record(
	ctx context.Context,
	session interaction.Session,
) (interaction.Session, error) {
	return f(ctx, session)
}

type hiddenReader struct {
	contextpack.AuthorizationReader
	hidden shoal.ID
}

func (r hiddenReader) Document(
	ctx context.Context,
	documentID shoal.ID,
	revisionID shoal.ID,
) (explorer.DocumentView, error) {
	if documentID == r.hidden {
		return explorer.DocumentView{}, shoal.NewError(
			shoal.ErrorNotFound, "document not found")
	}
	return r.AuthorizationReader.Document(ctx, documentID, revisionID)
}

type assertionReader struct {
	contextpack.AuthorizationReader
	assertion ontology.Assertion
}

func (r assertionReader) Neighborhood(
	ctx context.Context,
	request explorer.NeighborhoodRequest,
) (explorer.Neighborhood, error) {
	neighborhood, err := r.AuthorizationReader.Neighborhood(ctx, request)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	edgeID := shoal.ID(r.assertion.Metadata()["shoal.graph.edge_id"])
	for _, edge := range neighborhood.Edges {
		if edge.ID == edgeID {
			neighborhood.Assertions = append(
				neighborhood.Assertions, r.assertion)
			break
		}
	}
	return neighborhood, nil
}

func assertStrings(t *testing.T, actual, expected []string) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("strings = %v, want %v", actual, expected)
	}
	for index := range actual {
		if actual[index] != expected[index] {
			t.Fatalf("strings = %v, want %v", actual, expected)
		}
	}
}

func testContainsID(values []shoal.ID, target shoal.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
