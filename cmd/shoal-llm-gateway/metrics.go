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
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/phrocker/shoal-oss/internal/healthsurface"
	"github.com/phrocker/shoal-oss/internal/promtext"
)

// The gateway's metrics are served on the health listener (#425), and that
// listener is unauthenticated: anyone who can reach the probe port can scrape
// it. That decides what may be counted here, and it is narrower than "nothing
// derived from content".
//
// Only the process's own failures and refusals are counted, each by a reason
// drawn from a closed set fixed in this file. Nothing that measures tenant
// workload or caller behaviour is: no total of calls, no count of allowed or
// forwarded calls, no bytes or tokens, nothing in flight, no count of callers
// hanging up, and nothing per model, reference, principal, policy or
// document. A count of successful calls on an
// unauthenticated port is a usage meter for whoever is behind the gateway; a
// workload metric belongs behind authentication, as a separate decision.
//
// Every label value is a constant from the tables below, never a value taken
// from a request, an error or the plane's answer. That is what keeps the label
// set bounded and keeps a refusal that names no policy from being undone by a
// metric that does.

// infraReason is why the gateway refused a call that policy did not: the
// decision could not be obtained or could not be acted on. The caller is told
// 503 plane_unavailable for each, which a client treats as retryable, unlike
// the 403 a policy denial answers.
type infraReason int

const (
	// The plane gave no answer: transport failure, timeout, a 502, 503 or 504
	// from whatever fronts it, or a client that could not be built.
	infraPlaneUnreachable infraReason = iota
	// The plane answered 401 or 403 to the gateway's own credential. The
	// caller's request was never adjudicated; this is a gateway credential
	// problem, not a policy outcome.
	infraPlaneCredentialRejected
	// The plane answered and said it cannot admit now: a 503 carrying the
	// plane's own error code, as the whole-store rollout conditions answer.
	// An operator clears it on the plane; nothing is down.
	infraPlaneReportedUnavailable
	// Any other non-2xx answer from the plane, including a coded 502 or 504.
	infraPlaneErrorStatus
	// The plane answered 200 with something the gateway cannot act on: an
	// unknown outcome, an unusable token, a token naming another claim, or a
	// token expiring inside the report window.
	infraPlaneAnswerUnusable
	// The grant arrived with too little of its lease left to make the call and
	// still report it.
	infraGrantWindowExhausted
	// The gateway's own admission token could not be read, or was empty or
	// held a line break. Nothing was sent to the plane.
	infraAdmissionCredentialUnavailable
	// The gateway could not generate the identity an admission carries. Since
	// Go 1.24 crypto/rand.Read does not return an error, so this stays at
	// zero; it is kept so the refusal path that still exists is counted under
	// its own name rather than silently, should the source of IDs change.
	infraIdentityUnavailable
	infraReasonCount
)

var infraReasonNames = [infraReasonCount]string{
	infraPlaneUnreachable:               "plane_unreachable",
	infraPlaneCredentialRejected:        "plane_credential_rejected",
	infraPlaneReportedUnavailable:       "plane_reported_unavailable",
	infraPlaneErrorStatus:               "plane_error_status",
	infraPlaneAnswerUnusable:            "plane_answer_unusable",
	infraAdmissionCredentialUnavailable: "admission_credential_unavailable",
	infraGrantWindowExhausted:           "grant_window_exhausted",
	infraIdentityUnavailable:            "identity_unavailable",
}

// planeReasons are the infraReasons a call to the plane can fail with, and so
// the reasons a report failure is counted under.
var planeReasons = []infraReason{
	infraPlaneUnreachable, infraPlaneCredentialRejected,
	infraPlaneReportedUnavailable, infraPlaneErrorStatus,
	infraPlaneAnswerUnusable, infraAdmissionCredentialUnavailable,
}

// policyReason is a refusal that is the plane's answer.
type policyReason int

const (
	policyDenied policyReason = iota
	// The plane allowed the call with obligations the gateway cannot satisfy.
	policyObligationUnsatisfiable
	policyReasonCount
)

var policyReasonNames = [policyReasonCount]string{
	policyDenied:                  "denied",
	policyObligationUnsatisfiable: "obligation_unsatisfiable",
}

// upstreamReason is why an admitted call did not complete at the provider.
type upstreamReason int

const (
	// A provider credential was configured and could not be read.
	upstreamCredentialUnavailable upstreamReason = iota
	// The request could not be built or the provider could not be reached.
	upstreamUnreachable
	// The provider answered a status outside 2xx.
	upstreamErrorStatus
	// The provider's response stopped part-way.
	upstreamResponseTruncated
	upstreamReasonCount
)

var upstreamReasonNames = [upstreamReasonCount]string{
	upstreamCredentialUnavailable: "credential_unavailable",
	upstreamUnreachable:           "unreachable",
	upstreamErrorStatus:           "error_status",
	upstreamResponseTruncated:     "response_truncated",
}

