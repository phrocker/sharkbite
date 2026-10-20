// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package fleet

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// RecoveryClaimRequest identifies the live prepared claim whose material or
// Prepare result is requested. The claimant principal, ID, fence, and lease
// are all checked again on each call.
type RecoveryClaimRequest struct {
	ID         []byte
	ClaimID    []byte
	ClaimFence uint64
	Context    RequestContext
}

// PrepareClaimRequest commits the bound target request before any target
// send. Digests are compared server-side and never returned by public routes.
type PrepareClaimRequest struct {
	RecoveryClaimRequest
	Mode              RecoveryMode
	RequestDigest     [32]byte
	TargetScopeDigest [32]byte
}

// ClaimMaterial reveals executable material only to the live claimant of a
// claim atomically marked as using the prepared protocol. It is not a send
// grant; the worker must subsequently receive a successful Prepare answer.
func (s *DispatchService) ClaimMaterial(
	ctx context.Context, request RecoveryClaimRequest,
) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	current, _, _, err := s.currentRecoveryClaim(ctx, request)
	if err != nil {
		return ActionRecord{}, err
	}
	return cloneActionRecord(current), nil
}

// PrepareClaim records the first claim's exact bound-request commitment or
// compares a later live claimant's candidate against it. An exact retry after
// a lost response confirms the same durable value while that claimant holds
// a live lease. A later claim may proceed only for key or natural recovery.
func (s *DispatchService) PrepareClaim(
	ctx context.Context, request PrepareClaimRequest,
) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	current, operation, now, err := s.currentRecoveryClaim(ctx, request.RecoveryClaimRequest)
	if err != nil {
		return ActionRecord{}, err
	}
	if current.RecoveryCommitment != nil {
		committed := current.RecoveryCommitment
		if current.ClaimFence > committed.PreparedFence &&
			committed.Mode != RecoveryKey && committed.Mode != RecoveryNatural {
			return ActionRecord{}, &RecoveryRefusalError{RecoveryUnprotectedReplay, ErrActionConflict}
		}
		if committed.Version != RecoveryCommitmentVersion || committed.Mode != request.Mode {
			return ActionRecord{}, &RecoveryRefusalError{RecoveryModeDrift, ErrActionConflict}
		}
		if !bytes.Equal(committed.RequestDigest[:], request.RequestDigest[:]) {
			return ActionRecord{}, &RecoveryRefusalError{RecoveryRequestDrift, ErrActionConflict}
		}
		if !bytes.Equal(committed.TargetScopeDigest[:], request.TargetScopeDigest[:]) {
			return ActionRecord{}, &RecoveryRefusalError{RecoveryScopeDrift, ErrActionConflict}
		}
		return cloneActionRecord(current), nil
	}
	if current.ClaimFence != 1 {
		return ActionRecord{}, &RecoveryRefusalError{RecoveryMissingCommitment, ErrClaimLost}
	}
	proposed := RecoveryCommitment{
		Version: RecoveryCommitmentVersion, Mode: request.Mode,
		RequestDigest:     request.RequestDigest,
		TargetScopeDigest: request.TargetScopeDigest,
		PreparedFence:     current.ClaimFence, PreparedAt: now,
	}
	if err := proposed.validate(); err != nil {
		return ActionRecord{}, err
	}
	next := cloneActionRecord(current)
	next.Version++
	next.UpdatedAt = now
	next.RecoveryCommitment = &proposed
	next.TransitionRequestID = request.Context.RequestID
	next.TransitionCorrelationID = request.Context.CorrelationID
	if err := s.recorder.RecordAction(ctx, ActionAudit{
		Phase: "gateway_prepare", Operation: operation, Record: next,
	}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken("gateway_prepare", request.ID,
			request.ClaimID, next.Version),
		ExpectedVersion: current.Version, ExpectedFence: current.ClaimFence,
		Record: next,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	// A durable write is not a send grant if the lease was lost while audit or
	// storage was slow. Re-read the current claim and use a fresh clock before
	// acknowledging Prepare; the caller treats any failure as no SEND.
	verified, _, _, err := s.currentRecoveryClaim(ctx, request.RecoveryClaimRequest)
	if err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	if verified.Version < stored.Version || verified.RecoveryCommitment == nil ||
		*verified.RecoveryCommitment != proposed {
		return ActionRecord{}, errors.Join(ErrActionCommitted, ErrActionConflict)
	}
	return cloneActionRecord(verified), nil
}

