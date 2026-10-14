// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorattest"
	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/collector"
	collectorapi "github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/sdk"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const testCollector = shoal.ID("collector:tail")

type collectorFixture struct {
	registry  *collectorregistry.Registry
	provider  *collectorregistry.Provider
	authority *auth.Authority
	handler   *Handler
	decisions map[string]auth.Decision
}

func collectorPrincipal(t *testing.T, subject string) auth.Decision {
	t.Helper()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(subject), Actor: shoal.ID(subject), ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationIngest, auth.OperationRead}, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	return d
}

// newCollectorFixture wires the real engine-backed registry behind an
// authenticated handler. Bearer tokens name principals.
func newCollectorFixture(t *testing.T) *collectorFixture {
	t.Helper()
	eng, e := engine.Open(t.TempDir(), engine.Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if e = eng.CreateTable(collectorregistry.Table, engine.TableOptions{}); e != nil {
		t.Fatal(e)
	}
	backend, e := explorercoord.NewEngineStore(eng, collectorregistry.Table)
	if e != nil {
		t.Fatal(e)
	}
	f := &collectorFixture{authority: auth.NewAuthority(), decisions: map[string]auth.Decision{}}
	for _, subject := range []string{"tail", "intruder"} {
		f.decisions[subject] = collectorPrincipal(t, subject)
	}
	set, _ := collectorattest.NewSet()
	if f.registry, e = collectorregistry.New(collectorregistry.Config{Backend: backend, Resolver: f.authority.Resolver(), Attestation: set, Clock: time.Now}); e != nil {
		t.Fatal(e)
	}
	if f.provider, e = collectorregistry.NewProvider(f.registry); e != nil {
		t.Fatal(e)
	}
	authenticate := AuthenticatorFunc(func(r *http.Request) (auth.Decision, error) {
		if d, ok := f.decisions[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
			return d, nil
		}
		return auth.Decision{}, shoal.NewError(shoal.ErrorUnauthorized, "unknown token")
	})
	if f.handler, e = NewAuthenticatedHandler(&stubWorkspaceService{}, authenticate, f.authority.Binder(), "example.test"); e != nil {
		t.Fatal(e)
	}
	if e = f.handler.MountCollectors(f.provider, f.authority.Resolver()); e != nil {
		t.Fatal(e)
	}
	if _, e = f.registry.Provision(context.Background(), collectorregistry.Provisioning{CollectorID: testCollector, Subject: "tail", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved}); e != nil {
		t.Fatal(e)
	}
	return f
}

// client returns an SDK client for token against an httptest server that the
// handler sees as example.test.
func (f *collectorFixture) client(t *testing.T, token string) *sdk.Client {
	t.Helper()
	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	c, e := sdk.New(sdk.Config{BaseURL: "http://example.test", HTTPClient: &http.Client{Transport: transport}, Token: func(context.Context) (string, error) { return token, nil }})
	if e != nil {
		t.Fatal(e)
	}
	return c
}

var (
	extractorV1 = collector.ExtractorRef{ID: "extractor:lines", Version: "1"}
	extractorV2 = collector.ExtractorRef{ID: "extractor:lines", Version: "2"}
)

func collectorEnroll(authority ...shoal.ID) collector.EnrollRequest {
	return collector.EnrollRequest{CollectorID: testCollector, RequestedAuthorityPolicyIDs: authority, Extractors: []collector.ExtractorRef{extractorV1, extractorV2}}
}

func collectorArtifact(id, body string) collector.ArtifactRef {
	sum := sha256.Sum256([]byte(body))
	return collector.ArtifactRef{ID: shoal.ID(id), Digest: hex.EncodeToString(sum[:]), Size: int64(len(body)), MediaType: "text/plain", ObservedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second)}
}

func collectorObservation(t *testing.T, artifactID string, extractor collector.ExtractorRef, payload []byte) collector.Observation {
	t.Helper()
	o, e := collector.NewObservation(collector.ObservationConfig{CollectorID: testCollector, ArtifactID: shoal.ID(artifactID), Extractor: extractor, Confidence: collector.Confidence{Disposition: collector.Extracted}, SubjectID: "host:web-1", Kind: "log_line", Payload: payload, ObservedAt: time.Now().UTC().Add(-time.Second).Truncate(time.Second)})
	if e != nil {
		t.Fatal(e)
	}
	return o
}

