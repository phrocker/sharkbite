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

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	lowmodel "github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestDeterministicFixtureEvaluationReport(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	root := filepath.Join("..", "..", "..", "testdata", "explorer-eval")
	cases, err := LoadFixtureEvaluationCases(root, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runFixtureEvaluation(t, cases, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runFixtureEvaluation(t, cases, now)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := first.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("evaluation output is nondeterministic\nfirst:\n%s\nsecond:\n%s", firstJSON, secondJSON)
	}
	if first.Summary.CaseCount != 13 ||
		first.Summary.ClaimCount != 11 ||
		first.Summary.ExpectedClaimCount != 18 ||
		first.Summary.SupportedClaimCount != 11 ||
		first.Summary.MissingExpectedClaims != 7 ||
		first.Summary.GroundingSupportRate != 1 ||
		first.Summary.UnsupportedIssueCount != 2 ||
		first.Summary.InvalidCitationRefs != 0 ||
		first.Summary.InvalidGraphPathRefs != 0 ||
		first.Summary.CitationReferenceCount != 10 ||
		first.Summary.GraphPathReferenceCount != 1 ||
		first.Summary.StopReasons[StopReasonStop] != 13 {
		t.Fatalf("unexpected metrics: %#v", first.Summary)
	}
	citationRefs, graphRefs := 0, 0
	for _, report := range first.Cases {
		if report.Iterations != 2 || report.Budget.ModelCalls != 2 ||
			report.InvalidEvidenceRefs != 0 ||
			report.TraceDigest == "" {
			t.Fatalf("case metrics not defensible: %#v", report)
		}
		citationRefs += report.CitationReferences
		graphRefs += report.GraphPathReferences
	}
	if citationRefs != 10 || graphRefs != 1 {
		t.Fatalf("unexpected citation/path coverage: citations=%d graph=%d", citationRefs, graphRefs)
	}
}

