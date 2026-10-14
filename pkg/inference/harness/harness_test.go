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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var fixedTime = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func TestMergeEmbeddingSpacesIsCanonical(t *testing.T) {
	pack, _, _ := fixture(t)
	left, err := interaction.NewEmbeddingSpaceSet([]string{"space-b"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := interaction.NewEmbeddingSpaceSet(
		[]string{"space-a", "space-b"})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeEmbeddingSpaces(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Identities) != 2 ||
		merged.Identities[0] != "space-a" ||
		merged.Identities[1] != "space-b" ||
		merged.Digest == "" {
		t.Fatalf("merged embedding spaces = %+v", merged)
	}
	_ = pack
}

func TestToolResultRejectsNoncanonicalEmbeddingSpaces(t *testing.T) {
	pack, _, _ := fixture(t)
	if _, err := NewToolResultWithEmbeddingSpaces(
		"retrieve", ActionRetrieve, nil,
		pack.Snapshot(), pack.Authorization(),
		interaction.EmbeddingSpaceSet{
			Identities: []string{"space-a"},
			Digest:     "not-canonical",
		},
	); err == nil {
		t.Fatal("noncanonical embedding space set was accepted")
	}
}

func TestSuccessfulTraceAndCanonicalTranscript(t *testing.T) {
	pack, initial, additions := fixture(t)
	embeddingSpaces, err := interaction.NewEmbeddingSpaceSet(
		[]string{"space-v3"})
	if err != nil {
		t.Fatal(err)
	}
	retrieve := mustRetrieve(t, "r1", "more evidence", 2)
	open := mustOpen(t, "r2", "document", "section-initial")
	neighbors := mustNeighbors(t, "r3", "node-a", 1, 2)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
		"r1": {additions[0]}, "r2": {additions[1]}, "r3": {additions[2]},
	}, embeddingSpaces: embeddingSpaces}
	runner := NewFakeRunner(
		ScriptAction(retrieve), ScriptAction(open), ScriptAction(neighbors),
		func(_ context.Context, transcript Transcript) (Action, error) {
			if len(transcript.Exchanges()) != 3 || len(transcript.Context().Evidence()) != 4 {
				t.Fatal("runner did not receive grounded additions")
			}

			result := resultFor(t, transcript.Context(), additions[2])
			return NewStopAction("r4", result, Usage{OutputTokens: 1})
		},
	)
	g := newGenerator(t, runner, host)
	record, err := g.Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}

	if err := record.Result.ValidateFor(pack); err != nil {
		t.Fatal(err)
	}
	if len(record.Result.EvidenceAdditions()) != 3 {
		t.Fatal("final result did not retain verified additions")
	}
	if len(record.Transcript.Exchanges()) != 3 {
		t.Fatal("wrong transcript length")
	}
	if final, ok := record.Transcript.Final(); !ok || final.Kind() != ActionStop {
		t.Fatal("stop action missing from transcript")
	}
	if record.Transcript.ID() == "" || record.Request.ID() == "" {
		t.Fatal("missing canonical identity")
	}
	evaluation, err := evaluationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.EmbeddingSpaces.Digest != embeddingSpaces.Digest ||
		len(evaluation.EmbeddingSpaces.Identities) != 1 ||
		evaluation.EmbeddingSpaces.Identities[0] != "space-v3" {
		t.Fatalf("evaluation embedding spaces = %+v", evaluation.EmbeddingSpaces)
	}

	g2 := newGenerator(t, runner, host)
	record2, err := g2.Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if record.Transcript.ID() != record2.Transcript.ID() || record.Request.ID() != record2.Request.ID() {
		t.Fatal("deterministic execution changed canonical identity")
	}
	if initial.ID() == additions[0].ID() {
		t.Fatal("fixture anchors collided")
	}
}

func TestOverlappingToolEvidencePreservesSetSemantics(t *testing.T) {
	pack, initial, _ := fixture(t)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"same": {initial}}}
	runner := NewFakeRunner(ScriptAction(mustRetrieve(t, "same", "overlap", 1)), func(_ context.Context, tr Transcript) (Action, error) {
		return NewStopAction("stop", resultFor(t, tr.Context(), initial), Usage{})
	})
	record, err := newGenerator(t, runner, host).Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Transcript.Context().Evidence()) != 1 {
		t.Fatal("overlapping evidence was duplicated")
	}
}

