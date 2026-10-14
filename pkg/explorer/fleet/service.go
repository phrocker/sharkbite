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
	"crypto/sha256"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Config struct {
	Store     Store
	Resolver  auth.Resolver
	Recorder  LifecycleRecorder
	Snapshots InteractionSnapshotProvider
	Executors ExecutorRegistry
	Clock     func() time.Time
	// AttestationTrust reports which executor refs the host has an
	// attestation trust root for. Optional: nil means none is configured, so
	// Register refuses every action that requires attestation.
	AttestationTrust AttestationTrust
}

type Service struct {
	store     Store
	resolver  auth.Resolver
	recorder  LifecycleRecorder
	snapshots InteractionSnapshotProvider
	executors ExecutorRegistry
	clock     func() time.Time
	trust     AttestationTrust
}

func NewService(config Config) (*Service, error) {
	if nilDependency(config.Store) || nilDependency(config.Resolver) ||
		nilDependency(config.Recorder) || nilDependency(config.Snapshots) ||
		nilDependency(config.Executors) || config.Clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet registry dependencies are required")
	}

	return &Service{
		store: config.Store, resolver: config.Resolver, recorder: config.Recorder,
		snapshots: config.Snapshots,
		executors: config.Executors, clock: config.Clock,
		trust: config.AttestationTrust,
	}, nil
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (s *Service) Register(ctx context.Context, request RegisterRequest) (Descriptor, error) {
	ctx, cancel := s.withRequestDeadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, auth.OperationAgentRegister, request.Context)
	if err != nil {
		return Descriptor{}, err
	}
	if err := validateGeneration(request.ExpectedGeneration); err != nil {
		return Descriptor{}, err
	}
	if err := shoal.ValidateRequiredID("registration key", request.RegistrationKey); err != nil {
		return Descriptor{}, err
	}
	if err := shoal.ValidateRequiredID("agent ID", request.Spec.ID); err != nil {
		return Descriptor{}, err
	}
	if replay, ok, err := s.registrationReplay(
		ctx, decision, now, request,
	); err != nil {
		return Descriptor{}, err
	} else if ok {
		// A replay is still an authenticated registry request, so it gets a
		// lifecycle receipt like every other one, over the identical stored
		// mutation. Under the original request ID this reconciles with the
		// original receipt, and a different asserted reason conflicts instead
		// of succeeding. Under a new request ID it records that request's own
		// assertion, attributed to its caller; the receipt that admitted the
		// generation is unchanged.
		if err := s.record(ctx, decision, request.Context,
			auth.OperationAgentRegister, replay.ID,
			registryMutationDigests(Mutation{
				RegistrationKey:    request.RegistrationKey,
				ExpectedGeneration: request.ExpectedGeneration,
				Descriptor:         replay,
			})); err != nil {
			return Descriptor{}, err
		}
		return replay, nil
	}
	spec, err := request.Spec.canonical(now)
	if err != nil {
		return Descriptor{}, err
	}
	if err := authorizeScopes(decision, auth.OperationAgentRegister, spec.ID,
		spec.AuthorizationDomain, spec.Scopes, now); err != nil {
		return Descriptor{}, err
	}
	executor, ok := s.executors.ResolveExecutor(spec.ExecutorRef)
	if !ok {
		return Descriptor{}, shoal.NewError(shoal.ErrorInvalidArgument, "executor reference is not registered by the host")
	}
	// Refuse a descriptor that could never run within its own declaration.
	// Catching it here rather than at invoke means an agent claiming an
	// a declaration that does not match its executor's binding never becomes
	// registered state that looks operable.
	if err := validateDeclaredEffects(spec.Capabilities,
		executorFloor(executor), executorCeiling(executor)); err != nil {
		return Descriptor{}, err
	}
	// A requirement nothing could ever satisfy would register an agent whose
	// attested actions can never be claimed. Refused here; the message names
	// no ref, verifier or digest.
	if err := s.validateAttestationTrust(spec); err != nil {
		return Descriptor{}, err
	}
	if spec.ParentID != "" {
		if err := authorizeScopes(decision, auth.OperationDelegate, spec.ID,
			spec.AuthorizationDomain, spec.Scopes, now); err != nil {
			return Descriptor{}, err
		}
		parentChain, err := s.activeChain(ctx, spec.ParentID, now)
		if err != nil {
			return Descriptor{}, err
		}
		parent := parentChain[0]
		if parent.Subject != decision.Subject() {
			return Descriptor{}, auth.ObjectNotFound()
		}
		if len(parentChain) >= MaxDelegationDepth {
			return Descriptor{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "agent delegation depth exceeds its bound")
		}
		if !bytes.Equal(parent.AuthorizationDomain, spec.AuthorizationDomain) ||
			!scopesSubset(spec.Scopes, parent.Scopes) ||
			!capabilitiesSubset(spec.Capabilities, parent.Capabilities) ||
			spec.LeaseExpiresAt.After(parent.LeaseExpiresAt) {
			return Descriptor{}, shoal.NewError(shoal.ErrorUnauthorized, "delegated agent exceeds its parent")
		}
	}
	if request.ExpectedGeneration > 0 {
		current, err := s.store.Get(ctx, spec.ID)
		if err != nil {
			return Descriptor{}, concealStoreRead(err)
		}
		if current.Descriptor.Subject != decision.Subject() {
			return Descriptor{}, auth.ObjectNotFound()
		}
		if descriptorExpired(current.Descriptor, now) {
			return Descriptor{}, auth.ObjectNotFound()
		}
		if err := authorizeDescriptor(
			decision, auth.OperationAgentRegister, current.Descriptor, now,
		); err != nil {
			return Descriptor{}, err
		}
		if !bytes.Equal(current.Descriptor.AuthorizationDomain, spec.AuthorizationDomain) ||
			!scopesSubset(spec.Scopes, current.Descriptor.Scopes) ||
			!capabilitiesSubset(spec.Capabilities, current.Descriptor.Capabilities) {
			return Descriptor{}, shoal.NewError(shoal.ErrorUnauthorized, "agent update widens authorization")
		}
		if current.Descriptor.ParentID != spec.ParentID {
			return Descriptor{}, shoal.NewError(shoal.ErrorUnauthorized, "agent parent migration is denied")
		}
	}
	descriptor := Descriptor{
		ID: spec.ID, Generation: request.ExpectedGeneration + 1,
		Subject: decision.Subject(), Actor: decision.Actor(), ParentID: spec.ParentID,
		AuthorizationDomain: spec.AuthorizationDomain, Scopes: spec.Scopes,
		ExecutorRef: spec.ExecutorRef, Capabilities: spec.Capabilities,
		LeaseExpiresAt: spec.LeaseExpiresAt, UpdatedAt: now.UTC(),
	}
	mutation := Mutation{RegistrationKey: request.RegistrationKey,
		ExpectedGeneration: request.ExpectedGeneration, Descriptor: descriptor}
	if err := s.record(ctx, decision, request.Context, auth.OperationAgentRegister,
		descriptor.ID, registryMutationDigests(mutation)); err != nil {
		return Descriptor{}, err
	}
	stored, err := s.store.Apply(ctx, mutation)
	if err != nil {
		return Descriptor{}, err
	}
	return cloneDescriptor(stored.Descriptor), nil
}

