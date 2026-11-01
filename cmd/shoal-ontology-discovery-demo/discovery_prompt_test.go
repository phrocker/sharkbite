// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/inference/harness"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// discoveryScript is a self-contained scripted provider for the prompt-layout,
// delta, and repair tests. It walks once, then stops with a rename of
// provides_tool to offers_tool, as either a delta ("changes") or a complete
// proposed_version. It reads the base ontology from wherever the prompt
// carries it, so the same test runs against the previous prompt layout.
type discoveryScript struct {
	mu        sync.Mutex
	form      string // "changes" or "full"
	mutate    func(candidate map[string]any)
	malformed []string // returned, in order, in place of the stop answer
	prompts   []string
}

func (s *discoveryScript) Generate(_ context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, request.Prompt)
	var prompt struct {
		Query    string `json:"query"`
		Evidence []struct {
			ID       string `json:"id"`
			Citation *struct {
				DocumentID string `json:"document_id"`
			} `json:"citation"`
		} `json:"evidence"`
		Transcript  []json.RawMessage `json:"transcript"`
		HostContext *struct {
			Base webapi.OntologyProposalVersionDraft `json:"base"`
		} `json:"host_context"`
	}
	if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
		return model.GenerateResult{}, err
	}
	var text string
	if len(prompt.Transcript) == 0 {
		for _, anchor := range prompt.Evidence {
			if anchor.Citation != nil {
				encoded, err := json.Marshal(map[string]any{
					"action": "neighbors", "correlation_id": wireID("prompt-test-walk"),
					"node_id": anchor.Citation.DocumentID, "hops": 2, "fanout": 16,
				})
				if err != nil {
					return model.GenerateResult{}, err
				}
				text = string(encoded)
				break
			}
		}
		if text == "" {
			return model.GenerateResult{}, fmt.Errorf("fixture requires an original document")
		}
	} else if len(s.malformed) > 0 {
		text, s.malformed = s.malformed[0], s.malformed[1:]
	} else {
		var query struct {
			Base      *webapi.OntologyProposalVersionDraft `json:"base"`
			BaseID    string                               `json:"base_version_id"`
			Predicate string                               `json:"predicate"`
		}
		if err := json.Unmarshal([]byte(prompt.Query), &query); err != nil {
			return model.GenerateResult{}, err
		}
		base := query.Base
		if prompt.HostContext != nil {
			base = &prompt.HostContext.Base
		}
		if base == nil {
			return model.GenerateResult{}, fmt.Errorf("prompt carries no base ontology")
		}
		// The full form lists definitions in the order the delta produces
		// (base order, renamed definition last), so both describe the same
		// version definition by definition.
		var renamed webapi.OntologyProposalRelationshipDraft
		var kept []webapi.OntologyProposalRelationshipDraft
		for _, relationship := range base.Relationships {
			if relationship.Key == "provides_tool" {
				renamed = relationship
				renamed.Key, renamed.Name = "offers_tool", "Offers tool"
				continue
			}
			kept = append(kept, relationship)
		}
		base.Relationships = append(kept, renamed)
		candidate := map[string]any{
			"rationale": "Scripted rename for prompt-layout tests",
			"morphisms": []any{map[string]any{
				"kind":         "rename",
				"sources":      []any{map[string]string{"namespace": "relationship", "key": "provides_tool"}},
				"targets":      []any{map[string]string{"namespace": "relationship", "key": "offers_tool"}},
				"evidence_ids": []string{prompt.Evidence[0].ID}, "rationale": "Scripted rename",
			}},
			"counterevidence": []string{"Fixture only"},
			"open_questions":  []string{"Fixture only"},
		}
		if s.form == "full" {
			candidate["proposed_version"] = base
		} else {
			candidate["changes"] = map[string]any{
				"concepts": []any{}, "properties": []any{},
				"relationships": []any{renamed},
				"remove":        []any{map[string]string{"namespace": "relationship", "key": "provides_tool"}},
			}
		}
		if s.mutate != nil {
			s.mutate(candidate)
		}
		value, err := json.Marshal(candidate)
		if err != nil {
			return model.GenerateResult{}, err
		}
		encoded, err := json.Marshal(map[string]any{
			"action": "stop", "correlation_id": wireID("prompt-test-stop"),
			"claims": []any{map[string]any{
				"subject": query.BaseID, "predicate": query.Predicate, "confidence": 0.7,
				"object":       map[string]string{"type": "string", "value": string(value)},
				"evidence_ids": []string{prompt.Evidence[0].ID},
			}},
		})
		if err != nil {
			return model.GenerateResult{}, err
		}
		text = string(encoded)
	}
	return model.GenerateResult{
		Text: text, Provenance: model.Provenance{Provider: "scripted", Model: "ontology-conformance-v1"},
		Usage: model.Usage{InputTokens: (len(request.Prompt) + 3) / 4, OutputTokens: (len(text) + 3) / 4},
	}, nil
}