func TestEmptyToolResultIsGrounded(t *testing.T) {
	pack, initial, _ := fixture(t)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"empty": nil}}
	runner := NewFakeRunner(ScriptAction(mustRetrieve(t, "empty", "nothing", 1)), func(_ context.Context, tr Transcript) (Action, error) {
		return NewStopAction("stop", resultFor(t, tr.Context(), initial), Usage{})
	})
	if _, err := newGenerator(t, runner, host).Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsMalformedIDs(t *testing.T) {
	if _, err := NewRetrieveAction("", RetrieveRequest{query: "q", limit: 1}, Usage{}); err == nil {
		t.Fatal("empty correlation accepted")
	}
	if _, err := NewOpenSectionRequest("", "revision", "section"); err == nil {
		t.Fatal("empty document ID accepted")
	}
	if _, err := NewNeighborsRequest("", 1, 1); err == nil {
		t.Fatal("empty node ID accepted")
	}
	if _, err := NewRetrieveRequest(string([]byte{0xff}), 1); err == nil {
		t.Fatal("invalid UTF-8 query accepted")
	}
	if _, err := NewRetrieveRequest(strings.Repeat(" ", MaxToolQueryBytes+1), 1); err == nil {
		t.Fatal("oversized raw query accepted")
	}
}

func TestRejectsUnknownAndForbiddenActions(t *testing.T) {
	pack, _, _ := fixture(t)
	for _, kind := range []ActionKind{"shell", "filesystem", "url", "storage", "credentials", "arbitrary"} {
		t.Run(string(kind), func(t *testing.T) {
			a := Action{kind: kind, correlation: "x"}
			g := newGenerator(t, NewFakeRunner(ScriptAction(a)), &fakeTools{pack: pack})
			_, err := g.Generate(context.Background(), pack)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestBudgetBoundaries(t *testing.T) {
	pack, _, additions := fixture(t)
	tests := []struct {
		name    string
		budgets Budgets
		action  Action
	}{
		{"input tokens", budgets(), mustRetrieveUsage(t, "x", 1, Usage{InputTokens: 11})},
		{"output tokens", budgets(), mustRetrieveUsage(t, "x", 1, Usage{OutputTokens: 11})},
		{"retrieve fanout", budgets(), mustRetrieveUsage(t, "x", 6, Usage{})},
		{"graph hops", budgets(), mustNeighbors(t, "x", "node", 3, 1)},
		{"graph fanout", budgets(), mustNeighbors(t, "x", "node", 1, 6)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}}
			g := newGeneratorWithBudgets(t, NewFakeRunner(ScriptAction(tc.action)), host, tc.budgets)
			_, err := g.Generate(context.Background(), pack)
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("integer overflow", func(t *testing.T) {
		large := int(^uint(0) >> 1)
		custom := budgets()
		custom.MaxInputTokens = large
		first := mustRetrieveQueryUsage(t, "first", "first", 1, Usage{InputTokens: large})
		second := mustRetrieveQueryUsage(t, "second", "second", 1, Usage{InputTokens: 1})
		host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
			"first": {additions[0]}, "second": {additions[1]},
		}}
		g := newGeneratorWithBudgets(t, NewFakeRunner(ScriptAction(first), ScriptAction(second)), host, custom)
		if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("overflow error = %v", err)
		}
	})
	t.Run("exact token boundary", func(t *testing.T) {
		host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}}
		first := mustRetrieveUsage(t, "x", 1, Usage{InputTokens: 10, OutputTokens: 10})
		runner := NewFakeRunner(ScriptAction(first), func(_ context.Context, tr Transcript) (Action, error) {
			return NewStopAction("stop", resultFor(t, tr.Context(), additions[0]), Usage{})
		})
		g := newGenerator(t, runner, host)
		if _, err := g.Generate(context.Background(), pack); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("token overage is traced", func(t *testing.T) {
		host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}}
		first := mustRetrieveUsage(t, "x", 1, Usage{InputTokens: 11, OutputTokens: 12})
		g := newGenerator(t, NewFakeRunner(ScriptAction(first)), host)
		record, err := g.Run(context.Background(), pack)
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("error = %v", err)
		}
		if record.Trace.Usage.InputTokens != 11 || record.Trace.Usage.OutputTokens != 12 ||
			record.Trace.Iterations[0].Budget.InputTokens != 11 ||
			record.Trace.Iterations[0].Budget.OutputTokens != 12 {
			t.Fatalf("over-budget token usage was not traced: %#v", record.Trace)
		}
	})
}

