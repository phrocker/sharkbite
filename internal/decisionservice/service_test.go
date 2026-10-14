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

package decisionservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type catalog struct {
	bundle  Bundle
	revoked atomic.Bool
	loads   atomic.Int32
	onLoad  func()
}

func (c *catalog) LoadAuthorized(ctx context.Context, d auth.Decision, id shoal.ID) (Bundle, error) {
	c.loads.Add(1)
	if id != c.bundle.Request.ID() || c.revoked.Load() {
		return Bundle{}, auth.ObjectNotFound()
	}
	// Test catalog's trusted evidence mapping. Production must verify retained
	// artifacts/provenance and every source-derived registration, not just grants.
	if err := d.AuthorizeObject(auth.OperationRetrieve, auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("evidence"), PolicyID: []byte("evidence-policy"), ObjectID: "source"}, c.bundle.Request.Config().RequestedAt); err != nil {
		return Bundle{}, err
	}
	if c.onLoad != nil {
		c.onLoad()
	}
	return c.bundle, nil
}

type provider struct {
	identity decision.PredictorIdentity
	calls    atomic.Int32
	run      func(context.Context, decision.DecisionRequest, []byte) (decision.ResultConfig, error)
}

func (p *provider) Identity() decision.PredictorIdentity { return p.identity }
func (p *provider) Predict(ctx context.Context, r decision.DecisionRequest, input []byte) (decision.ResultConfig, error) {
	p.calls.Add(1)
	return p.run(ctx, r, input)
}

type registry struct {
	p         *provider
	calls     atomic.Int32
	release   shoal.ID
	fail      bool
	onResolve func()
}

func (r *registry) Resolve(_ context.Context, release, identity shoal.ID) (Predictor, error) {
	r.calls.Add(1)
	if r.onResolve != nil {
		r.onResolve()
	}
	if r.fail || release != r.release {
		return nil, errors.New("private registry diagnostic")
	}
	return r.p, nil
}

type harness struct {
	s          *Service
	store      *decisionstore.Store
	catalog    *catalog
	registry   *registry
	clock      atomic.Int64
	authority  *auth.Authority
	d          auth.Decision
	authConfig auth.DecisionConfig
	ctx        context.Context
}

