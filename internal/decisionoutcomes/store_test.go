// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T) (decision.PredictionRecord, auth.Decision, context.Context, auth.Resolver, func() time.Time) {
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

type memoryCAS struct {
	mu         sync.Mutex
	cells      map[string]allocator.Cell
	writes     atomic.Int64
	afterWrite func()
	writeError bool
	readError  atomic.Bool
}

func (m *memoryCAS) ReadExact(_ context.Context, coordinates []allocator.Coordinate) ([]allocator.Cell, error) {
	if m.readError.Load() {
		return nil, errors.New("private storage read")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var cells []allocator.Cell
	for _, c := range coordinates {
		if v, ok := m.cells[string(c.Row)]; ok {
			v.Value = append([]byte(nil), v.Value...)
			cells = append(cells, v)
		}
	}
	return cells, nil
}
func (m *memoryCAS) CompareAndMutate(_ context.Context, mutation allocator.Mutation) (allocator.Status, error) {
	m.writes.Add(1)
	m.mu.Lock()
	if _, ok := m.cells[string(mutation.Row)]; ok {
		m.mu.Unlock()
		return allocator.StatusRejected, nil
	}
	if m.cells == nil {
		m.cells = map[string]allocator.Cell{}
	}
	u := mutation.Updates[0]
	m.cells[string(mutation.Row)] = allocator.Cell{Coordinate: u.Coordinate, Timestamp: u.Timestamp, Value: append([]byte(nil), u.Value...)}
	m.mu.Unlock()
	if m.afterWrite != nil {
		m.afterWrite()
	}
	if m.writeError {
		return allocator.StatusUnknown, errors.New("private lost acknowledgement")
	}
	return allocator.StatusAccepted, nil
}

type outcomeAuthority struct {
	prediction     decision.PredictionRecord
	now            func() time.Time
	deny           atomic.Bool
	evidenceDenied sync.Map
	operations     sync.Map
}

func (a *outcomeAuthority) Resolve(_ context.Context, d auth.Decision, request, prediction shoal.ID, op auth.Operation) (decision.PredictionRecord, error) {
	a.operations.Store(op, true)
	if a.deny.Load() || request != a.prediction.Request().ID() || prediction != a.prediction.ID() {
		return decision.PredictionRecord{}, auth.ObjectNotFound()
	}
	e := d.AuthorizeObject(op, auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: request}, a.now())
	return a.prediction, e
}
func (a *outcomeAuthority) Verify(_ context.Context, d auth.Decision, chain []decision.OutcomeObservation) error {
	if a.deny.Load() {
		return auth.ObjectNotFound()
	}
	for _, o := range chain {
		for _, id := range o.Config().EvidenceIDs {
			if _, deny := a.evidenceDenied.Load(id); deny {
				return auth.ObjectNotFound()
			}
			if e := d.AuthorizeObject(auth.OperationRetrieve, auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("evidence"), PolicyID: []byte("evidence-policy"), ObjectID: id}, a.now()); e != nil {
				return e
			}
		}
	}
	return nil
}
func storeFixture(t *testing.T) (*Store, *memoryCAS, *outcomeAuthority, context.Context, decision.OutcomeObservationConfig) {
	t.Helper()
	p, _, ctx, resolver, clock := fixture(t)
	backend := &memoryCAS{}
	a := &outcomeAuthority{prediction: p, now: clock}
	s, e := New(Config{Backend: backend, Resolver: resolver, Authority: a, Clock: clock})
	if e != nil {
		t.Fatal(e)
	}
	cfg := decision.OutcomeObservationConfig{RequestID: p.Request().ID(), PredictionID: p.ID(), SubjectID: "subject", Kind: decision.OutcomeCorrectness, QuestionID: "review", Label: "inspect", EvidenceIDs: []shoal.ID{"finding"}, ObservedAt: clock(), AssertedProvenance: decision.OutcomeProvenance{ReporterID: "asserted-other-reporter"}}
	return s, backend, a, ctx, cfg
}
func appendCfg(s *Store, ctx context.Context, key string, c decision.OutcomeObservationConfig) (Receipt, error) {
	return s.Append(ctx, c.RequestID, c.PredictionID, []byte(key), c)
}
func TestExactRetryAttributionConflictAndRead(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	first, e := appendCfg(s, ctx, "key", c)
	if e != nil {
		t.Fatal(e)
	}
	if first.State != "proposed" || first.SubmitterID != "principal" || first.ActorID != "actor" || first.ClientID != "client" || first.ObservationConfig.AssertedProvenance.ReporterID == first.SubmitterID {
		t.Fatal("trusted attribution lost", first)
	}
	second, e := appendCfg(s, ctx, "key", c)
	if e != nil || !reflect.DeepEqual(first, second) || b.writes.Load() != 1 {
		t.Fatal("retry changed receipt", e)
	}
	read, e := s.Read(ctx, c.RequestID, c.PredictionID, []byte("key"))
	if e != nil || !reflect.DeepEqual(first, read) {
		t.Fatal("read changed receipt", e)
	}
	c.Label = "ordinary"
	if _, e = appendCfg(s, ctx, "key", c); !errors.Is(e, ErrConflict) || errors.Is(e, ErrIndeterminate) {
		t.Fatal("prior conflict incorrectly classified", e)
	}
	a.deny.Store(true)
	if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("missing")); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal(e)
	}
}
func TestConcurrentSameKeyReplaysImmutableWinner(t *testing.T) {
	s, b, _, ctx, c := storeFixture(t)
	const count = 12
	var wg sync.WaitGroup
	results := make(chan Receipt, count)
	failures := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := appendCfg(s, ctx, "key", c); results <- r; failures <- e }()
	}
	wg.Wait()
	close(results)
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first Receipt
	for r := range results {
		if first.ID == "" {
			first = r
		} else if !reflect.DeepEqual(first, r) {
			t.Fatal("concurrent winner differed")
		}
	}
	if b.writes.Load() < 1 {
		t.Fatal("no write")
	}
}
func TestCorrectionsPreserveBranchesAndRecheckAncestry(t *testing.T) {
	s, _, a, ctx, c := storeFixture(t)
	parent, e := appendCfg(s, ctx, "parent", c)
	if e != nil {
		t.Fatal(e)
	}
	c.Supersedes = parent.ID
	c.EvidenceIDs = []shoal.ID{"correction"}
	c.Label = "ordinary"
	first, e := appendCfg(s, ctx, "first", c)
	if e != nil {
		t.Fatal(e)
	}
	second, e := appendCfg(s, ctx, "second", c)
	if e != nil || first.ID == second.ID {
		t.Fatal("branches collapsed", e)
	}
	if _, ok := a.operations.Load(auth.OperationRead); !ok {
		t.Fatal("predecessor was not read-authorized")
	}
	a.evidenceDenied.Store(shoal.ID("finding"), true)
	if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("first")); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("ancestor evidence withdrawal ignored", e)
	}
	if _, e = appendCfg(s, ctx, "third", c); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("new correction laundered revoked ancestor", e)
	}
}
func TestRejectInvalidCorrectionLinksAndTimes(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	initial, e := appendCfg(s, ctx, "parent", c)
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"missing", "self", "kind", "action", "future"} {
		t.Run(kind, func(t *testing.T) {
			next := c
			next.Supersedes = initial.ID
			switch kind {
			case "missing":
				next.Supersedes = shoal.ID("outcome-receipt:" + strings.Repeat("a", 64))
			case "self":
				d, _, e := s.resolve(ctx, nil, c.RequestID, c.PredictionID, auth.OperationIngest)
				if e != nil {
					t.Fatal(e)
				}
				next.Supersedes = receiptID(scopeFor(d), c.RequestID, hash([]byte(kind)))
			case "kind":
				next.Kind = decision.OutcomeExecution
				next.QuestionID = ""
				next.Label = ""
				next.ActionID = "action"
				next.ExecutionStatus = decision.ExecutionSucceeded
			case "action":
				parent := c
				parent.Kind = decision.OutcomeExecution
				parent.QuestionID = ""
				parent.Label = ""
				parent.ActionID = "action"
				parent.ExecutionStatus = decision.ExecutionSucceeded
				r, e := appendCfg(s, ctx, "action-parent", parent)
				if e != nil {
					t.Fatal(e)
				}
				next = parent
				next.Supersedes = r.ID
				next.ActionID = "other-action"
			case "future":
				next.ObservedAt = a.now().Add(time.Nanosecond)
			}
			before := b.writes.Load()
			if _, e := appendCfg(s, ctx, kind, next); e == nil {
				t.Fatal("invalid link/time accepted")
			}
			if b.writes.Load() != before {
				t.Fatal("invalid correction attempted write")
			}
		})
	}
}
func TestUnknownCASAndPostCommitRevocationRemainUncertain(t *testing.T) {
	for _, kind := range []string{"lost-ack", "revoke", "read-fail", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, b, a, ctx, c := storeFixture(t)
			b.writeError = true
			switch kind {
			case "revoke":
				b.afterWrite = func() { a.deny.Store(true) }
			case "read-fail":
				b.afterWrite = func() { b.readError.Store(true) }
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				b.afterWrite = cancel
			}
			receipt, e := appendCfg(s, ctx, "key", c)
			if kind == "lost-ack" {
				if e != nil || receipt.ID == "" {
					t.Fatal("exact readback failed reconciliation", e)
				}
			} else if !errors.Is(e, ErrIndeterminate) || receipt.ID != "" {
				t.Fatal("post-CAS uncertainty or nondisclosure lost", e)
			}
		})
	}
}
func TestCodecCorruptionAndUnknownFieldsFailClosed(t *testing.T) {
	s, b, _, ctx, c := storeFixture(t)
	r, e := appendCfg(s, ctx, "key", c)
	if e != nil {
		t.Fatal(e)
	}
	original := b.cells[string(r.ID)]
	for _, value := range [][]byte{[]byte(`{}`), append(append([]byte{}, original.Value...), byte(' ')), bytes.Replace(original.Value, []byte(`"Schema":1`), []byte(`"Schema":1,"Schema":1`), 1)} {
		cell := original
		cell.Value = value
		b.cells[string(r.ID)] = cell
		if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("key")); !errors.Is(e, ErrUnavailable) {
			t.Fatal("corruption accepted", e)
		}
	}
}
func TestAncestryBound(t *testing.T) {
	s, b, _, ctx, c := storeFixture(t)
	for i := 0; i <= MaxOutcomeAncestors; i++ {
		r, e := appendCfg(s, ctx, string(rune('A'+i)), c)
		if e != nil {
			t.Fatal(i, e)
		}
		c.Supersedes = r.ID
	}
	before := b.writes.Load()
	if _, e := appendCfg(s, ctx, "beyond", c); e == nil || b.writes.Load() != before {
		t.Fatal("unbounded ancestry accepted", e)
	}
}
func TestRealEngineRestartRetainsReceipt(t *testing.T) {
	_, _, a, ctx, c := storeFixture(t)
	_, _, _, resolver, clock := fixture(t)
	dir := filepath.Join(t.TempDir(), "engine")
	var original Receipt
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
		// Bind the fixture decision to the resolver belonging to this store.
		_, d, _, _, _ := fixture(t)
		authority, _ := auth.NewAuthorityWithClock(clock)
		ctx, e = authority.Binder().Bind(ctx, d)
		if e != nil {
			t.Fatal(e)
		}
		resolver = authority.Resolver()
		s, e := New(Config{Backend: backend, Resolver: resolver, Authority: a, Clock: clock})
		if e != nil {
			t.Fatal(e)
		}
		r, e := appendCfg(s, ctx, "key", c)
		if e != nil {
			t.Fatal(e)
		}
		if run == 0 {
			original = r
		} else if !reflect.DeepEqual(original, r) {
			t.Fatal("restart changed receipt")
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
			t.Fatal("no durable RFile", e)
		}
		if e = eng.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

var _ decisionstore.CAS = (*memoryCAS)(nil)

func rebind(t *testing.T, s *Store, d auth.Decision, clock func() time.Time) context.Context {
	t.Helper()
	authority, e := auth.NewAuthorityWithClock(clock)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := authority.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	s.config.Resolver = authority.Resolver()
	return ctx
}
func caller(t *testing.T, now time.Time, actor string, generation int64) auth.Decision {
	t.Helper()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "principal", Actor: shoal.ID(actor), ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve, auth.OperationIngest}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: generation, AuthenticationExpires: now.Add(time.Hour), RequestID: "renewed"})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestPrincipalScopeAndGrantRotationPreserveAttribution(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	first, e := appendCfg(s, ctx, "key", c)
	if e != nil {
		t.Fatal(e)
	}
	current := a.now().Add(time.Minute)
	clock := func() time.Time { return current }
	s.config.Clock = clock
	ctx = rebind(t, s, caller(t, current, "actor", 2), clock)
	retry, e := appendCfg(s, ctx, "key", c)
	if e != nil || !reflect.DeepEqual(first, retry) {
		t.Fatal("new grants rewrote original attribution or receipt", e)
	}
	ctx = rebind(t, s, caller(t, current, "other-actor", 2), clock)
	if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("key")); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("different actor read original principal receipt", e)
	}
	correction := c
	correction.Supersedes = first.ID
	before := b.writes.Load()
	if _, e = appendCfg(s, ctx, "correction", correction); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || b.writes.Load() != before {
		t.Fatal("cross-principal predecessor accepted", e)
	}
}
func TestStoreClockAndExpiredTrustedDecisionsFailClosed(t *testing.T) {
	for _, kind := range []string{"zero", "out-of-range", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s, b, a, ctx, c := storeFixture(t)
			now := a.now()
			switch kind {
			case "zero":
				now = time.Time{}
			case "out-of-range":
				now = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "expired":
				now = now.Add(2 * time.Hour)
			}
			s.config.Clock = func() time.Time { return now }
			if _, e := appendCfg(s, ctx, "key", c); e == nil || b.writes.Load() != 0 {
				t.Fatal("bad clock/expired decision admitted", e)
			}
		})
	}
}
func TestCodecRejectsForgedScopeAndReceiptKeyBinding(t *testing.T) {
	s, b, _, ctx, c := storeFixture(t)
	r, e := appendCfg(s, ctx, "key", c)
	if e != nil {
		t.Fatal(e)
	}
	original := b.cells[string(r.ID)]
	for _, mutate := range []func(*row){func(v *row) { v.KeyDigest = strings.Repeat("a", 64) }, func(v *row) { v.ScopeDigest = strings.Repeat("b", 64) }, func(v *row) { v.Receipt.SubmitterID = "spoof" }, func(v *row) { v.Receipt.ReceivedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }} {
		value, e := decode(original.Value)
		if e != nil {
			t.Fatal(e)
		}
		mutate(&value)
		encoded, e := encode(value)
		if e != nil {
			continue
		}
		cell := original
		cell.Value = encoded
		b.cells[string(r.ID)] = cell
		if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("key")); e == nil {
			t.Fatal("forged persistence accepted")
		}
	}
}

