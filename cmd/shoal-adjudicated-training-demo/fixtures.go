// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// This executable is a finite synthetic conformance example, not a repository
// sampling authority, human witness system, or estimate of classifier quality.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const domain = "sealed-adjudicated-demo-v1"
const question = "property"

type taskDefinition struct {
	Task          decision.TaskSpec
	Evidence      decision.EvidencePolicy
	Ranking       decision.RankingPlan
	Labels        decision.LabelPolicy
	FeatureSchema shoal.ID
	Name          string
}

func definitions(name string) (taskDefinition, error) {
	var out taskDefinition
	if name != "source" && name != "review" {
		return out, fmt.Errorf("task must be source or review")
	}
	out.Name = name
	out.FeatureSchema = shoal.ID("sealed-" + name + "-numeric-v1")
	var err error
	out.Labels, err = decision.NewLabelPolicy(decision.LabelPolicyConfig{OwnerID: domain, Version: "1", AdjudicatorRoleID: "fixture-judge-role", DisputeResolverRoleID: "fixture-resolver-role", TrainingPurposeID: "fixture-training-purpose", MinIndependentWitnesses: 1})
	if err != nil {
		return out, err
	}
	out.Evidence, err = decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: 24 * time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"fixture-source-authority"}})
	if err != nil {
		return out, err
	}
	out.Ranking, err = decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: question, Weight: 1, Labels: []decision.LabelPriority{{Label: "negative", Value: 0}, {Label: "positive", Value: 1}}}}})
	if err != nil {
		return out, err
	}
	out.Task, err = decision.NewTaskSpec(decision.TaskConfig{OwnerID: domain, Name: "synthetic-" + name + "-property", Version: "1", InputSchemaID: "numeric-subjects-v1", EvidencePolicyID: out.Evidence.ID(), LabelPolicyID: out.Labels.ID(), EvaluationPolicyID: "fixture-conformance-only", PredictionUnit: "subject", LabelUnit: "narrow-objective-property", ActionUnit: "inspection", AggregationID: out.Ranking.ID(), Questions: []decision.Question{{ID: question, Kind: decision.Choice, RubricID: shoal.ID("fixture-" + name + "-oracle-v1"), Labels: []string{"negative", "positive"}}}})
	return out, err
}
func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }

type fixture struct {
	ID            shoal.ID
	Raw           []byte
	Split, Status string
}

func fixtures(name string) []fixture {
	var out []fixture
	for i := 0; i < 16; i++ {
		id := shoal.ID(fmt.Sprintf("%s-%02d", name, i))
		split := "train"
		if i >= 8 {
			split = "calibration"
		}
		if i >= 10 {
			split = "validation"
		}
		if i >= 12 {
			split = "test"
		}
		status := "verified"
		if i == 14 {
			status = "unknown"
		}
		if i == 15 {
			status = "disputed"
		}
		divisor := 2 + i
		if i%2 == 0 {
			divisor = 0
		}
		raw := []byte(fmt.Sprintf("package example\nfunc example%d(x int) int { return x / %d }\n", i, divisor))
		if name == "review" {
			line := 2
			if i%2 != 0 {
				line = 90 + i
			}
			raw = []byte(fmt.Sprintf("package example\nfunc example%d(x int) int { return x + %d }\n// original review claim: line=%d\n", i, i, line))
		}
		out = append(out, fixture{id, raw, split, status})
	}
	return out
}

