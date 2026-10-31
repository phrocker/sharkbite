// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisiondatasets

import (
	"context"
	"strings"
	"testing"
	"time"

	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func classWindow(s Sample) Window { return Window{From: s.Cutoff.Add(-time.Hour), To: s.Cutoff} }

func oneClass(t *testing.T, s *Service, a *exportAuthority, ctx context.Context) (Sample, ClassRecord) {
	t.Helper()
	sample, e := s.ClassSample(ctx, a.cohort.ID)
	if e != nil {
		t.Fatal(e)
	}
	records, e := Aggregate(sample, classWindow(sample))
	if e != nil {
		t.Fatal(e)
	}
	if len(records.Classes) != 1 || records.Members != 1 || records.InWindow != 1 || records.Outside != 0 {
		t.Fatalf("%+v", records)
	}
	return sample, records.Classes[0]
}

// setDisposition rewrites the single adjudication of the fixture target so
// the receipt, proposal, and basis stay the chain the journal would hold.
func setDisposition(t *testing.T, a *exportAuthority, disposition decision.AdjudicationDisposition, label string) {
	t.Helper()
	target := &a.targets[0]
	cfg := target.History[0].ProposalConfig
	cfg.Disposition, cfg.Label = disposition, label
	if disposition != decision.AdjudicationVerified {
		cfg.Reason = "insufficient corroboration"
	}
	p, e := decision.NewAdjudicationProposal(a.cohort.Policy, target.Prediction, cfg)
	if e != nil {
		t.Fatal(e)
	}
	b, e := decision.NewAdjudicationBasis(p, target.SelectedBasis.Config())
	if e != nil {
		t.Fatal(e)
	}
	target.SelectedBasis = b
	target.History[0].ProposalConfig = cfg
	target.History[0].ProposalID = p.ID()
	target.History[0].BasisID = b.ID()
}

func TestClassRecordCountsTheAdjudicationInForce(t *testing.T) {
	s, a, ctx := exportFixture(t)
	sample, rec := oneClass(t, s, a, ctx)
	want := decision.DecisionClass{TaskID: sample.TaskID, QuestionID: "review", PolicyID: sample.PolicyID, Label: "inspect"}
	id, _ := want.ID()
	if rec.ClassID != id || rec.Class != want || rec.Targets != 1 || rec.Adjudication.VerifiedAgree != 1 || rec.Precision == nil || *rec.Precision != 1 || rec.Outcomes.Correctness != 1 || rec.Outcomes.Receipts != 1 || rec.Outcomes.Superseded != 0 {
		t.Fatalf("%+v", rec)
	}
	if !rec.FirstObserved.Equal(sample.Rows[0].ObservedAt) || rec.FirstObserved.After(sample.Cutoff) || !rec.FirstObserved.Before(sample.Cutoff) {
		t.Fatalf("observation %v cutoff %v", rec.FirstObserved, sample.Cutoff)
	}
	for _, tc := range []struct {
		name        string
		disposition decision.AdjudicationDisposition
		label       string
		check       func(Adjudication) bool
	}{
		{"disagree", decision.AdjudicationVerified, "ordinary", func(x Adjudication) bool { return x.VerifiedDisagree == 1 && x.VerifiedAgree == 0 }},
		{"disputed", decision.AdjudicationDisputed, "", func(x Adjudication) bool { return x.Disputed == 1 }},
		{"unresolved", decision.AdjudicationUnresolved, "", func(x Adjudication) bool { return x.Unresolved == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			setDisposition(t, a, tc.disposition, tc.label)
			_, rec := oneClass(t, s, a, ctx)
			if !tc.check(rec.Adjudication) || rec.Targets != 1 {
				t.Fatalf("%+v", rec.Adjudication)
			}
			if tc.name == "disagree" {
				if rec.Precision == nil || *rec.Precision != 0 {
					t.Fatal("precision of a verified disagreement is zero, not unknown")
				}
			} else if rec.Precision != nil {
				t.Fatal("an unverified class has no precision")
			}
		})
	}
}

func TestClassRecordKeepsUnknownInTheDenominator(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Target)
		check  func(Adjudication) bool
	}{
		{"no adjudication", func(v *Target) { v.History = nil; v.SelectedBasis = decision.AdjudicationBasis{} }, func(x Adjudication) bool { return x.None == 1 }},
		{"withdrawn", func(v *Target) { v.LabelWithdrawn = true }, func(x Adjudication) bool { return x.Unresolved == 1 && x.Withdrawn == 1 && x.VerifiedAgree == 0 }},
		{"new report after the basis", func(v *Target) {
			v.OutcomeReceiptIDs = append(v.OutcomeReceiptIDs, shoal.ID("outcome-receipt:"+strings.Repeat("d", 64)))
		}, func(x Adjudication) bool { return x.Unresolved == 1 && x.Stale == 1 && x.VerifiedAgree == 0 }},
		{"basis cites a receipt the inventory lost", func(v *Target) { v.OutcomeReceiptIDs = nil }, func(x Adjudication) bool { return x.Unresolved == 1 && x.Stale == 1 && x.VerifiedAgree == 0 }},
		{"training denied", func(v *Target) { v.TrainingAllowed = false }, func(x Adjudication) bool { return x.Unresolved == 1 && x.TrainingDenied == 1 && x.VerifiedAgree == 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			tc.change(&a.targets[0])
			_, rec := oneClass(t, s, a, ctx)
			if !tc.check(rec.Adjudication) || rec.Targets != 1 || rec.Precision != nil {
				t.Fatalf("%+v precision=%v", rec.Adjudication, rec.Precision)
			}
		})
	}
}