func TestActionKeysFrameOpaqueIDs(t *testing.T) {
	left := mustOpen(t, "left", shoal.ID("a"), shoal.ID("b\x00c"))
	right := mustOpen(t, "right", shoal.ID("a\x00b"), shoal.ID("c"))
	if actionKey(left) == actionKey(right) {
		t.Fatal("distinct opaque IDs produced the same action key")
	}
}

func TestStepAndCycleLimits(t *testing.T) {
	pack, _, additions := fixture(t)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
		"a": {additions[0]}, "b": {additions[0]}, "c": {additions[0]},
	}}
	a := mustRetrieve(t, "a", "same", 1)
	b := mustRetrieve(t, "b", "same", 1)
	g := newGenerator(t, NewFakeRunner(ScriptAction(a), ScriptAction(b)), host)
	_, err := g.Generate(context.Background(), pack)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("cycle error = %v", err)
	}

	custom := budgets()
	custom.MaxSteps = 1
	g = newGeneratorWithBudgets(t, NewFakeRunner(ScriptAction(mustRetrieve(t, "c", "other", 1))), host, custom)
	_, err = g.Generate(context.Background(), pack)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("step error = %v", err)
	}
}

func TestRunAccountsSuccessfulActionBeforePostCallCancellation(t *testing.T) {
	pack, _, additions := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	action := mustRetrieveUsage(t, "x", 1, Usage{InputTokens: 2, OutputTokens: 3})
	runner := NewFakeRunner(func(context.Context, Transcript) (Action, error) {
		cancel()
		return action, nil
	})
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}}
	record, err := newGenerator(t, runner, host).Run(ctx, pack)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 1 || record.Trace.Usage.InputTokens != 2 ||
		record.Trace.Usage.OutputTokens != 3 {
		t.Fatalf("successful action was not accounted before cancellation: %#v", record.Trace)
	}
}

func TestRunAccountsDuplicateCorrelationBeforeRejecting(t *testing.T) {
	pack, _, additions := fixture(t)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}}
	first := mustRetrieveUsage(t, "x", 1, Usage{InputTokens: 1, OutputTokens: 1})
	second := mustRetrieveUsage(t, "x", 1, Usage{InputTokens: 2, OutputTokens: 3})
	record, err := newGenerator(t, NewFakeRunner(ScriptAction(first), ScriptAction(second)), host).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.ModelCalls != 2 || record.Trace.Usage.InputTokens != 3 ||
		record.Trace.Usage.OutputTokens != 4 {
		t.Fatalf("duplicate action usage was not accounted: %#v", record.Trace)
	}
}

func TestRunAccountsAttemptedToolUsageBeforeBudgetFailures(t *testing.T) {
	pack, _, additions := fixture(t)
	t.Run("evidence", func(t *testing.T) {
		custom := budgets()
		custom.MaxEvidence = 1
		host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
			"x": {additions[0], additions[1]},
		}}
		record, err := newGeneratorWithBudgets(
			t, NewFakeRunner(ScriptAction(mustRetrieve(t, "x", "query", 2))), host, custom,
		).Run(context.Background(), pack)
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("error = %v", err)
		}
		if record.Trace.Usage.Evidence != 3 || len(record.Trace.Iterations[0].EvidenceIDs) != 2 {
			t.Fatalf("attempted evidence overage was not traced: %#v", record.Trace)
		}
	})
	t.Run("graph nodes", func(t *testing.T) {
		custom := budgets()
		custom.MaxGraphNodes = 1
		host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{
			"x": {graphNeighborAnchor(t)},
		}}
		record, err := newGeneratorWithBudgets(
			t, NewFakeRunner(ScriptAction(mustRetrieve(t, "x", "query", 1))), host, custom,
		).Run(context.Background(), pack)
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("error = %v", err)
		}
		if record.Trace.Usage.GraphNodes != 2 || len(record.Trace.Iterations[0].EvidenceIDs) != 1 {
			t.Fatalf("attempted graph-node overage was not traced: %#v", record.Trace)
		}
	})
}

