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

package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type configuredFleetExecutor struct {
	reference string
}

type configuredFleetExecutors map[string]fleet.Executor

type fleetInteractionSink struct {
	durable    interaction.Sink
	authorized interaction.ResultSink
}

type boundFleetRegistry struct {
	service  *fleet.Service
	resolver auth.Resolver
}

type boundFleetDispatch struct {
	service  *fleet.DispatchService
	resolver auth.Resolver
}

// validFleetExecutorRefFlag holds one configured executor reference to the
// executor-reference rule (#391), so a host cannot allowlist, bind or trust a
// reference no descriptor could register or no worker could be bound to. The
// error names the flag and the entry's 1-based position (position < 0 for a
// single-valued flag) and never echoes the value, which may hold any bytes.
func validFleetExecutorRefFlag(flag string, position int, reference string) error {
	err := executorref.ValidExecutorRef(reference)
	if err == nil {
		return nil
	}
	where := flag
	if position >= 0 {
		where = fmt.Sprintf("%s entry %d", flag, position+1)
	}
	return shoal.NewError(shoal.ErrorInvalidArgument, where+": "+err.Error())
}

func newConfiguredFleetExecutors(
	references []string,
) (configuredFleetExecutors, error) {
	result := make(configuredFleetExecutors, len(references))
	for position, reference := range references {
		if err := validFleetExecutorRefFlag(
			"-fleet-executor-refs", position, reference,
		); err != nil {
			return nil, err
		}
		result[reference] = configuredFleetExecutor{reference: reference}
	}
	return result, nil
}

func (r configuredFleetExecutors) ResolveExecutor(
	reference string,
) (fleet.Executor, bool) {
	executor, ok := r[reference]
	return executor, ok
}

// bind attaches a real executor implementation to a reference the host has
// already allowlisted. An unbound reference keeps its existing meaning: a
// descriptor may register against it, and an invocation fails closed because
// the placeholder implements no action execution. Binding happens once during
// composition, before the listener serves, so the registry is never mutated
// concurrently with resolution.
func (r configuredFleetExecutors) bind(
	reference string, executor fleet.Executor,
) error {
	current, allowed := r[reference]
	if !allowed {
		return fmt.Errorf(
			"fleet executor reference %q is not in -fleet-executor-refs",
			reference)
	}
	// Refuse a second binding rather than overwrite the first. The callers
	// above each check the collisions they know about, but they check them
	// against their own inputs: bindExternalFleetEffects compares its two
	// lists and the ask reference, and nothing compares a future third
	// binding against any of them. This is the seam every binding passes
	// through, so it is the one place the invariant can be stated once.
	//
	// Silently overwriting is the specific failure worth refusing. Two
	// bindings on one reference are two different effect ceilings, and which
	// one survives would be decided by the order composition happens to run
	// in — so a reference the operator configured as a dispatch-only gateway
	// could resolve to the grounded-reasoning executor, or an external
	// ceiling could replace a floor that deliberately excludes external
	// mutation. Neither is visible at startup and both change what the
	// registry permits.
	if _, unbound := current.(configuredFleetExecutor); !unbound {
		return fmt.Errorf(
			"fleet executor reference %q is already bound to %T; one "+
				"reference carries one executor and one effect ceiling, and "+
				"binding it twice would let composition order decide which "+
				"ceiling the registry enforces",
			reference, current)
	}
	// fleet.Executor is an empty interface, so a typed nil would satisfy a
	// plain nil check, resolve, satisfy the ActionExecutor assertion, and then
	// panic mid-dispatch after the effect-admission record is already written.
	if isNilFleetDependency(executor) {
		return errors.New("fleet executor binding requires an implementation")
	}
	r[reference] = executor
	return nil
}

func (s fleetInteractionSink) EnsureInteractionSink(ctx context.Context) error {
	return s.durable.EnsureInteractionSink(ctx)
}

func (s fleetInteractionSink) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	_, err := s.authorized.RecordInteractionResult(ctx, session)
	return err
}

func (s fleetInteractionSink) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	return s.authorized.RecordInteractionResult(ctx, session)
}

func (s fleetInteractionSink) Record(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	return s.authorized.RecordInteractionResult(ctx, session)
}

