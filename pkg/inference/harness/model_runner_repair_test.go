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
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/inference"
	lowmodel "github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type promptCapture struct {
	inner   *scriptedTextGenerator
	prompts []string
}

func (p *promptCapture) Generate(ctx context.Context, req lowmodel.GenerateRequest) (lowmodel.GenerateResult, error) {
	p.prompts = append(p.prompts, req.Prompt)
	return p.inner.Generate(ctx, req)
}

func repairScript(t *testing.T, additions []inference.EvidenceAnchor, malformed ...string) *promptCapture {
	t.Helper()
	responses := []string{`{"action":"retrieve","correlation_id":"` + wireID("retrieve") + `","query":"entity","limit":1}`}
	responses = append(responses, malformed...)
	responses = append(responses, `{"action":"stop","correlation_id":"`+wireID("stop")+`","claims":[{"subject":"`+
		wireID("entity:fake")+`","predicate":"`+wireID("predicate:summary")+
		`","object":{"type":"string","value":"grounded"},"confidence":1,"evidence_ids":["`+wireID(additions[0].ID())+`"]}]}`)
	return &promptCapture{inner: &scriptedTextGenerator{responses: responses}}
}

func repairGenerator(t *testing.T, generator lowmodel.TextGenerator, cfg ModelRunnerConfig, b Budgets, repairs int) (*Generator, *captureRecorder, inference.ContextPack) {
	t.Helper()
	pack, _, additions := fixture(t)
	cfg.Now = func() time.Time { return fixedTime }
	runner, err := NewModelRunner(generator, cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &captureRecorder{}
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"retrieve": {additions[0]}}}
	g, err := NewGenerator(runner, host, b, provenance, recorder)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	if err := g.SetOutputRepairTurns(repairs); err != nil {
		t.Fatal(err)
	}
	return g, recorder, pack
}

func TestModelRunnerMalformedOutputEndsRunWithoutRepair(t *testing.T) {
	_, _, additions := fixture(t)
	custom := budgets()
	custom.MaxInputTokens = 100000
	g, _, pack := repairGenerator(t, repairScript(t, additions, `{"protocol":"echoed"}`), ModelRunnerConfig{}, custom, 0)
	record, err := g.Run(context.Background(), pack)
	if !errors.Is(err, ErrMalformedModelOutput) || !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), `unknown field "protocol"`) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.StopReason != StopReasonInvalid || len(record.Trace.Iterations) != 2 {
		t.Fatalf("trace = %+v", record.Trace)
	}
}