func TestRunRejectsInitialEvidenceBudgetBeforeRunnerStart(t *testing.T) {
	pack := mustPack(t, []inference.EvidenceAnchor{anchor(t, "one", 0), anchor(t, "two", 10)}, fixedTime.Add(time.Hour))
	custom := budgets()
	custom.MaxEvidence = 1
	runner := &startCountingRunner{}
	record, err := newGeneratorWithBudgets(t, runner, &fakeTools{pack: pack}, custom).Run(context.Background(), pack)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if runner.starts != 0 {
		t.Fatalf("runner started despite initial evidence overage")
	}
	if record.Trace.StopReason != StopReasonBudgetExhausted || record.Trace.Usage.Evidence != 2 {
		t.Fatalf("initial evidence overage not traced: %#v", record.Trace)
	}
}

func TestRunRejectsInitialGraphNodeBudgetBeforeRunnerStart(t *testing.T) {
	pack := mustPack(t, []inference.EvidenceAnchor{graphNeighborAnchor(t)}, fixedTime.Add(time.Hour))
	custom := budgets()
	custom.MaxGraphNodes = 1
	runner := &startCountingRunner{}
	record, err := newGeneratorWithBudgets(t, runner, &fakeTools{pack: pack}, custom).Run(context.Background(), pack)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if runner.starts != 0 {
		t.Fatalf("runner started despite initial graph-node overage")
	}
	if record.Trace.StopReason != StopReasonBudgetExhausted || record.Trace.Usage.GraphNodes != 2 {
		t.Fatalf("initial graph-node overage not traced: %#v", record.Trace)
	}
}

func TestCancellationTimeoutAndRunnerFailure(t *testing.T) {
	pack, _, _ := fixture(t)
	block := ScriptStep(func(ctx context.Context, _ Transcript) (Action, error) {
		<-ctx.Done()
		return Action{}, ctx.Err()
	})
	g := newGenerator(t, NewFakeRunner(block), &fakeTools{pack: pack})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Generate(ctx, pack); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}

	short := budgets()
	short.MaxElapsed = time.Millisecond
	g = newGeneratorWithBudgets(t, NewFakeRunner(block), &fakeTools{pack: pack}, short)
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	g = newGenerator(t, NewFakeRunner(ScriptFault(errors.New("offline"))), &fakeTools{pack: pack})
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrRunnerUnavailable) {
		t.Fatalf("runner error = %v", err)
	}

	short.MaxElapsed = time.Millisecond
	g = newGeneratorWithBudgets(t, blockingRunner{}, &fakeTools{pack: pack}, short)
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start cancellation error = %v", err)
	}

	g = newGeneratorWithBudgets(t, NewFakeRunner(ScriptAction(mustRetrieve(t, "tool", "wait", 1))), blockingTools{}, short)
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tool cancellation error = %v", err)
	}
}

