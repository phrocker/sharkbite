// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type datasetHTTPStub struct {
	calls  int
	id     shoal.ID
	reply  decisionapi.DatasetExport
	err    error
	during func()
}

func (p *datasetHTTPStub) Export(_ context.Context, id shoal.ID) (decisionapi.DatasetExport, error) {
	p.calls++
	p.id = id
	if p.during != nil {
		p.during()
	}
	return p.reply, p.err
}
func datasetHTTPHash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func datasetHTTPReply(id shoal.ID) decisionapi.DatasetExport {
	data := []byte("{\"opaque\": [1,2,3]}\n")
	manifest, _ := json.Marshal(map[string]any{"schema": 1, "kind": "authorized-numeric-training-export", "cohort_id": id, "dataset_sha256": datasetHTTPHash(data), "dataset_bytes": len(data)})
	return decisionapi.DatasetExport{CohortID: id, Dataset: data, Manifest: manifest, ManifestSHA256: datasetHTTPHash(manifest)}
}
func datasetHTTPRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://example.test"+DatasetExportsRoute, strings.NewReader(`{"cohort_id":"`+decisionapi.EncodeID("cohort")+`"}`))
	r.Header.Set("Content-Type", "application/json")
	return r
}
func datasetHTTPFixture(t *testing.T) (*Handler, *datasetHTTPStub, *auth.Authority, auth.Decision) {
	t.Helper()
	a := auth.NewAuthority()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(*http.Request) (auth.Decision, error) { return d, nil }), a.Binder(), "example.test")
	if e != nil {
		t.Fatal(e)
	}
	p := &datasetHTTPStub{reply: datasetHTTPReply("cohort")}
	if e = h.MountDatasetExports(p, a.Resolver()); e != nil {
		t.Fatal(e)
	}
	return h, p, a, d
}
func TestDatasetHTTPBytesAndIdentity(t *testing.T) {
	h, p, _, _ := datasetHTTPFixture(t)
	p.reply = datasetHTTPReply("cohort/世界\x00")
	r := datasetHTTPRequest()
	r.Body = ioNopString(`{"cohort_id":"` + decisionapi.EncodeID(p.reply.CohortID) + `"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	got, e := decisionapi.DecodeDatasetExport(w.Body.Bytes())
	if e != nil || got.CohortID != p.reply.CohortID || !bytes.Equal(got.Dataset, p.reply.Dataset) || !bytes.Equal(got.Manifest, p.reply.Manifest) {
		t.Fatal("bytes or identity changed", e)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get(CommitOutcomeHeader) != "" {
		t.Fatal(w.Header())
	}
}
func TestDatasetHTTPAuthenticationAndMounting(t *testing.T) {
	h, p, a, d := datasetHTTPFixture(t)
	if e := h.MountDatasetExports(p, a.Resolver()); e == nil {
		t.Fatal("duplicate mount")
	}
	anonymous, e := NewHandler(&stubWorkspaceService{}, "example.test")
	if e != nil {
		t.Fatal(e)
	}
	if e = anonymous.MountDatasetExports(p, a.Resolver()); e == nil {
		t.Fatal("anonymous mount")
	}
	var nilProvider *datasetHTTPStub
	if _, e = NewDatasetExportHTTPHandler(nilProvider, a.Resolver()); e == nil {
		t.Fatal("typednil provider")
	}
	handler, e := NewDatasetExportHTTPHandler(p, a.Resolver())
	if e != nil {
		t.Fatal(e)
	}
	r := datasetHTTPRequest()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("anonymous reached provider")
	}
	other := auth.NewAuthority()
	ctx, e := other.Binder().Bind(r.Context(), d)
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("foreign binder accepted")
	}
}
func TestDatasetHTTPRejectsPathsHeadersAndBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"query", func(r *http.Request) { r.URL.RawQuery = "member=x" }, 400}, {"force query", func(r *http.Request) { r.URL.ForceQuery = true }, 400}, {"slash", func(r *http.Request) { r.URL.Path += "/" }, 400}, {"dot redirect", func(r *http.Request) { r.URL.Path = "/api/v1/./dataset-exports" }, 400}, {"encoded path", func(r *http.Request) { r.URL.RawPath = "/api/v1/%64ataset-exports" }, 400},
		{"host", func(r *http.Request) { r.Host = "evil.test" }, 421}, {"origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, 403}, {"duplicate origins", func(r *http.Request) {
			r.Header["Origin"] = []string{"http://example.test"}
			r.Header["origin"] = []string{"http://evil.test"}
		}, 403}, {"fetch site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403}, {"encoding", func(r *http.Request) { r.Header["content-encoding"] = []string{"identity"} }, 400}, {"content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 400}, {"GET", func(r *http.Request) { r.Method = "GET" }, 405}, {"HEAD", func(r *http.Request) { r.Method = "HEAD" }, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p, _, _ := datasetHTTPFixture(t)
			r := datasetHTTPRequest()
			tc.change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || p.calls != 0 {
				t.Fatalf("code=%d calls=%d body=%s", w.Code, p.calls, w.Body.String())
			}
		})
	}
	for _, body := range []string{`null`, `{}`, `{"cohort_id":null}`, `{"cohort_id":"Y29ob3J0","cohort_id":"Y29ob3J0"}`, `{"Cohort_ID":"Y29ob3J0"}`, `{"cohort_id":"Y29ob3J0","grants":[]}`, `{"cohort_id":"Y29ob3J0"} {}`, `{"cohort_id":"bad="}`, strings.Repeat(" ", 4097), "{\"cohort_id\":\"\xff\"}"} {
		h, p, _, _ := datasetHTTPFixture(t)
		r := datasetHTTPRequest()
		r.Body = ioNopString(body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || p.calls != 0 {
			t.Fatalf("accepted body %q: %d", body, w.Code)
		}
	}
}
func TestDatasetHTTPProviderErrorsNeverCommitUncertainty(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{auth.ObjectNotFound(), 404}, {errors.New("private secret"), 503}, {decisionapi.ErrIndeterminate, 503}, {shoal.NewError(shoal.ErrorInvalidArgument, "private"), 400}} {
		h, p, _, _ := datasetHTTPFixture(t)
		p.err = tc.err
		w := httptest.NewRecorder()
		h.ServeHTTP(w, datasetHTTPRequest())
		if w.Code != tc.status || w.Header().Get(CommitOutcomeHeader) != "" || strings.Contains(w.Body.String(), "indeterminate") || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String(), w.Header())
		}
	}
	for _, change := range []func(*datasetHTTPStub){func(p *datasetHTTPStub) { p.reply = datasetHTTPReply("other") }, func(p *datasetHTTPStub) { p.reply.ManifestSHA256 = strings.Repeat("a", 64) }} {
		h, p, _, _ := datasetHTTPFixture(t)
		change(p)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, datasetHTTPRequest())
		if w.Code != 503 || strings.Contains(w.Body.String(), "dataset_bytes") {
			t.Fatal("invalid provider result disclosed")
		}
	}
}
func TestDatasetHTTPRevocationAndCancellationAfterProvider(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		_, p, _, d := datasetHTTPFixture(t)
		revoked := false
		resolver, e := auth.NewHostResolver(func(context.Context) (auth.Decision, error) {
			if revoked {
				return auth.Decision{}, auth.ObjectNotFound()
			}
			return d, nil
		})
		if e != nil {
			t.Fatal(e)
		}
		handler, e := NewDatasetExportHTTPHandler(p, resolver)
		if e != nil {
			t.Fatal(e)
		}
		r := datasetHTTPRequest()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		p.during = func() {
			if cancelled {
				cancel()
			} else {
				revoked = true
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		if w.Code != 401 || p.calls != 1 || strings.Contains(w.Body.String(), "dataset_bytes") || w.Header().Get(CommitOutcomeHeader) != "" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestDatasetHTTPFinalResolverCheckAndWorkspaceOutputLimit(t *testing.T) {
	_, p, _, d := datasetHTTPFixture(t)
	calls := 0
	resolver, e := auth.NewHostResolver(func(context.Context) (auth.Decision, error) {
		calls++
		if calls == 3 {
			return auth.Decision{}, auth.ObjectNotFound()
		}
		return d, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	handler, e := NewDatasetExportHTTPHandler(p, resolver)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, datasetHTTPRequest())
	if calls != 3 || w.Code != 401 || strings.Contains(w.Body.String(), "manifest_sha256") {
		t.Fatal("final encoding authorization check omitted")
	}
	_, p, a, d := datasetHTTPFixture(t)
	handler, e = NewDatasetExportHTTPHandler(p, a.Resolver())
	if e != nil {
		t.Fatal(e)
	}
	r := datasetHTTPRequest()
	ctx, e := a.Binder().Bind(r.Context(), d)
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: 128, indeterminateOnOverflow: false}, r.WithContext(ctx))
	if w.Code != 503 || w.Header().Get(CommitOutcomeHeader) != "" || strings.Contains(w.Body.String(), "manifest_sha256") {
		t.Fatal("workspace limit disclosed or implied commit", w.Code, w.Body.String())
	}
}
