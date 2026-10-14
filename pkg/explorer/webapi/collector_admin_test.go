// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/pkg/collector"
	collectorapi "github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func adminPrincipal(t *testing.T, subject, domain string, ops ...auth.Operation) auth.Decision {
	t.Helper()
	if len(ops) == 0 {
		ops = []auth.Operation{auth.OperationCollectorAdmin}
	}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(subject), Actor: shoal.ID(subject), ClientID: "admin-cli", AuthorizationDomain: []byte(domain), AllowedOperations: ops, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "admin-request"})
	if e != nil {
		t.Fatal(e)
	}
	return d
}

// newCollectorAdminFixture is the collector fixture with the admin routes
// mounted and admin tokens.
func newCollectorAdminFixture(t *testing.T) *collectorFixture {
	t.Helper()
	f := newCollectorFixture(t)
	f.decisions["admin"] = adminPrincipal(t, "admin", "domain")
	f.decisions["foreign-admin"] = adminPrincipal(t, "admin", "other-domain")
	f.decisions["writer"] = adminPrincipal(t, "writer", "domain", auth.OperationIngest, auth.OperationRead, auth.OperationWorkspaceSettingsWrite, auth.OperationAgentRevoke, auth.OperationDelegate)
	f.decisions["new"] = collectorPrincipal(t, "new")
	admin, e := collectorregistry.NewAdminProvider(f.registry)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.handler.MountCollectorAdmin(admin, f.authority.Resolver()); e != nil {
		t.Fatal(e)
	}
	return f
}

func newGrant(generation int64, authority ...shoal.ID) collectorapi.Provisioning {
	return collectorapi.Provisioning{CollectorID: "collector:new", Subject: "new", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: authority, Control: collector.ExternalControlled, Mode: collector.ServerObserved, Generation: generation}
}

// The whole life of a collector over HTTP, through sdk.Collectors(): an
// admin provisions it, it enrolls and submits within its ceiling, the admin
// revokes it and its observations read as quarantined, and the registration
// reads back with every change attributed to the admin.
func TestCollectorAdminSDKEndToEnd(t *testing.T) {
	f := newCollectorAdminFixture(t)
	ctx := context.Background()
	admin := f.client(t, "admin").Collectors()
	receipt, e := admin.ProvisionCollector(ctx, newGrant(1, "authority:logs"))
	if e != nil || !receipt.Changed || receipt.State != "provisioned" || receipt.Generation != 1 {
		t.Fatalf("provision: %+v %v", receipt, e)
	}
	if retry, e := admin.ProvisionCollector(ctx, newGrant(1, "authority:logs")); e != nil || retry.Changed || retry.Generation != 1 {
		t.Fatalf("idempotent provision: %+v %v", retry, e)
	}
	var httpErr *collectorapi.HTTPError
	if _, e := admin.ProvisionCollector(ctx, newGrant(1, "authority:logs", "authority:admin")); !errors.As(e, &httpErr) || httpErr.Status != http.StatusConflict {
		t.Fatalf("authority raise on an active registration: %v", e)
	}

	collector := f.client(t, "new").Collectors()
	enroll := collectorEnroll("authority:logs", "authority:admin")
	enroll.CollectorID = "collector:new"
	if _, e := collector.Enroll(ctx, []byte("k"), enroll); !errors.Is(e, collectorapi.ErrPermissionDenied) {
		t.Fatalf("excess authority: %v", e)
	}
	enroll.RequestedAuthorityPolicyIDs = enroll.RequestedAuthorityPolicyIDs[:1]
	if _, e := collector.Enroll(ctx, []byte("k"), enroll); e != nil {
		t.Fatal(e)
	}
	if _, e := collector.SubmitArtifact(ctx, "collector:new", collectorArtifact("artifact:1", "x")); e != nil {
		t.Fatal(e)
	}
	o, e := newCollectorObservation("artifact:1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := collector.SubmitObservation(ctx, o); e != nil {
		t.Fatal(e)
	}
	if got, e := collector.ReadObservation(ctx, o.ID()); e != nil || got.Status != collectorapi.StatusActive {
		t.Fatalf("before revoke: %+v %v", got, e)
	}

	revoked, e := admin.RevokeCollector(ctx, "collector:new", 1)
	if e != nil || !revoked.Changed || revoked.Generation != 2 || revoked.State != "revoked" {
		t.Fatalf("revoke: %+v %v", revoked, e)
	}
	if got, e := collector.ReadObservation(ctx, o.ID()); e != nil || got.Status != collectorapi.StatusQuarantined {
		t.Fatalf("after revoke: %+v %v", got, e)
	}
	if again, e := admin.RevokeCollector(ctx, "collector:new", 1); e != nil || again.Changed {
		t.Fatalf("idempotent revoke: %+v %v", again, e)
	}
	if _, e := admin.ProvisionCollector(ctx, newGrant(1, "authority:logs")); !errors.As(e, &httpErr) || httpErr.Status != http.StatusConflict {
		t.Fatalf("stale provision after revoke: %v", e)
	}

	record, e := admin.CollectorRegistration(ctx, "collector:new")
	if e != nil {
		t.Fatal(e)
	}
	if record.State != "revoked" || record.Generation != 2 || len(record.Changes) != 2 || len(record.Gaps) != 0 || len(record.Anomalies) != 0 || record.EnrollmentID != "" {
		t.Fatalf("record %+v", record)
	}
	for _, c := range record.Changes {
		if c.Source != collectorapi.SourceCollectorAdmin || c.By == nil || c.By.Subject != collectorapi.EncodeID("admin") || c.By.AuthorizationFingerprint == "" {
			t.Fatalf("change not attributed to the admin: %+v", c)
		}
	}
	if record.Changes[0].Action != collectorapi.ChangeRevoke || record.Changes[1].Action != collectorapi.ChangeProvision {
		t.Fatalf("changes %+v", record.Changes)
	}
}

