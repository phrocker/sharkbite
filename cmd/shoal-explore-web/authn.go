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

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The workspace serves one authorization domain. Every decision this command
// accepts, whether minted by the development authenticator or by a future
// integration, is scoped to it, and the policy selector registers ingested
// content under the same trusted source and grant identities.
var (
	workspaceAuthorizationDomain = []byte("shoal-explore-web")
	workspaceSourceID            = []byte("shoal-explore-web/workspace")
	workspaceGrantPolicyID       = []byte("shoal-explore-web/workspace-grant")
)

// workspacePolicyGeneration is the immutable configured generation published
// by this build. It is configuration rather than mutable process state, so
// every instance of the command reports the same value.
const workspacePolicyGeneration int64 = 1

// The development principal is deliberately named so it is unmistakable in
// audit output. It is minted only for loopback listeners under -dev-auth.
const (
	developmentSubject shoal.ID = "development-principal@localhost"
	developmentActor   shoal.ID = "shoal-explore-web-dev-auth"
	// developmentAuditPurpose records why the decision exists.
	developmentAuditPurpose = "localhost development workspace"
	// developmentSessionLifetime bounds every minted development credential.
	developmentSessionLifetime = 15 * time.Minute
)

// workspaceOperations is the operation set the workspace UI needs. It is the
// ceiling of the development principal, not of the transport: a real
// authenticator may mint narrower decisions and the authorized Explorer client
// enforces whatever it minted.
var workspaceOperations = []auth.Operation{
	auth.OperationIngest,
	auth.OperationList,
	auth.OperationRead,
	auth.OperationConnect,
	auth.OperationGraphMaterialize,
	auth.OperationNeighborhood,
	auth.OperationRetrieve,
	auth.OperationAnalyticsRead,
	auth.OperationTeamOverviewRead,
	auth.OperationWorkspaceSettingsRead,
	auth.OperationWorkspaceSettingsWrite,
	auth.OperationAgentRegister,
	auth.OperationAgentHeartbeat,
	auth.OperationAgentRevoke,
	auth.OperationAgentResolve,
	auth.OperationDelegate,
	auth.OperationDispatch,
	auth.OperationInvoke,
	// Deliberately no OperationExecute. It would buy this principal nothing:
	// -dev-auth mints one fixed identity, so it is always its own enqueuer and
	// already passes the principal check that the execute routes skip. What it
	// would add is access to those routes — the ones that answer without
	// comparing the caller to the record — for no capability gain. See
	// oidc_execute_grant_test.go, which asserts the absence here too.
	//
	// And no OperationActionApprove (#451): one fixed identity is both every
	// requester and every would-be approver, which is the self-approval the
	// approval service exists to refuse. oidc_approve_grant_test.go asserts
	// the absence.
	auth.OperationSubscriptionCreate,
	auth.OperationSubscriptionDelete,
	auth.OperationSubscriptionDeliver,
	auth.OperationEventPublish,
}

// developmentAuthenticator mints one short-lived decision per request for a
// fixed, clearly-named development principal. It authenticates nothing: it is
// valid only because the listener is proven to be loopback-only, so the only
// callers are processes on this host.
type developmentAuthenticator struct {
	clock    func() time.Time
	lifetime time.Duration
	// labels are the label policy IDs -dev-auth-labels grants (#570). They
	// add visibility and nothing else: the operations and the source are the
	// fixed development ones whatever is listed here.
	labels [][]byte
}

func newDevelopmentAuthenticator(
	clock func() time.Time,
) (*developmentAuthenticator, error) {
	return newDevelopmentAuthenticatorWithLabels(clock, "")
}

// newDevelopmentAuthenticatorWithLabels parses -dev-auth-labels: a
// comma-separated list of <source>=<label>, where the source must be the
// workspace source exactly as configured. '=' is the separator because the
// label charset excludes it and includes ':'; see
// authorized.LabelGrantSeparator.
func newDevelopmentAuthenticatorWithLabels(
	clock func() time.Time, labels string,
) (*developmentAuthenticator, error) {
	if clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "clock is required")
	}
	ids, err := authorized.ParseLabelGrantList(labels, [][]byte{workspaceSourceID})
	if err != nil {
		return nil, fmt.Errorf("-dev-auth-labels: %w", err)
	}
	return &developmentAuthenticator{
		clock:    clock,
		lifetime: developmentSessionLifetime,
		labels:   ids,
	}, nil
}

