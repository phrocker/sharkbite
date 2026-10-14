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

// shoal-local-ml-demo exercises an explicitly synthetic registered task. It is
// not a public registration API, production authority or model promotion tool.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

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

const featureSchema = "synthetic-numeric-signal-v1"

func definitions() (decision.TaskSpec, decision.EvidencePolicy, decision.RankingPlan, error) {
	policy, err := decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"synthetic-authority-v1"}})
	if err != nil {
		return decision.TaskSpec{}, policy, decision.RankingPlan{}, err
	}
	ranking, err := decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: "priority", Weight: 1, Labels: []decision.LabelPriority{{Label: "inspect", Value: 1}, {Label: "ordinary", Value: 0}}}}})
	if err != nil {
		return decision.TaskSpec{}, policy, ranking, err
	}
	task, err := decision.NewTaskSpec(decision.TaskConfig{OwnerID: "synthetic-demo", Name: "synthetic-linear-priority", Version: "1", InputSchemaID: "numeric-subjects-v1", EvidencePolicyID: policy.ID(), LabelPolicyID: "synthetic-labels-v1", EvaluationPolicyID: "synthetic-evaluation-v1", PredictionUnit: "subject", LabelUnit: "synthetic-sign", ActionUnit: "inspection", AggregationID: ranking.ID(), Questions: []decision.Question{{ID: "priority", Kind: decision.Choice, RubricID: "synthetic-positive-sign-v1", Labels: []string{"ordinary", "inspect"}}}})
	return task, policy, ranking, err
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// prepare writes a fixed numerical conformance dataset, not code-review evidence.
func prepare(path string) error {
	task, _, _, err := definitions()
	if err != nil {
		return err
	}
	rows := []map[string]any{}
	for i, value := range []float64{-4, -3, -2, -1, 1, 2, 3, 4, -2.5, 2.5, -3.5, 3.5} {
		split := "train"
		if i >= 8 {
			split = "test"
		}
		label := "ordinary"
		if value >= 0 {
			label = "inspect"
		}
		id := fmt.Sprintf("synthetic-%02d", i)
		rows = append(rows, map[string]any{"id": id, "family": id, "content_sha256": hash([]byte(fmt.Sprintf("signal=%.1f", value))), "source_revision": "synthetic-v1", "observed_at": "2026-10-01T00:00:00Z", "received_at": "2026-10-01T00:00:01Z", "label_received_at": "2026-10-01T00:00:02Z", "label_status": "verified", "training_allowed": true, "split": split, "features": []float64{value, 1}, "label": label})
	}
	data := map[string]any{"schema": 1, "kind": "numeric-training-dataset", "task_id": task.ID(), "question_id": "priority", "feature_schema_id": featureSchema, "labels": []string{"ordinary", "inspect"}, "cutoff": "2026-10-02T00:00:00Z", "provenance": "synthetic", "rows": rows}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return writeExclusive(path, b)
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

type demoState struct {
	Schema      int
	ModelSHA256 string
	CreatedAt   time.Time
}

func state(dir, modelSHA string) (demoState, error) {
	if err := makeStateDirectory(dir, syncDirectory); err != nil {
		return demoState{}, err
	}
	path := filepath.Join(dir, "demo-state.json")
	b, err := readBounded(path, 2048)
	if os.IsNotExist(err) {
		if _, engineErr := os.Stat(filepath.Join(dir, "engine")); engineErr == nil {
			return demoState{}, errors.New("demo state missing for existing engine; recovery required")
		} else if !os.IsNotExist(engineErr) {
			return demoState{}, engineErr
		}
		value := demoState{1, modelSHA, time.Now().UTC()}
		encoded, err := json.Marshal(value)
		if err != nil {
			return demoState{}, err
		}
		if err := writeExclusive(path, encoded); err != nil {
			return demoState{}, err
		}
		return value, nil
	}
	if err != nil {
		return demoState{}, err
	}
	var value demoState
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return demoState{}, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return demoState{}, errors.New("invalid state encoding")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, b) || value.Schema != 1 || value.ModelSHA256 != modelSHA || value.CreatedAt.IsZero() || value.CreatedAt.After(time.Now().UTC()) {
		return demoState{}, errors.New("state/model mismatch")
	}
	return value, nil
}

type fixtureAuthority struct{ record decisionartifacts.Record }

