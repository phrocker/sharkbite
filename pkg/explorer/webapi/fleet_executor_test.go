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
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestAskExecutorCeilingFollowsTheConfiguredProvider is the acceptance
// criterion #385 exists for: the same executor's ceiling differs between a
// loopback provider and a hosted one, because egress is configuration rather
// than code.
//
// Before this, the executor declared nothing at all — the two-class taxonomy
// split on mutation and could not express transmission, so no honest
// declaration existed. Handing corpus passages to a hosted model was classified
// as the most benign thing available.
func TestAskExecutorCeilingFollowsTheConfiguredProvider(t *testing.T) {
	for _, probe := range []struct {
		name     string
		provider AskProvider
		want     fleet.Effects
	}{
		{
			"loopback provider transmits nothing",
			&stubAskProvider{egresses: false},
			fleet.Effects{fleet.EffectReadsCorpus},
		},
		{
			"hosted provider transmits corpus content",
			&stubAskProvider{egresses: true},
			fleet.Effects{fleet.EffectEgressesContent, fleet.EffectReadsCorpus},
		},
		{
			// Silence is not evidence that content stays put.
			"a provider that cannot classify itself",
			unclassifiableAskProvider{},
			fleet.Effects{fleet.EffectEgressesContent, fleet.EffectReadsCorpus},
		},
	} {
		executor, err := NewAskExecutor(AskExecutorConfig{
			Provider: probe.provider, Capability: "explorer.reason", Action: "ask",
		})
		if err != nil {
			t.Fatalf("%s: %v", probe.name, err)
		}
		got := executor.MaxEffects()
		if len(got) != len(probe.want) {
			t.Fatalf("%s: ceiling = %v, want %v", probe.name, got, probe.want)
		}
		for i := range got {
			if got[i] != probe.want[i] {
				t.Fatalf("%s: ceiling = %v, want %v", probe.name, got, probe.want)
			}
		}
		// The floor equals the ceiling. For this executor the bound is not a
		// permission envelope, it is a description: every invocation reads the
		// corpus, and every invocation against a hosted provider transmits what
		// it read. Declaring only the ceiling would let an action declare less
		// than the truth and still resolve.
		floor := executor.MinEffects()
		if len(floor) != len(got) {
			t.Fatalf("%s: floor = %v, ceiling = %v; they must match",
				probe.name, floor, got)
		}
		for i := range floor {
			if floor[i] != got[i] {
				t.Fatalf("%s: floor = %v, ceiling = %v; they must match",
					probe.name, floor, got)
			}
		}
		// It never declares external mutation, whatever the provider: declaring
		// it would raise the ceiling enough for genuinely external actions to
		// resolve here.
		for _, effect := range got {
			if effect == fleet.EffectMutatesExternal {
				t.Fatalf("%s: the reasoning executor declared external mutation", probe.name)
			}
		}
	}
}

// unclassifiableAskProvider implements no egress reporting, so it is the
// fail-closed case.
type unclassifiableAskProvider struct{}

func (unclassifiableAskProvider) Ask(context.Context, AskRequest) (CitationEnvelope, error) {
	return CitationEnvelope{}, nil
}

// stubAskProvider stands in for the authorized reasoning service so the
// executor's own translation and guard behavior can be exercised in isolation.
// The end-to-end acceptance test in cmd/shoal-explore-web uses the real
// ChatService against a real corpus instead.
type stubAskProvider struct {
	envelope CitationEnvelope
	err      error
	calls    int
	last     AskRequest
	egresses bool
}

// EgressesOffHost makes this stub classifiable, the way ChatService is through
// its configured generator.
func (p *stubAskProvider) EgressesOffHost() bool { return p.egresses }

func (p *stubAskProvider) Ask(
	_ context.Context, request AskRequest,
) (CitationEnvelope, error) {
	p.calls++
	p.last = request
	if p.err != nil {
		return CitationEnvelope{}, p.err
	}
	return p.envelope, nil
}

