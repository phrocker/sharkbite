// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleetevents

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestPublicPublishReservesApprovalKinds pins the approval namespace on the
// public route (#451, #653). An approval event says a durable transition
// happened, and event_publish is not evidence of that. Every approval kind,
// including ones no build emits, is refused there.
//
// The refusal is asserted by its message, not merely by an error. A kind the
// event validator rejected for some other reason would pass a bare
// err != nil check without the reservation existing at all.
func TestPublicPublishReservesApprovalKinds(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	backend := &memoryBackend{}
	service := testService(
		t, "publisher", now, backend, &generationReader{generation: 7},
		&leaseValidator{}, &auditor{},
	)
	for _, kind := range []string{
		"approval.requested", "approval.approved", "approval.refused",
		"approval.expired", "approval.materialized", "approval.enqueued",
		"approval.anything_later",
	} {
		request := PublishRequest{
			Token: []byte("public-token"), RetryUntil: now.Add(time.Hour),
			Event: eventAt(0),
		}
		request.Event.Kind = kind
		_, err := service.Publish(context.Background(), request)
		if err == nil || !strings.Contains(
			err.Error(), "require trusted publication") {
			t.Fatalf("public publish of %q = %v, want the reservation", kind, err)
		}
	}
	if len(backend.events) != 0 {
		t.Fatalf("reserved approval events appended: %d", len(backend.events))
	}
	// The reservation is a prefix, not a substring. A kind that merely
	// mentions approval elsewhere is not reserved, and an ordinary kind still
	// publishes through the same service, so the refusals above are the
	// reservation and not a service that refuses everything.
	if isReservedLifecycleKind("gateway.approval_requested") {
		t.Fatal("a non-approval kind was reserved")
	}
	control := PublishRequest{
		Token: []byte("control-token"), RetryUntil: now.Add(time.Hour),
		Event: eventAt(0),
	}
	if _, err := service.Publish(context.Background(), control); err != nil {
		t.Fatalf("control publish = %v", err)
	}
}

// TestApprovalPublicationPermitsNameOneOperationPerTransition pins the
// trusted route's operation table for approval kinds: the requester's
// transitions under dispatch, the approver's under approve, and an expiry
// under whichever side observed it. No other operation, and no other
// approval kind, is admitted.
func TestApprovalPublicationPermitsNameOneOperationPerTransition(t *testing.T) {
	want := map[string][]auth.Operation{
		"approval.requested":    {auth.OperationDispatch},
		"approval.materialized": {auth.OperationDispatch},
		"approval.approved":     {auth.OperationActionApprove},
		"approval.refused":      {auth.OperationActionApprove},
		"approval.expired": {
			auth.OperationDispatch, auth.OperationActionApprove,
			auth.OperationApprovalSweep,
		},
		// #655: the requester withdraws; a stranding is observed by
		// either party or by the host's system observer, which publishes
		// expiry and stranding and nothing else.
		"approval.withdrawn": {auth.OperationDispatch},
		"approval.stranded": {
			auth.OperationDispatch, auth.OperationActionApprove,
			auth.OperationApprovalSweep,
		},
		"approval.enqueued":       nil,
		"approval.anything_later": nil,
	}
	every := []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke, auth.OperationExecute,
		auth.OperationActionApprove, auth.OperationEventPublish,
		auth.OperationSubscriptionCreate, auth.OperationApprovalSweep,
	}
	for kind, permitted := range want {
		for _, operation := range every {
			expected := false
			for _, candidate := range permitted {
				expected = expected || candidate == operation
			}
			if got := lifecyclePublicationPermits(kind, operation); got != expected {
				t.Errorf("lifecyclePublicationPermits(%q, %q) = %v, want %v",
					kind, operation, got, expected)
			}
		}
	}
}

type approvalLedgerProbe struct {
	err       error
	calls     int
	operation auth.Operation
	request   PublishRequest
	receipt   LifecycleReceipt
}

func (p *approvalLedgerProbe) VerifyApprovalEvent(
	_ context.Context, operation auth.Operation,
	request PublishRequest, receipt LifecycleReceipt,
) error {
	p.calls++
	p.operation, p.request, p.receipt = operation, request, receipt
	return p.err
}