func TestCollectorSDKEndToEnd(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	collectors := f.client(t, "tail").Collectors()

	// Asserting authority that was not provisioned is refused, definitely,
	// and records nothing: the same key then enrolls within the ceiling.
	_, e := collectors.Enroll(ctx, []byte("key-1"), collectorEnroll("authority:logs", "authority:admin"))
	if !errors.Is(e, collectorapi.ErrPermissionDenied) || errors.Is(e, collectorapi.ErrIndeterminate) {
		t.Fatalf("excess authority: %v", e)
	}
	if reg, _ := f.registry.Registration(ctx, testCollector); reg.State != collector.Provisioned {
		t.Fatal("refused enrollment changed the registration")
	}
	receipt, e := collectors.Enroll(ctx, []byte("key-1"), collectorEnroll("authority:logs"))
	if e != nil || receipt.Generation != 1 || receipt.State != "enrolled" {
		t.Fatalf("enroll: %+v %v", receipt, e)
	}
	retry, e := collectors.Enroll(ctx, []byte("key-1"), collectorEnroll("authority:logs"))
	if e != nil || retry != receipt {
		t.Fatalf("same-key retry: %+v %v", retry, e)
	}

	// An authenticated principal not bound to the collector sees nothing.
	intruder := f.client(t, "intruder").Collectors()
	var httpErr *collectorapi.HTTPError
	if _, e = intruder.Enroll(ctx, []byte("key-x"), collectorEnroll("authority:logs")); !errors.As(e, &httpErr) || httpErr.Status != http.StatusNotFound || httpErr.Indeterminate {
		t.Fatalf("unbound enroll: %v", e)
	}
	if _, e = intruder.SubmitArtifact(ctx, testCollector, collectorArtifact("artifact:1", "x")); !errors.As(e, &httpErr) || httpErr.Status != http.StatusNotFound {
		t.Fatalf("unbound artifact: %v", e)
	}

	ref := collectorArtifact("artifact:1", "GET /healthz 200\n")
	if _, e = collectors.SubmitArtifact(ctx, testCollector, ref); e != nil {
		t.Fatal(e)
	}
	first := collectorObservation(t, "artifact:1", extractorV1, []byte(`{"status":200}`))
	second := collectorObservation(t, "artifact:1", extractorV2, []byte(`{"status":200}`))
	for _, o := range []collector.Observation{first, second} {
		if _, e = collectors.SubmitObservation(ctx, o); e != nil {
			t.Fatal(e)
		}
	}
	if first.ID() == second.ID() {
		t.Fatal("extractor versions collide")
	}
	before, e := collectors.ReadObservation(ctx, first.ID())
	if e != nil || before.Status != collectorapi.StatusActive || before.ArtifactDigest != ref.Digest {
		t.Fatalf("read v1: %+v %v", before, e)
	}
	other, e := collectors.ReadObservation(ctx, second.ID())
	if e != nil || other.ArtifactDigest != before.ArtifactDigest || other.Observation.ArtifactID != before.Observation.ArtifactID {
		t.Fatalf("v2 lineage: %+v %v", other, e)
	}

	if _, e = f.registry.Revoke(ctx, testCollector); e != nil {
		t.Fatal(e)
	}
	for _, id := range []shoal.ID{first.ID(), second.ID()} {
		got, e := collectors.ReadObservation(ctx, id)
		if e != nil || got.Status != collectorapi.StatusQuarantined {
			t.Fatalf("not quarantined: %+v %v", got, e)
		}
	}
	if _, e = collectors.SubmitArtifact(ctx, testCollector, collectorArtifact("artifact:2", "y")); !errors.Is(e, collectorapi.ErrPermissionDenied) {
		t.Fatalf("revoked artifact submit: %v", e)
	}
	third := collectorObservation(t, "artifact:1", extractorV1, []byte(`{"status":500}`))
	if _, e = collectors.SubmitObservation(ctx, third); !errors.Is(e, collectorapi.ErrPermissionDenied) {
		t.Fatalf("revoked observation submit: %v", e)
	}
}