// validCitationEvidence builds one self-consistent document anchor: the quote
// length matches the citation range, the source roles match the citation, and
// the anchor is the canonical content-derived identity. The executor runs
// CitationEnvelope.Validate, which enforces all three, so an envelope that cut
// corners would only prove the check fires.
func validCitationEvidence(
	t *testing.T, name, quote string, visibility []string,
	snapshotID shoal.ID, asOf time.Time,
) CitationEvidence {
	t.Helper()
	documentID := shoal.ID(name + "-document")
	sectionID := shoal.ID(name + "-section")
	spanID := shoal.ID(name + "-span")
	citation := document.Citation{
		DocumentID: documentID, RevisionID: shoal.ID(name + "-revision"),
		SectionID: sectionID, SpanID: spanID,
		Range: document.SourceRange{
			End: document.SourcePosition{Offset: int64(len(quote))},
		},
	}
	anchor, err := inference.NewDocumentAnchor(citation, quote)
	if err != nil {
		t.Fatal(err)
	}
	return CitationEvidence{
		AnchorID: anchor.ID(), Status: reasoning.VerificationVerified,
		Use: reasoning.EvidenceCited, Origin: reasoning.OriginSource,
		SnapshotID: snapshotID, SnapshotAsOf: asOf,
		SourceIDs: []shoal.ID{documentID, sectionID, spanID},
		SectionID: sectionID, SpanID: spanID,
		Citation: &citation, Quote: quote, Visibility: visibility,
	}
}

// verifiedAskEnvelope builds an envelope that satisfies CitationEnvelope
// .Validate, including the canonical response identity the validator
// recomputes.
func verifiedAskEnvelope(t *testing.T, asOf time.Time) CitationEnvelope {
	t.Helper()
	evidence := validCitationEvidence(
		t, "primary", "a cited passage", nil, "snapshot", asOf)
	const issueInput = "what gates promotion?"
	canonicalIssue, err := inference.NewIssue(
		inference.IssueUnsupported, issueInput,
		reasoning.UnverifiedClaimReason, nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope := CitationEnvelope{
		Finalized:       true,
		DurablyRecorded: true,
		Verification:    reasoning.VerificationVerified,
		// The executor applies validateFinalizedChatResponse too, so the
		// envelope carries a matching output visibility, a consistent
		// workspace settings identity, and a resolved ontology interpretation.
		OutputVisibility:         "public",
		OntologyInterpretation:   &OntologyInterpretation{Status: "unresolved"},
		SessionID:                "session",
		ContextPackID:            "context-pack",
		ResultID:                 "result",
		PolicyID:                 "policy",
		RequestID:                "request",
		RecordedAt:               asOf,
		GeneratedAt:              asOf,
		AuthorizationFingerprint: "fingerprint",
		AuthorizationExpiresAt:   asOf.Add(2 * time.Hour),
		SnapshotID:               "snapshot",
		SnapshotAsOf:             asOf,
		Sources: []CitationSource{
			{ID: "primary-document", AnchorIDs: []shoal.ID{evidence.AnchorID}},
			{ID: "primary-section", AnchorIDs: []shoal.ID{evidence.AnchorID}},
			{ID: "primary-span", AnchorIDs: []shoal.ID{evidence.AnchorID}},
		},
		RetrievedSourceIDs: []shoal.ID{
			"primary-document", "primary-section", "primary-span",
		},
		Evidence: []CitationEvidence{evidence},
		// A response must carry at least one outcome. An issue keeps the
		// default envelope free of claims, so a test that needs the
		// claims-without-evidence guard adds them deliberately.
		Issues: []CitationIssue{{
			Kind:        reasoning.IssueUnsupported,
			OutcomeType: reasoning.IssueOutcomeInferenceIssue,
			OutcomeID:   canonicalIssue.ID(), Input: issueInput,
			Reason: reasoning.UnverifiedClaimReason,
		}},
	}
	id, err := reasoning.CanonicalResponseID(
		envelope.SessionID, envelope.RecordedAt,
		citationResponseIdentity(envelope))
	if err != nil {
		t.Fatal(err)
	}
	envelope.ID = id
	return envelope
}

func TestAskExecutorAcceptsAValidEnvelope(t *testing.T) {
	now := time.Now().UTC()
	provider := &stubAskProvider{envelope: verifiedAskEnvelope(t, now.Add(-time.Hour))}
	executor, err := NewAskExecutor(AskExecutorConfig{
		Provider: provider, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"what gates promotion?"}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %#v", result.Evidence)
	}
	// A public source declares no labels, so its evidence carries none. It must
	// still be recorded: dropping it made every action against an unlabeled
	// corpus fail.
	if len(result.Evidence[0].Visibility) != 0 {
		t.Fatalf("public evidence gained a label: %#v", result.Evidence[0])
	}
	// The verifier resolves all three citation source roles, and the recorded
	// grounding must match what the interaction record asserts for the anchor.
	if len(result.Evidence[0].NodeIDs) != 3 {
		t.Fatalf("node roles = %#v", result.Evidence[0].NodeIDs)
	}
	if result.EvidenceSnapshotID != "snapshot" {
		t.Fatalf("snapshot = %q", result.EvidenceSnapshotID)
	}
	var output AskExecutionOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatal(err)
	}
	if output.EvidenceCount != 1 || output.EvidenceConsidered != 1 ||
		output.EvidenceUnusable != 0 || output.EvidenceTruncated {
		t.Fatalf("receipt = %#v", output)
	}
}

