// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/inference/harness"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOntologyDiscoveryDemo(t *testing.T) {
	for _, scenario := range []string{"rename", "additive", "abstain", "unchanged", "published"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-dir", dir}
			if scenario == "published" {
				args = append(args, "-publish")
			} else {
				args = append(args, "-scenario", scenario)
			}
			var output bytes.Buffer
			if err := run(context.Background(), args, &output); err != nil {
				t.Fatalf("%s: %v\n%s", scenario, err, output.String())
			}
			data, err := os.ReadFile(filepath.Join(dir, "discovery-reports.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var audits []webapi.OntologyDiscoveryAudit
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
				var audit webapi.OntologyDiscoveryAudit
				if err := json.Unmarshal(line, &audit); err != nil {
					t.Fatal(err)
				}
				audits = append(audits, audit)
			}
			first := audits[0]
			if first.TranscriptID == "" || first.UsageSnapshot == "" || len(first.Trace.Iterations) < 2 ||
				first.Trace.Iterations[0].Decision != harness.ActionNeighbors {
				t.Fatalf("walk/provenance not recorded: %+v", first)
			}
			if scenario == "abstain" || scenario == "unchanged" {
				if first.Status != "abstained" || first.Reason == "" || first.ProposalID != "" {
					t.Fatalf("unexplained or non-abstaining result: %+v", first)
				}
				return
			}
			if first.Status != "draft" || first.Evaluation.Status != "no_improvement" ||
				first.CandidateVersionID == "" || len(first.EvidenceIDs) < 2 {
				t.Fatalf("candidate was not a bounded evaluated draft: %+v", first)
			}
			if scenario == "published" {
				if !strings.Contains(output.String(), "restart=preserved state=published") {
					t.Fatal(output.String())
				}
			} else if len(audits) != 2 || audits[1].Status != "duplicate" ||
				audits[1].ProposalID != first.ProposalID ||
				!strings.Contains(output.String(), "restart=preserved state=draft selected_version_resolved=0/") {
				t.Fatalf("retry, isolation, or restart failed: %s", output.String())
			}
		})
	}
}

type memoryDiscoveryRecorder struct {
	audits []webapi.OntologyDiscoveryAudit
	err    error
}

func (r *memoryDiscoveryRecorder) RecordOntologyDiscovery(_ context.Context, audit webapi.OntologyDiscoveryAudit) error {
	r.audits = append(r.audits, audit)
	return r.err
}

type discoveryGeneratorFunc func(context.Context, model.GenerateRequest) (model.GenerateResult, error)

func (f discoveryGeneratorFunc) Generate(ctx context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	return f(ctx, request)
}

type discoveryEvaluatorFunc func(context.Context, ontology.OntologyVersion, ontology.OntologyVersion) (webapi.OntologyDiscoveryEvaluation, error)

func (f discoveryEvaluatorFunc) EvaluateOntology(ctx context.Context, base, proposed ontology.OntologyVersion) (webapi.OntologyDiscoveryEvaluation, error) {
	return f(ctx, base, proposed)
}