func TestRejectsDuplicateCorrelationAndMismatchedToolResult(t *testing.T) {
	pack, _, additions := fixture(t)
	first := mustRetrieve(t, "same", "a", 1)
	second := mustOpen(t, "same", "document", "section")
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"same": {additions[0]}}}
	g := newGenerator(t, NewFakeRunner(ScriptAction(first), ScriptAction(second)), host)
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate error = %v", err)
	}

	host.mismatch = true
	g = newGenerator(t, NewFakeRunner(ScriptAction(mustRetrieve(t, "x", "a", 1))), host)
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestRejectsUnissuedCapabilitiesAndProvenance(t *testing.T) {
	pack, initial, _ := fixture(t)
	host := &fakeTools{pack: pack}
	for name, action := range map[string]Action{
		"section":  mustOpen(t, "open", "document", "not-issued"),
		"revision": mustOpenRevision(t, "open-revision", "document", "stale-revision", "section-initial"),
		"node":     mustNeighbors(t, "neighbors", "not-issued", 1, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newGenerator(t, NewFakeRunner(ScriptAction(action)), host).Generate(context.Background(), pack)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	model, _ := inference.NewModelProvenance("fake-provider", "fake-model", "other-version", shoal.Metadata{"temperature": "1"}, nil)
	_, prompt := provenanceParts(t)
	value, _ := ontology.NewStringValue("grounded")
	claim, _ := inference.NewClaim("subject", "predicate", value, 1, []shoal.ID{initial.ID()}, inference.ClaimInferred, model, prompt, nil)
	result, _ := inference.NewInferenceResult(pack, []inference.Claim{claim}, nil, fixedTime, nil)
	stop, _ := NewStopAction("stop", result, Usage{})
	_, err := newGenerator(t, NewFakeRunner(ScriptAction(stop)), host).Generate(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("provenance error = %v", err)
	}

	badNeighbor := anchor(t, "not-a-graph-path", 40)
	host.results = map[shoal.ID][]inference.EvidenceAnchor{"retrieve": {graphAnchor(t)}, "neighbors": {badNeighbor}}
	runner := NewFakeRunner(
		ScriptAction(mustRetrieve(t, "retrieve", "graph", 1)),
		ScriptAction(mustNeighbors(t, "neighbors", "node-a", 1, 1)),
	)
	if _, err := newGenerator(t, runner, host).Generate(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("neighbors result error = %v", err)
	}
}

func TestEvaluationRecordIsRedactedAndIncludesBudgets(t *testing.T) {
	pack, initial, _ := fixture(t)
	recorder := &captureRecorder{}
	secretCorrelation := shoal.ID("https://secret.example/token")
	stop, _ := NewStopAction(secretCorrelation, resultFor(t, pack, initial), Usage{InputTokens: 1})
	_, prompt := provenanceParts(t)
	model, _ := inference.NewModelProvenance("fake-provider", "fake-model", "v1", nil, nil)
	provenance, _ := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	g, err := NewGenerator(NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack}, budgets(), provenance, recorder)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	captured := recorder.last()
	if captured.Budgets != budgets() || len(captured.ActionKinds) != 1 ||
		len(captured.ActionUsage) != 1 {
		t.Fatal("evaluation record omitted budgets or bounded action data")
	}
	encoded := fmt.Sprintf("%+v", captured)
	if strings.Contains(encoded, string(secretCorrelation)) {
		t.Fatal("evaluation record retained correlation ID")
	}
}

func TestBudgetDefaultsPreserveFormerKeyedLiterals(t *testing.T) {
	pack, initial, _ := fixture(t)
	legacy := Budgets{
		MaxSteps: 1, MaxElapsed: time.Second, MaxInputTokens: 1, MaxOutputTokens: 1,
		MaxGraphHops: 1, MaxFanout: 1, MaxRepeatedAction: 1,
	}
	stop, _ := NewStopAction("stop", resultFor(t, pack, initial), Usage{})
	g := newGeneratorWithBudgets(t, NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack}, legacy)
	record, err := g.Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if record.Trace.Budgets.MaxEvidence != 1 || record.Trace.Budgets.MaxGraphNodes != 2 {
		t.Fatalf("legacy budgets not normalized: %#v", record.Trace.Budgets)
	}
}

