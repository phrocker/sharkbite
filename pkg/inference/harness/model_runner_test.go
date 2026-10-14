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
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	lowmodel "github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestModelRunnerDrivesBoundedLoopAndTrace(t *testing.T) {
	pack, _, additions := fixture(t)
	stopJSON := `{"action":"stop","correlation_id":"` + wireID("stop") + `","claims":[{"subject":"` + wireID("entity:fake") + `","predicate":"` + wireID("predicate:summary") + `","object":{"type":"string","value":"grounded"},"confidence":1,"evidence_ids":["` + wireID(additions[0].ID()) + `"]}]}`
	runner, err := NewModelRunner(&scriptedTextGenerator{responses: []string{
		`{"action":"retrieve","correlation_id":"` + wireID("retrieve") + `","query":"entity","limit":1}`,
		stopJSON,
	}}, ModelRunnerConfig{Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
		"retrieve": {additions[0]},
	}}
	custom := budgets()
	custom.MaxInputTokens = 10000
	record, err := newGeneratorWithBudgets(t, runner, host, custom).Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if record.Trace.StopReason != StopReasonStop || len(record.Trace.Iterations) != 2 {
		t.Fatalf("unexpected trace: %#v", record.Trace)
	}
	if record.Trace.Usage.ModelCalls != 2 || record.Trace.Usage.Evidence != 2 {
		t.Fatalf("budget usage not recorded: %#v", record.Trace.Usage)
	}
	if len(record.Result.Claims()) != 1 {
		t.Fatal("grounded claim was not returned")
	}
}

func TestModelPromptTemplateHashIsStable(t *testing.T) {
	const want = "sha256:21d657e9286481d67592c7c2a44ee0db23a658f82a6f47e25576e9b56c0a32bc"
	if got := ModelPromptTemplateHash(); got != want {
		t.Fatalf("template hash = %s, want %s", got, want)
	}
}