func TestEvaluationDetectsNegativeCases(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	root := filepath.Join("..", "..", "..", "testdata", "explorer-eval")
	cases, err := LoadFixtureEvaluationCases(root, now)
	if err != nil {
		t.Fatal(err)
	}
	base := cases[:1]

	t.Run("unsupported claim", func(t *testing.T) {
		g := scriptedEvaluationGenerator(t, now, func(_ context.Context, tr Transcript) (Action, error) {
			value, _ := ontology.NewStringValue("not-grounded")
			claim, err := inference.NewClaim(
				"subject:unexpected", "predicate:summary", value, 1,
				[]shoal.ID{tr.Context().Evidence()[0].ID()}, inference.ClaimInferred,
				evalModelProvenance(t), evalPromptProvenance(t), nil,
			)
			if err != nil {
				return Action{}, err
			}
			result, err := inference.NewInferenceResult(tr.Context(), []inference.Claim{claim}, nil, now, nil)
			if err != nil {
				return Action{}, err
			}
			return NewStopAction("stop", result, Usage{})
		})
		report, err := Evaluate(context.Background(), g, base, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.SupportedClaimCount != 0 || report.Summary.GroundingSupportRate != 0 {
			t.Fatalf("unsupported claim counted as supported: %#v", report.Summary)
		}
	})

	t.Run("hallucinated extra claim lowers support rate", func(t *testing.T) {
		g := scriptedEvaluationGenerator(t, now, func(_ context.Context, tr Transcript) (Action, error) {
			expected := base[0].ExpectedClaims[0]
			good, err := inference.NewClaim(
				expected.Subject, expected.Predicate, expected.Object, 1, expected.EvidenceIDs,
				inference.ClaimInferred, evalModelProvenance(t), evalPromptProvenance(t), nil,
			)
			if err != nil {
				return Action{}, err
			}
			value, _ := ontology.NewStringValue("not-grounded")
			bad, err := inference.NewClaim(
				"subject:unexpected", "predicate:summary", value, 1,
				[]shoal.ID{tr.Context().Evidence()[0].ID()}, inference.ClaimInferred,
				evalModelProvenance(t), evalPromptProvenance(t), nil,
			)
			if err != nil {
				return Action{}, err
			}
			result, err := inference.NewInferenceResult(tr.Context(), []inference.Claim{good, bad}, nil, now, nil)
			if err != nil {
				return Action{}, err
			}
			return NewStopAction("stop", result, Usage{})
		})
		report, err := Evaluate(context.Background(), g, base, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.SupportedClaimCount != 1 || report.Summary.ClaimCount != 2 ||
			report.Summary.GroundingSupportRate != 0.5 {
			t.Fatalf("hallucinated claim did not lower support rate: %#v", report.Summary)
		}
	})

	t.Run("stale citation", func(t *testing.T) {
		stale := append([]EvaluationCase(nil), base...)
		stale[0].Sources = map[DocumentRevision]string{
			DocumentRevision{DocumentID: "aster-relay-protocol", RevisionID: "r2"}: "fixture contents changed",
		}
		report, err := runFixtureEvaluation(t, stale, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.InvalidCitationRefs != 1 {
			t.Fatalf("stale citation was not detected: %#v", report.Summary)
		}
	})

	t.Run("unresolved stale citation", func(t *testing.T) {
		stale := append([]EvaluationCase(nil), base...)
		stale[0].Sources = map[DocumentRevision]string{
			DocumentRevision{DocumentID: "aster-relay-protocol", RevisionID: "r2"}: "fixture contents changed",
		}
		g := scriptedEvaluationGenerator(t, now, func(_ context.Context, tr Transcript) (Action, error) {
			issue, err := inference.NewIssue(
				inference.IssueUnresolved, "input", "needs more evidence",
				[]shoal.ID{tr.Context().Evidence()[0].ID()},
			)
			if err != nil {
				return Action{}, err
			}
			result, err := inference.NewInferenceResult(tr.Context(), nil, []inference.Issue{issue}, now, nil)
			if err != nil {
				return Action{}, err
			}
			return NewStopAction("stop", result, Usage{})
		})
		report, err := Evaluate(context.Background(), g, stale, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.InvalidCitationRefs != 1 {
			t.Fatalf("unresolved stale citation was not detected: %#v", report.Summary)
		}
	})

	t.Run("invalid graph path", func(t *testing.T) {
		graphCase := []EvaluationCase{graphEvaluationCase(t, now)}
		graphCase[0].GraphPaths = map[shoal.ID]graph.Path{}
		report, err := runFixtureEvaluation(t, graphCase, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.InvalidGraphPathRefs != 1 {
			t.Fatalf("invalid graph path was not detected: %#v", report.Summary)
		}
	})

	t.Run("invalid graph path metadata", func(t *testing.T) {
		graphCase := []EvaluationCase{graphEvaluationCase(t, now)}
		anchorID := graphCase[0].Pack.Evidence()[0].ID()
		want := graphCase[0].GraphPaths[anchorID]
		want.Nodes = append([]graph.Node(nil), want.Nodes...)
		want.Edges = append([]graph.Edge(nil), want.Edges...)
		want.Nodes[0].Labels = []string{"unexpected-label"}
		want.Nodes[0].Properties = shoal.Metadata{"unexpected": "node"}
		want.Edges[0].Weight = 0.5
		want.Edges[0].Properties = shoal.Metadata{"unexpected": "edge"}
		graphCase[0].GraphPaths = map[shoal.ID]graph.Path{anchorID: want}
		report, err := runFixtureEvaluation(t, graphCase, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.InvalidGraphPathRefs != 1 {
			t.Fatalf("invalid graph path metadata was not detected: %#v", report.Summary)
		}
	})

	t.Run("budget violation", func(t *testing.T) {
		g := scriptedEvaluationGenerator(t, now, func(context.Context, Transcript) (Action, error) {
			return mustRetrieve(t, "retrieve", "more", 1), nil
		})
		g.budgets.MaxSteps = 1
		report, err := Evaluate(context.Background(), g, base, now)
		if err != nil {
			t.Fatal(err)
		}
		if report.Cases[0].StopReason != StopReasonBudgetExhausted || report.Cases[0].Error == "" {
			t.Fatalf("budget violation not reported: %#v", report.Cases[0])
		}
	})
}

func TestFixtureOntologyValueRejectsNull(t *testing.T) {
	if _, err := fixtureOntologyValue(json.RawMessage("null")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("null fixture value error = %v", err)
	}
}

func TestSafeFixtureCorpusPathRejectsEscape(t *testing.T) {
	root := filepath.Join("..", "..", "..", "testdata", "explorer-eval")
	if _, err := safeFixtureCorpusPath(root, "..\\README.md"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("escaped fixture path error = %v", err)
	}
}

func TestEvaluationTraceDigestDetectsDifferentToolTraces(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	root := filepath.Join("..", "..", "..", "testdata", "explorer-eval")
	cases, err := LoadFixtureEvaluationCases(root, now)
	if err != nil {
		t.Fatal(err)
	}
	makeRunner := func(query string) *Generator {
		return scriptedEvaluationGenerator(t, now,
			func(context.Context, Transcript) (Action, error) {
				return mustRetrieve(t, "retrieve", query, 1), nil
			},
			func(_ context.Context, tr Transcript) (Action, error) {
				result := evalResultFor(t, tr.Context(), tr.Context().Evidence()[0], now)
				return NewStopAction("stop", result, Usage{})
			},
		)
	}
	first, err := Evaluate(context.Background(), makeRunner("alpha"), cases[:1], now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Evaluate(context.Background(), makeRunner("beta"), cases[:1], now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cases[0].Iterations != second.Cases[0].Iterations ||
		first.Cases[0].Budget != second.Cases[0].Budget {
		t.Fatal("test setup should differ only by trace identity")
	}
	if first.Cases[0].TraceDigest == second.Cases[0].TraceDigest {
		t.Fatal("different tool traces produced identical report digest")
	}
}

func TestEvaluationRejectsDuplicateCaseIDs(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	root := filepath.Join("..", "..", "..", "testdata", "explorer-eval")
	cases, err := LoadFixtureEvaluationCases(root, now)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := []EvaluationCase{cases[0], cases[1]}
	duplicate[1].ID = duplicate[0].ID
	g := scriptedEvaluationGenerator(t, now)
	if _, err := Evaluate(context.Background(), g, duplicate, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate case ID error = %v", err)
	}
}

func TestFixtureGraphPathValidationAcceptsCanonicalLabelOrder(t *testing.T) {
	path := graph.Path{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity", Labels: []string{"zeta", "alpha"}},
			{ID: "node-b", Kind: "entity"},
		},
		Edges: []graph.Edge{{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1}},
	}
	anchor, err := inference.NewGraphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical, ok := anchor.Path()
	if !ok {
		t.Fatal("graph anchor did not expose a path")
	}
	if err := validateFixtureGraphPath(anchor.ID(), canonical, map[shoal.ID]graph.Path{anchor.ID(): path}); err != nil {
		t.Fatalf("canonical label ordering rejected: %v", err)
	}
}

func runFixtureEvaluation(t *testing.T, cases []EvaluationCase, now time.Time) (EvaluationReport, error) {
	t.Helper()
	modelProvenance := evalModelProvenance(t)
	prompt := evalPromptProvenance(t)
	provenance, err := NewProvenance("fixture-harness", modelProvenance, prompt, "fixture-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewModelRunner(lowmodel.FakeGenerator{Model: "deterministic"}, ModelRunnerConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	g := evalGenerator(t, runner, now, provenance)
	return Evaluate(context.Background(), g, cases, now)
}

func scriptedEvaluationGenerator(t *testing.T, now time.Time, steps ...ScriptStep) *Generator {
	t.Helper()
	provenance, err := NewProvenance("fixture-harness", evalModelProvenance(t), evalPromptProvenance(t), "fixture-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	return evalGenerator(t, NewFakeRunner(steps...), now, provenance)
}

func evalGenerator(t *testing.T, runner Runner, now time.Time, provenance Provenance) *Generator {
	t.Helper()
	b := Budgets{
		MaxSteps: 4, MaxElapsed: time.Second, MaxInputTokens: 1_000_000, MaxOutputTokens: 10_000,
		MaxEvidence: 4, MaxGraphHops: 1, MaxGraphNodes: 4, MaxFanout: 1, MaxRepeatedAction: 1,
	}
	g, err := NewGenerator(runner, emptyRetrieveTools{}, b, provenance, &captureRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetClock(func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	return g
}

func graphEvaluationCase(t *testing.T, now time.Time) EvaluationCase {
	t.Helper()
	path := graph.Path{
		Nodes: []graph.Node{
			{ID: "node:violet-gate", Kind: "component", Labels: []string{"Violet Gate"}},
			{ID: "node:celadon-hub", Kind: "system", Labels: []string{"Celadon Hub"}},
			{ID: "node:sable-sink", Kind: "service", Labels: []string{"Sable Sink"}},
		},
		Edges: []graph.Edge{
			{ID: "edge:violet-part-of-celadon", From: "node:violet-gate", To: "node:celadon-hub", Type: "part_of"},
			{ID: "edge:celadon-routes-sable", From: "node:celadon-hub", To: "node:sable-sink", Type: "routes_to"},
		},
	}
	anchor, err := inference.NewGraphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inference.NewSnapshotPin("aster-graph-2026-04-01", now)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin("fixture-public", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack(
		"Which service is reached from Violet Gate through Celadon Hub?",
		[]inference.EvidenceAnchor{anchor}, nil, snapshot, auth, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("node:sable-sink")
	if err != nil {
		t.Fatal(err)
	}
	return EvaluationCase{
		ID:   "q-graph-validation",
		Pack: pack,
		ExpectedClaims: []ExpectedClaim{{
			Subject: "node:violet-gate", Predicate: "reaches", Object: value,
			EvidenceIDs: []shoal.ID{anchor.ID()},
		}},
		GraphPaths: map[shoal.ID]graph.Path{anchor.ID(): path},
	}
}

func evalModelProvenance(t *testing.T) inference.ModelProvenance {
	t.Helper()
	seed := int64(1)
	modelProvenance, err := inference.NewModelProvenance("fake", "deterministic", "v1", nil, &seed)
	if err != nil {
		t.Fatal(err)
	}
	return modelProvenance
}

func evalPromptProvenance(t *testing.T) inference.PromptProvenance {
	t.Helper()
	prompt, err := inference.NewPromptProvenance("agent", "v1", "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func evalResultFor(t *testing.T, pack inference.ContextPack, evidence inference.EvidenceAnchor, generatedAt time.Time) inference.InferenceResult {
	t.Helper()
	value, _ := ontology.NewStringValue("grounded")
	claim, err := inference.NewClaim(
		"entity:fake", "predicate:summary", value, 1, []shoal.ID{evidence.ID()},
		inference.ClaimInferred, evalModelProvenance(t), evalPromptProvenance(t), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewInferenceResult(pack, []inference.Claim{claim}, nil, generatedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type emptyRetrieveTools struct{}

func (emptyRetrieveTools) Retrieve(_ context.Context, call ToolContext, _ RetrieveRequest) (ToolResult, error) {
	return NewToolResult(call.CorrelationID(), ActionRetrieve, nil, call.Snapshot(), call.Authorization())
}
func (emptyRetrieveTools) OpenSection(_ context.Context, call ToolContext, _ OpenSectionRequest) (ToolResult, error) {
	return NewToolResult(call.CorrelationID(), ActionOpenSection, nil, call.Snapshot(), call.Authorization())
}
func (emptyRetrieveTools) Neighbors(_ context.Context, call ToolContext, _ NeighborsRequest) (ToolResult, error) {
	return NewToolResult(call.CorrelationID(), ActionNeighbors, nil, call.Snapshot(), call.Authorization())
}
