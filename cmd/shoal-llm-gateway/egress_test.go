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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// The proxy's half of #427: a failure after a partial egress says how much
// escaped, counted at the caller's ResponseWriter.

// event is one SSE event as the streaming upstream sends it.
const event = "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"

// cutWriter accepts a bounded number of writes and then fails, standing in for
// a caller that disconnects mid-stream. Unlike failingWriter it can flush, as
// net/http's own writer can, so chunks are observable.
type cutWriter struct {
	header  http.Header
	accept  int
	writes  int
	flushes int
	status  int
}

func newCutWriter(accept int) *cutWriter {
	return &cutWriter{header: http.Header{}, accept: accept}
}

func (c *cutWriter) Header() http.Header  { return c.header }
func (c *cutWriter) WriteHeader(code int) { c.status = code }
func (c *cutWriter) Flush()               { c.flushes++ }
func (c *cutWriter) Write(data []byte) (int, error) {
	c.writes++
	if c.writes > c.accept {
		return 0, errors.New("connection reset by peer")
	}
	return len(data), nil
}

// streamingUpstream sends events one flush at a time, pausing between them so
// each arrives at the proxy as its own read. With breakAfter it then drops the
// connection mid-body, which is an upstream break rather than a caller one.
func streamingUpstream(t *testing.T, events int, breakAfter bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			flusher := writer.(http.Flusher)
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			for index := 0; index < events; index++ {
				_, _ = io.WriteString(writer, event)
				flusher.Flush()
				time.Sleep(30 * time.Millisecond)
			}
			if breakAfter {
				conn, _, err := writer.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			}
		}))
	t.Cleanup(server.Close)
	return server
}

// pointAt sends the proxy's upstream calls to server.
func pointAt(t *testing.T, governed *proxy, server *httptest.Server) {
	t.Helper()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	governed.upstream = base
	governed.client.Transport = server.Client().Transport
}

// egressing makes the proxy declare what a hosted provider would. The test
// upstreams are loopback, so by default it declares no egress at all.
func egressing(governed *proxy) {
	governed.admission.effects = []string{
		admissionapi.EffectEgressesContent, admissionapi.EffectReadsCorpus,
	}
}