func TestModelRunnerRejectsUngroundedStopWithTrace(t *testing.T) {
	pack, _, _ := fixture(t)
	runner, err := NewModelRunner(&scriptedTextGenerator{responses: []string{
		`{"action":"stop","correlation_id":"` + wireID("stop") + `","claims":[{"subject":"` + wireID("entity:fake") + `","predicate":"` + wireID("predicate:summary") + `","object":{"type":"string","value":"grounded"},"confidence":1,"evidence_ids":["` + wireID("missing") + `"]}]}`,
	}}, ModelRunnerConfig{Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	custom := budgets()
	custom.MaxInputTokens = 10000
	record, err := newGeneratorWithBudgets(t, runner, &fakeTools{pack: pack}, custom).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.StopReason != StopReasonInvalid || len(record.Trace.Failures) != 1 {
		t.Fatalf("failure trace missing: %#v", record.Trace)
	}
}

func TestModelRunnerWorksWithDeterministicFakeProvider(t *testing.T) {
	pack, initial, _ := fixture(t)
	runner, err := NewModelRunner(lowmodel.FakeGenerator{}, ModelRunnerConfig{Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
		"fake-retrieve-1": nil,
	}}
	custom := budgets()
	custom.MaxInputTokens = 10000
	custom.MaxOutputTokens = 10000
	record, err := newGeneratorWithModel(t, runner, host, custom, "fake", "deterministic").Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if record.Trace.StopReason != StopReasonStop || len(record.Result.Claims()) != 1 {
		t.Fatalf("fake provider did not produce deterministic grounded stop: %#v", record.Trace)
	}
	if got := record.Result.Claims()[0].EvidenceIDs()[0]; got != initial.ID() {
		t.Fatalf("claim evidence = %q, want %q", got, initial.ID())
	}
}

func TestModelRunnerCacheIdentityIncludesClockIdentity(t *testing.T) {
	uncacheable, err := NewModelRunner(
		lowmodel.FakeGenerator{Model: "deterministic"},
		ModelRunnerConfig{Now: func() time.Time { return fixedTime }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uncacheable.CacheIdentity(); !errors.Is(err, ErrCacheIdentityUnsafe) {
		t.Fatalf("missing clock identity error = %v", err)
	}
	first, err := NewModelRunner(
		lowmodel.FakeGenerator{Model: "deterministic"},
		ModelRunnerConfig{
			Now:           func() time.Time { return fixedTime },
			ClockIdentity: "fixed-clock-v1",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewModelRunner(
		lowmodel.FakeGenerator{Model: "deterministic"},
		ModelRunnerConfig{
			Now:           func() time.Time { return fixedTime.Add(time.Second) },
			ClockIdentity: "fixed-clock-v2",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity, err := first.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := second.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity == secondIdentity {
		t.Fatal("clock identities did not separate model runner cache identity")
	}
}

func TestModelRunnerPreflightsInputBudget(t *testing.T) {
	pack, _, _ := fixture(t)
	runner, err := NewModelRunner(&scriptedTextGenerator{responses: []string{
		`{"action":"stop","correlation_id":"` + wireID("stop") + `","unsupported":[{"input":"x","reason":"y","evidence_ids":[]}]}`,
	}}, ModelRunnerConfig{Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	custom := budgets()
	custom.MaxInputTokens = 1
	record, err := newGeneratorWithBudgets(t, runner, &fakeTools{pack: pack}, custom).Run(context.Background(), pack)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 0 {
		t.Fatalf("provider was called despite input preflight: %#v", record.Trace)
	}
}

func TestModelRunnerAccountsInvalidOutputUsage(t *testing.T) {
	pack, _, _ := fixture(t)
	runner, err := NewModelRunner(&scriptedTextGenerator{responses: []string{"not-json"}}, ModelRunnerConfig{
		TokenEstimator: zeroTokenEstimator{},
		Now:            func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := newGenerator(t, runner, &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 1 || record.Trace.Usage.InputTokens != 1 ||
		record.Trace.Usage.OutputTokens != 1 {
		t.Fatalf("invalid output usage was not traced: %#v", record.Trace)
	}
}

func TestModelRunnerAccountsProviderFailureAsAttemptedCall(t *testing.T) {
	pack, _, _ := fixture(t)
	runner, err := NewModelRunner(&scriptedTextGenerator{}, ModelRunnerConfig{
		TokenEstimator: zeroTokenEstimator{},
		Now:            func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := newGenerator(t, runner, &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrRunnerUnavailable) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 1 || len(record.Trace.Iterations) != 1 {
		t.Fatalf("provider failure was not traced as attempted call: %#v", record.Trace)
	}
}

func TestModelRunnerClassifiesOversizedResponseAsBudget(t *testing.T) {
	pack, _, _ := fixture(t)
	runner, err := NewModelRunner(oversizedGenerator{}, ModelRunnerConfig{
		TokenEstimator: zeroTokenEstimator{},
		Now:            func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := newGenerator(t, runner, &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrBudgetExhausted) || record.Trace.StopReason != StopReasonBudgetExhausted {
		t.Fatalf("oversized response error=%v trace=%#v", err, record.Trace)
	}
	if record.Trace.Usage.ModelCalls != 1 {
		t.Fatalf("oversized response was not traced as a model call: %#v", record.Trace)
	}
}

func TestModelRunnerRejectsNegativeInputEstimateBeforeProviderCall(t *testing.T) {
	pack, _, _ := fixture(t)
	generator := &scriptedTextGenerator{responses: []string{
		`{"action":"stop","correlation_id":"` + wireID("stop") + `","unsupported":[{"input":"x","reason":"y","evidence_ids":[]}]}`,
	}}
	runner, err := NewModelRunner(generator, ModelRunnerConfig{
		TokenEstimator: negativeTokenEstimator{},
		Now:            func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := newGenerator(t, runner, &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 0 || generator.next != 0 {
		t.Fatalf("negative estimate reached provider: trace=%#v calls=%d", record.Trace, generator.next)
	}
}

func TestModelRunnerRejectsNegativeProviderUsageWithoutNegativeTrace(t *testing.T) {
	pack, _, _ := fixture(t)
	runner, err := NewModelRunner(negativeUsageGenerator{}, ModelRunnerConfig{
		TokenEstimator: zeroTokenEstimator{},
		Now:            func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := newGenerator(t, runner, &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 1 || record.Trace.Usage.InputTokens != 0 ||
		record.Trace.Usage.OutputTokens != 0 {
		t.Fatalf("negative provider usage reached trace: %#v", record.Trace)
	}
}

func TestModelRunnerRejectsClaimWithoutConfidence(t *testing.T) {
	pack, _, additions := fixture(t)
	runner, err := NewModelRunner(&scriptedTextGenerator{responses: []string{
		`{"action":"stop","correlation_id":"` + wireID("stop") + `","claims":[{"subject":"` + wireID("entity:fake") + `","predicate":"` + wireID("predicate:summary") + `","object":{"type":"string","value":"grounded"},"evidence_ids":["` + wireID(additions[0].ID()) + `"]}]}`,
	}}, ModelRunnerConfig{
		TokenEstimator: zeroTokenEstimator{},
		Now:            func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := newGenerator(t, runner, &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 1 {
		t.Fatalf("missing confidence action was not traced: %#v", record.Trace)
	}
}

func TestModelRunnerProtocolIDsAreLosslessBase64(t *testing.T) {
	documentAnchor, err := inference.NewDocumentAnchor(document.Citation{
		DocumentID: "doc\xff",
		RevisionID: "rev\xff",
		SectionID:  "sec\xff",
		SpanID:     "span\xff",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0},
			End:   document.SourcePosition{Offset: 1},
		},
	}, "x")
	if err != nil {
		t.Fatal(err)
	}
	graphAnchor, err := inference.NewGraphAnchor(graph.Path{
		Nodes: []graph.Node{{ID: "node\xff", Kind: "entity", Properties: shoal.Metadata{"key": "value"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pack := mustPack(t, []inference.EvidenceAnchor{documentAnchor, graphAnchor}, fixedTime.Add(time.Hour))
	model, promptProvenance := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, promptProvenance, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	request, err := newSessionRequest(pack, budgets(), provenance)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := modelPrompt(request, newTranscript(request))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []shoal.ID{"doc\xff", "rev\xff", "sec\xff", "span\xff", "node\xff"} {
		if !strings.Contains(prompt, wireID(id)) {
			t.Fatalf("prompt omitted lossless ID encoding for %q: %s", id, prompt)
		}
	}
	encodedMetadata, err := json.Marshal(promptMetadataFrom(shoal.Metadata{"key\xff": "value\xff"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []shoal.ID{"key\xff", "value\xff"} {
		if !strings.Contains(string(encodedMetadata), wireID(value)) {
			t.Fatalf("metadata omitted lossless encoding for %q: %s", value, encodedMetadata)
		}
	}
	action, err := parseModelAction(
		`{"action":"open_section","correlation_id":"`+wireID("open\xff")+`","document_id":"`+wireID("doc\xff")+`","revision_id":"`+wireID("rev\xff")+`","section_id":"`+wireID("sec\xff")+`"}`,
		pack, provenance, Usage{}, fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if action.open.documentID != "doc\xff" || action.open.revisionID != "rev\xff" ||
		action.open.sectionID != "sec\xff" || action.correlation != "open\xff" {
		t.Fatalf("decoded IDs were not preserved: %#v", action)
	}
}

func TestModelPromptIncludesRequiredActionSchemas(t *testing.T) {
	pack, _, _ := fixture(t)
	model, promptProvenance := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, promptProvenance, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	request, err := newSessionRequest(pack, budgets(), provenance)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := modelPrompt(request, newTranscript(request))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"action=retrieve", "correlation_id", "query", "limit",
		"action=open_section", "document_id", "revision_id", "section_id",
		"action=neighbors", "node_id", "hops", "fanout",
		"action=stop", "claims[].object.type", "claims[].object.value",
		"unresolved[].reason", "unsupported[].reason", "timestamp", "reference",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt omitted schema detail %q: %s", required, prompt)
		}
	}
	var envelope promptEnvelope
	if err := json.Unmarshal([]byte(prompt), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Consumed.Evidence != len(pack.Evidence()) ||
		envelope.Remaining.Evidence != budgets().MaxEvidence-len(pack.Evidence()) {
		t.Fatalf("prompt evidence budget excludes initial anchors: consumed=%#v remaining=%#v", envelope.Consumed, envelope.Remaining)
	}
}

func TestModelPromptIncludesPriorEmptyToolDetailsAndBudgets(t *testing.T) {
	pack, _, additions := fixture(t)
	model, promptProvenance := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, promptProvenance, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	request, err := newSessionRequest(pack, budgets(), provenance)
	if err != nil {
		t.Fatal(err)
	}
	action := mustRetrieveUsage(t, "empty", 1, Usage{InputTokens: 2, OutputTokens: 3})
	result, err := NewToolResult("empty", ActionRetrieve, nil, pack.Snapshot(), pack.Authorization())
	if err != nil {
		t.Fatal(err)
	}
	transcript := newTranscript(request)
	transcript.exchanges = []Exchange{{action: action, result: result}}
	prompt, err := modelPrompt(request, transcript)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"query":"query"`, `"limit":1`, `"evidence_ids"`, `"InputTokens":2`,
		`"consumed"`, `"remaining"`,
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt omitted prior tool detail %q: %s", required, prompt)
		}
	}
	result, err = NewToolResult("retrieved", ActionRetrieve, additions[:1], pack.Snapshot(), pack.Authorization())
	if err != nil {
		t.Fatal(err)
	}
	action = mustRetrieveUsage(t, "retrieved", 1, Usage{InputTokens: 2, OutputTokens: 3})
	transcript = newTranscript(request)
	transcript.context, err = addAnchors(
		transcript.context, result.anchors)
	if err != nil {
		t.Fatal(err)
	}
	transcript.exchanges = []Exchange{{action: action, result: result}}
	prompt, err = modelPrompt(request, transcript)
	if err != nil {
		t.Fatal(err)
	}
	var envelope promptEnvelope
	if err := json.Unmarshal([]byte(prompt), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Consumed.Evidence != len(pack.Evidence())+len(result.anchors) ||
		envelope.Remaining.Evidence != budgets().MaxEvidence-envelope.Consumed.Evidence {
		t.Fatalf("prompt double-counted evidence budget: consumed=%#v remaining=%#v", envelope.Consumed, envelope.Remaining)
	}
}

type scriptedTextGenerator struct {
	responses []string
	next      int
}

type zeroTokenEstimator struct{}

func (zeroTokenEstimator) EstimateTextTokens(context.Context, string) (int, error) { return 0, nil }

type negativeTokenEstimator struct{}

func (negativeTokenEstimator) EstimateTextTokens(context.Context, string) (int, error) {
	return -1, nil
}

func wireID(id shoal.ID) string {
	return base64.StdEncoding.EncodeToString([]byte(id))
}

func (g *scriptedTextGenerator) Generate(ctx context.Context, req lowmodel.GenerateRequest) (lowmodel.GenerateResult, error) {
	if err := ctx.Err(); err != nil {
		return lowmodel.GenerateResult{}, err
	}
	if !strings.Contains(req.Prompt, ModelPromptMarker) {
		return lowmodel.GenerateResult{}, errors.New("prompt omitted protocol marker")
	}
	if g.next >= len(g.responses) {
		return lowmodel.GenerateResult{}, errors.New("script exhausted")
	}
	text := g.responses[g.next]
	g.next++
	return lowmodel.GenerateResult{
		Text:       text,
		Provenance: lowmodel.Provenance{Provider: "fake-provider", Model: "fake-model"},
		Usage:      lowmodel.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}, nil
}

type negativeUsageGenerator struct{}

func (negativeUsageGenerator) Generate(ctx context.Context, req lowmodel.GenerateRequest) (lowmodel.GenerateResult, error) {
	if err := ctx.Err(); err != nil {
		return lowmodel.GenerateResult{}, err
	}
	if !strings.Contains(req.Prompt, ModelPromptMarker) {
		return lowmodel.GenerateResult{}, errors.New("prompt omitted protocol marker")
	}
	return lowmodel.GenerateResult{
		Text:       "not-json",
		Provenance: lowmodel.Provenance{Provider: "fake-provider", Model: "fake-model"},
		Usage:      lowmodel.Usage{InputTokens: -1, OutputTokens: 1, TotalTokens: 0},
	}, nil
}

type oversizedGenerator struct{}

func (oversizedGenerator) Generate(context.Context, lowmodel.GenerateRequest) (lowmodel.GenerateResult, error) {
	return lowmodel.GenerateResult{}, lowmodel.ErrOversizedResponse
}

func newGeneratorWithModel(t *testing.T, runner Runner, tools ToolHost, b Budgets, provider, modelName string) *Generator {
	t.Helper()
	_, prompt := provenanceParts(t)
	model, err := inference.NewModelProvenance(provider, modelName, "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGenerator(runner, tools, b, p, &captureRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	return g
}
