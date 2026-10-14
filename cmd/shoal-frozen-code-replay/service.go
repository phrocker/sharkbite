// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership. The ASF
// licenses this file under the Apache License, Version 2.0.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const localDomain = "frozen-code-local-registration-v1"
const sourcePolicy = "locally-imported-code-policy-v1"

func serviceDefinitions() (decision.TaskSpec, decision.EvidencePolicy, decision.RankingPlan, error) {
	policy, e := decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{sourcePolicy}})
	if e != nil {
		return decision.TaskSpec{}, policy, decision.RankingPlan{}, e
	}
	ranking, e := decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: question, Weight: 1, Labels: []decision.LabelPriority{{Label: "retain", Value: 1}, {Label: "lower_priority", Value: 0}}}}})
	if e != nil {
		return decision.TaskSpec{}, policy, ranking, e
	}
	task, e := decision.NewTaskSpec(decision.TaskConfig{OwnerID: localDomain, Name: "frozen-v9-imported-code-priority", Version: "1", InputSchemaID: featureSchema, EvidencePolicyID: policy.ID(), LabelPolicyID: "frozen-v9-priority-labels", EvaluationPolicyID: "offline-label-parity-only", PredictionUnit: "function", LabelUnit: "priority", ActionUnit: "full_review", AggregationID: ranking.ID(), Questions: []decision.Question{{ID: question, Kind: decision.Choice, RubricID: "frozen-v9-threshold", Labels: []string{"lower_priority", "retain"}}}})
	return task, policy, ranking, e
}
func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	// The filename anchors replay identity; syncing contents alone does not
	// make its directory entry durable across a crash.
	parentErr := syncDirectory(filepath.Dir(path))
	if err := errors.Join(writeErr, syncErr, closeErr, parentErr); err != nil {
		return fmt.Errorf("file may exist; durable publication unconfirmed: %w", err)
	}
	return nil
}
func readBounded(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, errors.New("file exceeds bound")
	}
	return b, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// Sync every ancestor, including on retry: a prior failed sync may have left an
