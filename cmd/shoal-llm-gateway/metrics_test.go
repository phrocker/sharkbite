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
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/healthsurface"
	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// allowedFamilies is every metric family the health listener may carry, with
// the one label each family has and the closed set of values it may take.
//
// It is written out here rather than derived from metrics.go so a new family
// or a new label value is a change a reviewer has to make twice — once where
// it is counted and once here, where the question "may an unauthenticated
// scraper see this?" is asked. The answer has to be an operational count of
// the gateway's own failures and refusals. A count of allowed or forwarded
// calls, bytes, tokens or anything per model, reference or principal is
// tenant workload and does not belong on this port.
var allowedFamilies = map[string]struct {
	label  string
	values []string
}{
	"shoal_llm_gateway_infrastructural_denials_total": {"reason", []string{
		"plane_unreachable", "plane_credential_rejected", "plane_reported_unavailable",
		"plane_error_status", "plane_answer_unusable", "admission_credential_unavailable",
		"grant_window_exhausted", "identity_unavailable",
	}},
	"shoal_llm_gateway_policy_refusals_total": {"reason", []string{
		"denied", "obligation_unsatisfiable",
	}},
	"shoal_llm_gateway_upstream_failures_total": {"reason", []string{
		"credential_unavailable", "unreachable", "error_status", "response_truncated",
	}},
	"shoal_llm_gateway_report_failures_total": {"reason", []string{
		"plane_unreachable", "plane_credential_rejected", "plane_reported_unavailable",
		"plane_error_status", "plane_answer_unusable", "admission_credential_unavailable",
	}},
}

var sampleLine = regexp.MustCompile(`^([a-z_]+)\{([a-z_]+)="([a-z_]+)"\} ([0-9]+)$`)

// scrape renders the exposition and parses it strictly: every line is a HELP,
// a TYPE, or a sample of an allowed family with its one allowed label and a
// value from the closed set. Anything else fails the test, so the shape of the
// exposition is pinned, not only the counts a test happens to look for.
func scrape(t *testing.T, governed *proxy) map[string]uint64 {
	t.Helper()
	var builder strings.Builder
	governed.writeMetrics(&builder)
	return parseExposition(t, builder.String())
}

func parseExposition(t *testing.T, body string) map[string]uint64 {
	t.Helper()
	samples := make(map[string]uint64)
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			name := strings.Fields(line)[2]
			if _, ok := allowedFamilies[name]; !ok {
				t.Fatalf("family %q is not one the health listener may carry", name)
			}
			if strings.HasPrefix(line, "# TYPE ") && !strings.HasSuffix(line, " counter") {
				t.Fatalf("family %q is not a counter: %q", name, line)
			}
			continue
		}
		match := sampleLine.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("unexpected exposition line %q", line)
		}
		family, ok := allowedFamilies[match[1]]
		if !ok {
			t.Fatalf("family %q is not one the health listener may carry", match[1])
		}
		if match[2] != family.label {
			t.Fatalf("%s carries label %q, want only %q", match[1], match[2], family.label)
		}
		known := false
		for _, value := range family.values {
			known = known || value == match[3]
		}
		if !known {
			t.Fatalf("%s carries %s=%q, outside its closed set", match[1], match[2], match[3])
		}
		key := match[1] + "/" + match[3]
		if _, seen := samples[key]; seen {
			t.Fatalf("duplicate series %s", key)
		}
		count, err := strconv.ParseUint(match[4], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		samples[key] = count
	}
	// Every series is present, at zero if nothing has happened, so an alert
	// has a baseline rather than a series that appears mid-incident.
	for name, family := range allowedFamilies {
		for _, value := range family.values {
			if _, ok := samples[name+"/"+value]; !ok {
				t.Fatalf("series %s{%s=%q} is missing", name, family.label, value)
			}
		}
	}
	return samples
}

