// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisiondatasets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"strings"
	"testing"
	"time"
)

func admissionPredictionFixture(t *testing.T) (decision.PredictionRecord, auth.Decision, context.Context, auth.Resolver, func() time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var d auth.Decision
	var authority *auth.Authority
	var ctx context.Context
	cfg := auth.DecisionConfig{Subject: "principal", Actor: "actor", ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve, auth.OperationIngest}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "http-request", CorrelationID: "correlation", AuditPurpose: "decision-test"}
	var err error
	d, err = auth.NewDecision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	authority, err = auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = authority.Binder().Bind(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(d)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"authority:1"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: "review", Weight: 1, Labels: []decision.LabelPriority{{Label: "inspect", Value: 1}, {Label: "ordinary", Value: 0}}}}})
	if err != nil {
		t.Fatal(err)
	}
	task, err := decision.NewTaskSpec(decision.TaskConfig{OwnerID: "owner", Name: "review-priority", Version: "1", InputSchemaID: "schema", EvidencePolicyID: ep.ID(), LabelPolicyID: "labels", EvaluationPolicyID: "evaluation", PredictionUnit: "subject", LabelUnit: "finding", ActionUnit: "inspection", AggregationID: plan.ID(), Questions: []decision.Question{{ID: "review", Kind: decision.Choice, RubricID: "rubric", Labels: []string{"inspect", "ordinary"}}}})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := inference.NewDocumentAnchor(document.Citation{DocumentID: "source", RevisionID: "revision", SectionID: "section", SpanID: "span", Range: document.SourceRange{End: document.SourcePosition{Offset: 4}}}, "code")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inference.NewSnapshotPin("snapshot", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	pin, err := inference.NewAuthPin(shoal.ID(fingerprint.String()), cfg.AuthenticationExpires)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("inspect", []inference.EvidenceAnchor{anchor}, nil, snapshot, pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("code")
	digest := sha256.Sum256(input)
	observed := now.Add(-time.Minute)
	picture, err := decision.NewPictureManifest(pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "observation", EnumerationID: "inventory", ScopeID: "scope", BuilderID: "builder", OntologyProjectionID: "projection", InputDigest: hex.EncodeToString(digest[:]), TokenizerID: "tokenizer", InputTokens: 1, TokenBudget: 100, Cutoff: now, Sources: []decision.Source{{ID: "s", ArtifactID: "source", RevisionID: "revision", Digest: hex.EncodeToString(digest[:]), OriginID: "origin", AuthorityPolicyID: "authority:1", Role: decision.Observation, Control: decision.CandidateControlled, ObservedAt: observed, ReceivedAt: now}}, Subjects: []decision.Subject{{ID: "subject", SourceID: "s", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}}}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := decision.NewPredictorIdentity(decision.PredictorConfig{Provider: "local", RuntimeID: "runtime", WeightsDigest: strings.Repeat("a", 64), TokenizerDigest: strings.Repeat("b", 64), EnvironmentDigest: strings.Repeat("c", 64), FormattingID: "format", PreprocessingID: "preprocess", CalibrationID: "uncalibrated", Device: "cpu", Precision: "float64", BatchPolicyID: "batch"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := decision.NewDecisionRequest(task, picture, model, decision.RequestConfig{PrincipalID: cfg.Subject, ReleaseID: "release:1", CorrelationID: "call", RequestedAt: now, Deadline: now.Add(time.Minute), SubjectIDs: []shoal.ID{"subject"}})
	if err != nil {
		t.Fatal(err)
	}
	p, e := decision.NewPredictionRecord(request, decision.ResultConfig{RequestID: request.ID(), PredictorID: model.ID(), EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: now, Answers: []decision.Answer{{SubjectID: "subject", QuestionID: "review", Status: decision.Answered, Label: "inspect"}}})
	if e != nil {
		t.Fatal(e)
	}
	return p, d, ctx, authority.Resolver(), clock
}

func admissionProposalFixture(t *testing.T) (decision.LabelPolicy, decision.PredictionRecord, decision.AdjudicationProposalConfig, func() time.Time) {
	t.Helper()
	old, _, _, _, clock := admissionPredictionFixture(t)
	policy, e := decision.NewLabelPolicy(decision.LabelPolicyConfig{OwnerID: "authority", Version: "1", AdjudicatorRoleID: "adjudicator", DisputeResolverRoleID: "resolver", TrainingPurposeID: "training", MinIndependentWitnesses: 2})
	if e != nil {
		t.Fatal(e)
	}
	tc := old.Request().Task().Config()
	tc.LabelPolicyID = policy.ID()
	task, e := decision.NewTaskSpec(tc)
	if e != nil {
		t.Fatal(e)
	}
	pc := old.Request().Picture().Config()
	pc.TaskID = task.ID()
	picture, e := decision.NewPictureManifest(old.Request().Picture().ContextPack(), pc)
	if e != nil {
		t.Fatal(e)
	}
	request, e := decision.NewDecisionRequest(task, picture, old.Request().Predictor(), old.Request().Config())
	if e != nil {
		t.Fatal(e)
	}
	rc := old.Config()
	rc.RequestID = request.ID()
	p, e := decision.NewPredictionRecord(request, rc)
	if e != nil {
		t.Fatal(e)
	}
	cfg := decision.AdjudicationProposalConfig{RequestID: request.ID(), PredictionID: p.ID(), SubjectID: "subject", QuestionID: "review", ObservationReceiptIDs: []shoal.ID{shoal.ID("outcome-receipt:" + strings.Repeat("a", 64))}, WitnessIDs: []shoal.ID{"witness"}, Disposition: decision.AdjudicationVerified, Label: "inspect"}
	return policy, p, cfg, clock
}
func admissionProposed(t *testing.T, policy decision.LabelPolicy, p decision.PredictionRecord, c decision.AdjudicationProposalConfig) decision.AdjudicationProposal {
	t.Helper()
	v, e := decision.NewAdjudicationProposal(policy, p, c)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func admissionPerson(name string) decision.BasisIdentity {
	return decision.BasisIdentity{SubjectID: []byte(name), ActorID: []byte(name + ":actor"), ClientID: []byte("shared-client")}
}
func admissionFixture(t *testing.T, n int) (decision.LabelPolicy, decision.PredictionRecord, decision.AdjudicationProposalConfig, decision.AdjudicationBasisConfig, decision.BasisIdentity) {
	t.Helper()
	policy, p, pc, clock := admissionProposalFixture(t)
	pc.WitnessIDs = nil
	c := decision.AdjudicationBasisConfig{AuthorityID: "authority", AuthorityRevisionID: "revision", EnumerationID: "enumeration", CapturedAt: clock(), Cutoff: clock(), InventoryComplete: true, ControllersComplete: true, RoleEvidenceIDs: []shoal.ID{"roles"}, SourceControllers: []decision.BasisIdentity{admissionPerson("source")}}
	for i := 0; i < n; i++ {
		id := shoal.ID(fmt.Sprintf("witness:%d", i))
		pc.WitnessIDs = append(pc.WitnessIDs, id)
		c.Witnesses = append(c.Witnesses, decision.BasisWitness{ID: id, Digest: fmt.Sprintf("%064x", i+1), VerificationReceiptID: shoal.ID(fmt.Sprintf("verification:%d", i)), OriginGroupID: shoal.ID(fmt.Sprintf("group:%d", i)), Origin: admissionPerson(fmt.Sprintf("origin:%d", i)), ReceivedAt: clock(), VerifiedAt: clock()})
	}
	c.Outcomes = []decision.BasisOutcome{{ReceiptID: pc.ObservationReceiptIDs[0], ObservationID: "observation", RequestID: pc.RequestID, PredictionID: pc.PredictionID, TaskID: p.Request().TaskID(), PictureID: p.Request().PictureID(), SubjectID: pc.SubjectID, QuestionID: pc.QuestionID, Kind: decision.OutcomeCorrectness, Reporter: admissionPerson("reporter"), ReceivedAt: clock()}}
	return policy, p, pc, c, admissionPerson("judge")
}
