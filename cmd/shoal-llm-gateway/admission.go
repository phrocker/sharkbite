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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// The admission client is the proxy's whole relationship with Shoal.
//
// It speaks HTTP to the explorer through the public contract in
// pkg/admission/api rather than importing the fleet package, because the proxy
// is a separate process on purpose: it handles untrusted prompt content from
// arbitrary callers and talks to third-party endpoints, and the explorer holds
// the policy store and the corpus. Linking them would put prompt injection in
// the same address space as the decision plane (#390).
//
// What follows is a thin adapter. The wire types, the grant checks (an unknown
// outcome is not an allowance, a token must be reportable and must echo the
// claim this proxy chose) and the transport rules (no redirects, no cookie
// jar) live in the client; this file maps its answers on to the two errors the
// proxy branches on.

// ErrPlaneUnreachable means the decision could not be obtained at all.
//
// It is deliberately distinct from a denial. A denial is an answer; this is the
// absence of one, and the two must not look alike — to the operator, because
// one is a policy outcome and the other is an outage, and to the caller,
// because a caller told "denied" will not retry while one told "unavailable"
// should.
var ErrPlaneUnreachable = errors.New("shoal-llm-gateway: decision plane unreachable")

// ErrDenied means Shoal refused the call.
var ErrDenied = errors.New("shoal-llm-gateway: admission denied")

// admissionClient asks before the call and reports after it.
type admissionClient struct {
	base       *url.URL
	http       *http.Client
	credential func() (string, error)

	clock           func() time.Time
	agentID         string
	agentGeneration int64
	capability      string
	action          string
	// effects is the set this proxy declares, derived from the configured
	// provider rather than hardcoded. Empty is never correct, so newProxy sets
	// it where the provider is resolved.
	effects  []string
	sourceID []byte
	policyID []byte
	lease    time.Duration

	once   sync.Once
	plane  *admissionapi.Client
	broken error
}

// grant is what the proxy acts on.
type grant struct {
	// Withhold is the subset of references the caller declared that must not
	// appear in what it sends. An obligated grant is still a grant: refusing
	// the whole call when the plane has said which part to drop would discard
	// the useful answer for the blunt one (#390).
	Withhold []string
	token    admissionapi.Token
}

// minimumReportWindow is the margin the report itself needs. A token that
// expires while the upstream call is still running cannot be reported at all,
// so a grant arriving with less than this left is refused rather than spent.
const minimumReportWindow = 5 * time.Second

// client builds the public admission client once, from the configured base
// URL, transport and credential. main builds it eagerly so a base URL the
// client refuses fails at startup rather than on every call.
func (c *admissionClient) client() (*admissionapi.Client, error) {
	c.once.Do(func() {
		if c.base == nil {
			c.broken = errors.New("admission plane URL is not configured")
			return
		}
		c.plane, c.broken = admissionapi.NewClient(admissionapi.Config{
			BaseURL: c.base.String(), HTTPClient: c.http,
			// Read per request, so a rotating credential file works.
			Token: func(context.Context) (string, error) { return c.bearerToken() },
		})
	})
	return c.plane, c.broken
}

// admissionCredentialError is the gateway's own admission token being
// unavailable: unreadable, empty, or holding a line break. It is typed here,
// inside the token closure, because the public client wraps whatever the
// closure returns in an untyped error, and an empty token in one with no cause
// at all. Without the type it is indistinguishable from the plane being down,
// when the fix is on this pod and not on the plane.
type admissionCredentialError struct{ err error }

func (e *admissionCredentialError) Error() string {
	return "admission credential unavailable: " + e.err.Error()
}
func (e *admissionCredentialError) Unwrap() error { return e.err }

// bearerToken reads the admission token for one call. The empty and CR/LF
// checks repeat the client's own, so a token the client would refuse is
// refused here first, with a type.
func (c *admissionClient) bearerToken() (string, error) {
	token, err := c.credential()
	if err != nil {
		return "", &admissionCredentialError{err: err}
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", &admissionCredentialError{
			err: errors.New("the token is empty or holds a line break")}
	}
	return token, nil
}