type concurrentWinner struct {
	decisionstore.CAS
	first   atomic.Bool
	unknown bool
}

func (b *concurrentWinner) ReadExact(ctx context.Context, coordinates []allocator.Coordinate) ([]allocator.Cell, error) {
	if !b.first.Swap(true) {
		return nil, nil
	}
	return b.CAS.ReadExact(ctx, coordinates)
}
func (b *concurrentWinner) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	status, e := b.CAS.CompareAndMutate(ctx, m)
	if b.unknown {
		return allocator.StatusUnknown, errors.New("unknown write acknowledgement")
	}
	return status, e
}
func TestConcurrentConflictRequiresKnownRejection(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		s, b, _, ctx, c := storeFixture(t)
		if _, e := appendCfg(s, ctx, "key", c); e != nil {
			t.Fatal(e)
		}
		s.config.Backend = &concurrentWinner{CAS: b, unknown: unknown}
		c.Label = "ordinary"
		_, e := appendCfg(s, ctx, "key", c)
		if !errors.Is(e, ErrConflict) || errors.Is(e, ErrIndeterminate) != unknown {
			t.Fatalf("unknown=%v error=%v", unknown, e)
		}
	}
}
func TestReturnedReceiptDoesNotAliasPersistedObservation(t *testing.T) {
	s, _, _, ctx, c := storeFixture(t)
	r, e := appendCfg(s, ctx, "key", c)
	if e != nil {
		t.Fatal(e)
	}
	r.ObservationConfig.EvidenceIDs[0] = "modified"
	c.EvidenceIDs[0] = "caller-modified"
	read, e := s.Read(ctx, c.RequestID, c.PredictionID, []byte("key"))
	if e != nil || read.ObservationConfig.EvidenceIDs[0] != "finding" {
		t.Fatal("mutable observation aliases storage", e)
	}
}

