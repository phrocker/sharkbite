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

// Package decisionservice composes trusted authentication, retained artifacts,
// registered predictors and durable receipts. Transport wiring remains separate.
package decisionservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var ErrUnavailable = shoal.NewError(shoal.ErrorUnavailable, "decision service unavailable")
var ErrIndeterminate = shoal.NewError(shoal.ErrorUnavailable, "decision receipt state indeterminate")

// Artifacts is a trusted, authorized retention boundary, not a caller-supplied
// request resolver. Implementations must retain the original bytes and verified
// provenance, check CURRENT access to every source/anchor/outcome contributing to
// the picture, and return a non-disclosing not-found for absent/revoked artifacts.
// A snapshot pin or authorization fingerprint alone cannot implement this check.
// The service rechecks this boundary before inference and after provider work.
// Other failures are availability errors, not evidence that an artifact is absent.
type Artifacts interface {
	LoadAuthorized(context.Context, auth.Decision, shoal.ID) (Bundle, error)
}

// Bundle is returned only by the trusted artifact/registry integration. Input
// is the exact bounded serialization whose digest the picture pins. TaskResource
// supplies the registry's invocation/read policy, never source-authored policy.
type Bundle struct {
	Request        decision.DecisionRequest
	EvidencePolicy decision.EvidencePolicy
	RankingPlan    decision.RankingPlan
	TaskResource   auth.ResourceRequest
	Input          []byte
}

// Providers resolves an immutable release/predictor pair once per execution.
// No arbitrary model path, device override or hosted fallback is accepted.
type Providers interface {
	Resolve(context.Context, shoal.ID, shoal.ID) (Predictor, error)
}

// Predictor implementations must honor cancellation/deadlines and enforce their
// runtime concurrency and egress limits. This service does not spawn an unbounded
// goroutine to hide a provider that ignores its context.
type Predictor interface {
	Identity() decision.PredictorIdentity
	Predict(context.Context, decision.DecisionRequest, []byte) (decision.ResultConfig, error)
}
type Receipts interface {
	Reserve(context.Context, decisionstore.Scope, []byte, decision.DecisionRequest, time.Duration) (decisionstore.Reservation, error)
	Get(context.Context, decisionstore.Scope, []byte, decision.DecisionRequest) (decisionstore.Receipt, error)
	Commit(context.Context, decisionstore.Scope, []byte, decision.DecisionRequest, decisionstore.Claim, decision.ResultConfig) (decisionstore.Receipt, error)
}
type Config struct {
	Resolver   auth.Resolver
	Artifacts  Artifacts
	Providers  Providers
	Receipts   Receipts
	Clock      func() time.Time
	Lease      time.Duration
	MaxCall    time.Duration
	Settlement time.Duration
}
type Service struct{ config Config }

func New(c Config) (*Service, error) {
	if nilDependency(c.Resolver) || nilDependency(c.Artifacts) || nilDependency(c.Providers) || nilDependency(c.Receipts) || c.Clock == nil || c.Lease <= 0 || c.Lease > decisionstore.MaxLease || c.MaxCall <= 0 || c.Settlement <= 0 || c.MaxCall >= c.Lease || c.Settlement >= c.Lease-c.MaxCall {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "invalid decision service configuration")
	}
	return &Service{c}, nil
}

// Response contains no source text or executor claim. A pending receipt carries
// no ranking. All entries in a completed ranking remain inspection candidates.
type Response struct {
	Receipt decisionstore.Receipt
	Ranking *decision.InspectionRanking
}