func newBoundFleetRegistry(
	service *fleet.Service,
	resolver auth.Resolver,
) (*boundFleetRegistry, error) {
	if service == nil || isNilFleetDependency(resolver) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"bound fleet registry dependencies are required",
		)
	}
	return &boundFleetRegistry{service: service, resolver: resolver}, nil
}

func newBoundFleetDispatch(
	service *fleet.DispatchService,
	resolver auth.Resolver,
) (*boundFleetDispatch, error) {
	if service == nil || isNilFleetDependency(resolver) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"bound fleet dispatch dependencies are required",
		)
	}
	return &boundFleetDispatch{service: service, resolver: resolver}, nil
}

func isNilFleetDependency(value any) bool {
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

func (r *boundFleetRegistry) requestContext(
	ctx context.Context,
	request fleet.RequestContext,
) (fleet.RequestContext, error) {
	decision, err := r.resolver.Resolve(ctx)
	if err != nil {
		return fleet.RequestContext{}, err
	}
	request.RequestID = decision.RequestID()
	request.CorrelationID = decision.CorrelationID()
	return request, nil
}

func (r *boundFleetRegistry) Register(
	ctx context.Context,
	request fleet.RegisterRequest,
) (fleet.Descriptor, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	request.Context = bound
	return r.service.Register(ctx, request)
}

func (r *boundFleetRegistry) Heartbeat(
	ctx context.Context,
	request fleet.HeartbeatRequest,
) (fleet.Descriptor, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	request.Context = bound
	return r.service.Heartbeat(ctx, request)
}

func (r *boundFleetRegistry) Revoke(
	ctx context.Context,
	request fleet.RevokeRequest,
) (fleet.Descriptor, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	request.Context = bound
	return r.service.Revoke(ctx, request)
}

func (r *boundFleetRegistry) Resolve(
	ctx context.Context,
	request fleet.ResolveRequest,
) (fleet.Resolved, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.Resolved{}, err
	}
	request.Context = bound
	return r.service.Resolve(ctx, request)
}

func (r *boundFleetRegistry) List(
	ctx context.Context,
	request fleet.ListRequest,
) (fleet.ListPage, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ListPage{}, err
	}
	request.Context = bound
	return r.service.List(ctx, request)
}

func (r *boundFleetDispatch) requestContext(
	ctx context.Context,
	request fleet.RequestContext,
) (fleet.RequestContext, error) {
	decision, err := r.resolver.Resolve(ctx)
	if err != nil {
		return fleet.RequestContext{}, err
	}
	request.RequestID = decision.RequestID()
	request.CorrelationID = decision.CorrelationID()
	return request, nil
}

func (r *boundFleetDispatch) Enqueue(
	ctx context.Context,
	request fleet.EnqueueRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.Enqueue(ctx, request)
}

func (r *boundFleetDispatch) Claim(
	ctx context.Context,
	request fleet.ClaimRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.Claim(ctx, request)
}

// CompleteClaim closes the loop for a worker that performed the effect out of
// process. The bound request context is applied exactly as every other
// dispatch call applies it, so a remote completion is authenticated as the
// caller and never as the process.
func (r *boundFleetDispatch) CompleteClaim(
	ctx context.Context,
	request fleet.CompletionRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.CompleteClaim(ctx, request)
}

func (r *boundFleetDispatch) ExtendClaim(
	ctx context.Context,
	request fleet.ExtendRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.ExtendClaim(ctx, request)
}

func (r *boundFleetDispatch) ReportAmbiguity(
	ctx context.Context,
	request fleet.AmbiguityRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.ReportAmbiguity(ctx, request)
}

func (r *boundFleetDispatch) Cancel(
	ctx context.Context,
	request fleet.CancelRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.Cancel(ctx, request)
}

func (r *boundFleetDispatch) Status(
	ctx context.Context,
	request fleet.StatusRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Context = bound
	return r.service.Status(ctx, request)
}

func (r *boundFleetDispatch) Pull(
	ctx context.Context,
	request fleet.PullActionsRequest,
) (fleet.ActionPage, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ActionPage{}, err
	}
	request.Context = bound
	return r.service.Pull(ctx, request)
}