func TestBinaryAuthenticatedAttributionSurvivesEngineReplay(t *testing.T) {
	for _, field := range []string{"subject", "actor", "client", "delegation"} {
		t.Run(field, func(t *testing.T) {
			_, _, a, _, c := storeFixture(t)
			now := a.now()
			opaque := shoal.ID(string([]byte{'x', 0, 0xff, 0xc0}))
			cfg := auth.DecisionConfig{Subject: "principal", Actor: "actor", ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve, auth.OperationIngest}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "request"}
			switch field {
			case "subject":
				cfg.Subject = opaque
			case "actor":
				cfg.Actor = opaque
			case "client":
				cfg.ClientID = opaque
			case "delegation":
				cfg.OnBehalfOf = []shoal.ID{opaque}
			}
			d, e := auth.NewDecision(cfg)
			if e != nil {
				t.Fatal(e)
			}
			authority, e := auth.NewAuthorityWithClock(a.now)
			if e != nil {
				t.Fatal(e)
			}
			ctx, e := authority.Binder().Bind(context.Background(), d)
			if e != nil {
				t.Fatal(e)
			}
			dir := t.TempDir()
			var original Receipt
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
				store, e := New(Config{Backend: backend, Resolver: authority.Resolver(), Authority: a, Clock: a.now})
				if e != nil {
					t.Fatal(e)
				}
				received, e := appendCfg(store, ctx, "binary-key", c)
				if e != nil {
					t.Fatal(e)
				}
				if received.SubmitterID != d.Subject() || received.ActorID != d.Actor() || received.ClientID != d.ClientID() || !reflect.DeepEqual(received.OnBehalfOf, d.OnBehalfOf()) {
					t.Fatal("opaque attribution changed", received)
				}
				if run == 0 {
					original = received
				} else if !reflect.DeepEqual(original, received) {
					t.Fatal("binary attribution changed on restart")
				}
				retry, e := appendCfg(store, ctx, "binary-key", c)
				if e != nil || !reflect.DeepEqual(received, retry) {
					t.Fatal("binary attribution exact retry failed", e)
				}
				read, e := store.Read(ctx, c.RequestID, c.PredictionID, []byte("binary-key"))
				if e != nil || !reflect.DeepEqual(received, read) {
					t.Fatal("binary attribution read failed", e)
				}
				if e = eng.Flush(Table); e != nil {
					t.Fatal(e)
				}
				if e = eng.Close(); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

type ancestryReadHook struct {
	decisionstore.CAS
	hook func([]allocator.Coordinate)
}

func (b *ancestryReadHook) ReadExact(ctx context.Context, coordinates []allocator.Coordinate) ([]allocator.Cell, error) {
	cells, e := b.CAS.ReadExact(ctx, coordinates)
	if b.hook != nil {
		b.hook(coordinates)
	}
	return cells, e
}
func TestAncestryReadRevocationRechecksWholeChain(t *testing.T) {
	for _, operation := range []string{"read", "append", "post-cas"} {
		for _, target := range []string{"current", "earlier-ancestor"} {
			t.Run(operation+"/"+target, func(t *testing.T) {
				s, b, a, ctx, c := storeFixture(t)
				parent, e := appendCfg(s, ctx, "parent", c)
				if e != nil {
					t.Fatal(e)
				}
				c.Supersedes = parent.ID
				c.EvidenceIDs = []shoal.ID{"middle-evidence"}
				middle, e := appendCfg(s, ctx, "middle", c)
				if e != nil {
					t.Fatal(e)
				}
				c.Supersedes = middle.ID
				c.EvidenceIDs = []shoal.ID{"current-evidence"}
				if _, e = appendCfg(s, ctx, "child", c); e != nil {
					t.Fatal(e)
				}
				var committed atomic.Bool
				hooks := 0
				b.afterWrite = func() { committed.Store(true) }
				s.config.Backend = &ancestryReadHook{CAS: b, hook: func(coordinates []allocator.Coordinate) {
					for _, coord := range coordinates {
						if string(coord.Row) == string(parent.ID) && (operation != "post-cas" || committed.Load()) {
							hooks++
							id := shoal.ID("current-evidence")
							if target == "earlier-ancestor" {
								id = "middle-evidence"
							}
							a.evidenceDenied.Store(id, true)
						}
					}
				}}
				before := b.writes.Load()
				var receipt Receipt
				if operation == "read" {
					receipt, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("child"))
				} else {
					receipt, e = appendCfg(s, ctx, "new-child", c)
				}
				if hooks == 0 || !shoal.IsErrorCode(e, shoal.ErrorNotFound) || receipt.ID != "" {
					t.Fatalf("revoked %s disclosed after ancestor read: hooks=%d receipt=%+v error=%v", target, hooks, receipt, e)
				}
				expectedWrites := before
				if operation == "post-cas" {
					expectedWrites++
					if !errors.Is(e, ErrIndeterminate) {
						t.Fatal("post-CAS denial lost uncertainty", e)
					}
				} else if errors.Is(e, ErrIndeterminate) {
					t.Fatal("pre-CAS/read-only denial marked uncertain", e)
				}
				if b.writes.Load() != expectedWrites {
					t.Fatal("unexpected write after revocation")
				}
			})
		}
	}
}

type jointOutcomeAuthority struct {
	*outcomeAuthority
	chains [][]shoal.ID
	deny   bool
}

func (a *jointOutcomeAuthority) Verify(ctx context.Context, d auth.Decision, chain []decision.OutcomeObservation) error {
	ids := make([]shoal.ID, len(chain))
	for i, o := range chain {
		ids[i] = o.ID()
	}
	a.chains = append(a.chains, ids)
	if a.deny && len(chain) > 1 {
		return auth.ObjectNotFound()
	}
	return a.outcomeAuthority.Verify(ctx, d, chain)
}
func TestJointAuthorityReceivesCompleteOrderedChainOncePerCheck(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	parent, e := appendCfg(s, ctx, "parent", c)
	if e != nil {
		t.Fatal(e)
	}
	c.Supersedes = parent.ID
	c.EvidenceIDs = []shoal.ID{"correction"}
	child, e := appendCfg(s, ctx, "child", c)
	if e != nil {
		t.Fatal(e)
	}
	joint := &jointOutcomeAuthority{outcomeAuthority: a, deny: true}
	s.config.Authority = joint
	if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("child")); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("joint disclosure denial ignored", e)
	}
	expected := []shoal.ID{child.ObservationID, parent.ObservationID}
	if len(joint.chains) != 1 || !reflect.DeepEqual(joint.chains[0], expected) {
		t.Fatal("authority did not receive one complete ordered chain", joint.chains)
	}
	joint.chains = nil
	before := b.writes.Load()
	if _, e = appendCfg(s, ctx, "second-child", c); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || b.writes.Load() != before {
		t.Fatal("joint denial admitted mutation", e)
	}
	if len(joint.chains) != 1 || !reflect.DeepEqual(joint.chains[0], expected) {
		t.Fatal("append did not batch full chain", joint.chains)
	}
}

