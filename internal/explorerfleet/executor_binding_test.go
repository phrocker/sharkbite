// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// bindingHarness drives the executor binding (#391) through the real durable
// dispatch and registry stores: every claim, extension, report and completion
// below is encoded, validated and compare-and-set by the engine, and a rebind
// is a real re-registration.
type bindingHarness struct {
	t          *testing.T
	clock      *time.Time
	authority  *auth.Authority
	registry   *fleet.Service
	dispatch   *fleet.DispatchService
	store      *DispatchStore
	registrar  context.Context
	generation int64
	keys       int
}

func newBindingHarness(t *testing.T, refs ...string) *bindingHarness {
	t.Helper()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := &now
	read := func() time.Time { return *clock }
	runtime := openFleetDispatchRuntime(t, t.TempDir())
	t.Cleanup(func() { _ = runtime.Close() })
	authority, err := auth.NewAuthorityWithClock(read)
	if err != nil {
		t.Fatal(err)
	}
	executors := integratedExecutors{}
	for _, ref := range refs {
		executors[ref] = &integratedExecutor{}
	}
	registryStore, err := NewStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := fleet.NewService(fleet.Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: integratedLifecycleRecorder{}, Snapshots: integratedSnapshot{now},
		Executors: executors, Clock: read,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fleet.NewDispatchService(fleet.DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: integratedActionRecorder{}, Events: integratedEvents{},
		Clock: read,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &bindingHarness{
		t: t, clock: clock, authority: authority, registry: registry,
		dispatch: dispatch, store: store,
	}
	h.registrar = h.bind(integratedDecision(t, now))
	return h
}

func (h *bindingHarness) now() time.Time { return *h.clock }

func (h *bindingHarness) advance(by time.Duration) { *h.clock = h.clock.Add(by) }

func (h *bindingHarness) bind(decision auth.Decision) context.Context {
	h.t.Helper()
	ctx, err := h.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		h.t.Fatal(err)
	}
	return ctx
}

func (h *bindingHarness) context() fleet.RequestContext {
	return fleet.RequestContext{
		RequestID: "request", CorrelationID: "correlation",
		ReasonCode: "operator_request", Deadline: h.now().Add(time.Hour),
	}
}

// register registers, or re-registers, the agent under ref.
func (h *bindingHarness) register(ref string) fleet.Descriptor {
	h.t.Helper()
	h.keys++
	descriptor, err := h.registry.Register(h.registrar, fleet.RegisterRequest{
		Context:            h.context(),
		RegistrationKey:    shoal.ID(fmt.Sprintf("register-%d", h.keys)),
		ExpectedGeneration: h.generation,
		Spec: fleet.Spec{
			ID: "agent", AuthorizationDomain: []byte("domain"),
			Scopes: []fleet.Scope{{
				SourceID: []byte("source"), PolicyID: []byte("policy"),
			}},
			ExecutorRef: ref, LeaseExpiresAt: h.now().Add(time.Hour),
			Capabilities: []fleet.Capability{{
				Name: "search", Actions: []fleet.Action{{
					Name: "query",
					InputSchema: json.RawMessage(
						`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`),
					OutputSchema: json.RawMessage(
						`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
				}},
			}},
		},
	})
	if err != nil {
		h.t.Fatalf("register under %d-byte ref: %v", len(ref), err)
	}
	h.generation = descriptor.Generation
	return descriptor
}

func (h *bindingHarness) enqueue(
	who context.Context, descriptor fleet.Descriptor, id string,
) fleet.ActionRecord {
	h.t.Helper()
	request := integratedEnqueue(
		h.now(), descriptor, []byte(id), []byte(id+"-key"))
	queued, err := h.dispatch.Enqueue(who, request)
	if err != nil {
		h.t.Fatal(err)
	}
	return queued
}

// worker is a principal bound to binding, as the executor mint will issue.
func (h *bindingHarness) worker(name, binding string) context.Context {
	h.t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(name), Actor: shoal.ID(name + "-actor"),
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationExecute},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: h.now().Add(2 * time.Hour),
		RequestID:             "request", CorrelationID: "correlation",
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: "executor-ceiling",
		ExecutorBinding:        binding,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.bind(decision)
}

func (h *bindingHarness) stored(id []byte) fleet.ActionRecord {
	h.t.Helper()
	record, err := h.store.GetAction(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return record
}

func (h *bindingHarness) claim(
	who context.Context, id []byte, claimID string, lease time.Duration,
) (fleet.ActionRecord, error) {
	return h.dispatch.Claim(who, fleet.ClaimRequest{
		ID: id, ExpectedVersion: h.stored(id).Version,
		ClaimID: []byte(claimID), Lease: lease, Context: h.context(),
	})
}

func (h *bindingHarness) pull(who context.Context) int {
	h.t.Helper()
	page, err := h.dispatch.Pull(who, fleet.PullActionsRequest{
		Limit: 10, Context: h.context(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return len(page.Actions)
}

func requireBindingNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("%s = %v, want not-found", what, err)
	}
}

// TestTheExecutorBindingAgainstTheDurableStore: claim under X, rebind to Y
// by re-registering, and every phase of the rule through the engine — the
// holder reports and completes, cannot renew, and loses the work it had not
// claimed to Y's worker. Then back to X, so a retained holder's ref is read
// back from an encoded record.
func TestTheExecutorBindingAgainstTheDurableStore(t *testing.T) {
	h := newBindingHarness(t, "exec", "exec-y")
	descriptor := h.register("exec")
	first := h.enqueue(h.registrar, descriptor, "first")
	second := h.enqueue(h.registrar, descriptor, "second")

	x, xAsY := h.worker("x", "exec"), h.worker("x", "exec-y")
	y := h.worker("y", "exec-y")
	if got := h.pull(x); got != 2 {
		t.Fatalf("x pulled %d, want 2", got)
	}
	if got := h.pull(y); got != 0 {
		t.Fatalf("y pulled %d before any rebind, want 0", got)
	}
	claimed, err := h.claim(x, first.ID, "x-first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.stored(first.ID).ClaimExecutorRef; got != "exec" {
		t.Fatalf("stored ClaimExecutorRef = %q, want exec", got)
	}

	h.register("exec-y")

	extend := func(who context.Context) error {
		_, err := h.dispatch.ExtendClaim(who, fleet.ExtendRequest{
			ID: first.ID, ExpectedVersion: h.stored(first.ID).Version,
			ClaimID: claimed.ClaimID, Lease: 2 * time.Minute,
			Context: h.context(),
		})
		return err
	}
	requireBindingNotFound(t, "extend after a rebind", extend(x))
	requireBindingNotFound(t, "extend under the new ref", extend(xAsY))
	_, err = h.dispatch.ReportAmbiguity(xAsY, fleet.AmbiguityRequest{
		ID: first.ID, ClaimFence: claimed.ClaimFence,
		Outcome: fleet.AmbiguityOutcomeUnknown, Context: h.context(),
	})
	requireBindingNotFound(t, "a report under the new ref", err)
	if _, err := h.dispatch.ReportAmbiguity(x, fleet.AmbiguityRequest{
		ID: first.ID, ClaimFence: claimed.ClaimFence,
		Outcome: fleet.AmbiguityOutcomeUnknown, Context: h.context(),
	}); err != nil {
		t.Fatalf("the holder could not report after a rebind: %v", err)
	}
	done, err := h.dispatch.CompleteClaim(x, fleet.CompletionRequest{
		ID: first.ID, ExpectedVersion: h.stored(first.ID).Version,
		ClaimFence: claimed.ClaimFence, ClaimID: claimed.ClaimID,
		Result:  fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: h.context(),
	})
	if err != nil || done.State != fleet.DispatchSucceeded {
		t.Fatalf("the holder could not complete after a rebind: %v %v",
			done.State, err)
	}

	// The unclaimed work belongs to Y now.
	if got := h.pull(x); got != 0 {
		t.Fatalf("x pulled %d after the rebind, want 0", got)
	}
	_, err = h.claim(x, second.ID, "x-second", time.Minute)
	requireBindingNotFound(t, "a claim by the old ref's worker", err)
	if got := h.pull(y); got != 1 {
		t.Fatalf("y pulled %d, want 1", got)
	}
	taken, err := h.claim(y, second.ID, "y-second", time.Second)
	if err != nil {
		t.Fatalf("the new ref's worker could not claim: %v", err)
	}
	if got := h.stored(second.ID).ClaimExecutorRef; got != "exec-y" {
		t.Fatalf("stored ClaimExecutorRef = %q, want exec-y", got)
	}

	// Y's lease lapses, the descriptor goes back to X, and X re-claims. Y is
	// now a retained holder whose ref was encoded and decoded by the engine.
	h.advance(2 * time.Second)
	h.register("exec")
	if _, err := h.claim(x, second.ID, "x-second", time.Minute); err != nil {
		t.Fatalf("x could not re-claim after the rebind back: %v", err)
	}
	history := h.stored(second.ID).ClaimHistory
	if len(history) != 1 || history[0].ExecutorRef != "exec-y" {
		t.Fatalf("retained holder = %+v, want y under exec-y", history)
	}
	if _, err := h.dispatch.ReportAmbiguity(y, fleet.AmbiguityRequest{
		ID: second.ID, ClaimFence: taken.ClaimFence,
		Outcome: fleet.AmbiguityOutcomeUnknown, Context: h.context(),
	}); err != nil {
		t.Fatalf("the displaced holder could not report: %v", err)
	}
	yAsX := h.worker("y", "exec")
	_, err = h.dispatch.ReportAmbiguity(yAsX, fleet.AmbiguityRequest{
		ID: second.ID, ClaimFence: taken.ClaimFence,
		Outcome: fleet.AmbiguityEffectObserved, Context: h.context(),
	})
	requireBindingNotFound(t, "a displaced report under another ref", err)
}

// TestAMaximalHolderHistoryAgainstTheDurableStore is the brick probe where
// the brick lives: encodeAction validates every write, so a holder the bound
// does not cover would refuse a claim here even if applyClaim let it in.
// Every holder carries the old maximum chain and a maximum-length ref.
func TestAMaximalHolderHistoryAgainstTheDurableStore(t *testing.T) {
	maximalRef := strings.Repeat("r", executorref.MaxExecutorRefBytes)
	h := newBindingHarness(t, maximalRef)
	descriptor := h.register(maximalRef)
	chain := make([]shoal.ID, 0, 4)
	for index := 0; index < 4; index++ {
		chain = append(chain, shoal.ID(strings.Repeat(
			string(rune('a'+index)), 1024)))
	}
	delegated, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "delegated", Actor: "delegated-actor", OnBehalfOf: chain,
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke, auth.OperationDelegate,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: h.now().Add(2 * time.Hour),
		RequestID:             "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	who := h.bind(delegated)
	queued := h.enqueue(who, descriptor, "maximal")
	for index := 0; index <= fleet.MaxActionClaimHistory+1; index++ {
		if _, err := h.claim(who, queued.ID,
			fmt.Sprintf("claim-%d", index), time.Second); err != nil {
			t.Fatalf("claim %d of a maximal holder under a maximal ref: %v",
				index, err)
		}
		h.advance(2 * time.Second)
	}
	if got := len(h.stored(queued.ID).ClaimHistory); got != fleet.MaxActionClaimHistory {
		t.Fatalf("history = %d, want %d", got, fleet.MaxActionClaimHistory)
	}
}
