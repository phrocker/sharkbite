// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
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
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const featureSchema = "frozen-v9-code-tfidf-60000-v1"
const question = "priority"
const release shoal.ID = "frozen-v9-offline-conformance-v1"
const cohortSize = 188
const originalThreshold = 0.2876853412223288
const archiveHash = "1e6b0fcc6025912d1181602830a0632d72e179d4c055f7784faacc8b9a437f11"

var originalMembers = map[string]string{
	"reserved/inputs-frozen.json": "1dcee1a5d6f62817d9ae15754955d875a55ffe3cef56a2f86362fe938d12cf6c",
	"reserved/predictions.json":   "4bf1c277778481250aed442b6f027ebcdfa29b7946a8bf41011ce17ef0bedad3",
	"reserved/selection.json":     "6d6e0510f3a18835c84af0ea66ce9ce9a5c613408491c7bfaa9d595bcbfc37f9",
	"trained/model.json":          "5c46253544281008b5292b7a99897f21eec339e060a92cdf507957b9d9c2b3a0",
}

type row struct {
	ID            string  `json:"id"`
	InputFile     string  `json:"input_file"`
	InputSHA256   string  `json:"input_sha256"`
	ExpectedLabel string  `json:"expected_label"`
	OriginalScore float64 `json:"original_score"`
}
type manifest struct {
	Schema          int             `json:"schema"`
	ModelSHA256     string          `json:"model_sha256"`
	TaskID          string          `json:"task_id"`
	QuestionID      string          `json:"question_id"`
	FeatureSchemaID string          `json:"feature_schema_id"`
	Rows            []row           `json:"rows"`
	Provenance      json.RawMessage `json:"provenance"`
}
type report struct {
	Kind                       string   `json:"kind"`
	ActualHistoricalTimestamps bool     `json:"actual_historical_timestamps"`
	SubjectIDs                 []string `json:"subject_ids"`
	Schema                     int      `json:"schema"`
	Count                      int      `json:"count"`
	Matches                    int      `json:"matches"`
	Mismatches                 int      `json:"mismatches"`
	AllFullReview              bool     `json:"all_full_review"`
	OptimizationEnabled        bool     `json:"optimization_enabled"`
	ModelSHA256                string   `json:"model_sha256"`
	ManifestSHA256             string   `json:"manifest_sha256"`
	PredictorID                shoal.ID `json:"predictor_id"`
	RuntimeID                  string   `json:"runtime_id"`
	EnvironmentDigest          string   `json:"environment_digest"`
	Labels                     []string `json:"labels"`
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func taskSpec() (decision.TaskSpec, error) {
	return decision.NewTaskSpec(decision.TaskConfig{OwnerID: "shoal-frozen-code-replay", Name: "frozen-v9-code-priority", Version: "1", InputSchemaID: featureSchema, EvidencePolicyID: "offline-frozen-v9-evidence", LabelPolicyID: "frozen-v9-priority-labels", EvaluationPolicyID: "offline-label-parity-only", PredictionUnit: "function", LabelUnit: "priority", ActionUnit: "full_review", AggregationID: "none", Questions: []decision.Question{{ID: question, Kind: decision.Choice, RubricID: "frozen-v9-threshold", Labels: []string{"lower_priority", "retain"}}}})
}

// exactObject rejects case aliases, absent fields and null before Go's struct
// decoder can apply case-insensitive matching or leave a scalar at zero.
func exactObject(raw []byte, fields map[string]string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if e := json.Unmarshal(raw, &object); e != nil {
		return nil, e
	}
	if object == nil || len(object) != len(fields) {
		return nil, errors.New("JSON object fields mismatch")
	}
	for key, kind := range fields {
		b, ok := object[key]
		if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return nil, fmt.Errorf("required non-null field %s", key)
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		if e := decoder.Decode(&value); e != nil {
			return nil, e
		}
		valid := false
		switch kind {
		case "string":
			_, valid = value.(string)
		case "number":
			_, valid = value.(json.Number)
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "bool":
			_, valid = value.(bool)
		}
		if !valid {
			return nil, fmt.Errorf("invalid field type %s", key)
		}
	}
	return object, nil
}
func validateManifestShape(raw []byte) error {
	object, e := exactObject(raw, map[string]string{"schema": "number", "model_sha256": "string", "task_id": "string", "question_id": "string", "feature_schema_id": "string", "rows": "array", "provenance": "object"})
	if e != nil {
		return e
	}
	var rows []json.RawMessage
	if e = json.Unmarshal(object["rows"], &rows); e != nil {
		return e
	}
	for _, row := range rows {
		if _, e = exactObject(row, map[string]string{"id": "string", "input_file": "string", "input_sha256": "string", "expected_label": "string", "original_score": "number"}); e != nil {
			return e
		}
	}
	return nil
}

// encoding/json replaces malformed Unicode with U+FFFD. A pinned manifest
// must preserve the exact scalar strings its producer wrote instead.
func validateUnicode(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("invalid UTF8")
	}
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
			return errors.New("incomplete escape")
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			return errors.New("incomplete Unicode escape")
		}
		value, e := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if e != nil {
			return e
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return errors.New("unpaired low surrogate")
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return errors.New("unpaired high surrogate")
			}
			low, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("invalid surrogate pair")
			}
			i += 6
		}
	}
	return nil
}

