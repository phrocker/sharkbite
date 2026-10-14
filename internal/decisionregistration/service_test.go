// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	registrations "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type focusedResolver struct {
	decision auth.Decision
	err      error
}

func (r *focusedResolver) Resolve(context.Context) (auth.Decision, error) { return r.decision, r.err }

type focusedBuilder struct{}

func (focusedBuilder) Build(context.Context, BuildInput) (BuiltPicture, error) {
	return BuiltPicture{}, errors.New("unexpected builder call")
}

type focusedAuthority struct {
	deny   bool
	mutate func(Profile, []shoal.ID)
	calls  int
}

func (a *focusedAuthority) Authorize(_ context.Context, _ auth.Decision, _ auth.Operation, p Profile, ids []shoal.ID) error {
	a.calls++
	if a.mutate != nil {
		a.mutate(p, ids)
	}
	if a.deny {
		return auth.ObjectNotFound()
	}
	return nil
}
func (a *focusedAuthority) Verify(context.Context, auth.Decision, auth.Operation, Profile, registrations.Registration, decisionartifacts.Record, []collectorregistry.Material) error {
	return errors.New("unexpected verifier call")
}

type focusedBackend struct {
	read   func()
	writes int
}

func (b *focusedBackend) ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error) {
	if b.read != nil {
		b.read()
	}
	return nil, errors.New("storage unavailable")
}
func (b *focusedBackend) CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error) {
	b.writes++
	return allocator.StatusUnknown, errors.New("unexpected write")
}
func focusedProfile(t *testing.T) Profile {
	t.Helper()
	ep, e := decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"source-policy"}})
	if e != nil {
		t.Fatal(e)
	}
	rank, e := decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: "q", Weight: 1, Labels: []decision.LabelPriority{{Label: "yes", Value: 1}, {Label: "no", Value: 0}}}}})
	if e != nil {
		t.Fatal(e)
	}
	task, e := decision.NewTaskSpec(decision.TaskConfig{OwnerID: "operator", Name: "focused task", Version: "1", InputSchemaID: "input", EvidencePolicyID: ep.ID(), LabelPolicyID: "label-policy", EvaluationPolicyID: "eval", PredictionUnit: "source", LabelUnit: "property", ActionUnit: "inspect", AggregationID: rank.ID(), Questions: []decision.Question{{ID: "q", Kind: decision.Choice, RubricID: "rubric", Labels: []string{"yes", "no"}}}})
	if e != nil {
		t.Fatal(e)
	}
	predictor, e := decision.NewPredictorIdentity(decision.PredictorConfig{Provider: "test", RuntimeID: "runtime", WeightsDigest: strings.Repeat("a", 64), TokenizerDigest: strings.Repeat("b", 64), FormattingID: "format", PreprocessingID: "builder", CalibrationID: "uncalibrated", EnvironmentDigest: strings.Repeat("c", 64), Device: "cpu", Precision: "float64", BatchPolicyID: "batch"})
	if e != nil {
		t.Fatal(e)
	}
	return Profile{AllowedModes: []collector.Mode{collector.ServerObserved}, ID: "focused", RevisionID: "v1", BuilderID: "builder", MaterialPurposeID: "purpose", ReleaseID: "release", Query: "Inspect source", TokenizerID: "numeric", OntologyProjectionID: "none", SourceAuthorityPolicyID: "source-policy", Task: task, Predictor: predictor, EvidencePolicy: ep, RankingPlan: rank, TaskResource: auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}, Builder: focusedBuilder{}, MaxDuration: time.Minute}
}
func focusedDecision(t *testing.T, now time.Time, generation int64) auth.Decision {
	t.Helper()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationIngest, auth.OperationRead, auth.OperationInvoke}, PolicyGeneration: generation, AuthenticationExpires: now.Add(time.Hour), RequestID: "auth-request"})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestServiceRechecksAuthorizationAfterInitialStorageFailure(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	p := focusedProfile(t)
	gate := &focusedAuthority{}
	resolver := &focusedResolver{decision: focusedDecision(t, now, 1)}
	backend := &focusedBackend{}
	store, e := registrations.New(registrations.Config{Backend: backend, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(Config{Resolver: resolver, Registrations: store, ArtifactBackend: backend, Materials: &collectorregistry.MaterialResolver{}, Profiles: []Profile{p}, Authority: gate, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	selection := Selection{ProfileID: p.ID, ProfileRevisionID: p.RevisionID, Sources: []SourceInput{{ObservationID: shoal.ID("observation:" + strings.Repeat("a", 64)), Bytes: []byte("source")}}}
	backend.read = func() { gate.deny = true }
	if _, e = s.Register(context.Background(), []byte("key"), selection); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || errors.Is(e, ErrIndeterminate) || backend.writes != 0 {
		t.Fatalf("postread revocation: %v", e)
	}
	gate.deny = false
	backend.read = func() { resolver.decision = focusedDecision(t, now, 2) }
	if _, e = s.Register(context.Background(), []byte("key"), selection); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || backend.writes != 0 {
		t.Fatalf("postread fingerprint: %v", e)
	}
}
func TestServiceAuthorityGetsDetachedImmutableClaims(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	p := focusedProfile(t)
	d := focusedDecision(t, now, 1)
	resolver := &focusedResolver{decision: d}
	gate := &focusedAuthority{}
	s := &Service{config: Config{Resolver: resolver, Authority: gate, Clock: func() time.Time { return now }}}
	originalDomain := string(p.TaskResource.AuthorizationDomain)
	ids := []shoal.ID{"original"}
	gate.mutate = func(p Profile, ids []shoal.ID) { p.TaskResource.AuthorizationDomain[0] = '!'; ids[0] = "replacement" }
	if e := s.authorize(context.Background(), d, auth.OperationIngest, p, ids); e == nil {
		t.Fatal("accepted changed authority claims")
	}
	if string(p.TaskResource.AuthorizationDomain) != originalDomain || ids[0] != "original" {
		t.Fatal("poisoned original claims")
	}
	gate.mutate = nil
	if e := s.authorize(context.Background(), d, auth.OperationIngest, p, ids); e != nil {
		t.Fatal(e)
	}
}
func TestServiceCallerExpiryAndBoundedKey(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := focusedDecision(t, now, 1)
	r := &focusedResolver{decision: d}
	s := &Service{config: Config{Resolver: r, Clock: func() time.Time { return now }}}
	if _, e := s.caller(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Hour)
	if _, e := s.caller(context.Background(), nil); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("expiry", e)
	}
	if _, e := s.Register(context.Background(), make([]byte, 1025), Selection{}); !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) {
		t.Fatal("key", e)
	}
	var missing *focusedResolver
	if _, e := New(Config{Resolver: missing}); e == nil {
		t.Fatal("typed nil resolver")
	}
}

func TestServiceRejectsDelegatedCallerBeforeRegistrationIO(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	delegated, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", OnBehalfOf: []shoal.ID{"delegator"}, AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationIngest, auth.OperationRead, auth.OperationInvoke}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "auth-request"})
	if e != nil {
		t.Fatal(e)
	}
	p := focusedProfile(t)
	gate := &focusedAuthority{}
	resolver := &focusedResolver{decision: delegated}
	reads := 0
	backend := &focusedBackend{read: func() { reads++ }}
	store, e := registrations.New(registrations.Config{Backend: backend, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(Config{Resolver: resolver, Registrations: store, ArtifactBackend: backend, Materials: &collectorregistry.MaterialResolver{}, Profiles: []Profile{p}, Authority: gate, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	selection := Selection{ProfileID: p.ID, ProfileRevisionID: p.RevisionID, Sources: []SourceInput{{ObservationID: shoal.ID("observation:" + strings.Repeat("a", 64)), Bytes: []byte("source")}}}
	if _, e = s.Register(context.Background(), []byte("key"), selection); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || errors.Is(e, ErrIndeterminate) {
		t.Fatalf("delegated registration: %v", e)
	}
	if _, e = s.Read(context.Background(), "request"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("delegated read: %v", e)
	}
	if _, e = s.Artifacts().LoadAuthorized(context.Background(), delegated, "request"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("delegated execution: %v", e)
	}
	if reads != 0 || backend.writes != 0 || gate.calls != 0 {
		t.Fatalf("delegation reached downstream IO or authority: reads%d writes%d grants%d", reads, backend.writes, gate.calls)
	}
}