// expectCounts asserts the scrape is exactly want: the named series at the
// given counts and every other series at zero. "Every other at zero" is what
// makes a policy denial counted as infrastructural (or the reverse) fail.
func expectCounts(t *testing.T, got map[string]uint64, want map[string]uint64) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if got[key] != want[key] {
			t.Errorf("%s = %d, want %d", key, got[key], want[key])
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("%s was expected but is not exposed", key)
		}
	}
}

const (
	infraFamily    = "shoal_llm_gateway_infrastructural_denials_total/"
	policyFamily   = "shoal_llm_gateway_policy_refusals_total/"
	upstreamFamily = "shoal_llm_gateway_upstream_failures_total/"
	reportFamily   = "shoal_llm_gateway_report_failures_total/"
)

// truncatingUpstream promises more body than it sends, so the relay ends in an
// unexpected EOF: a provider that stopped part-way.
func truncatingUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Length", "1000")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, `{"id":`)
		}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// TestEachFailureClassIsCountedUnderItsOwnReason drives every class through
// the real handler, against the fake plane and provider, and asserts the whole
// exposition each time. Policy refusals and infrastructural denials are
// separate families, and a call that succeeds counts nothing at all.
func TestEachFailureClassIsCountedUnderItsOwnReason(t *testing.T) {
	obligated := `{"model":"gpt","messages":[{"role":"user","content":"hi"}],` +
		`"shoal_references":["` + docA + `"]}`
	tests := []struct {
		name       string
		setup      func(*testing.T, *fakePlane, *fakeUpstream, *proxy)
		upstream   func(*testing.T) *fakeUpstream
		body       string
		makeBody   func(*testing.T) string
		cancelled  bool
		wantStatus int
		wantCode   string
		want       map[string]uint64
	}{
		{
			name:       "allowed counts nothing",
			wantStatus: http.StatusOK,
			want:       map[string]uint64{},
		},
		{
			name: "allowed with obligations counts nothing",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.outcome = admissionapi.OutcomeObligated
				plane.withhold = []string{docB}
			},
			makeBody:   func(t *testing.T) string { return acceptanceBody(t, nil) },
			wantStatus: http.StatusOK,
			want:       map[string]uint64{},
		},
		{
			name: "policy denial is a policy refusal, not infrastructural",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.outcome = admissionapi.OutcomeDenied
			},
			wantStatus: http.StatusForbidden, wantCode: "denied",
			want: map[string]uint64{policyFamily + "denied": 1},
		},
		{
			name: "unsatisfiable obligation is a policy refusal",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.outcome = admissionapi.OutcomeObligated
				plane.withhold = []string{docNeverDeclared}
			},
			body:       obligated,
			wantStatus: http.StatusForbidden, wantCode: "obligation_unsatisfiable",
			want: map[string]uint64{policyFamily + "obligation_unsatisfiable": 1},
		},
		{
			name: "plane gone is unreachable",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.server.Close()
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_unreachable": 1},
		},
		{
			name: "a 503 in front of the plane is unreachable",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.status = http.StatusServiceUnavailable
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_unreachable": 1},
		},
		{
			name: "the gateway's credential rejected is not a policy denial",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.status = http.StatusUnauthorized
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_credential_rejected": 1},
		},
		{
			name: "a 503 the plane wrote itself is the plane answering, not an outage",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				// As pkg/explorer/webapi answers ErrAdmissionUnmigrated and
				// ErrAdmissionSpanOccupied: a structured body with a code.
				plane.status = http.StatusServiceUnavailable
				plane.errorBody = `{"code":"unavailable","message":"admission requires migration"}`
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_reported_unavailable": 1},
		},
		{
			name: "a coded 502 is an error status, not unreachable",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.status = http.StatusBadGateway
				plane.errorBody = `{"code":"internal","message":"x"}`
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_error_status": 1},
		},
		{
			name: "an unreadable admission token file",
			setup: func(t *testing.T, plane *fakePlane, _ *fakeUpstream, governed *proxy) {
				// The real file reader, over a file it cannot read: a
				// directory where the token should be.
				governed.admission.credential = credentialFromFile(t.TempDir())
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "admission_credential_unavailable": 1},
		},
		{
			name: "an empty admission token",
			setup: func(_ *testing.T, _ *fakePlane, _ *fakeUpstream, governed *proxy) {
				governed.admission.credential = func() (string, error) { return "", nil }
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "admission_credential_unavailable": 1},
		},
		{
			name: "an admission token holding a line break",
			setup: func(_ *testing.T, _ *fakePlane, _ *fakeUpstream, governed *proxy) {
				governed.admission.credential = func() (string, error) { return "tok\r\nX: y", nil }
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "admission_credential_unavailable": 1},
		},
		{
			name: "a plane 500 is an error status",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.status = http.StatusInternalServerError
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_error_status": 1},
		},
		{
			name: "an outcome the gateway does not know is an unusable answer",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.outcome = admissionapi.Outcome("allowed_eventually")
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "plane_answer_unusable": 1},
		},
		{
			name: "a grant with no lease left is an exhausted window",
			setup: func(_ *testing.T, _ *fakePlane, _ *fakeUpstream, governed *proxy) {
				// The first reading is the admission; every later one is the
				// forward, an hour on, past the token's minute.
				var calls atomic.Int32
				governed.clock = func() time.Time {
					if calls.Add(1) == 1 {
						return time.Now()
					}
					return time.Now().Add(time.Hour)
				}
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "grant_window_exhausted": 1},
		},
		{
			name: "no admission identity",
			setup: func(t *testing.T, _ *fakePlane, _ *fakeUpstream, _ *proxy) {
				restore := mintCallerIdentity
				t.Cleanup(func() { mintCallerIdentity = restore })
				mintCallerIdentity = func() (callerIdentity, error) {
					return callerIdentity{}, errors.New("entropy unavailable")
				}
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "plane_unavailable",
			want: map[string]uint64{infraFamily + "identity_unavailable": 1},
		},
		{
			name:      "a caller that hangs up during admission counts nothing",
			cancelled: true,
			want:      map[string]uint64{},
		},
		{
			name: "an unreadable provider credential",
			setup: func(_ *testing.T, _ *fakePlane, _ *fakeUpstream, governed *proxy) {
				governed.credential = func() (string, error) {
					return "", errors.New("permission denied")
				}
			},
			wantStatus: http.StatusBadGateway, wantCode: "upstream_unreachable",
			want: map[string]uint64{upstreamFamily + "credential_unavailable": 1},
		},
		{
			name: "a provider that cannot be reached",
			setup: func(_ *testing.T, _ *fakePlane, upstream *fakeUpstream, _ *proxy) {
				upstream.server.Close()
			},
			wantStatus: http.StatusBadGateway, wantCode: "upstream_unreachable",
			want: map[string]uint64{upstreamFamily + "unreachable": 1},
		},
		{
			name: "a provider error status",
			setup: func(_ *testing.T, _ *fakePlane, upstream *fakeUpstream, _ *proxy) {
				upstream.status = http.StatusTooManyRequests
			},
			wantStatus: http.StatusTooManyRequests,
			want:       map[string]uint64{upstreamFamily + "error_status": 1},
		},
		{
			name:       "a provider response cut short",
			upstream:   truncatingUpstream,
			wantStatus: http.StatusOK,
			want:       map[string]uint64{upstreamFamily + "response_truncated": 1},
		},
		{
			name: "a report the plane refused",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.reportStatus = http.StatusInternalServerError
			},
			wantStatus: http.StatusOK,
			want:       map[string]uint64{reportFamily + "plane_error_status": 1},
		},
		{
			name: "a report the gateway's credential could not make",
			setup: func(_ *testing.T, plane *fakePlane, _ *fakeUpstream, _ *proxy) {
				plane.reportStatus = http.StatusForbidden
			},
			wantStatus: http.StatusOK,
			want:       map[string]uint64{reportFamily + "plane_credential_rejected": 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
			var upstream *fakeUpstream
			if test.upstream != nil {
				upstream = test.upstream(t)
			} else {
				upstream = newFakeUpstream(t)
			}
			governed, _ := newTestProxy(t, plane, upstream)
			if test.setup != nil {
				test.setup(t, plane, upstream, governed)
			}
			body := test.body
			if test.makeBody != nil {
				body = test.makeBody(t)
			}
			if body == "" {
				body = plainCall
			}

			request := httptest.NewRequest(
				http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Host = "example.test"
			if test.cancelled {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			governed.routes().ServeHTTP(response, request)

			if !test.cancelled {
				if response.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d: %s",
						response.Code, test.wantStatus, response.Body.String())
				}
				if test.wantCode != "" &&
					!strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
					t.Fatalf("refusal code is not %q: %s", test.wantCode, response.Body.String())
				}
			}
			expectCounts(t, scrape(t, governed), test.want)
		})
	}
}

