// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	registrationstore "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func rootHash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }

// This test authority models operator-managed revocation. Collector metadata and
// synthetic classifier output never modify its grant.
type rootAccess struct{ deny bool }

func (a *rootAccess) Authorize(_ context.Context, d auth.Decision, op auth.Operation, p Profile, ids []shoal.ID) error {
	if a.deny {
		return auth.ObjectNotFound()
	}
	if e := d.Authorize(op, p.TaskResource, time.Now()); e != nil {
		return e
	}
	for _, id := range ids {
		if e := d.Authorize(auth.OperationRead, auth.ResourceRequest{AuthorizationDomain: d.AuthorizationDomain(), SourceID: []byte(id), PolicyID: []byte(p.SourceAuthorityPolicyID), ObjectID: id}, time.Now()); e != nil {
			return e
		}
	}
	return nil
}
func (a *rootAccess) Verify(ctx context.Context, d auth.Decision, op auth.Operation, p Profile, _ registrationstore.Registration, _ decisionartifacts.Record, m []collectorregistry.Material) error {
	ids := make([]shoal.ID, len(m))
	for i, v := range m {
		ids[i] = v.Observation.Observation.ID()
	}
	return a.Authorize(ctx, d, op, p, ids)
}

type rootMaterialAccess struct{ access *rootAccess }

func (a rootMaterialAccess) Authorize(_ context.Context, d auth.Decision, p shoal.ID, ids []shoal.ID) error {
	if a.access.deny || p != "purpose:registration" {
		return auth.ObjectNotFound()
	}
	for _, id := range ids {
		if e := d.Authorize(auth.OperationRead, auth.ResourceRequest{AuthorizationDomain: d.AuthorizationDomain(), SourceID: []byte(id), PolicyID: []byte("authority:source"), ObjectID: id}, time.Now()); e != nil {
			return e
		}
	}
	return nil
}
func (a rootMaterialAccess) Verify(ctx context.Context, d auth.Decision, p shoal.ID, m []collectorregistry.Material) error {
	ids := make([]shoal.ID, len(m))
	for i, v := range m {
		ids[i] = v.Observation.Observation.ID()
	}
	return a.Authorize(ctx, d, p, ids)
}

// Hand-authored model and byte-count features establish pipeline conformance,
// not trained model quality or safety classification accuracy.
type rootBuilder struct{}

func (rootBuilder) Build(_ context.Context, in BuildInput) (BuiltPicture, error) {
	var anchors []inference.EvidenceAnchor
	var subjects []decision.Subject
	var rows []map[string]any
	for _, s := range in.Sources {
		id := shoal.ID("subject:" + string(s.Source.ID))
		anchor, e := inference.NewDocumentAnchor(document.Citation{DocumentID: s.Source.ArtifactID, RevisionID: s.Source.RevisionID, SectionID: "whole", SpanID: "whole", Range: document.SourceRange{End: document.SourcePosition{Offset: int64(len(s.Bytes))}}}, string(s.Bytes))
		if e != nil {
			return BuiltPicture{}, e
		}
		anchors = append(anchors, anchor)
		subjects = append(subjects, decision.Subject{ID: id, SourceID: s.Source.ID, Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}})
		rows = append(rows, map[string]any{"id": id, "features": []float64{float64(len(s.Bytes)), float64(strings.Count(string(s.Bytes), "unsafe"))}})
	}
	raw, e := json.Marshal(map[string]any{"schema": 1, "feature_schema_id": "features:bytes-v1", "subjects": rows})
	if e != nil {
		return BuiltPicture{}, e
	}
	count := uint64(len(subjects))
	return BuiltPicture{Input: raw, Anchors: anchors, Subjects: subjects, Measurements: []decision.Measurement{{ID: "selected", Unit: "selected observations", MethodID: "selected-only:v1", Numerator: count, Denominator: &count}}, InputTokens: count, TokenBudget: 64}, nil
}

type rootCounted struct {
	*decisionlinear.Provider
	calls int
}

