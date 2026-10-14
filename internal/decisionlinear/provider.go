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

// Package decisionlinear serves bounded, data-only numeric binary linear SVMs.
// Feature extraction is external and pinned by feature_schema_id. This is not a
// text/TF-IDF runtime, and its uncalibrated margins are never probabilities.
package decisionlinear

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxModelBytes = 4 << 20
const MaxFeatures = 65536

// MaxAbsFeature bounds supplied numeric features; values are rejected, never clipped.
const MaxAbsFeature = 1_000_000
const runtimeID = "decisionlinear:serial-float64:v1"
const formatterID = "decisionlinear:strict-numeric-json:v2"

type Config struct {
	ModelBytes     []byte
	ExpectedSHA256 string
	ReleaseID      shoal.ID
}

// Provider owns one immutable release. Construction is the readiness gate;
// Resolve never loads artifacts, invokes subprocesses, or accesses the network.
type Provider struct {
	gate                     chan struct{}
	identity                 decision.PredictorIdentity
	release                  shoal.ID
	task, question, features string
	labels                   [2]string
	coefficients             []float64
	intercept                float64
}

var _ decisionservice.Predictor = (*Provider)(nil)
var _ decisionservice.Providers = (*Provider)(nil)

func New(c Config) (*Provider, error) {
	if len(c.ModelBytes) == 0 || len(c.ModelBytes) > MaxModelBytes || !digestValid(c.ExpectedSHA256) || digest(c.ModelBytes) != c.ExpectedSHA256 {
		return nil, errors.New("invalid model size or external SHA256")
	}
	if shoal.ValidateRequiredID("release", c.ReleaseID) != nil || !utf8.ValidString(string(c.ReleaseID)) {
		return nil, errors.New("invalid release ID")
	}
	v, err := decode(context.Background(), c.ModelBytes)
	if err != nil {
		return nil, err
	}
	m, err := object(v, "schema", "kind", "task_id", "question_id", "feature_schema_id", "dataset_sha256", "recipe_sha256", "training_runtime_sha256", "labels", "coefficients", "intercept", "threshold")
	if err != nil {
		return nil, err
	}
	if m["schema"] != json.Number("1") || m["kind"] != "linear-svm" {
		return nil, errors.New("unsupported model schema/kind")
	}
	p := &Provider{release: c.ReleaseID, gate: make(chan struct{}, 1)}
	for key, dst := range map[string]*string{"task_id": &p.task, "question_id": &p.question, "feature_schema_id": &p.features} {
		s, ok := m[key].(string)
		if !ok || !textValid(s) {
			return nil, fmt.Errorf("invalid %s", key)
		}
		*dst = s
	}
	for _, key := range []string{"dataset_sha256", "recipe_sha256", "training_runtime_sha256"} {
		s, ok := m[key].(string)
		if !ok || !digestValid(s) {
			return nil, fmt.Errorf("invalid %s", key)
		}
	}
	labels, ok := m["labels"].([]any)
	if !ok || len(labels) != 2 {
		return nil, errors.New("model requires two labels")
	}
	for i, v := range labels {
		s, ok := v.(string)
		if !ok || !textValid(s) {
			return nil, errors.New("invalid label")
		}
		p.labels[i] = s
	}
	if p.labels[0] == p.labels[1] {
		return nil, errors.New("duplicate labels")
	}
	p.coefficients, err = numbers(m["coefficients"])
	if err != nil {
		return nil, err
	}
	p.intercept, err = number(m["intercept"])
	if err != nil {
		return nil, err
	}
	threshold, err := number(m["threshold"])
	if err != nil || threshold != 0 {
		return nil, errors.New("threshold must be zero")
	}
	p.identity, err = IdentityFor(c.ExpectedSHA256, p.features, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// IdentityFor is the predictor identity New gives a model with the given
// SHA-256 and feature schema when served by the given Go toolchain version,
// OS and architecture. It is a pure function, so a pinned identity can be
// checked on any toolchain; New uses it for the running one.
func IdentityFor(modelSHA256, featureSchemaID, goVersion, goos, goarch string) (decision.PredictorIdentity, error) {
	environment, _ := json.Marshal(struct{ Runtime, Formatter, Go, OS, Arch string }{runtimeID, formatterID, goVersion, goos, goarch})
	return decision.NewPredictorIdentity(decision.PredictorConfig{Provider: "local-linear-svm", RuntimeID: runtimeID, WeightsDigest: modelSHA256, TokenizerDigest: digest([]byte("decisionlinear:no-tokenizer:v1")), FormattingID: formatterID, PreprocessingID: shoal.ID(featureSchemaID), CalibrationID: "decisionlinear:uncalibrated-zero-margin:v1", EnvironmentDigest: digest(environment), Device: "cpu", Precision: "float64", BatchPolicyID: "decisionlinear:serial-subjects:v1", ReplayTolerance: 0})
}
func (p *Provider) Identity() decision.PredictorIdentity { return p.identity }
func (p *Provider) Resolve(ctx context.Context, release, predictor shoal.ID) (decisionservice.Predictor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if release != p.release || predictor != p.identity.ID() {
		return nil, errors.New("unregistered release/predictor")
	}
	return p, nil
}
func (p *Provider) Predict(ctx context.Context, r decision.DecisionRequest, input []byte) (decision.ResultConfig, error) {
	empty := decision.ResultConfig{}
	if p == nil || p.gate == nil {
		return empty, errors.New("provider is not ready")
	}
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return empty, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if len(input) == 0 || len(input) > inference.MaxContextPackBytes {
		return empty, errors.New("input exceeds bound")
	}
	if err := r.Validate(); err != nil {
		return empty, err
	}
	if r.PredictorID() != p.identity.ID() || r.Config().ReleaseID != p.release || string(r.TaskID()) != p.task {
		return empty, errors.New("request binding mismatch")
	}
	qs := r.Task().Config().Questions
	if len(qs) != 1 || string(qs[0].ID) != p.question || qs[0].Kind != decision.Choice || len(qs[0].Labels) != 2 || !((qs[0].Labels[0] == p.labels[0] && qs[0].Labels[1] == p.labels[1]) || (qs[0].Labels[1] == p.labels[0] && qs[0].Labels[0] == p.labels[1])) {
		return empty, errors.New("question/label mismatch")
	}
	if digest(input) != r.Picture().Config().InputDigest {
		return empty, errors.New("input digest mismatch")
	}
	v, err := decode(ctx, input)
	if err != nil {
		return empty, err
	}
	m, err := object(v, "schema", "feature_schema_id", "subjects")
	if err != nil {
		return empty, err
	}
	if m["schema"] != json.Number("1") || m["feature_schema_id"] != p.features {
		return empty, errors.New("feature schema mismatch")
	}
	subjects, ok := m["subjects"].([]any)
	ids := r.Config().SubjectIDs
	if !ok || len(subjects) != len(ids) {
		return empty, errors.New("subject membership mismatch")
	}
	pending := make(map[string]bool, len(ids))
	for _, id := range ids {
		pending[string(id)] = true
	}
	answers := make(map[shoal.ID]decision.Answer, len(ids))
	for _, v := range subjects {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		s, err := object(v, "id", "features")
		if err != nil {
			return empty, err
		}
		id, ok := s["id"].(string)
		if !ok || !pending[id] {
			return empty, errors.New("unexpected or duplicate subject")
		}
		delete(pending, id)
		xs, err := numbers(s["features"])
		if err != nil {
			return empty, err
		}
		if len(xs) != len(p.coefficients) {
			return empty, errors.New("feature width mismatch")
		}
		margin := p.intercept
		for i, x := range xs {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return empty, err
				}
			}
			if math.Abs(x) > MaxAbsFeature {
				return empty, errors.New("feature magnitude exceeds bound")
			}
			product := float64(x * p.coefficients[i])
			margin = float64(margin + product)
			if !finite(product) || !finite(margin) {
				return empty, errors.New("nonfinite margin")
			}
		}
		label := p.labels[0]
		if margin >= 0 {
			label = p.labels[1]
		}
		answers[shoal.ID(id)] = decision.Answer{SubjectID: shoal.ID(id), QuestionID: shoal.ID(p.question), Status: decision.Answered, Label: label}
	}
	result := decision.ResultConfig{RequestID: r.ID(), PredictorID: p.identity.ID(), EffectiveDevice: "cpu", Status: decision.Completed}
	for _, id := range ids {
		result.Answers = append(result.Answers, answers[id])
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return result, nil
}
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func digestValid(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func textValid(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= 1024
}
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
func number(v any) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, errors.New("expected finite number")
	}
	f, err := n.Float64()
	if err != nil || !finite(f) {
		return 0, errors.New("expected finite number")
	}
	return f, nil
}
func numbers(v any) ([]float64, error) {
	a, ok := v.([]any)
	if !ok || len(a) == 0 || len(a) > MaxFeatures {
		return nil, errors.New("invalid feature count")
	}
	out := make([]float64, len(a))
	for i, v := range a {
		f, err := number(v)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}
func object(v any, keys ...string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(keys) {
		return nil, errors.New("invalid object fields")
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return nil, errors.New("missing object field")
		}
	}
	return m, nil
}

