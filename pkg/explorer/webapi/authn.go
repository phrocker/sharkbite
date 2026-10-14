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

package webapi

import (
	"context"
	"net/http"
	"reflect"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Authenticator turns an inbound request into a trusted decision. Any returned
// error denies the request; the transport never distinguishes denial reasons to
// the caller. Implementations must derive identity only from material they can
// verify, never from unauthenticated request metadata.
type Authenticator interface {
	Authenticate(*http.Request) (auth.Decision, error)
}

// AuthenticatorFunc adapts a function to the Authenticator contract.
type AuthenticatorFunc func(*http.Request) (auth.Decision, error)

// Authenticate calls the trusted authenticator function. A nil function value
// denies the request instead of panicking, so an adapter that was never given
// a function fails closed on every call path rather than crashing the request.
func (f AuthenticatorFunc) Authenticate(
	request *http.Request,
) (auth.Decision, error) {
	if f == nil {
		return auth.Decision{}, authenticationDenied()
	}
	return f(request)
}

// NewAuthenticatedHandler constructs the standard HTTP transport with
// mandatory per-request identity. Every request, including the workspace shell
// and its assets, must carry a decision that the binder accepts before any
// route observes it. Requests that fail authentication are answered with the
// repository's unauthorized shape and never reach the service.
//
// The binder must belong to the same auth.Authority whose resolver the
// service's Explorer client was constructed with; identity is carried per
// request through the context capability rather than through process state, so
// the transport scales unchanged from one to many instances.
//
// allowedAuthorities is the exact-match host-authority allow-list applied to
// every request before authentication (see NewHandler and ServeHTTP); at least
// one is required.
func NewAuthenticatedHandler(
	service Service,
	authenticator Authenticator,
	binder auth.Binder,
	allowedAuthorities ...string,
) (*Handler, error) {
	if isAbsentInterface(authenticator) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace authenticator is required")
	}
	if isAbsentInterface(binder) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace decision binder is required")
	}
	handler, err := NewHandler(service, allowedAuthorities...)
	if err != nil {
		return nil, err
	}
	handler.authenticator = authenticator
	handler.binder = binder
	return handler, nil
}

// authenticate fails closed. Every authenticator or binder failure denies the
// request with shoal.ErrorUnauthorized, which writeError maps to 401.
func (h *Handler) authenticate(
	request *http.Request,
) (context.Context, error) {
	if isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return nil, authenticationDenied()
	}
	decision, err := h.authenticator.Authenticate(request)
	if err != nil {
		return nil, authenticationDenied()
	}
	ctx, err := h.binder.Bind(request.Context(), decision)
	if err != nil || ctx == nil {
		return nil, authenticationDenied()
	}
	// Surface only what the transport already trusts. The identity endpoint
	// reads this projection to show the caller who they are; enforcement stays
	// with the authorized Explorer client behind the resolver.
	return withIdentity(ctx, decision), nil
}

func authenticationDenied() error {
	return shoal.NewError(shoal.ErrorUnauthorized, "authentication required")
}

// isAbsentInterface reports whether an interface value carries no usable
// implementation. A typed nil such as AuthenticatorFunc(nil) or a nil pointer
// receiver is not equal to nil as an interface, so a plain nil comparison
// admits an adapter whose every call panics. Mandatory authentication must
// reject that at construction and deny it at request time, never discover it
// as a crash while serving.
func isAbsentInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}