// request asks whether a call may happen.
//
// declaration is what the call would do, never the prompt. Shoal's admission
// surface takes a declaration precisely so the plane deciding whether content
// may be transmitted does not itself receive a copy of that content, and the
// proxy must not be the thing that undoes it.
func (c *admissionClient) request(
	ctx context.Context,
	identity callerIdentity,
	declaration json.RawMessage,
	disclosures []string,
	now time.Time,
) (grant, error) {
	body := admissionapi.Request{
		Context: admissionapi.RequestContext{
			RequestID:     identity.RequestID,
			CorrelationID: identity.CorrelationID,
			ReasonCode:    "llm_gateway_call",
			Deadline:      now.Add(c.lease),
		},
		ID:              identity.AdmissionID,
		IdempotencyKey:  identity.AdmissionID,
		TokenID:         identity.TokenID,
		AgentID:         c.agentID,
		AgentGeneration: c.agentGeneration,
		Capability:      c.capability,
		Action:          c.action,
		SourceID:        c.sourceID,
		PolicyID:        c.policyID,
		ObjectID:        identity.ObjectID,
		// Derived from the configured provider, not fixed. See
		// declaredEffects: the fleet contract requires it, and declaring
		// egress against a loopback provider is denied rather than refused.
		Effects:     c.effects,
		Input:       declaration,
		Disclosures: disclosures,
		Lease:       c.lease,
	}
	plane, err := c.client()
	if err != nil {
		return grant{}, fmt.Errorf("%w: %v", ErrPlaneUnreachable, err)
	}
	// An allowance the proxy could not report is refused before the egress,
	// not discovered after it: the client refuses an unknown outcome, a
	// missing or unusable token (unparseable or over-long IDs, zero version,
	// no expiry), a token naming a different claim than the one this proxy
	// chose, and one expiring inside minimumReportWindow.
	granted, err := plane.Request(ctx, body, admissionapi.RequestOptions{
		MinReportWindow: minimumReportWindow, Clock: c.clockNow,
	})
	switch {
	case errors.Is(err, admissionapi.ErrDenied):
		return grant{}, ErrDenied
	case err != nil:
		return grant{}, planeError("request", err)
	}
	return grant{Withhold: granted.Withhold, token: *granted.Token}, nil
}

// report closes the loop.
//
// Without it admission is a stateless gate: the plane grants permission and
// never learns whether the call happened. The report is the edge a cross-call
// accumulator charges (#389), which is why a failure to report is logged and
// surfaced rather than discarded — a silently unreported grant is
// indistinguishable from a caller that went dark.
//
// effected is how much escaped before a failure (#427), nil when nothing did.
// It is sent only on a failure, only when it counts at least one byte, and only
// when this proxy declares egresses-content: the plane refuses it in each other
// case, and a refused report is a grant left unreported, which is worse than a
// report without the number.
//
// A report whose answer was lost is resent once, as the identical report. The
// plane's replay comparison includes effected and the field is write-once, so
// the resend is accepted only if it carries the value the first attempt did;
// a different value is a different report and is refused. The value is
// therefore copied into the report here, once, and the resend reuses that
// report — it is never re-derived, from the caller's pointer or anywhere else.
func (c *admissionClient) report(
	ctx context.Context,
	token admissionapi.Token,
	identity callerIdentity,
	outcome json.RawMessage,
	failure string,
	effected *admissionapi.Effected,
	now time.Time,
) error {
	body := admissionapi.Report{
		Context: admissionapi.RequestContext{
			RequestID:     identity.RequestID,
			CorrelationID: identity.CorrelationID,
			ReasonCode:    "llm_gateway_report",
			Deadline:      now.Add(c.lease),
		},
		Token: token,
	}
	// An outcome and a failure are exclusive: the admission surface refuses a
	// report carrying both, because the pair would make an exact-replay
	// comparison undefined.
	if failure != "" {
		body.Failed = true
		body.ErrorCode = failure
		if effected != nil && effected.Bytes > 0 && c.declaresEgress() {
			// A copy: the report is the record of what was sent, and the
			// resend below must carry the same number.
			frozen := *effected
			body.Effected = &frozen
		}
	} else {
		body.Outcome = outcome
	}
	plane, err := c.client()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPlaneUnreachable, err)
	}
	_, err = plane.Report(ctx, body)
	if err != nil && answerLost(err) && ctx.Err() == nil {
		// The same body, not a rebuilt one.
		_, err = plane.Report(ctx, body)
	}
	if err != nil {
		return planeError("report", err)
	}
	return nil
}