// Token parsing rejects duplicate keys before they can be silently overwritten.
// Depth, total bytes, and scalar counts bound hostile inputs; null is never valid.
func decode(ctx context.Context, b []byte) (any, error) {
	if !utf8.Valid(b) || !validEscapes(b) {
		return nil, errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	count := 0
	v, err := value(ctx, d, 0, &count)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}
func value(ctx context.Context, d *json.Decoder, depth int, count *int) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	*count++
	if depth > 8 || *count > 1000000 {
		return nil, errors.New("JSON complexity bound")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				s, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid key")
				}
				if _, ok := m[s]; ok {
					return nil, errors.New("duplicate key")
				}
				v, err := value(ctx, d, depth+1, count)
				if err != nil {
					return nil, err
				}
				m[s] = v
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, errors.New("invalid object")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, err := value(ctx, d, depth+1, count)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, errors.New("invalid array")
			}
			return a, nil
		}
		return nil, errors.New("unexpected delimiter")
	case nil:
		return nil, errors.New("null not allowed")
	default:
		return t, nil
	}
}

// encoding/json replaces isolated UTF-16 surrogates with U+FFFD. Reject that
// lossy repair so identifiers remain exactly what the artifact encoded.
func validEscapes(b []byte) bool {
	inString := false
	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			return false
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			return false
		}
		n, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}