func (s *DispatchService) currentRecoveryClaim(
	ctx context.Context, request RecoveryClaimRequest,
) (ActionRecord, auth.Operation, time.Time, error) {
	decision, now, err := s.beginClaimant(ctx, request.Context)
	if err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	if err := validateOpaque("action ID", request.ID, false); err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	if err := validateOpaque("claim ID", request.ClaimID, false); err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	if request.ClaimFence == 0 {
		return ActionRecord{}, "", time.Time{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "claim fence is required")
	}
	current, action, _, operation, err := s.authorizedClaimant(
		ctx, decision, request.ID, now, executorPhaseExtend, 0)
	if err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	if current.isAdmission() || !sameClaimantPrincipal(decision, current) ||
		!bytes.Equal(current.ClaimID, request.ClaimID) {
		return ActionRecord{}, "", time.Time{}, auth.ObjectNotFound()
	}
	// beginClaimant's clock reading predates resolver, registry, and store IO.
	// Resolve the complete descriptor binding again at a fresh instant; it
	// may have lapsed or changed while the first registry/store IO ran.
	now = s.clock().UTC()
	current, action, _, operation, err = s.authorizedClaimant(
		ctx, decision, request.ID, now, executorPhaseExtend, 0)
	if err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	if current.isAdmission() || !sameClaimantPrincipal(decision, current) ||
		!bytes.Equal(current.ClaimID, request.ClaimID) {
		return ActionRecord{}, "", time.Time{}, auth.ObjectNotFound()
	}
	// activeChainLapsing returns the current descriptor and its parent chain.
	// Check the returned lease bounds against the time after that final IO,
	// not merely the clock reading supplied to the lookup.
	chain, err := s.registry.activeChainLapsing(ctx, current.AgentID, now, false)
	if err != nil {
		return ActionRecord{}, "", time.Time{}, ErrClaimLost
	}
	now = s.clock().UTC()
	for _, descriptor := range chain {
		if !now.Before(descriptor.LeaseExpiresAt) {
			return ActionRecord{}, "", time.Time{}, ErrClaimLost
		}
	}
	if err := request.Context.validate(now); err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	resource := auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            current.SourceID, PolicyID: current.PolicyID,
		ObjectID: shoal.ID(current.ID),
	}
	if err := decision.AuthorizeObject(operation, resource, now); err != nil {
		return ActionRecord{}, "", time.Time{}, err
	}
	if len(decision.OnBehalfOf()) > 0 {
		if err := decision.AuthorizeObject(auth.OperationDelegate, resource, now); err != nil {
			return ActionRecord{}, "", time.Time{}, err
		}
	}
	if current.isAdmission() || !sameClaimantPrincipal(decision, current) ||
		!bytes.Equal(current.ClaimID, request.ClaimID) {
		return ActionRecord{}, "", time.Time{}, auth.ObjectNotFound()
	}
	if current.State != DispatchClaimed ||
		current.ClaimFence != request.ClaimFence || current.ClaimFence == 0 ||
		current.RecoveryProtocolVersion != RecoveryCommitmentVersion ||
		!current.EffectPossible ||
		(current.ClaimFence > 1 && (current.RecoveryCommitment == nil ||
			(current.RecoveryCommitment.Mode != RecoveryKey &&
				current.RecoveryCommitment.Mode != RecoveryNatural))) ||
		!now.Before(current.ClaimLeaseUntil) || !now.Before(current.Deadline) ||
		(!action.Effects.contains(EffectMutatesExternal) &&
			!action.Effects.contains(EffectEgressesContent)) {
		return ActionRecord{}, "", time.Time{}, ErrClaimLost
	}
	return current, operation, now, nil
}