// existing but not yet durable directory entry. The callback permits fault tests.
func makeStateDirectory(path string, syncDir func(string) error) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return err
	}
	for current := absolute; ; current = filepath.Dir(current) {
		if err := syncDir(current); err != nil {
			return fmt.Errorf("state directory may exist; durable publication unconfirmed: %w", err)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

type serviceState struct {
	PredictorID          shoal.ID
	Schema               int
	ModelSHA256          string
	ManifestSHA256       string
	SourceManifestSHA256 string
	CreatedAt            time.Time
}

func loadServiceState(dir, modelHash, manifestHash, sourceHash string, predictorID shoal.ID) (serviceState, error) {
	return loadServiceStateWithSync(dir, modelHash, manifestHash, sourceHash, predictorID, func(f *os.File) error { return f.Sync() })
}
func loadServiceStateWithSync(dir, modelHash, manifestHash, sourceHash string, predictorID shoal.ID, syncFile func(*os.File) error) (serviceState, error) {
	if !validDigest(modelHash) || !validDigest(manifestHash) || !validDigest(sourceHash) {
		return serviceState{}, errors.New("three external SHA256 pins required")
	}
	if e := shoal.ValidateRequiredID("predictor ID", predictorID); e != nil {
		return serviceState{}, e
	}
	if e := makeStateDirectory(dir, syncDirectory); e != nil {
		return serviceState{}, e
	}
	path := filepath.Join(dir, "service-state.json")
	b, e := readBounded(path, 2048)
	if os.IsNotExist(e) {
		if _, e := os.Stat(filepath.Join(dir, "engine")); e == nil {
			return serviceState{}, errors.New("state missing for existing engine; recovery required")
		} else if !os.IsNotExist(e) {
			return serviceState{}, e
		}
		state := serviceState{Schema: 2, PredictorID: predictorID, ModelSHA256: modelHash, ManifestSHA256: manifestHash, SourceManifestSHA256: sourceHash, CreatedAt: time.Now().UTC()}
		b, e := json.Marshal(state)
		if e != nil {
			return serviceState{}, e
		}
		if e = writeExclusive(path, b); e != nil {
			return serviceState{}, e
		}
		return state, nil
	}
	if e != nil {
		return serviceState{}, e
	}
	var state serviceState
	if e = json.Unmarshal(b, &state); e != nil {
		return serviceState{}, e
	}
	if state.Schema != 2 {
		return serviceState{}, errors.New("unsupported service state schema; explicit recovery required")
	}
	if state.PredictorID != predictorID {
		return serviceState{}, errors.New("incompatible predictor/runtime for persisted service state; explicit recovery required")
	}
	canonical, e := json.Marshal(state)
	if e != nil || !bytes.Equal(canonical, b) || state.Schema != 2 || state.ModelSHA256 != modelHash || state.ManifestSHA256 != manifestHash || state.SourceManifestSHA256 != sourceHash || state.CreatedAt.IsZero() || state.CreatedAt.After(time.Now().UTC()) {
		return serviceState{}, errors.New("state or pinned bundle mismatch")
	}

	// A prior creation can leave complete bytes after file.Sync failed. Retrying
	// must confirm the file itself, not merely its already-existing directory.
	f, e := os.Open(path)
	if e != nil {
		return serviceState{}, e
	}
	retained, readErr := io.ReadAll(io.LimitReader(f, 2049))
	if readErr != nil || !bytes.Equal(retained, b) {
		f.Close()
		return serviceState{}, errors.New("state changed while confirming durability")
	}
	syncErr := syncFile(f)
	closeErr := f.Close()
	if e = errors.Join(syncErr, closeErr); e != nil {
		return serviceState{}, fmt.Errorf("state exists; durable publication unconfirmed: %w", e)
	}
	return state, nil
}

// Only records rebuilt from the exact locally pinned import are registered.
// This is an in-process showcase authority, not an open registration endpoint.
// Every source gets its own grant and is checked before retained bytes are read.
type localRegistration struct {
	records   map[shoal.ID]decisionartifacts.Record
	resources map[shoal.ID][]auth.ResourceRequest
}

func (a *localRegistration) AuthorizeRequest(_ context.Context, d auth.Decision, id shoal.ID) error {
	record, ok := a.records[id]
	if !ok || d.Subject() != localDomain {
		return auth.ObjectNotFound()
	}
	if e := d.AuthorizeObject(auth.OperationRead, record.Bundle.TaskResource, time.Now().UTC()); e != nil {
		return e
	}
	for _, resource := range a.resources[id] {
		if e := d.AuthorizeObject(auth.OperationRetrieve, resource, time.Now().UTC()); e != nil {
			return e
		}
	}
	return nil
}
func (a *localRegistration) Verify(ctx context.Context, d auth.Decision, r decisionartifacts.Record) error {
	if e := a.AuthorizeRequest(ctx, d, r.Bundle.Request.ID()); e != nil {
		return e
	}
	if !reflect.DeepEqual(a.records[r.Bundle.Request.ID()], r) {
		return auth.ObjectNotFound()
	}
	return nil
}
func codeGrant(id string) []byte { return []byte("code:" + digest([]byte(id))) }
func localDecision(sources []sourceEvidence, denied map[string]bool) (auth.Decision, error) {
	grants := [][]byte{[]byte("tasks")}
	for _, source := range sources {
		if !denied[source.ID] {
			grants = append(grants, codeGrant(source.ID))
		}
	}
	return auth.NewDecision(auth.DecisionConfig{Subject: localDomain, Actor: "local-import", ClientID: "local-import", AuthorizationDomain: []byte(localDomain), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve}, PermittedSourceIDs: grants, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte(sourcePolicy)}, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "local-frozen-inquiry"})
}

type serviceProvider struct {
	*decisionlinear.Provider
	calls int
}

func (p *serviceProvider) Predict(ctx context.Context, r decision.DecisionRequest, input []byte) (decision.ResultConfig, error) {
	p.calls++
	return p.Provider.Predict(ctx, r, input)
}
func (p *serviceProvider) Resolve(ctx context.Context, r, pid shoal.ID) (decisionservice.Predictor, error) {
	if _, e := p.Provider.Resolve(ctx, r, pid); e != nil {
		return nil, e
	}
	return p, nil
}