func (h *harness) now() time.Time { return time.Unix(0, h.clock.Load()).UTC() }
func newHarness(t *testing.T, stale bool) *harness {
	t.Helper()
	h := &harness{}
	h.clock.Store(time.Now().UTC().UnixNano())
	now := h.now()
	cfg := auth.DecisionConfig{Subject: "principal", Actor: "actor", ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "http-request", CorrelationID: "correlation", AuditPurpose: "decision-test"}
	h.authConfig = cfg
	var err error
	h.d, err = auth.NewDecision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.authority, err = auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.ctx, err = h.authority.Binder().Bind(context.Background(), h.d)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(h.d)
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
	if stale {
		observed = now.Add(-2 * time.Hour)
	}
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
	h.catalog = &catalog{bundle: Bundle{Request: request, EvidencePolicy: ep, RankingPlan: plan, TaskResource: auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}, Input: input}}
	h.registry = &registry{release: "release:1", p: &provider{identity: model}}
	h.registry.p.run = func(_ context.Context, r decision.DecisionRequest, _ []byte) (decision.ResultConfig, error) {
		h.clock.Add(int64(time.Second))
		return validOutput(r), nil
	}
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if err := eng.CreateTable(decisionstore.Table, engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	backend, err := explorercoord.NewEngineStore(eng, decisionstore.Table)
	if err != nil {
		t.Fatal(err)
	}
	h.store, err = decisionstore.New(backend, nil, h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.s, err = New(Config{Resolver: h.authority.Resolver(), Artifacts: h.catalog, Providers: h.registry, Receipts: h.store, Clock: h.now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func validOutput(r decision.DecisionRequest) decision.ResultConfig {
	return decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu", Status: decision.Completed, Answers: []decision.Answer{{SubjectID: "subject", QuestionID: "review", Status: decision.Answered, Label: "inspect"}}}
}
func TestEvaluateReadAndReplayUseOneProviderCall(t *testing.T) {
	h := newHarness(t, false)
	id := h.catalog.bundle.Request.ID()
	key := []byte("key")
	first, err := h.s.Evaluate(h.ctx, id, key)
	if err != nil {
		t.Fatal(err)
	}
	if first.Receipt.State != decisionstore.Committed || first.Ranking == nil || *first.Ranking.Entries()[0].Score != 1 {
		t.Fatal("missing ranked receipt")
	}
	replay, err := h.s.Evaluate(h.ctx, id, key)
	if err != nil {
		t.Fatal(err)
	}
	read, err := h.s.Read(h.ctx, id, key)
	if err != nil {
		t.Fatal(err)
	}
	if first.Receipt.PredictionID != replay.Receipt.PredictionID || read.Receipt.PredictionID != first.Receipt.PredictionID || h.registry.p.calls.Load() != 1 || h.registry.calls.Load() != 1 {
		t.Fatal("retry recomputed or changed result")
	}
	// A new service instance can return the same durable receipt without invoking.
	restarted, err := New(h.s.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Evaluate(h.ctx, id, key); err != nil || h.registry.p.calls.Load() != 1 {
		t.Fatal("service restart reinvoked:", err)
	}
}
func TestUntrustedContextAndOperationDenialPrecedeArtifactLookup(t *testing.T) {
	h := newHarness(t, false)
	id := h.catalog.bundle.Request.ID()
	if _, err := h.s.Evaluate(context.Background(), id, []byte("key")); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatal("forged/unbound context accepted:", err)
	}
	cfg := h.authConfig
	cfg.AllowedOperations = []auth.Operation{auth.OperationRead, auth.OperationRetrieve}
	d, err := auth.NewDecision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := h.authority.Binder().Bind(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Evaluate(ctx, id, []byte("key")); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatal("missing invoke allowed:", err)
	}
	if h.catalog.loads.Load() != 0 || h.registry.calls.Load() != 0 {
		t.Fatal("unauthorized request reached artifacts/provider")
	}
}
func TestRevocationMasksExistingAndAbsentRequests(t *testing.T) {
	h := newHarness(t, false)
	id := h.catalog.bundle.Request.ID()
	key := []byte("key")
	if _, err := h.s.Evaluate(h.ctx, id, key); err != nil {
		t.Fatal(err)
	}
	h.catalog.revoked.Store(true)
	for _, requestID := range []shoal.ID{id, "absent"} {
		if _, err := h.s.Read(h.ctx, requestID, key); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatal("revocation disclosed receipt:", err)
		}
	}
	if _, err := h.s.Evaluate(h.ctx, id, key); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatal("revoked retry allowed:", err)
	}
	if h.registry.p.calls.Load() != 1 {
		t.Fatal("revoked evidence reinvoked")
	}
}
func TestRevocationDuringProviderDoesNotExposeResult(t *testing.T) {
	h := newHarness(t, false)
	h.registry.p.run = func(_ context.Context, r decision.DecisionRequest, _ []byte) (decision.ResultConfig, error) {
		h.catalog.revoked.Store(true)
		return validOutput(r), nil
	}
	if response, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) || response.Ranking != nil || response.Receipt.ID != "" {
		t.Fatal("revoked output exposed:", err)
	}
	scope, err := scopeFor(h.d)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := h.store.Get(h.ctx, scope, []byte("key"), h.catalog.bundle.Request)
	if err != nil || pending.State != decisionstore.Pending {
		t.Fatal("revocation fabricated terminal result")
	}
}
func TestIneligibleEvidenceAbstainsWithoutResolvingProvider(t *testing.T) {
	h := newHarness(t, true)
	out, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Receipt.Result.Status != decision.Abstained || out.Ranking.Entries()[0].Score != nil || h.registry.calls.Load() != 0 {
		t.Fatal("ineligible evidence reached predictor or became a score")
	}
}
func TestProviderFailuresBecomeBoundedFailedReceipts(t *testing.T) {
	for _, mode := range []string{"error", "wrong identity", "malformed", "late", "unavailable", "pre-invocation deadline"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, false)
			switch mode {
			case "error":
				h.registry.p.run = func(context.Context, decision.DecisionRequest, []byte) (decision.ResultConfig, error) {
					return decision.ResultConfig{}, errors.New("secret source text")
				}
			case "wrong identity":
				cfg := h.registry.p.identity.Config()
				cfg.PreprocessingID = "other"
				var err error
				h.registry.p.identity, err = decision.NewPredictorIdentity(cfg)
				if err != nil {
					t.Fatal(err)
				}
			case "malformed":
				h.registry.p.run = func(_ context.Context, r decision.DecisionRequest, _ []byte) (decision.ResultConfig, error) {
					v := validOutput(r)
					v.PredictorID = "substituted"
					return v, nil
				}
			case "late":
				h.registry.p.run = func(_ context.Context, r decision.DecisionRequest, _ []byte) (decision.ResultConfig, error) {
					h.clock.Add(int64(61 * time.Second))
					return validOutput(r), nil
				}
			case "pre-invocation deadline":
				h.registry.onResolve = func() { h.clock.Add(int64(61 * time.Second)) }
			case "unavailable":
				h.registry.fail = true
			}
			out, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
			if err != nil {
				t.Fatal(err)
			}
			if out.Receipt.Result.Status != decision.Failed || out.Receipt.Result.EffectiveDevice != "" || out.Ranking.Entries()[0].Score != nil || strings.Contains(out.Receipt.Result.Reason, "secret") {
				t.Fatal("provider failure leaked or became a score")
			}
		})
	}
}
func TestInputDigestCheckedBeforeReservationOrProvider(t *testing.T) {
	h := newHarness(t, false)
	h.catalog.bundle.Input = []byte("substituted")
	if _, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key")); !errors.Is(err, ErrUnavailable) {
		t.Fatal("substituted input accepted:", err)
	}
	if h.registry.calls.Load() != 0 {
		t.Fatal("substituted input reached provider")
	}
	scope, err := scopeFor(h.d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Get(h.ctx, scope, []byte("key"), h.catalog.bundle.Request); !errors.Is(err, decisionstore.ErrNotFound) {
		t.Fatal("invalid artifact reserved")
	}
}
func TestConcurrentPendingRetryDoesNotInvokeAgain(t *testing.T) {
	h := newHarness(t, false)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	h.registry.p.run = func(ctx context.Context, r decision.DecisionRequest, _ []byte) (decision.ResultConfig, error) {
		close(entered)
		select {
		case <-release:
			return validOutput(r), nil
		case <-ctx.Done():
			return decision.ResultConfig{}, ctx.Err()
		}
	}
	go func() { _, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key")); done <- err }()
	<-entered
	pending, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
	close(release)
	if err != nil || pending.Receipt.State != decisionstore.Pending || pending.Ranking != nil {
		t.Fatal("pending retry invalid:", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.registry.p.calls.Load() != 1 {
		t.Fatal("pending retry invoked twice")
	}
}

type receiptHook struct {
	Receipts
	reserveErr  error
	afterCommit func()
}

func (r receiptHook) Reserve(ctx context.Context, scope decisionstore.Scope, key []byte, request decision.DecisionRequest, lease time.Duration) (decisionstore.Reservation, error) {
	if r.reserveErr != nil {
		return decisionstore.Reservation{}, r.reserveErr
	}
	return r.Receipts.Reserve(ctx, scope, key, request, lease)
}
func (r receiptHook) Commit(ctx context.Context, scope decisionstore.Scope, key []byte, request decision.DecisionRequest, claim decisionstore.Claim, result decision.ResultConfig) (decisionstore.Receipt, error) {
	v, err := r.Receipts.Commit(ctx, scope, key, request, claim, result)
	if r.afterCommit != nil {
		r.afterCommit()
	}
	return v, err
}
func TestUnknownReservationDoesNotInvoke(t *testing.T) {
	h := newHarness(t, false)
	cfg := h.s.config
	cfg.Receipts = receiptHook{Receipts: h.store, reserveErr: decisionstore.ErrIndeterminate}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key")); !errors.Is(err, ErrIndeterminate) {
		t.Fatal("unknown reservation status lost:", err)
	}
	if h.registry.calls.Load() != 0 {
		t.Fatal("provider resolved without confirmed ownership")
	}
}
func TestRevocationDuringCommitHidesCommittedReceipt(t *testing.T) {
	h := newHarness(t, false)
	cfg := h.s.config
	cfg.Receipts = receiptHook{Receipts: h.store, afterCommit: func() { h.catalog.revoked.Store(true) }}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || out.Receipt.ID != "" || out.Ranking != nil {
		t.Fatal("committed payload escaped revocation:", err)
	}
	scope, err := scopeFor(h.d)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.store.Get(h.ctx, scope, []byte("key"), h.catalog.bundle.Request)
	if err != nil || stored.State != decisionstore.Committed {
		t.Fatal("committed state misrepresented")
	}
}
func TestProviderCannotMutateRetainedInput(t *testing.T) {
	h := newHarness(t, false)
	h.registry.p.run = func(_ context.Context, r decision.DecisionRequest, input []byte) (decision.ResultConfig, error) {
		input[0] = 'X'
		return validOutput(r), nil
	}
	if _, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key")); err != nil {
		t.Fatal(err)
	}
	if string(h.catalog.bundle.Input) != "code" {
		t.Fatal("provider changed retained input")
	}
}
func TestScopeIncludesCallerIdentityButNotMutableGrants(t *testing.T) {
	h := newHarness(t, false)
	original, err := scopeFor(h.d)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.authConfig
	cfg.PolicyGeneration++
	d, err := auth.NewDecision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := scopeFor(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(original.Domain) != string(changed.Domain) {
		t.Fatal("grant change minted new idempotency namespace")
	}
	cfg.Actor = "different-actor"
	d, err = auth.NewDecision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = scopeFor(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(original.Domain) == string(changed.Domain) {
		t.Fatal("actor identity omitted from namespace")
	}
}
func TestChangedAuthorizationProjectionDoesNotReinvoke(t *testing.T) {
	h := newHarness(t, false)
	id := h.catalog.bundle.Request.ID()
	key := []byte("key")
	if _, err := h.s.Evaluate(h.ctx, id, key); err != nil {
		t.Fatal(err)
	}
	cfg := h.authConfig
	cfg.PolicyGeneration++
	d, err := auth.NewDecision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := h.authority.Binder().Bind(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Evaluate(ctx, id, key); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatal("changed projection invoked old request:", err)
	}
	// Reading still uses current authorization against the original retained scope.
	if _, err := h.s.Read(ctx, id, key); err != nil {
		t.Fatal("authorized historical read failed:", err)
	}
	if h.registry.p.calls.Load() != 1 {
		t.Fatal("changed grants caused new inference")
	}
}
func TestProviderReceivesCancellationDeadline(t *testing.T) {
	h := newHarness(t, false)
	cfg := h.s.config
	cfg.MaxCall = 10 * time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.registry.p.run = func(ctx context.Context, _ decision.DecisionRequest, _ []byte) (decision.ResultConfig, error) {
		<-ctx.Done()
		return decision.ResultConfig{}, ctx.Err()
	}
	out, err := s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Receipt.Result.Status != decision.Failed || out.Receipt.Result.Reason != "deadline_exceeded" {
		t.Fatal("deadline not recorded as failed call")
	}
}

func TestTypedNilDependenciesFailClosed(t *testing.T) {
	h := newHarness(t, false)
	cfg := h.s.config
	var absent *catalog
	cfg.Artifacts = absent
	if _, err := New(cfg); err == nil {
		t.Fatal("typed nil catalog accepted")
	}
	h.registry.p = nil
	out, err := h.s.Evaluate(h.ctx, h.catalog.bundle.Request.ID(), []byte("nil-provider"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Receipt.Result.Status != decision.Failed {
		t.Fatal("nil provider became success")
	}
}

func TestMalformedKeysDoNotConsultArtifacts(t *testing.T) {
	h := newHarness(t, false)
	for _, operation := range []struct {
		name string
		call func(context.Context, shoal.ID, []byte) (Response, error)
	}{{"evaluate", h.s.Evaluate}, {"read", h.s.Read}} {
		for _, revoked := range []bool{false, true} {
			h.catalog.revoked.Store(revoked)
			for _, id := range []shoal.ID{h.catalog.bundle.Request.ID(), "absent"} {
				for _, key := range [][]byte{nil, {}, make([]byte, shoal.MaxIDBytes+1)} {
					out, err := operation.call(h.ctx, id, key)
					if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) || out.Receipt.ID != "" || out.Ranking != nil {
						t.Fatalf("%s disclosed state for malformed key: %v", operation.name, err)
					}
				}
			}
		}
	}
	if h.catalog.loads.Load() != 0 || h.registry.calls.Load() != 0 {
		t.Fatal("malformed key reached artifacts or registry")
	}
}

func TestCancellationBeforeProviderInvocation(t *testing.T) {
	for _, stage := range []string{"entry", "registry", "last authorization"} {
		t.Run(stage, func(t *testing.T) {
			h := newHarness(t, false)
			ctx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			if stage == "entry" {
				cancel()
			} else if stage == "registry" {
				h.registry.onResolve = cancel
			} else {
				h.catalog.onLoad = func() {
					if h.catalog.loads.Load() == 3 {
						cancel()
					}
				}
			}
			out, err := h.s.Evaluate(ctx, h.catalog.bundle.Request.ID(), []byte("key"))
			if !errors.Is(err, context.Canceled) || out.Receipt.ID != "" || out.Ranking != nil {
				t.Fatalf("cancellation returned output: %v", err)
			}
			if h.registry.p.calls.Load() != 0 {
				t.Fatal("canceled request invoked provider")
			}
			if stage == "entry" && h.catalog.loads.Load() != 0 {
				t.Fatal("canceled request loaded artifacts")
			}
			if stage != "entry" {
				scope, err := scopeFor(h.d)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := h.store.Get(h.ctx, scope, []byte("key"), h.catalog.bundle.Request)
				if err != nil || receipt.State != decisionstore.Pending {
					t.Fatalf("cancellation fabricated result: %v", err)
				}
			}
		})
	}
}

// TestAPredictorCannotClaimAServiceReason is #509.
//
// The predictor's native Abstained/Failed result passed through with whatever
// reason it gave, and the service also stamps its own terminal reasons through
// terminal(...). The stored result recorded neither which one set it nor any
// constraint on what the predictor could say. So a predictor returning
// "evidence_ineligible" produced a receipt reading "the service found the
// evidence ineligible and ran no predictor" — false, and nothing in the record
// contradicted it.
//
// Reserved rather than attributed. An origin field makes the misattribution
// legible; reserving makes it unrepresentable, needs no new persisted field on
// every stored result, and lets a reader take the reason at face value. A
// predictor with its own view of the same situation says so in its own words,
// which is more honest — its judgement that evidence was unusable is not the
// service's eligibility gate.
func TestAPredictorCannotClaimAServiceReason(t *testing.T) {
	// Every reserved reason, on the status it is reserved for and on the
	// other one. A per-status check would admit a predictor returning
	// "evidence_ineligible" on a Failed result, which is the same false
	// claim.
	for _, status := range []decision.ResultStatus{
		decision.Abstained, decision.Failed,
	} {
		for _, reason := range append(
			append([]string(nil), decision.ServiceReasons()[decision.Abstained]...),
			decision.ServiceReasons()[decision.Failed]...,
		) {
			t.Run(string(status)+"/"+reason, func(t *testing.T) {
				h := newHarness(t, false)
				h.registry.p.run = func(
					_ context.Context, r decision.DecisionRequest, _ []byte,
				) (decision.ResultConfig, error) {
					out := validOutput(r)
					out.Status = status
					out.Reason = reason
					out.Answers = nil
					return out, nil
				}
				out, err := h.s.Evaluate(
					h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
				if err != nil {
					t.Fatal(err)
				}
				if out.Receipt.Result.Reason != decision.ReasonInvalidPredictorResponse {
					t.Fatalf("a predictor returned the service's own reason "+
						"%q and the receipt kept it (reason=%q): the record "+
						"now reads as a finding the service never made",
						reason, out.Receipt.Result.Reason)
				}
				if out.Receipt.Result.Status != decision.Failed {
					t.Fatalf("status = %q, want failed: a response claiming "+
						"an authority it does not have is not a valid "+
						"prediction", out.Receipt.Result.Status)
				}
			})
		}
	}

	// And a predictor's own words still pass through, which is the half that
	// must not break: reserving the service's vocabulary is not the same as
	// refusing the predictor a voice.
	t.Run("the predictor's own reason survives", func(t *testing.T) {
		h := newHarness(t, false)
		const own = "model_declined_low_support"
		if decision.ReservedServiceReason(own) {
			t.Fatal("the fixture's reason is reserved, so this cannot tell " +
				"a passed-through reason from a refused one")
		}
		h.registry.p.run = func(
			_ context.Context, r decision.DecisionRequest, _ []byte,
		) (decision.ResultConfig, error) {
			out := validOutput(r)
			out.Status = decision.Abstained
			out.Reason = own
			out.Answers = nil
			return out, nil
		}
		out, err := h.s.Evaluate(
			h.ctx, h.catalog.bundle.Request.ID(), []byte("key"))
		if err != nil {
			t.Fatal(err)
		}
		if out.Receipt.Result.Reason != own {
			t.Fatalf("the predictor's own reason = %q, want %q",
				out.Receipt.Result.Reason, own)
		}
		if out.Receipt.Result.Status != decision.Abstained {
			t.Fatalf("status = %q, want abstained", out.Receipt.Result.Status)
		}
	})
}