func TestClassRecordAbstentionIsItsOwnClass(t *testing.T) {
	s, a, ctx := exportFixture(t)
	target := &a.targets[0]
	r := target.Prediction.Request()
	p, e := decision.NewPredictionRecord(r, decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: r.Config().RequestedAt, Answers: []decision.Answer{{SubjectID: "subject", QuestionID: "review", Status: decision.AnswerAbstained, Reason: "insufficient evidence"}}})
	if e != nil {
		t.Fatal(e)
	}
	target.Prediction, target.History, target.SelectedBasis = p, nil, decision.AdjudicationBasis{}
	a.cohort.Members[0].PredictionID = p.ID()
	_, rec := oneClass(t, s, a, ctx)
	if !rec.Class.Abstained || rec.Class.Label != "" || rec.Adjudication.None != 1 {
		t.Fatalf("%+v", rec)
	}
}

func TestClassRecordCountsASupersededOutcomeOnce(t *testing.T) {
	s, a, ctx := exportFixture(t)
	target := &a.targets[0]
	first := target.History[0].ProposalConfig.ObservationReceiptIDs[0]
	second := shoal.ID("outcome-receipt:" + strings.Repeat("e", 64))
	cfg := target.History[0].ProposalConfig
	cfg.ObservationReceiptIDs = []shoal.ID{first, second}
	p, e := decision.NewAdjudicationProposal(a.cohort.Policy, target.Prediction, cfg)
	if e != nil {
		t.Fatal(e)
	}
	bc := target.SelectedBasis.Config()
	replacement := bc.Outcomes[0]
	replacement.ReceiptID, replacement.ObservationID, replacement.Supersedes = second, "observation-2", first
	bc.Outcomes = append(bc.Outcomes, replacement)
	b, e := decision.NewAdjudicationBasis(p, bc)
	if e != nil {
		t.Fatal(e)
	}
	target.SelectedBasis = b
	target.History[0].ProposalConfig, target.History[0].ProposalID, target.History[0].BasisID = cfg, p.ID(), b.ID()
	target.OutcomeReceiptIDs = []shoal.ID{first, second}
	_, rec := oneClass(t, s, a, ctx)
	if rec.Outcomes.Receipts != 2 || rec.Outcomes.Correctness != 1 || rec.Outcomes.Superseded != 1 || rec.Adjudication.VerifiedAgree != 1 {
		t.Fatalf("%+v", rec.Outcomes)
	}
}

