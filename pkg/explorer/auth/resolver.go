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

package auth

import (
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Binder is the trusted capability that attaches an immutable decision to a
// request context.
type Binder interface {
	Bind(context.Context, Decision) (context.Context, error)
}

// Resolver resolves and revalidates one trusted decision per request.
type Resolver interface {
	Resolve(context.Context) (Decision, error)
}

// Authority owns one unforgeable context capability shared only by its Binder
// and Resolver.
type Authority struct {
	key        *authorityContextKey
	capability *authorityCapability
	clock      func() time.Time
}

type authorityContextKey struct {
	unique byte
}

type authorityCapability struct {
	unique byte
}

type boundDecision struct {
	capability *authorityCapability
	decision   Decision
}

type authorityBinder struct {
	authority *Authority
}

type authorityResolver struct {
	authority *Authority
}

// NewAuthority constructs an authority using the process clock.
func NewAuthority() *Authority {
	authority, _ := NewAuthorityWithClock(time.Now)
	return authority
}

// NewAuthorityWithClock constructs a testable authority with an injected
// trusted clock.
func NewAuthorityWithClock(clock func() time.Time) (*Authority, error) {
	if clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "clock is required")
	}
	return &Authority{
		key:        &authorityContextKey{},
		capability: &authorityCapability{},
		clock:      clock,
	}, nil
}

// Binder returns the capability-scoped trusted binder.
func (a *Authority) Binder() Binder {
	return authorityBinder{authority: a}
}

// Resolver returns the matching capability-scoped resolver.
func (a *Authority) Resolver() Resolver {
	return authorityResolver{authority: a}
}

func (b authorityBinder) Bind(
	ctx context.Context,
	decision Decision,
) (context.Context, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	if b.authority == nil || b.authority.key == nil ||
		b.authority.capability == nil || b.authority.clock == nil {
		return nil, unauthorized()
	}
	cloned, err := decision.cloneValidated()
	now := b.authority.clock()
	if err != nil || now.IsZero() || !now.Before(cloned.expiresAt) {
		return nil, unauthorized()
	}
	return context.WithValue(ctx, b.authority.key, boundDecision{
		capability: b.authority.capability,
		decision:   cloned,
	}), nil
}

func (r authorityResolver) Resolve(ctx context.Context) (Decision, error) {
	if err := contextFailure(ctx); err != nil {
		return Decision{}, err
	}
	if r.authority == nil || r.authority.key == nil ||
		r.authority.capability == nil || r.authority.clock == nil {
		return Decision{}, unauthorized()
	}
	bound, ok := ctx.Value(r.authority.key).(boundDecision)
	if !ok || bound.capability != r.authority.capability {
		return Decision{}, unauthorized()
	}
	cloned, err := bound.decision.cloneValidated()
	now := r.authority.clock()
	if err != nil || now.IsZero() || !now.Before(cloned.expiresAt) {
		return Decision{}, unauthorized()
	}
	if err := contextFailure(ctx); err != nil {
		return Decision{}, err
	}
	return cloned, nil
}

type staticResolver struct {
	decision Decision
	clock    func() time.Time
}

// NewStaticResolver constructs a trusted embedded-host resolver.
func NewStaticResolver(decision Decision) (Resolver, error) {
	return NewStaticResolverWithClock(decision, time.Now)
}

// NewStaticResolverWithClock constructs a testable trusted static resolver.
func NewStaticResolverWithClock(
	decision Decision,
	clock func() time.Time,
) (Resolver, error) {
	if clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "clock is required")
	}
	cloned, err := decision.cloneValidated()
	if err != nil {
		return nil, err
	}
	now := clock()
	if now.IsZero() || !now.Before(cloned.expiresAt) {
		return nil, unauthorized()
	}
	return staticResolver{decision: cloned, clock: clock}, nil
}

func (r staticResolver) Resolve(ctx context.Context) (Decision, error) {
	if err := contextFailure(ctx); err != nil {
		return Decision{}, err
	}
	cloned, err := r.decision.cloneValidated()
	if err != nil || r.clock == nil {
		return Decision{}, unauthorized()
	}
	now := r.clock()
	if now.IsZero() || !now.Before(cloned.expiresAt) {
		return Decision{}, unauthorized()
	}
	return cloned, nil
}

// HostDecisionFunc resolves a trusted host decision for one request.
type HostDecisionFunc func(context.Context) (Decision, error)

type hostResolver struct {
	resolve HostDecisionFunc
	clock   func() time.Time
}

// NewHostResolver constructs a trusted dynamic embedded-host resolver.
func NewHostResolver(resolve HostDecisionFunc) (Resolver, error) {
	return NewHostResolverWithClock(resolve, time.Now)
}

// NewHostResolverWithClock constructs a testable trusted host resolver.
func NewHostResolverWithClock(
	resolve HostDecisionFunc,
	clock func() time.Time,
) (Resolver, error) {
	if resolve == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "host resolver is required")
	}
	if clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "clock is required")
	}
	return hostResolver{resolve: resolve, clock: clock}, nil
}

func (r hostResolver) Resolve(ctx context.Context) (Decision, error) {
	if err := contextFailure(ctx); err != nil {
		return Decision{}, err
	}
	decision, err := r.resolve(ctx)
	if err != nil {
		if contextErr := contextFailure(ctx); contextErr != nil {
			return Decision{}, contextErr
		}
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
			shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			return Decision{}, unauthorized()
		}
		return Decision{}, shoal.WrapError(
			shoal.ErrorUnavailable, "authorization resolution unavailable", err)
	}
	if err := contextFailure(ctx); err != nil {
		return Decision{}, err
	}
	cloned, err := decision.cloneValidated()
	now := r.clock()
	if err != nil || now.IsZero() || !now.Before(cloned.expiresAt) {
		return Decision{}, unauthorized()
	}
	return cloned, nil
}

var (
	_ Binder   = authorityBinder{}
	_ Resolver = authorityResolver{}
	_ Resolver = staticResolver{}
	_ Resolver = hostResolver{}
)