func TestIncompatiblePredecessorErrorDoesNotDiscloseRevokedEvidence(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	execution := c
	execution.Kind = decision.OutcomeExecution
	execution.QuestionID = ""
	execution.Label = ""
	execution.ActionID = "action"
	execution.ExecutionStatus = decision.ExecutionSucceeded
	execution.EvidenceIDs = []shoal.ID{"private-action-evidence"}
	parent, e := appendCfg(s, ctx, "execution", execution)
	if e != nil {
		t.Fatal(e)
	}
	c.Supersedes = parent.ID
	a.evidenceDenied.Store(shoal.ID("private-action-evidence"), true)
	before := b.writes.Load()
	if _, e = appendCfg(s, ctx, "incorrect-link", c); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) || b.writes.Load() != before {
		t.Fatal("incompatible parent existence disclosed", e)
	}
}

func alternativePrediction(t *testing.T, p decision.PredictionRecord, differentRequest bool) decision.PredictionRecord {
	t.Helper()
	request := p.Request()
	result := p.Config()
	if differentRequest {
		cfg := request.Config()
		cfg.CorrelationID = "other-call"
		var e error
		request, e = decision.NewDecisionRequest(request.Task(), request.Picture(), request.Predictor(), cfg)
		if e != nil {
			t.Fatal(e)
		}
		result.RequestID = request.ID()
	} else {
		result.Answers[0].Label = "ordinary"
	}
	alternate, e := decision.NewPredictionRecord(request, result)
	if e != nil {
		t.Fatal(e)
	}
	if alternate.ID() == p.ID() {
		t.Fatal("fixture did not change prediction")
	}
	return alternate
}
func TestCrossPredictionPredecessorIndistinguishableFromAbsent(t *testing.T) {
	for _, differentRequest := range []bool{false, true} {
		name := "same-request"
		if differentRequest {
			name = "different-request"
		}
		t.Run(name, func(t *testing.T) {
			s, b, a, ctx, c := storeFixture(t)
			parent, e := appendCfg(s, ctx, "parent", c)
			if e != nil {
				t.Fatal(e)
			}
			a.prediction = alternativePrediction(t, a.prediction, differentRequest)
			a.evidenceDenied.Store(shoal.ID("finding"), true)
			next := c
			next.RequestID = a.prediction.Request().ID()
			next.PredictionID = a.prediction.ID()
			next.EvidenceIDs = []shoal.ID{"current-evidence"}
			next.Supersedes = parent.ID
			before := b.writes.Load()
			_, boundErr := appendCfg(s, ctx, "correction", next)
			next.Supersedes = shoal.ID("outcome-receipt:" + strings.Repeat("0", 64))
			_, missingErr := appendCfg(s, ctx, "correction", next)
			if !shoal.IsErrorCode(boundErr, shoal.ErrorNotFound) || !shoal.IsErrorCode(missingErr, shoal.ErrorNotFound) || boundErr.Error() != missingErr.Error() || b.writes.Load() != before {
				t.Fatalf("cross-prediction existence disclosed: bound=%v missing=%v", boundErr, missingErr)
			}
		})
	}
}
func TestDirectWrongPredictionLookupMasksExistingReceipt(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	if _, e := appendCfg(s, ctx, "key", c); e != nil {
		t.Fatal(e)
	}
	a.prediction = alternativePrediction(t, a.prediction, false)
	a.evidenceDenied.Store(shoal.ID("finding"), true)
	c.PredictionID = a.prediction.ID()
	c.EvidenceIDs = []shoal.ID{"current-evidence"}
	before := b.writes.Load()
	_, readExisting := s.Read(ctx, c.RequestID, c.PredictionID, []byte("key"))
	_, readMissing := s.Read(ctx, c.RequestID, c.PredictionID, []byte("missing"))
	if !shoal.IsErrorCode(readExisting, shoal.ErrorNotFound) || !shoal.IsErrorCode(readMissing, shoal.ErrorNotFound) || readExisting.Error() != readMissing.Error() {
		t.Fatalf("read revealed wrong-prediction receipt: existing=%v missing=%v", readExisting, readMissing)
	}
	if _, e := appendCfg(s, ctx, "key", c); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || b.writes.Load() != before {
		t.Fatal("append revealed wrong-prediction receipt or attempted write", e)
	}
}