// TestAskExecutorForwardsAbsentTopKForWorkspaceClamping proves the executor
// does not substitute its own retrieval width. Sending a constant here would
// exceed a workspace whose retrieval limit is lower and fail an action the same
// principal can ask over chat.
func TestAskExecutorForwardsAbsentTopKForWorkspaceClamping(t *testing.T) {
	now := time.Now().UTC()
	provider := &stubAskProvider{envelope: verifiedAskEnvelope(t, now.Add(-time.Hour))}
	executor, err := NewAskExecutor(AskExecutorConfig{
		Provider: provider, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"anything"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if provider.last.TopK != 0 {
		t.Fatalf("absent top_k was rewritten to %d", provider.last.TopK)
	}
	if _, err := executor.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"anything","top_k":3}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if provider.last.TopK != 3 {
		t.Fatalf("explicit top_k = %d", provider.last.TopK)
	}
}

// TestAskExecutorAppliesChatFinalizationContract proves the executor refuses an
// envelope the chat transport would refuse. Both consumers take the same
// provider, so a weaker check here would commit as a durable effect what the
// HTTP path rejects. It also proves a post-Ask failure still carries the
// session identity, which is the only route back to a reasoning call that was
// already performed and paid for.
func TestAskExecutorAppliesChatFinalizationContract(t *testing.T) {
	now := time.Now().UTC()
	for name, mutate := range map[string]func(*CitationEnvelope){
		"missing ontology interpretation": func(e *CitationEnvelope) {
			e.OntologyInterpretation = nil
		},
		"response not verified": func(e *CitationEnvelope) {
			e.Verification = reasoning.VerificationUnverified
		},
	} {
		t.Run(name, func(t *testing.T) {
			envelope := verifiedAskEnvelope(t, now.Add(-time.Hour))
			mutate(&envelope)
			executor, err := NewAskExecutor(AskExecutorConfig{
				Provider: &stubAskProvider{envelope: envelope},
				Clock:    func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), fleet.Invocation{
				Capability: AskCapability, Action: AskAction,
				Input: json.RawMessage(`{"question":"anything"}`),
			})
			if err != nil {
				t.Fatalf("a post-Ask failure must still return a receipt: %v", err)
			}
			if result.ErrorCode != AskErrorUnverified {
				t.Fatalf("error code = %q", result.ErrorCode)
			}
			if len(result.Evidence) != 0 {
				t.Fatalf("a refused envelope recorded evidence: %#v", result.Evidence)
			}
			var output AskExecutionOutput
			if err := json.Unmarshal(result.Output, &output); err != nil {
				t.Fatal(err)
			}
			if output.SessionID == "" {
				t.Fatal("a failed record must carry the reasoning session identity")
			}
		})
	}
}

// TestAskExecutorRejectsFutureSnapshot proves a snapshot pinned ahead of the
// execution clock fails with a code naming the cause, rather than being
// rewritten to fit or rejected generically by the dispatch service.
func TestAskExecutorRejectsFutureSnapshot(t *testing.T) {
	now := time.Now().UTC()
	executor, err := NewAskExecutor(AskExecutorConfig{
		Provider: &stubAskProvider{envelope: verifiedAskEnvelope(t, now.Add(time.Hour))},
		Clock:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"anything"}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.ErrorCode != AskErrorEvidenceSkew || len(result.Evidence) != 0 {
		t.Fatalf("result = %#v", result)
	}
	var output AskExecutionOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatal(err)
	}
	// Nothing was truncated: the member bound was never reached. Deriving the
	// flag from the recorded count would report every post-translation failure
	// as an overflow.
	if output.EvidenceTruncated {
		t.Fatalf("skew reported as truncation: %#v", output)
	}
	// The pin is the subject of this failure, so the operator must be able to
	// see which one was ahead.
	if output.SnapshotID == "" {
		t.Fatalf("skew receipt omits the snapshot pin: %#v", output)
	}
}

// TestAskExecutorRecordsSourceLabels proves a labeled source's labels reach the
// recorded evidence. The empty case means public; this is the other half, and
// it is the value a dispatch reader will match on once #369 enforces labels.
func TestAskExecutorRecordsSourceLabels(t *testing.T) {
	now := time.Now().UTC()
	envelope := verifiedAskEnvelope(t, now.Add(-time.Hour))
	labeled := validCitationEvidence(t, "primary", "a cited passage",
		[]string{"project-x", "secret"}, "snapshot", now.Add(-time.Hour))
	envelope.Evidence = []CitationEvidence{labeled}
	refs, tally := askExecutorEvidence(envelope)
	if len(refs) != 1 || tally.usable != 1 {
		t.Fatalf("refs = %#v, tally = %#v", refs, tally)
	}
	conjoined, err := interaction.Conjoin(labeled.Visibility)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs[0].Visibility) != len(conjoined) {
		t.Fatalf("visibility = %#v", refs[0].Visibility)
	}
	for index := range conjoined {
		if refs[0].Visibility[index] != conjoined[index] {
			t.Fatalf("visibility = %#v, want %#v", refs[0].Visibility, conjoined)
		}
	}
}

