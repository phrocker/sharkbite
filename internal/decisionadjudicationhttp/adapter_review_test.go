// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionadjudicationhttp

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestReviewProjectionPreservesOriginalAndDetaches(t *testing.T) {
	no := false
	cfg := decision.AdjudicationProposalConfig{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", QuestionID: "question", ObservationReceiptIDs: []shoal.ID{shoal.ID("outcome-receipt:" + strings.Repeat("a", 64))}, WitnessIDs: []shoal.ID{"witness"}, Disposition: decision.AdjudicationVerified, Truth: &no}
	target, e := decision.AdjudicationTargetID("task", "picture", cfg.SubjectID, cfg.QuestionID)
	if e != nil {
		t.Fatal(e)
	}
	policy := shoal.ID("decision:label-policy:v1:" + strings.Repeat("d", 64))
	pid, e := decision.AdjudicationProposalID(policy, target, cfg)
	if e != nil {
		t.Fatal(e)
	}
	r := journal.Receipt{ID: shoal.ID("adjudication-receipt:" + strings.Repeat("b", 64)), BasisID: shoal.ID("decision:adjudication-basis:v1:" + strings.Repeat("c", 64)), Version: 1, TargetID: target, TaskID: "task", PictureID: "picture", PolicyID: policy, ProposalID: pid, ProposalConfig: cfg, Adjudicator: journal.Attribution{SubjectID: shoal.ID(string([]byte{0, 255})), ActorID: shoal.ID(string([]byte{254, 0})), ClientID: shoal.ID(string([]byte{253})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{252, 0}))}, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("e", 64)}, ReceivedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	got, e := project(r)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(toCore(got.Proposal), cfg) || got.Adjudicator.SubjectID != r.Adjudicator.SubjectID || got.Adjudicator.ActorID != r.Adjudicator.ActorID || got.Adjudicator.ClientID != r.Adjudicator.ClientID || !reflect.DeepEqual(got.Adjudicator.OnBehalfOf, r.Adjudicator.OnBehalfOf) || got.BasisID != r.BasisID || got.ProposalID != pid {
		t.Fatal("projection changed retained material")
	}
	*got.Proposal.Truth = true
	got.Proposal.ObservationReceiptIDs[0] = "changed"
	got.Proposal.WitnessIDs[0] = "changed"
	got.Adjudicator.OnBehalfOf[0] = "changed"
	if *cfg.Truth || cfg.ObservationReceiptIDs[0] == "changed" || cfg.WitnessIDs[0] == "changed" || r.Adjudicator.OnBehalfOf[0] == "changed" {
		t.Fatal("projection aliases retained material")
	}
}
func TestReviewProposalMappingAndInputDetachment(t *testing.T) {
	p := api.AdjudicationProposal{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", QuestionID: "question", ObservationReceiptIDs: []shoal.ID{"receipt"}, WitnessIDs: []shoal.ID{"witness"}, Disposition: "unresolved", Reason: "needs evidence", ExpectedHeadID: "head", ExpectedVersion: 17}
	c := toCore(p)
	if !reflect.DeepEqual(proposal(c), p) {
		t.Fatal("proposal fields lost")
	}
	c.ObservationReceiptIDs[0] = "changed"
	c.WitnessIDs[0] = "changed"
	if p.ObservationReceiptIDs[0] == "changed" || p.WitnessIDs[0] == "changed" {
		t.Fatal("input aliases core")
	}
}
func TestReviewMissingServiceFailsClosed(t *testing.T) {
	if _, e := New(nil); e == nil {
		t.Fatal("nil service accepted")
	}
	for _, a := range []*Adapter{nil, {}} {
		if r, e := a.Adjudicate(context.Background(), api.AdjudicationProposal{}, []byte("key")); e == nil || r.ID != "" {
			t.Fatal("nil adjudication service")
		}
		if h, e := a.AdjudicationHistory(context.Background(), "target"); e == nil || len(h.Receipts) != 0 {
			t.Fatal("nil history service")
		}
	}
}