func sourceRecord(task decision.TaskSpec, policy decision.EvidencePolicy, ranking decision.RankingPlan, provider *decisionlinear.Provider, d auth.Decision, state serviceState, source sourceEvidence, input []byte) (decisionartifacts.Record, error) {
	var zero decisionartifacts.Record
	created := state.CreatedAt
	if source.ID == "" || len(source.Bytes) == 0 || digest(source.Bytes) != source.Digest {
		return zero, errors.New("invalid registered source")
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return zero, e
	}
	snap, e := inference.NewSnapshotPin(shoal.ID("locally-imported-v9:"+source.Digest), created)
	if e != nil {
		return zero, e
	}
	pin, e := inference.NewAuthPin(shoal.ID(fp.String()), created.Add(24*time.Hour))
	if e != nil {
		return zero, e
	}
	quote := source.Bytes
	if len(quote) > 512 {
		quote = quote[:512]
		for len(quote) > 0 && !utf8.Valid(quote) {
			quote = quote[:len(quote)-1]
		}
	}
	sourceID := shoal.ID("cached-code:" + digest([]byte(source.ID)))
	anchor, e := inference.NewDocumentAnchor(document.Citation{DocumentID: sourceID, RevisionID: shoal.ID(source.Digest), SectionID: "cached-text-code", SpanID: "prefix", Range: document.SourceRange{End: document.SourcePosition{Offset: int64(len(quote))}}}, string(quote))
	if e != nil {
		return zero, e
	}
	pack, e := inference.NewContextPack("prioritize cached code; retain full review", []inference.EvidenceAnchor{anchor}, nil, snap, pin, nil)
	if e != nil {
		return zero, e
	}
	picture, e := decision.NewPictureManifest(pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "local-import:" + shoal.ID(source.Digest), EnumerationID: shoal.ID("frozen-v9-import:" + state.SourceManifestSHA256), ScopeID: localDomain, BuilderID: shoal.ID("pinned-numeric-import:" + state.ManifestSHA256), OntologyProjectionID: "none", InputDigest: digest(input), TokenizerID: "numeric-features-v1", InputTokens: 1, TokenBudget: 1, Cutoff: created, Sources: []decision.Source{{ID: sourceID, ArtifactID: sourceID, RevisionID: shoal.ID(source.Digest), Digest: source.Digest, OriginID: "cached-v9-text-code", AuthorityPolicyID: sourcePolicy, Role: decision.Observation, Control: decision.CandidateControlled, ObservedAt: created, ReceivedAt: created}}, Subjects: []decision.Subject{{ID: shoal.ID(source.ID), SourceID: sourceID, Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}}}})
	if e != nil {
		return zero, e
	}
	req, e := decision.NewDecisionRequest(task, picture, provider.Identity(), decision.RequestConfig{PrincipalID: d.Subject(), ReleaseID: release, CorrelationID: shoal.ID(source.ID), RequestedAt: created, Deadline: created.Add(time.Hour), SubjectIDs: []shoal.ID{shoal.ID(source.ID)}})
	if e != nil {
		return zero, e
	}
	resource, e := (auth.ResourceRequest{AuthorizationDomain: d.AuthorizationDomain(), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}).Normalize()
	if e != nil {
		return zero, e
	}
	return decisionartifacts.Record{Bundle: decisionservice.Bundle{Request: req, EvidencePolicy: policy, RankingPlan: ranking, TaskResource: resource, Input: input}, Sources: []decisionartifacts.SourceBytes{{ID: sourceID, Bytes: source.Bytes}}}, nil
}

type serviceSession struct {
	engine       *engine.Engine
	authority    *auth.Authority
	registration *localRegistration
	catalog      *decisionartifacts.Catalog
	service      *decisionservice.Service
	provider     *serviceProvider
	records      []decisionartifacts.Record
	decision     auth.Decision
}

