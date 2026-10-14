// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionbasisstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func predictionFixture(t *testing.T) (decision.PredictionRecord, auth.Decision, context.Context, auth.Resolver, func() time.Time) {
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

func proposalFixture(t *testing.T) (decision.LabelPolicy, decision.PredictionRecord, decision.AdjudicationProposalConfig, func() time.Time) {
	t.Helper()
	old, _, _, _, clock := predictionFixture(t)
	policy, e := decision.NewLabelPolicy(decision.LabelPolicyConfig{OwnerID: "authority", Version: "1", AdjudicatorRoleID: "adjudicator", DisputeResolverRoleID: "resolver", TrainingPurposeID: "training", MinIndependentWitnesses: 1})
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
func proposed(t *testing.T, policy decision.LabelPolicy, p decision.PredictionRecord, c decision.AdjudicationProposalConfig) decision.AdjudicationProposal {
	t.Helper()
	v, e := decision.NewAdjudicationProposal(policy, p, c)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

type memoryCAS struct {
	mu        sync.Mutex
	cells     map[string]allocator.Cell
	writes    atomic.Int64
	beforeCAS func()
	afterCAS  func()
	unknown   bool
	readFail  atomic.Bool
}

func (b *memoryCAS) ReadExact(_ context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	if b.readFail.Load() {
		return nil, errors.New("private read failure")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []allocator.Cell
	for _, c := range coords {
		if cell, ok := b.cells[string(c.Row)]; ok {
			cell.Value = append([]byte(nil), cell.Value...)
			result = append(result, cell)
		}
	}
	return result, nil
}
func (b *memoryCAS) CompareAndMutate(_ context.Context, m allocator.Mutation) (allocator.Status, error) {
	b.writes.Add(1)
	if b.beforeCAS != nil {
		b.beforeCAS()
	}
	b.mu.Lock()
	cell, exists := b.cells[string(m.Row)]
	condition := m.Conditions[0]
	if (condition.Absent && exists) || (!condition.Absent && (!exists || !bytes.Equal(cell.Value, condition.Value) || condition.TimestampSet && cell.Timestamp != condition.Timestamp)) {
		b.mu.Unlock()
		return allocator.StatusRejected, nil
	}
	if b.cells == nil {
		b.cells = map[string]allocator.Cell{}
	}
	u := m.Updates[0]
	b.cells[string(m.Row)] = allocator.Cell{Coordinate: u.Coordinate, Value: append([]byte(nil), u.Value...), Timestamp: u.Timestamp}
	b.mu.Unlock()
	if b.afterCAS != nil {
		b.afterCAS()
	}
	if b.unknown {
		return allocator.StatusUnknown, errors.New("private lost acknowledgment")
	}
	return allocator.StatusAccepted, nil
}
func fixture(t *testing.T) (decision.AdjudicationBasis, decision.LabelPolicy, decision.PredictionRecord) {
	t.Helper()
	policy, p, c, clock := proposalFixture(t)
	proposal := proposed(t, policy, p, c)
	person := decision.BasisIdentity{SubjectID: []byte{255, 0}, ActorID: []byte{254}, ClientID: []byte{253}, OnBehalfOf: [][]byte{{252}}}
	cfg := decision.AdjudicationBasisConfig{AuthorityID: "authority", AuthorityRevisionID: "revision", EnumerationID: "inventory", CapturedAt: clock().Add(time.Minute), Cutoff: clock().Add(time.Second), InventoryComplete: true, ControllersComplete: true, SourceControllers: []decision.BasisIdentity{person}, RoleEvidenceIDs: []shoal.ID{"role"}}
	for _, id := range proposal.Config().ObservationReceiptIDs {
		cfg.Outcomes = append(cfg.Outcomes, decision.BasisOutcome{ReceiptID: id, ObservationID: "observation", RequestID: c.RequestID, PredictionID: c.PredictionID, TaskID: proposal.TaskID(), PictureID: proposal.PictureID(), SubjectID: c.SubjectID, QuestionID: c.QuestionID, Kind: decision.OutcomeCorrectness, Reporter: person, ReceivedAt: clock()})
	}
	for _, id := range proposal.Config().WitnessIDs {
		cfg.Witnesses = append(cfg.Witnesses, decision.BasisWitness{ID: id, Digest: strings.Repeat("a", 64), VerificationReceiptID: "verification", OriginGroupID: "shared-origin", Origin: person, ControllerIdentities: []decision.BasisIdentity{person}, ReceivedAt: clock(), VerifiedAt: clock().Add(time.Second)})
	}
	basis, e := decision.NewAdjudicationBasis(proposal, cfg)
	if e != nil {
		t.Fatal(e)
	}
	return basis, policy, p
}
func TestExactRetentionReplayAndLoadPreserveOpaqueBytes(t *testing.T) {
	basis, _, _ := fixture(t)
	b := &memoryCAS{}
	s, e := New(Config{Backend: b})
	if e != nil {
		t.Fatal(e)
	}
	scope := Scope{[]byte{'d', 0, 255}}
	for i := 0; i < 2; i++ {
		if e = s.Retain(context.Background(), scope, basis); e != nil {
			t.Fatal(e)
		}
	}
	if b.writes.Load() != 1 {
		t.Fatal("exact retry attempted mutation")
	}
	loaded, e := s.Load(context.Background(), scope, basis.ID(), basis.Proposal())
	if e != nil || loaded.ID() != basis.ID() || !reflect.DeepEqual(loaded.Config(), basis.Config()) {
		t.Fatal("retention changed basis", e)
	}
	copy := loaded.Config()
	copy.Outcomes[0].Reporter.SubjectID[0] = 1
	again, e := s.Load(context.Background(), scope, basis.ID(), basis.Proposal())
	if e != nil || again.Config().Outcomes[0].Reporter.SubjectID[0] != 255 {
		t.Fatal("mutable copy escaped", e)
	}
	if _, e = s.Load(context.Background(), Scope{[]byte("other")}, basis.ID(), basis.Proposal()); !errors.Is(e, ErrNotFound) {
		t.Fatal("cross-domain load", e)
	}
}
func TestConcurrentRetainHasOneImmutableValue(t *testing.T) {
	basis, _, _ := fixture(t)
	b := &memoryCAS{}
	var barrier sync.WaitGroup
	barrier.Add(2)
	b.beforeCAS = func() { barrier.Done(); barrier.Wait() }
	s, _ := New(Config{Backend: b})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- s.Retain(context.Background(), Scope{[]byte("domain")}, basis) }()
	}
	for i := 0; i < 2; i++ {
		if e := <-results; e != nil {
			t.Fatal(e)
		}
	}
	if len(b.cells) != 1 {
		t.Fatal("immutable retain forked")
	}
}
func TestLostAcknowledgementAndUncertainReadback(t *testing.T) {
	for _, mode := range []string{"confirmed", "read-failed", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			basis, _, _ := fixture(t)
			b := &memoryCAS{unknown: true}
			ctx := context.Background()
			switch mode {
			case "read-failed":
				b.afterCAS = func() { b.readFail.Store(true) }
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				b.afterCAS = cancel
			}
			s, _ := New(Config{Backend: b})
			e := s.Retain(ctx, Scope{[]byte("domain")}, basis)
			if mode == "confirmed" {
				if e != nil {
					t.Fatal("exact readback did not reconcile", e)
				}
			} else if !errors.Is(e, ErrIndeterminate) {
				t.Fatal("lost mutation uncertainty", e)
			}
		})
	}
}
func TestRejectProposalAndBasisSubstitution(t *testing.T) {
	basis, policy, p := fixture(t)
	s, _ := New(Config{Backend: &memoryCAS{}})
	scope := Scope{[]byte("domain")}
	if e := s.Retain(context.Background(), scope, basis); e != nil {
		t.Fatal(e)
	}
	cfg := basis.Proposal().Config()
	cfg.Label = "ordinary"
	other := proposed(t, policy, p, cfg)
	if _, e := s.Load(context.Background(), scope, basis.ID(), other); !errors.Is(e, ErrCorrupt) {
		t.Fatal("proposal substitution accepted", e)
	}
	if _, e := s.Load(context.Background(), scope, "decision:adjudication-basis:v1:"+shoal.ID(strings.Repeat("0", 64)), basis.Proposal()); !errors.Is(e, ErrNotFound) {
		t.Fatal("missing basis returned", e)
	}
	if e := s.Retain(context.Background(), scope, decision.AdjudicationBasis{}); e == nil {
		t.Fatal("zero basis retained")
	}
}
func TestCanonicalCodecRejectsCorruptionAndRebinding(t *testing.T) {
	basis, _, _ := fixture(t)
	b := &memoryCAS{}
	s, _ := New(Config{Backend: b})
	scope := Scope{[]byte("domain")}
	if e := s.Retain(context.Background(), scope, basis); e != nil {
		t.Fatal(e)
	}
	coord, _ := s.coordinate(scope, basis.ID())
	original := b.cells[string(coord.Row)]
	for _, mutate := range []func(*payload){func(p *payload) { p.ProposalID = "other" }, func(p *payload) { p.BasisID = "other" }, func(p *payload) { p.Config.AuthorityRevisionID = "other" }, func(p *payload) { p.Config.InventoryComplete = false }, func(p *payload) { p.Config.Witnesses[0].Digest = strings.Repeat("b", 64) }} {
		var env envelope
		if e := json.Unmarshal(original.Value, &env); e != nil {
			t.Fatal(e)
		}
		var p payload
		if e := json.Unmarshal(env.Payload, &p); e != nil {
			t.Fatal(e)
		}
		mutate(&p)
		env.Payload, _ = json.Marshal(p)
		env.Checksum = hash(env.Payload)
		raw, _ := json.Marshal(env)
		cell := original
		cell.Value = raw
		b.cells[string(coord.Row)] = cell
		if _, e := s.Load(context.Background(), scope, basis.ID(), basis.Proposal()); !errors.Is(e, ErrCorrupt) {
			t.Fatal("modified basis accepted", e)
		}
		before := b.writes.Load()
		if e := s.Retain(context.Background(), scope, basis); !errors.Is(e, ErrConflict) || b.writes.Load() != before {
			t.Fatal("changed immutable row overwritten", e)
		}
	}
	for _, raw := range [][]byte{[]byte(`{}`), append(append([]byte{}, original.Value...), byte(' ')), bytes.Replace(original.Value, []byte(`"Schema":1`), []byte(`"Schema":1,"Schema":1`), 1)} {
		cell := original
		cell.Value = raw
		b.cells[string(coord.Row)] = cell
		if _, e := s.Load(context.Background(), scope, basis.ID(), basis.Proposal()); !errors.Is(e, ErrCorrupt) {
			t.Fatal("ambiguous encoding accepted", e)
		}
	}
}
func TestBoundedDecodeBeforeWideAllocation(t *testing.T) {
	for _, raw := range []string{`{"Outcomes":[` + strings.TrimSuffix(strings.Repeat("{},", decision.MaxAdjudicationObservations+1), ",") + `]}`, `{"Witnesses":[` + strings.TrimSuffix(strings.Repeat("{},", decision.MaxAdjudicationWitnesses+1), ",") + `]}`, strings.Repeat("[", 17) + "0" + strings.Repeat("]", 17), `{"OnBehalfOf":[` + strings.TrimSuffix(strings.Repeat(`"",`, auth.MaxOnBehalfOfEntries+1), ",") + `]}`} {
		if e := boundedShape([]byte(raw)); e == nil {
			t.Fatal("unbounded shape admitted")
		}
	}
	basis, _, _ := fixture(t)
	if _, e := decode(make([]byte, MaxStoredBytes+1), basis.ID(), basis.Proposal()); !errors.Is(e, ErrCorrupt) {
		t.Fatal("oversized envelope admitted", e)
	}
}
func TestRealEngineRFileRestartRetainsBasis(t *testing.T) {
	basis, _, _ := fixture(t)
	scope := Scope{[]byte("domain")}
	dir := t.TempDir()
	for run := 0; run < 2; run++ {
		eng, e := engine.Open(dir, engine.Options{})
		if e != nil {
			t.Fatal(e)
		}
		if run == 0 {
			if e = eng.CreateTable(Table, engine.TableOptions{}); e != nil {
				t.Fatal(e)
			}
		}
		backend, e := explorercoord.NewEngineStore(eng, Table)
		if e != nil {
			t.Fatal(e)
		}
		s, e := New(Config{Backend: backend})
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Retain(context.Background(), scope, basis); e != nil {
			t.Fatal(e)
		}
		loaded, e := s.Load(context.Background(), scope, basis.ID(), basis.Proposal())
		if e != nil || !reflect.DeepEqual(loaded.Config(), basis.Config()) {
			t.Fatal("restart changed basis", e)
		}
		if e = eng.Flush(Table); e != nil {
			t.Fatal(e)
		}
		files := 0
		if e = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !entry.IsDir() && filepath.Ext(path) == ".rf" {
				files++
			}
			return nil
		}); e != nil || files == 0 {
			t.Fatal("no physical RFile", e)
		}
		if e = eng.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