func (s *discoveryScript) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

// largeDemoOntology is the demo ontology plus extra concepts with long
// descriptions, sized to encode to about the given number of bytes.
func largeDemoOntology(t *testing.T, base ontology.OntologyVersion, approxBytes int) ontology.OntologyVersion {
	t.Helper()
	concepts := base.Concepts()
	description := strings.TrimSpace(strings.Repeat("A budget function concept with a long, explicit scope note. ", 8))
	for i := 0; len(concepts)*len(description) < approxBytes; i++ {
		concept, err := ontology.NewConceptDefinition(fmt.Sprintf("function_%03d", i), fmt.Sprintf("Function %03d", i),
			description, []shoal.ID{base.Properties()[0].ID()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		concepts = append(concepts, concept)
	}
	large, err := ontology.NewOntologyVersion(base.Schema(), "v1-large", base.CreatedAt(),
		concepts, base.Relationships(), base.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return large
}

func encodedOntologyBytes(t *testing.T, version ontology.OntologyVersion) int {
	t.Helper()
	draft := webapi.OntologyProposalVersionDraft{Version: version.Version()}
	for _, concept := range version.Concepts() {
		draft.Concepts = append(draft.Concepts, webapi.OntologyProposalConceptDraft{
			Key: concept.Key(), Name: concept.Name(), Description: concept.Description(), Properties: []string{"name"},
		})
	}
	encoded, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded)
}

// withOntology replaces the fixture's active ontology before its usage and
// context pack are built, so the pack pins the larger version.
func withOntology(t *testing.T, version ontology.OntologyVersion) (*demoEnvironment, webapi.OntologyDiscoveryRequest, webapi.OntologyDiscoveryConfig, *memoryDiscoveryRecorder) {
	t.Helper()
	demo, err := newDemo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := demo.close(); err != nil {
			t.Error(err)
		}
	})
	demo.base = version
	if err := demo.service.SetOntologyVersion(version); err != nil {
		t.Fatal(err)
	}
	request, err := demo.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := inference.NewModelProvenance("scripted", "ontology-conformance-v1", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &memoryDiscoveryRecorder{}
	return demo, request, webapi.OntologyDiscoveryConfig{Model: provenance, Recorder: recorder, ProtocolVersion: "v2"}, recorder
}

func TestOntologyDiscoveryOntologyLargerThanQueryBoundReachesModel(t *testing.T) {
	demo, request, config, recorder := withOntology(t, largeDemoOntology(t, mustDemoOntology(t), 24<<10))
	if size := encodedOntologyBytes(t, demo.base); size <= 16<<10 || size >= 32<<10 {
		t.Fatalf("fixture ontology encodes to %d bytes; want between 16 KiB and the 32 KiB default bound", size)
	}
	script := &discoveryScript{form: "full"}
	config.Generator = script
	result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if err != nil {
		t.Fatalf("a %d-byte ontology within the input budget was refused: %v", encodedOntologyBytes(t, demo.base), err)
	}
	if script.calls() == 0 || result.Audit.Status != "draft" || result.Proposal.ID() == "" {
		t.Fatalf("large ontology did not reach the model and persist a draft: calls=%d audit=%+v", script.calls(), result.Audit)
	}
	if !strings.Contains(script.prompts[0], `"host_context":{"base":`) ||
		strings.Contains(script.prompts[0], `\"base\":{`) {
		t.Fatal("base ontology is not carried in its own host_context section")
	}
	if len(recorder.audits) != 1 {
		t.Fatalf("audits = %d", len(recorder.audits))
	}
}

func TestOntologyDiscoveryOntologyOverBoundFailsByName(t *testing.T) {
	demo, request, config, recorder := withOntology(t, largeDemoOntology(t, mustDemoOntology(t), 40<<10))
	script := &discoveryScript{form: "changes"}
	config.Generator = script
	_, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if err == nil || !strings.Contains(err.Error(), "base ontology") ||
		!strings.Contains(err.Error(), "exceeds the discovery ontology bound of 32768 bytes") {
		t.Fatalf("oversized ontology did not fail naming the ontology and its bound: %v", err)
	}
	if script.calls() != 0 || len(recorder.audits) != 0 {
		t.Fatal("oversized ontology reached the model or started an attempt")
	}
	// The same ontology fits once the run's input budget is raised.
	config.Budgets = harness.Budgets{
		MaxSteps: 8, MaxElapsed: time.Minute, MaxInputTokens: 262144, MaxOutputTokens: 16384,
		MaxEvidence: 64, MaxGraphHops: 4, MaxGraphNodes: 128, MaxFanout: 16, MaxRepeatedAction: 1,
	}
	if result, err := demo.service.DiscoverOntology(demo.ctx, request, config); err != nil || result.Audit.Status != "draft" {
		t.Fatalf("ontology within a raised input budget was refused: %+v %v", result.Audit, err)
	}
}

func TestOntologyDiscoveryDeltaMatchesFullAnswer(t *testing.T) {
	for _, order := range [][]string{{"changes", "full"}, {"full", "changes"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			demo, request, config, _ := discoveryFixture(t)
			config.Generator = &discoveryScript{form: order[0]}
			first, err := demo.service.DiscoverOntology(demo.ctx, request, config)
			if err != nil || first.Audit.Status != "draft" {
				t.Fatalf("%s answer did not persist a draft: %+v %v", order[0], first.Audit, err)
			}
			config.Generator = &discoveryScript{form: order[1]}
			second, err := demo.service.DiscoverOntology(demo.ctx, request, config)
			if err != nil || second.Audit.Status != "duplicate" || second.Proposal.ID() != first.Proposal.ID() ||
				second.Audit.CandidateVersionID != first.Audit.CandidateVersionID {
				t.Fatalf("%s answer is not the same candidate as the %s answer: %+v %v", order[1], order[0], second.Audit, err)
			}
			proposals, err := demo.client.OntologyProposals(demo.ctx)
			if err != nil || len(proposals) != 1 {
				t.Fatalf("persisted proposals = %d, %v", len(proposals), err)
			}
			stored := proposals[0]
			keys := map[string]bool{}
			for _, relationship := range stored.ProposedVersion().Relationships() {
				keys[relationship.Key()] = true
			}
			if !keys["offers_tool"] || keys["provides_tool"] || !keys["provides_capability"] ||
				len(stored.ProposedVersion().Concepts()) != len(demo.base.Concepts()) {
				t.Fatalf("persisted draft is not base + rename: %v", keys)
			}
		})
	}
}