// declaresEgress reports whether this proxy's declared effects include
// egresses-content, the only declaration a volume may accompany.
func (c *admissionClient) declaresEgress() bool {
	for _, effect := range c.effects {
		if effect == admissionapi.EffectEgressesContent {
			return true
		}
	}
	return false
}

// answerLost is a report that may have been recorded whose answer did not
// arrive: a transport failure, a 200 the client could not accept (the route
// commits before it answers), a status the plane marked indeterminate, or a
// 502, 503 or 504 that a proxy in front of the plane can answer after the
// plane processed the request. A definite refusal is not lost and is not
// resent: resending it would only repeat the refusal.
func answerLost(err error) bool {
	var protocol *admissionapi.ProtocolError
	if errors.As(err, &protocol) {
		return protocol.Committed
	}
	var status *admissionapi.HTTPError
	if errors.As(err, &status) {
		return status.Indeterminate ||
			status.Status == http.StatusBadGateway ||
			status.Status == http.StatusServiceUnavailable ||
			status.Status == http.StatusGatewayTimeout
	}
	return !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

// planeFault is an ErrPlaneUnreachable that also carries the closed reason it
// is counted under (see metrics.go). The reason is decided here, where the
// error's structure is still visible, so nothing downstream classifies by
// matching error text.
type planeFault struct {
	reason infraReason
	err    error
}

func (f *planeFault) Error() string { return f.err.Error() }
func (f *planeFault) Unwrap() error { return f.err }

func newPlaneFault(reason infraReason, err error) error {
	return &planeFault{reason: reason, err: err}
}

// planeError folds every way of not getting an answer into
// ErrPlaneUnreachable.
func planeError(route string, err error) error {
	var status *admissionapi.HTTPError
	if !errors.As(err, &status) {
		reason := infraPlaneUnreachable
		var protocol *admissionapi.ProtocolError
		var credential *admissionCredentialError
		switch {
		case errors.As(err, &credential):
			// Nothing was sent: this pod could not present its own token.
			reason = infraAdmissionCredentialUnavailable
		case errors.As(err, &protocol):
			// The plane answered and the answer could not be acted on.
			reason = infraPlaneAnswerUnusable
		}
		return newPlaneFault(reason, fmt.Errorf("%w: %v", ErrPlaneUnreachable, err))
	}
	switch status.Status {
	case http.StatusForbidden, http.StatusUnauthorized:
		// The plane answered and refused this proxy's own credential. That is
		// a denial of the proxy, not of the caller, and it is reported as
		// unreachable rather than as a policy denial on the call: the caller's
		// request was never adjudicated.
		return newPlaneFault(infraPlaneCredentialRejected,
			fmt.Errorf("%w: proxy credential rejected", ErrPlaneUnreachable))
	default:
		// A 502, 503 or 504 with no structured error body is what a proxy or
		// load balancer in front of the plane answers when the plane is not
		// there, so it is counted as the plane being unreachable. One the plane
		// wrote itself carries a code, and is the plane answering. A coded 503
		// is its own refusal to admit right now: the whole-store rollout
		// conditions (an unmigrated admission, an occupied reserved span)
		// answer exactly that, and an operator clears them on the plane rather
		// than in the network.
		reason := infraPlaneErrorStatus
		switch {
		case status.Code == "" && (status.Status == http.StatusBadGateway ||
			status.Status == http.StatusServiceUnavailable ||
			status.Status == http.StatusGatewayTimeout):
			reason = infraPlaneUnreachable
		case status.Code != "" && status.Status == http.StatusServiceUnavailable:
			reason = infraPlaneReportedUnavailable
		}
		// The plane's message is deliberately not included. A plane error can
		// carry detail the caller is not entitled to, and this error reaches a
		// caller-facing response.
		return newPlaneFault(reason, fmt.Errorf(
			"%w: admission %s returned %d",
			ErrPlaneUnreachable, route, status.Status))
	}
}

// encodeID renders an opaque identity the way every fleet wire field expects.
func encodeID(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// decodeID parses a configured base64url identity.
func decodeID(name, value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s must be unpadded base64url", name)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("%s is required", name)
	}
	return decoded, nil
}

// newHTTPClient builds every outbound client this proxy uses.
//
// The redirect policy lives here rather than at each call site because it is a
// security property, and a call site that only meant to change the transport
// should not be able to drop it by assigning a fresh http.Client. The test
// harness swaps the transport for the same reason.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: refuseRedirect}
}