func (r *boundFleetDispatch) Invoke(
	ctx context.Context,
	request fleet.InvokeRequest,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, request.Enqueue.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	request.Enqueue.Context = bound
	return r.service.Invoke(ctx, request)
}

// boundAdmission binds the pre-call admission seam to the authenticated request
// exactly as the dispatch surface is bound.
//
// The binding is the security property, not a convenience: the request and
// correlation identity come from the resolved decision rather than from the
// body, so a proxy cannot ask for admission under a request identity that is
// not its own.
type boundAdmission struct {
	service  *fleet.AdmissionService
	resolver auth.Resolver
}

func newBoundAdmission(
	service *fleet.AdmissionService,
	resolver auth.Resolver,
) (*boundAdmission, error) {
	if service == nil || isNilFleetDependency(resolver) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"bound admission dependencies are required",
		)
	}
	return &boundAdmission{service: service, resolver: resolver}, nil
}

func (r *boundAdmission) requestContext(
	ctx context.Context,
	request fleet.RequestContext,
) (fleet.RequestContext, error) {
	decision, err := r.resolver.Resolve(ctx)
	if err != nil {
		return fleet.RequestContext{}, err
	}
	request.RequestID = decision.RequestID()
	request.CorrelationID = decision.CorrelationID()
	return request, nil
}

func (r *boundAdmission) Request(
	ctx context.Context,
	request fleet.AdmissionRequest,
) (fleet.AdmissionGrant, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.AdmissionGrant{}, err
	}
	request.Context = bound
	return r.service.Request(ctx, request)
}

func (r *boundAdmission) Report(
	ctx context.Context,
	report fleet.AdmissionReport,
) (fleet.ActionRecord, error) {
	bound, err := r.requestContext(ctx, report.Context)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	report.Context = bound
	return r.service.Report(ctx, report)
}

func (r *boundAdmission) Outstanding(
	ctx context.Context,
	request fleet.OutstandingAdmissionsRequest,
) (fleet.OutstandingAdmissionsPage, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.OutstandingAdmissionsPage{}, err
	}
	request.Context = bound
	return r.service.Outstanding(ctx, request)
}

// boundApproval binds the approval surface to the authenticated request as the
// dispatch and admission surfaces are bound: request and correlation identity
// come from the resolved decision, never from the body.
//
// The refusal of a workspace-narrowed approver is not here. It used to be, on
// Decide alone, which left Pending and Status open to the same narrowing; it
// now lives in the approval service's eligibility check, fed by the Narrowed
// predicate main.go supplies, so every approver path applies it.
type boundApproval struct {
	service  *fleet.ApprovalService
	resolver auth.Resolver
}

func newBoundApproval(
	service *fleet.ApprovalService,
	resolver auth.Resolver,
) (*boundApproval, error) {
	if service == nil || isNilFleetDependency(resolver) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"bound approval dependencies are required",
		)
	}
	return &boundApproval{service: service, resolver: resolver}, nil
}

func (r *boundApproval) requestContext(
	ctx context.Context,
	request fleet.RequestContext,
) (fleet.RequestContext, error) {
	decision, err := r.resolver.Resolve(ctx)
	if err != nil {
		return fleet.RequestContext{}, err
	}
	request.RequestID = decision.RequestID()
	request.CorrelationID = decision.CorrelationID()
	return request, nil
}

func (r *boundApproval) Request(
	ctx context.Context,
	request fleet.EnqueueRequest,
) (fleet.ApprovalReceipt, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ApprovalReceipt{}, err
	}
	request.Context = bound
	return r.service.Request(ctx, request)
}

func (r *boundApproval) Decide(
	ctx context.Context,
	request fleet.ApprovalDecisionRequest,
) (fleet.ApprovalRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ApprovalRecord{}, err
	}
	request.Context = bound
	return r.service.Decide(ctx, request)
}

func (r *boundApproval) Pending(
	ctx context.Context,
	request fleet.PendingApprovalsRequest,
) (fleet.PendingApprovalsPage, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.PendingApprovalsPage{}, err
	}
	request.Context = bound
	return r.service.Pending(ctx, request)
}

func (r *boundApproval) Status(
	ctx context.Context,
	request fleet.ApprovalStatusRequest,
) (fleet.ApprovalStatus, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ApprovalStatus{}, err
	}
	request.Context = bound
	return r.service.Status(ctx, request)
}