// TestAskExecutorOutputCarriesNoDocumentContent proves the action receipt holds
// no document-derived content. The dispatch read paths serialize Output to any
// reader authorized on the action's scope without checking visibility labels,
// so claim values, issue reasons, ontology identifiers, and the effective label
// expression must not appear.
func TestAskExecutorOutputCarriesNoDocumentContent(t *testing.T) {
	now := time.Now().UTC()
	envelope := verifiedAskEnvelope(t, now.Add(-time.Hour))
	executor, err := NewAskExecutor(AskExecutorConfig{
		Provider: &stubAskProvider{envelope: envelope},
		Clock:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"anything"}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, leaked := range []string{
		envelope.Issues[0].Reason, envelope.Evidence[0].Quote,
		string(envelope.Evidence[0].Citation.DocumentID), "output_visibility",
	} {
		if bytes.Contains(result.Output, []byte(leaked)) {
			t.Fatalf("%q leaked into the receipt: %s", leaked, result.Output)
		}
	}
	var receipt map[string]any
	if err := json.Unmarshal(result.Output, &receipt); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"verification": true, "snapshot_id": true, "session_id": true,
		"evidence_count": true, "evidence_considered": true,
		"evidence_unusable": true, "evidence_truncated": true,
		"cited_evidence_truncated": true, "claim_count": true,
		"issue_count": true,
	}
	for key := range receipt {
		if !allowed[key] {
			t.Fatalf("unexpected receipt field %q", key)
		}
	}
}