type discoveryV3Script struct{ legacy *discoveryScript }

func (s *discoveryV3Script) Generate(ctx context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	result, err := s.legacy.Generate(ctx, request)
	if err != nil {
		return result, err
	}
	var action struct {
		Action string `json:"action"`
		Claims []struct {
			Object struct {
				Value string `json:"value"`
			} `json:"object"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"claims"`
	}
	if err := json.Unmarshal([]byte(result.Text), &action); err != nil || action.Action != "stop" {
		return result, err
	}
	if len(action.Claims) != 1 {
		return result, fmt.Errorf("scripted stop has %d claims", len(action.Claims))
	}
	var candidate map[string]any
	if err := json.Unmarshal([]byte(action.Claims[0].Object.Value), &candidate); err != nil {
		return result, err
	}
	changes, ok := candidate["changes"].(map[string]any)
	if !ok {
		return result, fmt.Errorf("scripted stop has no changes")
	}
	changes["morphisms"] = candidate["morphisms"]
	encoded, err := json.Marshal(map[string]any{"action": "stop", "proposal": map[string]any{
		"rationale": candidate["rationale"], "changes": changes,
		"counterevidence": candidate["counterevidence"], "open_questions": candidate["open_questions"],
		"evidence_ids": action.Claims[0].EvidenceIDs,
	}})
	if err != nil {
		return result, err
	}
	result.Text = string(encoded)
	return result, nil
}

func TestOntologyDiscoveryV3AndV2PersistSameDraft(t *testing.T) {
	demo, request, config, _ := discoveryFixture(t)
	config.Generator = &discoveryScript{form: "changes"}
	first, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if err != nil || first.Audit.Status != "draft" {
		t.Fatalf("v2 draft: %+v %v", first.Audit, err)
	}
	config.ProtocolVersion = "v3"
	config.Generator = &discoveryV3Script{legacy: &discoveryScript{form: "changes"}}
	second, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if err != nil || second.Audit.Status != "duplicate" || second.Proposal.ID() != first.Proposal.ID() || second.Audit.CandidateVersionID != first.Audit.CandidateVersionID {
		t.Fatalf("v3 differs from v2 persisted draft: %+v %v", second.Audit, err)
	}
}

func TestOntologyDiscoveryDeltaRejectedLikeFull(t *testing.T) {
	for name, mutate := range map[string]func(form string) func(map[string]any){
		// A rename with no morphism: the base relationship disappears unexplained.
		"unsupported_evolution": func(string) func(map[string]any) {
			return func(candidate map[string]any) { candidate["morphisms"] = []any{} }
		},
		// Every concept removed: the relationships reference undefined keys.
		"unknown_definition": func(form string) func(map[string]any) {
			return func(candidate map[string]any) {
				if form == "full" {
					candidate["proposed_version"].(*webapi.OntologyProposalVersionDraft).Concepts = nil
					return
				}
				candidate["changes"].(map[string]any)["remove"] = []any{
					map[string]string{"namespace": "relationship", "key": "provides_tool"},
					map[string]string{"namespace": "concept", "key": "skill"},
					map[string]string{"namespace": "concept", "key": "tool"},
					map[string]string{"namespace": "concept", "key": "capability"},
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var reasons, errs []string
			for _, form := range []string{"full", "changes"} {
				demo, request, config, recorder := discoveryFixture(t)
				config.Generator = &discoveryScript{form: form, mutate: mutate(form)}
				result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
				if err == nil || result.Proposal.ID() != "" || len(recorder.audits) != 1 || recorder.audits[0].Status != "failed" {
					t.Fatalf("%s form accepted an invalid candidate: %+v %v", form, result.Audit, err)
				}
				if proposals, err := demo.client.OntologyProposals(demo.ctx); err != nil || len(proposals) != 0 {
					t.Fatalf("%s form persisted a proposal: %d %v", form, len(proposals), err)
				}
				reasons = append(reasons, recorder.audits[0].Reason)
				errs = append(errs, err.Error())
			}
			if reasons[0] != reasons[1] || errs[0] != errs[1] {
				t.Fatalf("delta was not rejected identically to the full form:\nfull:  %s: %s\ndelta: %s: %s",
					reasons[0], errs[0], reasons[1], errs[1])
			}
		})
	}
}

func TestOntologyDiscoveryDeltaContract(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"both_forms":   func(c map[string]any) { c["proposed_version"] = map[string]any{"version": "x"} },
		"neither_form": func(c map[string]any) { delete(c, "changes") },
		"remove_unknown": func(c map[string]any) {
			c["changes"].(map[string]any)["remove"] = []any{map[string]string{"namespace": "concept", "key": "missing"}}
		},
		"bad_namespace": func(c map[string]any) {
			c["changes"].(map[string]any)["remove"] = []any{map[string]string{"namespace": "schema", "key": "skill"}}
		},
		"change_and_remove": func(c map[string]any) {
			changes := c["changes"].(map[string]any)
			changes["remove"] = append(changes["remove"].([]any), map[string]string{"namespace": "relationship", "key": "offers_tool"})
		},
		"listed_twice": func(c map[string]any) {
			changes := c["changes"].(map[string]any)
			changes["relationships"] = append(changes["relationships"].([]any), changes["relationships"].([]any)[0])
		},
		"unknown_change_field": func(c map[string]any) { c["changes"].(map[string]any)["publish"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			demo, request, config, recorder := discoveryFixture(t)
			config.Generator = &discoveryScript{form: "changes", mutate: mutate}
			result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
			if err == nil || result.Proposal.ID() != "" || len(recorder.audits) != 1 || recorder.audits[0].Status != "failed" {
				t.Fatalf("invalid delta accepted: %+v %v", result.Audit, err)
			}
			if proposals, err := demo.client.OntologyProposals(demo.ctx); err != nil || len(proposals) != 0 {
				t.Fatalf("invalid delta persisted a proposal: %d %v", len(proposals), err)
			}
		})
	}
}

func TestOntologyDiscoveryRepairTurn(t *testing.T) {
	// The 8B failure mode: the prompt envelope echoed back instead of an action.
	echoed := `{"protocol":"shoal-harness-action-json/v1","tools":["retrieve"]}`
	t.Run("invalid_then_valid", func(t *testing.T) {
		demo, request, config, recorder := discoveryFixture(t)
		script := &discoveryScript{form: "changes", malformed: []string{echoed}}
		config.Generator = script
		result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
		if err != nil || result.Audit.Status != "draft" || result.Proposal.ID() == "" {
			t.Fatalf("repaired run did not persist a draft: %+v %v", result.Audit, err)
		}
		if script.calls() != 3 {
			t.Fatalf("model calls = %d, want walk + malformed + repaired stop", script.calls())
		}
		trace := recorder.audits[0].Trace
		if len(trace.Iterations) != 3 || trace.Usage.ModelCalls != 3 ||
			trace.Iterations[1].Failure == "" || trace.Iterations[1].Usage.OutputTokens == 0 ||
			trace.Iterations[2].Decision != harness.ActionStop || len(trace.Failures) != 1 ||
			!strings.Contains(trace.Failures[0].Error, `unknown field "protocol"`) {
			t.Fatalf("both attempts were not recorded: %+v", trace)
		}
		repairPrompt := script.prompts[2]
		if !strings.Contains(repairPrompt, `"repair":{"attempt":1`) ||
			!strings.Contains(repairPrompt, `unknown field \"protocol\"`) {
			t.Fatalf("repair turn did not show the parse error: %.300s", repairPrompt[len(repairPrompt)-600:])
		}
	})
	t.Run("invalid_twice", func(t *testing.T) {
		demo, request, config, recorder := discoveryFixture(t)
		script := &discoveryScript{form: "changes", malformed: []string{echoed, `{"action":`}}
		config.Generator = script
		result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
		if err == nil || result.Proposal.ID() != "" || len(recorder.audits) != 1 ||
			recorder.audits[0].Status != "failed" || !strings.Contains(err.Error(), "not a valid action JSON object") {
			t.Fatalf("second malformed output did not fail the run with an explanation: %+v %v", result.Audit, err)
		}
		trace := recorder.audits[0].Trace
		if script.calls() != 3 || len(trace.Iterations) != 3 || len(trace.Failures) != 2 ||
			trace.Iterations[1].Failure == "" || trace.Iterations[2].Failure == "" {
			t.Fatalf("only one repair turn is allowed and both failures are recorded: calls=%d %+v", script.calls(), trace)
		}
		if proposals, err := demo.client.OntologyProposals(demo.ctx); err != nil || len(proposals) != 0 {
			t.Fatalf("failed run persisted a proposal: %d %v", len(proposals), err)
		}
	})
	t.Run("step_budget", func(t *testing.T) {
		demo, request, config, recorder := discoveryFixture(t)
		script := &discoveryScript{form: "changes", malformed: []string{echoed}}
		config.Generator = script
		config.Budgets = harness.Budgets{
			MaxSteps: 2, MaxElapsed: time.Minute, MaxInputTokens: 65536, MaxOutputTokens: 16384,
			MaxEvidence: 64, MaxGraphHops: 4, MaxGraphNodes: 128, MaxFanout: 16, MaxRepeatedAction: 1,
		}
		if _, err := demo.service.DiscoverOntology(demo.ctx, request, config); err == nil {
			t.Fatal("a repair turn ran beyond MaxSteps")
		}
		if script.calls() != 2 || recorder.audits[0].Trace.Usage.ModelCalls != 2 {
			t.Fatalf("repair exceeded the step budget: calls=%d", script.calls())
		}
	})
}

func mustDemoOntology(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	base, err := demoOntology()
	if err != nil {
		t.Fatal(err)
	}
	return base
}
