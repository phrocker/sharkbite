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

package decisionartifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func fixture(t *testing.T) (Record, auth.Decision, context.Context, auth.Resolver, func() time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var d auth.Decision
	var authority *auth.Authority
	var ctx context.Context
	cfg := auth.DecisionConfig{Subject: "principal", Actor: "actor", ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "http-request", CorrelationID: "correlation", AuditPurpose: "decision-test"}
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
	r := Record{Bundle: decisionservice.Bundle{Request: request, EvidencePolicy: ep, RankingPlan: plan, TaskResource: auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}, Input: input}, Sources: []SourceBytes{{ID: "s", Bytes: []byte("code")}}}
	return r, d, ctx, authority.Resolver(), clock
}

// Test-only authoritative registry: matches independently registered bytes and
// claims, then checks current access. Production must implement its own source,
// builder, snapshot, attestation and joint-disclosure verification.
type testAuthority struct {
	expected   []byte
	id         shoal.ID
	deny       bool
	fail       bool
	verified   int
	preflights int
	hook       func(Record)
}

func (a *testAuthority) AuthorizeRequest(_ context.Context, _ auth.Decision, id shoal.ID) error {
	a.preflights++
	if a.fail {
		return errors.New("private authority diagnostic")
	}
	if a.deny || id != a.id {
		return auth.ObjectNotFound()
	}
	return nil
}
func (a *testAuthority) Verify(_ context.Context, d auth.Decision, r Record) error {
	a.verified++
	if a.deny {
		return auth.ObjectNotFound()
	}
	b, err := encode(r)
	if err != nil || !bytes.Equal(b, a.expected) {
		return auth.ObjectNotFound()
	}
	if err := d.AuthorizeObject(auth.OperationRetrieve, auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("evidence"), PolicyID: []byte("evidence-policy"), ObjectID: "source"}, r.Bundle.Request.Config().RequestedAt); err != nil {
		return err
	}
	if a.hook != nil {
		a.hook(r)
	}
	return nil
}
func newCatalog(t *testing.T, r Record, resolver auth.Resolver, clock func() time.Time) (*Catalog, *testAuthority, *engine.Engine) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if err := eng.CreateTable(Table, engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	backend, err := explorercoord.NewEngineStore(eng, Table)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encode(r)
	if err != nil {
		t.Fatal(err)
	}
	authority := &testAuthority{expected: b, id: r.Bundle.Request.ID()}
	c, err := New(Config{Backend: backend, Resolver: resolver, Authority: authority, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return c, authority, eng
}
func TestRetainLoadAndMutationIsolation(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal("exact replay", err)
	}
	r.Bundle.Input[0] = 'X'
	r.Sources[0].Bytes[0] = 'X'
	a.hook = func(r Record) { r.Bundle.Input[0] = 'Y'; r.Sources[0].Bytes[0] = 'Y' }
	for i := 0; i < 2; i++ {
		b, err := c.LoadAuthorized(ctx, d, a.id)
		if err != nil {
			t.Fatal(err)
		}
		if string(b.Input) != "code" {
			t.Fatal("mutable input aliased persisted record")
		}
		b.Input[0] = 'Z'
	}
}
func TestCurrentAuthorityAndNonDisclosure(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	a.deny = true
	for _, id := range []shoal.ID{a.id, "absent"} {
		b, err := c.LoadAuthorized(ctx, d, id)
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || b.Request.ID() != "" {
			t.Fatal("denied row disclosed", err)
		}
	}
	a.deny = false
	a.fail = true
	if _, err := c.LoadAuthorized(ctx, d, a.id); !errors.Is(err, ErrUnavailable) {
		t.Fatal("outage disguised as missing", err)
	}
	a.fail = false
	if _, err := c.LoadAuthorized(context.Background(), d, a.id); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatal("unbound caller accepted", err)
	}
}
func TestIncompleteOrSubstitutedRetentionRejected(t *testing.T) {
	for _, mode := range []string{"missing source", "wrong source", "wrong input", "oversize source", "false authority", "quote mismatch"} {
		t.Run(mode, func(t *testing.T) {
			r, d, ctx, resolver, clock := fixture(t)
			c, a, _ := newCatalog(t, r, resolver, clock)
			switch mode {
			case "missing source":
				r.Sources = nil
			case "wrong source":
				r.Sources[0].Bytes = []byte("evil")
			case "wrong input":
				r.Bundle.Input = []byte("evil")
			case "oversize source":
				r.Sources[0].Bytes = make([]byte, MaxSourceBytes+1)
			case "false authority", "quote mismatch":
				pc := r.Bundle.Request.Picture().Config()
				if mode == "false authority" {
					pc.Sources[0].Control = decision.RegistryControlled
				} else {
					r.Sources[0].Bytes = []byte("evil")
					pc.Sources[0].Digest = digest(r.Sources[0].Bytes)
				}
				picture, err := decision.NewPictureManifest(r.Bundle.Request.Picture().ContextPack(), pc)
				if err != nil {
					t.Fatal(err)
				}
				request, err := decision.NewDecisionRequest(r.Bundle.Request.Task(), picture, r.Bundle.Request.Predictor(), r.Bundle.Request.Config())
				if err != nil {
					t.Fatal(err)
				}
				r.Bundle.Request = request
				a.id = request.ID()
			}
			if err := c.Retain(ctx, r); err == nil {
				t.Fatal("substitution retained")
			}
			cells, err := c.config.Backend.ReadExact(ctx, []allocator.Coordinate{c.coordinate(d, r.Bundle.Request.ID())})
			if err != nil || len(cells) != 0 {
				t.Fatal("invalid record reached storage", err)
			}
		})
	}
}
func TestFlushFormatsAndRehydrate(t *testing.T) {
	for _, format := range []engine.StorageFormat{engine.StorageFormatRFile, engine.StorageFormatParquet} {
		t.Run(string(format), func(t *testing.T) {
			r, d, ctx, resolver, clock := fixture(t)
			dir := t.TempDir()
			eng, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { eng.Close() }()
			if err := eng.CreateTable(Table, engine.TableOptions{FileFormat: format}); err != nil {
				t.Fatal(err)
			}
			raw, err := encode(r)
			if err != nil {
				t.Fatal(err)
			}
			a := &testAuthority{id: r.Bundle.Request.ID(), expected: raw}
			bind := func() *Catalog {
				backend, err := explorercoord.NewEngineStore(eng, Table)
				if err != nil {
					t.Fatal(err)
				}
				c, err := New(Config{Backend: backend, Resolver: resolver, Authority: a, Clock: clock})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			c := bind()
			if err := c.Retain(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err := eng.Flush(Table); err != nil {
				t.Fatal(err)
			}
			ext := ".rf"
			if format == engine.StorageFormatParquet {
				ext = ".parquet"
			}
			files := 0
			if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && filepath.Ext(path) == ext {
					files++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if files == 0 {
				t.Fatal("no format files")
			}
			if err := eng.Close(); err != nil {
				t.Fatal(err)
			}
			eng, err = engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			c = bind()
			b, err := c.LoadAuthorized(ctx, d, r.Bundle.Request.ID())
			if err != nil {
				t.Fatal(err)
			}
			if b.Request.ID() != r.Bundle.Request.ID() || b.Request.Picture().ContextPackID() != r.Bundle.Request.Picture().ContextPackID() || !bytes.Equal(b.Input, r.Bundle.Input) {
				t.Fatal("rehydration changed identities/input")
			}
			if err := c.Retain(ctx, r); err != nil {
				t.Fatal("reopened exact replay", err)
			}
		})
	}
}

type faultyCAS struct {
	decisionstore.CAS
	loseAck, failRead, corrupt bool
	afterWrite                 func()
	reads                      int
}

func (f *faultyCAS) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	status, err := f.CAS.CompareAndMutate(ctx, m)
	if f.afterWrite != nil {
		f.afterWrite()
	}
	if f.loseAck {
		return allocator.StatusUnknown, errors.New("lost acknowledgment")
	}
	return status, err
}
func (f *faultyCAS) ReadExact(ctx context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	f.reads++
	if f.failRead {
		return nil, errors.New("unreachable")
	}
	cells, err := f.CAS.ReadExact(ctx, coords)
	if f.corrupt && len(cells) > 0 {
		cells[0].Value = []byte("corrupt")
	}
	return cells, err
}
func TestLostAcknowledgmentAndRevocationDuringWrite(t *testing.T) {
	for _, mode := range []string{"reconcile", "indeterminate", "revocation"} {
		t.Run(mode, func(t *testing.T) {
			r, d, ctx, resolver, clock := fixture(t)
			c, a, _ := newCatalog(t, r, resolver, clock)
			f := &faultyCAS{CAS: c.config.Backend, loseAck: true}
			c.config.Backend = f
			if mode == "indeterminate" {
				f.failRead = true
			}
			if mode == "revocation" {
				f.loseAck = false
				f.afterWrite = func() { a.deny = true }
			}
			err := c.Retain(ctx, r)
			switch mode {
			case "reconcile":
				if err != nil {
					t.Fatal(err)
				}
			case "indeterminate":
				if !errors.Is(err, ErrIndeterminate) {
					t.Fatal(err)
				}
			case "revocation":
				if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
					t.Fatal(err)
				}
			}
			a.deny = false
			f.failRead = false
			f.afterWrite = nil
			if _, err := c.LoadAuthorized(ctx, d, a.id); err != nil {
				t.Fatal("write was not actually durable", err)
			}
		})
	}
}
func TestCorruptionAndDenialBeforeRead(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	f := &faultyCAS{CAS: c.config.Backend, corrupt: true}
	c.config.Backend = f
	if _, err := c.LoadAuthorized(ctx, d, a.id); !errors.Is(err, ErrUnavailable) {
		t.Fatal("corruption accepted", err)
	}
	a.deny = true
	f.reads = 0
	if _, err := c.LoadAuthorized(ctx, d, a.id); !shoal.IsErrorCode(err, shoal.ErrorNotFound) || f.reads != 0 {
		t.Fatal("denied request inspected storage", err)
	}
}

// Every caller uses its own test registry; the real CAS backend is shared.
func TestConcurrentRetention(t *testing.T) {
	r, _, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := c.config
			cfg.Authority = &testAuthority{id: a.id, expected: a.expected}
			worker, err := New(cfg)
			if err == nil {
				err = worker.Retain(ctx, r)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

type fixedProvider struct {
	identity decision.PredictorIdentity
	calls    int
}

func (p *fixedProvider) Identity() decision.PredictorIdentity { return p.identity }
func (p *fixedProvider) Predict(_ context.Context, r decision.DecisionRequest, input []byte) (decision.ResultConfig, error) {
	p.calls++
	if string(input) != "code" {
		return decision.ResultConfig{}, errors.New("substituted model input")
	}
	return decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu", Status: decision.Completed, Answers: []decision.Answer{{SubjectID: "subject", QuestionID: "review", Status: decision.Answered, Label: "inspect"}}}, nil
}
func (p *fixedProvider) Resolve(_ context.Context, release, predictor shoal.ID) (decisionservice.Predictor, error) {
	if release != "release:1" || predictor != p.identity.ID() {
		return nil, errors.New("unexpected provider")
	}
	return p, nil
}
func TestServiceReplayAfterEngineRestart(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { eng.Close() }()
	for _, table := range []string{Table, decisionstore.Table} {
		if err := eng.CreateTable(table, engine.TableOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := encode(r)
	if err != nil {
		t.Fatal(err)
	}
	a := &testAuthority{id: r.Bundle.Request.ID(), expected: raw}
	provider := &fixedProvider{identity: r.Bundle.Request.Predictor()}
	bind := func() (*Catalog, *decisionservice.Service) {
		ab, err := explorercoord.NewEngineStore(eng, Table)
		if err != nil {
			t.Fatal(err)
		}
		c, err := New(Config{Backend: ab, Resolver: resolver, Authority: a, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		rb, err := explorercoord.NewEngineStore(eng, decisionstore.Table)
		if err != nil {
			t.Fatal(err)
		}
		receipts, err := decisionstore.New(rb, nil, clock)
		if err != nil {
			t.Fatal(err)
		}
		service, err := decisionservice.New(decisionservice.Config{Resolver: resolver, Artifacts: c, Providers: provider, Receipts: receipts, Clock: clock, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return c, service
	}
	c, service := bind()
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	first, err := service.Evaluate(ctx, r.Bundle.Request.ID(), []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Ranking == nil || first.Receipt.Result.Status != decision.Completed {
		t.Fatal("no completed decision")
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, service = bind()
	replay, err := service.Evaluate(ctx, r.Bundle.Request.ID(), []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Receipt.PredictionID != first.Receipt.PredictionID || provider.calls != 1 {
		t.Fatal("restart changed prediction or reinvoked model")
	}
	a.deny = true
	if _, err := service.Read(ctx, r.Bundle.Request.ID(), []byte("key")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatal("retained evidence bypassed current authority", err)
	}
	if _, err := c.LoadAuthorized(ctx, d, r.Bundle.Request.ID()); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatal(err)
	}
}
func TestConflictingPolicyMappingCannotOverwrite(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	// A second registered policy mapping under the same request must not replace
	// the original. Both source and policy grants still authorize these values.
	r.Bundle.TaskResource.SourceID = []byte("evidence")
	a.expected, _ = encode(r)
	if err := c.Retain(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatal("mapping overwrote original", err)
	}
	r.Bundle.TaskResource.SourceID = []byte("tasks")
	a.expected, _ = encode(r)
	b, err := c.LoadAuthorized(ctx, d, a.id)
	if err != nil || string(b.TaskResource.SourceID) != "tasks" {
		t.Fatal("original changed", err)
	}
}
func TestVerificationRevocationAndInvalidClock(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	a.hook = func(Record) { a.deny = true }
	if b, err := c.LoadAuthorized(ctx, d, a.id); !shoal.IsErrorCode(err, shoal.ErrorNotFound) || b.Request.ID() != "" {
		t.Fatal("revocation during verification disclosed payload", err)
	}
	a.deny = false
	a.hook = nil
	c.config.Clock = func() time.Time { return time.Time{} }
	if _, err := c.LoadAuthorized(ctx, d, a.id); !errors.Is(err, ErrUnavailable) {
		t.Fatal("invalid clock accepted", err)
	}
}

func TestRequiredAuthorityAndScopeIsolation(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	cfg := c.config
	var absent *testAuthority
	cfg.Authority = absent
	if _, err := New(cfg); err == nil {
		t.Fatal("typed nil authority accepted")
	}
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"domain", "principal", "actor", "grants"} {
		t.Run(mode, func(t *testing.T) {
			cfg := auth.DecisionConfig{Subject: d.Subject(), Actor: d.Actor(), ClientID: d.ClientID(), AuthorizationDomain: d.AuthorizationDomain(), AllowedOperations: d.AllowedOperations(), PermittedSourceIDs: d.PermittedSourceIDs(), PermittedPolicyIDs: d.PermittedPolicyIDs(), PolicyGeneration: d.PolicyGeneration(), AuthenticationExpires: d.AuthenticationExpires(), RequestID: d.RequestID()}
			switch mode {
			case "domain":
				cfg.AuthorizationDomain = []byte("other")
			case "principal":
				cfg.Subject = "other"
			case "actor":
				cfg.Actor = "other"
			case "grants":
				cfg.PermittedSourceIDs = [][]byte{[]byte("tasks")}
			}
			changed, err := auth.NewDecision(cfg)
			if err != nil {
				t.Fatal(err)
			}
			// A substituted Decision cannot replace the resolver's authenticated identity.
			if _, err := c.LoadAuthorized(ctx, changed, a.id); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatal("substituted authority accepted", err)
			}
			if mode == "domain" || mode == "principal" {
				if bytes.Equal(c.coordinate(d, a.id).Row, c.coordinate(changed, a.id).Row) {
					t.Fatal("scope collision")
				}
			}
		})
	}
}

func TestArtifactWALSurvivesAbruptExit(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	if dir := os.Getenv("SHOAL_ARTIFACT_CRASH_DIR"); dir != "" {
		eng, err := engine.Open(dir, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.CreateTable(Table, engine.TableOptions{}); err != nil {
			t.Fatal(err)
		}
		backend, err := explorercoord.NewEngineStore(eng, Table)
		if err != nil {
			t.Fatal(err)
		}
		b, err := encode(r)
		if err != nil {
			t.Fatal(err)
		}
		c, err := New(Config{Backend: backend, Resolver: resolver, Authority: &testAuthority{id: r.Bundle.Request.ID(), expected: b}, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Retain(ctx, r); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Intentionally omit Engine.Close and table flush.
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactWALSurvivesAbruptExit$")
	cmd.Env = append(os.Environ(), "SHOAL_ARTIFACT_CRASH_DIR="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v %s", err, output)
	}
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	backend, err := explorercoord.NewEngineStore(eng, Table)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encode(r)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Backend: backend, Resolver: resolver, Authority: &testAuthority{id: r.Bundle.Request.ID(), expected: raw}, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.LoadAuthorized(ctx, d, r.Bundle.Request.ID())
	if err != nil || !bytes.Equal(b.Input, r.Bundle.Input) {
		t.Fatal("WAL lost acknowledged artifacts", err)
	}
}
func TestExpiryDuringVerificationWithholdsBytes(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	a.hook = func(Record) { c.config.Clock = func() time.Time { return d.AuthenticationExpires() } }
	if b, err := c.LoadAuthorized(ctx, d, a.id); !shoal.IsErrorCode(err, shoal.ErrorNotFound) || b.Request.ID() != "" {
		t.Fatal("expired authentication returned source bytes", err)
	}
}

func TestHistoricalLoadRequiresCurrentEvidenceGrants(t *testing.T) {
	r, d, ctx, resolver, clock := fixture(t)
	c, a, _ := newCatalog(t, r, resolver, clock)
	if err := c.Retain(ctx, r); err != nil {
		t.Fatal(err)
	}
	cfg := auth.DecisionConfig{Subject: d.Subject(), Actor: d.Actor(), ClientID: d.ClientID(), AuthorizationDomain: d.AuthorizationDomain(), AllowedOperations: d.AllowedOperations(), PermittedSourceIDs: d.PermittedSourceIDs(), PermittedPolicyIDs: d.PermittedPolicyIDs(), PolicyGeneration: d.PolicyGeneration() + 1, AuthenticationExpires: d.AuthenticationExpires(), RequestID: d.RequestID()}
	authority, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	c.config.Resolver = authority.Resolver()
	for _, revoked := range []bool{false, true} {
		if revoked {
			cfg.PermittedSourceIDs = [][]byte{[]byte("tasks")}
		}
		current, err := auth.NewDecision(cfg)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := authority.Binder().Bind(context.Background(), current)
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.LoadAuthorized(bound, current, a.id)
		if revoked {
			if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || b.Request.ID() != "" {
				t.Fatal("current evidence denial bypassed", err)
			}
		} else {
			if err != nil || b.Request.ID() != r.Bundle.Request.ID() {
				t.Fatal("authorized historical read failed", err)
			}
			if err := c.Retain(bound, r); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatal("changed projection admitted old request", err)
			}
		}
	}
}

type revokingReadCAS struct {
	decisionstore.CAS
	afterRead func()
	corrupt   bool
}

func (b *revokingReadCAS) ReadExact(ctx context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	cells, err := b.CAS.ReadExact(ctx, coords)
	if b.corrupt && len(cells) > 0 {
		cells[0].Value = []byte("corrupt")
	}
	b.afterRead()
	return cells, err
}
func TestRevocationDuringStorageDoesNotDiscloseErrorState(t *testing.T) {
	for _, operation := range []string{"load", "retain conflict"} {
		for _, exists := range []bool{false, true} {
			name := operation + " absent"
			if exists {
				name = operation + " existing"
			}
			t.Run(name, func(t *testing.T) {
				r, d, ctx, resolver, clock := fixture(t)
				c, a, _ := newCatalog(t, r, resolver, clock)
				if exists {
					if err := c.Retain(ctx, r); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "load" {
					// Corruption and absence must be indistinguishable after revocation.
					c.config.Backend = &revokingReadCAS{CAS: c.config.Backend, corrupt: true, afterRead: func() { a.deny = true }}
					b, err := c.LoadAuthorized(ctx, d, a.id)
					if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || b.Request.ID() != "" {
						t.Fatal("read exposed storage state after revocation", err)
					}
				} else {
					r.Bundle.TaskResource.SourceID = []byte("evidence")
					var err error
					a.expected, err = encode(r)
					if err != nil {
						t.Fatal(err)
					}
					c.config.Backend = &faultyCAS{CAS: c.config.Backend, afterWrite: func() { a.deny = true }}
					if err := c.Retain(ctx, r); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
						t.Fatal("write exposed conflict after revocation", err)
					}
				}
			})
		}
	}
}

// Force a deadline error at an exact I/O boundary without a timing-dependent test.
type deadlineErrorContext struct{ context.Context }

func (c deadlineErrorContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}
func TestUncertainWritePreservesAmbiguityAcrossReauthorizationFailure(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "authority outage", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			r, d, ctx, resolver, clock := fixture(t)
			c, a, _ := newCatalog(t, r, resolver, clock)
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var requestCtx context.Context = callCtx
			if mode == "deadline" {
				requestCtx = deadlineErrorContext{callCtx}
			}
			f := &faultyCAS{CAS: c.config.Backend, loseAck: true, failRead: true, afterWrite: func() {
				switch mode {
				case "cancel", "deadline":
					cancel()
				case "authority outage":
					a.fail = true
				case "revoked":
					a.deny = true
				}
			}}
			c.config.Backend = f
			err := c.Retain(requestCtx, r)
			if mode == "revoked" {
				if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || errors.Is(err, ErrIndeterminate) {
					t.Fatal("denial disclosed ambiguous storage state", err)
				}
			} else {
				if !errors.Is(err, ErrIndeterminate) {
					t.Fatal("uncertain retention lost ambiguity", err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("lost cancellation cause", err)
				}
				if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("lost deadline cause", err)
				}
			}
			a.fail = false
			a.deny = false
			f.failRead = false
			if _, err := c.LoadAuthorized(ctx, d, a.id); err != nil {
				t.Fatal("uncertain write did not persist as expected", err)
			}
		})
	}
}