func (s *Service) registrationReplay(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	request RegisterRequest,
) (Descriptor, bool, error) {
	current, err := s.store.Get(ctx, request.Spec.ID)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return Descriptor{}, false, nil
		}
		return Descriptor{}, false, concealStoreRead(err)
	}
	keyDigest := sha256.Sum256([]byte(request.RegistrationKey))
	if current.Descriptor.Generation != request.ExpectedGeneration+1 ||
		current.RegistrationDigest != keyDigest {
		return Descriptor{}, false, nil
	}
	if current.Descriptor.Subject != decision.Subject() {
		return Descriptor{}, false, auth.ObjectNotFound()
	}
	spec, err := request.Spec.canonical(current.Descriptor.UpdatedAt)
	if err != nil {
		return Descriptor{}, false, shoal.NewError(
			shoal.ErrorConflict, "registration key replay is divergent")
	}
	candidate := Descriptor{
		ID: spec.ID, Generation: request.ExpectedGeneration + 1,
		Subject: current.Descriptor.Subject, Actor: current.Descriptor.Actor,
		ParentID: spec.ParentID, AuthorizationDomain: spec.AuthorizationDomain,
		Scopes: spec.Scopes, ExecutorRef: spec.ExecutorRef,
		Capabilities: spec.Capabilities, LeaseExpiresAt: spec.LeaseExpiresAt,
		UpdatedAt: current.Descriptor.UpdatedAt,
	}
	if descriptorDigest(candidate) != descriptorDigest(current.Descriptor) {
		return Descriptor{}, false, shoal.NewError(
			shoal.ErrorConflict, "registration key replay is divergent")
	}
	if err := authorizeScopes(
		decision, auth.OperationAgentRegister, spec.ID,
		spec.AuthorizationDomain, spec.Scopes, now,
	); err != nil {
		return Descriptor{}, false, err
	}
	return cloneDescriptor(current.Descriptor), true, nil
}