func (p *rootCounted) Resolve(ctx context.Context, release, id shoal.ID) (decisionservice.Predictor, error) {
	if _, e := p.Provider.Resolve(ctx, release, id); e != nil {
		return nil, e
	}
	return p, nil
}
func (p *rootCounted) Predict(ctx context.Context, r decision.DecisionRequest, b []byte) (decision.ResultConfig, error) {
	p.calls++
	return p.Provider.Predict(ctx, r, b)
}
func rootProfile(t *testing.T, name string) (Profile, *rootCounted) {
	t.Helper()
	ep, e := decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"authority:source"}})
	if e != nil {
		t.Fatal(e)
	}
	ranking, e := decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: "question", Weight: 1, Labels: []decision.LabelPriority{{Label: "retain", Value: 1}, {Label: "ordinary", Value: 0}}}}})
	if e != nil {
		t.Fatal(e)
	}
	task, e := decision.NewTaskSpec(decision.TaskConfig{OwnerID: "operator", Name: name, Version: "1", InputSchemaID: "features:bytes-v1", EvidencePolicyID: ep.ID(), LabelPolicyID: "labels", EvaluationPolicyID: "evaluation", PredictionUnit: "subject", LabelUnit: "finding", ActionUnit: "inspect", AggregationID: ranking.ID(), Questions: []decision.Question{{ID: "question", Kind: decision.Choice, RubricID: "rubric", Labels: []string{"ordinary", "retain"}}}})
	if e != nil {
		t.Fatal(e)
	}
	model, _ := json.Marshal(map[string]any{"schema": 1, "kind": "linear-svm", "task_id": task.ID(), "question_id": "question", "feature_schema_id": "features:bytes-v1", "dataset_sha256": strings.Repeat("a", 64), "recipe_sha256": strings.Repeat("b", 64), "training_runtime_sha256": strings.Repeat("c", 64), "labels": []string{"ordinary", "retain"}, "coefficients": []float64{1, 1}, "intercept": 0, "threshold": 0})
	provider, e := decisionlinear.New(decisionlinear.Config{ModelBytes: model, ExpectedSHA256: rootHash(model), ReleaseID: "release:conformance"})
	if e != nil {
		t.Fatal(e)
	}
	return Profile{AllowedModes: []collector.Mode{collector.Imported}, ID: shoal.ID("profile:" + name), RevisionID: "revision:1", BuilderID: "builder:bytes-v1", MaterialPurposeID: "purpose:registration", ReleaseID: "release:conformance", Task: task, Predictor: provider.Identity(), EvidencePolicy: ep, RankingPlan: ranking, TaskResource: auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}, SourceAuthorityPolicyID: "authority:source", Builder: rootBuilder{}, MaxDuration: time.Minute, Query: "Prioritize for full inspection", TokenizerID: "byte-count:v1", OntologyProjectionID: "no-ontology:v1"}, &rootCounted{Provider: provider}
}

type rootRuntime struct {
	eng       *engine.Engine
	registry  *collectorregistry.Registry
	service   *Service
	decisions *decisionservice.Service
}