func newCollectorObservation(artifactID string) (collector.Observation, error) {
	return collector.NewObservation(collector.ObservationConfig{CollectorID: "collector:new", ArtifactID: shoal.ID(artifactID), Extractor: extractorV1, Confidence: collector.Confidence{Disposition: collector.Extracted}, Kind: "log_line", Payload: []byte("{}"), ObservedAt: time.Now().UTC().Add(-time.Second).Truncate(time.Second)})
}

func TestCollectorAdminRoutesRefuseEveryoneButAnAdmin(t *testing.T) {
	f := newCollectorAdminFixture(t)
	ctx := context.Background()
	for _, token := range []string{"tail", "writer", "unknown"} {
		c := f.client(t, token).Collectors()
		var httpErr *collectorapi.HTTPError
		if _, e := c.ProvisionCollector(ctx, newGrant(1, "authority:logs")); !errors.As(e, &httpErr) || httpErr.Status != http.StatusUnauthorized {
			t.Fatalf("%s provision: %v", token, e)
		}
		if _, e := c.RevokeCollector(ctx, testCollector, 1); !errors.As(e, &httpErr) || httpErr.Status != http.StatusUnauthorized {
			t.Fatalf("%s revoke: %v", token, e)
		}
		if _, e := c.CollectorRegistration(ctx, testCollector); !errors.As(e, &httpErr) || httpErr.Status != http.StatusUnauthorized {
			t.Fatalf("%s read: %v", token, e)
		}
	}
	if _, e := f.registry.Registration(ctx, "collector:new"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("refused provision wrote: %v", e)
	}
	if reg, _ := f.registry.Registration(ctx, testCollector); reg.State != collector.Provisioned {
		t.Fatalf("refused revoke wrote: %+v", reg)
	}
	// The admin token buys nothing on the collector routes.
	enroll := collectorEnroll("authority:logs")
	var httpErr *collectorapi.HTTPError
	if _, e := f.client(t, "admin").Collectors().Enroll(ctx, []byte("k"), enroll); !errors.As(e, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("admin enrolled: %v", e)
	}
}

func TestCollectorAdminCannotProvisionItselfOverHTTP(t *testing.T) {
	f := newCollectorAdminFixture(t)
	self := newGrant(1, "authority:logs")
	self.Subject = "admin"
	if _, e := f.client(t, "admin").Collectors().ProvisionCollector(context.Background(), self); !errors.Is(e, collectorapi.ErrPermissionDenied) || errors.Is(e, collectorapi.ErrIndeterminate) {
		t.Fatalf("self provision: %v", e)
	}
	if _, e := f.registry.Registration(context.Background(), "collector:new"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("self provision wrote: %v", e)
	}
}