func (s *Service) Heartbeat(ctx context.Context, request HeartbeatRequest) (Descriptor, error) {
	ctx, cancel := s.withRequestDeadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, auth.OperationAgentHeartbeat, request.Context)
	if err != nil {
		return Descriptor{}, err
	}
	if err := validateMutationIdentity(request.ID, request.RegistrationKey, request.ExpectedGeneration); err != nil {
		return Descriptor{}, err
	}
	currentDescriptor, err := s.active(ctx, request.ID, now)
	if err != nil {
		return Descriptor{}, concealStoreRead(err)
	}
	current := Stored{Descriptor: currentDescriptor}
	if current.Descriptor.Subject != decision.Subject() {
		return Descriptor{}, auth.ObjectNotFound()
	}
	if err := authorizeDescriptor(decision, auth.OperationAgentHeartbeat, current.Descriptor, now); err != nil {
		return Descriptor{}, err
	}
	if request.LeaseExpiresAt.Location() != time.UTC || !now.Before(request.LeaseExpiresAt) ||
		request.LeaseExpiresAt.Sub(now) > MaxLease {
		return Descriptor{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent lease is outside its bound")
	}
	if current.Descriptor.ParentID != "" {
		parent, err := s.active(ctx, current.Descriptor.ParentID, now)
		if err != nil {
			return Descriptor{}, err
		}
		if request.LeaseExpiresAt.After(parent.LeaseExpiresAt) {
			return Descriptor{}, shoal.NewError(shoal.ErrorUnauthorized, "delegated agent lease exceeds its parent")
		}
	}
	next := cloneDescriptor(current.Descriptor)
	next.Generation = request.ExpectedGeneration + 1
	next.Actor = decision.Actor()
	next.LeaseExpiresAt = request.LeaseExpiresAt
	next.UpdatedAt = now.UTC()
	mutation := Mutation{RegistrationKey: request.RegistrationKey,
		ExpectedGeneration: request.ExpectedGeneration, Descriptor: next}
	if err := s.record(ctx, decision, request.Context, auth.OperationAgentHeartbeat,
		request.ID, registryMutationDigests(mutation)); err != nil {
		return Descriptor{}, err
	}
	stored, err := s.store.Apply(ctx, mutation)
	if err != nil {
		return Descriptor{}, err
	}
	return cloneDescriptor(stored.Descriptor), nil
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) (Descriptor, error) {
	ctx, cancel := s.withRequestDeadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, auth.OperationAgentRevoke, request.Context)
	if err != nil {
		return Descriptor{}, err
	}
	if err := validateMutationIdentity(request.ID, request.RegistrationKey, request.ExpectedGeneration); err != nil {
		return Descriptor{}, err
	}
	current, err := s.store.Get(ctx, request.ID)
	if err != nil {
		return Descriptor{}, concealStoreRead(err)
	}
	if current.Descriptor.Subject != decision.Subject() {
		return Descriptor{}, auth.ObjectNotFound()
	}
	if err := authorizeDescriptor(decision, auth.OperationAgentRevoke, current.Descriptor, now); err != nil {
		return Descriptor{}, err
	}
	next := cloneDescriptor(current.Descriptor)
	keyDigest := sha256.Sum256([]byte(request.RegistrationKey))
	replay := current.Descriptor.Generation == request.ExpectedGeneration+1 &&
		!current.Descriptor.RevokedAt.IsZero() &&
		current.RegistrationDigest == keyDigest
	if !replay {
		next.Generation = request.ExpectedGeneration + 1
		next.Actor = decision.Actor()
		next.RevokedAt = now.UTC()
		next.UpdatedAt = now.UTC()
	}
	mutation := Mutation{RegistrationKey: request.RegistrationKey,
		ExpectedGeneration: request.ExpectedGeneration, Descriptor: next}
	if err := s.record(ctx, decision, request.Context, auth.OperationAgentRevoke,
		request.ID, registryMutationDigests(mutation)); err != nil {
		return Descriptor{}, err
	}
	stored, err := s.store.Apply(ctx, mutation)
	if err != nil {
		return Descriptor{}, err
	}
	return cloneDescriptor(stored.Descriptor), nil
}

