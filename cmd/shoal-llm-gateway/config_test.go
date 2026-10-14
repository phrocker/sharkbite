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
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/healthsurface"
	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// TestTheRoleFieldCannotCarryThePrompt closes the channel the declaration
// guarantee was open to.
//
// A role is a free-form string on the wire, and the declaration copied it
// verbatim. TestShoalNeverReceivesThePrompt could not see it, because that test
// puts its secret in content — the field anyone would think to check. A
// guarantee about shape has to hold for every string that travels, not the
// obvious one.
func TestTheRoleFieldCannotCarryThePrompt(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	const secret = "a-prompt-smuggled-through-the-role-field"
	post(t, governed,
		`{"model":"gpt","messages":[{"role":"`+secret+`","content":"hi"}]}`)

	if len(plane.requests) != 1 {
		t.Fatalf("admissions = %d", len(plane.requests))
	}
	everything, err := json.Marshal(plane.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(everything), secret) {
		t.Fatalf("the role reached the decision plane: %s", everything)
	}
	// The declaration still has to say something useful about the roles, or
	// sanitising them has thrown away the signal along with the content.
	if !strings.Contains(string(plane.requests[0].Input), roleOther) {
		t.Fatalf("an unknown role left no trace at all: %s", plane.requests[0].Input)
	}
	// A known role is reported as itself. Collapsing everything to a marker
	// would be safe and useless.
	plane.requests = nil
	post(t, governed, plainCall)
	if !strings.Contains(string(plane.requests[0].Input), "user") {
		t.Fatalf("a known role was not reported: %s", plane.requests[0].Input)
	}
}

// TestTheRequestKeepsTheCallersOwnRole is the other half: only the declaration
// is sanitised. Rewriting the caller's role would make the proxy the reason an
// unmodified client gets a different answer.
func TestTheRequestKeepsTheCallersOwnRole(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	post(t, governed,
		`{"model":"gpt","messages":[{"role":"custom-agent","content":"hi"}]}`)
	if !strings.Contains(string(upstream.received), "custom-agent") {
		t.Fatalf("the caller's role was rewritten: %s", upstream.received)
	}
}

// TestRemotePlaintextEndpointsAreRefused covers both URLs with one rule. The
// upstream request carries the operator's credential and the caller's prompt,
// so plain HTTP there is worse than on the admission path where only the
// declaration travels.
func TestRemotePlaintextEndpointsAreRefused(t *testing.T) {
	for _, probe := range []struct {
		name    string
		url     string
		refused bool
	}{
		{"remote https", "https://api.example.test", false},
		{"loopback http", "http://127.0.0.1:11434", false},
		{"localhost http", "http://localhost:11434", false},
		{"remote plaintext", "http://api.example.test", true},
		// The loopback exemption is for http specifically. Admitting any
		// scheme on loopback only moves the failure from startup to every
		// request, where it is far more expensive to diagnose.
		{"non-http loopback", "ftp://localhost/models", true},
		{"relative", "/v1", true},
		{"hostless", "https://", true},
	} {
		_, err := absoluteURL(probe.url)
		if probe.refused && err == nil {
			t.Fatalf("%s (%s) was accepted", probe.name, probe.url)
		}
		if !probe.refused && err != nil {
			t.Fatalf("%s (%s) was refused: %v", probe.name, probe.url, err)
		}
	}
}

// TestTheProxyRefusesAPlaintextUpstreamAtConstruction is the call-site half.
//
// Testing absoluteURL alone proves the rule exists, not that the upstream URL
// goes through it — swapping newProxy back to a bare url.Parse passed every
// absoluteURL assertion. The rule and its application need separate tests.
func TestTheProxyRefusesAPlaintextUpstreamAtConstruction(t *testing.T) {
	client := &admissionClient{}
	for _, probe := range []struct {
		name    string
		url     string
		refused bool
	}{
		{"remote plaintext", "http://api.example.test", true},
		{"non-http loopback", "ftp://localhost/models", true},
		{"remote https", "https://api.example.test", false},
		{"loopback http", "http://127.0.0.1:11434", false},
	} {
		_, err := newProxy(
			client, probe.url, func() (string, error) { return "k", nil },
			[]string{"example.test"}, nil, time.Minute, time.Now,
			func(string, ...any) {})
		if probe.refused && err == nil {
			t.Fatalf("%s upstream (%s) was accepted", probe.name, probe.url)
		}
		if !probe.refused && err != nil {
			t.Fatalf("%s upstream (%s) was refused: %v", probe.name, probe.url, err)
		}
	}
}