// A foreign domain's collector answers byte for byte like a nonexistent one.
func TestCollectorAdminConcealsOtherDomains(t *testing.T) {
	f := newCollectorAdminFixture(t)
	ctx, e := f.authority.Binder().Bind(context.Background(), f.decisions["foreign-admin"])
	if e != nil {
		t.Fatal(e)
	}
	admin, _ := collectorregistry.NewAdminProvider(f.registry)
	handler, _ := NewCollectorAdminHTTPHandler(admin, f.authority.Resolver())
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		return w
	}
	for _, pair := range [][2]*http.Request{
		{
			collectorRequest(t, http.MethodGet, collectorapi.AdminRegistrationsRoute+"/"+collectorapi.EncodeID(testCollector), nil),
			collectorRequest(t, http.MethodGet, collectorapi.AdminRegistrationsRoute+"/"+collectorapi.EncodeID("collector:absent"), nil),
		},
		{
			collectorRequest(t, http.MethodPost, collectorapi.AdminRevokeRoute, collectorapi.RevokeRequest{CollectorID: collectorapi.EncodeID(testCollector), Generation: 1}),
			collectorRequest(t, http.MethodPost, collectorapi.AdminRevokeRoute, collectorapi.RevokeRequest{CollectorID: collectorapi.EncodeID("collector:absent"), Generation: 1}),
		},
	} {
		existing, absent := serve(pair[0]), serve(pair[1])
		if existing.Code != http.StatusNotFound || existing.Code != absent.Code || existing.Body.String() != absent.Body.String() || existing.Header().Get(CommitOutcomeHeader) != absent.Header().Get(CommitOutcomeHeader) {
			t.Fatalf("%s: existing %d %q, absent %d %q", pair[0].URL.Path, existing.Code, existing.Body.String(), absent.Code, absent.Body.String())
		}
	}
	if reg, _ := f.registry.Registration(context.Background(), testCollector); reg.State != collector.Provisioned {
		t.Fatalf("foreign admin revoked: %+v", reg)
	}
}