// TestTheCallerCannotLearnTheInfrastructuralReason pins the caller-facing half
// of #425: an infrastructural denial is a retryable 503 with a fixed body, a
// policy denial a final 403, and neither names a reason the metrics do.
func TestTheCallerCannotLearnTheInfrastructuralReason(t *testing.T) {
	bodies := map[string]bool{}
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusInternalServerError, http.StatusServiceUnavailable,
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		plane.status = status
		governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
		response := post(t, governed, plainCall)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("plane %d: status = %d, want 503", status, response.Code)
		}
		for _, name := range infraReasonNames {
			if strings.Contains(response.Body.String(), name) {
				t.Fatalf("plane %d: caller was told the reason %q: %s",
					status, name, response.Body.String())
			}
		}
		bodies[response.Body.String()] = true
	}
	if len(bodies) != 1 {
		t.Fatalf("infrastructural refusals differ by cause, which tells the caller "+
			"something about the plane: %v", bodies)
	}
}

// TestLabelsStayBoundedWhateverTheCallerSends sends calls that vary every
// caller-controlled field, under every outcome, and asserts the series set does
// not grow and nothing the caller sent appears in it.
func TestLabelsStayBoundedWhateverTheCallerSends(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	baseline := len(scrape(t, governed))

	var builder strings.Builder
	for i, outcome := range []admissionapi.Outcome{
		admissionapi.OutcomeAllowed, admissionapi.OutcomeDenied,
		admissionapi.OutcomeObligated, admissionapi.Outcome("novel"),
	} {
		plane.outcome = outcome
		plane.withhold = []string{docNeverDeclared}
		for _, model := range []string{"gpt", "secret-model-" + strconv.Itoa(i)} {
			post(t, governed, `{"model":"`+model+`","messages":[{"role":"user",`+
				`"content":"canary-prompt"}],"shoal_references":["`+docA+`"]}`)
		}
	}
	plane.status = http.StatusUnauthorized
	post(t, governed, plainCall)

	builder.Reset()
	governed.writeMetrics(&builder)
	exposition := builder.String()
	if got := len(parseExposition(t, exposition)); got != baseline {
		t.Fatalf("series count grew from %d to %d", baseline, got)
	}
	for _, content := range []string{
		"canary-prompt", "secret-model", "gpt", docA, docNeverDeclared,
		"YWN0aW9u", "dG9rZW4", "plane-token", "upstream-key", "example.test",
	} {
		if strings.Contains(exposition, content) {
			t.Fatalf("exposition carries caller or credential content %q:\n%s",
				content, exposition)
		}
	}
}