func openServiceSession(bundle *loadedBundle, sources []sourceEvidence, state serviceState, stateDir string) (*serviceSession, error) {
	if state.Schema != 2 || state.PredictorID != bundle.Provider.Identity().ID() {
		return nil, errors.New("incompatible predictor/runtime for persisted service state; explicit recovery required")
	}
	task, policy, ranking, e := serviceDefinitions()
	if e != nil {
		return nil, e
	}
	d, e := localDecision(sources, nil)
	if e != nil {
		return nil, e
	}
	if len(sources) != len(bundle.Manifest.Rows) {
		return nil, errors.New("source cohort mismatch")
	}
	registration := &localRegistration{records: map[shoal.ID]decisionartifacts.Record{}, resources: map[shoal.ID][]auth.ResourceRequest{}}
	records := make([]decisionartifacts.Record, 0, len(sources))
	for i, source := range sources {
		if source.ID != bundle.Manifest.Rows[i].ID {
			return nil, errors.New("source ordering mismatch")
		}
		input, e := bundle.Input(i)
		if e != nil {
			return nil, e
		}
		record, e := sourceRecord(task, policy, ranking, bundle.Provider, d, state, source, input)
		if e != nil {
			return nil, e
		}
		id := record.Bundle.Request.ID()
		registration.records[id] = record
		registration.resources[id] = []auth.ResourceRequest{{AuthorizationDomain: []byte(localDomain), SourceID: codeGrant(source.ID), PolicyID: []byte(sourcePolicy), ObjectID: record.Sources[0].ID}}
		records = append(records, record)
	}
	eng, e := engine.Open(filepath.Join(stateDir, "engine"), engine.Options{})
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			eng.Close()
		}
	}()
	tables := map[string]bool{}
	for _, name := range eng.TableNames() {
		tables[name] = true
	}
	for _, name := range []string{decisionartifacts.Table, decisionstore.Table} {
		if !tables[name] {
			if e = eng.CreateTable(name, engine.TableOptions{}); e != nil {
				return nil, e
			}
		}
	}
	authority := auth.NewAuthority()
	ab, e := explorercoord.NewEngineStore(eng, decisionartifacts.Table)
	if e != nil {
		return nil, e
	}
	catalog, e := decisionartifacts.New(decisionartifacts.Config{Backend: ab, Resolver: authority.Resolver(), Authority: registration, Clock: time.Now})
	if e != nil {
		return nil, e
	}
	rb, e := explorercoord.NewEngineStore(eng, decisionstore.Table)
	if e != nil {
		return nil, e
	}
	receipts, e := decisionstore.New(rb, nil, time.Now)
	if e != nil {
		return nil, e
	}
	provider := &serviceProvider{Provider: bundle.Provider}
	service, e := decisionservice.New(decisionservice.Config{Resolver: authority.Resolver(), Artifacts: catalog, Providers: provider, Receipts: receipts, Clock: time.Now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if e != nil {
		return nil, e
	}
	ok = true
	return &serviceSession{eng, authority, registration, catalog, service, provider, records, d}, nil
}

type serviceRow struct {
	ID           string   `json:"id"`
	SourceSHA256 string   `json:"source_sha256"`
	PictureID    shoal.ID `json:"picture_id"`
	RequestID    shoal.ID `json:"request_id"`
	ReceiptID    string   `json:"receipt_id"`
	PredictionID shoal.ID `json:"prediction_id"`
	Label        string   `json:"label"`
}
type serviceReport struct {
	Kind                       string       `json:"kind"`
	Schema                     int          `json:"schema"`
	ModelSHA256                string       `json:"model_sha256"`
	ManifestSHA256             string       `json:"manifest_sha256"`
	SourceManifestSHA256       string       `json:"source_manifest_sha256"`
	ImportCreatedAt            time.Time    `json:"import_created_at"`
	ActualHistoricalTimestamps bool         `json:"actual_historical_timestamps"`
	Count                      int          `json:"count"`
	Matches                    int          `json:"matches"`
	Mismatches                 int          `json:"mismatches"`
	AllFullReview              bool         `json:"all_full_review"`
	OptimizationEnabled        bool         `json:"optimization_enabled"`
	ProviderCalls              int          `json:"provider_calls_this_process"`
	ReplayMatched              bool         `json:"replay_matched"`
	EffectiveDevice            string       `json:"effective_device"`
	PredictorID                shoal.ID     `json:"predictor_id"`
	Rows                       []serviceRow `json:"rows"`
}

func serviceKey(id string) []byte { return []byte("frozen-code-inquiry:" + digest([]byte(id))) }
func inquireService(dir, modelHash, manifestHash, sourceManifestHash, stateDir string) (serviceReport, error) {
	return inquireServiceRows(dir, modelHash, manifestHash, sourceManifestHash, stateDir, cohortSize)
}
func inquireServiceRows(dir, modelHash, manifestHash, sourceManifestHash, stateDir string, expectedRows int) (out serviceReport, retErr error) {
	task, _, _, e := serviceDefinitions()
	if e != nil {
		return out, e
	}
	bundle, e := loadBundle(filepath.Join(dir, "numeric"), modelHash, manifestHash, task, expectedRows)
	if e != nil {
		return out, e
	}
	defer bundle.Close()
	sources, e := loadSources(dir, sourceManifestHash, bundle)
	if e != nil {
		return out, e
	}

	lock, e := acquireSessionLock(stateDir)
	if e != nil {
		return out, e
	}
	// Registered first, this defer runs after the engine has fully closed.
	defer func() {
		if e := lock.Close(); e != nil {
			out = serviceReport{}
			retErr = errors.Join(retErr, e)
		}
	}()
	saved, e := loadServiceState(stateDir, modelHash, manifestHash, sourceManifestHash, bundle.Provider.Identity().ID())
	if e != nil {
		return out, e
	}
	session, e := openServiceSession(bundle, sources, saved, stateDir)
	if e != nil {
		return out, e
	}
	defer func() {
		if e := session.engine.Close(); e != nil {
			out = serviceReport{}
			retErr = errors.Join(retErr, e)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx, e = session.authority.Binder().Bind(ctx, session.decision)
	if e != nil {
		return out, e
	}
	out = serviceReport{Kind: "frozen-code-local-service-inquiry", Schema: 1, ModelSHA256: modelHash, ManifestSHA256: manifestHash, SourceManifestSHA256: sourceManifestHash, ImportCreatedAt: saved.CreatedAt, Count: len(sources), AllFullReview: true, ReplayMatched: true, EffectiveDevice: "cpu", PredictorID: bundle.Provider.Identity().ID(), Rows: make([]serviceRow, 0, len(sources))}
	for i, record := range session.records {
		if e = session.catalog.Retain(ctx, record); e != nil {
			return serviceReport{}, e
		}
		id := record.Bundle.Request.ID()
		key := serviceKey(sources[i].ID)
		first, e := session.service.Evaluate(ctx, id, key)
		if e != nil {
			return serviceReport{}, e
		}
		second, e := session.service.Evaluate(ctx, id, key)
		if e != nil {
			return serviceReport{}, e
		}
		read, e := session.service.Read(ctx, id, key)
		if e != nil {
			return serviceReport{}, e
		}
		if first.Receipt.Result == nil || first.Receipt.Result.Status != decision.Completed || len(first.Receipt.Result.Answers) != 1 || first.Receipt.Result.EffectiveDevice != "cpu" {
			return serviceReport{}, errors.New("inquiry did not complete on CPU")
		}
		if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, read) {
			return serviceReport{}, errors.New("receipt replay differed")
		}
		label := first.Receipt.Result.Answers[0].Label
		if label == bundle.Manifest.Rows[i].ExpectedLabel {
			out.Matches++
		} else {
			out.Mismatches++
		}
		out.Rows = append(out.Rows, serviceRow{sources[i].ID, sources[i].Digest, record.Bundle.Request.Picture().ID(), id, first.Receipt.ID, first.Receipt.PredictionID, label})
	}
	out.ProviderCalls = session.provider.calls
	if out.Mismatches != 0 {
		return out, errors.New("historical label parity failed")
	}
	return out, nil
}