func TestCollectorAdminRejectsMalformedRequests(t *testing.T) {
	f := newCollectorAdminFixture(t)
	ctx, e := f.authority.Binder().Bind(context.Background(), f.decisions["admin"])
	if e != nil {
		t.Fatal(e)
	}
	admin, _ := collectorregistry.NewAdminProvider(f.registry)
	handler, _ := NewCollectorAdminHTTPHandler(admin, f.authority.Resolver())
	valid := `"collector_id":"Y29sbGVjdG9yOm5ldw","subject":"bmV3","domain":"ZG9tYWlu","authority_policy_ids":["YXV0aG9yaXR5OmxvZ3M"],"control":"external_controlled","mode":"server_observed"`
	for name, body := range map[string]string{
		"zero generation":     `{` + valid + `,"generation":0}`,
		"missing generation":  `{` + valid + `}`,
		"unknown field":       `{` + valid + `,"generation":1,"state":"enrolled"}`,
		"duplicate key":       `{` + valid + `,"generation":1,"generation":1}`,
		"case alias":          `{` + valid + `,"Generation":1}`,
		"null list":           `{"collector_id":"Y29sbGVjdG9yOm5ldw","subject":"bmV3","domain":"ZG9tYWlu","authority_policy_ids":null,"control":"external_controlled","mode":"server_observed","generation":1}`,
		"bad control":         `{"collector_id":"Y29sbGVjdG9yOm5ldw","subject":"bmV3","domain":"ZG9tYWlu","authority_policy_ids":["YQ"],"control":"trusted","mode":"server_observed","generation":1}`,
		"duplicate authority": `{"collector_id":"Y29sbGVjdG9yOm5ldw","subject":"bmV3","domain":"ZG9tYWlu","authority_policy_ids":["YQ","YQ"],"control":"external_controlled","mode":"server_observed","generation":1}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+collectorapi.AdminProvisionRoute, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if _, e := f.registry.Registration(context.Background(), "collector:new"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("a rejected body wrote: %v", e)
	}
	// Unknown admin paths and methods.
	for _, r := range []*http.Request{
		collectorRequest(t, http.MethodPost, collectorapi.AdminRoute+"/enroll", collectorapi.RevokeRequest{CollectorID: "Yw", Generation: 1}),
		collectorRequest(t, http.MethodGet, collectorapi.AdminRegistrationsRoute, nil),
		collectorRequest(t, http.MethodGet, collectorapi.AdminProvisionRoute, nil),
		collectorRequest(t, http.MethodPost, collectorapi.AdminRegistrationsRoute+"/Yw", collectorapi.RevokeRequest{CollectorID: "Yw", Generation: 1}),
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		if w.Code != http.StatusNotFound || w.Header().Get(CommitOutcomeHeader) != "" {
			t.Fatalf("%s %s: %d %s", r.Method, r.URL.Path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, collectorRequest(t, http.MethodPut, collectorapi.AdminProvisionRoute, nil).WithContext(ctx))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d", w.Code)
	}
}

// Without MountCollectorAdmin the admin paths answer exactly as any unknown
// collector path did before slice 5, and the collector routes are unchanged.
func TestCollectorAdminIsOptIn(t *testing.T) {
	f := newCollectorFixture(t)
	f.decisions["admin"] = adminPrincipal(t, "admin", "domain")
	var httpErr *collectorapi.HTTPError
	if _, e := f.client(t, "admin").Collectors().ProvisionCollector(context.Background(), newGrant(1, "authority:logs")); !errors.As(e, &httpErr) || httpErr.Status != http.StatusNotFound {
		t.Fatalf("unmounted admin route: %v", e)
	}
	if _, e := f.registry.Registration(context.Background(), "collector:new"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("unmounted admin route wrote: %v", e)
	}
	g := newCollectorAdminFixture(t)
	if e := g.handler.MountCollectorAdmin(&collectorregistry.AdminProvider{}, g.authority.Resolver()); e == nil {
		t.Fatal("duplicate admin mount accepted")
	}
	anonymous, _ := NewHandler(&stubWorkspaceService{}, "example.test")
	admin, _ := collectorregistry.NewAdminProvider(g.registry)
	if e := anonymous.MountCollectorAdmin(admin, g.authority.Resolver()); e == nil {
		t.Fatal("anonymous admin mount accepted")
	}
}

// Admin writes are commit-bearing and their receipts fit a narrow budget.
func TestCollectorAdminReceiptsSurviveNarrowOutputBudget(t *testing.T) {
	f := newCollectorAdminFixture(t)
	ctx, e := f.authority.Binder().Bind(context.Background(), f.decisions["admin"])
	if e != nil {
		t.Fatal(e)
	}
	admin, _ := collectorregistry.NewAdminProvider(f.registry)
	handler, _ := NewCollectorAdminHTTPHandler(admin, f.authority.Resolver())
	long := shoal.ID(strings.Repeat("c", shoal.MaxIDBytes))
	grant := newGrant(1, "authority:logs")
	grant.CollectorID = long
	for _, step := range []struct {
		path string
		body any
	}{
		{collectorapi.AdminProvisionRoute, collectorapi.EncodeProvision(grant)},
		{collectorapi.AdminRevokeRoute, collectorapi.RevokeRequest{CollectorID: collectorapi.EncodeID(long), Generation: 1}},
	} {
		if !requestMayCommit(http.MethodPost, step.path) {
			t.Fatalf("%s is not listed as commit-bearing", step.path)
		}
		r := collectorRequest(t, http.MethodPost, step.path, step.body).WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: collectorapi.MaxAdminReceiptBytes, indeterminateOnOverflow: true}, r)
		var receipt collectorapi.AdminReceipt
		if w.Code != http.StatusOK || collectorapi.DecodeStrict(w.Body.Bytes(), &receipt) != nil || receipt.Validate() != nil {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body.String())
		}
	}
	if requestMayCommit(http.MethodGet, collectorapi.AdminRegistrationsRoute+"/x") {
		t.Fatal("the read route is listed as commit-bearing")
	}
}