func TestIncompatibleCorrectionHidesRevokedGrandparent(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	execution := c
	execution.Kind = decision.OutcomeExecution
	execution.QuestionID = ""
	execution.Label = ""
	execution.ActionID = "action"
	execution.ExecutionStatus = decision.ExecutionSucceeded
	execution.EvidenceIDs = []shoal.ID{"oldest-private-evidence"}
	oldest, e := appendCfg(s, ctx, "oldest", execution)
	if e != nil {
		t.Fatal(e)
	}
	execution.Supersedes = oldest.ID
	execution.EvidenceIDs = []shoal.ID{"parent-visible-evidence"}
	parent, e := appendCfg(s, ctx, "parent", execution)
	if e != nil {
		t.Fatal(e)
	}
	a.evidenceDenied.Store(shoal.ID("oldest-private-evidence"), true)
	_, parentReadErr := s.Read(ctx, c.RequestID, c.PredictionID, []byte("parent"))
	c.Supersedes = parent.ID
	c.EvidenceIDs = []shoal.ID{"current-visible-evidence"}
	before := b.writes.Load()
	_, incompatibleErr := appendCfg(s, ctx, "current", c)
	c.Supersedes = shoal.ID("outcome-receipt:" + strings.Repeat("0", 64))
	_, absentErr := appendCfg(s, ctx, "current", c)
	if !shoal.IsErrorCode(parentReadErr, shoal.ErrorNotFound) || !shoal.IsErrorCode(incompatibleErr, shoal.ErrorNotFound) || !shoal.IsErrorCode(absentErr, shoal.ErrorNotFound) || incompatibleErr.Error() != absentErr.Error() || incompatibleErr.Error() != parentReadErr.Error() {
		t.Fatalf("incompatible link revealed hidden ancestry: read=%v incompatible=%v absent=%v", parentReadErr, incompatibleErr, absentErr)
	}
	if b.writes.Load() != before {
		t.Fatal("incompatible correction attempted write")
	}
}

