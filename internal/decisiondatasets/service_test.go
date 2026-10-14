// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisiondatasets

import (
	"context"
	"encoding/json"
	"fmt"
	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"math"
	"strings"
	"testing"
	"time"
)

type exportAuthority struct {
	verifyMaterial func(Cohort, []Target)
	cohort         Cohort
	targets        []Target
	loads          int
	revoked        bool
	afterLoad      func()
	verify         func()
}

func (a *exportAuthority) Resolve(context.Context, auth.Decision, shoal.ID) (Cohort, error) {
	return a.cohort, nil
}
func (a *exportAuthority) Load(_ context.Context, _ auth.Decision, _ Cohort, _ Member) (Target, error) {
	v := a.targets[a.loads%len(a.targets)]
	a.loads++
	if a.afterLoad != nil {
		a.afterLoad()
	}
	return v, nil
}
func (a *exportAuthority) Verify(_ context.Context, _ auth.Decision, c Cohort, targets []Target) error {
	if a.verifyMaterial != nil {
		a.verifyMaterial(c, targets)
	}
	if a.verify != nil {
		a.verify()
	}
	if a.revoked {
		return denied()
	}
	return nil
}
func exportFixture(t *testing.T) (*Service, *exportAuthority, context.Context) {
	t.Helper()
	policy, p, pc, bc, _ := admissionFixture(t, 2)
	proposal, e := decision.NewAdjudicationProposal(policy, p, pc)
	if e != nil {
		t.Fatal(e)
	}
	basis, e := decision.NewAdjudicationBasis(proposal, bc)
	if e != nil {
		t.Fatal(e)
	}
	_, _, ctx, resolver, clock := admissionPredictionFixture(t)
	m := Member{ID: "row", TargetID: proposal.TargetID(), RequestID: pc.RequestID, PredictionID: pc.PredictionID, SubjectID: pc.SubjectID, FamilyIDs: []shoal.ID{"family"}, Split: "train"}
	c := Cohort{ID: "cohort", AuthorityRevisionID: "revision", InventoryID: "inventory", Task: p.Request().Task(), Policy: policy, QuestionID: pc.QuestionID, FeatureSchemaID: "features", FeatureBuilderID: "builder", SplitPolicyID: "splits", SamplingPolicyID: "sampling", Cutoff: clock(), Mode: "reconstructed", Members: []Member{m}}
	receipt := journal.Receipt{ID: shoal.ID("adjudication-receipt:" + strings.Repeat("c", 64)), Version: 1, TargetID: proposal.TargetID(), TaskID: proposal.TaskID(), PictureID: proposal.PictureID(), PolicyID: policy.ID(), ProposalID: proposal.ID(), BasisID: basis.ID(), ProposalConfig: pc, ReceivedAt: clock()}
	target := Target{Prediction: p, History: []journal.Receipt{receipt}, SelectedBasis: basis, Features: []float64{1, -2}, FeatureInputDigest: p.Request().Picture().Config().InputDigest, FeatureReceivedAt: clock(), InventoryID: "inventory", OutcomeReceiptIDs: pc.ObservationReceiptIDs, InventoryComplete: true, SourceAvailable: true, TrainingAllowed: true}
	a := &exportAuthority{cohort: c, targets: []Target{target}}
	s, e := New(Config{Resolver: resolver, Authority: a, Clock: clock})
	if e != nil {
		t.Fatal(e)
	}
	return s, a, ctx
}
func datasetRow(t *testing.T, b Bundle) map[string]any {
	t.Helper()
	var d map[string]any
	if e := json.Unmarshal(b.Dataset, &d); e != nil {
		t.Fatal(e)
	}
	return d["rows"].([]any)[0].(map[string]any)
}
func TestExportVerifiedAndCurrentQuarantine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Target)
		status string
	}{
		{"verified", func(*Target) {}, "verified"}, {"new report", func(v *Target) {
			v.OutcomeReceiptIDs = append(v.OutcomeReceiptIDs, shoal.ID("outcome-receipt:"+strings.Repeat("d", 64)))
		}, "unknown"}, {"withdrawn", func(v *Target) { v.LabelWithdrawn = true }, "unknown"}, {"training denied", func(v *Target) { v.TrainingAllowed = false }, "unknown"}, {"no label", func(v *Target) { v.History = nil; v.SelectedBasis = decision.AdjudicationBasis{} }, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			tc.change(&a.targets[0])
			b, e := s.Export(ctx, a.cohort.ID)
			if e != nil {
				t.Fatal(e)
			}
			row := datasetRow(t, b)
			if row["label_status"] != tc.status {
				t.Fatal(row)
			}
			if tc.status != "verified" && row["label"] != nil {
				t.Fatal("unknown became a label")
			}
			if sum(b.Manifest) != b.ManifestSHA256 {
				t.Fatal("manifest digest mismatch")
			}
		})
	}
}
func TestExportRejectsUnavailableOrFutureMaterial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*exportAuthority)
	}{
		{"source unavailable", func(a *exportAuthority) { a.targets[0].SourceAvailable = false }}, {"inventory incomplete", func(a *exportAuthority) { a.targets[0].InventoryComplete = false }}, {"feature future", func(a *exportAuthority) { a.targets[0].FeatureReceivedAt = a.cohort.Cutoff.Add(time.Nanosecond) }}, {"wrong feature digest", func(a *exportAuthority) { a.targets[0].FeatureInputDigest = strings.Repeat("b", 64) }}, {"substituted basis", func(a *exportAuthority) { a.targets[0].SelectedBasis = decision.AdjudicationBasis{} }}, {"revocation during last load", func(a *exportAuthority) { a.afterLoad = func() { a.revoked = true } }}, {"changed generation final check", func(a *exportAuthority) { a.verify = func() { a.revoked = true } }}, {"duplicate current outcome", func(a *exportAuthority) {
			a.targets[0].OutcomeReceiptIDs = append(a.targets[0].OutcomeReceiptIDs, a.targets[0].OutcomeReceiptIDs[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			tc.change(a)
			b, e := s.Export(ctx, a.cohort.ID)
			if !shoal.IsErrorCode(e, shoal.ErrorNotFound) || len(b.Dataset) != 0 || len(b.Manifest) != 0 {
				t.Fatalf("export disclosed denied material: %v", e)
			}
		})
	}
}
func TestFutureAdjudicationCannotSupplyEarlierLabel(t *testing.T) {
	s, a, ctx := exportFixture(t)
	a.cohort.Cutoff = a.cohort.Cutoff.Add(time.Second)
	s.config.Clock = func() time.Time { return a.cohort.Cutoff.Add(time.Minute) }
	a.targets[0].History[0].ReceivedAt = a.cohort.Cutoff.Add(time.Second)
	a.targets[0].SelectedBasis = decision.AdjudicationBasis{}
	b, e := s.Export(ctx, a.cohort.ID)
	if e != nil {
		t.Fatal(e)
	}
	r := datasetRow(t, b)
	if r["label"] != nil || r["label_status"] != "unknown" {
		t.Fatal("future label leaked")
	}
}
func TestRegisteredFamilyIsolationIncludesQuarantinedMembers(t *testing.T) {
	s, a, ctx := exportFixture(t)
	m := a.cohort.Members[0]
	m.ID = "row2"
	m.TargetID = "different-target"
	m.Split = "test"
	a.cohort.Members = append(a.cohort.Members, m)
	a.targets[0].TrainingAllowed = false
	if _, e := s.Export(ctx, a.cohort.ID); e == nil {
		t.Fatal("cross-split family admitted")
	}
	if a.loads != 0 {
		t.Fatal("invalid cohort loaded")
	}
}
func TestCancellationInFinalVerification(t *testing.T) {
	s, a, ctx := exportFixture(t)
	ctx, cancel := context.WithCancel(ctx)
	a.verify = cancel
	if b, e := s.Export(ctx, a.cohort.ID); e == nil || len(b.Dataset) != 0 {
		t.Fatal("cancelled export disclosed")
	}
}
func TestTypedNilAuthorityRejected(t *testing.T) {
	s, _, _ := exportFixture(t)
	var a *exportAuthority
	s.config.Authority = a
	if _, e := New(s.config); e == nil {
		t.Fatal("typed nil authority admitted")
	}
}

func TestContentIsolationIncludesUnlabeledMembers(t *testing.T) {
	s, a, ctx := exportFixture(t)
	first := a.targets[0]
	old := first.Prediction.Request()
	pc := old.Picture().Config()
	pc.Subjects[0].ID = "second-subject"
	picture, e := decision.NewPictureManifest(old.Picture().ContextPack(), pc)
	if e != nil {
		t.Fatal(e)
	}
	rc := old.Config()
	rc.SubjectIDs = []shoal.ID{"second-subject"}
	request, e := decision.NewDecisionRequest(old.Task(), picture, old.Predictor(), rc)
	if e != nil {
		t.Fatal(e)
	}
	result := first.Prediction.Config()
	result.RequestID = request.ID()
	result.Answers[0].SubjectID = "second-subject"
	pred, e := decision.NewPredictionRecord(request, result)
	if e != nil {
		t.Fatal(e)
	}
	target, e := decision.AdjudicationTargetID(old.TaskID(), picture.ID(), "second-subject", a.cohort.QuestionID)
	if e != nil {
		t.Fatal(e)
	}
	m := a.cohort.Members[0]
	m.ID = "row2"
	m.TargetID = target
	m.RequestID = request.ID()
	m.PredictionID = pred.ID()
	m.SubjectID = "second-subject"
	m.FamilyIDs = []shoal.ID{"different-family"}
	m.Split = "test"
	a.cohort.Members = append(a.cohort.Members, m)
	second := first
	second.Prediction = pred
	second.History = nil
	second.SelectedBasis = decision.AdjudicationBasis{}
	second.TrainingAllowed = false
	a.targets = append(a.targets, second)
	if b, e := s.Export(ctx, a.cohort.ID); e == nil || len(b.Dataset) != 0 {
		t.Fatal("cross-split identical source exported")
	}
}
func TestUnresolvedAndDisputedNeverNegative(t *testing.T) {
	for _, disposition := range []decision.AdjudicationDisposition{decision.AdjudicationUnresolved, decision.AdjudicationDisputed} {
		t.Run(string(disposition), func(t *testing.T) {
			s, a, ctx := exportFixture(t)
			target := &a.targets[0]
			cfg := target.History[0].ProposalConfig
			cfg.Disposition = disposition
			cfg.Label = ""
			cfg.Reason = "insufficient corroboration"
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
			bundle, e := s.Export(ctx, a.cohort.ID)
			if e != nil {
				t.Fatal(e)
			}
			r := datasetRow(t, bundle)
			if r["label"] != nil || r["label_status"] == "verified" {
				t.Fatal("unresolved label converted to negative")
			}
		})
	}
}

func TestVerificationMutationCannotExportOldGrant(t *testing.T) {
	for _, mutate := range []func(Cohort, []Target){
		func(_ Cohort, v []Target) {
			v[0].TrainingAllowed = false
			v[0].LabelWithdrawn = true
			v[0].Features[0] = 42
		},
		func(c Cohort, _ []Target) { c.Members[0].FamilyIDs[0] = "changed" },
		func(_ Cohort, v []Target) { v[0].History[0].ProposalConfig.WitnessIDs[0] = "changed" },
		func(_ Cohort, v []Target) { v[0].OutcomeReceiptIDs[0] = "changed" },
	} {
		s, a, ctx := exportFixture(t)
		a.verifyMaterial = mutate
		if b, e := s.Export(ctx, a.cohort.ID); e == nil || len(b.Dataset) != 0 || len(b.Manifest) != 0 {
			t.Fatal("mutating verification disclosed old material")
		}
	}
}
func TestHydratedBudgetStopsLoadingBeforeAllMembers(t *testing.T) {
	s, a, ctx := exportFixture(t)
	a.targets[0].Features = []float64{1}
	// Every individual proposal is valid and bounded; the aggregate retained
	// histories would exceed the exporter budget without incremental accounting.
	base := a.targets[0].History[0]
	a.targets[0].History = nil
	var previous shoal.ID
	for i := 0; i < 128; i++ {
		r := base
		c := r.ProposalConfig
		c.Disposition = decision.AdjudicationUnresolved
		c.Label = ""
		c.Reason = strings.Repeat("r", 4000)
		c.ExpectedVersion = int64(i)
		c.ExpectedHeadID = previous
		p, e := decision.NewAdjudicationProposal(a.cohort.Policy, a.targets[0].Prediction, c)
		if e != nil {
			t.Fatal(e)
		}
		r.ProposalConfig = c
		r.ProposalID = p.ID()
		r.ID = shoal.ID(fmt.Sprintf("adjudication-receipt:%064x", i+1))
		r.Version = int64(i + 1)
		previous = r.ID
		a.targets[0].History = append(a.targets[0].History, r)
	}
	member := a.cohort.Members[0]
	a.cohort.Members = nil
	for i := 0; i < 256; i++ {
		m := member
		m.ID = shoal.ID(fmt.Sprintf("row:%d", i))
		m.TargetID = shoal.ID(fmt.Sprintf("target:%d", i))
		a.cohort.Members = append(a.cohort.Members, m)
	}
	if b, e := s.Export(ctx, a.cohort.ID); e == nil || len(b.Dataset) != 0 {
		t.Fatal("unbounded history admitted")
	}
	if a.loads < 2 || a.loads >= len(a.cohort.Members) {
		t.Fatalf("hydrated budget did not stop loads early: %d", a.loads)
	}
}
func TestDetachedMutableMaterial(t *testing.T) {
	_, a, _ := exportFixture(t)
	v := a.targets[0]
	truth := true
	v.History[0].ProposalConfig.Truth = &truth
	v.History[0].Adjudicator.OnBehalfOf = []shoal.ID{shoal.ID(string([]byte{255, 0}))}
	detached := cloneTarget(v)
	v.Features[0] = 99
	v.History[0].ProposalConfig.WitnessIDs[0] = "changed"
	*v.History[0].ProposalConfig.Truth = false
	v.History[0].Adjudicator.OnBehalfOf[0] = "changed"
	if detached.Features[0] == 99 || !*detached.History[0].ProposalConfig.Truth || detached.History[0].Adjudicator.OnBehalfOf[0] != shoal.ID(string([]byte{255, 0})) {
		t.Fatal("mutable material was shared or opaque identities lost")
	}
}

func TestVerificationCannotChangeSignedFeatureBits(t *testing.T) {
	s, a, ctx := exportFixture(t)
	a.targets[0].Features[0] = math.Copysign(0, -1)
	a.verifyMaterial = func(_ Cohort, t []Target) { t[0].Features[0] = 0 }
	if b, e := s.Export(ctx, a.cohort.ID); e == nil || len(b.Dataset) != 0 {
		t.Fatal("verified different feature bits")
	}
}