// The external digest binds exact bytes; strict decoding also rejects ambiguous
// duplicate fields and unsupported fields before interpreting the manifest.
func strictJSON(b []byte, dst any) error {
	return strictJSONShape(b, dst, validateManifestShape)
}
func strictJSONShape(b []byte, dst any, shape func([]byte) error) error {
	if e := validateUnicode(b); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON too deep")
		}
		tok, e := d.Token()
		if e != nil {
			return e
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					key, ok := k.(string)
					if !ok || seen[key] {
						return errors.New("duplicate JSON key")
					}
					seen[key] = true
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
				_, e = d.Token()
				return e
			case '[':
				for d.More() {
					if e := walk(depth + 1); e != nil {
						return e
					}
				}
				_, e = d.Token()
				return e
			default:
				return errors.New("invalid JSON delimiter")
			}
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	if e := shape(b); e != nil {
		return e
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func readBound(root *os.Root, name string, limit int64) ([]byte, error) {
	// Root confines even a raced symlink to the bundle. Static symlinks are refused.
	st, e := root.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("bundle member is not regular")
	}
	f, e := root.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("bundle member exceeds limit")
	}
	return b, nil
}
func request(task decision.TaskSpec, p *decisionlinear.Provider, id string, input []byte) (decision.DecisionRequest, error) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	quote := input
	if len(quote) > 512 {
		quote = quote[:512]
		for len(quote) > 0 && !utf8.Valid(quote) {
			quote = quote[:len(quote)-1]
		}
	}
	anchor, e := inference.NewDocumentAnchor(document.Citation{DocumentID: "numeric-input", RevisionID: shoal.ID(digest(input)), SectionID: "input", SpanID: "input", Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: int64(len(quote))}}}, string(quote))
	if e != nil {
		return decision.DecisionRequest{}, e
	}
	snap, e := inference.NewSnapshotPin("offline-frozen-export", now)
	if e != nil {
		return decision.DecisionRequest{}, e
	}
	auth, e := inference.NewAuthPin("offline-conformance-fixture", now.Add(time.Hour))
	if e != nil {
		return decision.DecisionRequest{}, e
	}
	pack, e := inference.NewContextPack("offline label conformance", []inference.EvidenceAnchor{anchor}, nil, snap, auth, nil)
	if e != nil {
		return decision.DecisionRequest{}, e
	}
	picture, e := decision.NewPictureManifest(pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "frozen-v9-export", EnumerationID: "frozen-v9-cohort", ScopeID: "offline", BuilderID: "numeric-export-v1", OntologyProjectionID: "none", InputDigest: digest(input), TokenizerID: "none", InputTokens: 1, TokenBudget: 1, Cutoff: now, Sources: []decision.Source{{ID: "numeric-input", ArtifactID: "numeric-input", RevisionID: shoal.ID(digest(input)), Digest: digest(input), OriginID: "frozen-export", AuthorityPolicyID: "offline-fixture", Role: decision.Observation, Control: decision.ExternalControlled, ObservedAt: now, ReceivedAt: now}}, Subjects: []decision.Subject{{ID: shoal.ID(id), SourceID: "numeric-input", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}}}})
	if e != nil {
		return decision.DecisionRequest{}, e
	}
	return decision.NewDecisionRequest(task, picture, p.Identity(), decision.RequestConfig{PrincipalID: "offline-fixture", ReleaseID: release, CorrelationID: shoal.ID(id), RequestedAt: now, Deadline: now.Add(time.Minute), SubjectIDs: []shoal.ID{shoal.ID(id)}})
}
func validateProvenance(raw json.RawMessage) error {
	p, e := exactObject(raw, map[string]string{"archive_sha256": "string", "original_members": "object", "original_model_id": "string", "original_inputs_id": "string", "original_threshold": "number", "original_prediction_id": "string", "original_selection_id": "string", "training_performed": "bool", "original_training_runtime_verified": "bool", "runtime_binding": "string", "optimization_enabled": "bool", "action": "string"})
	if e != nil {
		return e
	}
	var rawMembers map[string]json.RawMessage
	if e = json.Unmarshal(p["original_members"], &rawMembers); e != nil {
		return e
	}
	for _, member := range rawMembers {
		if _, e = exactObject(member, map[string]string{"path": "string", "sha256": "string", "bytes": "number"}); e != nil {
			return e
		}
	}

	var archive, action string
	var threshold float64
	if json.Unmarshal(p["archive_sha256"], &archive) != nil || archive != archiveHash || json.Unmarshal(p["action"], &action) != nil || action != "full_review" || json.Unmarshal(p["original_threshold"], &threshold) != nil || threshold != originalThreshold {
		return errors.New("frozen provenance mismatch")
	}
	for _, key := range []string{"optimization_enabled", "training_performed", "original_training_runtime_verified"} {
		if !bytes.Equal(bytes.TrimSpace(p[key]), []byte("false")) {
			return errors.New("unsupported provenance authority")
		}
	}
	var members map[string]struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	}
	if json.Unmarshal(p["original_members"], &members) != nil || len(members) != len(originalMembers) {
		return errors.New("original members missing")
	}
	for path, hash := range originalMembers {
		m := members[path]
		if m.Path != path || m.SHA256 != hash || m.Bytes <= 0 {
			return errors.New("original member pin mismatch")
		}
	}
	return nil
}

