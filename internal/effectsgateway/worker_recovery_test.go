// Licensed under the Apache Software Foundation, Version 2.0. See LICENSE and NOTICE.

package effectsgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

func lapsedPreparedCharge(t *testing.T, h *workerHarness, id string) {
	t.Helper()
	h.explorer.enqueue(id, "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.explorer.mu.Lock()
	record := h.explorer.records[id]
	record.action.State = dispatchapi.DispatchClaimed
	record.action.Version = 3
	record.action.ClaimFence = 1
	record.action.ClaimID = []byte("old-claim")
	record.action.EffectPossible = true
	record.action.RecoveryProtocolVersion = 1
	record.leaseUntil = workerEpoch.Add(-time.Second)
	input, key := append(json.RawMessage(nil), record.input...), record.key
	h.explorer.mu.Unlock()
	route, ok := h.cfg.Routes.Lookup("charge")
	if !ok {
		t.Fatal("missing charge route")
	}
	bound, err := h.cfg.Binder.Bind(route, input, key)
	if err != nil {
		t.Fatal(err)
	}
	h.explorer.mu.Lock()
	record.prepared = &RecoveryPrepareRequest{
		Mode: IdempotencyKey, RequestDigest: boundRequestDigest(bound),
		TargetScopeDigest: targetScopeDigest(h.cfg.TargetScope),
	}
	h.explorer.mu.Unlock()
}

func TestReplacementNoNewSendCannotEraseOriginalEffect(t *testing.T) {
	for _, condition := range []string{"credential_unavailable", "lease_elapsed_during_credential"} {
		t.Run(condition, func(t *testing.T) {
			h := newWorkerHarness(t, func(cfg *WorkerConfig) {
				if condition == "credential_unavailable" {
					cfg.TargetAuthorization = func() (string, string, error) {
						return "", "", errors.New("credential unavailable")
					}
				}
			})
			if condition == "lease_elapsed_during_credential" {
				h.credential = func() { h.clock.Advance(time.Minute) }
			}
			lapsedPreparedCharge(t, h, "prior-effect")
			h.start()
			eventually(t, "replacement no-send report", func() bool {
				h.explorer.mu.Lock()
				defer h.explorer.mu.Unlock()
				return len(h.explorer.reports) != 0
			})
			h.explorer.mu.Lock()
			got := h.explorer.records["prior-effect"].action
			completions := len(h.explorer.completions)
			h.explorer.mu.Unlock()
			if got.State != dispatchapi.DispatchClaimed || completions != 0 {
				t.Fatalf("replacement no-send terminalized action: %+v completions=%d", got, completions)
			}
			if _, requests, _ := h.target.stats(); requests != 0 {
				t.Fatalf("replacement contacted target %d times", requests)
			}
		})
	}
}

func TestReplacementTargetRejectionKeepsEarlierEffectUnresolved(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newWorkerHarness(t, nil)
			lapsedPreparedCharge(t, h, "prior-success")
			h.explorer.mu.Lock()
			record := h.explorer.records["prior-success"]
			input, key := append(json.RawMessage(nil), record.input...), record.key
			h.explorer.mu.Unlock()
			route, _ := h.cfg.Routes.Lookup("charge")
			bound, err := h.cfg.Binder.Bind(route, input, key)
			if err != nil {
				t.Fatal(err)
			}
			h.target.dropFirst = true
			original, err := http.NewRequest(bound.Method, bound.URL, bytes.NewReader(bound.Body))
			if err != nil {
				t.Fatal(err)
			}
			original.Header = bound.Header.Clone()
			response, err := h.target.server.Client().Do(original)
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("first effect answered instead of losing its response")
			}
			if effects, requests, _ := h.target.stats(); effects != 1 || requests != 1 {
				t.Fatalf("first effect/request counts = %d/%d", effects, requests)
			}
			h.target.mu.Lock()
			h.target.script = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(status)
				return true
			}
			h.target.mu.Unlock()
			h.start()
			eventually(t, "replacement rejection report", func() bool {
				h.explorer.mu.Lock()
				defer h.explorer.mu.Unlock()
				return len(h.explorer.reports) != 0
			})
			h.explorer.mu.Lock()
			got := h.explorer.records["prior-success"].action
			completions := len(h.explorer.completions)
			report := h.explorer.reports[0]
			h.explorer.mu.Unlock()
			if got.State != dispatchapi.DispatchClaimed || completions != 0 ||
				report.Outcome != dispatchapi.AmbiguityOutcomeUnknown {
				t.Fatalf("replacement rejection erased earlier effect: %+v completions=%d report=%+v",
					got, completions, report)
			}
			if effects, requests, _ := h.target.stats(); effects != 1 || requests != 2 {
				t.Fatalf("effect/request counts = %d/%d, want 1/2", effects, requests)
			}
		})
	}
}