func TestCollectorMountRequiresTrustedAuthentication(t *testing.T) {
	f := newCollectorFixture(t)
	anonymous, e := NewHandler(&stubWorkspaceService{}, "example.test")
	if e != nil {
		t.Fatal(e)
	}
	if e = anonymous.MountCollectors(f.provider, f.authority.Resolver()); e == nil {
		t.Fatal("anonymous mount accepted")
	}
	if e = f.handler.MountCollectors(f.provider, f.authority.Resolver()); e == nil {
		t.Fatal("duplicate mount accepted")
	}
	var nilProvider *collectorregistry.Provider
	if _, e = NewCollectorHTTPHandler(nilProvider, f.authority.Resolver()); e == nil {
		t.Fatal("typed nil provider accepted")
	}
	standalone, e := NewCollectorHTTPHandler(f.provider, f.authority.Resolver())
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	standalone.ServeHTTP(w, collectorRequest(t, http.MethodPost, collectorapi.EnrollRoute, collectorapi.EncodeEnroll(collectorEnroll("authority:logs"))))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unbound context: %d", w.Code)
	}
}

func collectorRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var raw []byte
	if body != nil {
		var e error
		if raw, e = json.Marshal(body); e != nil {
			t.Fatal(e)
		}
	}
	r := httptest.NewRequest(method, "http://example.test"+path, bytes.NewReader(raw))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Idempotency-Key", collectorapi.EncodeKey([]byte("key")))
	return r
}