// TestAnUnreportableTokenIsRefusedBeforeTheEgress is the fail-closed rule the
// nil check only half enforced.
//
// Checking for nil let every other unreportable shape through, and in each case
// the call would have been forwarded and the report then rejected — the exact
// unreportable grant the refusal exists to prevent, discovered after the
// irreversible part.
func TestAnUnreportableTokenIsRefusedBeforeTheEgress(t *testing.T) {
	now := time.Now()
	for _, probe := range []struct {
		name  string
		token *admissionapi.Token
		// echo makes the fake carry back the token ID this proxy sent. Every
		// probe not about the ID itself needs it: without it the probe is
		// refused for naming a different claim before the property it names
		// is ever checked, and the check under test could be deleted unseen.
		echo bool
	}{
		{"absent", nil, false},
		{"empty", &admissionapi.Token{}, false},
		{"unparseable action ID", &admissionapi.Token{
			ActionID: "not base64!", TokenID: "dG9rZW4", Version: 1,
			ExpiresAt: now.Add(time.Minute)}, true},
		{"unparseable token ID", &admissionapi.Token{
			ActionID: "YWN0aW9u", TokenID: "not base64!", Version: 1,
			ExpiresAt: now.Add(time.Minute)}, false},
		{"zero version", &admissionapi.Token{
			ActionID: "YWN0aW9u", TokenID: "dG9rZW4", Version: 0,
			ExpiresAt: now.Add(time.Minute)}, true},
		{"already expired", &admissionapi.Token{
			ActionID: "YWN0aW9u", TokenID: "dG9rZW4", Version: 1,
			ExpiresAt: now.Add(-time.Second)}, true},
		{"expiring inside the report window", &admissionapi.Token{
			ActionID: "YWN0aW9u", TokenID: "dG9rZW4", Version: 1,
			ExpiresAt: now.Add(minimumReportWindow / 2)}, true},
		// The byte bound is covered separately, in
		// TestAnOversizedActionIDIsRefusedBeforeTheEgress. Probes for it were
		// here first and could not fail: they ran without token echo, so every
		// token was already refused for naming a different claim, and the
		// bound was never what the assertion measured. The expiry probes had
		// the same flaw until they were given an echoing plane (see echo).
		//
		// The shape the check was written to let through. It read "if an
		// expiry is set and it is too close", so a plane answering without one
		// produced a token this function called reportable while nothing could
		// establish a window for it. Absent is not generous here, it is
		// unknown, and an unknown deadline cannot be shown to leave room —
		// which makes this the one case the whole guard most needed to catch.
		{"no expiry at all", &admissionapi.Token{
			ActionID: "YWN0aW9u", TokenID: "dG9rZW4", Version: 1}, true},
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		plane.token = probe.token
		plane.echoTokenID = probe.echo
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)

		recorder := post(t, governed, plainCall)
		if recorder.Code != 503 {
			t.Fatalf("%s token: status = %d, want 503", probe.name, recorder.Code)
		}
		if upstream.calls != 0 {
			t.Fatalf("%s token: the egress happened anyway", probe.name)
		}
		// Refused at admission, not discovered later: the proxy's own
		// pre-egress budget check would also stop an expiring grant, but it
		// spends a failure report doing so. No report means the grant never
		// got past the reportability check this table exists to pin.
		if len(plane.reports) != 0 {
			t.Fatalf("%s token: refused only after admission (%d reports)",
				probe.name, len(plane.reports))
		}
	}

	// A usable token is still accepted, or the check above is just a refusal.
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	if recorder := post(t, governed, plainCall); recorder.Code != 200 {
		t.Fatalf("a usable token was refused: %d", recorder.Code)
	}
}

// TestTheLeaseMustOutlastTheCallItAdmits pins a configuration invariant rather
// than a code path.
//
// The shipped defaults violated it: a one-minute lease against a two-minute
// timeout meant every call over a minute was forwarded and then had its report
// rejected as expired. That is an unreportable grant guaranteed by
// configuration, which no amount of runtime checking recovers.
//
// The upper bound is separate and just as sharp: the fleet refuses a lease
// above its claim ceiling rather than shortening it, so too large a value does
// not degrade — it denies every call.
func TestTheLeaseMustOutlastTheCallItAdmits(t *testing.T) {
	for _, probe := range []struct {
		name    string
		lease   time.Duration
		timeout time.Duration
		refused string
	}{
		{"lease shorter than the timeout", time.Minute, 2 * time.Minute, "-lease must exceed"},
		{"lease equal to the timeout", 90 * time.Second, 90 * time.Second, "-lease must exceed"},
		{"lease inside the report window", 92 * time.Second, 90 * time.Second, "-lease must exceed"},
		{"lease above the fleet ceiling", 10 * time.Minute, time.Minute, "-lease must not exceed"},
		{"zero lease", 0, time.Minute, "must be positive"},
		{"zero timeout", time.Minute, 0, "must be positive"},
	} {
		err := validateDurations(probe.lease, probe.timeout)
		if err == nil {
			t.Fatalf("%s was accepted", probe.name)
		}
		if !strings.Contains(err.Error(), probe.refused) {
			t.Fatalf("%s refused for the wrong reason: %v", probe.name, err)
		}
	}

	// A lease that leaves room is accepted, and the fleet ceiling itself is
	// allowed — a rule nothing valid passes is a rule against the feature.
	for _, probe := range []struct {
		name    string
		lease   time.Duration
		timeout time.Duration
	}{
		{"room to report", 4 * time.Minute, 90 * time.Second},
		{"exactly the ceiling", admissionapi.MaxLease, time.Minute},
	} {
		if err := validateDurations(probe.lease, probe.timeout); err != nil {
			t.Fatalf("%s was refused: %v", probe.name, err)
		}
	}
}

// TestTheShippedDefaultsSatisfyTheirOwnInvariant is the case that matters most,
// because the previous defaults did not. Reading the flag block is not enough:
// the two values are declared forty lines apart.
func TestTheShippedDefaultsSatisfyTheirOwnInvariant(t *testing.T) {
	lease, timeout := defaultLease, defaultRequestTimeout
	if err := validateDurations(lease, timeout); err != nil {
		t.Fatalf("the shipped defaults violate the invariant: %v", err)
	}
}