type loadedBundle struct {
	Root           *os.Root
	Manifest       manifest
	Provider       *decisionlinear.Provider
	ManifestSHA256 string
}

func (b *loadedBundle) Close() error { return b.Root.Close() }
func (b *loadedBundle) Input(index int) ([]byte, error) {
	if index < 0 || index >= len(b.Manifest.Rows) {
		return nil, errors.New("invalid row index")
	}
	r := b.Manifest.Rows[index]
	input, e := readBound(b.Root, r.InputFile, inference.MaxContextPackBytes)
	if e != nil {
		return nil, e
	}
	if digest(input) != r.InputSHA256 {
		return nil, errors.New("input SHA256 mismatch")
	}
	return input, nil
}
func loadBundle(dir, modelHash, manifestHash string, task decision.TaskSpec, expectedRows int) (*loadedBundle, error) {
	if !validDigest(modelHash) || !validDigest(manifestHash) {
		return nil, errors.New("external SHA256 pins required")
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	success := false
	defer func() {
		if !success {
			root.Close()
		}
	}()
	b, e := readBound(root, "manifest.json", 1<<20)
	if e != nil {
		return nil, e
	}
	if digest(b) != manifestHash {
		return nil, errors.New("manifest SHA256 mismatch")
	}
	var m manifest
	if e = strictJSON(b, &m); e != nil {
		return nil, e
	}
	if m.Schema != 1 || m.ModelSHA256 != modelHash || m.TaskID != string(task.ID()) || m.QuestionID != question || m.FeatureSchemaID != featureSchema || len(m.Rows) != expectedRows || expectedRows < 1 || expectedRows > cohortSize {
		return nil, errors.New("manifest contract or cohort mismatch")
	}
	if e = validateProvenance(m.Provenance); e != nil {
		return nil, e
	}
	model, e := readBound(root, "model.json", decisionlinear.MaxModelBytes)
	if e != nil {
		return nil, e
	}
	p, e := decisionlinear.New(decisionlinear.Config{ModelBytes: model, ExpectedSHA256: modelHash, ReleaseID: release})
	if e != nil {
		return nil, e
	}
	st, e := root.Lstat("inputs")
	if e != nil {
		return nil, e
	}
	if !st.IsDir() {
		return nil, errors.New("inputs is not a directory")
	}
	seen := map[string]bool{}
	for i, r := range m.Rows {
		if r.ID == "" || seen[r.ID] || r.InputFile != fmt.Sprintf("inputs/%04d.json", i) || !validDigest(r.InputSHA256) || (r.ExpectedLabel != "retain" && r.ExpectedLabel != "lower_priority") || math.IsNaN(r.OriginalScore) || math.IsInf(r.OriginalScore, 0) || r.OriginalScore < 0 || r.OriginalScore > 1 {
			return nil, errors.New("invalid manifest row")
		}
		seen[r.ID] = true
		expected := "retain"
		if r.OriginalScore < originalThreshold {
			expected = "lower_priority"
		}
		if r.ExpectedLabel != expected {
			return nil, errors.New("original score and label disagree")
		}
	}
	success = true
	return &loadedBundle{root, m, p, manifestHash}, nil
}
func replay(dir, modelHash, manifestHash string, expectedRows int) (report, error) {
	var out report
	task, e := taskSpec()
	if e != nil {
		return out, e
	}
	bundle, e := loadBundle(dir, modelHash, manifestHash, task, expectedRows)
	if e != nil {
		return out, e
	}
	defer bundle.Close()
	m, p := bundle.Manifest, bundle.Provider
	out = report{Kind: "offline-provider-conformance", Schema: 1, Count: len(m.Rows), AllFullReview: true, ModelSHA256: modelHash, ManifestSHA256: manifestHash, PredictorID: p.Identity().ID(), RuntimeID: string(p.Identity().Config().RuntimeID), EnvironmentDigest: p.Identity().Config().EnvironmentDigest, Labels: make([]string, 0, len(m.Rows))}
	for i, r := range m.Rows {
		input, e := bundle.Input(i)
		if e != nil {
			return report{}, e
		}
		req, e := request(task, p, r.ID, input)
		if e != nil {
			return report{}, e
		}
		result, e := p.Predict(context.Background(), req, input)
		if e != nil {
			return report{}, e
		}
		result.CompletedAt = req.Config().RequestedAt.Add(time.Second)
		if _, e = decision.NewPredictionRecord(req, result); e != nil {
			return report{}, e
		}
		if len(result.Answers) != 1 {
			return report{}, errors.New("expected exactly one answer")
		}
		label := result.Answers[0].Label
		out.Labels = append(out.Labels, label)
		out.SubjectIDs = append(out.SubjectIDs, r.ID)
		if label == r.ExpectedLabel {
			out.Matches++
		} else {
			out.Mismatches++
		}
	}
	return out, nil
}
func run(args []string, w io.Writer) error {
	if len(args) == 1 && args[0] == "describe-service" {
		task, _, _, e := serviceDefinitions()
		if e != nil {
			return e
		}
		return json.NewEncoder(w).Encode(map[string]any{"task_id": task.ID(), "question_id": question, "feature_schema_id": featureSchema, "labels": []string{"lower_priority", "retain"}})
	}
	if len(args) > 0 && args[0] == "inquire" {
		f := flag.NewFlagSet("inquire", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		dir := f.String("bundle", "", "source bundle directory")
		model := f.String("model-sha256", "", "external model pin")
		manifest := f.String("manifest-sha256", "", "external numeric manifest pin")
		sources := f.String("source-manifest-sha256", "", "external source manifest pin")
		state := f.String("state-dir", "", "persistent service state directory")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if f.NArg() != 0 || *dir == "" || *state == "" {
			return errors.New("inquire requires bundle and state directory; no positional arguments")
		}
		out, e := inquireService(*dir, *model, *manifest, *sources, *state)
		if e != nil {
			return e
		}
		return json.NewEncoder(w).Encode(out)
	}

	if len(args) == 1 && args[0] == "describe" {
		t, e := taskSpec()
		if e != nil {
			return e
		}
		return json.NewEncoder(w).Encode(map[string]any{"task_id": t.ID(), "question_id": question, "feature_schema_id": featureSchema, "labels": []string{"lower_priority", "retain"}})
	}
	if len(args) == 0 || args[0] != "replay" {
		return errors.New("usage: shoal-frozen-code-replay describe | replay --bundle DIR --model-sha256 SHA256 --manifest-sha256 SHA256")
	}
	f := flag.NewFlagSet("replay", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("bundle", "", "bundle directory")
	model := f.String("model-sha256", "", "external model pin")
	manifest := f.String("manifest-sha256", "", "external manifest pin")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 || *dir == "" {
		return errors.New("bundle required; no positional arguments")
	}
	out, e := replay(*dir, *model, *manifest, cohortSize)
	if e != nil {
		return e
	}
	if e = json.NewEncoder(w).Encode(out); e != nil {
		return e
	}
	if out.Mismatches != 0 {
		return errors.New("label parity failed")
	}
	return nil
}
func main() {
	if e := run(os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
