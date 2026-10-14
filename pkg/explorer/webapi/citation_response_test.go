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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

var citationWireSequence atomic.Uint64

func TestCitationEnvelopeOpaqueIDRoundTrip(t *testing.T) {
	client, reader, pack, result, policyID := citationWireFixture(t)
	builder, err := reasoning.NewBuilder(reader)
	if err != nil {
		t.Fatal(err)
	}
	embeddingSpaces, err := interaction.NewEmbeddingSpaceSet(
		[]string{"model:v2", "model:v1"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(context.Background(), reasoning.BuildInput{
		ContextPack: pack, Result: result, EmbeddingSpaces: embeddingSpaces,
		Policy: reasoning.Policy{
			ID: policyID, ExtraOutputVisibility: []string{"api"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := prepared.CaptureMetadata()
	recordedAt := pack.Snapshot().AsOf().Add(2 * time.Minute)
	session, err := metadata.NewSession(
		interaction.OperationChat, shoal.ID("\xfbcorrelation"), recordedAt)
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
	summaries, err := client.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Visibility != "internal" {
		t.Fatalf("durable response visibility = %+v", summaries)
	}
	original := NewCitationEnvelope(response)
	if got := original.EffectiveVisibility; !reflect.DeepEqual(
		got, []string{"api", "internal"},
	) {
		t.Fatalf("response visibility = %v", got)
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("\ufffd")) {
		t.Fatalf("opaque ID was replaced during JSON encoding: %s", encoded)
	}
	var decoded CitationEnvelope
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string][2]shoal.ID{
		"policy":  {original.PolicyID, decoded.PolicyID},
		"request": {original.RequestID, decoded.RequestID},
		"authorization": {
			original.AuthorizationFingerprint,
			decoded.AuthorizationFingerprint,
		},
		"response": {original.ID, decoded.ID},
	} {
		if values[0] != values[1] {
			t.Fatalf("%s ID changed: %x != %x",
				name, []byte(values[0]), []byte(values[1]))
		}
	}
	if len(decoded.Claims) != 1 ||
		len(decoded.Claims[0].CitationAnchorIDs) != 1 ||
		decoded.Claims[0].CitationAnchorIDs[0] != decoded.Evidence[0].AnchorID ||
		decoded.Evidence[0].Status != reasoning.VerificationVerified {
		t.Fatal("verified citation status did not survive wire round trip")
	}
	if decoded.Evidence[0].SectionID !=
		decoded.Evidence[0].Citation.SectionID ||
		decoded.Evidence[0].SpanID != decoded.Evidence[0].Citation.SpanID {
		t.Fatal("resolved citation source roles did not survive wire round trip")
	}
	if len(decoded.RetrievedSourceIDs) != len(original.RetrievedSourceIDs) ||
		len(decoded.CitedSourceIDs) != len(original.CitedSourceIDs) {
		t.Fatal("wire round trip changed provenance cardinality")
	}
	if !reflect.DeepEqual(decoded.EmbeddingSpaces, embeddingSpaces) {
		t.Fatalf(
			"wire embedding spaces = %+v, want %+v",
			decoded.EmbeddingSpaces, embeddingSpaces,
		)
	}
	forgedEmbeddingDigest := bytes.Replace(
		encoded,
		[]byte(embeddingSpaces.Digest),
		[]byte(strings.Repeat("0", len(embeddingSpaces.Digest))),
		1,
	)
	if err := json.Unmarshal(
		forgedEmbeddingDigest, &CitationEnvelope{},
	); err == nil {
		t.Fatal("forged embedding-space digest was accepted")
	}
	encodedQuote, err := json.Marshal(original.Evidence[0].Quote)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(encoded, encodedQuote); got != 1 {
		t.Fatalf("verified quote appears %d times in envelope; want 1", got)
	}
	for name, entry := range map[string]map[string]any{
		"metadata key":   {"key": "_x", "value": "dg"},
		"metadata value": {"key": "aw", "value": "_x"},
	} {
		t.Run(name, func(t *testing.T) {
			var wire map[string]any
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			claims := wire["claims"].([]any)
			model := claims[0].(map[string]any)["model"].(map[string]any)
			model["parameters"] = []any{entry}
			invalid, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var rejected CitationEnvelope
			err = json.Unmarshal(invalid, &rejected)
			if err == nil {
				t.Fatal("full envelope accepted non-canonical metadata")
			}
			if !strings.Contains(err.Error(), "canonical unpadded base64url") {
				t.Fatalf("full envelope metadata error = %v", err)
			}
		})
	}
	var inactiveUnion map[string]any
	if err := json.Unmarshal(encoded, &inactiveUnion); err != nil {
		t.Fatal(err)
	}
	inactiveClaims := inactiveUnion["claims"].([]any)
	inactiveObject := inactiveClaims[0].(map[string]any)["object"].(map[string]any)
	inactiveObject["reference"] = "_x"
	inactiveJSON, err := json.Marshal(inactiveUnion)
	if err != nil {
		t.Fatal(err)
	}
	var inactiveRejected CitationEnvelope
	if err := json.Unmarshal(inactiveJSON, &inactiveRejected); err == nil {
		t.Fatal("full envelope accepted an inactive ontology union field")
	}

	duplicateClaim := original
	duplicateClaim.Claims = append(
		append([]CitationClaim(nil), original.Claims...), original.Claims[0])
	if _, err := json.Marshal(duplicateClaim); err == nil {
		t.Fatal("duplicate claim ID was accepted")
	}

	nativeIssue, err := inference.NewIssue(
		inference.IssueUnresolved,
		"input",
		"reason",
		[]shoal.ID{original.Evidence[0].AnchorID},
	)
	if err != nil {
		t.Fatal(err)
	}
	issue := CitationIssue{
		Kind:        reasoning.IssueUnverified,
		OutcomeType: reasoning.IssueOutcomeInferenceIssue,
		OutcomeID:   nativeIssue.ID(), Input: "input", Reason: "reason",
		EvidenceAnchorIDs: []shoal.ID{original.Evidence[0].AnchorID},
	}
	duplicateIssue := original
	duplicateIssue.Issues = []CitationIssue{issue, issue}
	if _, err := json.Marshal(duplicateIssue); err == nil {
		t.Fatal("duplicate issue outcome ID was accepted")
	}
	repeatedEvidence := original
	issue.EvidenceAnchorIDs = append(
		issue.EvidenceAnchorIDs, issue.EvidenceAnchorIDs[0])
	repeatedEvidence.Issues = []CitationIssue{issue}
	if _, err := json.Marshal(repeatedEvidence); err == nil {
		t.Fatal("duplicate issue evidence was accepted")
	}
	alteredIssue := original
	issue.OutcomeID = nativeIssue.ID()
	issue.EvidenceAnchorIDs = []shoal.ID{original.Evidence[0].AnchorID}
	issue.Reason = "altered reason"
	alteredIssue.Issues = []CitationIssue{issue}
	if _, err := json.Marshal(alteredIssue); err == nil {
		t.Fatal("issue payload changed without changing its outcome ID")
	}
	emptySource := original
	emptySource.Sources = append([]CitationSource(nil), original.Sources...)
	emptySource.Sources[0].AnchorIDs = nil
	if _, err := json.Marshal(emptySource); err == nil {
		t.Fatal("source without evidence anchors was accepted")
	}
	derivedCitation := original
	derivedCitation.Evidence = append(
		[]CitationEvidence(nil), original.Evidence...)
	derivedCitation.Evidence[0].Origin = reasoning.OriginDerived
	if _, err := json.Marshal(derivedCitation); err == nil {
		t.Fatal("derived-origin document citation was accepted")
	}
	unverifiedEvidence := original
	unverifiedEvidence.Evidence = append(
		[]CitationEvidence(nil), original.Evidence...)
	unverifiedEvidence.Evidence[0].Status =
		reasoning.VerificationUnverified
	if _, err := json.Marshal(unverifiedEvidence); err == nil {
		t.Fatal("unverified envelope evidence was accepted")
	}
	hiddenSource := original
	hiddenSource.Sources = append([]CitationSource(nil), original.Sources...)
	hiddenSource.Sources[0].Visibility = []string{"secret"}
	if _, err := json.Marshal(hiddenSource); err == nil {
		t.Fatal("source visibility omitted from effective output label")
	}
	underLabeledEvidence := original
	underLabeledEvidence.Evidence = append(
		[]CitationEvidence(nil), original.Evidence...)
	underLabeledEvidence.Evidence[0].Visibility = nil
	if _, err := json.Marshal(underLabeledEvidence); err == nil {
		t.Fatal("document evidence omitted source visibility")
	}
	mergedVisibility := original
	mergedVisibility.Sources = append(
		[]CitationSource(nil), original.Sources...)
	mergedVisibility.Sources[0].Visibility = []string{"internal", "secret"}
	mergedVisibility.Evidence = append(
		[]CitationEvidence(nil), original.Evidence...)
	mergedVisibility.Evidence[0].Visibility = []string{"internal", "secret"}
	mergedVisibility.EffectiveVisibility = []string{"api", "internal", "secret"}
	mergedVisibility.ID, err = reasoning.CanonicalResponseID(
		mergedVisibility.SessionID,
		mergedVisibility.RecordedAt,
		citationResponseIdentity(mergedVisibility),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(mergedVisibility); err != nil {
		t.Fatalf("valid merged document visibility was rejected: %v", err)
	}
	for name, omit := range map[string]func(*document.Citation){
		"section": func(citation *document.Citation) {
			citation.SectionID = ""
		},
		"span": func(citation *document.Citation) {
			citation.SpanID = ""
		},
	} {
		t.Run("omitted "+name+" identity", func(t *testing.T) {
			var candidate CitationEnvelope
			if err := json.Unmarshal(encoded, &candidate); err != nil {
				t.Fatal(err)
			}
			citation := *candidate.Evidence[0].Citation
			omit(&citation)
			candidate.Evidence[0].Citation = &citation
			reanchorCitationDocumentEnvelope(t, &candidate)
			payload, err := json.Marshal(candidate)
			if err != nil {
				t.Fatalf("omitted %s identity error = %v", name, err)
			}
			var roundTrip CitationEnvelope
			if err := json.Unmarshal(payload, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if roundTrip.Evidence[0].SectionID == "" ||
				roundTrip.Evidence[0].SpanID == "" {
				t.Fatalf(
					"omitted %s identity lost resolved roles: %+v",
					name, roundTrip.Evidence[0],
				)
			}
		})
	}
	tamperedRole := original
	tamperedRole.Evidence = append(
		[]CitationEvidence(nil), original.Evidence...)
	tamperedRole.Evidence[0].SpanID = "forged-span"
	if _, err := json.Marshal(tamperedRole); err == nil {
		t.Fatal("forged resolved citation source role was accepted")
	}
	clippedCitation := original
	clippedCitation.Evidence = append(
		[]CitationEvidence(nil), original.Evidence...)
	omitted := clippedCitation.Evidence[0].Citation.SpanID
	if omitted == "" {
		omitted = clippedCitation.Evidence[0].Citation.SectionID
	}
	clippedCitation.Evidence[0].SourceIDs = withoutCitationID(
		clippedCitation.Evidence[0].SourceIDs, omitted)
	clippedCitation.RetrievedSourceIDs = withoutCitationID(
		original.RetrievedSourceIDs, omitted)
	clippedCitation.CitedSourceIDs = withoutCitationID(
		original.CitedSourceIDs, omitted)
	clippedCitation.Sources = withoutCitationSource(
		original.Sources, omitted)
	if _, err := json.Marshal(clippedCitation); err == nil {
		t.Fatal("citation with clipped source identity was accepted")
	}

	for name, mutate := range map[string]func(*CitationEnvelope){
		"claims": func(value *CitationEnvelope) {
			value.Claims = make([]CitationClaim, inference.MaxClaims+1)
			value.Issues = nil
		},
		"issues": func(value *CitationEnvelope) {
			value.Claims = nil
			value.Issues = make([]CitationIssue, inference.MaxIssues+1)
			for index := range value.Issues {
				value.Issues[index].OutcomeType =
					reasoning.IssueOutcomeInferenceIssue
			}
		},
		"claims including demoted": func(value *CitationEnvelope) {
			value.Claims = make([]CitationClaim, inference.MaxClaims)
			value.Issues = []CitationIssue{{
				OutcomeType: reasoning.IssueOutcomeClaim,
			}}
		},
		"evidence": func(value *CitationEnvelope) {
			value.Evidence = make(
				[]CitationEvidence, inference.MaxEvidenceAnchors+1)
		},
	} {
		t.Run("cardinality "+name, func(t *testing.T) {
			invalid := original
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("citation envelope accepted impossible cardinality")
			}
		})
	}

	oversizedResult := original
	oversizedResult.Claims = nil
	largeValue, err := ontology.NewStringValue(
		strings.Repeat("v", 900*1024))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 20; index++ {
		claim := original.Claims[0]
		claim.Subject = shoal.ID(fmt.Sprintf("large-claim-%d", index))
		claim.Object = largeValue
		claim.ID, err = citationClaimCanonicalID(claim)
		if err != nil {
			t.Fatal(err)
		}
		oversizedResult.Claims = append(oversizedResult.Claims, claim)
	}
	if err := oversizedResult.Validate(); err == nil {
		t.Fatal("aggregate inference result byte bound was bypassed")
	}

	oversizedContext := original
	largeQuote := strings.Repeat("q", 60*1024)
	for index := 0; index < 140; index++ {
		documentID := shoal.ID(fmt.Sprintf("large-document-%d", index))
		revisionID := shoal.ID(fmt.Sprintf("large-revision-%d", index))
		sectionID := shoal.ID(fmt.Sprintf("large-section-%d", index))
		spanID := shoal.ID(fmt.Sprintf("large-span-%d", index))
		citation := document.Citation{
			DocumentID: documentID, RevisionID: revisionID,
			SectionID: sectionID, SpanID: spanID,
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0},
				End: document.SourcePosition{
					Offset: int64(len(largeQuote)),
				},
			},
		}
		anchor, err := inference.NewDocumentAnchor(citation, largeQuote)
		if err != nil {
			t.Fatal(err)
		}
		anchorID := anchor.ID()
		oversizedContext.Evidence = append(
			oversizedContext.Evidence,
			CitationEvidence{
				AnchorID: anchorID, SnapshotID: original.SnapshotID,
				SnapshotAsOf: original.SnapshotAsOf,
				Status:       reasoning.VerificationVerified,
				Use:          reasoning.EvidenceCited,
				Origin:       reasoning.OriginSource,
				SourceIDs:    []shoal.ID{documentID, sectionID, spanID},
				SectionID:    sectionID,
				SpanID:       spanID,
				Citation:     &citation,
				Quote:        largeQuote,
			},
		)
		for _, sourceID := range []shoal.ID{
			documentID, sectionID, spanID,
		} {
			oversizedContext.RetrievedSourceIDs = append(
				oversizedContext.RetrievedSourceIDs, sourceID)
			oversizedContext.Sources = append(
				oversizedContext.Sources,
				CitationSource{
					ID: sourceID, AnchorIDs: []shoal.ID{anchorID},
				},
			)
		}
	}
	if err := oversizedContext.Validate(); err == nil {
		t.Fatal("aggregate context evidence byte bound was bypassed")
	}

	for name, issue := range map[string]CitationIssue{
		"blank input": {
			Kind:        reasoning.IssueUnverified,
			OutcomeType: reasoning.IssueOutcomeInferenceIssue,
			OutcomeID:   "issue",
			Reason:      "reason",
		},
		"oversized input": {
			Kind:        reasoning.IssueUnverified,
			OutcomeType: reasoning.IssueOutcomeInferenceIssue,
			OutcomeID:   "issue",
			Input:       strings.Repeat("i", inference.MaxIssueInputBytes+1),
			Reason:      "reason",
		},
		"oversized reason": {
			Kind:        reasoning.IssueUnverified,
			OutcomeType: reasoning.IssueOutcomeInferenceIssue,
			OutcomeID:   "issue",
			Input:       "input",
			Reason:      strings.Repeat("r", inference.MaxIssueReasonBytes+1),
		},
		"too many references": {
			Kind:        reasoning.IssueUnverified,
			OutcomeType: reasoning.IssueOutcomeInferenceIssue,
			OutcomeID:   "issue",
			Input:       "input", Reason: "reason",
			EvidenceAnchorIDs: citationTestIDs(
				inference.MaxEvidenceRefsPerOutcome + 1),
		},
	} {
		t.Run("issue "+name, func(t *testing.T) {
			invalid := original
			invalid.Claims = nil
			invalid.CitedSourceIDs = nil
			invalid.Issues = []CitationIssue{issue}
			if err := invalid.Validate(); err == nil {
				t.Fatal("citation envelope accepted impossible issue")
			}
		})
	}

	graphPath := graph.Path{
		Nodes: []graph.Node{{ID: "left"}, {ID: "right"}},
		Edges: []graph.Edge{{
			ID: "edge", From: "left", To: "right",
			Type: "related", Weight: 1,
		}},
	}
	graphAnchor, err := inference.NewGraphAnchor(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	unverifiedDerived := CitationEvidence{
		AnchorID: graphAnchor.ID(), SnapshotID: original.SnapshotID,
		SnapshotAsOf: original.SnapshotAsOf,
		Status:       reasoning.VerificationUnverified,
		Use:          reasoning.EvidenceDerived,
		Origin:       reasoning.OriginDerived,
		SourceIDs:    []shoal.ID{"left", "right"},
		Path:         &graphPath,
	}
	claim := original.Claims[0]
	claim.DerivedEvidenceAnchorIDs = []shoal.ID{graphAnchor.ID()}
	model, err := inference.NewModelProvenance(
		claim.Model.Provider, claim.Model.Model, claim.Model.Version,
		claim.Model.Parameters, claim.Model.Seed)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := inference.NewPromptProvenance(
		claim.Prompt.TemplateID, claim.Prompt.Version, claim.Prompt.Hash)
	if err != nil {
		t.Fatal(err)
	}
	canonicalClaim, err := inference.NewClaim(
		claim.Subject, claim.Predicate, claim.Object, claim.Confidence,
		append(
			append([]shoal.ID(nil), claim.CitationAnchorIDs...),
			claim.DerivedEvidenceAnchorIDs...,
		),
		claim.Status, model, prompt, claim.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	claim.ID = canonicalClaim.ID()
	if _, err := validateCitationClaim(claim, map[shoal.ID]CitationEvidence{
		original.Evidence[0].AnchorID: original.Evidence[0],
		graphAnchor.ID():              unverifiedDerived,
	}); err == nil {
		t.Fatal("successful claim accepted unverified derived evidence")
	}

	for name, mutate := range map[string]func(*CitationEnvelope){
		"generation before snapshot": func(value *CitationEnvelope) {
			value.GeneratedAt = value.SnapshotAsOf.Add(-time.Nanosecond)
		},
		"recording before generation": func(value *CitationEnvelope) {
			value.RecordedAt = value.GeneratedAt.Add(-time.Nanosecond)
		},
		"generation at expiry": func(value *CitationEnvelope) {
			value.GeneratedAt = value.AuthorizationExpiresAt
		},
		"recording at expiry": func(value *CitationEnvelope) {
			value.RecordedAt = value.AuthorizationExpiresAt
		},
		"recording identity drift": func(value *CitationEnvelope) {
			value.RecordedAt = value.GeneratedAt.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := original
			mutate(&invalid)
			if _, err := json.Marshal(invalid); err == nil {
				t.Fatal("invalid citation chronology was accepted")
			}
		})
	}
}

func TestCitationMetadataRequiresCanonicalBase64URL(t *testing.T) {
	decoded, err := citationMetadataValue(wireMetadata{{
		Key: "aw", Value: "dg",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if decoded["k"] != "v" {
		t.Fatalf("decoded metadata = %+v", decoded)
	}
	for name, metadata := range map[string]wireMetadata{
		"key":   {{Key: "_x", Value: "dg"}},
		"value": {{Key: "aw", Value: "_x"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := citationMetadataValue(metadata); err == nil {
				t.Fatal("non-canonical metadata encoding was accepted")
			}
		})
	}
}

func TestCitationEnvelopeRejectsOversizedProviderInput(t *testing.T) {
	var envelope CitationEnvelope
	if err := envelope.UnmarshalJSON(
		make([]byte, MaxCitationEnvelopeBytes+1)); err == nil {
		t.Fatal("oversized citation envelope input was accepted")
	}
}

func TestCitationEnvelopeRejectsDuplicateJSONKeysAtAnyDepth(t *testing.T) {
	for name, data := range map[string][]byte{
		"top-level": []byte(`{"id":"YQ","id":"YQ"}`),
		"top-level alias": []byte(
			`{"id":"YQ","ID":"YQ"}`),
		"evidence alias": []byte(
			`{"evidence":[{"anchor_id":"YQ","Anchor_ID":"YQ"}]}`),
		"citation alias": []byte(
			`{"evidence":[{"citation":{"document_id":"YQ","Document_ID":"YQ"}}]}`),
		"path node alias": []byte(
			`{"evidence":[{"path":{"nodes":[{"id":"YQ","ID":"YQ"}]}}]}`),
		"path edge alias": []byte(
			`{"evidence":[{"path":{"edges":[{"from":"YQ","From":"YQ"}]}}]}`),
		"assertion alias": []byte(
			`{"evidence":[{"assertions":[{"assertion_id":"YQ","Assertion_ID":"YQ"}]}]}`),
		"ontology value alias": []byte(
			`{"claims":[{"object":{"reference":"YQ","Reference":"YQ"}}]}`),
		"unicode simple-fold alias": []byte(
			`{"claims":[{"status":"inferred","\u017ftatus":"inferred"}]}`),
		"array": []byte(`[{"id":"YQ","id":"YQ"}]`),
	} {
		t.Run(name, func(t *testing.T) {
			var envelope CitationEnvelope
			if err := json.Unmarshal(data, &envelope); err == nil {
				t.Fatal("duplicate JSON key was accepted")
			}
		})
	}
}

func TestCitationDuplicateScannerPreservesDynamicMapKeyCase(t *testing.T) {
	type dynamicEnvelope struct {
		Metadata map[string]int `json:"metadata"`
	}
	data := []byte(`{"metadata":{"Key":1,"key":2}}`)
	if err := rejectDuplicateJSONKeys(
		data, reflect.TypeOf(dynamicEnvelope{})); err != nil {
		t.Fatalf("case-distinct dynamic map keys were rejected: %v", err)
	}
	var decoded dynamicEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Metadata) != 2 {
		t.Fatalf("dynamic metadata = %+v", decoded.Metadata)
	}
}

func TestCitationIDDecoderRequiresCanonicalBase64URL(t *testing.T) {
	id, err := decodeCitationID("_w")
	if err != nil || id != shoal.ID("\xff") {
		t.Fatalf("canonical opaque ID = %x, %v", []byte(id), err)
	}
	for _, encoded := range []string{"_x", "_w\r\n", "_w=="} {
		if _, err := decodeCitationID(encoded); err == nil {
			t.Fatalf("noncanonical ID %q was accepted", encoded)
		}
	}

	for name, mutate := range map[string]func(*wireCitation){
		"document": func(value *wireCitation) { value.DocumentID = "_x" },
		"revision": func(value *wireCitation) { value.RevisionID = "_x" },
		"section":  func(value *wireCitation) { value.SectionID = "_x" },
		"span":     func(value *wireCitation) { value.SpanID = "_x" },
	} {
		t.Run("citation "+name, func(t *testing.T) {
			value := wireCitation{
				DocumentID: "YQ", RevisionID: "Yg",
				SectionID: "Yw", SpanID: "ZA",
			}
			mutate(&value)
			if _, err := citationValueStrict(value); err == nil {
				t.Fatal("noncanonical nested citation ID was accepted")
			}
		})
	}
	for name, path := range map[string]wirePath{
		"node": {Nodes: []wireNode{{ID: "_x"}}},
		"edge": {
			Nodes: []wireNode{{ID: "YQ"}, {ID: "Yg"}},
			Edges: []wireEdge{{ID: "_x", From: "YQ", To: "Yg", Type: "edge"}},
		},
		"from": {
			Nodes: []wireNode{{ID: "YQ"}, {ID: "Yg"}},
			Edges: []wireEdge{{ID: "Yw", From: "_x", To: "Yg", Type: "edge"}},
		},
		"to": {
			Nodes: []wireNode{{ID: "YQ"}, {ID: "Yg"}},
			Edges: []wireEdge{{ID: "Yw", From: "YQ", To: "_x", Type: "edge"}},
		},
	} {
		t.Run("path "+name, func(t *testing.T) {
			if _, err := pathValueStrict(path); err == nil {
				t.Fatal("noncanonical nested path ID was accepted")
			}
		})
	}
	nonCanonicalReference := "_x"
	if _, err := citationOntologyValue(wireCitationOntologyValue{
		Type: ontology.ValueReference, Reference: &nonCanonicalReference,
	}); err == nil {
		t.Fatal("noncanonical reference value ID was accepted")
	}
	text := "value"
	if _, err := citationOntologyValue(wireCitationOntologyValue{
		Type: ontology.ValueString, Text: &text,
		Reference: &nonCanonicalReference,
	}); err == nil {
		t.Fatal("inactive ontology union field was accepted")
	}
}

func TestCitationEnvelopeAssertionReferencesRoundTrip(t *testing.T) {
	path := graph.Path{
		Nodes: []graph.Node{{ID: "left"}, {ID: "right"}},
		Edges: []graph.Edge{{
			ID: "edge", From: "left", To: "right",
			Type: "related", Weight: 1,
			Properties: shoal.Metadata{
				ontologyRelationshipIDProperty:  "relationship",
				ontologyAssertionIDProperty:     "assertion",
				ontologyAssertionOriginProperty: string(ontology.AssertionInferred),
			},
		}},
	}
	assertionReferences := []interaction.AssertionReference{{
		AssertionID: "assertion", EdgeID: "edge",
		Origin: ontology.AssertionInferred,
	}}
	anchor, err := inference.NewGraphAnchorWithAssertions(
		path, assertionReferences)
	if err != nil {
		t.Fatal(err)
	}
	asOf := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	evidence := CitationEvidence{
		AnchorID: anchor.ID(), SnapshotID: "snapshot", SnapshotAsOf: asOf,
		Status: reasoning.VerificationVerified, Use: reasoning.EvidenceDerived,
		Origin: reasoning.OriginDerived, SourceIDs: []shoal.ID{"left", "right"},
		Path: &path,
		Assertions: []CitationAssertion{{
			AssertionID: "assertion", EdgeID: "edge",
			Origin: ontology.AssertionInferred,
		}},
	}
	model, err := inference.NewModelProvenance(
		"test", "model", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := inference.NewPromptProvenance(
		"test", "v1",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("derived answer")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := inference.NewClaim(
		"subject", "predicate", value, 1, []shoal.ID{anchor.ID()},
		inference.ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimPayload := citationClaimValue(claim)
	claimPayload.DerivedEvidenceAnchorIDs = []shoal.ID{anchor.ID()}
	envelope := CitationEnvelope{
		SessionID: "session", RecordedAt: asOf.Add(2 * time.Minute),
		ContextPackID: "pack", ResultID: "result", PolicyID: "policy",
		SnapshotID: "snapshot", SnapshotAsOf: asOf,
		AuthorizationFingerprint: "auth",
		AuthorizationExpiresAt:   asOf.Add(time.Hour),
		GeneratedAt:              asOf.Add(time.Minute),
		RetrievedSourceIDs:       []shoal.ID{"left", "right"},
		Sources: []CitationSource{
			{ID: "left", AnchorIDs: []shoal.ID{anchor.ID()}},
			{ID: "right", AnchorIDs: []shoal.ID{anchor.ID()}},
		},
		Evidence: []CitationEvidence{evidence},
		Issues: []CitationIssue{{
			Kind:        reasoning.IssueUnverified,
			OutcomeType: reasoning.IssueOutcomeClaim,
			OutcomeID:   claim.ID(), Input: string(claim.ID()),
			Reason:            reasoning.UnverifiedClaimReason,
			EvidenceAnchorIDs: []shoal.ID{evidence.AnchorID},
			Claim:             &claimPayload,
		}},
	}
	envelope.ID, err = reasoning.CanonicalResponseID(
		envelope.SessionID,
		envelope.RecordedAt,
		citationResponseIdentity(envelope),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CitationEnvelope
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Evidence) != 1 ||
		len(decoded.Evidence[0].Assertions) != 1 ||
		decoded.Evidence[0].Assertions[0].AssertionID != "assertion" ||
		decoded.Evidence[0].Assertions[0].EdgeID != "edge" ||
		decoded.Evidence[0].Assertions[0].Origin != ontology.AssertionInferred {
		t.Fatalf("assertion references = %+v", decoded.Evidence)
	}
	if decoded.Issues[0].Claim == nil ||
		decoded.Issues[0].Claim.ID != claim.ID() {
		t.Fatal("unverified claim payload did not survive round trip")
	}
	tamperedClaim := decoded
	tamperedClaim.Issues = append(
		[]CitationIssue(nil), decoded.Issues...)
	claimCopy := *decoded.Issues[0].Claim
	claimCopy.Subject = "different-subject"
	tamperedClaim.Issues[0].Claim = &claimCopy
	if _, err := json.Marshal(tamperedClaim); err == nil {
		t.Fatal("unverified claim payload changed without changing outcome ID")
	}
	clipped := envelope
	clipped.RetrievedSourceIDs = []shoal.ID{"left"}
	clipped.Sources = append([]CitationSource(nil), envelope.Sources[:1]...)
	clipped.Evidence = append([]CitationEvidence(nil), envelope.Evidence...)
	clipped.Evidence[0].SourceIDs = []shoal.ID{"left"}
	if _, err := json.Marshal(clipped); err == nil {
		t.Fatal("graph path with clipped node provenance was accepted")
	}

	missingAssertion := decoded
	missingAssertion.Evidence = append(
		[]CitationEvidence(nil), decoded.Evidence...)
	missingAssertion.Evidence[0].Assertions = nil
	if _, err := json.Marshal(missingAssertion); err == nil {
		t.Fatal("assertion-marked edge without reference was accepted")
	}
	duplicateEdgeAssertion := decoded
	duplicateEdgeAssertion.Evidence = append(
		[]CitationEvidence(nil), decoded.Evidence...)
	duplicateEdgeAssertion.Evidence[0].Assertions = append(
		append(
			[]CitationAssertion(nil),
			decoded.Evidence[0].Assertions...,
		),
		CitationAssertion{
			AssertionID: "second-assertion", EdgeID: "edge",
			Origin: ontology.AssertionInferred,
		},
	)
	if _, err := json.Marshal(duplicateEdgeAssertion); err == nil {
		t.Fatal("multiple assertion references for one edge were accepted")
	}

	for name, mutate := range map[string]func(*CitationEnvelope){
		"stripped visibility": func(value *CitationEnvelope) {
			value.Evidence[0].Path.Nodes[0].Properties = shoal.Metadata{
				interaction.PropertyVisibility: "secret",
			}
		},
		"interaction node": func(value *CitationEnvelope) {
			value.Evidence[0].Path.Nodes[0].Kind = interaction.KindSession
		},
		"interaction identity": func(value *CitationEnvelope) {
			oldID := value.Evidence[0].Path.Nodes[0].ID
			newID := shoal.ID(interaction.KindSession + "_forged")
			value.Evidence[0].Path.Nodes[0].ID = newID
			value.Evidence[0].Path.Edges[0].From = newID
			value.Evidence[0].SourceIDs = replaceCitationID(
				value.Evidence[0].SourceIDs, oldID, newID)
			value.RetrievedSourceIDs = replaceCitationID(
				value.RetrievedSourceIDs, oldID, newID)
			for index := range value.Sources {
				if value.Sources[index].ID == oldID {
					value.Sources[index].ID = newID
				}
			}
		},
		"source-labeled provenance": func(value *CitationEnvelope) {
			value.Evidence[0].Path.Nodes[0].Kind = graph.NodeKindProducer
			value.Evidence[0].Path.Edges[0].Properties[ontologyAssertionOriginProperty] =
				string(ontology.AssertionExplicit)
			value.Evidence[0].Assertions[0].Origin =
				ontology.AssertionExplicit
			value.Evidence[0].Origin = reasoning.OriginSource
		},
		"mismatched assertion origin": func(value *CitationEnvelope) {
			value.Evidence[0].Path.Edges[0].Properties[ontologyAssertionOriginProperty] =
				string(ontology.AssertionExplicit)
		},
		"mismatched assertion identity": func(value *CitationEnvelope) {
			value.Evidence[0].Path.Edges[0].Properties[ontologyAssertionIDProperty] =
				"different-assertion"
		},
	} {
		t.Run(name, func(t *testing.T) {
			var invalid CitationEnvelope
			if err := json.Unmarshal(encoded, &invalid); err != nil {
				t.Fatal(err)
			}
			mutate(&invalid)
			validationErr := reanchorCitationGraphEnvelope(t, &invalid)
			if validationErr == nil {
				_, validationErr = json.Marshal(invalid)
			}
			if validationErr == nil {
				t.Fatal("invalid graph citation semantics were accepted")
			}
		})
	}
}

func TestCitationEvidenceAllowsAssertionAcrossMultipleEdges(t *testing.T) {
	path := graph.Path{
		Nodes: []graph.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Edges: []graph.Edge{
			{ID: "edge-a", From: "a", To: "b", Type: "related", Weight: 1},
			{ID: "edge-b", From: "b", To: "c", Type: "related", Weight: 1},
		},
	}
	references := []interaction.AssertionReference{
		{AssertionID: "assertion", EdgeID: "edge-a", Origin: ontology.AssertionExplicit},
		{AssertionID: "assertion", EdgeID: "edge-b", Origin: ontology.AssertionExplicit},
	}
	anchor, err := inference.NewGraphAnchorWithAssertions(path, references)
	if err != nil {
		t.Fatal(err)
	}
	evidence := CitationEvidence{
		AnchorID: anchor.ID(), SnapshotID: "snapshot",
		SnapshotAsOf: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
		Status:       reasoning.VerificationVerified,
		Use:          reasoning.EvidenceDerived, Origin: reasoning.OriginSource,
		SourceIDs: []shoal.ID{"a", "b", "c"}, Path: &path,
		Assertions: []CitationAssertion{
			{AssertionID: "assertion", EdgeID: "edge-a", Origin: ontology.AssertionExplicit},
			{AssertionID: "assertion", EdgeID: "edge-b", Origin: ontology.AssertionExplicit},
		},
	}
	if err := validateCitationEvidence(evidence); err != nil {
		t.Fatalf("multi-edge assertion evidence rejected: %v", err)
	}
}

func reanchorCitationGraphEnvelope(
	t *testing.T,
	envelope *CitationEnvelope,
) error {
	t.Helper()
	oldAnchorID := envelope.Evidence[0].AnchorID
	assertions := make(
		[]interaction.AssertionReference,
		len(envelope.Evidence[0].Assertions),
	)
	for index, assertion := range envelope.Evidence[0].Assertions {
		assertions[index] = interaction.AssertionReference{
			AssertionID: assertion.AssertionID,
			EdgeID:      assertion.EdgeID, Origin: assertion.Origin,
		}
	}
	anchor, err := inference.NewGraphAnchorWithAssertions(
		*envelope.Evidence[0].Path, assertions)
	if err != nil {
		t.Fatal(err)
	}
	newAnchorID := anchor.ID()
	envelope.Evidence[0].AnchorID = newAnchorID
	for sourceIndex := range envelope.Sources {
		for anchorIndex, anchorID := range envelope.Sources[sourceIndex].AnchorIDs {
			if anchorID == oldAnchorID {
				envelope.Sources[sourceIndex].AnchorIDs[anchorIndex] = newAnchorID
			}
		}
	}
	for claimIndex := range envelope.Claims {
		for anchorIndex, anchorID := range envelope.Claims[claimIndex].CitationAnchorIDs {
			if anchorID == oldAnchorID {
				envelope.Claims[claimIndex].CitationAnchorIDs[anchorIndex] = newAnchorID
			}
		}
		for anchorIndex, anchorID := range envelope.Claims[claimIndex].DerivedEvidenceAnchorIDs {
			if anchorID == oldAnchorID {
				envelope.Claims[claimIndex].DerivedEvidenceAnchorIDs[anchorIndex] =
					newAnchorID
			}
		}
	}
	for issueIndex := range envelope.Issues {
		for anchorIndex, anchorID := range envelope.Issues[issueIndex].EvidenceAnchorIDs {
			if anchorID == oldAnchorID {
				envelope.Issues[issueIndex].EvidenceAnchorIDs[anchorIndex] =
					newAnchorID
			}
		}
		if envelope.Issues[issueIndex].Claim != nil {
			claim := envelope.Issues[issueIndex].Claim
			for anchorIndex, anchorID := range claim.CitationAnchorIDs {
				if anchorID == oldAnchorID {
					claim.CitationAnchorIDs[anchorIndex] = newAnchorID
				}
			}
			for anchorIndex, anchorID := range claim.DerivedEvidenceAnchorIDs {
				if anchorID == oldAnchorID {
					claim.DerivedEvidenceAnchorIDs[anchorIndex] = newAnchorID
				}
			}
			canonicalID, err := citationClaimCanonicalID(*claim)
			if err != nil {
				t.Fatal(err)
			}
			claim.ID = canonicalID
			envelope.Issues[issueIndex].OutcomeID = canonicalID
			envelope.Issues[issueIndex].Input = string(canonicalID)
		}
	}
	envelope.ID, err = reasoning.CanonicalResponseID(
		envelope.SessionID,
		envelope.RecordedAt,
		citationResponseIdentity(*envelope),
	)
	return err
}

func reanchorCitationDocumentEnvelope(
	t *testing.T,
	envelope *CitationEnvelope,
) {
	t.Helper()
	oldAnchorID := envelope.Evidence[0].AnchorID
	anchor, err := inference.NewDocumentAnchor(
		*envelope.Evidence[0].Citation,
		envelope.Evidence[0].Quote,
	)
	if err != nil {
		t.Fatal(err)
	}
	newAnchorID := anchor.ID()
	envelope.Evidence[0].AnchorID = newAnchorID
	for sourceIndex := range envelope.Sources {
		for anchorIndex, anchorID := range envelope.Sources[sourceIndex].AnchorIDs {
			if anchorID == oldAnchorID {
				envelope.Sources[sourceIndex].AnchorIDs[anchorIndex] = newAnchorID
			}
		}
	}
	for claimIndex := range envelope.Claims {
		for anchorIndex, anchorID := range envelope.Claims[claimIndex].CitationAnchorIDs {
			if anchorID == oldAnchorID {
				envelope.Claims[claimIndex].CitationAnchorIDs[anchorIndex] =
					newAnchorID
			}
		}
		canonicalID, err := citationClaimCanonicalID(
			envelope.Claims[claimIndex])
		if err != nil {
			t.Fatal(err)
		}
		envelope.Claims[claimIndex].ID = canonicalID
	}
	envelope.ID, err = reasoning.CanonicalResponseID(
		envelope.SessionID,
		envelope.RecordedAt,
		citationResponseIdentity(*envelope),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func citationClaimCanonicalID(claim CitationClaim) (shoal.ID, error) {
	model, err := inference.NewModelProvenance(
		claim.Model.Provider, claim.Model.Model, claim.Model.Version,
		claim.Model.Parameters, claim.Model.Seed)
	if err != nil {
		return "", err
	}
	prompt, err := inference.NewPromptProvenance(
		claim.Prompt.TemplateID, claim.Prompt.Version, claim.Prompt.Hash)
	if err != nil {
		return "", err
	}
	canonical, err := inference.NewClaim(
		claim.Subject, claim.Predicate, claim.Object, claim.Confidence,
		append(
			append([]shoal.ID(nil), claim.CitationAnchorIDs...),
			claim.DerivedEvidenceAnchorIDs...,
		),
		claim.Status, model, prompt, claim.Metadata)
	if err != nil {
		return "", err
	}
	return canonical.ID(), nil
}

func replaceCitationID(
	values []shoal.ID,
	oldID shoal.ID,
	newID shoal.ID,
) []shoal.ID {
	result := append([]shoal.ID(nil), values...)
	for index, value := range result {
		if value == oldID {
			result[index] = newID
		}
	}
	return result
}

func withoutCitationID(values []shoal.ID, omitted shoal.ID) []shoal.ID {
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		if value != omitted {
			result = append(result, value)
		}
	}
	return result
}

func withoutCitationSource(
	values []CitationSource,
	omitted shoal.ID,
) []CitationSource {
	result := make([]CitationSource, 0, len(values))
	for _, value := range values {
		if value.ID != omitted {
			result = append(result, value)
		}
	}
	return result
}

func citationTestIDs(count int) []shoal.ID {
	result := make([]shoal.ID, count)
	for index := range result {
		result[index] = shoal.ID(fmt.Sprintf("citation-test-%d", index))
	}
	return result
}

func citationWireFixture(
	t *testing.T,
) (
	*explorer.Explorer,
	contextpack.AuthorizationReader,
	inference.ContextPack,
	inference.InferenceResult,
	shoal.ID,
) {
	t.Helper()
	path := filepath.Join(
		"testdata",
		fmt.Sprintf("citation-wire-%d-%d", os.Getpid(), citationWireSequence.Add(1)),
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
	receipt, err := client.IngestWithOptions(
		context.Background(),
		explorer.Source{
			URI: "memory://citation-wire", Title: "Citation wire",
			MediaType: explorer.MediaTypeMarkdown,
			Content:   "# Wire\n\nOpaque identifiers survive transport.\n",
			Metadata: shoal.Metadata{
				interaction.PropertyVisibility: "internal",
			},
		},
		explorer.IngestOptions{
			CreatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
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
	span := firstCitationWireSpan(t, view.Root)
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request, err := (retrieval.Request{
		Text: "opaque identifiers", TopK: 1,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
		AsOf:  snapshot.AsOf,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	response := retrieval.Response{
		RequestID: "\xferequest",
		Results: []retrieval.Result{{
			ID: receipt.Document.ID, Score: 1,
			Evidence: []retrieval.Evidence{{
				Citation: document.Citation{
					DocumentID: span.DocumentID, RevisionID: span.RevisionID,
					SectionID: span.SectionID, SpanID: span.ID, Range: span.Range,
				},
				Quote: span.Text, Score: 1,
			}},
		}},
	}
	snapshotPin, err := inference.NewSnapshotPin(
		shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	authPin, err := inference.NewAuthPin(
		"\xfdauthorization", snapshot.AsOf.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	policyID := shoal.ID("\xffpolicy")
	pack, err := (contextpack.Builder{Reader: client}).Build(
		context.Background(), contextpack.InitialRequest{
			Request: request, Response: response,
			Documents: []explorer.DocumentView{view},
			Pins: contextpack.Pins{
				Snapshot: snapshotPin, Authorization: authPin, PolicyID: policyID,
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	model, err := inference.NewModelProvenance(
		"test", "model", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := inference.NewPromptProvenance(
		"wire", "v1",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("answer")
	if err != nil {
		t.Fatal(err)
	}
	anchor := pack.Evidence()[0]
	claim, err := inference.NewClaim(
		"\xf9subject", "\xf8predicate", value, 1,
		[]shoal.ID{anchor.ID()}, inference.ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewInferenceResult(
		pack, []inference.Claim{claim}, nil,
		snapshot.AsOf.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := citationAuthorizationReader{
		SnapshotReader: client,
		authorization:  authPin,
	}
	return client, reader, pack, result, policyID
}

type citationAuthorizationReader struct {
	contextpack.SnapshotReader
	authorization inference.AuthPin
}

func (r citationAuthorizationReader) ValidateAuthorization(
	_ context.Context,
	pin inference.AuthPin,
) error {
	if pin.Fingerprint() != r.authorization.Fingerprint() ||
		!pin.ExpiresAt().Equal(r.authorization.ExpiresAt()) {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"authorization pin does not match citation fixture",
		)
	}
	return nil
}

func firstCitationWireSpan(
	t *testing.T,
	root explorer.SectionView,
) document.Span {
	t.Helper()
	for _, span := range root.Spans {
		if span.Text != "" {
			return span
		}
	}
	for _, child := range root.Children {
		if span, ok := findCitationWireSpan(child); ok {
			return span
		}
	}
	t.Fatal("document has no nonempty span")
	return document.Span{}
}

func findCitationWireSpan(
	root explorer.SectionView,
) (document.Span, bool) {
	for _, span := range root.Spans {
		if span.Text != "" {
			return span, true
		}
	}
	for _, child := range root.Children {
		if span, ok := findCitationWireSpan(child); ok {
			return span, true
		}
	}
	return document.Span{}, false
}