const streamCall = `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func serveTo(governed *proxy, writer http.ResponseWriter) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(streamCall))
	request.Host = "example.test"
	governed.routes().ServeHTTP(writer, request)
}

func onlyReport(t *testing.T, plane *fakePlane) admissionapi.Report {
	t.Helper()
	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(plane.reports))
	}
	return plane.reports[0]
}

// TestAStreamCutAfterNBytesReportsN: the caller took two events and was gone
// on the third. The report says exactly what was handed to its writer — two
// events' bytes in two chunks — not what the upstream offered (four), and
// not what was counted part-way through the handler.
func TestAStreamCutAfterNBytesReportsN(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	governed, logged := newTestProxy(t, plane, newFakeUpstream(t))
	egressing(governed)
	pointAt(t, governed, streamingUpstream(t, 4, false))

	writer := newCutWriter(2)
	serveTo(governed, writer)

	report := onlyReport(t, plane)
	if !report.Failed || report.ErrorCode != "response_truncated" {
		t.Fatalf("report = %+v, want a truncation", report)
	}
	want := admissionapi.Effected{Bytes: int64(2 * len(event)), Chunks: 2}
	if report.Effected == nil || *report.Effected != want {
		t.Fatalf("effected = %+v, want %+v", report.Effected, want)
	}
	if len(report.Outcome) != 0 {
		t.Fatalf("a failed report carried an outcome: %s", report.Outcome)
	}
	for _, line := range *logged {
		if strings.Contains(line, "report failed") {
			t.Fatalf("the report was not acknowledged: %q", line)
		}
	}
}

// TestAnUpstreamBreakReportsWhatAlreadyLeft: the provider dropped the
// connection after one event. The caller's writer never failed, and the event
// it accepted is still an egress the proxy cannot recall.
func TestAnUpstreamBreakReportsWhatAlreadyLeft(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
	egressing(governed)
	pointAt(t, governed, streamingUpstream(t, 1, true))

	writer := newCutWriter(100)
	serveTo(governed, writer)

	report := onlyReport(t, plane)
	if !report.Failed || report.ErrorCode != "response_truncated" {
		t.Fatalf("report = %+v, want a truncation", report)
	}
	want := admissionapi.Effected{Bytes: int64(len(event)), Chunks: 1}
	if report.Effected == nil || *report.Effected != want {
		t.Fatalf("effected = %+v, want %+v", report.Effected, want)
	}
}

// TestNothingLeftReportsNoVolume: the caller was gone before the first byte.
// That is a non-event, and the report must say so by omitting the field — the
// same bytes a report sent before it existed — rather than by a zero, and
// never by chunks without bytes, which the plane refuses.
func TestNothingLeftReportsNoVolume(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
	egressing(governed)
	pointAt(t, governed, streamingUpstream(t, 3, false))

	var raw []byte
	inner := plane.server.Config.Handler
	plane.server.Config.Handler = http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if strings.HasSuffix(request.URL.Path, "/report") {
				raw, _ = io.ReadAll(request.Body)
				request.Body = io.NopCloser(strings.NewReader(string(raw)))
			}
			inner.ServeHTTP(writer, request)
		})

	writer := newCutWriter(0)
	serveTo(governed, writer)

	report := onlyReport(t, plane)
	if report.ErrorCode != "response_truncated" || report.Effected != nil {
		t.Fatalf("report = %+v, want a truncation with no volume", report)
	}
	if writer.flushes == 0 {
		t.Fatal("relay never flushed, so a chunk counted without bytes " +
			"could not have been seen")
	}
	if strings.Contains(string(raw), "effected") {
		t.Fatalf("the wire carried the field: %s", raw)
	}
}

// TestAVolumeIsSentOnlyWhereThePlaneAcceptsIt: the plane refuses a volume on
// a success and on an action that declares no egress, and a refused report
// leaves the grant unreported. So the proxy does not send one there, whatever
// it counted.
func TestAVolumeIsSentOnlyWhereThePlaneAcceptsIt(t *testing.T) {
	t.Run("loopback provider declares no egress", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
		// The default for a loopback upstream: reads-corpus only.
		pointAt(t, governed, streamingUpstream(t, 3, false))
		serveTo(governed, newCutWriter(1))
		report := onlyReport(t, plane)
		if report.ErrorCode != "response_truncated" || report.Effected != nil {
			t.Fatalf("report = %+v, want a truncation with no volume", report)
		}
	})
	t.Run("a completed stream", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
		egressing(governed)
		pointAt(t, governed, streamingUpstream(t, 2, false))
		serveTo(governed, newCutWriter(100))
		report := onlyReport(t, plane)
		if report.Failed || report.Effected != nil {
			t.Fatalf("report = %+v, want a success with no volume", report)
		}
	})
}

// TestTheCounterCountsOnlyWhatLeft pins the counter's two rules directly.
func TestTheCounterCountsOnlyWhatLeft(t *testing.T) {
	// A flush that pushes nothing is not a chunk.
	writer := newCutWriter(0)
	counter := &egressCounter{ResponseWriter: writer}
	counter.Flush()
	_, _ = counter.Write([]byte("refused"))
	counter.Flush()
	if counter.chunks != 0 || counter.bytes != 0 || counter.volume() != nil {
		t.Fatalf("nothing left, counted bytes=%d chunks=%d",
			counter.bytes, counter.chunks)
	}

	// One chunk per flush that pushed accepted bytes.
	counter = &egressCounter{ResponseWriter: newCutWriter(10)}
	_, _ = counter.Write([]byte("abc"))
	_, _ = counter.Write([]byte("de"))
	counter.Flush()
	counter.Flush()
	_, _ = counter.Write([]byte("f"))
	counter.Flush()
	if got := counter.volume(); got == nil || *got != (admissionapi.Effected{Bytes: 6, Chunks: 2}) {
		t.Fatalf("volume = %+v, want 6 bytes in 2 chunks", got)
	}

	// A writer that cannot flush pushed no chunk.
	recorder := &failingWriter{header: http.Header{}, failAfter: 10}
	counter = &egressCounter{ResponseWriter: recorder}
	_, _ = counter.Write([]byte("abc"))
	counter.Flush()
	if got := counter.volume(); got == nil || *got != (admissionapi.Effected{Bytes: 3}) {
		t.Fatalf("volume = %+v, want 3 bytes and no chunks", got)
	}

	// And chunks never travel without bytes, whatever state produced them.
	if got := (&egressCounter{chunks: 4}).volume(); got != nil {
		t.Fatalf("chunks without bytes = %+v, want nothing", got)
	}
}

// TestAResendCarriesTheFirstAttemptsVolume: the first report's answer was
// lost. The resend is the identical report — the plane compares it with what
// it recorded, and a different volume would be refused — so it is taken from
// the report already built, not re-read from wherever the number came from.
//
// The test makes re-reading observable by changing the caller's value between
// the two attempts. Nothing in the proxy does that today (the count is read
// after the handler stops writing); the point is that the resend could not
// follow it if something did.
func TestAResendCarriesTheFirstAttemptsVolume(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
	egressing(governed)

	effected := &admissionapi.Effected{Bytes: 120, Chunks: 3}
	var bodies []string
	var mu sync.Mutex
	inner := plane.server.Config.Handler
	plane.server.Config.Handler = http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			raw, _ := io.ReadAll(request.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			first := len(bodies) == 1
			mu.Unlock()
			if first {
				// Recorded, then the answer is lost.
				var decoded admissionapi.Report
				_ = json.Unmarshal(raw, &decoded)
				plane.reports = append(plane.reports, decoded)
				effected.Bytes, effected.Chunks = 999, 9
				conn, _, _ := writer.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			request.Body = io.NopCloser(strings.NewReader(string(raw)))
			inner.ServeHTTP(writer, request)
		})

	token := *plane.token
	identity := callerIdentity{RequestID: "cmVxdWVzdA", CorrelationID: "Y29y"}
	err := governed.admission.report(context.Background(), token, identity,
		nil, "response_truncated", effected, time.Now())
	if err != nil {
		t.Fatalf("the resend was not accepted: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want the first and one resend", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the resend differed from the first attempt:\n%s\n%s",
			bodies[0], bodies[1])
	}
	if got := plane.reports[1].Effected; got == nil ||
		*got != (admissionapi.Effected{Bytes: 120, Chunks: 3}) {
		t.Fatalf("resent volume = %+v, want the first attempt's", got)
	}
}

// replayingProvider records the first report under a token and compares every
// later one with it, as the admission service does (sameReportedOutcome): the
// identical report is answered with the record, a different one — including
// a different volume — is ErrAdmissionSpent.
type replayingProvider struct {
	handlerProvider
	committed map[string]fleet.AdmissionReport
}

func (p *replayingProvider) Report(
	ctx context.Context, report fleet.AdmissionReport,
) (fleet.ActionRecord, error) {
	p.mu.Lock()
	prior, seen := p.committed[string(report.Token.ActionID)]
	if !seen {
		if p.committed == nil {
			p.committed = map[string]fleet.AdmissionReport{}
		}
		p.committed[string(report.Token.ActionID)] = report
	}
	p.mu.Unlock()
	if seen && (prior.Failed != report.Failed ||
		prior.ErrorCode != report.ErrorCode ||
		prior.Effected != report.Effected ||
		string(prior.Outcome) != string(report.Outcome)) {
		p.mu.Lock()
		p.reports = append(p.reports, report)
		p.mu.Unlock()
		return fleet.ActionRecord{}, fleet.ErrAdmissionSpent
	}
	return p.handlerProvider.Report(ctx, report)
}

// loseFirstReport delivers the first report and then loses its answer.
type loseFirstReport struct {
	inner http.RoundTripper
	mu    sync.Mutex
	lost  bool
}

func (l *loseFirstReport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := l.inner.RoundTrip(request)
	if err != nil || !strings.HasSuffix(request.URL.Path, "/report") {
		return response, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return response, nil
	}
	l.lost = true
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, io.ErrUnexpectedEOF
}

// TestALostReportIsResentIdenticallyAndAccepted drives the whole path against
// the real admission handler: a stream cut after two events, the report's
// answer lost, the resend accepted because it is the same report. And the
// comparison is real — the same failure with a different volume is refused —
// so acceptance here means the gateway did not send a different one.
func TestALostReportIsResentIdenticallyAndAccepted(t *testing.T) {
	provider := &replayingProvider{
		handlerProvider: handlerProvider{outcome: fleet.AdmissionAllowed},
	}
	upstream := newFakeUpstream(t)
	governed, logged := newHandlerProxyOver(t, provider, upstream)
	egressing(governed)
	pointAt(t, governed, streamingUpstream(t, 4, false))
	lossy := &loseFirstReport{inner: governed.admission.http.Transport}
	governed.admission.http.Transport = lossy

	serveTo(governed, newCutWriter(2))

	if len(provider.reports) != 2 {
		t.Fatalf("reports = %d, want the first and its resend (logs %q)",
			len(provider.reports), *logged)
	}
	want := fleet.EffectedVolume{Bytes: int64(2 * len(event)), Chunks: 2}
	for index, report := range provider.reports {
		if !report.Failed || report.ErrorCode != "response_truncated" ||
			report.Effected != want {
			t.Fatalf("report %d = %+v, want a truncation of %+v", index, report, want)
		}
	}
	for _, line := range *logged {
		if strings.Contains(line, "report failed") {
			t.Fatalf("the resend was refused: %q", line)
		}
	}

	// The fixture's comparison is not vacuous: the same failure under the
	// same token with any other volume is refused.
	recorded := provider.reports[0]
	token := admissionapi.Token{
		ActionID: admissionapi.EncodeID(recorded.Token.ActionID),
		TokenID:  admissionapi.EncodeID(recorded.Token.TokenID),
		Version:  recorded.Token.Version, ExpiresAt: time.Now().Add(time.Minute),
	}
	err := governed.admission.report(context.Background(), token,
		callerIdentity{RequestID: "cmVxdWVzdA"}, nil, "response_truncated",
		&admissionapi.Effected{Bytes: want.Bytes + 1, Chunks: want.Chunks}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("a different volume = %v, want the plane's 409", err)
	}
}

// TestTheReportItselfRefusesAVolumeThePlaneWould: the admission client is the
// last place before the wire, so it applies the plane's rules itself whatever
// its caller passes — no volume on a success, none without bytes — rather
// than relying on the counter having been right.
func TestTheReportItselfRefusesAVolumeThePlaneWould(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	governed, _ := newTestProxy(t, plane, newFakeUpstream(t))
	egressing(governed)
	identity := callerIdentity{RequestID: "cmVxdWVzdA"}
	for name, row := range map[string]struct {
		failure  string
		effected *admissionapi.Effected
	}{
		"chunks without bytes": {"response_truncated", &admissionapi.Effected{Chunks: 2}},
		"on a success":         {"", &admissionapi.Effected{Bytes: 10, Chunks: 1}},
	} {
		plane.reports = nil
		if err := governed.admission.report(context.Background(), *plane.token,
			identity, json.RawMessage(`{}`), row.failure, row.effected,
			time.Now()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := onlyReport(t, plane).Effected; got != nil {
			t.Errorf("%s: sent %+v, want no volume", name, got)
		}
	}
}