// absoluteURL parses a configured base URL and refuses anything that could not
// address a service.
func absoluteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return nil, errors.New("must be an absolute URL with a host")
	}
	if parsed.Scheme != "https" &&
		!(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		// Plain HTTP to a remote endpoint would put the bearer token and every
		// declaration on the wire in clear. Loopback is exempted because it
		// does not leave the host, which is the same line pkg/model draws for
		// model providers.
		//
		// The exemption is for http specifically, not for every scheme on
		// loopback. "ftp://localhost" is not a transport this can speak, and
		// admitting it at startup only moves the failure to every request.
		return nil, errors.New("must use https, or http addressing loopback")
	}
	return parsed, nil
}

// planeURL applies absoluteURL to the admission plane, with the one
// acknowledged exception.
//
// The chart already has llmGateway.admission.allowPlaintext for a mesh that
// supplies the transport authentication the scheme would, and asserts that
// configuration renders — but nothing carried the acknowledgement into the
// process, so absoluteURL refused it at startup and the documented mesh
// deployment produced a pod in CrashLoopBackOff. A chart that renders a
// configuration the binary refuses is the third mismatch of this kind found on
// this work; the acknowledgement is a flag now so the two agree.
//
// It is deliberately narrower than "allow plaintext". The upstream URL carries
// the prompt and the operator's credential and has no opt-out, because a mesh
// that authenticates the hop to the decision plane says nothing about the hop
// to a third-party provider.
func planeURL(raw string, allowPlaintext bool) (*url.URL, error) {
	parsed, err := absoluteURL(raw)
	if err == nil || !allowPlaintext {
		return parsed, err
	}
	// Re-parse under the relaxed rule, still refusing anything that is not an
	// absolute http(s) URL with a host. The acknowledgement covers the
	// transport, not the shape.
	relaxed, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || !relaxed.IsAbs() || relaxed.Hostname() == "" ||
		relaxed.Scheme != "http" {
		return nil, err
	}
	return relaxed, nil
}

// declaredEffects derives the effect set from the provider this proxy was
// configured with, which the fleet contract requires rather than permits.
//
// pkg/explorer/fleet/model.go says it outright: EffectEgressesContent "is not a
// property of the code. The same executor is egress-free against a loopback
// model provider and egress-bearing against a hosted one, so an executor
// declaring this must derive it from the provider it was actually configured
// with."
//
// This declared both classes unconditionally, with a comment arguing that
// declaring less than you do is the understatement #385's effect floor exists
// to refuse. That reasoning is right about understatement and wrong about this:
// the floor refuses declaring too little, so over-declaring is never refused —
// it is silently denied instead. A policy that forbids egress then denies every
// call on a deployment where nothing leaves the host, and the chart supports
// exactly that deployment and asserts it renders. Over-declaration is not the
// safe direction here, it is the invisible one.
//
// reads-corpus stays unconditional: the proxy reads the references the caller
// declared whatever the provider is.
func declaredEffects(upstream *url.URL) []string {
	if upstream != nil && isLoopback(upstream.Hostname()) {
		return []string{admissionapi.EffectReadsCorpus}
	}
	return []string{
		admissionapi.EffectEgressesContent,
		admissionapi.EffectReadsCorpus,
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// clockNow defaults so a zero-value client in a test still has a clock.
func (c *admissionClient) clockNow() time.Time {
	if c == nil || c.clock == nil {
		return time.Now()
	}
	return c.clock()
}

// normalizeAuthority folds a Host or :authority into a comparable form, using
// the standard library for the host:port split so bracketed IPv6 literals are
// handled rather than guessed at.
//
// The host is lowercased because hostnames are case-insensitive, and a single
// trailing dot is dropped so the FQDN-root spelling matches its bare form. Both
// foldings apply to configured and request authorities alike, which is what
// keeps the comparison symmetric.
func normalizeAuthority(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		host, port = trimmed, ""
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return "", false
	}
	if port == "" {
		return host, true
	}
	return net.JoinHostPort(host, port), true
}