// proxyMetrics holds the counters. Each is an atomic so a scrape reads them
// concurrently with the calls that update them.
type proxyMetrics struct {
	infrastructural [infraReasonCount]atomic.Uint64
	policy          [policyReasonCount]atomic.Uint64
	upstream        [upstreamReasonCount]atomic.Uint64
	reportFailures  [infraReasonCount]atomic.Uint64
}

func (p *proxy) countInfrastructural(reason infraReason) {
	if p.metrics != nil && reason >= 0 && reason < infraReasonCount {
		p.metrics.infrastructural[reason].Add(1)
	}
}

func (p *proxy) countPolicy(reason policyReason) {
	if p.metrics != nil && reason >= 0 && reason < policyReasonCount {
		p.metrics.policy[reason].Add(1)
	}
}

func (p *proxy) countUpstream(reason upstreamReason) {
	if p.metrics != nil && reason >= 0 && reason < upstreamReasonCount {
		p.metrics.upstream[reason].Add(1)
	}
}

func (p *proxy) countReportFailure(err error) {
	if p.metrics != nil {
		p.metrics.reportFailures[planeFailureReason(err)].Add(1)
	}
}

// countAdmissionFailure counts an admission that produced no usable decision.
//
// A caller that hung up while the plane was being asked is not counted at all.
// Its cancelled context is what failed the request, and an outage signal a
// client timeout can raise is not one an operator can trust. Nor is it counted
// as a hang-up: how often callers give up is caller behaviour that grows with
// traffic, which this unauthenticated port does not carry. A count of it is a
// follow-up for an authenticated surface.
func (p *proxy) countAdmissionFailure(request *http.Request, err error) {
	if request.Context().Err() != nil {
		return
	}
	p.countInfrastructural(planeFailureReason(err))
}

// countUpstreamFailure is countAdmissionFailure for the provider leg.
func (p *proxy) countUpstreamFailure(request *http.Request, reason upstreamReason) {
	if request.Context().Err() != nil {
		return
	}
	p.countUpstream(reason)
}

// planeFailureReason maps an admission error to its closed reason. Anything
// not classified where it was produced is counted as unreachable, which is the
// meaning ErrPlaneUnreachable already carries: no usable answer.
func planeFailureReason(err error) infraReason {
	var fault *planeFault
	if errors.As(err, &fault) && fault.reason >= 0 && fault.reason < infraReasonCount {
		return fault.reason
	}
	return infraPlaneUnreachable
}

// healthConfig is what the gateway adds to its health listener.
func (p *proxy) healthConfig() healthsurface.Config {
	return healthsurface.Config{Metrics: p.writeMetrics}
}

// writeMetrics renders the exposition. Every series is written at zero from
// the first scrape, so an alert on a rate has a baseline rather than a series
// that appears only once something has already gone wrong.
func (p *proxy) writeMetrics(builder *strings.Builder) {
	metrics := p.metrics
	if metrics == nil {
		metrics = &proxyMetrics{}
	}
	family(builder, "shoal_llm_gateway_infrastructural_denials_total",
		"Calls refused because a decision could not be obtained or acted on, by closed reason. Not policy outcomes.")
	for reason, name := range infraReasonNames {
		sample(builder, "shoal_llm_gateway_infrastructural_denials_total",
			"reason", name, metrics.infrastructural[reason].Load())
	}
	family(builder, "shoal_llm_gateway_policy_refusals_total",
		"Calls refused by the decision plane's answer, by closed reason.")
	for reason, name := range policyReasonNames {
		sample(builder, "shoal_llm_gateway_policy_refusals_total",
			"reason", name, metrics.policy[reason].Load())
	}
	family(builder, "shoal_llm_gateway_upstream_failures_total",
		"Admitted calls the upstream provider did not complete, by closed reason.")
	for reason, name := range upstreamReasonNames {
		sample(builder, "shoal_llm_gateway_upstream_failures_total",
			"reason", name, metrics.upstream[reason].Load())
	}
	family(builder, "shoal_llm_gateway_report_failures_total",
		"Admission reports the decision plane did not acknowledge; each leaves a grant outstanding.")
	for _, reason := range planeReasons {
		sample(builder, "shoal_llm_gateway_report_failures_total",
			"reason", infraReasonNames[reason], metrics.reportFailures[reason].Load())
	}
}

func family(builder *strings.Builder, name, help string) {
	builder.WriteString("# HELP " + name + " " + promtext.EscapeHelp(help) + "\n")
	builder.WriteString("# TYPE " + name + " counter\n")
}

func sample(builder *strings.Builder, name, label, value string, count uint64) {
	builder.WriteString(name + "{" + promtext.Label(label, value) + "} " +
		strconv.FormatUint(count, 10) + "\n")
}