// proxyArgs is a configuration that run() accepts, so a probe can change one
// field and attribute the refusal to that field.
func proxyArgs(overrides ...string) []string {
	args := []string{
		"-admission-url", "https://workspace.test",
		"-upstream-base-url", "https://api.example.test",
		"-agent-id", "YWdlbnQ",
		"-agent-generation", "1",
		"-capability", "llm.gateway",
		"-action", "complete",
		"-source-id", "c291cmNl",
		"-policy-id", "cG9saWN5",
	}
	return append(args, overrides...)
}

// refusal drives run() and returns why it refused. Every probe here refuses
// during configuration, which is before run() binds a listener — the point of
// the exercise is that these are startup failures and not per-call ones.
//
// It must not hang when a probe is wrongly accepted, which is the state a
// mutation deliberately creates: run() then reaches Serve and blocks forever,
// so the mutant looks like a timeout instead of a failed assertion and the
// whole suite stalls. An accepted configuration is cancelled and reported as
// what it is. The listener is also asked for port 0 so a probe that does get
// that far cannot collide with anything.
func refusal(t *testing.T, args []string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, append([]string{"-listen", "127.0.0.1:0"}, args...), io.Discard)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("configuration was accepted: %v", args)
		}
		return err.Error()
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatalf("configuration was accepted and served: %v", args)
		return ""
	}
}

// TestAMisencodedAgentIDIsRefusedAtStartup covers a contract that was stated
// nowhere and could not be guessed.
//
// The workspace decodes agent_id as unpadded base64url. The flag documented no
// encoding and sent any string verbatim, so a descriptor's readable name — the
// obvious thing to pass — was denied once per call as a descriptor that does
// not exist. The operator reads that as a registration problem and goes looking
// in the wrong place.
//
// What this check does and does not reach is worth being exact about, because
// the gap is not fixable here. A shoal.ID is opaque and variable-length, so
// there is no canonical width to compare against, and a readable name that
// happens to decode is indistinguishable from a real ID at startup: of fifteen
// plausible names, eleven decode cleanly to garbage. Those still fail at the
// first call. What moves to startup is the encoding mistakes below — and the
// value of that is not the count, it is that the error names the encoding
// instead of blaming the registry.
func TestAMisencodedAgentIDIsRefusedAtStartup(t *testing.T) {
	for _, probe := range []struct{ name, id, refused string }{
		// Undecodable length: anything with len%4 == 1, which catches a good
		// share of short names ("proxy", "agent", "llm-proxy").
		{"a name of undecodable length", "llm-proxy", "base64url"},
		{"a short name of undecodable length", "proxy", "base64url"},
		// Padding and the standard alphabet are the two ways someone who does
		// know it is base64 still gets it wrong.
		{"padded base64", "YWdlbnQ=", "base64url"},
		{"standard base64 alphabet", "YWdlbnQ/", "base64url"},
		{"empty", "", "-agent-id"},
	} {
		detail := refusal(t, proxyArgs("-agent-id", probe.id))
		if !strings.Contains(detail, probe.refused) {
			t.Fatalf("%s refused for the wrong reason: %s", probe.name, detail)
		}
	}

	// The honest half of the contract, pinned so it is not mistaken for a
	// guarantee later: a name that decodes is accepted here. If this ever
	// starts refusing, something has learned to tell IDs apart locally and
	// this test should be replaced rather than deleted.
	if _, err := decodeID("-agent-id", "gateway"); err != nil {
		t.Fatalf("a decodable name was refused, so the stated gap has closed: %v", err)
	}
}

