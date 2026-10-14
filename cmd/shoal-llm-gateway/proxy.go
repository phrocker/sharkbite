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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// refuseRedirect stops a redirect from escaping the transport rule.
//
// absoluteURL settles the scheme of the URL that is configured, and a followed
// redirect is a different URL that it never saw. A 307 or 308 preserves the
// method and body, so one hop to http:// replays the prompt — and, on the
// upstream path, the operator's credential — in the clear, past a check that
// passed. Go retains the Authorization header across a same-host redirect,
// which is exactly the case a compromised or misconfigured provider would use.
//
// The 3xx is handed back as the response instead, so the caller sees the
// provider's own answer and the outcome is reported as it happened. This
// matches pkg/model/openai.go, which does the same for the same reason.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// maxRequestBytes bounds what the proxy will read from a caller. An
// OpenAI-compatible body is prompt text; without a bound a caller could make
// the proxy hold arbitrary memory before any decision is taken.
const maxRequestBytes = 8 << 20

// proxy is the enforcement point: admit, apply, forward, report.
type proxy struct {
	admission    *admissionClient
	upstream     *url.URL
	client       *http.Client
	credential   func() (string, error)
	allowedHosts []string
	metrics      *proxyMetrics
	// models the operator has named. Empty means every model is reported as a
	// marker, which keeps caller text out of the declaration by default.
	models map[string]struct{}
	clock  func() time.Time
	// log receives operational events only. It is never given prompt or
	// completion content; see logRefusal.
	log func(string, ...any)
}

func (p *proxy) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", p.completions)
	return p.guardHost(mux)
}