func discoveryFixture(t *testing.T) (*demoEnvironment, webapi.OntologyDiscoveryRequest, webapi.OntologyDiscoveryConfig, *memoryDiscoveryRecorder) {
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
	request, err := demo.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := inference.NewModelProvenance("scripted", "ontology-conformance-v1", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &memoryDiscoveryRecorder{}
	return demo, request, webapi.OntologyDiscoveryConfig{
		Generator: scriptedModel{scenario: "rename"}, Model: provenance, Recorder: recorder,
	}, recorder
}

func TestOntologyDiscoveryRejectsCandidates(t *testing.T) {
	for _, scenario := range []string{"unknown_field", "duplicate_field", "unknown_definition", "unsupported_evolution", "missing_morphism_evidence", "uncited_morphism_evidence", "ambiguous", "bad_predicate", "oversized", "stale_base", "evaluation_overlap", "budget", "malformed_action"} {
		t.Run(scenario, func(t *testing.T) {
			demo, request, config, recorder := discoveryFixture(t)
			config.Generator = discoveryGeneratorFunc(func(ctx context.Context, input model.GenerateRequest) (model.GenerateResult, error) {
				response, err := (scriptedModel{scenario: "rename"}).Generate(ctx, input)
				if err != nil {
					return response, err
				}
				var action map[string]json.RawMessage
				if err := json.Unmarshal([]byte(response.Text), &action); err != nil {
					return response, err
				}
				if _, stop := action["claims"]; !stop {
					return response, nil
				}
				if scenario == "malformed_action" {
					response.Text = `{"action":`
					return response, nil
				}
				var claims []map[string]any
				if err := json.Unmarshal(action["claims"], &claims); err != nil {
					return response, err
				}
				object := claims[0]["object"].(map[string]any)
				text := object["value"].(string)
				var candidate map[string]any
				if err := json.Unmarshal([]byte(text), &candidate); err != nil {
					return response, err
				}
				switch scenario {
				case "unknown_field":
					candidate["publish"] = true
				case "duplicate_field":
					text = `{"rationale":"one","rationale":"two"}`
				case "unknown_definition":
					candidate["proposed_version"].(map[string]any)["concepts"] = []any{}
				case "unsupported_evolution":
					candidate["morphisms"] = []any{}
				case "missing_morphism_evidence":
					candidate["morphisms"].([]any)[0].(map[string]any)["evidence_ids"] = []any{}
				case "uncited_morphism_evidence":
					var prompt struct {
						Evidence []struct{ ID string } `json:"evidence"`
					}
					if err := json.Unmarshal([]byte(input.Prompt), &prompt); err != nil {
						return response, err
					}
					if len(prompt.Evidence) < 2 {
						t.Fatal("walk did not expose multiple anchors")
					}
					candidate["morphisms"].([]any)[0].(map[string]any)["evidence_ids"] = []string{prompt.Evidence[1].ID}
				case "ambiguous":
					claims = append(claims, claims[0])
				case "bad_predicate":
					claims[0]["predicate"] = wireID("not-discovery")
				case "oversized":
					text = strings.Repeat("x", webapi.MaxDiscoveryCandidateBytes+1)
				case "stale_base":
					base := demo.base
					other, err := ontology.NewOntologyVersion(base.Schema(), "changed-during-walk", time.Now().UTC(),
						base.Concepts(), base.Relationships(), base.Properties(), nil)
					if err != nil {
						return response, err
					}
					if err := demo.service.SetOntologyVersion(other); err != nil {
						return response, err
					}
				}
				if scenario != "duplicate_field" && scenario != "oversized" {
					encoded, err := json.Marshal(candidate)
					if err != nil {
						return response, err
					}
					text = string(encoded)
				}
				object["value"] = text
				action["claims"], err = json.Marshal(claims)
				if err != nil {
					return response, err
				}
				encoded, err := json.Marshal(action)
				response.Text = string(encoded)
				return response, err
			})
			if scenario == "evaluation_overlap" {
				config.Evaluator = discoveryEvaluatorFunc(func(ctx context.Context, base, candidate ontology.OntologyVersion) (webapi.OntologyDiscoveryEvaluation, error) {
					evaluation, err := (compatibilityEvaluator{}).EvaluateOntology(ctx, base, candidate)
					evaluation.EvidenceIDs = []string{base64.RawURLEncoding.EncodeToString([]byte(request.Context.Evidence()[0].ID()))}
					return evaluation, err
				})
			}
			if scenario == "budget" {
				config.Budgets = harness.Budgets{
					MaxSteps: 1, MaxElapsed: time.Minute, MaxInputTokens: 65536, MaxOutputTokens: 16384,
					MaxEvidence: 64, MaxGraphHops: 4, MaxGraphNodes: 128, MaxFanout: 16, MaxRepeatedAction: 1,
				}
			}
			if result, err := demo.service.DiscoverOntology(demo.ctx, request, config); err == nil || result.Proposal.ID() != "" {
				t.Fatalf("invalid candidate accepted: result=%+v err=%v", result, err)
			}
			proposals, err := demo.client.OntologyProposals(demo.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(proposals) != 0 {
				t.Fatal("failed discovery persisted an ontology proposal")
			}
			if len(recorder.audits) != 1 || recorder.audits[0].Status != "failed" {
				t.Fatalf("failure was not recorded: %+v", recorder.audits)
			}
			audit := recorder.audits[0]
			if len(audit.EvidenceIDs) != len(audit.Evidence) || len(audit.EvidenceIDs) <= len(request.Context.Evidence()) {
				t.Fatalf("failed walk lost its full expanded exposure: %+v", audit)
			}
			for _, anchor := range request.Context.Evidence() {
				found := false
				for _, evidence := range audit.Evidence {
					found = found || evidence.AnchorID == anchor.ID()
				}
				if !found {
					t.Fatalf("failed walk lost initial exposure %q", anchor.ID())
				}
			}
		})
	}
}

func TestOntologyDiscoveryRejectsUnauthorizedInputs(t *testing.T) {
	for _, scenario := range []string{"foreign_principal", "forged_quote", "duplicate_usage", "unknown_coverage", "uncovered_usage", "no_recorder", "output_restriction"} {
		t.Run(scenario, func(t *testing.T) {
			demo, request, config, _ := discoveryFixture(t)
			calls := 0
			config.Generator = discoveryGeneratorFunc(func(ctx context.Context, input model.GenerateRequest) (model.GenerateResult, error) {
				calls++
				return (scriptedModel{scenario: "rename"}).Generate(ctx, input)
			})
			ctx := demo.ctx
			switch scenario {
			case "foreign_principal":
				decision, err := auth.NewDecision(auth.DecisionConfig{
					Subject: "outsider", Actor: "outsider", AuthorizationDomain: []byte("ontology-demo"),
					AllowedOperations:  []auth.Operation{auth.OperationIngest, auth.OperationRead, auth.OperationRetrieve, auth.OperationValidate},
					PermittedSourceIDs: [][]byte{[]byte("other")}, PermittedPolicyIDs: [][]byte{[]byte("other")},
					PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "foreign-request",
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, err = demo.authority.Binder().Bind(context.Background(), decision)
				if err != nil {
					t.Fatal(err)
				}
			case "forged_quote":
				var anchor inference.EvidenceAnchor
				for _, candidate := range request.Context.Evidence() {
					if candidate.Kind() == inference.AnchorDocument {
						anchor = candidate
						break
					}
				}
				citation, quote, ok := anchor.Document()
				if !ok || len(quote) == 0 {
					t.Fatal("fixture has no original document quote")
				}
				forged, err := inference.NewDocumentAnchor(citation, "X"+quote[1:])
				if err != nil {
					t.Fatal(err)
				}
				identity, _ := request.Context.Ontology()
				request.Context, err = inference.NewContextPack(request.Context.Query(), []inference.EvidenceAnchor{forged},
					&identity, request.Context.Snapshot(), request.Context.Authorization(), request.Context.Metadata())
				if err != nil {
					t.Fatal(err)
				}
			case "duplicate_usage":
				request.UsageSessionIDs = append(request.UsageSessionIDs, request.UsageSessionIDs[0])
			case "unknown_coverage":
				request.PopulationSize, request.CoverageGaps = 0, nil
			case "uncovered_usage":
				session, err := demo.client.Interaction(demo.ctx, request.UsageSessionIDs[0])
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				session.ID, session.RecordedAt = interaction.SessionID("legacy-extra-exposure", now), now
				session.SeedEvidence = nil
				if err := demo.client.RecordInteraction(demo.ctx, session); err != nil {
					t.Fatal(err)
				}
				request.UsageSessionIDs = []shoal.ID{session.ID}
			case "no_recorder":
				config.Recorder = nil
			case "output_restriction":
				var err error
				ctx, err = interaction.WithRequiredVisibility(ctx, []string{"extra-output-policy"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := demo.service.DiscoverOntology(ctx, request, config); err == nil {
				t.Fatal("invalid input accepted")
			}
			if calls != 0 {
				t.Fatal("unauthorized/unverifiable inputs reached the model")
			}
		})
	}
}

func TestOntologyDiscoveryRecorderFailureIsNotSuccess(t *testing.T) {
	demo, request, config, recorder := discoveryFixture(t)
	recorder.err = errors.New("audit storage unavailable")
	result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if !errors.Is(err, recorder.err) || result.Proposal.ID() == "" {
		t.Fatalf("audit failure hidden or draft unexpectedly absent: %+v %v", result, err)
	}
	proposals, err := demo.client.OntologyProposals(demo.ctx)
	if err != nil || len(proposals) != 1 || proposals[0].State() != ontology.ProposalDraft {
		t.Fatalf("committed draft lost after audit failure: %+v %v", proposals, err)
	}
}

func TestOntologyDiscoveryRejectsUnprojectableExposure(t *testing.T) {
	demo, request, config, recorder := discoveryFixture(t)
	anchors := request.Context.Evidence()
	seen := make(map[shoal.ID]bool)
	for _, anchor := range anchors {
		seen[anchor.ID()] = true
	}
	added := 0
	for _, anchor := range request.Context.Evidence() {
		citation, quote, ok := anchor.Document()
		if !ok {
			continue
		}
		for start := 0; start < len(quote) && added < 33; start++ {
			for end := start + 1; end <= len(quote) && added < 33; end++ {
				part := citation
				part.Range.Start.Offset = citation.Range.Start.Offset + int64(start)
				part.Range.End.Offset = citation.Range.Start.Offset + int64(end)
				extra, err := inference.NewDocumentAnchor(part, quote[start:end])
				if err != nil {
					t.Fatal(err)
				}
				if seen[extra.ID()] {
					continue
				}
				seen[extra.ID()] = true
				anchors = append(anchors, extra)
				added++
			}
		}
		if added == 33 {
			break
		}
	}
	if added != 33 {
		t.Fatal("fixture did not provide sufficient distinct citation ranges")
	}
	identity, _ := request.Context.Ontology()
	var err error
	request.Context, err = inference.NewContextPack(request.Context.Query(), anchors, &identity,
		request.Context.Snapshot(), request.Context.Authorization(), request.Context.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if err == nil || result.Proposal.ID() != "" || len(recorder.audits) != 1 ||
		recorder.audits[0].Reason != "candidate exceeds governance projection limits" {
		t.Fatalf("unprojectable exposure was not rejected at the governance bound: %+v, %v", result, err)
	}
	proposals, err := demo.client.OntologyProposals(demo.ctx)
	if err != nil || len(proposals) != 0 {
		t.Fatalf("unprojectable proposal was persisted: %d, %v", len(proposals), err)
	}
}

func TestOntologyDiscoveryConcurrentServicesDeduplicate(t *testing.T) {
	demo, request, config, _ := discoveryFixture(t)
	other, err := webapi.NewEmbeddedService(demo.client)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetOntologyVersion(demo.base); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result webapi.OntologyDiscoveryResult
		err    error
	}
	ready, release := make(chan struct{}, 2), make(chan struct{})
	results := make(chan outcome, 2)
	for index, service := range []*webapi.EmbeddedService{demo.service, other} {
		go func() {
			local := config
			local.Recorder = &memoryDiscoveryRecorder{}
			local.Generator = discoveryGeneratorFunc(func(ctx context.Context, input model.GenerateRequest) (model.GenerateResult, error) {
				response, err := (scriptedModel{scenario: "rename"}).Generate(ctx, input)
				if err != nil || !strings.Contains(response.Text, `"claims"`) {
					return response, err
				}
				var action map[string]any
				if err := json.Unmarshal([]byte(response.Text), &action); err != nil {
					return response, err
				}
				object := action["claims"].([]any)[0].(map[string]any)["object"].(map[string]any)
				var candidate map[string]any
				if err := json.Unmarshal([]byte(object["value"].(string)), &candidate); err != nil {
					return response, err
				}
				candidate["rationale"] = []string{"First independent rationale", "Second independent rationale"}[index]
				encoded, err := json.Marshal(candidate)
				if err != nil {
					return response, err
				}
				object["value"] = string(encoded)
				encoded, err = json.Marshal(action)
				response.Text = string(encoded)
				ready <- struct{}{}
				<-release
				return response, err
			})
			result, err := service.DiscoverOntology(demo.ctx, request, local)
			results <- outcome{result, err}
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case early := <-results:
			close(release)
			t.Fatalf("worker failed before producing a candidate: %v", early.err)
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatal("concurrent discovery did not reach candidate generation")
		}
	}
	close(release)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.result.Proposal.ID() != second.result.Proposal.ID() {
		t.Fatalf("concurrent candidates were not coalesced: %+v / %+v", first, second)
	}
	statuses := map[string]int{first.result.Audit.Status: 1}
	statuses[second.result.Audit.Status]++
	if statuses["draft"] != 1 || statuses["duplicate"] != 1 {
		t.Fatalf("concurrent discovery statuses = %v", statuses)
	}
	proposals, err := demo.client.OntologyProposals(demo.ctx)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("concurrent semantic duplicates persisted: %d, %v", len(proposals), err)
	}
}