// TestTheAdmissionTokenCanComeFromAFile covers the form a rotating credential
// actually takes.
//
// A projected ServiceAccount token is a file the kubelet rewrites in place when
// it rotates. It never updates an environment variable, so a proxy that can
// only read env could not use one at all — it would have to be given a static
// secret, which is the thing projected tokens exist to avoid.
func TestTheAdmissionTokenCanComeFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := credentialSource("-admission-token", "", path, false)
	if err != nil {
		t.Fatal(err)
	}
	value, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if value != "first-token" {
		t.Fatalf("token = %q, want the trimmed file contents", value)
	}

	// Read per call, not captured. If the file were read once at startup the
	// proxy would keep presenting the token that has since expired, which is
	// the entire failure mode rotation introduces.
	if err := os.WriteFile(path, []byte("rotated-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err = read(); err != nil || value != "rotated-token" {
		t.Fatalf("token = %q, %v; want the rotated value", value, err)
	}

	// An unreadable or empty file is an error at use time rather than a silent
	// empty bearer token, which the workspace would reject as unauthenticated
	// and the operator would read as a policy problem.
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = read(); err == nil {
		t.Fatal("an empty token file was accepted")
	}
	absent, err := credentialSource(
		"-admission-token", "", filepath.Join(t.TempDir(), "absent"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = absent(); err == nil {
		t.Fatal("a missing token file was accepted")
	}
}

// TestTwoTokenSourcesAreRefusedRatherThanRanked pins the refusal instead of a
// precedence rule.
//
// Picking one silently makes the effective credential invisible: an operator
// who adds a file while a stale env var is still in the manifest cannot tell
// from the configuration which is being presented, and the symptom of the wrong
// choice is an authentication failure naming neither source.
//
// The env flag ships with a non-empty default, so the default being present
// cannot count as a second choice — otherwise the file form would be
// unreachable without also blanking the env flag.
func TestTwoTokenSourcesAreRefusedRatherThanRanked(t *testing.T) {
	const envDefault = "SHOAL_ADMISSION_TOKEN"

	// Passed beside a file, whatever its value — including the default, which
	// is the case the old rule missed.
	for _, value := range []string{"OTHER_VAR", envDefault} {
		if _, err := credentialSource(
			"-admission-token", value, "/run/token", true); err == nil {
			t.Fatalf("-admission-token-env=%s was accepted beside a file", value)
		}
	}
	// Not passed: the flag sits at its default, which is not a choice, so the
	// file form is reachable without blanking anything.
	if _, err := credentialSource(
		"-admission-token", envDefault, "/run/token", false); err != nil {
		t.Fatalf("the file form is unreachable with the env flag unset: %v", err)
	}
	if _, err := credentialSource("-admission-token", "", "", false); err == nil {
		t.Fatal("no token source at all was accepted")
	}

	// Through run(), which is where the hole actually was: credentialSource
	// could not see whether a flag had been passed, so run() had to tell it,
	// and it told it the resolved value instead.
	for _, value := range []string{"OTHER_VAR", envDefault} {
		detail := refusal(t, proxyArgs(
			"-admission-token-env", value, "-admission-token-file", "/run/token"))
		if !strings.Contains(detail, "mutually exclusive") {
			t.Fatalf("run() accepted -admission-token-env=%s beside a file: %s",
				value, detail)
		}
	}
	// And the same for the upstream credential, which has the same shape.
	for _, value := range []string{"OTHER", "SHOAL_UPSTREAM_API_KEY"} {
		detail := refusal(t, proxyArgs(
			"-upstream-api-key-env", value, "-upstream-api-key-file", "/run/key"))
		if !strings.Contains(detail, "mutually exclusive") {
			t.Fatalf("run() accepted -upstream-api-key-env=%s beside a file: %s",
				value, detail)
		}
	}
}

// TestADrainingProxyWaitsOutTheCallsItAdmitted covers an unreported grant that
// a rolling update produced on purpose, on a schedule, and invisibly.
//
// Shutdown abandons whatever is still connected when its context expires. The
// window was a fixed ten seconds while an admitted call may run for the whole
// request timeout — 90s by default — before it has anything to report. So every
// rollout killed the calls that were mid-flight after the egress had happened
// and the grant had been spent, which is the one outcome this proxy exists to
// prevent.
//
// The window is the lease because the lease is already the bound on an admitted
// call's entire lifetime including its report; that is what validateDurations
// checks it against. Asserting it equals the lease rather than merely "enough"
// is the point — "enough" is a second invariant that can drift from the first.
//
// A behavioural test cannot separate the two: distinguishing a 10s window from
// a correct one needs an upstream call that outlives ten seconds, so the test
// would cost more than the bug. The deadline is read off the context instead,
// which is the same seam listenTCP exists for.
func TestADrainingProxyWaitsOutTheCallsItAdmitted(t *testing.T) {
	const lease = 70 * time.Second
	restoreDrain, restoreListen := drain, listenTCP
	t.Cleanup(func() { drain, listenTCP = restoreDrain, restoreListen })

	windows := make(chan time.Duration, 4)
	drain = func(
		ctx context.Context, state *healthsurface.State,
		workspace healthsurface.GracefulServer, health *healthsurface.Server,
	) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			windows <- -1
		} else {
			windows <- time.Until(deadline)
		}
		return restoreDrain(ctx, state, workspace, health)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, proxyArgs(
			"-listen", "127.0.0.1:0",
			"-lease", lease.String(),
			"-request-timeout", "60s",
		), io.Discard)
	}()

	// Give the listener time to come up, then signal. A drain that never
	// happened would hang here rather than report a wrong window, which the
	// timeout below turns into a failure either way.
	select {
	case err := <-done:
		t.Fatalf("the proxy exited before it was signalled: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy did not exit after being signalled")
	}

	var window time.Duration
	select {
	case window = <-windows:
	default:
		t.Fatal("the proxy exited without draining, so admitted calls were cut off")
	}
	if window < 0 {
		t.Fatal("the drain had no deadline, so a stuck call would hang shutdown forever")
	}
	// Generous slack for scheduling, far tighter than the gap to ten seconds.
	if window < lease-5*time.Second || window > lease {
		t.Fatalf("drain window = %s, want the lease (%s): a window shorter than "+
			"an admitted call abandons it after the egress, stranding the grant",
			window, lease)
	}
}