// Features depend only on the original picture bytes. Neither adjudication nor
// revised code/review is an argument. Oracles below independently parse bytes.
func features(name string, raw []byte) ([]float64, error) {
	if name == "source" {
		return []float64{float64(strings.Count(string(raw), "/ 0")), float64(strings.Count(string(raw), "/ ")), 1}, nil
	}
	if name == "review" {
		var line int
		tail := strings.Split(string(raw), "// original review claim: line=")
		if len(tail) != 2 {
			return nil, fmt.Errorf("malformed fixture review")
		}
		if _, err := fmt.Sscanf(tail[1], "%d", &line); err != nil {
			return nil, err
		}
		return []float64{float64(line), float64(strings.Count(tail[0], "\n")), 1}, nil
	}
	return nil, fmt.Errorf("unknown feature task")
}
func oracle(name string, raw []byte) (string, error) {
	positive := false
	if name == "source" {
		f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", raw, 0)
		if err != nil {
			return "", err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			b, ok := n.(*ast.BinaryExpr)
			if ok && b.Op == token.QUO {
				lit, ok := b.Y.(*ast.BasicLit)
				if ok && lit.Kind == token.INT && lit.Value == "0" {
					positive = true
				}
			}
			return true
		})
	} else if name == "review" {
		parts := strings.Split(string(raw), "// original review claim: line=")
		if len(parts) != 2 {
			return "", fmt.Errorf("malformed review")
		}
		var line int
		if _, err := fmt.Sscanf(parts[1], "%d", &line); err != nil {
			return "", err
		}
		lines := strings.Split(strings.TrimSuffix(parts[0], "\n"), "\n")
		positive = line > 0 && line <= len(lines) && strings.TrimSpace(lines[line-1]) != ""
	} else {
		return "", fmt.Errorf("unknown oracle")
	}
	if positive {
		return "positive", nil
	}
	return "negative", nil
}
func baseline(def taskDefinition) (*decisionlinear.Provider, []byte, error) {
	// Hand-set bootstrap predictor; it supplies real proposed predictions, never labels.
	b, err := json.Marshal(map[string]any{"schema": 1, "kind": "linear-svm", "task_id": def.Task.ID(), "question_id": question, "feature_schema_id": def.FeatureSchema, "dataset_sha256": hash([]byte("untrained-bootstrap")), "recipe_sha256": hash([]byte("hand-set-zero-weights")), "training_runtime_sha256": hash([]byte("no-training-performed")), "labels": []string{"negative", "positive"}, "coefficients": []float64{0, 0, 0}, "intercept": -1, "threshold": 0})
	if err != nil {
		return nil, nil, err
	}
	p, err := decisionlinear.New(decisionlinear.Config{ModelBytes: b, ExpectedSHA256: hash(b), ReleaseID: shoal.ID("fixture-bootstrap:" + hash(b))})
	return p, b, err
}
func buildRecord(def taskDefinition, f fixture, provider *decisionlinear.Provider, release shoal.ID, d auth.Decision, created time.Time) (decisionartifacts.Record, error) {
	var zero decisionartifacts.Record
	fs, err := features(def.Name, f.Raw)
	if err != nil {
		return zero, err
	}
	input, err := json.Marshal(map[string]any{"schema": 1, "feature_schema_id": def.FeatureSchema, "subjects": []map[string]any{{"id": f.ID, "features": fs}}})
	if err != nil {
		return zero, err
	}
	fp, err := auth.AuthorizationFingerprint(d)
	if err != nil {
		return zero, err
	}
	snapshot, err := inference.NewSnapshotPin(shoal.ID("fixture-snapshot:"+hash(f.Raw)), created)
	if err != nil {
		return zero, err
	}
	pin, err := inference.NewAuthPin(shoal.ID(fp.String()), created.Add(time.Hour))
	if err != nil {
		return zero, err
	}
	anchor, err := inference.NewDocumentAnchor(document.Citation{DocumentID: f.ID, RevisionID: "original-fixture-v1", SectionID: "all", SpanID: "all", Range: document.SourceRange{End: document.SourcePosition{Offset: int64(len(f.Raw))}}}, string(f.Raw))
	if err != nil {
		return zero, err
	}
	pack, err := inference.NewContextPack("Classify the original synthetic fixture's narrow objective property", []inference.EvidenceAnchor{anchor}, nil, snapshot, pin, nil)
	if err != nil {
		return zero, err
	}
	picture, err := decision.NewPictureManifest(pack, decision.PictureConfig{TaskID: def.Task.ID(), ObservationID: f.ID, EnumerationID: "sealed-sixteen-fixtures-v1", ScopeID: domain, BuilderID: shoal.ID("original-bytes:" + string(def.FeatureSchema)), OntologyProjectionID: "no-ontology-v1", InputDigest: hash(input), TokenizerID: "numeric-features-v1", InputTokens: 3, TokenBudget: 3, Cutoff: created, Sources: []decision.Source{{ID: f.ID, ArtifactID: f.ID, RevisionID: "original-fixture-v1", Digest: hash(f.Raw), OriginID: "synthetic-source-controller", AuthorityPolicyID: "fixture-source-authority", Role: decision.Observation, Control: decision.CandidateControlled, ObservedAt: created, ReceivedAt: created}}, Subjects: []decision.Subject{{ID: f.ID, SourceID: f.ID, Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}}}})
	if err != nil {
		return zero, err
	}
	request, err := decision.NewDecisionRequest(def.Task, picture, provider.Identity(), decision.RequestConfig{PrincipalID: d.Subject(), ReleaseID: release, CorrelationID: f.ID, RequestedAt: created, Deadline: created.Add(10 * time.Minute), SubjectIDs: []shoal.ID{f.ID}})
	if err != nil {
		return zero, err
	}
	resource, err := auth.ResourceRequest{AuthorizationDomain: []byte(domain), SourceID: []byte("tasks"), PolicyID: []byte("fixture-policy"), ObjectID: def.Task.ID()}.Normalize()
	if err != nil {
		return zero, err
	}
	return decisionartifacts.Record{Bundle: decisionservice.Bundle{Request: request, EvidencePolicy: def.Evidence, RankingPlan: def.Ranking, TaskResource: resource, Input: input}, Sources: []decisionartifacts.SourceBytes{{ID: f.ID, Bytes: f.Raw}}}, nil
}
