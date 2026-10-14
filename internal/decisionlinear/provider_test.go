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

package decisionlinear

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func modelBytes(task string) []byte {
	b, _ := json.Marshal(map[string]any{"schema": 1, "kind": "linear-svm", "task_id": task, "question_id": "question", "feature_schema_id": "features:1", "dataset_sha256": strings.Repeat("a", 64), "recipe_sha256": strings.Repeat("b", 64), "training_runtime_sha256": strings.Repeat("c", 64), "labels": []string{"no", "yes"}, "coefficients": []float64{2, -1}, "intercept": 0, "threshold": 0})
	return b
}
func load(t *testing.T, b []byte) *Provider {
	t.Helper()
	p, err := New(Config{b, digest(b), "release:1"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func fixture(t *testing.T, input []byte) (*Provider, decision.DecisionRequest) {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	task, err := decision.NewTaskSpec(decision.TaskConfig{OwnerID: "owner", Name: "fixture", Version: "1", InputSchemaID: "features:1", EvidencePolicyID: "evidence:1", LabelPolicyID: "labels:1", EvaluationPolicyID: "eval:1", PredictionUnit: "subject", LabelUnit: "label", ActionUnit: "inspect", AggregationID: "aggregation:1", Questions: []decision.Question{{ID: "question", Kind: decision.Choice, RubricID: "rubric:1", Labels: []string{"yes", "no"}}}})
	if err != nil {
		t.Fatal(err)
	}
	p := load(t, modelBytes(string(task.ID())))
	a, err := inference.NewDocumentAnchor(document.Citation{DocumentID: "source", RevisionID: "revision", SectionID: "section", SpanID: "span", Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: 4}}}, "code")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := inference.NewSnapshotPin("snapshot", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin("auth", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("inspect", []inference.EvidenceAnchor{a}, nil, snap, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	picture, err := decision.NewPictureManifest(pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "run", EnumerationID: "inventory", ScopeID: "scope", BuilderID: "builder", OntologyProjectionID: "none", InputDigest: digest(input), TokenizerID: "none", InputTokens: 1, TokenBudget: 100, Cutoff: now, Sources: []decision.Source{{ID: "source", ArtifactID: "source", RevisionID: "revision", Digest: strings.Repeat("b", 64), OriginID: "origin", AuthorityPolicyID: "authority", Role: decision.Observation, Control: decision.ExternalControlled, ObservedAt: now, ReceivedAt: now}}, Subjects: []decision.Subject{{ID: "s1", SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{a.ID()}}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := decision.NewDecisionRequest(task, picture, p.Identity(), decision.RequestConfig{PrincipalID: "principal", ReleaseID: "release:1", CorrelationID: "call", RequestedAt: now, Deadline: now.Add(time.Minute), SubjectIDs: []shoal.ID{"s1"}})
	if err != nil {
		t.Fatal(err)
	}
	return p, r
}
func TestPrediction(t *testing.T) {
	for _, tc := range []struct{ features, label string }{{"[2,1]", "yes"}, {"[-1,1]", "no"}, {"[1,2]", "yes"}} {
		input := []byte(`{"schema":1,"feature_schema_id":"features:1","subjects":[{"id":"s1","features":` + tc.features + `}]}`)
		p, r := fixture(t, input)
		for i := 0; i < 2; i++ {
			result, err := p.Predict(context.Background(), r, input)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Answers) != 1 || result.Answers[0].Label != tc.label || len(result.Answers[0].Distribution) != 0 || result.Answers[0].Probability != nil {
				t.Fatalf("bad result %+v", result)
			}
			result.CompletedAt = r.Config().RequestedAt.Add(time.Second)
			if _, err := decision.NewPredictionRecord(r, result); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func TestRejectModels(t *testing.T) {
	good := string(modelBytes("task"))
	cases := []string{strings.Replace(good, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(good, `"threshold":0`, `"Threshold":0`, 1), strings.Replace(good, `"threshold":0`, `"threshold":null`, 1), strings.Replace(good, `"threshold":0`, `"threshold":1`, 1), strings.Replace(good, `"coefficients":[2,-1]`, `"coefficients":[true,1]`, 1), strings.Replace(good, `"coefficients":[2,-1]`, `"coefficients":[1e999,1]`, 1), strings.Replace(good, `"labels":["no","yes"]`, `"labels":["no","no"]`, 1), good + `{}`, `[]`, strings.Repeat(" ", MaxModelBytes+1)}
	for _, s := range cases {
		if _, err := New(Config{[]byte(s), digest([]byte(s)), "release:1"}); err == nil {
			t.Fatalf("accepted invalid model %.100s", s)
		}
	}
	if _, err := New(Config{[]byte(good), strings.Repeat("0", 64), "release:1"}); err == nil {
		t.Fatal("wrong external digest accepted")
	}
	b := []byte(good)
	p := load(t, b)
	original := p.Identity().ID()
	for i := range b {
		b[i] = ' '
	}
	if p.Identity().ID() != original || p.coefficients[0] != 2 {
		t.Fatal("caller bytes alias runtime")
	}
	if p.Identity().Config().EnvironmentDigest == strings.Repeat("c", 64) {
		t.Fatal("training runtime used as serving runtime")
	}
}
func TestRejectInputs(t *testing.T) {
	base := `{"schema":1,"feature_schema_id":"features:1","subjects":[{"id":"s1","features":[1,2]}]}`
	cases := []string{strings.Replace(base, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(base, `"id":"s1"`, `"id":"s2"`, 1), strings.Replace(base, `[1,2]`, `[1]`, 1), strings.Replace(base, `[1,2]`, `[true,2]`, 1), strings.Replace(base, `[1,2]`, `[null,2]`, 1), strings.Replace(base, `[1,2]`, `[1e308,2]`, 1), strings.Replace(base, `features:1`, `features:2`, 1), strings.Replace(base, `"id":"s1"`, `"id":"s1","extra":0`, 1), base + `{}`}
	for _, s := range cases {
		b := []byte(s)
		p, r := fixture(t, b)
		if _, err := p.Predict(context.Background(), r, b); err == nil {
			t.Fatalf("accepted bad input %s", s)
		}
	}
	p, r := fixture(t, []byte(base))
	if _, err := p.Predict(context.Background(), r, []byte(base+" ")); err == nil {
		t.Fatal("unbound input accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Predict(ctx, r, []byte(base)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	p.gate <- struct{}{}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := p.Predict(ctx, r, []byte(base)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	<-p.gate
	for _, pair := range [][2]shoal.ID{{"release:2", p.Identity().ID()}, {"release:1", "alias"}} {
		if _, err := p.Resolve(context.Background(), pair[0], pair[1]); err == nil {
			t.Fatal("alias resolved")
		}
	}
	if _, err := p.Resolve(context.Background(), "release:1", p.Identity().ID()); err != nil {
		t.Fatal(err)
	}
}

func TestStrictJSONUnicodeAndDepth(t *testing.T) {
	for _, s := range []string{`{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800\u0041"}`, `{"x":[[[[[[[[[1]]]]]]]]]}`} {
		if _, err := decode(context.Background(), []byte(s)); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	for _, s := range []string{`{"x":"\ud83d\ude00"}`, `{"x":"\\ud800"}`} {
		if _, err := decode(context.Background(), []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRejectRequestBindings(t *testing.T) {
	input := []byte(`{"schema":1,"feature_schema_id":"features:1","subjects":[{"id":"s1","features":[1,2]}]}`)
	original, r := fixture(t, input)
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["task_id"] = "another-task" },
		func(m map[string]any) { m["question_id"] = "another-question" },
		func(m map[string]any) { m["labels"] = []string{"no", "different"} },
	} {
		m := map[string]any{}
		if err := json.Unmarshal(modelBytes(string(r.TaskID())), &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		p := load(t, b)
		rr, err := decision.NewDecisionRequest(r.Task(), r.Picture(), p.Identity(), r.Config())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Predict(context.Background(), rr, input); err == nil {
			t.Fatal("unmatched model contract accepted")
		}
		if _, err := original.Predict(context.Background(), rr, input); err == nil {
			t.Fatal("wrong predictor accepted")
		}
	}
	c := r.Config()
	c.ReleaseID = "another-release"
	rr, err := decision.NewDecisionRequest(r.Task(), r.Picture(), original.Identity(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := original.Predict(context.Background(), rr, input); err == nil {
		t.Fatal("wrong release accepted")
	}
}

func TestFeatureMagnitudeBoundsAndArithmeticOverflow(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"1000000", true}, {"-1000000", true}, {"1000000.0001", false}, {"-1000000.0001", false}, {"1e308", false},
	} {
		input := []byte(`{"schema":1,"feature_schema_id":"features:1","subjects":[{"id":"s1","features":[` + tc.value + `,0]}]}`)
		p, r := fixture(t, input)
		_, err := p.Predict(context.Background(), r, input)
		if (err == nil) != tc.valid {
			t.Fatalf("value %s: %v", tc.value, err)
		}
	}
	input := []byte(`{"schema":1,"feature_schema_id":"features:1","subjects":[{"id":"s1","features":[1000000,0]}]}`)
	_, r := fixture(t, input)
	// Coefficients and intercept are finite-only, independent of input bounds.
	for _, tc := range []struct {
		coeff, intercept float64
		overflow         bool
	}{
		{1e308, 0, true}, {1, 1e308, false},
	} {
		m := map[string]any{}
		if err := json.Unmarshal(modelBytes(string(r.TaskID())), &m); err != nil {
			t.Fatal(err)
		}
		m["coefficients"] = []float64{tc.coeff, 0}
		m["intercept"] = tc.intercept
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		p := load(t, b)
		rr, err := decision.NewDecisionRequest(r.Task(), r.Picture(), p.Identity(), r.Config())
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Predict(context.Background(), rr, input)
		if tc.overflow {
			if err == nil || err.Error() != "nonfinite margin" {
				t.Fatalf("expected arithmetic overflow, got %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}