func TestLeaseEndingDuringRetryCredentialLookupKeepsWrittenOutcomeHonest(t *testing.T) {
	h := newWorkerHarness(t, func(cfg *WorkerConfig) {
		cfg.Renew = false
		cfg.ClaimLease = 90 * time.Second
		cfg.OperationTimeout = 30 * time.Second
	})
	// Keep this test on the first claim. A later prepared claimant may safely
	// resend the same committed request after this lease expires.
	h.explorer.onClaim = func(n int, _ []byte, _ ClaimRequest) (Action, error, bool) {
		if n > 1 {
			return Action{}, conflict("claim", http.StatusConflict), true
		}
		return Action{}, nil, false
	}
	// Production refuses completion after the claim lease ends. The fake must
	// do the same or this test can mistake a late terminal write for safety.
	h.explorer.onComplete = func(_ int, _ []byte, _ Completion) (Action, error, bool) {
		record := h.explorer.records["retry-lease"]
		now := h.explorer.serverNow()
		if !now.Before(record.action.ClaimLeaseUntil) || !now.Before(record.action.Deadline) {
			return Action{}, conflict("complete", http.StatusConflict), true
		}
		return Action{}, nil, false
	}
	h.explorer.enqueue("retry-lease", "charge", chargeInput, workerEpoch.Add(time.Hour))
	credentialReads := 0
	h.credential = func() {
		credentialReads++
		if credentialReads == 2 {
			h.clock.Advance(90 * time.Second)
		}
	}
	h.target.mu.Lock()
	h.target.script = func(n int, w http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	h.target.mu.Unlock()
	h.start()
	eventually(t, "first written retryable response", func() bool {
		_, requests, _ := h.target.stats()
		return requests == 1
	})
	for i := 0; i < 5; i++ {
		settle()
		h.clock.Advance(time.Second)
	}
	eventually(t, "uncertain first claim to be reported", func() bool {
		h.explorer.mu.Lock()
		defer h.explorer.mu.Unlock()
		return len(h.explorer.reports) != 0
	})
	if _, requests, _ := h.target.stats(); requests != 1 {
		t.Fatalf("sent after credential lookup consumed lease: %d requests, credential reads=%d, now=%s, logs=%s",
			requests, credentialReads, h.clock.Now().Format(time.RFC3339Nano), h.logs.String())
	}
	h.explorer.mu.Lock()
	state := h.explorer.records["retry-lease"].action.State
	completions := len(h.explorer.completions)
	reports := append([]AmbiguityReport(nil), h.explorer.reports...)
	h.explorer.mu.Unlock()
	if state != dispatchapi.DispatchClaimed || completions > 1 || len(reports) != 1 ||
		reports[0].ClaimFence != 1 || reports[0].Outcome != dispatchapi.AmbiguityOutcomeUnknown {
		t.Fatalf("late completion resolved written uncertainty: state=%s completions=%d reports=%+v",
			state, completions, reports)
	}
}

func TestReplacementBindFailureKeepsEarlierPossibleEffectUnresolved(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.enqueue("rebind-drift", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.explorer.mu.Lock()
	record := h.explorer.records["rebind-drift"]
	record.action.State = dispatchapi.DispatchClaimed
	record.action.Version = 3
	record.action.ClaimFence = 1
	record.action.ClaimID = []byte("old-claim")
	record.action.EffectPossible = true
	record.action.RecoveryProtocolVersion = 1
	record.leaseUntil = workerEpoch.Add(-time.Second)
	record.input = json.RawMessage(`{"changed":"schema"}`)
	record.prepared = &RecoveryPrepareRequest{Mode: IdempotencyKey}
	h.explorer.mu.Unlock()
	h.start()
	eventually(t, "replacement refusal report", func() bool {
		h.explorer.mu.Lock()
		defer h.explorer.mu.Unlock()
		return len(h.explorer.reports) != 0
	})
	h.explorer.mu.Lock()
	got := h.explorer.records["rebind-drift"].action
	completions := len(h.explorer.completions)
	report := h.explorer.reports[0]
	h.explorer.mu.Unlock()
	if got.State != dispatchapi.DispatchClaimed || got.ClaimFence != 2 || completions != 0 ||
		report.Outcome != dispatchapi.AmbiguityRequestNotSent {
		t.Fatalf("drift erased unresolved effect: action=%+v completions=%d report=%+v", got, completions, report)
	}
	if effects, requests, _ := h.target.stats(); effects != 0 || requests != 0 {
		t.Fatalf("drift contacted target: %d effects, %d requests", effects, requests)
	}
	found := false
	for _, line := range h.logs.lines() {
		found = found || line["recovery_refusal"] == "bind_failed"
	}
	if !found {
		t.Fatal("replacement bind refusal has no closed operator reason")
	}
}

func TestRecoveryRefusalNeverContactsTarget(t *testing.T) {
	for _, refused := range []string{"material", "prepare"} {
		t.Run(refused, func(t *testing.T) {
			h := newWorkerHarness(t, nil)
			h.explorer.enqueue("recovery-refusal", "charge", chargeInput, workerEpoch.Add(time.Hour))
			h.explorer.gate = func(_ context.Context, op string) error {
				if op == refused {
					return conflict(op, http.StatusConflict)
				}
				return nil
			}
			h.start()
			eventually(t, "request_not_sent report", func() bool {
				h.explorer.mu.Lock()
				defer h.explorer.mu.Unlock()
				return len(h.explorer.reports) != 0
			})
			if effects, requests, _ := h.target.stats(); effects != 0 || requests != 0 {
				t.Fatalf("refused %s contacted target: effects=%d requests=%d", refused, effects, requests)
			}
			h.explorer.mu.Lock()
			report := h.explorer.reports[0]
			h.explorer.mu.Unlock()
			if report.Outcome != dispatchapi.AmbiguityRequestNotSent {
				t.Fatalf("refusal report = %q", report.Outcome)
			}
			if !h.logs.has("dispatch_error") {
				t.Fatal("refusal was invisible in the gateway log")
			}
		})
	}
}

func TestRecoveryNeverReclaimsPossiblySentOffer(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.enqueue("legacy", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.explorer.mu.Lock()
	record := h.explorer.records["legacy"]
	record.action.State = dispatchapi.DispatchClaimed
	record.action.ClaimFence = 1
	record.action.EffectPossible = true
	record.leaseUntil = workerEpoch.Add(-time.Second)
	h.explorer.mu.Unlock()
	h.start()
	eventually(t, "pull of legacy offer", func() bool {
		h.explorer.mu.Lock()
		defer h.explorer.mu.Unlock()
		return h.explorer.pulls > 0
	})
	h.explorer.mu.Lock()
	claims := len(h.explorer.claims)
	h.explorer.mu.Unlock()
	if claims != 0 {
		t.Fatalf("reclaimed legacy possibly sent action %d times", claims)
	}
	if _, requests, _ := h.target.stats(); requests != 0 {
		t.Fatal("replayed legacy action")
	}
}