func (s *Service) Resolve(ctx context.Context, request ResolveRequest) (Resolved, error) {
	ctx, cancel := s.withRequestDeadline(ctx, request.Context)
	defer cancel()
	if err := shoal.ValidateRequiredID("agent ID", request.ID); err != nil {
		return Resolved{}, err
	}
	decision, now, err := s.begin(ctx, auth.OperationAgentResolve, request.Context)
	if err != nil {
		return Resolved{}, err
	}
	descriptor, err := s.authorizedActive(ctx, decision, request.ID, now)
	if err != nil {
		return Resolved{}, err
	}
	if !resolvableUnderBinding(decision, descriptor) {
		return Resolved{}, auth.ObjectNotFound()
	}
	if err := s.record(ctx, decision, request.Context, auth.OperationAgentResolve,
		request.ID, registryDigests{}); err != nil {
		return Resolved{}, err
	}
	executor, ok := s.executors.ResolveExecutor(descriptor.ExecutorRef)
	if !ok {
		return Resolved{}, shoal.NewError(shoal.ErrorUnavailable, "agent executor is unavailable")
	}
	return Resolved{Descriptor: cloneDescriptor(descriptor), Executor: executor}, nil
}

func (s *Service) List(ctx context.Context, request ListRequest) (ListPage, error) {
	ctx, cancel := s.withRequestDeadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, auth.OperationAgentResolve, request.Context)
	if err != nil {
		return ListPage{}, err
	}
	if request.Limit < 0 || request.Limit > MaxListResults {
		return ListPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "agent list limit is outside its bound")
	}
	limit := request.Limit
	if limit == 0 {
		limit = MaxListResults
	}
	if _, err := decision.IntersectSourceIDs(auth.OperationAgentResolve,
		decision.AuthorizationDomain(), request.SourceIDs, now); err != nil {
		return ListPage{}, err
	}
	if _, err := decision.IntersectPolicyIDs(auth.OperationAgentResolve,
		decision.AuthorizationDomain(), request.PolicyIDs, now); err != nil {
		return ListPage{}, err
	}
	if err := s.record(ctx, decision, request.Context, auth.OperationAgentResolve,
		"fleet-list", registryDigests{}); err != nil {
		return ListPage{}, err
	}
	result := ListPage{
		Descriptors: make([]Descriptor, 0, limit),
	}
	cursor := append([]byte(nil), request.Cursor...)
	var continuation []byte
	const maxDiscoveryScans = 1024
	for scanned := 0; scanned < maxDiscoveryScans; scanned++ {
		stored, err := s.store.List(ctx, cursor, 1)
		if err != nil {
			return ListPage{}, err
		}
		if len(stored.Entries) == 0 {
			return result, nil
		}
		item := stored.Entries[0]
		descriptor := item.Descriptor
		visible := matchesFilter(descriptor, request) && !descriptorExpired(descriptor, now) &&
			authorizeDescriptor(decision, auth.OperationAgentResolve, descriptor, now) == nil &&
			resolvableUnderBinding(decision, descriptor)
		if visible && descriptor.ParentID != "" {
			if _, parentErr := s.authorizedActive(
				ctx, decision, descriptor.ParentID, now,
			); parentErr != nil {
				visible = false
			}
		}
		if visible {
			if len(result.Descriptors) == limit {
				result.Next = continuation
				return result, nil
			}
			result.Descriptors = append(result.Descriptors, cloneDescriptor(descriptor))
			continuation = append([]byte(nil), stored.Next...)
		}
		if len(stored.Next) == 0 {
			return result, nil
		}
		cursor = stored.Next
	}
	result.Next = append([]byte(nil), cursor...)
	return result, nil
}