// Authenticate returns a fresh decision with a unique request identity so that
// two concurrent requests are never correlated as one.
func (a *developmentAuthenticator) Authenticate(
	request *http.Request,
) (auth.Decision, error) {
	if request == nil {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "request is required")
	}
	return a.mint(request)
}

// mint issues one short-lived development decision.
func (a *developmentAuthenticator) mint(
	request *http.Request,
) (auth.Decision, error) {
	requestID, err := newRequestID()
	if err != nil {
		return auth.Decision{}, err
	}
	correlationID, err := correlationIDFor(request, "dev-correlation-")
	if err != nil {
		return auth.Decision{}, err
	}
	policies := append([][]byte{workspaceGrantPolicyID}, a.labels...)
	return auth.NewDecision(auth.DecisionConfig{
		Subject:               developmentSubject,
		Actor:                 developmentActor,
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     workspaceOperations,
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: a.clock().Add(a.lifetime),
		RequestID:             requestID,
		CorrelationID:         correlationID,
		AuditPurpose:          developmentAuditPurpose,
	})
}

// CorrelationIDHeader threads a caller's trace across a hop.
//
// RequestID identifies one request; CorrelationID groups the related ones. A
// caller with no upstream context is the root of its own trace and gets a
// generated value; a caller that is itself handling an upstream request — a
// gateway, an agent acting for a user — supplies this so the correlation
// survives the hop, which is the field's entire purpose.
const CorrelationIDHeader = "Shoal-Correlation-ID"

// correlationIDFor resolves the correlation ID a minted decision carries.
//
// Every dispatch, admission and approval route required this and neither
// shipped authenticator set it, so every such request was refused with
// "dispatch correlation ID is required" (#524). The asymmetry was the whole
// bug: one of the two provenance identifiers was minted and the other was not,
// and the bound wrappers then overwrote the request body's value with the
// decision's empty one — correctly, because the authenticated decision has to
// be authoritative so a body cannot forge provenance. So the fix belongs here,
// at the mint, and not there.
//
// A supplied value is honoured rather than ignored, and honouring it is safe
// to reason about: AuthorizationFingerprint deliberately excludes request IDs
// and correlation, so a caller-chosen correlation cannot shift an
// authorization decision. It is provenance for grouping, not authority.
//
// A malformed value is refused rather than silently replaced, so a caller that
// meant to thread a trace learns that it did not.
func correlationIDFor(
	request *http.Request, prefix string,
) (shoal.ID, error) {
	if request != nil {
		values := request.Header.Values(CorrelationIDHeader)
		if len(values) > 1 {
			return "", shoal.NewError(
				shoal.ErrorInvalidArgument,
				"correlation ID header must appear at most once")
		}
		if len(values) == 1 {
			return validateSuppliedCorrelationID(values[0])
		}
	}
	generated, err := newCorrelationID(prefix)
	if err != nil {
		return "", err
	}
	// The generated value meets the same rule a supplied one does, so every
	// correlation a decision carries is one the interaction audit records.
	if err := interaction.ValidateCorrelationID(generated); err != nil {
		return "", shoal.WrapError(shoal.ErrorInternal,
			"generated correlation ID is not recordable", err)
	}
	return generated, nil
}

// validateSuppliedCorrelationID refuses a caller's value that this process
// would not want in an audit record.
//
// The character rule is interaction.ValidateCorrelationID, the same function
// the interaction session boundary applies (#532), so one rule governs what is
// minted and what is recorded. Its own loop here once ranged over the string,
// which turns an invalid UTF-8 byte into U+FFFD — printable — so a header
// net/http accepts as obs-text passed the mint, reached the approval and
// action records raw, and was then silently dropped from the audit session.
// The value is caller-controlled and reaches durable records that something
// eventually renders, where a control character is at best unreadable and at
// worst a terminal escape. Only the header-specific checks — padding and
// emptiness — are made here.
func validateSuppliedCorrelationID(value string) (shoal.ID, error) {
	id := shoal.ID(strings.TrimSpace(value))
	if string(id) != value {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"correlation ID must not be padded with whitespace")
	}
	if err := shoal.ValidateRequiredID("correlation ID", id); err != nil {
		return "", err
	}
	if err := interaction.ValidateCorrelationID(id); err != nil {
		return "", err
	}
	return id, nil
}

