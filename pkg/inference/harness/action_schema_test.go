package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/quick"
	"time"

	lowmodel "github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestActionSchemaStopPropertyParses(t *testing.T) {
	pack, anchor, _ := fixture(t)
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("property-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ActionSchema(promptEnvelope{Evidence: []promptEvidence{{ID: newProtocolID(anchor.ID())}}})
	if err != nil || !strings.Contains(string(schema), `"claims"`) {
		t.Fatalf("schema = %s, %v", schema, err)
	}
	check := func(input string, score uint16) bool {
		value := fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
		for _, output := range []map[string]any{
			{"action": "stop", "correlation_id": wireID("turn:0"), "claims": []any{map[string]any{
				"subject": wireID("subject"), "predicate": wireID("predicate"), "object": map[string]any{"type": "string", "value": value},
				"confidence": float64(score) / 65535, "evidence_ids": []string{wireID(anchor.ID())},
			}}, "unresolved": []any{}, "unsupported": []any{}},
			{"action": "stop", "correlation_id": wireID("turn:0"), "claims": []any{}, "unresolved": []any{map[string]any{
				"input": "question", "reason": value, "evidence_ids": []string{wireID(anchor.ID())},
			}}, "unsupported": []any{}},
		} {
			encoded, _ := json.Marshal(output)
			if _, err := parseModelAction(string(encoded), pack, provenance, Usage{InputTokens: 1, OutputTokens: 1}, fixedTime); err != nil {
				t.Log(err)
				return false
			}
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 128}); err != nil {
		t.Fatal(err)
	}
}

func TestActionSchemaUsesVisibleIDsAndRemainingBudget(t *testing.T) {
	_, initial, _ := fixture(t)
	citation, _, ok := initial.Document()
	if !ok {
		t.Fatal("fixture lacks document anchor")
	}
	envelope := promptEnvelope{
		Budgets:   Budgets{MaxFanout: 2},
		Remaining: BudgetUsage{ModelCalls: 2, Evidence: 2, GraphHops: 1, GraphNodes: 2},
		Evidence:  []promptEvidence{{ID: newProtocolID(initial.ID()), Citation: promptCitationFrom(citation)}},
	}
	schema, err := ActionSchema(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), wireID(initial.ID())) || strings.Contains(string(schema), wireID(shoal.ID("hidden-section"))) {
		t.Fatalf("visible evidence not reflected in schema: %s", schema)
	}
	envelope.Evidence = append(envelope.Evidence, promptEvidence{ID: newProtocolID("second-anchor"), Citation: promptCitationFrom(citation)})
	schema, err = ActionSchema(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, raw := range parsed["oneOf"].([]any) {
		variant := raw.(map[string]any)
		properties := variant["properties"].(map[string]any)
		if properties["action"].(map[string]any)["const"] == string(ActionOpenSection) && len(variant["oneOf"].([]any)) != 1 {
			t.Fatal("duplicate anchors made the visible section ambiguous")
		}
	}
	envelope.Remaining.ModelCalls = 1
	schema, err = ActionSchema(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schema), `"retrieve"`) || strings.Contains(string(schema), `"open_section"`) {
		t.Fatalf("tool actions survived final turn: %s", schema)
	}
}

type capRepairGenerator struct {
	limits []int
	calls  int
}

func (g *capRepairGenerator) Generate(_ context.Context, req lowmodel.GenerateRequest) (lowmodel.GenerateResult, error) {
	g.limits = append(g.limits, req.MaxOutputTokens)
	g.calls++
	text, output := "incomplete", req.MaxOutputTokens
	if g.calls == 2 {
		text = `{"action":"stop","correlation_id":"` + wireID("repair") + `","unsupported":[{"input":"question","reason":"insufficient evidence","evidence_ids":[]}]}`
		output = 1
	}
	return lowmodel.GenerateResult{Text: text, Provenance: lowmodel.Provenance{Provider: "fake-provider", Model: "fake-model"}, Usage: lowmodel.Usage{InputTokens: 1, OutputTokens: output}}, nil
}

func TestModelRunnerRepairsCapHitWithinReservedBudget(t *testing.T) {
	pack, _, _ := fixture(t)
	provider := &capRepairGenerator{}
	runner, err := NewModelRunner(provider, ModelRunnerConfig{TokenEstimator: zeroTokenEstimator{}, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	b := budgets()
	b.MaxInputTokens, b.MaxOutputTokens = 100, 6
	b.MaxOutputTokensPerTurn, b.RepairOutputTokens = 2, 3
	g := newGeneratorWithBudgets(t, runner, &fakeTools{pack: pack}, b)
	if err := g.SetOutputRepairTurns(1); err != nil {
		t.Fatal(err)
	}
	record, err := g.Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.limits) != 2 || provider.limits[0] != 2 || provider.limits[1] != 2 {
		t.Fatalf("limits = %v", provider.limits)
	}
	if record.Trace.Usage.OutputTokens != 3 || record.Trace.Usage.ModelCalls != 2 || record.Trace.StopReason != StopReasonStop {
		t.Fatalf("trace = %#v", record.Trace)
	}
}

func TestConfigurableByteEstimate(t *testing.T) {
	runner, err := NewModelRunner(&scriptedTextGenerator{}, ModelRunnerConfig{BytesPerToken: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	got, err := runner.estimateInputTokens(context.Background(), "0123456789")
	if err != nil || got != 4 {
		t.Fatalf("estimate = %d, %v", got, err)
	}
}