// TestMetricsAreServedOnTheHealthListener runs the binary: /metrics answers on
// the health listener and not on the completions listener, the completions
// route does not answer on the health listener, and an infrastructural denial
// counted there leaves /readyz ready.
//
// Ready is the decision, not an accident (#425). A gateway failing closed
// because the plane is gone is doing what it was built to do, and every
// replica shares the same plane: marking them not-ready would empty the
// Service at once, turning a structured, retryable 503 into a connection
// failure that says nothing, and no restart or reroute would bring the plane
// back. The outage is an alert on the counter, not a probe result.
func TestMetricsAreServedOnTheHealthListener(t *testing.T) {
	// A plane that drops every connection unanswered, so every admission is
	// a transport failure. It stays bound for the whole test: a port closed
	// and assumed dead can be taken by another process under a parallel run.
	plane, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	go func() {
		for {
			conn, err := plane.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	planeAddress := plane.Addr().String()
	// A token, so the denial is the plane being absent and not this pod
	// lacking its own credential.
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")

	restoreListen, restoreHealthListen := listenTCP, healthsurface.ListenTCP
	t.Cleanup(func() { listenTCP, healthsurface.ListenTCP = restoreListen, restoreHealthListen })
	requests := make(chan net.Listener, 1)
	healths := make(chan net.Listener, 1)
	listenTCP = func(network, address string) (net.Listener, error) {
		listener, err := restoreListen(network, address)
		if err == nil {
			requests <- listener
		}
		return listener, err
	}
	healthsurface.ListenTCP = func(network, address string) (net.Listener, error) {
		listener, err := restoreHealthListen(network, address)
		if err == nil {
			healths <- listener
		}
		return listener, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, proxyArgs(
			"-listen", "127.0.0.1:0",
			"-health-address", "127.0.0.1:0",
			"-allowed-host", "example.test",
			"-admission-url", "http://"+planeAddress,
		), io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the gateway did not shut down")
		}
	})

	var requestListener, healthListener net.Listener
	for requestListener == nil || healthListener == nil {
		select {
		case requestListener = <-requests:
		case healthListener = <-healths:
		case err := <-done:
			t.Fatalf("the gateway exited: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the gateway did not bind both listeners")
		}
	}

	call := func(listener net.Listener, method, path, body string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(method,
			"http://"+listener.Addr().String()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "example.test"
		var response *http.Response
		// /readyz turns ready just after the health listener binds.
		for attempt := 0; ; attempt++ {
			response, err = http.DefaultClient.Do(request)
			if err == nil || attempt > 50 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(raw)
	}
	waitReady := func() int {
		var status int
		for attempt := 0; attempt < 100; attempt++ {
			if status, _ = call(healthListener, http.MethodGet, "/readyz", ""); status == http.StatusOK {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		return status
	}
	if status := waitReady(); status != http.StatusOK {
		t.Fatalf("/readyz = %d before any call", status)
	}

	for i := 0; i < 3; i++ {
		status, body := call(requestListener, http.MethodPost, "/v1/chat/completions", plainCall)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("call against an absent plane = %d: %s", status, body)
		}
	}

	status, body := call(healthListener, http.MethodGet, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("/metrics on the health listener = %d: %s", status, body)
	}
	samples := parseExposition(t, body)
	expectCounts(t, samples, map[string]uint64{infraFamily + "plane_unreachable": 3})

	if status, _ := call(healthListener, http.MethodGet, "/readyz", ""); status != http.StatusOK {
		t.Fatalf("/readyz = %d after infrastructural denials, want 200: failing "+
			"closed is the designed behaviour, not unreadiness", status)
	}
	if status, _ := call(requestListener, http.MethodGet, "/metrics", ""); status == http.StatusOK {
		t.Fatal("the completions listener answers /metrics")
	}
	if status, _ := call(healthListener, http.MethodPost, "/v1/chat/completions", plainCall); status == http.StatusOK ||
		status == http.StatusServiceUnavailable {
		t.Fatalf("the health listener answers the completions route: %d", status)
	}
}
