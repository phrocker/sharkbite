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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Requester withdrawal (#655), against the composition openService builds,
// with the real approval store as the outbox and the real event service
// behind the publisher.

func (h *approvalHarness) withdraw(
	who principal, id []byte,
) (fleet.ApprovalRecord, error) {
	h.t.Helper()
	withdrawer, ok := h.opened.approvals.(webapi.FleetApprovalWithdrawer)
	if !ok {
		h.t.Fatal("the composed approval provider does not serve withdrawal")
	}
	return withdrawer.Withdraw(h.as(who), fleet.ApprovalWithdrawRequest{
		ID: id, Context: h.context(h.now().Add(time.Minute)),
	})
}

// TestApprovalWithdrawalIsTheRequestersAndFinal: only the requester may
// withdraw, anyone else is answered as for a missing ID, and a withdrawn
// request is final across replay and restart. It publishes one event, by the
// requester, and never becomes work.
func TestApprovalWithdrawalIsTheRequestersAndFinal(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("withdraw-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)

	// Not the requester. A co-tenant holding dispatch on the scope is told
	// exactly what a missing ID tells it. An approver holds no dispatch, so
	// it is refused before any record is read, for an ID that exists and
	// one that does not alike.
	_, existing := h.withdraw(coTenant, request.id)
	_, missing := h.withdraw(coTenant, []byte("withdraw-missing"))
	if !shoal.IsErrorCode(existing, shoal.ErrorNotFound) ||
		existing.Error() != missing.Error() {
		t.Fatalf("co-tenant withdraw = %v, missing ID = %v", existing, missing)
	}
	_, existing = h.withdraw(approver, request.id)
	_, missing = h.withdraw(approver, []byte("withdraw-missing"))
	if !shoal.IsErrorCode(existing, shoal.ErrorUnauthorized) ||
		existing.Error() != missing.Error() {
		t.Fatalf("approver withdraw = %v, missing ID = %v", existing, missing)
	}
	// The requester's own principal under another client is not the
	// requester.
	otherClient := requester
	otherClient.client = "another-console"
	if _, err := h.withdraw(otherClient, request.id); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound) {
		t.Fatalf("requester under another client = %v", err)
	}
	if status := h.status(request.id); status.Approval.State != fleet.ApprovalPending {
		t.Fatalf("a refused withdrawal moved the request to %q", status.Approval.State)
	}

	withdrawn, err := h.withdraw(requester, request.id)
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if withdrawn.State != fleet.ApprovalWithdrawn || withdrawn.WithdrawnAt.IsZero() ||
		!withdrawn.WithdrawnAt.Equal(withdrawn.UpdatedAt) ||
		withdrawn.WithdrawalReason.Code != "test" || withdrawn.Verdict != "" {
		t.Fatalf("withdrawn record = %+v", withdrawn)
	}
	// A replay answers the same record and publishes nothing more.
	again, err := h.withdraw(requester, request.id)
	if err != nil || again.Version != withdrawn.Version ||
		!again.WithdrawnAt.Equal(withdrawn.WithdrawnAt) {
		t.Fatalf("withdraw replay = %+v, %v", again, err)
	}
	// Final: a re-request answers withdrawn, a decision is refused as
	// withdrawn, and the approver's queue omits it.
	h.mustRequest(requester, request, fleet.ApprovalWithdrawn)
	if _, err := h.decide(
		approver, receipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalWithdrawn) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("decide a withdrawn request = %v", err)
	}
	page, err := h.opened.approvals.Pending(h.as(approver), fleet.PendingApprovalsRequest{
		Limit: fleet.MaxApprovalListResults, Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Approvals {
		if string(record.ID) == string(request.id) {
			t.Fatal("a withdrawn request is still in the approver's queue")
		}
	}
	for _, who := range []principal{requester, approver} {
		status := h.statusAs(who, request.id)
		if status.State != fleet.ApprovalWithdrawn ||
			status.Condition != fleet.ApprovalConditionNone {
			t.Fatalf("status as %s = %q/%q", who.subject, status.State, status.Condition)
		}
	}
	h.assertNotWork(request)

	// The sweep leaves a final request alone.
	if total := h.sweep(); total.Expired+total.Stranded != 0 {
		t.Fatalf("the sweep moved a withdrawn request: %+v", total)
	}
	h.reopen()
	h.mustRequest(requester, request, fleet.ApprovalWithdrawn)
	h.assertNotWork(request)

	want := []publication{
		{fleet.ApprovalEventRequested, requester.subject, requester.actor},
		{fleet.ApprovalEventWithdrawn, requester.subject, requester.actor},
	}
	if got := probe.publications(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	for _, who := range []principal{requesterSubscriber, approverSubscriber} {
		kinds, events := h.approvalEventsOf(who, request.id)
		assertKinds(t, string(who.subject), kinds,
			fleet.ApprovalEventRequested, fleet.ApprovalEventWithdrawn)
		// The withdrawal's reason is the requester's, and is not on the
		// event.
		if event := eventOf(t, events, fleet.ApprovalEventWithdrawn); event.Reason.Code != "" ||
			!event.OccurredAt.Equal(withdrawn.WithdrawnAt) {
			t.Fatalf("withdrawn event = %+v", event)
		}
	}
	kinds, _ := h.approvalEventsOf(coTenant, request.id)
	assertKinds(t, "co-tenant", kinds)
}

// TestApprovalWithdrawalAfterTheWindowOrADecision: a decision already made
// wins, and the request stays exactly as the decision left it — an approved
// request still materializes. A request whose window has closed is expired,
// as a decision would find it.
func TestApprovalWithdrawalAfterTheWindowOrADecision(t *testing.T) {
	h, probe := newEventHarness(t)
	approved := h.held("withdraw-approved")
	refused := h.held("withdraw-refused")
	lapsed := h.held("withdraw-lapsed")
	approvedReceipt := h.mustRequest(requester, approved, fleet.ApprovalPending)
	refusedReceipt := h.mustRequest(requester, refused, fleet.ApprovalPending)
	h.mustRequest(requester, lapsed, fleet.ApprovalPending)
	if _, err := h.decide(approver, approvedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(approver, refusedReceipt, fleet.ApprovalVerdictRefuse); err != nil {
		t.Fatal(err)
	}
	for _, request := range []heldRequest{approved, refused} {
		if _, err := h.withdraw(requester, request.id); !errors.Is(
			err, fleet.ErrApprovalConflict) ||
			!shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("withdraw %s after the decision = %v", request.id, err)
		}
	}
	if status := h.status(approved.id); status.Approval.State != fleet.ApprovalApproved {
		t.Fatalf("a refused withdrawal moved the approval to %q", status.Approval.State)
	}
	h.mustRequest(requester, approved, fleet.ApprovalEnqueued)
	if _, err := h.withdraw(requester, approved.id); !errors.Is(
		err, fleet.ErrApprovalConflict) {
		t.Fatalf("withdraw materialized work = %v", err)
	}

	h.advance(fleet.DefaultApprovalWindow + time.Second)
	if _, err := h.withdraw(requester, lapsed.id); !errors.Is(
		err, fleet.ErrApprovalExpired) {
		t.Fatalf("withdraw after the window = %v", err)
	}
	stored := h.status(lapsed.id).Approval
	if stored.State != fleet.ApprovalExpired || !stored.WithdrawnAt.IsZero() {
		t.Fatalf("withdraw after the window stored %+v", stored)
	}
	var expiries []publication
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventExpired ||
			published.kind == fleet.ApprovalEventWithdrawn {
			expiries = append(expiries, published)
		}
	}
	want := []publication{{fleet.ApprovalEventExpired, requester.subject, requester.actor}}
	if fmt.Sprint(expiries) != fmt.Sprint(want) {
		t.Fatalf("published %v, want %v", expiries, want)
	}
}

// TestApprovalWithdrawalAndADecisionRaceAtomically: a decision committed
// between the withdrawal's read and its write wins, and the withdrawal is
// refused against it; a withdrawal committed between a decision's read and
// its write wins, and the decision is refused as withdrawn. Never both, and
// one event for the winner only.
func TestApprovalWithdrawalAndADecisionRaceAtomically(t *testing.T) {
	for _, first := range []fleet.ApprovalState{
		fleet.ApprovalApproved, fleet.ApprovalWithdrawn,
	} {
		t.Run(string(first), func(t *testing.T) {
			armed := &atomic.Bool{}
			var request heldRequest
			var receipt fleet.ApprovalReceipt
			var injected error
			// The hook runs as the losing call is about to write, and lands
			// the winner first, through the real services.
			loser := fleet.ApprovalWithdrawn
			if first == fleet.ApprovalWithdrawn {
				loser = fleet.ApprovalApproved
			}
			h := newHookedHarness(t, func(
				h *approvalHarness, mutation fleet.ApprovalMutation,
			) error {
				if mutation.Record.State != loser || !armed.CompareAndSwap(true, false) {
					return nil
				}
				if first == fleet.ApprovalApproved {
					_, injected = h.decide(approver, receipt, fleet.ApprovalVerdictApprove)
				} else {
					_, injected = h.withdraw(requester, request.id)
				}
				return nil
			})
			request = h.held("withdraw-race")
			receipt = h.mustRequest(requester, request, fleet.ApprovalPending)
			armed.Store(true)
			var err error
			if first == fleet.ApprovalApproved {
				_, err = h.withdraw(requester, request.id)
				if !errors.Is(err, fleet.ErrApprovalConflict) {
					t.Fatalf("withdrawal that lost to a decision = %v", err)
				}
			} else {
				_, err = h.decide(approver, receipt, fleet.ApprovalVerdictApprove)
				if !errors.Is(err, fleet.ErrApprovalWithdrawn) {
					t.Fatalf("decision that lost to a withdrawal = %v", err)
				}
			}
			if armed.Load() || injected != nil {
				t.Fatalf("the racing call did not land: armed %v, %v",
					armed.Load(), injected)
			}
			stored := h.status(request.id).Approval
			if stored.State != first || stored.Version != 2 {
				t.Fatalf("after the race the record is %q v%d, want %q v2",
					stored.State, stored.Version, first)
			}
			if first == fleet.ApprovalApproved {
				h.mustRequest(requester, request, fleet.ApprovalEnqueued)
			} else {
				h.assertNotWork(request)
			}
		})
	}
}

// TestApprovalWithdrawalEventLostIsPublishedByTheReplay: the withdrawal
// commits and its event does not publish. The requester is told so, as the
// unmarked 503 every approval transition uses, and the same withdrawal sent
// again publishes it, once.
func TestApprovalWithdrawalEventLostIsPublishedByTheReplay(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("withdraw-lost")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	probe.loseBefore(fleet.ApprovalEventWithdrawn, 1)
	if _, err := h.withdraw(requester, request.id); !errors.Is(
		err, fleet.ErrApprovalEventUnpublished) {
		t.Fatalf("withdraw with its event lost = %v", err)
	}
	if status := h.status(request.id); status.Approval.State != fleet.ApprovalWithdrawn {
		t.Fatalf("the withdrawal did not commit: %q", status.Approval.State)
	}
	if _, err := h.withdraw(requester, request.id); err != nil {
		t.Fatalf("withdraw replay = %v", err)
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after the replay", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventWithdrawn)
}

// TestApprovalWithdrawalAnswersForTheExpiryItWrote: a withdrawal that finds
// the window closed writes the expiry, as the requester. If that expiry's
// event does not publish, the withdrawal answers 503, and a retried
// withdrawal is the retry it asked for: it answers for that row until it
// publishes, then answers expired.
func TestApprovalWithdrawalAnswersForTheExpiryItWrote(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("withdraw-own-expiry")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	probe.loseBefore(fleet.ApprovalEventExpired, 2)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := h.withdraw(requester, request.id); !errors.Is(
			err, fleet.ErrApprovalEventUnpublished) {
			t.Fatalf("withdraw %d with the expiry event lost = %v", attempt, err)
		}
	}
	if _, err := h.withdraw(requester, request.id); !errors.Is(
		err, fleet.ErrApprovalExpired) {
		t.Fatalf("withdraw once the event published = %v", err)
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after the retries", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventExpired)
}

// TestApprovalWithdrawOverHTTP: the route is served by the composed provider
// and answers the record.
func TestApprovalWithdrawOverHTTP(t *testing.T) {
	h := newApprovalHarness(t)
	approvalHandler, err := webapi.NewFleetApprovalHandler(h.opened.approvals)
	if err != nil {
		t.Fatal(err)
	}
	request := h.held("http-withdraw")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	path := "/api/v1/fleet/approvals/" + encodeWireID(request.id) + "/withdraw"
	body := map[string]any{
		"request_id":  encodeWireID([]byte("http-withdraw-request")),
		"reason_code": "changed_my_mind",
		"deadline":    h.now().Add(time.Minute).Format(time.RFC3339Nano),
	}
	status, answer := serveApprovalJSON(t, approvalHandler, h.as(requester), path, body)
	if status != http.StatusOK || answer["state"] != "withdrawn" ||
		answer["withdrawn_at"] == nil {
		t.Fatalf("withdraw over HTTP = %d %v", status, answer)
	}
	status, answer = serveApprovalJSON(t, approvalHandler, h.as(approver), path, body)
	if status != http.StatusUnauthorized {
		t.Fatalf("approver withdraw over HTTP = %d %v", status, answer)
	}
	stored := h.status(request.id).Approval
	if stored.WithdrawalReason.Code != "changed_my_mind" {
		t.Fatalf("withdrawal reason = %+v", stored.WithdrawalReason)
	}
	if strings.Contains(fmt.Sprint(answer), "changed_my_mind") {
		t.Fatal("the withdrawal reason is served")
	}
}

func encodeWireID(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// serveApprovalJSON posts body to handler as ctx and decodes the answer.
func serveApprovalJSON(
	t *testing.T, handler http.Handler, ctx context.Context, path string, body any,
) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, path, bytes.NewReader(encoded)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s response is not JSON: %s", path, recorder.Body)
	}
	return recorder.Code, decoded
}