func TestCollectorRejectsAuthorityInRequestBody(t *testing.T) {
	f := newCollectorFixture(t)
	ctx, e := f.authority.Binder().Bind(context.Background(), f.decisions["tail"])
	if e != nil {
		t.Fatal(e)
	}
	handler, _ := NewCollectorHTTPHandler(f.provider, f.authority.Resolver())
	for name, body := range map[string]string{
		"authority field": `{"collector_id":"Y29sbGVjdG9yOnRhaWw","requested_authority_policy_ids":["YXV0aG9yaXR5OmxvZ3M"],"extractors":[{"id":"ZQ","version":"1"}],"authority_policy_ids":["YWRtaW4"]}`,
		"control field":   `{"collector_id":"Y29sbGVjdG9yOnRhaWw","requested_authority_policy_ids":["YXV0aG9yaXR5OmxvZ3M"],"extractors":[{"id":"ZQ","version":"1"}],"control":"registry_controlled"}`,
		"duplicate key":   `{"collector_id":"Y29sbGVjdG9yOnRhaWw","collector_id":"Y29sbGVjdG9yOnRhaWw","requested_authority_policy_ids":["YXV0aG9yaXR5OmxvZ3M"],"extractors":[{"id":"ZQ","version":"1"}]}`,
		"null list":       `{"collector_id":"Y29sbGVjdG9yOnRhaWw","requested_authority_policy_ids":null,"extractors":[{"id":"ZQ","version":"1"}]}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+collectorapi.EnrollRoute, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", collectorapi.EncodeKey([]byte("key")))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if reg, _ := f.registry.Registration(ctx, testCollector); reg.State != collector.Provisioned {
		t.Fatal("rejected body changed the registration")
	}
}

// Commit-bearing collector routes must stay readable under a narrowed
// workspace output budget, because they echo no submitted content. The read
// route commits nothing, so its overflow is a plain failure.
func TestCollectorReceiptsSurviveNarrowOutputBudget(t *testing.T) {
	const budget = 1024
	f := newCollectorFixture(t)
	ctx, e := f.authority.Binder().Bind(context.Background(), f.decisions["tail"])
	if e != nil {
		t.Fatal(e)
	}
	handler, _ := NewCollectorHTTPHandler(f.provider, f.authority.Resolver())
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: budget, indeterminateOnOverflow: requestMayCommit(r.Method, r.URL.Path)}, r.WithContext(ctx))
		return w
	}
	payload := bytes.Repeat([]byte("p"), 4*budget)
	enroll := collectorEnroll("authority:logs")
	enroll.Attestation = &collector.AttestationReport{Kind: "unverified", Format: "blob", Evidence: bytes.Repeat([]byte("e"), 4*budget)}
	o := collectorObservation(t, "artifact:1", extractorV1, payload)
	steps := []struct {
		path string
		body any
		out  interface{ Validate() error }
	}{
		{collectorapi.EnrollRoute, collectorapi.EncodeEnroll(enroll), &collectorapi.EnrollReceipt{}},
		{collectorapi.ArtifactsRoute, collectorapi.EncodeArtifact(testCollector, collectorArtifact("artifact:1", "body")), &collectorapi.ArtifactReceipt{}},
		{collectorapi.ObservationsRoute, collectorapi.ObservationRequest{Observation: collectorapi.EncodeObservation(o)}, &collectorapi.ObservationReceipt{}},
	}
	for _, step := range steps {
		if !requestMayCommit(http.MethodPost, step.path) {
			t.Fatalf("%s is not listed as commit-bearing", step.path)
		}
		w := serve(collectorRequest(t, http.MethodPost, step.path, step.body))
		if w.Code != http.StatusOK || w.Body.Len() > budget || w.Header().Get(CommitOutcomeHeader) != "" {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body.String())
		}
		if e := collectorapi.DecodeStrict(w.Body.Bytes(), step.out); e != nil || step.out.Validate() != nil {
			t.Fatalf("%s: unreadable receipt %v", step.path, e)
		}
	}
	readPath := collectorapi.ObservationsRoute + "/" + collectorapi.EncodeID(o.ID())
	if requestMayCommit(http.MethodGet, readPath) {
		t.Fatal("read route listed as commit-bearing")
	}
	w := serve(collectorRequest(t, http.MethodGet, readPath, nil))
	if w.Code != http.StatusInternalServerError || w.Header().Get(CommitOutcomeHeader) != "" || strings.Contains(w.Body.String(), "indeterminate") {
		t.Fatalf("read overflow: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	// With room, the same read returns the full content.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, collectorRequest(t, http.MethodGet, readPath, nil).WithContext(ctx))
	var record collectorapi.ObservationRecord
	if w.Code != http.StatusOK || collectorapi.DecodeStrict(w.Body.Bytes(), &record) != nil || record.Validate() != nil {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
}

// At exactly MaxReceiptBytes a write with every ID at its maximum length
// still returns a readable receipt; below the receipt's size the committed
// write reports indeterminate (safe, but unreadable), never a failure.
func TestCollectorReceiptBudgetBoundary(t *testing.T) {
	f := newCollectorFixture(t)
	long := shoal.ID(strings.Repeat("c", shoal.MaxIDBytes))
	if _, e := f.registry.Provision(context.Background(), collectorregistry.Provisioning{CollectorID: long, Subject: "tail", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved}); e != nil {
		t.Fatal(e)
	}
	ctx, e := f.authority.Binder().Bind(context.Background(), f.decisions["tail"])
	if e != nil {
		t.Fatal(e)
	}
	handler, _ := NewCollectorHTTPHandler(f.provider, f.authority.Resolver())
	serve := func(budget uint64, path string, body any) *httptest.ResponseRecorder {
		r := collectorRequest(t, http.MethodPost, path, body).WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: budget, indeterminateOnOverflow: requestMayCommit(r.Method, r.URL.Path)}, r)
		return w
	}
	enroll := collector.EnrollRequest{CollectorID: long, RequestedAuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Extractors: []collector.ExtractorRef{extractorV1}}
	if w := serve(collectorapi.MaxReceiptBytes, collectorapi.EnrollRoute, collectorapi.EncodeEnroll(enroll)); w.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	ref := collectorArtifact(strings.Repeat("a", shoal.MaxIDBytes), "body")
	w := serve(collectorapi.MaxReceiptBytes, collectorapi.ArtifactsRoute, collectorapi.EncodeArtifact(long, ref))
	var receipt collectorapi.ArtifactReceipt
	if w.Code != http.StatusOK || collectorapi.DecodeStrict(w.Body.Bytes(), &receipt) != nil || receipt.Validate() != nil {
		t.Fatalf("max-ID receipt at MaxReceiptBytes: %d %s", w.Code, w.Body.String())
	}
	// Retry the same committed write under a budget smaller than its receipt.
	w = serve(256, collectorapi.ArtifactsRoute, collectorapi.EncodeArtifact(long, ref))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get(CommitOutcomeHeader) != CommitOutcomeIndeterminate {
		t.Fatalf("undersized budget: %d %v", w.Code, w.Header())
	}
}