func TestClassRecordCountsALaterAdjudicationWithoutApplyingIt(t *testing.T) {
	s, a, ctx := exportFixture(t)
	target := &a.targets[0]
	head := target.History[0]
	cfg := head.ProposalConfig
	cfg.Disposition, cfg.Label, cfg.Reason = decision.AdjudicationDisputed, "", "reopened"
	cfg.ExpectedHeadID, cfg.ExpectedVersion = head.ID, 1
	p, e := decision.NewAdjudicationProposal(a.cohort.Policy, target.Prediction, cfg)
	if e != nil {
		t.Fatal(e)
	}
	later := journal.Receipt{ID: shoal.ID("adjudication-receipt:" + strings.Repeat("f", 64)), Version: 2, TargetID: head.TargetID, TaskID: head.TaskID, PictureID: head.PictureID, PolicyID: head.PolicyID, ProposalID: p.ID(), BasisID: shoal.ID("basis-2"), ProposalConfig: cfg, ReceivedAt: a.cohort.Cutoff.Add(30 * time.Second)}
	target.History = append(target.History, later)
	// The service clock must have reached the later receipt; the cutoff has not.
	clock := func() time.Time { return a.cohort.Cutoff.Add(time.Minute) }
	s2, e := New(Config{Resolver: s.config.Resolver, Authority: a, Clock: clock})
	if e != nil {
		t.Fatal(e)
	}
	_, rec := oneClass(t, s2, a, ctx)
	// The export nulls a label with a later adjudication; so does the record.
	if rec.Adjudication.VerifiedAgree != 0 || rec.Adjudication.Unresolved != 1 || rec.Adjudication.Later != 1 || rec.Adjudication.Disputed != 0 || rec.Precision != nil {
		t.Fatalf("%+v", rec.Adjudication)
	}
}