// TestTrustedApprovalPublicationRequiresTheLedger is the half of #653 that
// the capability cannot provide. Host code holding the lifecycle capability
// may only publish an approval event the ledger confirms. No ledger refuses
// every approval kind, a refusing ledger refuses, and only a confirming one
// appends. The ledger is asked with the exact operation, request and receipt
// being published.
func TestTrustedApprovalPublicationRequiresTheLedger(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "requester", Actor: "requester", RequestID: "request",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationDispatch},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    7, AuthenticationExpires: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := auth.NewStaticResolverWithClock(
		decision, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{}
	capability := explorerfleetcap.New()
	build := func(ledger ApprovalLedger) *Service {
		service, err := NewWithLifecycleCapability(Config{
			Backend: backend, Resolver: resolver,
			GenerationReader: &generationReader{generation: 7},
			LeaseValidator:   &leaseValidator{}, Auditor: &auditor{},
			CursorKey: make([]byte, 32), Clock: func() time.Time { return now },
			Approvals: ledger,
		}, capability)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	request := PublishRequest{
		Token: []byte("approval-token"), RetryUntil: now.Add(time.Hour),
		Event: eventAt(0),
	}
	request.Event.Kind = "approval.requested"
	request.Event.CorrelationID = []byte("correlation")
	receipt := LifecycleReceipt{
		RequestID: "request", CorrelationID: []byte("correlation"),
		AuthorizationFingerprint: fingerprint,
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	publish := func(service *Service) error {
		_, err := service.PublishLifecycle(
			context.Background(), capability, auth.OperationDispatch,
			request, receipt)
		return err
	}

	if err := publish(build(nil)); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) || !strings.Contains(
		err.Error(), "approval ledger") {
		t.Fatalf("approval event without a ledger = %v", err)
	}
	refusing := &approvalLedgerProbe{err: errors.New("no such transition")}
	if err := publish(build(refusing)); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) || refusing.calls != 1 {
		t.Fatalf("approval event the ledger refused = %v (calls %d)",
			err, refusing.calls)
	}
	if len(backend.events) != 0 {
		t.Fatalf("an unconfirmed approval event was appended: %d",
			len(backend.events))
	}
	confirming := &approvalLedgerProbe{}
	if err := publish(build(confirming)); err != nil {
		t.Fatalf("confirmed approval event = %v", err)
	}
	if len(backend.events) != 1 || confirming.operation != auth.OperationDispatch ||
		!bytes.Equal(confirming.request.Token, request.Token) ||
		confirming.request.Event.Kind != request.Event.Kind ||
		confirming.receipt.RequestID != receipt.RequestID {
		t.Fatalf("ledger was asked %+v / %+v; events %d",
			confirming.request, confirming.receipt, len(backend.events))
	}
	// The ledger is the approval kinds' business only: an action event does
	// not consult it.
	actionLedger := &approvalLedgerProbe{err: errors.New("must not be asked")}
	request.Event.Kind = "action.enqueued"
	request.Token = []byte("action-token")
	if err := publish(build(actionLedger)); err != nil || actionLedger.calls != 0 {
		t.Fatalf("action event = %v, ledger calls %d", err, actionLedger.calls)
	}
}

// TestApprovalEventsAreDeliveredOnlyThroughTheApprovalRule pins the
// delivery-time half (#653). The authorization join admits every subscriber
// holding subscription_create on the scope, which every co-tenant does. An
// approval event additionally needs the approval's own concealment rule,
// asked under the subscriber's context. No rule configured delivers it to
// nobody, a false answer withholds it, and an error fails the pull rather
// than delivering. A non-approval event is unaffected.
func TestApprovalEventsAreDeliveredOnlyThroughTheApprovalRule(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	approval := eventAt(1)
	approval.Kind, approval.ActionID = "approval.requested", []byte("held-request")
	ordinary := eventAt(2)
	for _, probe := range []struct {
		name       string
		visibility ApprovalVisibility
		want       []string
		fails      bool
	}{
		{name: "no rule", want: []string{"agent.completed"}},
		{name: "rule refuses", want: []string{"agent.completed"},
			visibility: func(context.Context, []byte) (bool, error) {
				return false, nil
			}},
		{name: "rule admits",
			want: []string{"approval.requested", "agent.completed"},
			visibility: func(_ context.Context, id []byte) (bool, error) {
				return bytes.Equal(id, []byte("held-request")), nil
			}},
		{name: "rule fails", fails: true,
			visibility: func(context.Context, []byte) (bool, error) {
				return false, shoal.NewError(shoal.ErrorUnavailable, "store down")
			}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			backend := &memoryBackend{}
			service := testService(
				t, "subscriber", now, backend, &generationReader{generation: 7},
				&leaseValidator{}, &auditor{})
			service.approvalVisibility = probe.visibility
			subscription, err := service.Create(context.Background(), CreateRequest{
				Token: []byte("create"), RetryUntil: now.Add(time.Hour),
				AgentID: "agent", AgentGeneration: 1, TTL: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			backend.events = []Event{approval, ordinary}
			page, err := service.Pull(context.Background(), PullRequest{
				SubscriptionID: subscription.ID, Limit: 10,
			})
			if probe.fails {
				if err == nil {
					t.Fatal("a failing approval rule delivered the page")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, event := range page.Events {
				got = append(got, event.Kind)
			}
			if strings.Join(got, ",") != strings.Join(probe.want, ",") {
				t.Fatalf("delivered %v, want %v", got, probe.want)
			}
		})
	}
}