// TestAnAcknowledgedPlaintextPlaneIsAccepted closes a chart/binary mismatch.
//
// The chart has llmGateway.admission.allowPlaintext for a mesh that supplies the
// transport authentication the scheme would, and validate-chart.sh asserts that
// configuration *renders*. Nothing carried the acknowledgement into the
// process, so absoluteURL refused it at startup: the documented mesh deployment
// rendered cleanly and produced a pod in CrashLoopBackOff.
//
// The exception is deliberately narrow, and the narrowness is the part worth
// pinning. It covers the admission plane, where only the declaration and the
// bearer token travel — not the upstream, which carries the prompt itself and
// has no opt-out, because a mesh authenticating the hop to the decision plane
// says nothing about the hop to a third-party provider.
func TestAnAcknowledgedPlaintextPlaneIsAccepted(t *testing.T) {
	const remote = "http://shoal-explorer:8098"
	if _, err := planeURL(remote, false); err == nil {
		t.Fatal("remote plaintext was accepted without the acknowledgement")
	}
	parsed, err := planeURL(remote, true)
	if err != nil {
		t.Fatalf("the acknowledged configuration was refused: %v", err)
	}
	if parsed.Scheme != "http" || parsed.Host != "shoal-explorer:8098" {
		t.Fatalf("parsed = %v, want the configured URL", parsed)
	}

	// The acknowledgement covers the transport, not the shape. A value that is
	// not an absolute URL is still refused, or the flag becomes a way to skip
	// validation rather than to accept one known risk.
	for _, probe := range []string{"", "shoal-explorer:8098", "ftp://host/x", "http:///nohost"} {
		if _, err = planeURL(probe, true); err == nil {
			t.Fatalf("%q was accepted under the acknowledgement", probe)
		}
	}
	// https still works with the flag set, and the flag does not downgrade it.
	if parsed, err = planeURL("https://workspace.test", true); err != nil ||
		parsed.Scheme != "https" {
		t.Fatalf("https under the acknowledgement = %v, %v", parsed, err)
	}

	// And run() applies it, which is the half that testing planeURL alone does
	// not reach — the same gap a mutation found for absoluteURL and newProxy.
	detail := refusal(t, proxyArgs("-admission-url", remote))
	if !strings.Contains(detail, "-admission-url") {
		t.Fatalf("run() accepted remote plaintext by default: %s", detail)
	}
	if err = run(context.Background(), proxyArgs(
		"-admission-url", remote, "-allow-plaintext-admission",
		"-upstream-base-url", "ftp://nope",
	), io.Discard); err == nil ||
		!strings.Contains(err.Error(), "upstream base URL") {
		// Reaching the upstream check proves the admission URL was accepted;
		// a deliberately bad upstream stops run() before it binds a listener.
		// Matching the upstream URL error specifically, not merely the word
		// "upstream", which other refusals also contain.
		t.Fatalf("run() did not honour the acknowledgement: %v", err)
	}

	// The exception does not extend to the upstream, which carries the prompt.
	if _, err = absoluteURL("http://api.example.test"); err == nil {
		t.Fatal("the upstream transport rule was relaxed too")
	}
}