// Every condition under which the export withholds a verified label is a
// condition under which the record withholds it: the two never disagree
// about a label.
func TestClassRecordNeverAppliesALabelTheExportWithholds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Target)
	}{
		{"withdrawn", func(v *Target) { v.LabelWithdrawn = true }},
		{"training denied", func(v *Target) { v.TrainingAllowed = false }},
		{"stale inventory", func(v *Target) {
			v.OutcomeReceiptIDs = append(v.OutcomeReceiptIDs, shoal.ID("outcome-receipt:"+strings.Repeat("d", 64)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			tc.change(&a.targets[0])
			bundle, e := s.Export(ctx, a.cohort.ID)
			if e != nil {
				t.Fatal(e)
			}
			row := datasetRow(t, bundle)
			_, rec := oneClass(t, s, a, ctx)
			exportLabel := row["label"] != nil
			recordLabel := rec.Adjudication.VerifiedAgree+rec.Adjudication.VerifiedDisagree > 0
			if exportLabel != recordLabel || recordLabel {
				t.Fatalf("export label=%v record verified=%v", exportLabel, recordLabel)
			}
		})
	}
}

func TestClassSampleRefusesAFailedPrediction(t *testing.T) {
	s, a, ctx := exportFixture(t)
	target := &a.targets[0]
	r := target.Prediction.Request()
	p, e := decision.NewPredictionRecord(r, decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu", Status: decision.Failed, Reason: "predictor timed out", CompletedAt: r.Config().RequestedAt})
	if e != nil {
		t.Fatal(e)
	}
	target.Prediction, target.History, target.SelectedBasis = p, nil, decision.AdjudicationBasis{}
	a.cohort.Members[0].PredictionID = p.ID()
	if _, e := s.ClassSample(ctx, a.cohort.ID); e == nil {
		t.Fatal("a failed prediction was placed in a class")
	}
	// A verified row carrying a qualifier is not a sample the service makes.
	forged := Sample{CohortID: "cohort", TaskID: "task", QuestionID: "q", PolicyID: "policy", Labels: []string{"a", "b"}, Cutoff: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	forged.Rows = []SampleRow{{MemberID: "m", TargetID: "t", Class: decision.DecisionClass{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: "a"}, ObservedAt: forged.Cutoff, Status: StatusVerified, VerifiedLabel: "a", Later: true}}
	if _, e := Aggregate(forged, Window{From: forged.Cutoff, To: forged.Cutoff}); e == nil {
		t.Fatal("a qualified verified row aggregated as verified")
	}
	forged.Rows[0].Later = false
	forged.Rows[0].BasisOutcomes = []SampleOutcome{{ReceiptID: "outcome-receipt:x", Kind: decision.OutcomeExecution}}
	if _, e := Aggregate(forged, Window{From: forged.Cutoff, To: forged.Cutoff}); e == nil {
		t.Fatal("an execution receipt in a basis aggregated")
	}
}

func TestClassRecordWindowIsDeclaredAndInsideTheCutoff(t *testing.T) {
	s, a, ctx := exportFixture(t)
	sample, e := s.ClassSample(ctx, a.cohort.ID)
	if e != nil {
		t.Fatal(e)
	}
	observed := sample.Rows[0].ObservedAt
	for name, w := range map[string]Window{
		"empty":             {},
		"no end":            {From: observed},
		"no start":          {To: sample.Cutoff},
		"reversed":          {From: sample.Cutoff, To: observed.Add(-time.Second)},
		"past the cutoff":   {From: observed, To: sample.Cutoff.Add(time.Nanosecond)},
		"year out of range": {From: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), To: sample.Cutoff},
	} {
		if _, e := Aggregate(sample, w); !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: %v", name, e)
		}
	}
	outside, e := Aggregate(sample, Window{From: observed.Add(time.Second), To: sample.Cutoff})
	if e != nil || outside.Outside != 1 || outside.InWindow != 0 || len(outside.Classes) != 0 || outside.Members != 1 {
		t.Fatalf("%v %+v", e, outside)
	}
	exact, e := Aggregate(sample, Window{From: observed, To: observed})
	if e != nil || exact.InWindow != 1 || !exact.Window.From.Equal(observed) {
		t.Fatalf("%v %+v", e, exact)
	}
	// The sample itself is checked: a row outside the cohort's identities or
	// after its cutoff never aggregates.
	bad := sample
	bad.Rows = append([]SampleRow(nil), sample.Rows...)
	bad.Rows[0].Class.TaskID = "other"
	if _, e := Aggregate(bad, classWindow(sample)); e == nil {
		t.Fatal("foreign class aggregated")
	}
	bad.Rows[0] = sample.Rows[0]
	bad.Rows[0].ObservedAt = sample.Cutoff.Add(time.Second)
	if _, e := Aggregate(bad, classWindow(sample)); e == nil {
		t.Fatal("observation after the cutoff aggregated")
	}
	bad.Rows = append(bad.Rows[:1], sample.Rows[0])
	if _, e := Aggregate(bad, classWindow(sample)); e == nil {
		t.Fatal("duplicate member aggregated")
	}
}

func TestClassSampleRefusesWhatAnExportRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*exportAuthority)
	}{
		{"revoked at verification", func(a *exportAuthority) { a.revoked = true }},
		{"verification mutates material", func(a *exportAuthority) {
			a.verifyMaterial = func(_ Cohort, targets []Target) { targets[0].OutcomeReceiptIDs = nil }
		}},
		{"source unavailable", func(a *exportAuthority) { a.targets[0].SourceAvailable = false }},
		{"inventory incomplete", func(a *exportAuthority) { a.targets[0].InventoryComplete = false }},
		{"prediction substituted", func(a *exportAuthority) { a.cohort.Members[0].PredictionID = "other" }},
		{"unknown cohort", func(a *exportAuthority) { a.cohort.ID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			tc.change(a)
			if _, e := s.ClassSample(ctx, "cohort"); e == nil {
				t.Fatal("sample taken")
			}
		})
	}
	s, a, ctx := exportFixture(t)
	if _, e := s.ClassSample(ctx, ""); e == nil {
		t.Fatal("empty cohort id")
	}
	// A sample never carries features, sources, or receipt contents.
	sample, e := s.ClassSample(ctx, a.cohort.ID)
	if e != nil || len(sample.Rows) != 1 || sample.Rows[0].OutcomeReceipts != 1 || len(sample.Rows[0].BasisOutcomes) != 1 || sample.Rows[0].BasisOutcomes[0].Kind != decision.OutcomeCorrectness {
		t.Fatalf("%v %+v", e, sample)
	}
}