// ValidateDelivery is the narrow structural adapter consumed by durable event
// delivery. It resolves a fresh decision from the bound request context,
// rechecks agent_resolve against every descriptor scope, verifies the complete
// parent delegation chain is still active and narrowing, and pins the exact
// positive descriptor generation. It deliberately accepts no caller-supplied
// Decision and returns no executor or descriptor, preventing a dispatcher from
// transferring trusted session state or using stale authorization material.
func (s *Service) ValidateDelivery(
	ctx context.Context,
	agentID shoal.ID,
	expectedGeneration int64,
) error {
	return s.validateDelivery(ctx, agentID, expectedGeneration, true)
}

// ValidateCurrentDelivery is the non-generation-pinned counterpart used by
// integrations whose durable record intentionally follows the current agent
// generation. Every registry transition is CAS-protected and non-widening;
// this method still rechecks the current lease, revocation, authorization, and
// complete parent chain on every call.
func (s *Service) ValidateCurrentDelivery(
	ctx context.Context,
	agentID shoal.ID,
) error {
	return s.validateDelivery(ctx, agentID, 0, false)
}

func (s *Service) validateDelivery(
	ctx context.Context,
	agentID shoal.ID,
	expectedGeneration int64,
	pinned bool,
) error {
	if err := shoal.ValidateRequiredID("agent ID", agentID); err != nil {
		return err
	}
	if pinned && expectedGeneration <= 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"expected agent generation must be positive",
		)
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	descriptor, err := s.authorizedActive(ctx, decision, agentID, now)
	if err != nil {
		return err
	}
	if !resolvableUnderBinding(decision, descriptor) {
		return auth.ObjectNotFound()
	}
	if pinned && descriptor.Generation != expectedGeneration {
		return auth.ObjectNotFound()
	}
	return nil
}

// resolvableUnderBinding confines an action-execution decision's agent_resolve
// to the one descriptor its executor binding names (#391).
//
// A worker resolves its own descriptor to learn what it serves, and nothing
// else: it holds no registrar credential and never heartbeats, because it
// cannot truthfully assert a descriptor's liveness. So the role may resolve,
// and only where the descriptor's executor reference is its binding. Applied
// to the descriptor being resolved or listed, not to its parents, which are
// authorized as before. Every other decision is unaffected.
//
// Keyed on the role rather than on a non-empty binding, so a role without a
// binding — which auth refuses to mint — would resolve nothing rather than
// everything.
func resolvableUnderBinding(decision auth.Decision, descriptor Descriptor) bool {
	if decision.ServiceRole() != auth.ServiceRoleActionExecution {
		return true
	}
	binding := decision.ExecutorBinding()
	return binding != "" && descriptor.ExecutorRef == binding
}

func (s *Service) begin(ctx context.Context, operation auth.Operation, request RequestContext) (auth.Decision, time.Time, error) {
	now := s.clock().UTC()
	if err := request.validate(now); err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	// The registry's lifecycle receipt records the caller's reason as a
	// caller-asserted reason. Refuse a shape it cannot record (notably an
	// "atpl-apply" detail that is not a policy digest) here, before replay
	// detection, the receipt, or any registry write. This is registry-only:
	// the shared RequestContext.validate also serves dispatch and admission.
	if _, err := CallerAssertedRegistryReason(
		request.ReasonCode, request.ReasonDetail,
	); err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	if request.RequestID != decision.RequestID() ||
		request.CorrelationID != decision.CorrelationID() {
		return auth.Decision{}, time.Time{}, shoal.NewError(shoal.ErrorUnauthorized, "request identity does not match authentication")
	}
	if err := decision.Authorize(operation, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
	}, now); err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	return decision, now, nil
}

func (s *Service) withRequestDeadline(
	ctx context.Context,
	request RequestContext,
) (context.Context, context.CancelFunc) {
	now := s.clock().UTC()
	if request.Deadline.IsZero() || !now.Before(request.Deadline) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled, func() {}
	}
	return context.WithTimeout(ctx, request.Deadline.Sub(now))
}

