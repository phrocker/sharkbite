// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func spanOf(start, end int) lexicon.Span { return lexicon.Span{Start: start, End: end} }

const routerDir = "../router"

func routerReceipt() router.Receipt {
	return router.Receipt{
		Router: router.Version, GrammarSetDigest: strings.Repeat("a", 64), CatalogDigest: strings.Repeat("b", 64),
		Candidates: 3, FeatureSchemaID: router.FeatureSchemaID, TaskID: "decision:task:v1:t",
		PictureID: "decision:picture:v1:p", RequestID: "decision:request:v1:r",
		PredictorID: "decision:predictor:v1:m", PredictionID: "decision:prediction:v1:x",
	}
}

func sealProposal(t testing.TB, p router.Proposal) router.Proposal {
	t.Helper()
	id, err := router.ProposalID(p)
	if err != nil {
		t.Fatal(err)
	}
	p.ID = id
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture proposal is invalid: %v", err)
	}
	return p
}

func routerTargetOf(kind router.Kind, approval bool) *router.TargetRef {
	switch kind {
	case router.KindAction:
		return &router.TargetRef{Kind: kind, Action: &router.ActionRef{
			AgentID: "agent-ops", AgentGeneration: 1, Capability: "ops", Action: "restart", RequiresApproval: approval,
		}}
	case router.KindDecision:
		return &router.TargetRef{Kind: kind, Decision: &router.DecisionRef{
			ProfileID: "service-operation-risk", ProfileRevisionID: "r1", TaskID: "task-risk",
		}}
	case router.KindLookup:
		return &router.TargetRef{Kind: kind, Lookup: &router.LookupRef{
			TemplateID: "lookup:owned_by:out", RelationKey: "owned_by", Direction: "out",
		}}
	}
	return nil
}

// routerProposal is a valid proposal of kind, with one node slot and, except
// for a lookup, one enum slot.
func routerProposal(t testing.TB, kind router.Kind, approval bool) router.Proposal {
	p := router.Proposal{Kind: kind, Target: routerTargetOf(kind, approval), Receipt: routerReceipt()}
	switch kind {
	case router.KindLookup:
		p.Slots = []router.Slot{{Name: "subject", NodeIDs: []shoal.ID{"node-payments"}, TokenSpan: spanOf(2, 3)}}
		p.Input = json.RawMessage(`{"subject":"node-payments"}`)
	default:
		p.Slots = []router.Slot{
			{Name: "mode", Enum: "graceful", TokenSpan: spanOf(4, 5)},
			{Name: "service", NodeIDs: []shoal.ID{"node-payments"}, TokenSpan: spanOf(1, 2)},
		}
		p.Input = json.RawMessage(`{"mode":"graceful","service":"node-payments"}`)
	}
	return sealProposal(t, p)
}

// routerAbstention is a valid abstention for reason, naming the target and
// slots a router gives with it.
func routerAbstention(t testing.TB, reason router.Reason) router.Proposal {
	p := router.Proposal{Kind: router.KindAbstain, Reasons: []router.Reason{reason}, Receipt: routerReceipt()}
	if slotReasons[reason] {
		p.Target = routerTargetOf(router.KindAction, true)
		if reason != router.ReasonInvalidInput {
			p.Missing = []string{"environment", "service"}
		}
	}
	switch reason {
	case router.ReasonEmptyText, router.ReasonTextOutOfBounds:
		// No decision is requested for an early abstention.
		p.Receipt.FeatureSchemaID, p.Receipt.TaskID, p.Receipt.PictureID = "", "", ""
		p.Receipt.RequestID, p.Receipt.PredictorID, p.Receipt.PredictionID = "", "", ""
	}
	return sealProposal(t, p)
}

func routerCases(t *testing.T) []valueCase {
	var out []valueCase
	for _, k := range values(typedConsts(t, routerDir, "Kind")) {
		kind := router.Kind(k)
		if kind == router.KindAbstain {
			continue
		}
		for _, approval := range []bool{false, true} {
			if approval && kind != router.KindAction {
				continue
			}
			p := routerProposal(t, kind, approval)
			out = append(out, valueCase{
				name: "proposal kind=" + k + map[bool]string{true: " approval", false: ""}[approval],
				run:  func(r *Renderer) ([]Sentence, error) { return r.Proposal(p, Options{}) },
			})
		}
	}
	for _, reason := range values(typedConsts(t, routerDir, "Reason")) {
		p := routerAbstention(t, router.Reason(reason))
		out = append(out, valueCase{
			name: "abstain reason=" + reason,
			run:  func(r *Renderer) ([]Sentence, error) { return r.Proposal(p, Options{}) },
		})
	}
	return out
}

func TestParityRouterVocabularies(t *testing.T) {
	sameSet(t, "router Kind", values(typedConsts(t, routerDir, "Kind")), sorted(router.Kinds))
	sameSet(t, "router Reason", values(typedConsts(t, routerDir, "Reason")), sorted(router.Reasons))
}

// TestCoverageRouterProposals renders every kind and every abstention reason
// read from pkg/router's source, and requires each to say it is a proposal or
// an abstention and that nothing ran, and to carry its receipt.
func TestCoverageRouterProposals(t *testing.T) {
	r := newObserved(t)
	for _, c := range routerCases(t) {
		sentences, err := c.run(r.Renderer)
		noFallback(t, c.name, sentences, err)
		outcome := sentences[0]
		if !strings.HasPrefix(outcome.Key, "router.proposal.") && !strings.HasPrefix(outcome.Key, "router.abstain.") {
			t.Errorf("%s: outcome key %s", c.name, outcome.Key)
		}
		if !strings.Contains(outcome.Text, "Nothing") || !(strings.Contains(outcome.Text, "not run") || strings.Contains(outcome.Text, "or run")) {
			t.Errorf("%s: does not say nothing ran: %s", c.name, outcome.Text)
		}
		kinds := map[string]bool{}
		for _, ref := range outcome.Refs {
			kinds[ref.Kind] = true
		}
		for _, want := range []string{"proposal", "catalog", "grammar_set", "router"} {
			if !kinds[want] {
				t.Errorf("%s: receipt ref %s missing", c.name, want)
			}
		}
		if strings.HasPrefix(c.name, "proposal") && (!kinds["prediction"] || !kinds["predictor"] || !kinds["picture"]) {
			t.Errorf("%s: decision receipt refs missing: %v", c.name, outcome.Refs)
		}
		if strings.HasPrefix(c.name, "proposal") && strings.Contains(c.name, "approval") != strings.Contains(outcome.Text, "require approval") {
			t.Errorf("%s: approval not stated: %s", c.name, outcome.Text)
		}
	}
}

func TestRouterProposalRefusesInvalid(t *testing.T) {
	p := routerProposal(t, router.KindAction, true)
	p.Input = json.RawMessage(`{"service":"other"}`)
	if _, err := New(nil).Proposal(p, Options{}); err == nil {
		t.Fatal("a proposal whose ID does not match its content was narrated")
	}
	bad := router.Proposal{Kind: router.KindAbstain, Reasons: []router.Reason{"made_up"}}
	bad.ID, _ = router.ProposalID(bad)
	if _, err := New(nil).Proposal(bad, Options{}); err == nil {
		t.Fatal("an unknown reason was narrated")
	}
}