// guardHost refuses any request whose Host is not an exactly configured
// authority, before routing.
//
// Without it the default loopback listener is reachable by DNS rebinding: a
// page in a browser resolves a name it controls to 127.0.0.1 and POSTs to the
// proxy, which then spends the operator's upstream credential on a call the
// operator never made. A JSON body is not protection — a form post with a
// text/plain content type reaches the same handler, and this surface does not
// require a preflight.
//
// Exact match, no wildcard and no suffix form, and X-Forwarded-Host is never
// consulted; each of those is a known bypass. This mirrors the workspace gate
// in pkg/explorer/webapi, which is also why the proxy needs a separate health
// listener: a kubelet addresses the pod by an authority no static list can name.
func (p *proxy) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !p.permits(request.Host) {
			// A fixed body that never echoes the submitted authority, so the
			// refusal cannot be used to probe what this proxy answers to.
			http.Error(writer, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (p *proxy) permits(authority string) bool {
	candidate, ok := normalizeAuthority(authority)
	if !ok {
		return false
	}
	for _, allowed := range p.allowedHosts {
		if allowed == candidate {
			return true
		}
	}
	return false
}

// completions wraps the handler so a forwarded call is reported only once it
// has stopped writing to the caller.
//
// That ordering is what makes the reported volume final (#427). Every byte the
// caller can receive passes through counted, and the report is made after
// admitAndForward returns, when nothing is left that could add one: the count
// read here is the count, so the first report is the whole report — which is
// what the plane's write-once field and its replay comparison require.
func (p *proxy) completions(writer http.ResponseWriter, request *http.Request) {
	counted := &egressCounter{ResponseWriter: writer}
	settled := p.admitAndForward(counted, request)
	if settled == nil {
		return
	}
	// Only a failure carries a volume: on a success the field is refused, and
	// the outcome's response_bytes already says how much was delivered.
	var effected *admissionapi.Effected
	if settled.failure != "" {
		effected = counted.volume()
	}
	p.reportOutcome(
		context.WithoutCancel(request.Context()), settled.token, settled.identity,
		settled.status, settled.transferred, settled.failure, effected)
}

// forwarded is what a call that reached the upstream leaves to be reported.
type forwarded struct {
	token       admissionapi.Token
	identity    callerIdentity
	status      int
	transferred int64
	failure     string
}

// admitAndForward is the whole contract in one function, in the order that
// matters: nothing reaches the upstream before Shoal has answered.
//
// It returns what to report for a call that reached the upstream, and nil when
// it has already reported (or had nothing to report). Every inline report
// below is made before the refusal is written, so none of them follows an
// egress.
func (p *proxy) admitAndForward(writer http.ResponseWriter, request *http.Request) *forwarded {
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil || len(raw) > maxRequestBytes {
		p.refuse(writer, http.StatusRequestEntityTooLarge, "request_too_large",
			"request body exceeds the proxy's bound")
		return nil
	}
	parsed, err := parseChatRequest(raw)
	if err != nil {
		p.refuse(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return nil
	}
	references, err := parsed.references()
	if err != nil {
		p.refuse(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return nil
	}
	// Validated in full before admission, digests included, so a malformed
	// attribution is a 400 that spends no decision.
	attributed, err := parsed.attribution(references)
	if err != nil {
		p.refuse(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return nil
	}
	identity, err := mintCallerIdentity()
	if err != nil {
		// No identity means no admission, and no admission means no call.
		p.countInfrastructural(infraIdentityUnavailable)
		p.refuse(writer, http.StatusServiceUnavailable, "plane_unavailable",
			"the decision plane could not be consulted")
		return nil
	}

	now := p.clock()
	granted, err := p.admission.request(
		request.Context(), identity, parsed.declaration(references, p.models), references, now)
	switch {
	case errors.Is(err, ErrDenied):
		// A policy denial. The body names no policy, compartment or document:
		// a refusal that explains itself is an authorization oracle over
		// whatever it names.
		p.log("admission denied request_id=%s", identity.RequestID)
		p.countPolicy(policyDenied)
		p.refuse(writer, http.StatusForbidden, "denied",
			"the decision plane denied this call")
		return nil
	case errors.Is(err, ErrPlaneUnreachable):
		// Fail closed, and say which kind of failure it is. A caller told
		// "denied" will not retry; one told "unavailable" should. The operator
		// needs the same distinction to tell an outage from a policy change.
		p.log("admission unavailable request_id=%s: %v", identity.RequestID, err)
		p.countAdmissionFailure(request, err)
		p.refuse(writer, http.StatusServiceUnavailable, "plane_unavailable",
			"the decision plane could not be consulted")
		return nil
	case err != nil:
		p.log("admission failed request_id=%s: %v", identity.RequestID, err)
		p.countAdmissionFailure(request, err)
		p.refuse(writer, http.StatusServiceUnavailable, "plane_unavailable",
			"the decision plane could not be consulted")
		return nil
	}

	applied := parsed.applyObligations(granted.Withhold, references, attributed)
	if applied.refusal != "" {
		// The obligation could not be met. This is the one place refusal is
		// correct rather than lazy, and it is reported so the plane learns the
		// obligation was unsatisfiable rather than that the caller went dark.
		//
		// Counts and the refusal class only. Never content, digests, reference
		// IDs or offsets: the class says which rule refused, which is what an
		// operator needs, and nothing about what was being withheld.
		p.log("obligation unsatisfiable request_id=%s withheld=%d class=%s",
			identity.RequestID, len(granted.Withhold), applied.refusal)
		p.countPolicy(policyObligationUnsatisfiable)
		p.reportFailure(request.Context(), granted.token, identity, "obligation_unsatisfiable")
		p.refuse(writer, http.StatusForbidden, "obligation_unsatisfiable",
			"the call cannot be made within the obligations returned")
		return nil
	}
	if len(granted.Withhold) > 0 {
		p.log("obligation applied request_id=%s references=%d segments=%d bytes=%d",
			identity.RequestID, applied.references, applied.segments, applied.bytes)
	}
	outbound := applied.body

	return p.forward(writer, request, granted, identity, outbound, parsed.stream)
}

// forward sends the obligated request upstream and returns what to report.
func (p *proxy) forward(
	writer http.ResponseWriter,
	request *http.Request,
	granted grant,
	identity callerIdentity,
	outbound json.RawMessage,
	stream bool,
) *forwarded {
	// The upstream call is bounded by the grant's real expiry, not only by the
	// configured timeout.
	//
	// validateDurations makes the *configured* lease outlast the configured
	// timeout with room to report, and that is not enough, because the fleet
	// clamps the granted expiry to the deadline this proxy sent before the
	// admission round trip (dispatch_service.go:381-383). A slow plane
	// therefore spends the headroom: with the chart's own 60s lease and 30s
	// timeout, a 28s admission leaves 32s, reportable says yes because 32s
	// exceeds the 5s window, and a 30s upstream call then leaves 2s — the
	// egress happens and the report is refused as expired.
	//
	// So the configured invariant is checked again against what was actually
	// granted. A grant with no room left is refused before the egress rather
	// than after it, which is the only point at which refusing is still worth
	// anything.
	ctx := request.Context()
	if !granted.token.ExpiresAt.IsZero() {
		budget := granted.token.ExpiresAt.Sub(p.clock()) - minimumReportWindow
		if budget <= 0 {
			p.log("grant window exhausted request_id=%s", identity.RequestID)
			p.countInfrastructural(infraGrantWindowExhausted)
			p.reportFailure(ctx, granted.token, identity, "grant_window_exhausted")
			p.refuse(writer, http.StatusServiceUnavailable, "plane_unavailable",
				"the decision plane could not be consulted")
			return nil
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	endpoint := p.upstream.JoinPath("chat", "completions")
	upstreamRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint.String(), bytes.NewReader(outbound))
	if err != nil {
		p.countUpstreamFailure(request, upstreamUnreachable)
		p.reportFailure(request.Context(), granted.token, identity, "upstream_unreachable")
		p.refuse(writer, http.StatusBadGateway, "upstream_unreachable",
			"the upstream provider could not be reached")
		return nil
	}
	// A credential is required of a remote provider and optional for a
	// loopback one, because a local model server generally has no notion of
	// one. The chart and the deployment guide already say so — a case asserts
	// that a loopback upstream renders with no Secret — and this path demanded
	// one unconditionally, so that documented configuration refused every
	// admitted call after admission had been spent on it.
	//
	// Optional is not the same as ignored, and the distinction has to be in the
	// error rather than in a comment. Only ErrNoCredential — nothing supplied —
	// is exempt, and only for loopback. A credential that was configured and
	// could not be read is breakage: it refuses here instead of forwarding the
	// prompt with no Authorization header, which is what a single bare error
	// for both cases used to do to a loopback upstream whose token file had the
	// wrong permissions.
	credential, err := p.credential()
	switch {
	case err != nil &&
		!(errors.Is(err, ErrNoCredential) && isLoopback(p.upstream.Hostname())):
		p.countUpstream(upstreamCredentialUnavailable)
		p.reportFailure(request.Context(), granted.token, identity, "upstream_credential_unavailable")
		p.refuse(writer, http.StatusBadGateway, "upstream_unreachable",
			"the upstream provider could not be reached")
		return nil
	case err == nil:
		upstreamRequest.Header.Set("Authorization", "Bearer "+credential)
	}
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if accept := request.Header.Get("Accept"); accept != "" {
		upstreamRequest.Header.Set("Accept", accept)
	}

	response, err := p.client.Do(upstreamRequest)
	if err != nil {
		p.countUpstreamFailure(request, upstreamUnreachable)
		p.reportFailure(request.Context(), granted.token, identity, "upstream_unreachable")
		p.refuse(writer, http.StatusBadGateway, "upstream_unreachable",
			"the upstream provider could not be reached")
		return nil
	}
	defer response.Body.Close()

	// The egress has now happened. Everything below reports it; nothing below
	// can prevent it, and the proxy does not pretend otherwise — no mid-stream
	// interception, because tokens already sent cannot be recalled (#390).
	// Retry-After travels because a provider's backoff instruction is useless
	// to a caller that never sees it. This repository's own OpenAI client reads
	// the header (pkg/model/openai.go:584), so a Shoal-built caller behind this
	// proxy would retry a 429 immediately against a provider that asked it to
	// wait — the proxy turning a well-behaved client into a badly-behaved one.
	for _, header := range []string{"Content-Type", "Cache-Control", "Retry-After"} {
		if value := response.Header.Get(header); value != "" {
			writer.Header().Set(header, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	transferred, copyErr := p.relay(writer, response.Body, stream)

	failure := ""
	switch {
	case copyErr != nil:
		failure = "response_truncated"
	case response.StatusCode < 200 || response.StatusCode >= 300:
		// Every non-2xx, not only 4xx and 5xx. Redirects are deliberately not
		// followed, so a 3xx is a response with no completion in it — and this
		// read it as successful work, which would feed a result into the next
		// admission for a call that produced nothing. The redirect refusal is
		// what made 3xx reachable as a terminal status, so this is a
		// consequence of that fix rather than an independent oversight.
		failure = "upstream_error"
	}
	// A failure only. A completed call is not counted: the health listener
	// these counters are served on is unauthenticated, and a count of
	// successful calls there is a usage meter (see metrics.go).
	switch failure {
	case "response_truncated":
		p.countUpstreamFailure(request, upstreamResponseTruncated)
	case "upstream_error":
		p.countUpstream(upstreamErrorStatus)
	}
	// Reported by completions, after this returns: see there for why.
	return &forwarded{
		token: granted.token, identity: identity,
		status: response.StatusCode, transferred: transferred, failure: failure,
	}
}

// relay streams the response through, flushing per chunk so a streamed body
// reaches the caller as it arrives rather than when it completes.
func (p *proxy) relay(
	writer http.ResponseWriter, body io.Reader, stream bool,
) (int64, error) {
	if !stream {
		return io.Copy(writer, body)
	}
	flusher, _ := writer.(http.Flusher)
	buffer := make([]byte, 16<<10)
	var total int64
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			written, writeErr := writer.Write(buffer[:read])
			total += int64(written)
			if flusher != nil {
				flusher.Flush()
			}
			if writeErr != nil {
				return total, writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// reportOutcome tells the plane what happened.
//
// The report carries status and size, never the completion — and that is
// structural rather than careful. This function is not given the response
// body, and relay streams it straight to the caller without buffering it, so
// there is no variable here holding a completion for a future edit to pass
// along. Leaking one would take a signature change, which is a different kind
// of mistake from forgetting to redact.
//
// A streamed response is reported once, after it finishes, which is why this
// is the only place a forwarded call reports: reporting before the stream ends
// would record an outcome the proxy had not yet observed. effected is the
// volume of a failure that followed a partial egress, nil when nothing left
// or the call succeeded; completions reads it once the handler has stopped
// writing.
//
// The report is bounded by minimumReportWindow rather than by a timeout of its
// own, because that constant is the margin validateDurations withholds from the
// lease for exactly this call. A separate bound would be a second number
// obliged to agree with the first, and it already did not: the report was
// allowed ten seconds inside a window reserved as five, so a report that took
// the time it was given outlived the lease it was closing. One number cannot
// drift from itself.
func (p *proxy) reportOutcome(
	ctx context.Context,
	token admissionapi.Token,
	identity callerIdentity,
	status int,
	transferred int64,
	failure string,
	effected *admissionapi.Effected,
) {
	outcome, _ := json.Marshal(map[string]any{
		"upstream_status": status,
		"response_bytes":  transferred,
	})
	ctx, cancel := context.WithTimeout(ctx, minimumReportWindow)
	defer cancel()
	if err := p.admission.report(
		ctx, token, identity, outcome, failure, effected, p.clock()); err != nil {
		p.countReportFailure(err)
		// An unreported grant is the gap the report closes, so this is said out
		// loud. The plane already shows it as outstanding; the operator should
		// not have to find it there to learn the proxy could not report.
		p.log("report failed request_id=%s: %v", identity.RequestID, err)
	}
}

func (p *proxy) reportFailure(
	ctx context.Context, token admissionapi.Token, identity callerIdentity, code string,
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), minimumReportWindow)
	defer cancel()
	if err := p.admission.report(
		ctx, token, identity, nil, code, nil, p.clock()); err != nil {
		p.countReportFailure(err)
		p.log("report failed request_id=%s: %v", identity.RequestID, err)
	}
}

// refuse writes a structured refusal in the shape an OpenAI-compatible client
// already understands, so an unmodified client surfaces it as an API error
// rather than as a parse failure.
//
// The detail is a fixed string per class. It never names a policy, a
// compartment, a document or another principal: a refusal that explains itself
// is an oracle over whatever it explains.
func (p *proxy) refuse(
	writer http.ResponseWriter, status int, code, detail string,
) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"type":    code,
			"code":    code,
			"message": detail,
			"param":   nil,
		},
	})
}

// declaredModels folds the operator's list into a set, dropping blanks so a
// trailing comma in a values file does not name an empty model.
func declaredModels(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			set[trimmed] = struct{}{}
		}
	}
	return set
}

func newProxy(
	admission *admissionClient,
	upstream string,
	credential func() (string, error),
	allowedHosts []string,
	models []string,
	timeout time.Duration,
	clock func() time.Time,
	log func(string, ...any),
) (*proxy, error) {
	// The same transport policy as the admission URL, and for a sharper reason:
	// this is the request that carries the operator's upstream credential and
	// the caller's prompt. Allowing remote plain HTTP here would put both on
	// the wire in clear, which is worse than the admission path where only the
	// declaration travels.
	parsed, err := absoluteURL(upstream)
	if err != nil {
		return nil, fmt.Errorf("upstream base URL %v", err)
	}
	// The effect set is a property of the configured provider, so it is derived
	// here, where the provider is resolved, rather than fixed at the admission
	// client's construction, where the provider is not yet known.
	if admission != nil {
		admission.effects = declaredEffects(parsed)
	}
	return &proxy{
		admission: admission, upstream: parsed,
		client:     newHTTPClient(timeout),
		credential: credential, allowedHosts: allowedHosts,
		metrics: &proxyMetrics{},
		models:  declaredModels(models),
		clock:   clock, log: log,
	}, nil
}