func rootOpen(t *testing.T, dir string, a *auth.Authority, p Profile, provider *rootCounted, access *rootAccess) rootRuntime {
	t.Helper()
	eng, e := engine.Open(dir, engine.Options{})
	if e != nil {
		t.Fatal(e)
	}
	backend := func(table string) *explorercoord.EngineStore {
		if !slices.Contains(eng.TableNames(), table) {
			if e := eng.CreateTable(table, engine.TableOptions{}); e != nil {
				t.Fatal(e)
			}
		}
		b, e := explorercoord.NewEngineStore(eng, table)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	cr, e := collectorregistry.New(collectorregistry.Config{Backend: backend(collectorregistry.Table), Resolver: a.Resolver(), Clock: time.Now})
	if e != nil {
		t.Fatal(e)
	}
	materials, e := collectorregistry.NewMaterialResolver(collectorregistry.MaterialResolverConfig{Registry: cr, Authority: rootMaterialAccess{access}})
	if e != nil {
		t.Fatal(e)
	}
	registrations, e := registrationstore.New(registrationstore.Config{Backend: backend(registrationstore.Table), Clock: time.Now})
	if e != nil {
		t.Fatal(e)
	}
	service, e := New(Config{Resolver: a.Resolver(), Registrations: registrations, ArtifactBackend: backend(decisionartifacts.Table), Materials: materials, Profiles: []Profile{p}, Authority: access, Clock: time.Now})
	if e != nil {
		t.Fatal(e)
	}
	receipts, e := decisionstore.New(backend(decisionstore.Table), nil, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	ds, e := decisionservice.New(decisionservice.Config{Resolver: a.Resolver(), Artifacts: service.Artifacts(), Providers: provider, Receipts: receipts, Clock: time.Now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	return rootRuntime{eng, cr, service, ds}
}
func rootBind(t *testing.T, a *auth.Authority, subject string, sourceIDs ...shoal.ID) (context.Context, auth.Decision) {
	t.Helper()
	sources := [][]byte{[]byte("tasks")}
	for _, id := range sourceIDs {
		sources = append(sources, []byte(id))
	}
	d, e := auth.NewDecision(auth.DecisionConfig{PermittedSourceIDs: sources, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("authority:source")}, Subject: shoal.ID(subject), Actor: shoal.ID(subject), ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationIngest, auth.OperationRead, auth.OperationRetrieve, auth.OperationInvoke}, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "root-request"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := a.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	return ctx, d
}
func TestRealCollectorRegistrationCPUReplayAndRevocation(t *testing.T) {
	for _, name := range []string{"source", "review"} {
		t.Run(name, func(t *testing.T) {
			p, provider := rootProfile(t, name)
			a := auth.NewAuthority()
			var e error
			access := &rootAccess{}
			dir := t.TempDir()
			runtime := rootOpen(t, dir, a, p, provider, access)
			defer func() { runtime.eng.Close() }()
			collectCtx, _ := rootBind(t, a, "collector-principal")
			ctx, d := rootBind(t, a, "decision-principal")
			cid := shoal.ID("collector:" + name)
			if _, e = runtime.registry.Provision(context.Background(), collectorregistry.Provisioning{CollectorID: cid, Subject: "collector-principal", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:source"}, Control: collector.CandidateControlled, Mode: collector.Imported}); e != nil {
				t.Fatal(e)
			}
			extractor := collector.ExtractorRef{ID: "extractor:document", Version: "1"}
			if _, e = runtime.registry.Enroll(collectCtx, []byte("enroll"), collector.EnrollRequest{CollectorID: cid, RequestedAuthorityPolicyIDs: []shoal.ID{"authority:source"}, Extractors: []collector.ExtractorRef{extractor}}); e != nil {
				t.Fatal(e)
			}
			body := []byte("func inspect() { /* unsafe input remains data */ }")
			if name == "review" {
				body = []byte("Review unsafe operations and retain full source inspection.")
			}
			artifact := collector.ArtifactRef{ID: "artifact:document", Digest: rootHash(body), Size: int64(len(body)), MediaType: "text/plain", ObservedAt: time.Now().UTC().Add(-time.Second)}
			if _, e = runtime.registry.SubmitArtifact(collectCtx, cid, artifact); e != nil {
				t.Fatal(e)
			}
			o, e := collector.NewObservation(collector.ObservationConfig{CollectorID: cid, ArtifactID: artifact.ID, Extractor: extractor, SubjectID: "document", Kind: "source_document", Confidence: collector.Confidence{Disposition: collector.Extracted}, Payload: []byte(`{"description":"untrusted extractor assertion"}`), ObservedAt: time.Now().UTC()})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = runtime.registry.SubmitObservation(collectCtx, o); e != nil {
				t.Fatal(e)
			}
			ctx, d = rootBind(t, a, "decision-principal", o.ID())
			selection := Selection{ProfileID: p.ID, ProfileRevisionID: p.RevisionID, Sources: []SourceInput{{ObservationID: o.ID(), Bytes: body}}}
			key := []byte("registration")
			registered, e := runtime.service.Register(ctx, key, selection)
			if e != nil || registered.State != registrationstore.Ready {
				t.Fatal("register", e)
			}
			again, e := runtime.service.Register(ctx, key, selection)
			if e != nil || !reflect.DeepEqual(registered, again) {
				t.Fatal("registration retry changed identity", e)
			}
			bundle, e := runtime.service.Artifacts().LoadAuthorized(ctx, d, registered.Frozen.RequestID)
			if e != nil {
				t.Fatal(e)
			}
			if bundle.Request.Picture().Config().Sources[0].Control != decision.CandidateControlled {
				t.Fatal("candidate source authority laundered")
			}
			first, e := runtime.decisions.Evaluate(ctx, registered.Frozen.RequestID, []byte("inquiry"))
			if e != nil || provider.calls != 1 || first.Receipt.Result == nil {
				t.Fatal("CPU inference", e, provider.calls)
			}
			if e = runtime.eng.Close(); e != nil {
				t.Fatal(e)
			}
			provider.calls = 0
			runtime = rootOpen(t, dir, a, p, provider, access)
			reread, e := runtime.service.Read(ctx, registered.Frozen.RequestID)
			if e != nil || !reflect.DeepEqual(registered, reread) {
				t.Fatal("restart registration", e)
			}
			replay, e := runtime.decisions.Evaluate(ctx, registered.Frozen.RequestID, []byte("inquiry"))
			if e != nil || provider.calls != 0 || !reflect.DeepEqual(first, replay) {
				t.Fatal("durable replay", e, provider.calls)
			}
			access.deny = true
			if _, e = runtime.service.Read(ctx, registered.Frozen.RequestID); e == nil {
				t.Fatal("source revocation ignored")
			}
			access.deny = false
			if _, e = runtime.registry.Revoke(context.Background(), cid); e != nil {
				t.Fatal(e)
			}
			if _, e = runtime.service.Artifacts().LoadAuthorized(ctx, d, registered.Frozen.RequestID); e == nil {
				t.Fatal("collector revocation ignored")
			}
			if _, e = runtime.service.Register(ctx, key, selection); e == nil {
				t.Fatal("revoked exact retry disclosed")
			}
		})
	}
}