func (a *fixtureAuthority) AuthorizeRequest(_ context.Context, d auth.Decision, id shoal.ID) error {
	if id != a.record.Bundle.Request.ID() || d.Subject() != "synthetic-demo" {
		return auth.ObjectNotFound()
	}
	return d.AuthorizeObject(auth.OperationRead, a.record.Bundle.TaskResource, time.Now().UTC())
}
func (a *fixtureAuthority) Verify(_ context.Context, d auth.Decision, r decisionartifacts.Record) error {
	if !reflect.DeepEqual(r, a.record) {
		return auth.ObjectNotFound()
	}
	return d.AuthorizeObject(auth.OperationRetrieve, auth.ResourceRequest{AuthorizationDomain: []byte("synthetic-demo"), SourceID: []byte("fixture"), PolicyID: []byte("fixture-policy"), ObjectID: "synthetic-source"}, time.Now().UTC())
}

type countingProvider struct {
	*decisionlinear.Provider
	calls int
}

func (p *countingProvider) Predict(ctx context.Context, r decision.DecisionRequest, b []byte) (decision.ResultConfig, error) {
	p.calls++
	return p.Provider.Predict(ctx, r, b)
}
func (p *countingProvider) Resolve(ctx context.Context, release, predictor shoal.ID) (decisionservice.Predictor, error) {
	if _, err := p.Provider.Resolve(ctx, release, predictor); err != nil {
		return nil, err
	}
	return p, nil
}

type report struct {
	Synthetic       bool     `json:"synthetic"`
	ModelSHA256     string   `json:"model_sha256"`
	RequestID       shoal.ID `json:"request_id"`
	ReceiptID       string   `json:"receipt_id"`
	PredictionID    shoal.ID `json:"prediction_id"`
	Label           string   `json:"label"`
	EffectiveDevice string   `json:"effective_device"`
	ProviderCalls   int      `json:"provider_calls_this_process"`
	ReplayMatched   bool     `json:"replay_matched"`
}