func TestDepthLimitHidesRevokedOldestAncestor(t *testing.T) {
	s, backend, authority, ctx, config := storeFixture(t)
	config.EvidenceIDs = []shoal.ID{"oldest-private-evidence"}
	last, err := appendCfg(s, ctx, "oldest", config)
	if err != nil {
		t.Fatal(err)
	}
	config.EvidenceIDs = []shoal.ID{"visible-correction-evidence"}
	for i := 0; i < MaxOutcomeAncestors; i++ {
		config.Supersedes = last.ID
		last, err = appendCfg(s, ctx, string(rune(100+i)), config)
		if err != nil {
			t.Fatal(err)
		}
	}
	authority.evidenceDenied.Store(shoal.ID("oldest-private-evidence"), true)
	before := backend.writes.Load()
	config.Supersedes = last.ID
	_, hiddenErr := appendCfg(s, ctx, "beyond-depth-limit", config)
	config.Supersedes = shoal.ID("outcome-receipt:" + strings.Repeat("0", 64))
	_, missingErr := appendCfg(s, ctx, "beyond-depth-limit", config)
	if !shoal.IsErrorCode(hiddenErr, shoal.ErrorNotFound) || !shoal.IsErrorCode(missingErr, shoal.ErrorNotFound) || hiddenErr.Error() != missingErr.Error() {
		t.Fatalf("depth limit disclosed hidden ancestry: hidden=%v missing=%v", hiddenErr, missingErr)
	}
	if backend.writes.Load() != before {
		t.Fatal("over-depth correction attempted a write")
	}
}