func TestEvidenceBudgetCannotExceedInferenceContract(t *testing.T) {
	b := budgets()
	b.MaxEvidence = inference.MaxEvidenceAnchors + 1
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGenerator(NewFakeRunner(), &fakeTools{}, b, provenance, &captureRecorder{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized evidence budget error = %v", err)
	}
	b = budgets()
	b.MaxFanout = int(maxUint32Value) + 1
	if _, err := NewGenerator(NewFakeRunner(), &fakeTools{}, b, provenance, &captureRecorder{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized graph budget error = %v", err)
	}
	if uint32RepresentableProduct(int(maxUint32Value), int(maxUint32Value)) {
		t.Fatal("overflowing graph budget product accepted")
	}
}

func TestRunRefreshesGraphHopBudgetWhenNeighborFails(t *testing.T) {
	pack, _, _ := fixture(t)
	action := mustNeighbors(t, "neighbors", "missing-node", 1, 1)
	record, err := newGenerator(t, NewFakeRunner(ScriptAction(action)), &fakeTools{pack: pack}).Run(context.Background(), pack)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if record.Trace.Usage.GraphHops != 1 || record.Trace.Iterations[0].Budget.GraphHops != 1 {
		t.Fatalf("failed neighbor hop was not traced: %#v", record.Trace)
	}
}

func TestRejectsStaleSnapshotAndAuthorization(t *testing.T) {
	pack, _, additions := fixture(t)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}, stale: true}
	g := newGenerator(t, NewFakeRunner(ScriptAction(mustRetrieve(t, "x", "a", 1))), host)
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale result error = %v", err)
	}

	expired := packWithExpiry(t, fixedTime.Add(-time.Minute))
	g = newGenerator(t, NewFakeRunner(), &fakeTools{pack: expired})
	g.now = func() time.Time { return fixedTime }
	if _, err := g.Generate(context.Background(), expired); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale auth error = %v", err)
	}

	stop, _ := NewStopAction("stop", resultFor(t, pack, pack.Evidence()[0]), Usage{})
	g = newGenerator(t, NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack})
	calls := 0
	g.now = func() time.Time {
		calls++
		if calls >= 4 {
			return pack.Authorization().ExpiresAt()
		}
		return fixedTime
	}
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("post-next expiry error = %v", err)
	}
}

func TestRejectsHallucinatedAnchorAndClaimWithoutEvidence(t *testing.T) {
	pack, _, additions := fixture(t)
	otherPack := mustPack(t, []inference.EvidenceAnchor{additions[2]}, fixedTime.Add(time.Hour))
	badResult := resultFor(t, otherPack, additions[2])
	stop, err := NewStopAction("stop", badResult, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	g := newGenerator(t, NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack})
	if _, err := g.Generate(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("hallucination error = %v", err)
	}

	model, prompt := provenanceParts(t)
	value, _ := ontology.NewStringValue("value")
	if _, err := inference.NewClaim("subject", "predicate", value, 1, nil, inference.ClaimInferred, model, prompt, nil); err == nil {
		t.Fatal("claim without evidence accepted")
	}
}

func TestMutationIsolation(t *testing.T) {
	pack, _, additions := fixture(t)
	host := &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}}
	runner := NewFakeRunner(ScriptAction(mustRetrieve(t, "x", "a", 1)), func(_ context.Context, tr Transcript) (Action, error) {
		copy := tr.Exchanges()
		copy[0].result.anchors[0] = additions[1]
		if tr.Exchanges()[0].Result().Anchors()[0].ID() != additions[0].ID() {
			t.Fatal("transcript mutated")
		}
		return NewStopAction("stop", resultFor(t, tr.Context(), additions[0]), Usage{})
	})
	record, err := newGenerator(t, runner, host).Run(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	anchors := record.Transcript.Exchanges()[0].Result().Anchors()
	anchors[0] = additions[1]
	if record.Transcript.Exchanges()[0].Result().Anchors()[0].ID() != additions[0].ID() {
		t.Fatal("record mutated")
	}
}