// TestTheUpstreamCredentialAlsoHasAFileForm is the symmetry the admission
// token gained for the same reason: an environment variable cannot rotate.
func TestTheUpstreamCredentialAlsoHasAFileForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte("sk-first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := credentialSource("-upstream-api-key", "", path, false)
	if err != nil {
		t.Fatal(err)
	}
	if value, readErr := read(); readErr != nil || value != "sk-first" {
		t.Fatalf("credential = %q, %v", value, readErr)
	}
	if err = os.WriteFile(path, []byte("sk-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, readErr := read(); readErr != nil || value != "sk-rotated" {
		t.Fatalf("credential = %q, %v; want the rotated value", value, readErr)
	}
	detail := refusal(t, proxyArgs(
		"-upstream-api-key-env", "OTHER", "-upstream-api-key-file", path))
	if !strings.Contains(detail, "mutually exclusive") {
		t.Fatalf("run() did not apply the rule: %s", detail)
	}
	// Reachable without blanking the env flag, since leaving it at its default
	// is not a choice. A rule that forced the operator to blank it would make
	// the file form awkward enough to avoid.
	//
	// The assertion names the transport rule rather than just "upstream",
	// which a mutation showed was necessary: swapping Visit for VisitAll makes
	// every flag look chosen, and the resulting "mutually exclusive" refusal
	// also contains the word "upstream". Reaching the URL check is what proves
	// the credential stage was passed.
	err = run(context.Background(), proxyArgs(
		"-upstream-api-key-file", path, "-upstream-base-url", "ftp://nope",
	), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("the file form was not reachable on its own: %v", err)
	}
}

// TestAnAllowListEntryWithNoHostIsRefusedAtStartup covers the guard a mutation
// showed nothing reached.
//
// Deleting the empty-host check in normalizeAuthority left every request-side
// test passing, because a request authority of ":8100" fails to match any
// sensible allow-list entry anyway. The check earns its place on the other
// side: without it, ":8100" is accepted as a *configured* authority, and then
// a request sending that literal Host matches it — an allow-list entry that
// names no host, admitting calls on the strength of a port alone.
//
// Testing the consumer could not find that. The property belongs to the
// producer, which is authorities().
func TestAnAllowListEntryWithNoHostIsRefusedAtStartup(t *testing.T) {
	for _, probe := range []string{
		":8100",    // a bare port
		".",        // the root label alone
		"a.test, ", // a trailing empty element, which YAML makes easy to write
		"a.test,,b.test",
		" , ", // every element blank, which is not the same as unset
	} {
		if _, err := authorities(probe, "127.0.0.1:8100"); err == nil {
			t.Fatalf("%q was accepted as an allow-list entry", probe)
		}
	}

	// And the forms that must still work, or the guard refuses the feature.
	for _, probe := range []struct {
		configured string
		want       []string
	}{
		{"example.test", []string{"example.test"}},
		{"example.test:8100", []string{"example.test:8100"}},
		{"EXAMPLE.TEST.", []string{"example.test"}},
		{"a.test,b.test", []string{"a.test", "b.test"}},
		{" a.test , b.test ", []string{"a.test", "b.test"}},
		{"[::1]:8100", []string{"[::1]:8100"}},
	} {
		got, err := authorities(probe.configured, "127.0.0.1:8100")
		if err != nil {
			t.Fatalf("%q was refused: %v", probe.configured, err)
		}
		if len(got) != len(probe.want) {
			t.Fatalf("%q = %v, want %v", probe.configured, got, probe.want)
		}
		for i := range got {
			if got[i] != probe.want[i] {
				t.Fatalf("%q = %v, want %v", probe.configured, got, probe.want)
			}
		}
	}

	// An unset configuration falls back to the bound address, which is what
	// makes the loopback default safe rather than open. A blank value is unset
	// rather than invalid — a flag given "" and a flag given " " are the same
	// intent, and I had this in the refused list until the test said otherwise.
	var got []string
	var err error
	for _, unset := range []string{"", " ", "   "} {
		got, err = authorities(unset, "127.0.0.1:8100")
		if err != nil || len(got) != 1 || got[0] != "127.0.0.1:8100" {
			t.Fatalf("the fallback for %q = %v, %v; want the bound address",
				unset, got, err)
		}
	}
	// A wildcard bind is accepted as configuration and matches nothing, which
	// is the property main.go claims and is not the same as being refused.
	//
	// I asserted a refusal here first and it failed: "[::]:8100" is a
	// syntactically fine authority. The claim worth testing is the one the
	// comment actually makes — defaulting to a wildcard bind refuses every real
	// request rather than admitting any — so it belongs on the request side,
	// not in rejecting the configuration.
	for _, bound := range []string{"[::]:8100", "0.0.0.0:8100"} {
		got, err = authorities("", bound)
		if err != nil {
			t.Fatalf("a wildcard bind was refused as configuration: %v", err)
		}
		governed := &proxy{allowedHosts: got}
		for _, sent := range []string{
			"shoal-llm-gateway.default.svc", "shoal-llm-gateway:8100",
			"example.test", "127.0.0.1:8100", "localhost:8100",
		} {
			if governed.permits(sent) {
				t.Fatalf("a wildcard bind (%s) admitted %q; a public bind must "+
					"name its authority explicitly", bound, sent)
			}
		}
	}
}

// TestTheModelFieldCannotCarryThePrompt closes the channel that role closed,
// in the one other place the declaration copied caller text verbatim.
//
// This is the same finding as the role one and I did not generalise it at the
// time, which is the lesson worth recording: the fix was applied to the field
// that had been named rather than to the property that had been broken. A
// prompt or a secret fits in model exactly as well, and nothing about the
// field's name prevents it.
//
// There is no finite vocabulary available here — model names are whatever a
// provider serves — so the operator names the ones the deployment expects and
// anything else becomes a marker.
func TestTheModelFieldCannotCarryThePrompt(t *testing.T) {
	const secret = "a-prompt-smuggled-through-the-model-field"
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	post(t, governed,
		`{"model":"`+secret+`","messages":[{"role":"user","content":"hi"}]}`)
	if len(plane.requests) != 1 {
		t.Fatalf("admissions = %d", len(plane.requests))
	}
	everything, err := json.Marshal(plane.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(everything), secret) {
		t.Fatalf("the model reached the decision plane: %s", everything)
	}
	if !strings.Contains(string(plane.requests[0].Input), modelOther) {
		t.Fatalf("an unnamed model left no trace at all: %s", plane.requests[0].Input)
	}

	// A named model is reported as itself, or the sanitisation has thrown away
	// the signal along with the content. The harness names "gpt".
	plane.requests = nil
	post(t, governed, plainCall)
	if !strings.Contains(string(plane.requests[0].Input), `"model":"gpt"`) {
		t.Fatalf("a named model was not reported: %s", plane.requests[0].Input)
	}

	// The caller's own model still goes upstream. Rewriting it would make the
	// proxy the reason an unmodified client gets a different answer.
	if !strings.Contains(string(upstream.received), secret) {
		// The first call carried the secret model; the second carried "gpt".
		// Only the second is in upstream.received now, so check the request
		// directly instead.
		plane.requests = nil
		post(t, governed,
			`{"model":"`+secret+`","messages":[{"role":"user","content":"hi"}]}`)
		if !strings.Contains(string(upstream.received), secret) {
			t.Fatalf("the caller's model was rewritten: %s", upstream.received)
		}
	}

	// With no allow-list at all, every model is a marker. This is the default
	// and the reason the guarantee holds without configuration.
	if got := classifyModel("gpt", nil); got != modelOther {
		t.Fatalf("classifyModel with no allow-list = %q, want %q", got, modelOther)
	}
}

// TestAGrantNamingAnotherClaimIsRefused covers an echo that was trusted.
//
// The claim is created under the token ID this proxy chose and sent — the
// service sets ClaimID from request.TokenID — and the report selects the claim
// by the ID that came back. So a response carrying a different ID does not
// merely misdescribe this call: it makes the proxy forward the call and then
// close somebody else's outstanding admission.
//
// Structural validation could not catch it, because a swapped ID is perfectly
// well formed. The fake plane could not express it either: it returned a fixed
// token ID rather than echoing the request's, so every test was running against
// a plane that always disagreed and nothing noticed. It echoes now, which is
// what a real plane does, and this probe is the one place that overrides it.
func TestAGrantNamingAnotherClaimIsRefused(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	// A well-formed, reportable token for a different claim.
	plane.echoTokenID = false
	plane.token = &admissionapi.Token{
		ActionID:  "YWN0aW9u",
		TokenID:   "c29tZWJvZHktZWxzZQ",
		Version:   1,
		ExpiresAt: time.Now().Add(time.Minute),
	}

	recorder := post(t, governed, plainCall)
	if upstream.calls != 0 {
		t.Fatal("the call was forwarded under a grant naming another claim")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	// Nothing is reported, because reporting is the harm: it would resolve the
	// claim the response named, which is not this call's.
	if len(plane.reports) != 0 {
		t.Fatalf("a mismatched grant was reported anyway: %+v", plane.reports)
	}

	// And the echoed form is accepted, or this refuses every real grant.
	plane.echoTokenID = true
	plane.reports = nil
	if recorder = post(t, governed, plainCall); recorder.Code != http.StatusOK {
		t.Fatalf("an echoed token was refused: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestFleetNamesAreValidatedAtStartup moves a per-call 400 to a startup refusal.
//
// The admission service applies a grammar to capability and action names
// (validateName in pkg/explorer/fleet/model.go): non-empty, at most
// admissionapi.MaxNameBytes, no surrounding whitespace, and only letters, digits and
// _-.: — and a name outside it takes a 400 on every admission, which this
// client reports as plane_unavailable. So static configuration started cleanly,
// passed both probes, and told every caller to retry a configuration error
// while the operator saw what looked like an outage.
//
// The surrounding-whitespace case is the one the previous check actively hid:
// it trimmed before testing for emptiness, so " complete" passed and was then
// sent with the space for the plane to refuse.
func TestFleetNamesAreValidatedAtStartup(t *testing.T) {
	for _, probe := range []struct{ name, value, refused string }{
		{"empty", "", "required"},
		{"only whitespace", "   ", "whitespace"},
		{"a leading space", " complete", "whitespace"},
		{"a trailing space", "complete ", "whitespace"},
		{"a space inside", "com plete", "letters, digits"},
		{"a slash", "chat/completions", "letters, digits"},
		{"an at sign", "complete@v1", "letters, digits"},
		{"over the byte bound", strings.Repeat("a", admissionapi.MaxNameBytes+1), "at most"},
	} {
		detail := refusal(t, proxyArgs("-action", probe.value))
		if !strings.Contains(detail, probe.refused) {
			t.Fatalf("-action %q refused for the wrong reason: %s",
				probe.name, detail)
		}
		// Both flags, or only one of them is guarded.
		if detail = refusal(t, proxyArgs("-capability", probe.value)); !strings.Contains(
			detail, probe.refused) {
			t.Fatalf("-capability %q refused for the wrong reason: %s",
				probe.name, detail)
		}
	}

	// The grammar the plane does accept must still pass, or this refuses the
	// feature. Every character class the service allows is covered.
	for _, probe := range []string{
		"complete", "chat.completions", "llm_gateway", "llm-gateway",
		"chat:complete", "Complete9", strings.Repeat("a", admissionapi.MaxNameBytes),
	} {
		if err := fleetName("-action", probe); err != nil {
			t.Fatalf("%q was refused: %v", probe, err)
		}
	}
}

// TestTheAgentIDSentIsTheOneValidated closes a gap between a check and the
// value it checked.
//
// decodeID trims before decoding, so " YWdlbnQ " validated cleanly and was then
// stored and sent verbatim — and the workspace's decoder does not trim, so it
// failed to decode there. Static configuration that passed startup produced a
// 400 on every call, which this client reports as a retryable 503.
//
// The bug is the divergence: a value validated in one form and transmitted in
// another. So the assertion has to be on what reaches the plane, not on whether
// startup accepted it — my first version of this test checked that run() got as
// far as binding a listener, which the untrimmed value also did. It could not
// have failed for the thing it is named for.
func TestTheAgentIDSentIsTheOneValidated(t *testing.T) {
	// The premise, stated independently: the plane's decoder does not trim.
	if _, err := base64.RawURLEncoding.DecodeString(" YWdlbnQ "); err == nil {
		t.Fatal("the premise no longer holds: an untrimmed ID now decodes")
	}

	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	// run() reads this at request time, and without it the admission POST
	// fails as unauthenticated before the agent ID is ever sent — which is
	// what the first run of this test actually measured.
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")

	restore := listenTCP
	t.Cleanup(func() { listenTCP = restore })
	bound := make(chan string, 1)
	listenTCP = func(network, address string) (net.Listener, error) {
		listener, err := restore(network, address)
		if err == nil {
			bound <- listener.Addr().String()
		}
		return listener, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-listen", "127.0.0.1:0",
			"-admission-url", plane.server.URL,
			"-upstream-base-url", upstream.server.URL,
			"-agent-id", " YWdlbnQ ",
			"-agent-generation", "1",
			"-capability", "llm.gateway",
			"-action", "complete",
			"-source-id", "c291cmNl",
			"-policy-id", "cG9saWN5",
			"-lease", "70s",
			"-request-timeout", "60s",
		}, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var address string
	select {
	case address = <-bound:
	case err := <-done:
		t.Fatalf("the proxy exited before binding: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never bound a listener")
	}

	// The default allow-list is the resolved listen address, which is the
	// authority this request carries.
	request, err := http.NewRequest(http.MethodPost,
		"http://"+address+"/v1/chat/completions", strings.NewReader(plainCall))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = address
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}

	if len(plane.requests) != 1 {
		t.Fatalf("admissions = %d, want 1", len(plane.requests))
	}
	sent := plane.requests[0].AgentID
	if sent != "YWdlbnQ" {
		t.Fatalf("agent_id on the wire = %q, want the trimmed form that was "+
			"validated: the plane's decoder does not trim, so this is a 400 on "+
			"every call", sent)
	}
	if _, err = base64.RawURLEncoding.DecodeString(sent); err != nil {
		t.Fatalf("the agent ID sent is not decodable by the plane: %v", err)
	}
}

// TestReadinessIsNeverFlippedBackAfterShutdownBegins covers a startup race.
//
// MarkReady ran after the shutdown watcher was launched. A context already
// cancelled — or cancelled during startup — lets the watcher run Drain and mark
// the surface draining first, after which MarkReady flips readiness back to
// true while the listener is already closing. A probe then gets a ready answer
// from a process that is shutting down, which is the inverse of what the health
// surface exists for.
//
// Marking ready before the watcher starts means every cancellation transition
// happens after it and wins.
func TestReadinessIsNeverFlippedBackAfterShutdownBegins(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")

	restoreListen, restoreDrain := listenTCP, drain
	t.Cleanup(func() { listenTCP, drain = restoreListen, restoreDrain })

	// The invariant is about ordering, so it is observed at the moment the
	// drain begins: readiness must already have been marked.
	//
	// Being exact about what this does and does not prove, because the
	// alternative is pretending. Restoring the bug — MarkReady after the
	// goroutine rather than before — does NOT fail this test, and 400
	// iterations at GOMAXPROCS=8 did not trip it either. The window is one
	// statement wide with no yield point in it, so the main goroutine
	// effectively always reaches MarkReady before the watcher can enter the
	// drain, even with the context already cancelled.
	//
	// So the fix is kept on correctness-by-construction grounds: with MarkReady
	// before the goroutine exists, program order guarantees the ordering and no
	// scheduling outcome can violate it. This test pins the invariant and would
	// catch a larger reordering; it is not evidence that the narrow race was
	// reachable, and it is recorded here as such rather than counted as a
	// mutation caught.
	var readyAtEntry []bool
	drain = func(
		ctx context.Context, state *healthsurface.State,
		workspace healthsurface.GracefulServer, health *healthsurface.Server,
	) error {
		recorder := httptest.NewRecorder()
		healthsurface.NewHandler(state).ServeHTTP(
			recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		readyAtEntry = append(readyAtEntry, recorder.Code == http.StatusOK)
		return restoreDrain(ctx, state, workspace, health)
	}

	// Cancelled before run() is called, so the watcher observes a closed
	// channel immediately — the sharpest form of the race.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, []string{
		"-listen", "127.0.0.1:0",
		"-admission-url", plane.server.URL,
		"-upstream-base-url", upstream.server.URL,
		"-agent-id", "YWdlbnQ", "-agent-generation", "1",
		"-capability", "llm.gateway", "-action", "complete",
		"-source-id", "c291cmNl", "-policy-id", "cG9saWN5",
		"-lease", "70s", "-request-timeout", "60s",
	}, io.Discard)
	if err != nil {
		t.Fatalf("run returned %v", err)
	}
	if len(readyAtEntry) == 0 {
		t.Fatal("the drain never ran, so the ordering was never exercised")
	}
	for _, ready := range readyAtEntry {
		if !ready {
			t.Fatal("the drain began before readiness was marked, so the " +
				"MarkReady that follows flips a draining surface back to " +
				"ready while the listener is already closing")
		}
	}
}

// TestAnOversizedActionIDIsRefusedBeforeTheEgress covers the byte bound the
// report endpoint applies, before the call rather than after it.
//
// AdmissionToken.validate runs both IDs through validateOpaque, which refuses
// anything over admissionapi.MaxIDBytes. So an over-long ID is a grant that
// passes reportable, is spent on the upstream call, and can then never be
// reported — the exact outcome reportable exists to prevent. Checking
// decodability without checking length left half of its purpose open.
//
// Token echo stays on here, which is the whole reason this is a separate test.
// With echo off the grant is refused for naming a different claim and the bound
// is never reached, so probes placed in the unreportable-token table could not
// fail for the thing they were named for.
//
// Only the action ID is probed, and that is a statement about reachability
// rather than an omission. The token ID is the one this proxy generated — 16
// bytes from newCallerIdentity — and the echo check requires the response to
// carry it back unchanged, so an over-long token ID cannot survive to reach the
// bound. The loop checks both because the cost is nothing and the guarantee
// then does not depend on two other rules holding.
func TestAnOversizedActionIDIsRefusedBeforeTheEgress(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	plane.token.ActionID = base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte("a"), admissionapi.MaxIDBytes+1))

	recorder := post(t, governed, plainCall)
	if upstream.calls != 0 {
		t.Fatal("the call was forwarded under a grant the report endpoint " +
			"will refuse, which is an unreportable grant by construction")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}

	// Exactly at the bound is accepted, or this refuses valid grants.
	plane.token.ActionID = base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte("a"), admissionapi.MaxIDBytes))
	if recorder = post(t, governed, plainCall); recorder.Code != http.StatusOK {
		t.Fatalf("an action ID exactly at the bound was refused: %d %s",
			recorder.Code, recorder.Body.String())
	}

	// And the proxy's own token ID is well inside the bound, which is what
	// makes the token-ID half of the loop unreachable rather than untested.
	identity, err := newCallerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(identity.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) > admissionapi.MaxIDBytes {
		t.Fatalf("the proxy generates a token ID of %d bytes, over the bound",
			len(decoded))
	}
}
