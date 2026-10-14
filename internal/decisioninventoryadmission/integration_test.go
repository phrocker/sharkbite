// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventoryadmission_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	admission "github.com/phrocker/shoal-oss/internal/decisioninventoryadmission"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func integrationPrediction(t *testing.T) (decision.PredictionRecord, auth.Decision, context.Context, auth.Resolver, func() time.Time) {
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

type integrationCAS struct {
	decisionstore.CAS
	writes                             int
	failReads, failWrites              bool
	beforeWrite, afterWrite, afterRead func()
}

func (b *integrationCAS) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	if b.failReads {
		return nil, errors.New("readback lost")
	}
	out, err := b.CAS.ReadExact(ctx, c)
	if b.afterRead != nil {
		b.afterRead()
	}
	return out, err
}
func (b *integrationCAS) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	b.writes++
	if b.beforeWrite != nil {
		b.beforeWrite()
	}
	if b.failWrites {
		return allocator.StatusUnknown, errors.New("write unavailable")
	}
	status, err := b.CAS.CompareAndMutate(ctx, m)
	if b.afterWrite != nil {
		b.afterWrite()
	}
	return status, err
}

type integrationPolicy struct {
	prediction decision.PredictionRecord
	binding    inventory.Binding
	denied     bool
	clock      func() time.Time
}

func (p *integrationPolicy) check(d auth.Decision) error {
	if p.denied {
		return auth.ObjectNotFound()
	}
	return d.AuthorizeObject(auth.OperationIngest, auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("evidence"), PolicyID: []byte("evidence-policy"), ObjectID: "source"}, p.clock())
}

type integrationOutcomeAuthority struct{ p *integrationPolicy }

func (a integrationOutcomeAuthority) Resolve(_ context.Context, d auth.Decision, req, pred shoal.ID, _ auth.Operation) (decision.PredictionRecord, error) {
	if req != a.p.prediction.Request().ID() || pred != a.p.prediction.ID() {
		return decision.PredictionRecord{}, auth.ObjectNotFound()
	}
	return a.p.prediction, a.p.check(d)
}
func (a integrationOutcomeAuthority) Verify(_ context.Context, d auth.Decision, _ []decision.OutcomeObservation) error {
	return a.p.check(d)
}

type integrationAdmissionAuthority struct{ p *integrationPolicy }

func (a integrationAdmissionAuthority) Resolve(_ context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation) (inventory.Binding, error) {
	if p.ID() != a.p.prediction.ID() || o.PredictionID() != p.ID() {
		return inventory.Binding{}, auth.ObjectNotFound()
	}
	return a.p.binding, a.p.check(d)
}
func (a integrationAdmissionAuthority) Verify(_ context.Context, d auth.Decision, b inventory.Binding, p decision.PredictionRecord, o decision.OutcomeObservation) error {
	if b != a.p.binding || p.ID() != a.p.prediction.ID() || o.PredictionID() != p.ID() {
		return auth.ObjectNotFound()
	}
	return a.p.check(d)
}

type integrationFixture struct {
	eng                    *engine.Engine
	dir                    string
	inv                    *inventory.Store
	store                  *outcomes.Store
	invBackend, outBackend *integrationCAS
	policy                 *integrationPolicy
	authn                  *auth.Authority
	ctx                    context.Context
	caller                 auth.Decision
	cfg                    decision.OutcomeObservationConfig
	clock                  func() time.Time
}