func TestConcurrentGeneration(t *testing.T) {
	pack, _, additions := fixture(t)
	runner := NewFakeRunner(ScriptAction(mustRetrieve(t, "x", "a", 1)), func(_ context.Context, tr Transcript) (Action, error) {
		return NewStopAction("stop", resultFor(t, tr.Context(), additions[0]), Usage{})
	})
	g := newGenerator(t, runner, &fakeTools{pack: pack, results: map[shoal.ID][]inference.EvidenceAnchor{"x": {additions[0]}}})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := g.Generate(context.Background(), pack); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

type fakeTools struct {
	pack            inference.ContextPack
	results         map[shoal.ID][]inference.EvidenceAnchor
	embeddingSpaces interaction.EmbeddingSpaceSet
	mismatch        bool
	stale           bool
}

// captureRecorder is shared across goroutines by TestConcurrentGeneration,
// which calls Generate concurrently, so recording must be synchronized. A real
// Recorder has the same obligation: capture is on the serving path.
type captureRecorder struct {
	mu     sync.Mutex
	record EvaluationRecord
}

func (r *captureRecorder) Record(_ context.Context, record EvaluationRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record = record
	return nil
}

func (r *captureRecorder) last() EvaluationRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.record
}

type startCountingRunner struct{ starts int }

func (r *startCountingRunner) Start(context.Context, SessionRequest) (Session, error) {
	r.starts++
	return nil, errors.New("should not start")
}

type blockingRunner struct{}