func newCorrelationID(prefix string) (shoal.ID, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", shoal.WrapError(
			shoal.ErrorUnavailable, "correlation identity unavailable", err)
	}
	return shoal.ID(prefix + hex.EncodeToString(raw)), nil
}

func newRequestID() (shoal.ID, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", shoal.WrapError(
			shoal.ErrorUnavailable, "request identity unavailable", err)
	}
	return shoal.ID("dev-request-" + hex.EncodeToString(raw)), nil
}

// fixedGenerationReader publishes the immutable configured authorization
// generation for the workspace domain. It holds no mutable process state, so
// every instance of the command answers identically. An unknown domain reports
// zero, which the generation guard treats as unavailable and fails closed.
type fixedGenerationReader struct {
	domain     []byte
	generation int64
}

func (r fixedGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !bytes.Equal(domain, r.domain) {
		return 0, nil
	}
	return r.generation, nil
}

// lookupListenHost resolves a listen hostname to its addresses. It is a
// variable so tests can pin the multi-address cases that DNS cannot be relied
// on to produce.
var lookupListenHost = net.LookupIP

// listenAddressIsLoopback reports whether a listen address is bound
// exclusively to loopback interfaces. It resolves names but never binds, so it
// classifies both the requested flag value and the resolved listener address.
//
// An unspecified address binds every interface and is never loopback, so
// ":8080", "0.0.0.0:8080", and "[::]:8080" are all rejected. Literal addresses
// are classified by net.IP.IsLoopback. A hostname is loopback only when every
// address it resolves to is loopback: one non-loopback candidate makes the
// whole bind non-loopback, and an unresolvable name is not loopback.
func listenAddressIsLoopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return false
		}
		return ip.IsLoopback()
	}
	resolved, err := lookupListenHost(host)
	if err != nil || len(resolved) == 0 {
		return false
	}
	for _, ip := range resolved {
		if ip.IsUnspecified() || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// selectAuthenticator fails closed. The workspace refuses to serve any request
// without a trusted decision. It returns the real OIDC authenticator when OIDC
// configuration is supplied, which unlocks a non-loopback listener; it
// returns the development principal only for -dev-auth on a loopback listener;
// and it refuses supplying both, which is a configuration error rather than a
// silent preference of one over the other.
func selectAuthenticator(
	developmentAuth bool,
	developmentLabels string,
	oidc oidcConfig,
	address string,
	clock func() time.Time,
) (webapi.Authenticator, error) {
	if developmentLabels != "" && !developmentAuth {
		return nil, fmt.Errorf(
			"refusing to serve %s: -dev-auth-labels grants labels to the "+
				"-dev-auth principal and means nothing without -dev-auth; "+
				"OIDC label grants come from -oidc-label-grants-file",
			address)
	}
	oidcConfigured := oidc.configured()
	if developmentAuth && oidcConfigured {
		return nil, fmt.Errorf(
			"refusing to serve %s: -dev-auth and the OIDC authenticator are "+
				"mutually exclusive. -dev-auth mints the %s development "+
				"principal on a loopback listener only, while the OIDC "+
				"authenticator validates real bearer tokens for a public "+
				"listener. Configure exactly one",
			address, developmentSubject,
		)
	}
	if oidcConfigured {
		// A validated OIDC token is trusted on any listener, so a non-loopback
		// bind is allowed here: this is the unlock the development principal
		// could never provide.
		authenticator, err := newOIDCAuthenticator(oidc, clock)
		if err != nil {
			return nil, fmt.Errorf(
				"refusing to serve %s with the OIDC authenticator: %w",
				address, err)
		}
		return authenticator, nil
	}
	if !developmentAuth {
		return nil, fmt.Errorf(
			"refusing to serve %s without authentication: no authenticator is "+
				"configured; pass -dev-auth to mint the %s development "+
				"principal on a loopback listener, or supply a real "+
				"authenticator before exposing the Explorer API",
			address, developmentSubject,
		)
	}
	if !listenAddressIsLoopback(address) {
		return nil, fmt.Errorf(
			"refusing to serve %s with -dev-auth: the %s development principal "+
				"is granted the whole workspace corpus and is only safe on a "+
				"loopback listener; bind 127.0.0.1 or [::1], or supply a real "+
				"authenticator",
			address, developmentSubject,
		)
	}
	return newDevelopmentAuthenticatorWithLabels(clock, developmentLabels)
}
