// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleetevents

import (
	"context"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Approval lifecycle events (#653).
//
// The approval.* namespace is reserved on the public publish route (see
// isReservedLifecycleKind). On the trusted lifecycle route it is admitted
// only for an event the approval ledger confirms. An approval transition's
// outbox row exists if and only if that transition committed, and the event
// must be exactly the one that row derives, under exactly the operation and
// receipt it names. Holding the lifecycle capability is not enough. No
// caller-supplied field is evidence that a transition happened, so a
// principal holding event_publish, or code holding the capability, cannot
// publish "approval.approved" for a request nobody approved.

// ApprovalLedger confirms that a trusted approval publication is the event of
// a durable approval transition.
type ApprovalLedger interface {
	// VerifyApprovalEvent returns nil only if the approval named by the
	// event's ActionID has a committed transition whose derived publication
	// is exactly request, under operation, with exactly receipt.
	VerifyApprovalEvent(
		ctx context.Context, operation auth.Operation,
		request PublishRequest, receipt LifecycleReceipt,
	) error
}

// ApprovalVisibility reports whether the subscriber behind ctx may see the
// lifecycle events of one approval. It is asked at delivery, under each
// subscriber's own context, after the ordinary authorization join has
// admitted the event.
//
// The join alone is not the approval's concealment rule. subscription_create
// on a request's source and policy is held by every co-tenant of the scope.
// The approval routes show a held request only to its requester and to
// principals eligible to decide it, and answer everyone else as though it did
// not exist. A subscriber must not learn from the stream what Status would
// not tell it. Nil means no subscriber may: approval events are withheld from
// every envelope.
type ApprovalVisibility func(ctx context.Context, approvalID []byte) (bool, error)

// IsApprovalKind reports whether a kind is in the approval namespace.
func IsApprovalKind(kind string) bool {
	return strings.HasPrefix(kind, "approval.")
}

// approvalPublicationPermits is the approval half of
// lifecyclePublicationPermits: the operation each approval transition is made
// under. The requester requests, materializes and withdraws under dispatch.
// An approver decides under approve. An expiry or a stranding is written by
// whichever observed it: either party, or the host's system observer under
// approval_sweep (#655), which publishes those two kinds and nothing else.
// An unlisted approval kind is admitted by no operation.
func approvalPublicationPermits(kind string, operation auth.Operation) bool {
	switch kind {
	case "approval.requested", "approval.materialized", "approval.withdrawn":
		return operation == auth.OperationDispatch
	case "approval.approved", "approval.refused":
		return operation == auth.OperationActionApprove
	case "approval.expired", "approval.stranded":
		return operation == auth.OperationDispatch ||
			operation == auth.OperationActionApprove ||
			operation == auth.OperationApprovalSweep
	default:
		return false
	}
}

func (s *Service) verifyApprovalPublication(
	ctx context.Context, operation auth.Operation,
	request PublishRequest, receipt LifecycleReceipt,
) error {
	if s.approvals == nil {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet approval events require an approval ledger")
	}
	if err := s.approvals.VerifyApprovalEvent(
		ctx, operation, request, receipt); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			return err
		}
		return shoal.WrapError(
			shoal.ErrorUnauthorized,
			"fleet approval event does not describe a durable transition", err)
	}
	return nil
}

// approvalDeliverable is the delivery-time approval gate, under the
// subscriber's context. Events outside the approval namespace pass.
func (s *Service) approvalDeliverable(ctx context.Context, event Event) (bool, error) {
	if !IsApprovalKind(event.Kind) {
		return true, nil
	}
	if s.approvalVisibility == nil {
		return false, nil
	}
	return s.approvalVisibility(ctx, event.ActionID)
}