func (s *Service) authorizedActive(ctx context.Context, decision auth.Decision, id shoal.ID, now time.Time) (Descriptor, error) {
	chain, err := s.activeChain(ctx, id, now)
	if err != nil {
		return Descriptor{}, err
	}
	for _, descriptor := range chain {
		if err := authorizeDescriptor(
			decision, auth.OperationAgentResolve, descriptor, now,
		); err != nil {
			return Descriptor{}, auth.ObjectNotFound()
		}
	}
	return chain[0], nil
}

func (s *Service) active(
	ctx context.Context,
	id shoal.ID,
	now time.Time,
) (Descriptor, error) {
	chain, err := s.activeChain(ctx, id, now)
	if err != nil {
		return Descriptor{}, err
	}
	return chain[0], nil
}

func (s *Service) activeChain(
	ctx context.Context,
	id shoal.ID,
	now time.Time,
) ([]Descriptor, error) {
	return s.activeChainLapsing(ctx, id, now, false)
}

// activeChainLapsing is activeChain that, when allowLapsed, accepts a
// descriptor whose lease has lapsed. A revoked descriptor is refused either
// way. Only resolveActionBindingLapsing asks for it.
func (s *Service) activeChainLapsing(
	ctx context.Context,
	id shoal.ID,
	now time.Time,
	allowLapsed bool,
) ([]Descriptor, error) {
	seen := make(map[shoal.ID]struct{}, MaxDelegationDepth)
	chain := make([]Descriptor, 0, MaxDelegationDepth)
	currentID := id
	for len(chain) < MaxDelegationDepth {
		if _, exists := seen[currentID]; exists {
			return nil, auth.ObjectNotFound()
		}
		seen[currentID] = struct{}{}
		stored, err := s.store.Get(ctx, currentID)
		if err != nil {
			return nil, concealStoreRead(err)
		}
		descriptor := stored.Descriptor
		if descriptorExpired(descriptor, now) &&
			(!allowLapsed || !descriptor.RevokedAt.IsZero()) {
			return nil, auth.ObjectNotFound()
		}
		chain = append(chain, descriptor)
		if descriptor.ParentID == "" {
			if err := validateDelegationChain(chain); err != nil {
				return nil, err
			}
			return chain, nil
		}
		currentID = descriptor.ParentID
	}
	return nil, auth.ObjectNotFound()
}

func validateDelegationChain(chain []Descriptor) error {
	for index := 0; index+1 < len(chain); index++ {
		descriptor, parent := chain[index], chain[index+1]
		if descriptor.Subject != parent.Subject ||
			!bytes.Equal(descriptor.AuthorizationDomain, parent.AuthorizationDomain) ||
			!scopesSubset(descriptor.Scopes, parent.Scopes) ||
			!capabilitiesSubset(descriptor.Capabilities, parent.Capabilities) ||
			descriptor.LeaseExpiresAt.After(parent.LeaseExpiresAt) {
			return auth.ObjectNotFound()
		}
	}
	return nil
}

func (s *Service) record(
	ctx context.Context,
	decision auth.Decision,
	request RequestContext,
	operation auth.Operation,
	id shoal.ID,
	digests registryDigests,
) error {
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return err
	}
	snapshot, err := s.snapshots.InteractionSnapshot(ctx)
	if err != nil {
		return err
	}
	return s.recorder.RecordLifecycle(ctx, Lifecycle{
		Operation: operation, RequestID: decision.RequestID(),
		CorrelationID: decision.CorrelationID(), Subject: decision.Subject(),
		Actor: decision.Actor(), ClientID: decision.ClientID(),
		OnBehalfOf: decision.OnBehalfOf(), AgentID: id,
		MutationDigest:       digests.current,
		LegacyMutationDigest: digests.v1,
		ReasonCode:           request.ReasonCode, ReasonDetail: request.ReasonDetail,
		Deadline:                 request.Deadline.UnixNano(),
		AuthorizationFingerprint: fingerprint,
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		AuditPurpose:             decision.AuditPurpose(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf.UTC(),
	})
}