func TestModelRunnerRepairTurnRecordsBothAttempts(t *testing.T) {
	_, _, additions := fixture(t)
	script := repairScript(t, additions, `{"protocol":"echoed"}`)
	custom := budgets()
	custom.MaxInputTokens = 100000
	g, recorder, pack := repairGenerator(t, script, ModelRunnerConfig{}, custom, 1)
	record, err := g.Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if record.Transcript.RepairAttempts() != 1 || len(record.Trace.Iterations) != 3 ||
		record.Trace.Usage.ModelCalls != 3 || record.Trace.Usage.InputTokens != 3 ||
		record.Trace.Iterations[1].Failure == "" || record.Trace.Iterations[2].Decision != ActionStop ||
		len(record.Trace.Failures) != 1 || record.Trace.Failures[0].Iteration != 1 {
		t.Fatalf("trace = %+v", record.Trace)
	}
	turns := recorder.record.Turns
	if len(turns) != 3 || turns[0].Index != 0 || turns[0].Decision != ActionRetrieve || turns[0].Failed ||
		turns[1].Index != 1 || !turns[1].Failed || turns[1].Usage.InputTokens != 1 ||
		turns[2].Index != 2 || turns[2].Decision != ActionStop || turns[2].Failed {
		t.Fatalf("interaction turns = %+v", turns)
	}
	if strings.Contains(script.prompts[1], `"repair"`) {
		t.Fatal("repair section shown before any malformed output")
	}
	var repaired struct {
		Repair *promptRepair `json:"repair"`
	}
	if err := json.Unmarshal([]byte(script.prompts[2]), &repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.Repair == nil || repaired.Repair.Attempt != 1 ||
		!strings.Contains(repaired.Repair.Error, `unknown field "protocol"`) ||
		repaired.Repair.PreviousOutput != `{"protocol":"echoed"}` {
		t.Fatalf("repair prompt = %+v", repaired.Repair)
	}
}

func TestModelRunnerRepairTurnIsBounded(t *testing.T) {
	_, _, additions := fixture(t)
	custom := budgets()
	custom.MaxInputTokens = 100000
	t.Run("one repair only", func(t *testing.T) {
		script := repairScript(t, additions, `not json`, `{"action":`)
		g, _, pack := repairGenerator(t, script, ModelRunnerConfig{}, custom, 1)
		record, err := g.Run(context.Background(), pack)
		if !errors.Is(err, ErrMalformedModelOutput) || len(script.prompts) != 3 || len(record.Trace.Failures) != 2 {
			t.Fatalf("err=%v calls=%d trace=%+v", err, len(script.prompts), record.Trace)
		}
	})
	t.Run("step budget", func(t *testing.T) {
		steps := custom
		steps.MaxSteps = 2
		script := repairScript(t, additions, `not json`)
		g, _, pack := repairGenerator(t, script, ModelRunnerConfig{}, steps, 1)
		if _, err := g.Run(context.Background(), pack); !errors.Is(err, ErrMalformedModelOutput) || len(script.prompts) != 2 {
			t.Fatalf("repair ran past MaxSteps: err=%v calls=%d", err, len(script.prompts))
		}
	})
	t.Run("token budget", func(t *testing.T) {
		tokens := custom
		tokens.MaxInputTokens = 2 // retrieve and the malformed turn use one each
		script := repairScript(t, additions, `not json`)
		g, _, pack := repairGenerator(t, script, ModelRunnerConfig{TokenEstimator: zeroTokenEstimator{}}, tokens, 1)
		if _, err := g.Run(context.Background(), pack); !errors.Is(err, ErrBudgetExhausted) || len(script.prompts) != 2 {
			t.Fatalf("repair ignored the input-token budget: err=%v calls=%d", err, len(script.prompts))
		}
	})
	t.Run("not repairable", func(t *testing.T) {
		// Valid action JSON with an ungrounded claim is a protocol violation,
		// not a malformed output: it is never repaired.
		script := &promptCapture{inner: &scriptedTextGenerator{responses: []string{
			`{"action":"stop","correlation_id":"` + wireID("stop") + `","claims":[{"subject":"` + wireID("entity:fake") + `","predicate":"` + wireID("predicate:summary") + `","object":{"type":"string","value":"x"},"confidence":1,"evidence_ids":["` + wireID("missing") + `"]}]}`,
		}}}
		g, _, pack := repairGenerator(t, script, ModelRunnerConfig{}, custom, 1)
		if _, err := g.Run(context.Background(), pack); errors.Is(err, ErrMalformedModelOutput) || !errors.Is(err, ErrInvalid) || len(script.prompts) != 1 {
			t.Fatalf("ungrounded stop was treated as repairable: err=%v calls=%d", err, len(script.prompts))
		}
	})
	if err := (&Generator{}).SetOutputRepairTurns(MaxOutputRepairTurns + 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded repair setting accepted: %v", err)
	}
}

func TestModelRunnerHostContextSection(t *testing.T) {
	pack, _, _ := fixture(t)
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	request, err := newSessionRequest(pack, budgets(), provenance)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := modelPrompt(request, newTranscript(request), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "host_context") || strings.Contains(plain, `"repair"`) {
		t.Fatal("prompt without extensions is not the base envelope")
	}
	runner, err := NewModelRunner(lowmodel.FakeGenerator{}, ModelRunnerConfig{HostContext: json.RawMessage(` { "base" : {"concepts":[]} } `)})
	if err != nil {
		t.Fatal(err)
	}
	extended, err := modelPrompt(request, newTranscript(request), runner.cfg.HostContext)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(extended, `"host_context":{"base":{"concepts":[]}}`) ||
		!strings.HasPrefix(extended, strings.TrimSuffix(plain, "}")) {
		t.Fatalf("host context was not appended to the base envelope: %s", extended)
	}
	if ModelPromptExtendedTemplateHash() == ModelPromptTemplateHash() {
		t.Fatal("extended template shares the base template hash")
	}
	for _, invalidContext := range []string{`[1]`, `{"a":`, `"text"`, `{"a":"` + strings.Repeat("x", MaxHostContextBytes) + `"}`} {
		if _, err := NewModelRunner(lowmodel.FakeGenerator{}, ModelRunnerConfig{HostContext: json.RawMessage(invalidContext)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("host context %.20q accepted: %v", invalidContext, err)
		}
	}
	base, err := NewModelRunner(cacheableGenerator{}, ModelRunnerConfig{ClockIdentity: "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	withContext, err := NewModelRunner(cacheableGenerator{}, ModelRunnerConfig{ClockIdentity: "fixed", HostContext: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	left, leftErr := base.CacheIdentity()
	right, rightErr := withContext.CacheIdentity()
	if leftErr != nil || rightErr != nil || left == right {
		t.Fatalf("host context does not change the runner cache identity: %v %v", leftErr, rightErr)
	}
}

type cacheableGenerator struct{}

func (cacheableGenerator) Generate(context.Context, lowmodel.GenerateRequest) (lowmodel.GenerateResult, error) {
	return lowmodel.GenerateResult{}, errors.New("not called")
}

func (cacheableGenerator) CacheIdentity() (string, error) { return "cacheable-generator-v1", nil }