func (s *Service) Evaluate(ctx context.Context, requestID shoal.ID, key []byte) (Response, error) {
	if err := validateKey(key); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	principal, bundle, scope, err := s.authorize(ctx, requestID, auth.OperationInvoke)
	if err != nil {
		return Response{}, err
	}
	reservation, err := s.config.Receipts.Reserve(ctx, scope, key, bundle.Request, s.config.Lease)
	if err != nil {
		return Response{}, storeError(err)
	}
	if reservation.Claim == nil {
		// Storage may block across revocation or authentication expiry.
		if _, _, _, err := s.authorizeSame(ctx, requestID, auth.OperationInvoke, principal, bundle); err != nil {
			return Response{}, err
		}
		return response(bundle, reservation.Receipt)
	}
	// Never invoke a provider merely because the caller supplied a valid ID.
	// A reservation must have durably granted this worker the claim first.
	principal, bundle, _, err = s.authorizeSame(ctx, requestID, auth.OperationInvoke, principal, bundle)
	if err != nil {
		return Response{}, err
	}
	eligibility, err := decision.NewEvidenceEligibility(bundle.Request, bundle.EvidencePolicy)
	if err != nil {
		return Response{}, ErrUnavailable
	}
	eligible := map[shoal.ID]bool{}
	for _, entry := range eligibility.Entries() {
		eligible[entry.SubjectID] = len(entry.Reasons) == 0
	}
	ineligible := false
	for _, id := range bundle.Request.Config().SubjectIDs {
		if !eligible[id] {
			ineligible = true
		}
	}
	// Resolve the registered runtime only after access and eligibility checks.
	var result decision.ResultConfig
	if ineligible {
		result = terminal(bundle.Request, decision.Abstained,
			decision.ReasonEvidenceIneligible, s.now())
	} else {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		provider, resolveErr := s.config.Providers.Resolve(ctx, bundle.Request.Config().ReleaseID, bundle.Request.PredictorID())
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		if resolveErr != nil || nilDependency(provider) {
			result = terminal(bundle.Request, decision.Failed,
				decision.ReasonPredictorUnavailable, s.now())
		} else {
			identity := provider.Identity()
			if identity.Validate() != nil || identity.ID() != bundle.Request.PredictorID() {
				result = terminal(bundle.Request, decision.Failed,
					decision.ReasonPredictorIdentityMismatch, s.now())
			} else {
				// Registry latency must not bypass revocation checks or consume the lease.
				principal, bundle, _, err = s.authorizeSame(ctx, requestID, auth.OperationInvoke, principal, bundle)
				if err != nil {
					return Response{}, err
				}
				now := s.now()
				until := bundle.Request.Config().Deadline
				if t := now.Add(s.config.MaxCall); t.Before(until) {
					until = t
				}
				if t := reservation.Receipt.LeaseUntil.Add(-s.config.Settlement); t.Before(until) {
					until = t
				}
				if principal.AuthenticationExpires().Before(until) {
					until = principal.AuthenticationExpires()
				}
				if !until.After(now) {
					result = terminal(bundle.Request, decision.Failed,
						decision.ReasonDeadlineExceeded, now)
				} else {
					callCtx, cancel := context.WithTimeout(ctx, until.Sub(now))
					if err := callCtx.Err(); err != nil {
						cancel()
						return Response{}, err
					}
					native, predictErr := provider.Predict(callCtx, bundle.Request, append([]byte(nil), bundle.Input...))
					callErr := callCtx.Err()
					cancel()
					completed := s.now()
					switch {
					case callErr != nil || completed.After(until):
						result = terminal(bundle.Request, decision.Failed,
							decision.ReasonDeadlineExceeded, completed)
					case predictErr != nil:
						result = terminal(bundle.Request, decision.Failed,
							decision.ReasonPredictorFailed, completed)
					default:
						// Completion time is stamped by this executor, not trusted from output.
						native.CompletedAt = completed
						// A predictor may not claim one of the service's own
						// reasons. Passing its reason through unchecked let a
						// predictor returning "evidence_ineligible" produce a
						// stored result reading "the service found the
						// evidence ineligible and ran no predictor", which is
						// false and which nothing in the record contradicted
						// (#509).
						//
						// Adjudicated as invalid_predictor_response, the
						// reason that already exists for a response that is
						// not a valid prediction. A response claiming an
						// authority it does not have is exactly that, and
						// reusing it needs no new vocabulary — note that the
						// reason the service then writes is itself reserved,
						// so a reader can tell this adjudication from
						// anything the predictor said.
						if decision.ReservedServiceReason(native.Reason) {
							result = terminal(
								bundle.Request, decision.Failed,
								decision.ReasonInvalidPredictorResponse,
								completed)
						} else if _, err := decision.NewPredictionRecord(bundle.Request, native); err != nil {
							result = terminal(bundle.Request, decision.Failed,
								decision.ReasonInvalidPredictorResponse, completed)
						} else {
							result = native
						}
					}
				}
			}
		}
	}
	// Re-authorize before persisting or exposing a provider result. Revocation can
	// leave a pending reservation; it never returns the now-inaccessible payload.
	if _, _, _, err := s.authorizeSame(ctx, requestID, auth.OperationInvoke, principal, bundle); err != nil {
		return Response{}, err
	}
	settleCtx, cancel := context.WithTimeout(ctx, s.config.Settlement)
	defer cancel()
	committed, err := s.config.Receipts.Commit(settleCtx, scope, key, bundle.Request, *reservation.Claim, result)
	if err != nil {
		return Response{}, storeError(err)
	}
	if _, _, _, err := s.authorizeSame(ctx, requestID, auth.OperationInvoke, principal, bundle); err != nil {
		return Response{}, err
	}
	return response(bundle, committed)
}
func (s *Service) Read(ctx context.Context, requestID shoal.ID, key []byte) (Response, error) {
	if err := validateKey(key); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	principal, bundle, scope, err := s.authorize(ctx, requestID, auth.OperationRead)
	if err != nil {
		return Response{}, err
	}
	receipt, err := s.config.Receipts.Get(ctx, scope, key, bundle.Request)
	if err != nil {
		return Response{}, storeError(err)
	}
	if _, _, _, err := s.authorizeSame(ctx, requestID, auth.OperationRead, principal, bundle); err != nil {
		return Response{}, err
	}
	return response(bundle, receipt)
}
func (s *Service) authorize(ctx context.Context, id shoal.ID, operation auth.Operation) (auth.Decision, Bundle, decisionstore.Scope, error) {
	d, err := s.config.Resolver.Resolve(ctx)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			return auth.Decision{}, Bundle{}, decisionstore.Scope{}, shoal.NewError(shoal.ErrorUnauthorized, "unauthorized")
		}
		return auth.Decision{}, Bundle{}, decisionstore.Scope{}, ErrUnavailable
	}
	fail := func(err error) (auth.Decision, Bundle, decisionstore.Scope, error) {
		return auth.Decision{}, Bundle{}, decisionstore.Scope{}, err
	}
	if err := shoal.ValidateRequiredID("request ID", id); err != nil {
		return fail(err)
	}
	if err := d.Authorize(operation, auth.ResourceRequest{AuthorizationDomain: d.AuthorizationDomain()}, s.now()); err != nil {
		return fail(err)
	}
	b, err := s.config.Artifacts.LoadAuthorized(ctx, d, id)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) || shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			return fail(auth.ObjectNotFound())
		}
		return fail(ErrUnavailable)
	}
	if b.Request.Validate() != nil || b.Request.ID() != id || b.Request.Config().PrincipalID != d.Subject() {
		return fail(auth.ObjectNotFound())
	}
	resource, err := b.TaskResource.Normalize()
	if err != nil || resource.ObjectID != b.Request.TaskID() || len(resource.SourceID) == 0 || len(resource.PolicyID) == 0 {
		return fail(ErrUnavailable)
	}
	if err := d.AuthorizeObject(operation, resource, s.now()); err != nil {
		return fail(err)
	}
	if b.EvidencePolicy.Validate() != nil || b.Request.Task().Config().EvidencePolicyID != b.EvidencePolicy.ID() || b.RankingPlan.ValidateTask(b.Request.Task()) != nil {
		return fail(ErrUnavailable)
	}
	if len(b.Input) == 0 || len(b.Input) > inference.MaxContextPackBytes {
		return fail(ErrUnavailable)
	}
	sum := sha256.Sum256(b.Input)
	if hex.EncodeToString(sum[:]) != b.Request.Picture().Config().InputDigest {
		return fail(ErrUnavailable)
	}
	if operation == auth.OperationInvoke {
		fp, err := auth.AuthorizationFingerprint(d)
		if err != nil || shoal.ID(fp.String()) != b.Request.Picture().Authorization().Fingerprint() {
			return fail(auth.ObjectNotFound())
		}
	}
	b.TaskResource = resource
	b.Input = append([]byte(nil), b.Input...)
	scope, err := scopeFor(d)
	if err != nil {
		return fail(ErrUnavailable)
	}
	return d, b, scope, nil
}
func (s *Service) authorizeSame(ctx context.Context, id shoal.ID, operation auth.Operation, original auth.Decision, b Bundle) (auth.Decision, Bundle, decisionstore.Scope, error) {
	d, current, scope, err := s.authorize(ctx, id, operation)
	if err != nil {
		return d, current, scope, err
	}
	before, err := scopeFor(original)
	if err != nil {
		return auth.Decision{}, Bundle{}, decisionstore.Scope{}, ErrUnavailable
	}
	if !bytes.Equal(before.Domain, scope.Domain) || before.Principal != scope.Principal || current.Request.ID() != b.Request.ID() {
		return auth.Decision{}, Bundle{}, decisionstore.Scope{}, auth.ObjectNotFound()
	}
	return d, current, scope, nil
}
func scopeFor(d auth.Decision) (decisionstore.Scope, error) {
	// Stable caller identity includes delegation but excludes mutable grants and
	// expiry. Grant changes must re-authorize access, never mint a second key space.
	payload := struct {
		Domain                 []byte
		Subject, Actor, Client []byte
		Delegates              [][]byte
	}{Domain: d.AuthorizationDomain(), Subject: []byte(d.Subject()), Actor: []byte(d.Actor()), Client: []byte(d.ClientID())}
	for _, id := range d.OnBehalfOf() {
		payload.Delegates = append(payload.Delegates, []byte(id))
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return decisionstore.Scope{}, err
	}
	sum := sha256.Sum256(b)
	return decisionstore.Scope{Domain: sum[:], Principal: d.Subject()}, nil
}
func (s *Service) now() time.Time { return s.config.Clock().Round(0).UTC() }
func terminal(r decision.DecisionRequest, status decision.ResultStatus, reason string, t time.Time) decision.ResultConfig {
	result := decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), Status: status, Reason: reason, CompletedAt: t}
	// The abstention contract requires the pinned device even when eligibility
	// prevents execution. Failed receipts omit an unverified effective device.
	if status == decision.Abstained {
		result.EffectiveDevice = r.Predictor().Config().Device
	}
	return result
}
func response(b Bundle, r decisionstore.Receipt) (Response, error) {
	if r.State == decisionstore.Pending {
		return Response{Receipt: r}, nil
	}
	if r.Result == nil {
		return Response{}, ErrUnavailable
	}
	p, err := decision.NewPredictionRecord(b.Request, *r.Result)
	if err != nil {
		return Response{}, ErrUnavailable
	}
	ranking, err := decision.NewInspectionRanking(p, b.EvidencePolicy, b.RankingPlan)
	if err != nil {
		return Response{}, ErrUnavailable
	}
	return Response{Receipt: r, Ranking: &ranking}, nil
}
func storeError(err error) error {
	switch {
	case errors.Is(err, decisionstore.ErrIndeterminate):
		return ErrIndeterminate
	case errors.Is(err, decisionstore.ErrNotFound):
		return auth.ObjectNotFound()
	case errors.Is(err, decisionstore.ErrConflict):
		return shoal.NewError(shoal.ErrorConflict, "decision request conflict")
	default:
		return ErrUnavailable
	}
}

func nilDependency(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// Reject malformed keys independently of artifact existence for both operations.
func validateKey(key []byte) error {
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid idempotency key")
	}
	return nil
}