func authorizeScopes(decision auth.Decision, operation auth.Operation, id shoal.ID, domain []byte, scopes []Scope, now time.Time) error {
	for _, scope := range scopes {
		if err := decision.AuthorizeObject(operation, auth.ResourceRequest{
			AuthorizationDomain: domain, SourceID: scope.SourceID,
			PolicyID: scope.PolicyID, ObjectID: id,
		}, now); err != nil {
			return err
		}
	}
	return nil
}

func authorizeDescriptor(decision auth.Decision, operation auth.Operation, descriptor Descriptor, now time.Time) error {
	return authorizeScopes(decision, operation, descriptor.ID,
		descriptor.AuthorizationDomain, descriptor.Scopes, now)
}

// validateDeclaredEffects refuses any action whose declaration does not sit
// between what its bound executor always does and what the host permits it to
// do.
//
// Both directions matter and they fail for opposite reasons. Exceeding the
// ceiling asks for authority the host did not grant. Omitting the floor
// understates what will happen — a descriptor that reads as non-transmitting
// while every invocation transmits — and that is the worse direction for
// egress, which leaves no trace in Shoal's own record to reconcile against.
func validateDeclaredEffects(capabilities []Capability, floor, ceiling Effects) error {
	for _, capability := range capabilities {
		for _, action := range capability.Actions {
			if action.Effects.exceeds(ceiling) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"action declares effects its executor is not bound to "+
						"perform; effects beyond the binding are dispatched, "+
						"not performed in process")
			}
			// An executor whose ceiling permits an external effect and whose
			// floor requires none has declined to say whether one happens.
			// The host is telling Shoal "this may reach outside and I cannot
			// tell you when" — so an action bound to it must declare that it
			// may, or the record would assert the whole outcome is in Shoal
			// for work Shoal cannot account for.
			//
			// That assertion is what EffectPossible makes. It is set at claim
			// time from the declaration, so a non-declaring action bound to a
			// dispatch-only reference completes with the flag false while a
			// remote worker may have mutated a third-party system — and the
			// flag reads "no external effect was declared", which an operator
			// is entitled to mistake for "none was possible" (#510, #514).
			//
			// Keyed on floor-below-ceiling rather than on "runs in process",
			// for two reasons. It is the property that actually matters: an
			// in-process executor that declines to declare a floor is in the
			// same position, because the flag is written at claim time before
			// any invocation, so Shoal does not know then either. And it is
			// derivable from the (floor, ceiling) pair, which is all pkg/atpl
			// has — so validate and plan refuse this exactly as Register
			// does, with no new manifest field for an operator to get wrong.
			//
			// AskExecutor is unaffected: its floor equals its ceiling, so the
			// floor check below already forces the declaration.
			//
			// Narrowed to an action declaring *nothing*, which is weaker than
			// requiring an external class and is the strongest rule that
			// forces no false declaration. Requiring one was tried and is
			// wrong: a dispatch-only action may legitimately reach outside
			// nothing — a remote worker that reads Shoal's own corpus and
			// returns — and the project's own ATPL fixture is exactly that
			// shape, `search.query` declaring reads-corpus on a reference
			// whose ceiling permits external. There is no narrower reference
			// to bind it to, because NewExternalEffectBinding refuses a
			// ceiling without EffectMutatesExternal. So the rule would have
			// forced that action to claim an external effect it does not
			// have, making EffectPossible true where it should be false —
			// the same class of false claim, pointing the other way.
			//
			// An action declaring nothing is unambiguous: it asserts its
			// whole outcome is in this record, on a reference whose host has
			// said it may reach outside and declined to say when. That is the
			// shape the reported case had, and it leaves the residual gap
			// honestly open — an action declaring reads-corpus on such a
			// reference still completes with EffectPossible false while a
			// remote worker may have done anything the ceiling permits (#514).
			if external := ceiling.externalClasses(); len(external) > 0 &&
				len(floor.externalClasses()) == 0 &&
				len(action.Effects) == 0 {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"action declares no effects at all and is bound to an "+
						"executor that may reach outside Shoal ("+
						strings.Join(external, ", ")+") without declaring "+
						"when; declare what this action does — any class "+
						"will do, including "+string(EffectReadsCorpus)+
						" alone if it reaches outside nothing")
			}
			if missing := action.Effects.missingFrom(floor); len(missing) > 0 {
				// The missing classes are named. They are host configuration,
				// not another principal's data, so this discloses nothing — and
				// a registrant cannot guess them, because they depend on how
				// the operator configured the executor rather than on the
				// action. Refusing without saying what is missing would make a
				// correct registration a guessing game.
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"action omits effects its executor causes on every "+
						"invocation ("+strings.Join(missing, ", ")+"); the "+
						"declaration must not understate what running it does")
			}
		}
	}
	return nil
}