func TestAskExecutorRejectsForeignAction(t *testing.T) {
	now := time.Now().UTC()
	executor, err := NewAskExecutor(AskExecutorConfig{
		Provider: &stubAskProvider{envelope: verifiedAskEnvelope(t, now.Add(-time.Hour))},
		Clock:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), fleet.Invocation{
		Capability: "documents", Action: "delete",
		Input: json.RawMessage(`{"question":"anything"}`),
	})
	if err == nil || result.ErrorCode != AskErrorUnsupportedAction {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

// TestAskExecutorSeparatesAuthorizationRefusal proves an authorization refusal
// from the reasoning path is distinguishable from a model or transport failure.
// The usual cause is an agent principal granted invoke but not retrieve.
func TestAskExecutorSeparatesAuthorizationRefusal(t *testing.T) {
	refused, err := NewAskExecutor(AskExecutorConfig{
		Provider: &stubAskProvider{
			err: shoal.NewError(shoal.ErrorUnauthorized, "retrieve is not permitted"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := refused.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"anything"}`),
	})
	if err == nil || result.ErrorCode != AskErrorReasoningUnauthorized {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	failure := errors.New("reasoning unavailable")
	broken, err := NewAskExecutor(AskExecutorConfig{
		Provider: &stubAskProvider{err: failure},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = broken.Execute(context.Background(), fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		Input: json.RawMessage(`{"question":"anything"}`),
	})
	if !errors.Is(err, failure) || result.ErrorCode != AskErrorReasoningFailed {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

// The mapper guards below are defense in depth. CitationEnvelope.Validate now
// rejects most malformed evidence before Execute reaches them, so they are
// exercised directly rather than through an envelope that could not occur.

func TestAskEvidenceMapperSkipsUnexpressibleAnchors(t *testing.T) {
	now := time.Now().UTC()
	valid := validCitationEvidence(t, "primary", "quote", nil, "snapshot", now)
	envelope := CitationEnvelope{Evidence: []CitationEvidence{
		valid,
		// Anonymous: countable, never usable.
		{Citation: valid.Citation},
		// A graph anchor whose edges do not connect its nodes.
		{
			AnchorID: "broken-path",
			Path: &graph.Path{
				Nodes: []graph.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}},
				Edges: []graph.Edge{{ID: "e", From: "a", To: "b"}},
			},
		},
		// Neither a citation nor a path.
		{AnchorID: "empty"},
	}}
	refs, tally := askExecutorEvidence(envelope)
	if len(refs) != 1 || refs[0].AnchorID != valid.AnchorID {
		t.Fatalf("refs = %#v", refs)
	}
	if tally.considered != 4 || tally.usable != 1 {
		t.Fatalf("tally = %#v", tally)
	}
}

func TestAskEvidenceMapperShedsUnreferencedAssertions(t *testing.T) {
	envelope := CitationEnvelope{Evidence: []CitationEvidence{{
		AnchorID: "graph",
		Path:     &graph.Path{Nodes: []graph.Node{{ID: "node"}}},
		Assertions: []CitationAssertion{{
			AssertionID: "assertion", EdgeID: "unreferenced",
			Origin: ontology.AssertionExplicit,
		}},
	}}}
	refs, tally := askExecutorEvidence(envelope)
	if len(refs) != 1 || len(refs[0].Assertions) != 0 {
		t.Fatalf("refs = %#v", refs)
	}
	if tally.usable != 1 {
		t.Fatalf("tally = %#v", tally)
	}
	reference := interaction.EvidenceReference{
		AnchorID: refs[0].AnchorID, Kind: refs[0].Kind,
		Citation: refs[0].Citation, NodeIDs: refs[0].NodeIDs,
		EdgeIDs: refs[0].EdgeIDs, Assertions: refs[0].Assertions,
	}
	if _, err := reference.Canonical(); err != nil {
		t.Fatalf("mapped evidence is not canonical: %v", err)
	}
}

// TestAskEvidenceMapperPacksWithinTheMemberBound proves the bound counts
// members rather than anchors, that an oversized anchor does not discard the
// smaller ones after it, and that the tally reports the loss.
func TestAskEvidenceMapperPacksWithinTheMemberBound(t *testing.T) {
	now := time.Now().UTC()
	envelope := CitationEnvelope{}
	for index := 0; index < 120; index++ {
		envelope.Evidence = append(envelope.Evidence, validCitationEvidence(
			t, "doc"+strconv.Itoa(index), "quote", nil, "snapshot", now))
	}
	refs, tally := askExecutorEvidence(envelope)
	if tally.considered != 120 || tally.usable != 120 {
		t.Fatalf("tally = %#v", tally)
	}
	// Each fully-cited anchor costs three members against a bound of 256.
	if len(refs) != fleet.MaxActionEvidence/3 {
		t.Fatalf("recorded %d anchors", len(refs))
	}
	output := askExecutorOutput(envelope, len(refs), tally)
	if !output.EvidenceTruncated || output.EvidenceUnusable != 0 {
		t.Fatalf("receipt = %#v", output)
	}
}

// TestAskExecutorSeparatesStructuralRejection proves a structurally invalid
// envelope is distinguishable from one that is merely not finalized. Both are
// refused, but they need different responses: one is a provider correctness
// problem, the other a workspace or ontology state problem.
func TestAskExecutorSeparatesStructuralRejection(t *testing.T) {
	now := time.Now().UTC()
	for name, mutate := range map[string]func(*CitationEnvelope){
		"anchor identity is not canonical": func(e *CitationEnvelope) {
			e.Evidence[0].AnchorID = "forged"
		},
		"evidence not verified": func(e *CitationEnvelope) {
			e.Evidence[0].Status = reasoning.VerificationUnverified
		},
		// Evidence visibility must conjoin to the response's effective
		// visibility, which Validate cross-checks before finalization runs.
		"output visibility not derived from evidence": func(e *CitationEnvelope) {
			e.EffectiveVisibility = []string{"secret"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			envelope := verifiedAskEnvelope(t, now.Add(-time.Hour))
			mutate(&envelope)
			executor, err := NewAskExecutor(AskExecutorConfig{
				Provider: &stubAskProvider{envelope: envelope},
				Clock:    func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), fleet.Invocation{
				Capability: AskCapability, Action: AskAction,
				Input: json.RawMessage(`{"question":"anything"}`),
			})
			if err != nil {
				t.Fatalf("a post-Ask failure must still return a receipt: %v", err)
			}
			if result.ErrorCode != AskErrorInvalidEnvelope {
				t.Fatalf("error code = %q", result.ErrorCode)
			}
			if len(result.Evidence) != 0 {
				t.Fatalf("a refused envelope recorded evidence: %#v", result.Evidence)
			}
		})
	}
}

// TestAskExecutorIsIdempotentForOneExecutorKey proves a retried action does not
// bill a second model call. The dispatch service threads a stable executor key
// into every invocation for this purpose: an ambiguous execution leaves the
// action claimed, and once the claim lease expires it is re-claimed and
// executed again.
func TestAskExecutorIsIdempotentForOneExecutorKey(t *testing.T) {
	now := time.Now().UTC()
	provider := &stubAskProvider{envelope: verifiedAskEnvelope(t, now.Add(-time.Hour))}
	executor, err := NewAskExecutor(AskExecutorConfig{
		Provider: provider, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation := fleet.Invocation{
		Capability: AskCapability, Action: AskAction,
		IdempotencyKey: []byte("executor-key"),
		Input:          json.RawMessage(`{"question":"anything"}`),
	}
	first, err := executor.Execute(context.Background(), invocation)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	second, err := executor.Execute(context.Background(), invocation)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("a retry billed %d model calls", provider.calls)
	}
	if !bytes.Equal(first.Output, second.Output) ||
		len(first.Evidence) != len(second.Evidence) ||
		first.EvidenceSnapshotID != second.EvidenceSnapshotID {
		t.Fatalf("retry returned a different result:\n%#v\n%#v", first, second)
	}
	// A different action must not be served from the first one's result.
	invocation.IdempotencyKey = []byte("other-key")
	if _, err := executor.Execute(context.Background(), invocation); err != nil {
		t.Fatalf("second action: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("distinct executor keys shared a result: %d calls", provider.calls)
	}
}

// TestAskEvidenceMapperPacksCitedAnchorsFirst proves the member bound drops
// retrieved-but-uncited evidence before anything a claim rests on. Without the
// ordering an evidence-rich answer would commit as succeeded while the anchors
// its own claims cite were the ones discarded.
func TestAskEvidenceMapperPacksCitedAnchorsFirst(t *testing.T) {
	now := time.Now().UTC()
	envelope := CitationEnvelope{}
	var cited []shoal.ID
	for index := 0; index < 120; index++ {
		evidence := validCitationEvidence(
			t, "doc"+strconv.Itoa(index), "quote", nil, "snapshot", now)
		envelope.Evidence = append(envelope.Evidence, evidence)
		// The claim rests on the anchors that arrive last, so only the
		// cited-first ordering can keep them.
		if index >= 110 {
			cited = append(cited, evidence.AnchorID)
		}
	}
	envelope.Claims = []CitationClaim{{CitationAnchorIDs: cited}}
	refs, tally := askExecutorEvidence(envelope)
	if !tally.truncated || tally.citedDropped {
		t.Fatalf("tally = %#v", tally)
	}
	recorded := make(map[shoal.ID]struct{}, len(refs))
	for _, ref := range refs {
		recorded[ref.AnchorID] = struct{}{}
	}
	for _, anchorID := range cited {
		if _, ok := recorded[anchorID]; !ok {
			t.Fatalf("a cited anchor was dropped: %q", anchorID)
		}
	}
}

// TestAskExecutorReportsCitedGroundingLeftOut proves that when a claim rests on
// an anchor the record cannot hold, the receipt says so. Failing the action
// instead would discard a verified answer the chat path returns happily, after
// the model call was already billed; the complete grounding stays in the
// interaction session the receipt names.
func TestAskExecutorReportsCitedGroundingLeftOut(t *testing.T) {
	now := time.Now().UTC()
	envelope := verifiedAskEnvelope(t, now.Add(-time.Hour))
	nodes := make([]graph.Node, 0, 400)
	edges := make([]graph.Edge, 0, 399)
	for index := 0; index < 400; index++ {
		nodes = append(nodes, graph.Node{ID: shoal.ID("n" + strconv.Itoa(index))})
		if index > 0 {
			edges = append(edges, graph.Edge{
				ID:   shoal.ID("e" + strconv.Itoa(index)),
				From: shoal.ID("n" + strconv.Itoa(index-1)),
				To:   shoal.ID("n" + strconv.Itoa(index)),
			})
		}
	}
	oversized := CitationEvidence{
		AnchorID: "huge", Path: &graph.Path{Nodes: nodes, Edges: edges},
	}
	envelope.Evidence = append(envelope.Evidence, oversized)
	envelope.Claims = []CitationClaim{{
		CitationAnchorIDs: []shoal.ID{oversized.AnchorID},
	}}
	refs, tally := askExecutorEvidence(envelope)
	if !tally.citedDropped {
		t.Fatalf("an unrecordable cited anchor was not reported: %#v", tally)
	}
	if len(refs) == 0 {
		t.Fatal("the smaller anchors should still have been packed")
	}
	output := askExecutorOutput(envelope, len(refs), tally)
	if !output.CitedEvidenceTruncated || output.SessionID == "" {
		t.Fatalf("receipt must name the session holding the full grounding: %#v",
			output)
	}
}

// TestAskEvidenceMapperReportsUntranslatableCitedAnchors proves an anchor a
// claim cites that cannot be expressed at all is reported the same way as one
// the bound could not fit. Setting the flag only in the packing loop would
// leave the guard depending on an invariant owned by another file.
func TestAskEvidenceMapperReportsUntranslatableCitedAnchors(t *testing.T) {
	now := time.Now().UTC()
	valid := validCitationEvidence(t, "primary", "quote", nil, "snapshot", now)
	envelope := CitationEnvelope{
		Evidence: []CitationEvidence{
			valid,
			{AnchorID: "unexpressible"},
		},
		Claims: []CitationClaim{{
			CitationAnchorIDs: []shoal.ID{valid.AnchorID, "unexpressible"},
		}},
	}
	refs, tally := askExecutorEvidence(envelope)
	if len(refs) != 1 {
		t.Fatalf("refs = %#v", refs)
	}
	if !tally.citedDropped || tally.truncated {
		t.Fatalf("tally = %#v", tally)
	}
}