func openIntegration(t *testing.T, dir string, register bool) *integrationFixture {
	t.Helper()
	pred, d, _, _, clock := integrationPrediction(t)
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := &integrationFixture{eng: eng, dir: dir, caller: d, clock: clock}
	target, err := decision.AdjudicationTargetID(pred.Request().Task().ID(), pred.Request().Picture().ID(), "subject", "review")
	if err != nil {
		t.Fatal(err)
	}
	f.policy = &integrationPolicy{prediction: pred, binding: inventory.Binding{CoverageID: "guarded-test-namespace-v1", TargetID: target, TaskID: pred.Request().Task().ID(), PictureID: pred.Request().Picture().ID(), SubjectID: "subject", QuestionID: "review"}, clock: clock}
	for _, table := range []string{inventory.Table, outcomes.Table} {
		if register {
			if err = eng.CreateTable(table, engine.TableOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	ib, err := explorercoord.NewEngineStore(eng, inventory.Table)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := explorercoord.NewEngineStore(eng, outcomes.Table)
	if err != nil {
		t.Fatal(err)
	}
	f.invBackend = &integrationCAS{CAS: ib}
	f.outBackend = &integrationCAS{CAS: ob}
	f.inv, err = inventory.New(inventory.Config{Backend: f.invBackend, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if register {
		if _, err = f.inv.Register(context.Background(), inventory.Scope{Domain: []byte("domain")}, f.policy.binding); err != nil {
			t.Fatal(err)
		}
	}
	f.invBackend.writes = 0
	f.authn, err = auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	f.ctx, err = f.authn.Binder().Bind(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := admission.New(admission.Config{Store: f.inv, Authority: integrationAdmissionAuthority{f.policy}})
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = outcomes.New(outcomes.Config{Backend: f.outBackend, Resolver: f.authn.Resolver(), Authority: integrationOutcomeAuthority{f.policy}, Admission: adapter, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = decision.OutcomeObservationConfig{RequestID: pred.Request().ID(), PredictionID: pred.ID(), SubjectID: "subject", QuestionID: "review", Kind: decision.OutcomeCorrectness, Label: "inspect", EvidenceIDs: []shoal.ID{"source"}, ObservedAt: clock(), AssertedProvenance: decision.OutcomeProvenance{ReporterID: "claimed-reporter"}}
	return f
}
func (f *integrationFixture) append(ctx context.Context) (outcomes.Receipt, error) {
	return f.store.Append(ctx, f.cfg.RequestID, f.cfg.PredictionID, []byte("stable-key"), f.cfg)
}
func (f *integrationFixture) snapshot(t *testing.T) inventory.Snapshot {
	t.Helper()
	s, err := f.inv.Load(context.Background(), inventory.Scope{Domain: []byte("domain")}, f.policy.binding)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestIntegrationPendingPrecedesOutcomeAndExactRetryPublishes(t *testing.T) {
	for _, fault := range []string{"outcome-write", "outcome-readback", "publication-write", "publication-readback"} {
		t.Run(fault, func(t *testing.T) {
			f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
			defer f.eng.Close()
			f.outBackend.beforeWrite = func() {
				snap := f.snapshot(t)
				if snap.Complete() || len(snap.Entries) != 1 {
					t.Fatal("outcome CAS preceded durable pending")
				}
			}
			switch fault {
			case "outcome-write":
				f.outBackend.failWrites = true
			case "outcome-readback":
				f.outBackend.afterWrite = func() { f.outBackend.failReads = true }
			case "publication-write":
				f.outBackend.afterWrite = func() { f.invBackend.failWrites = true }
			case "publication-readback":
				f.outBackend.afterWrite = func() { f.invBackend.afterWrite = func() { f.invBackend.failReads = true } }
			}
			got, err := f.append(f.ctx)
			if err == nil || !errors.Is(err, outcomes.ErrIndeterminate) || got.ID != "" {
				t.Fatalf("uncertain append %+v %v", got, err)
			}
			f.invBackend.failReads = false
			f.invBackend.afterWrite = nil
			if snap := f.snapshot(t); len(snap.Entries) != 1 || snap.Complete() != (fault == "publication-readback") {
				t.Fatal("failed acknowledgement changed expected durable state")
			}
			f.outBackend.failWrites = false
			f.outBackend.failReads = false
			f.outBackend.afterWrite = nil
			f.invBackend.failWrites = false
			var original outcomes.Receipt
			if fault != "outcome-write" {
				original, err = f.store.Read(f.ctx, f.cfg.RequestID, f.cfg.PredictionID, []byte("stable-key"))
				if err != nil {
					t.Fatal(err)
				}
			}
			repaired, err := f.append(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if fault != "outcome-write" && !reflect.DeepEqual(original, repaired) {
				t.Fatal("repair regenerated original receipt")
			}
			if snap := f.snapshot(t); !snap.Complete() || len(snap.Entries) != 1 {
				t.Fatal("retry did not publish")
			}
			before := f.snapshot(t)
			again, err := f.append(f.ctx)
			if err != nil || !reflect.DeepEqual(again, repaired) || f.snapshot(t).ID != before.ID {
				t.Fatal("exact retry altered publication")
			}
			f.cfg.Label = "ordinary"
			if _, err := f.append(f.ctx); !errors.Is(err, outcomes.ErrConflict) || f.snapshot(t).ID != before.ID {
				t.Fatal("conflicting idempotency key altered inventory")
			}
		})
	}
}
func TestIntegrationReportersUnion(t *testing.T) {
	f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
	defer f.eng.Close()
	first, err := f.append(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := auth.NewDecision(auth.DecisionConfig{Subject: "other-reporter", Actor: "other-actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead, auth.OperationRetrieve, auth.OperationIngest}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: f.clock().Add(time.Hour), RequestID: "other-call"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := f.authn.Binder().Bind(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.append(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t)
	if first.ID == second.ID || len(snap.Entries) != 2 || !snap.Complete() {
		t.Fatal("principal inventories collapsed or split")
	}
}
func TestIntegrationRevocationDuringInventoryIO(t *testing.T) {
	for _, at := range []string{"begin-read", "publication-write"} {
		t.Run(at, func(t *testing.T) {
			f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
			defer f.eng.Close()
			if at == "begin-read" {
				f.invBackend.afterRead = func() { f.policy.denied = true }
			} else {
				f.outBackend.afterWrite = func() { f.invBackend.afterWrite = func() { f.policy.denied = true } }
			}
			got, err := f.append(f.ctx)
			if err == nil || got.ID != "" {
				t.Fatal("revoked outcome disclosed")
			}
			if at == "begin-read" && f.outBackend.writes != 0 {
				t.Fatal("outcome committed after inventory IO revoked source")
			}
			if at == "publication-write" && !errors.Is(err, outcomes.ErrIndeterminate) {
				t.Fatal("postcommit denial lost uncertainty")
			}
		})
	}
}
func TestIntegrationRFileRestartRepairsPendingPublication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "engine")
	f := openIntegration(t, dir, true)
	f.outBackend.afterWrite = func() { f.invBackend.failWrites = true }
	if _, err := f.append(f.ctx); !errors.Is(err, outcomes.ErrIndeterminate) {
		t.Fatalf("expected uncertain outcome: %v", err)
	}
	original, err := f.store.Read(f.ctx, f.cfg.RequestID, f.cfg.PredictionID, []byte("stable-key"))
	if err != nil {
		t.Fatal(err)
	}
	pending := f.snapshot(t)
	for _, table := range []string{inventory.Table, outcomes.Table} {
		if err = f.eng.Flush(table); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.eng.Close(); err != nil {
		t.Fatal(err)
	}
	files := 0
	if err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && strings.HasSuffix(path, ".rf") {
			files++
		}
		return nil
	}); err != nil || files < 2 {
		t.Fatalf("missing physical RFiles: %d %v", files, err)
	}
	restarted := openIntegration(t, dir, false)
	defer restarted.eng.Close()
	if got := restarted.snapshot(t); got.ID != pending.ID || got.Complete() {
		t.Fatal("pending lost across restart")
	}
	repaired, err := restarted.append(restarted.ctx)
	if err != nil || !reflect.DeepEqual(repaired, original) {
		t.Fatalf("restart regenerated outcome: %v", err)
	}
	if !restarted.snapshot(t).Complete() || restarted.outBackend.writes != 0 {
		t.Fatal("restart failed exact durable repair")
	}
}