// Withdraw is the requester's withdrawal (#655), served through
// webapi.FleetApprovalWithdrawer.
func (r *boundApproval) Withdraw(
	ctx context.Context,
	request fleet.ApprovalWithdrawRequest,
) (fleet.ApprovalRecord, error) {
	bound, err := r.requestContext(ctx, request.Context)
	if err != nil {
		return fleet.ApprovalRecord{}, err
	}
	request.Context = bound
	return r.service.Withdraw(ctx, request)
}

// externalFleetEffectBindings is the operator's per-reference opt-in to the
// one binding in this process that admits external work.
//
// The three fields are separate rather than one list because they are three
// different declarations and the difference is the whole control. A reference
// in mutating may serve actions declaring {external}; a reference in
// transmitting may also serve ones that transmit corpus content off the host;
// askReference is here only so a collision with the grounded-reasoning
// executor can be refused rather than resolved by binding order.
//
// Nothing here has a default that widens. An operator who sets neither list
// gets what the explorer has always had: every allowlisted reference resolves
// to a placeholder declaring no ceiling, which permits nothing.
type externalFleetEffectBindings struct {
	mutating     []string
	transmitting []string
	askReference string
}

// bindExternalFleetEffects attaches an external-mutation ceiling to each
// reference the operator named for it.
//
// Two collisions are refused rather than resolved, because both are a values
// file that says two things about one reference and both would otherwise be
// settled silently by the order these loops happen to run in.
//
// A reference named in both lists is a contradiction about whether the
// operation transmits. Binding order would pick one, and the wider set winning
// would mean an operator acquires egress authority by listing a reference
// twice — which is exactly the accidental widening this flag pair exists to
// prevent.
//
// A reference that is also -fleet-ask-executor-ref is worse, because the two
// bindings are not narrower and wider but incompatible. AskExecutor carries a
// floor equal to its ceiling and deliberately excludes external mutation; this
// binding carries an external ceiling and no floor. One reference cannot be
// both, and whichever bound last would overwrite the other in the registry: an
// ask descriptor would stop resolving, or a reasoning executor would be reached
// through a reference the operator believes is a dispatch-only gateway.
func bindExternalFleetEffects(
	executors configuredFleetExecutors,
	config externalFleetEffectBindings,
) error {
	if config.askReference != "" {
		if err := validFleetExecutorRefFlag(
			"-fleet-ask-executor-ref", -1, config.askReference,
		); err != nil {
			return err
		}
	}
	claimedBy := make(
		map[string]string, len(config.mutating)+len(config.transmitting))
	for _, group := range []struct {
		flag       string
		references []string
		ceiling    fleet.Effects
	}{
		{
			flag:       "-fleet-external-executor-refs",
			references: config.mutating,
			ceiling:    fleet.Effects{fleet.EffectMutatesExternal},
		},
		{
			flag:       "-fleet-external-egress-executor-refs",
			references: config.transmitting,
			ceiling: fleet.Effects{
				fleet.EffectEgressesContent, fleet.EffectMutatesExternal,
			},
		},
	} {
		for position, reference := range group.references {
			if err := validFleetExecutorRefFlag(
				group.flag, position, reference,
			); err != nil {
				return err
			}
			if config.askReference != "" &&
				reference == config.askReference {
				return fmt.Errorf(
					"fleet executor reference %q is named by both %s and "+
						"-fleet-ask-executor-ref; one reference cannot carry "+
						"both an external-mutation ceiling with no floor and "+
						"the grounded-reasoning executor's floor, which "+
						"equals its ceiling and excludes external mutation",
					reference, group.flag)
			}
			if previous, claimed := claimedBy[reference]; claimed &&
				previous != group.flag {
				return fmt.Errorf(
					"fleet executor reference %q is named by both %s and %s; "+
						"whether the operation transmits corpus content off "+
						"the host is one declaration per reference, not a "+
						"choice made by binding order",
					reference, previous, group.flag)
			}
			claimedBy[reference] = group.flag
			binding, err := fleet.NewExternalEffectBinding(group.ceiling)
			if err != nil {
				return err
			}
			if err := executors.bind(reference, binding); err != nil {
				return err
			}
		}
	}
	return nil
}
