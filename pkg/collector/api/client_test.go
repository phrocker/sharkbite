// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func token(context.Context) (string, error) { return "t", nil }

func ref() collector.ArtifactRef {
	return collector.ArtifactRef{ID: "a", Digest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", MediaType: "text/plain", ObservedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func serve(t *testing.T, status int, body string, header http.Header) (*Client, *int) {
	t.Helper()
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if _, e := r.Cookie("session"); e == nil {
			t.Error("cookie sent")
		}
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	jar, _ := cookiejar.New(nil)
	c, e := NewClient(Config{BaseURL: s.URL, HTTPClient: &http.Client{Jar: jar}, Token: token})
	if e != nil {
		t.Fatal(e)
	}
	return c, &calls
}

func TestClientErrorClassification(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t, http.StatusForbidden, `{"code":"permission_denied","message":"collector permission denied"}`, nil)
	_, e := c.SubmitArtifact(ctx, "c", ref())
	if !errors.Is(e, ErrPermissionDenied) || errors.Is(e, ErrIndeterminate) {
		t.Fatalf("403: %v", e)
	}
	c, _ = serve(t, http.StatusServiceUnavailable, `{"code":"unavailable","message":"x"}`, nil)
	if _, e = c.SubmitArtifact(ctx, "c", ref()); !errors.Is(e, ErrIndeterminate) {
		t.Fatalf("POST 5xx must be indeterminate: %v", e)
	}
	if _, e = c.ReadObservation(ctx, shoal.ID("observation:"+ref().Digest)); errors.Is(e, ErrIndeterminate) {
		t.Fatalf("GET failure marked indeterminate: %v", e)
	}
	c, _ = serve(t, http.StatusOK, `{"schema":1,"collector_id":"Yw","artifact_id":"YQ","generation":1,"received_at":"2026-01-01T00:00:00Z","extra":1}`, nil)
	if _, e = c.SubmitArtifact(ctx, "c", ref()); !errors.Is(e, ErrIndeterminate) {
		t.Fatalf("unknown response field: %v", e)
	}
	c, _ = serve(t, http.StatusOK, `{"schema":1,"collector_id":"eA","artifact_id":"YQ","generation":1,"received_at":"2026-01-01T00:00:00Z"}`, nil)
	if _, e = c.SubmitArtifact(ctx, "c", ref()); e == nil {
		t.Fatal("mismatched receipt identity accepted")
	}
	c, calls := serve(t, http.StatusFound, ``, http.Header{"Location": {"http://elsewhere.invalid/"}})
	if _, e = c.SubmitArtifact(ctx, "c", ref()); e == nil || *calls != 1 {
		t.Fatalf("redirect followed or accepted: %v calls=%d", e, *calls)
	}
	c, _ = serve(t, http.StatusOK, `{"schema":1,"collector_id":"Yw","artifact_id":"YQ","generation":1,"received_at":"2026-01-01T00:00:00Z"}`, nil)
	if got, e := c.SubmitArtifact(ctx, "c", ref()); e != nil || got.Generation != 1 {
		t.Fatalf("valid receipt: %+v %v", got, e)
	}
}

func TestClientConfigurationAndWireRoundTrip(t *testing.T) {
	for _, base := range []string{"", "ftp://h", "http://u@h", "http://h/p", "http://h?q"} {
		if _, e := NewClient(Config{BaseURL: base, Token: token}); e == nil {
			t.Fatal(base, "accepted")
		}
	}
	if _, e := NewClient(Config{BaseURL: "http://h"}); e == nil {
		t.Fatal("missing token accepted")
	}
	value := 0.25
	o, e := collector.NewObservation(collector.ObservationConfig{CollectorID: "c\x00/世界", ArtifactID: "a", Extractor: collector.ExtractorRef{ID: "x", Version: "1"}, Confidence: collector.Confidence{Disposition: collector.LowConfidence, Value: &value}, Kind: "k", Payload: []byte{0, 255}, ObservedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if e != nil {
		t.Fatal(e)
	}
	back, e := DecodeObservation(EncodeObservation(o))
	if e != nil || back.ID() != o.ID() {
		t.Fatalf("round trip: %v", e)
	}
	r := collector.EnrollRequest{CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"a"}, Extractors: []collector.ExtractorRef{{ID: "x", Version: "1"}}, Attestation: &collector.AttestationReport{Kind: "k", Format: "f", Evidence: []byte{1}}}
	got, e := DecodeEnroll(EncodeEnroll(r))
	want, _ := r.Digest()
	if d, _ := got.Digest(); e != nil || d != want {
		t.Fatalf("enroll round trip: %v", e)
	}
}