func (blockingRunner) Start(ctx context.Context, _ SessionRequest) (Session, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type blockingTools struct{}

func (blockingTools) Retrieve(ctx context.Context, _ ToolContext, _ RetrieveRequest) (ToolResult, error) {
	<-ctx.Done()
	return ToolResult{}, ctx.Err()
}
func (blockingTools) OpenSection(ctx context.Context, _ ToolContext, _ OpenSectionRequest) (ToolResult, error) {
	<-ctx.Done()
	return ToolResult{}, ctx.Err()
}
func (blockingTools) Neighbors(ctx context.Context, _ ToolContext, _ NeighborsRequest) (ToolResult, error) {
	<-ctx.Done()
	return ToolResult{}, ctx.Err()
}

func (f *fakeTools) Retrieve(_ context.Context, call ToolContext, _ RetrieveRequest) (ToolResult, error) {
	return f.make(call.CorrelationID(), ActionRetrieve)
}
func (f *fakeTools) OpenSection(_ context.Context, call ToolContext, _ OpenSectionRequest) (ToolResult, error) {
	return f.make(call.CorrelationID(), ActionOpenSection)
}
func (f *fakeTools) Neighbors(_ context.Context, call ToolContext, _ NeighborsRequest) (ToolResult, error) {
	return f.make(call.CorrelationID(), ActionNeighbors)
}
func (f *fakeTools) make(id shoal.ID, kind ActionKind) (ToolResult, error) {
	correlation := id
	if f.mismatch {
		correlation = "different"
	}
	snapshot, auth := f.pack.Snapshot(), f.pack.Authorization()
	if f.stale {
		snapshot, _ = inference.NewSnapshotPin("stale", snapshot.AsOf())
	}
	return NewToolResultWithEmbeddingSpaces(
		correlation, kind, f.results[id], snapshot, auth,
		f.embeddingSpaces,
	)
}

func budgets() Budgets {
	return Budgets{
		MaxSteps: 8, MaxElapsed: time.Second, MaxInputTokens: 10, MaxOutputTokens: 10,
		MaxEvidence: 5, MaxGraphHops: 2, MaxGraphNodes: 5, MaxFanout: 5,
		MaxRepeatedAction: 1,
	}
}
func newGenerator(t *testing.T, runner Runner, tools ToolHost) *Generator {
	return newGeneratorWithBudgets(t, runner, tools, budgets())
}
func newGeneratorWithBudgets(t *testing.T, runner Runner, tools ToolHost, b Budgets) *Generator {
	t.Helper()
	model, prompt := provenanceParts(t)
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
func fixture(t *testing.T) (inference.ContextPack, inference.EvidenceAnchor, []inference.EvidenceAnchor) {
	t.Helper()
	initial := anchor(t, "initial", 0)
	additions := []inference.EvidenceAnchor{graphAnchor(t), anchorInSection(t, "open", 20, "section-initial"), graphNeighborAnchor(t)}
	return mustPack(t, []inference.EvidenceAnchor{initial}, fixedTime.Add(time.Hour)), initial, additions
}

func graphAnchor(t *testing.T) inference.EvidenceAnchor {
	t.Helper()
	anchor, err := inference.NewGraphAnchor(graph.Path{Nodes: []graph.Node{{ID: "node-a", Kind: "entity"}}})
	if err != nil {
		t.Fatal(err)
	}

	return anchor
}

func graphNeighborAnchor(t *testing.T) inference.EvidenceAnchor {
	t.Helper()
	anchor, err := inference.NewGraphAnchor(graph.Path{
		Nodes: []graph.Node{{ID: "node-a", Kind: "entity"}, {ID: "node-b", Kind: "entity"}},
		Edges: []graph.Edge{{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}
func anchor(t *testing.T, quote string, offset int64) inference.EvidenceAnchor {
	return anchorInSection(t, quote, offset, shoal.ID("section-"+quote))
}

func anchorInSection(t *testing.T, quote string, offset int64, sectionID shoal.ID) inference.EvidenceAnchor {
	t.Helper()
	a, err := inference.NewDocumentAnchor(document.Citation{
		DocumentID: "document", RevisionID: "revision", SectionID: sectionID, SpanID: shoal.ID("span-" + quote),
		Range: document.SourceRange{Start: document.SourcePosition{Offset: offset}, End: document.SourcePosition{Offset: offset + int64(len(quote))}},
	}, quote)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func mustPack(t *testing.T, anchors []inference.EvidenceAnchor, expiry time.Time) inference.ContextPack {
	t.Helper()
	snapshot, _ := inference.NewSnapshotPin("snapshot", fixedTime.Add(-time.Hour))
	auth, _ := inference.NewAuthPin("auth", expiry)
	pack, err := inference.NewContextPack("answer the question", anchors, nil, snapshot, auth, shoal.Metadata{"safe": "metadata"})
	if err != nil {
		t.Fatal(err)
	}
	return pack
}
func packWithExpiry(t *testing.T, expiry time.Time) inference.ContextPack {
	t.Helper()
	return mustPack(t, []inference.EvidenceAnchor{anchor(t, "initial", 0)}, expiry)
}
func provenanceParts(t *testing.T) (inference.ModelProvenance, inference.PromptProvenance) {
	t.Helper()
	model, err := inference.NewModelProvenance("fake-provider", "fake-model", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := inference.NewPromptProvenance("agent", "v1", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return model, prompt
}
func resultFor(t *testing.T, pack inference.ContextPack, evidence inference.EvidenceAnchor) inference.InferenceResult {
	t.Helper()
	model, prompt := provenanceParts(t)
	value, _ := ontology.NewStringValue("grounded")
	claim, err := inference.NewClaim("subject", "predicate", value, 1, []shoal.ID{evidence.ID()}, inference.ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inference.NewInferenceResult(pack, []inference.Claim{claim}, nil, fixedTime, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func mustRetrieve(t *testing.T, id shoal.ID, query string, limit int) Action {
	return mustRetrieveQueryUsage(t, id, query, limit, Usage{})
}
func mustRetrieveUsage(t *testing.T, id shoal.ID, limit int, usage Usage) Action {
	return mustRetrieveQueryUsage(t, id, "query", limit, usage)
}
func mustRetrieveQueryUsage(t *testing.T, id shoal.ID, query string, limit int, usage Usage) Action {
	t.Helper()
	req, err := NewRetrieveRequest(query, limit)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewRetrieveAction(id, req, usage)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func mustOpen(t *testing.T, id, documentID, sectionID shoal.ID) Action {
	return mustOpenRevision(t, id, documentID, "revision", sectionID)
}

func mustOpenRevision(t *testing.T, id, documentID, revisionID, sectionID shoal.ID) Action {
	t.Helper()
	req, err := NewOpenSectionRequest(documentID, revisionID, sectionID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewOpenSectionAction(id, req, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func mustNeighbors(t *testing.T, id, nodeID shoal.ID, hops, fanout int) Action {
	t.Helper()
	req, err := NewNeighborsRequest(nodeID, hops, fanout)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewNeighborsAction(id, req, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