// validateAttestationTrust refuses a spec in which any action requires
// attestation while the executor ref has no configured trust root. A nil trust
// configures none.
func (s *Service) validateAttestationTrust(spec Spec) error {
	for _, capability := range spec.Capabilities {
		for _, action := range capability.Actions {
			if !action.RequiresAttestation {
				continue
			}
			if nilDependency(s.trust) || !s.trust.Configured(spec.ExecutorRef) {
				return shoal.NewError(shoal.ErrorInvalidArgument,
					"action requires attestation but its executor reference "+
						"has no configured attestation trust root")
			}
		}
	}
	return nil
}

func validateGeneration(generation int64) error {
	if generation < 0 || generation == math.MaxInt64 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"expected generation is outside the incrementable range",
		)
	}
	return nil
}

func validateMutationIdentity(id, key shoal.ID, generation int64) error {
	if err := shoal.ValidateRequiredID("agent ID", id); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("registration key", key); err != nil {
		return err
	}
	if generation <= 0 || generation == math.MaxInt64 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"expected generation must be positive and incrementable",
		)
	}
	return nil
}

func descriptorExpired(descriptor Descriptor, now time.Time) bool {
	return !descriptor.RevokedAt.IsZero() || !now.Before(descriptor.LeaseExpiresAt)
}

func scopesSubset(child, parent []Scope) bool {
	for _, wanted := range child {
		found := false
		for _, allowed := range parent {
			if bytes.Equal(wanted.SourceID, allowed.SourceID) &&
				bytes.Equal(wanted.PolicyID, allowed.PolicyID) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func capabilitiesSubset(child, parent []Capability) bool {
	for _, wantedCapability := range child {
		var allowed *Capability
		for i := range parent {
			if parent[i].Name == wantedCapability.Name {
				allowed = &parent[i]
				break
			}
		}
		if allowed == nil {
			return false
		}
		for _, wantedAction := range wantedCapability.Actions {
			found := false
			for _, allowedAction := range allowed.Actions {
				// Effect is part of what delegation may narrow. Without it a
				// child, or a later generation, could turn an evidence-only
				// action into an external one and still pass as a subset,
				// which would let the effect boundary be widened by exactly
				// the path that exists to prevent widening.
				if wantedAction.Name == allowedAction.Name &&
					bytes.Equal(wantedAction.InputSchema, allowedAction.InputSchema) &&
					bytes.Equal(wantedAction.OutputSchema, allowedAction.OutputSchema) &&
					!wantedAction.Effects.exceeds(allowedAction.Effects) &&
					// Approval narrows in one direction only. A child or a
					// later generation may add it; neither may drop it,
					// because dropping it is exactly the widening this
					// predicate exists to refuse — and a delegate that could
					// shed its parent's approval would be the cheapest way
					// round the control.
					(wantedAction.RequiresApproval ||
						!allowedAction.RequiresApproval) &&
					// Attestation narrows the same way. A delegate that
					// could drop it would be a claimant path round the
					// requirement for exactly the external work it exists
					// to gate.
					(wantedAction.RequiresAttestation ||
						!allowedAction.RequiresAttestation) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func matchesFilter(descriptor Descriptor, request ListRequest) bool {
	if len(request.SourceIDs) == 0 && len(request.PolicyIDs) == 0 {
		return true
	}
	for _, scope := range descriptor.Scopes {
		if containsBytes(request.SourceIDs, scope.SourceID) &&
			containsBytes(request.PolicyIDs, scope.PolicyID) {
			return true
		}
	}
	return false
}

func containsBytes(values [][]byte, value []byte) bool {
	if len(values) == 0 {
		return true
	}
	for _, candidate := range values {
		if bytes.Equal(candidate, value) {
			return true
		}
	}
	return false
}

func concealStoreRead(err error) error {
	if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return auth.ObjectNotFound()
	}
	return err
}