func inquire(modelPath, expectedSHA, dir string) (report, error) {
	b, err := readBounded(modelPath, decisionlinear.MaxModelBytes)
	if err != nil {
		return report{}, err
	}
	release := shoal.ID("synthetic-release:" + expectedSHA)
	provider, err := decisionlinear.New(decisionlinear.Config{ModelBytes: b, ExpectedSHA256: expectedSHA, ReleaseID: release})
	if err != nil {
		return report{}, err
	}
	saved, err := state(dir, expectedSHA)
	if err != nil {
		return report{}, err
	}
	created := saved.CreatedAt
	task, policy, ranking, err := definitions()
	if err != nil {
		return report{}, err
	}
	d, err := auth.NewDecision(auth.DecisionConfig{Subject: "synthetic-demo", Actor: "local-demo", ClientID: "local-demo", AuthorizationDomain: []byte("synthetic-demo"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("fixture")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("fixture-policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "local-demo"})
	if err != nil {
		return report{}, err
	}
	authority := auth.NewAuthority()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx, err = authority.Binder().Bind(ctx, d)
	if err != nil {
		return report{}, err
	}
	fp, err := auth.AuthorizationFingerprint(d)
	if err != nil {
		return report{}, err
	}
	snapshot, err := inference.NewSnapshotPin("synthetic-snapshot-v1", created)
	if err != nil {
		return report{}, err
	}
	pin, err := inference.NewAuthPin(shoal.ID(fp.String()), created.Add(24*time.Hour))
	if err != nil {
		return report{}, err
	}
	raw := []byte(`{"kind":"synthetic","signal":2.5,"constant":1}`)
	anchor, err := inference.NewDocumentAnchor(document.Citation{DocumentID: "synthetic-source", RevisionID: "synthetic-v1", SectionID: "section", SpanID: "span", Range: document.SourceRange{End: document.SourcePosition{Offset: int64(len(raw))}}}, string(raw))
	if err != nil {
		return report{}, err
	}
	pack, err := inference.NewContextPack("classify synthetic numerical signal", []inference.EvidenceAnchor{anchor}, nil, snapshot, pin, nil)
	if err != nil {
		return report{}, err
	}
	input, err := json.Marshal(map[string]any{"schema": 1, "feature_schema_id": featureSchema, "subjects": []map[string]any{{"id": "subject", "features": []float64{2.5, 1}}}})
	if err != nil {
		return report{}, err
	}
	picture, err := decision.NewPictureManifest(pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "synthetic-observation-v1", EnumerationID: "synthetic-one-subject-v1", ScopeID: "synthetic-demo", BuilderID: "synthetic-builder-v1", OntologyProjectionID: "no-ontology-v1", InputDigest: hash(input), TokenizerID: "numeric-features-v1", InputTokens: 2, TokenBudget: 2, Cutoff: created, Sources: []decision.Source{{ID: "source", ArtifactID: "synthetic-source", RevisionID: "synthetic-v1", Digest: hash(raw), OriginID: "synthetic-demo", AuthorityPolicyID: "synthetic-authority-v1", Role: decision.Observation, Control: decision.CandidateControlled, ObservedAt: created, ReceivedAt: created}}, Subjects: []decision.Subject{{ID: "subject", SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}}}})
	if err != nil {
		return report{}, err
	}
	request, err := decision.NewDecisionRequest(task, picture, provider.Identity(), decision.RequestConfig{PrincipalID: d.Subject(), ReleaseID: release, CorrelationID: "synthetic-demo", RequestedAt: created, Deadline: created.Add(10 * time.Minute), SubjectIDs: []shoal.ID{"subject"}})
	if err != nil {
		return report{}, err
	}
	record := decisionartifacts.Record{Bundle: decisionservice.Bundle{Request: request, EvidencePolicy: policy, RankingPlan: ranking, Input: input, TaskResource: auth.ResourceRequest{AuthorizationDomain: d.AuthorizationDomain(), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}}, Sources: []decisionartifacts.SourceBytes{{ID: "source", Bytes: raw}}}
	record.Bundle.TaskResource, err = record.Bundle.TaskResource.Normalize()
	if err != nil {
		return report{}, err
	}
	eng, err := engine.Open(filepath.Join(dir, "engine"), engine.Options{})
	if err != nil {
		return report{}, err
	}
	defer eng.Close()
	existing := make(map[string]bool)
	for _, name := range eng.TableNames() {
		existing[name] = true
	}
	for _, name := range []string{decisionartifacts.Table, decisionstore.Table} {
		if !existing[name] {
			if err := eng.CreateTable(name, engine.TableOptions{}); err != nil {
				return report{}, err
			}
		}
	}
	ab, err := explorercoord.NewEngineStore(eng, decisionartifacts.Table)
	if err != nil {
		return report{}, err
	}
	catalog, err := decisionartifacts.New(decisionartifacts.Config{Backend: ab, Resolver: authority.Resolver(), Authority: &fixtureAuthority{record}, Clock: time.Now})
	if err != nil {
		return report{}, err
	}
	if err := catalog.Retain(ctx, record); err != nil {
		return report{}, err
	}
	rb, err := explorercoord.NewEngineStore(eng, decisionstore.Table)
	if err != nil {
		return report{}, err
	}
	receipts, err := decisionstore.New(rb, nil, time.Now)
	if err != nil {
		return report{}, err
	}
	counted := &countingProvider{Provider: provider}
	service, err := decisionservice.New(decisionservice.Config{Resolver: authority.Resolver(), Artifacts: catalog, Providers: counted, Receipts: receipts, Clock: time.Now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if err != nil {
		return report{}, err
	}
	first, err := service.Evaluate(ctx, request.ID(), []byte("synthetic-inquiry-v1"))
	if err != nil {
		return report{}, err
	}
	second, err := service.Evaluate(ctx, request.ID(), []byte("synthetic-inquiry-v1"))
	if err != nil {
		return report{}, err
	}
	if first.Receipt.Result == nil || first.Receipt.Result.Status != decision.Completed || len(first.Receipt.Result.Answers) != 1 {
		return report{}, errors.New("inquiry did not complete")
	}
	return report{true, expectedSHA, request.ID(), first.Receipt.ID, first.Receipt.PredictionID, first.Receipt.Result.Answers[0].Label, first.Receipt.Result.EffectiveDevice, counted.calls, first.Receipt.PredictionID == second.Receipt.PredictionID}, nil
}
func run(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("use prepare or inquire")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "prepare":
		output := flags.String("output", "", "new dataset path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *output == "" || flags.NArg() != 0 {
			return errors.New("prepare requires --output")
		}
		return prepare(*output)
	case "inquire":
		model := flags.String("model", "", "model.json path")
		expected := flags.String("model-sha256", "", "external expected model SHA256")
		dir := flags.String("state-dir", "", "persistent demo state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *model == "" || *expected == "" || *dir == "" || flags.NArg() != 0 {
			return errors.New("inquire requires --model, --model-sha256 and --state-dir")
		}
		r, err := inquire(*model, *expected, *dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(r)
	default:
		return errors.New("unknown command")
	}
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
