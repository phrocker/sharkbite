// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func outcomeFixture() OutcomeReceipt {
	f := false
	t := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	return OutcomeReceipt{ID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: shoal.ID("decision:outcome:v1:" + strings.Repeat("b", 64)), Observation: OutcomeObservation{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", Kind: "correctness", QuestionID: "question", Truth: &f, EvidenceIDs: []shoal.ID{"z", "a"}, ObservedAt: t, AssertedProvenance: OutcomeProvenance{ReporterID: "asserted-reporter"}}, SubmitterID: shoal.ID(string([]byte{255, 0, 1})), ActorID: shoal.ID(string([]byte{254, 0})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{253, 0}))}, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("c", 64), ReceivedAt: t.Add(time.Second), State: "proposed"}
}
func TestOutcomeCodecOpaqueAndCanonical(t *testing.T) {
	r := outcomeFixture()
	raw, e := EncodeOutcomeReceipt(r)
	if e != nil {
		t.Fatal(e)
	}
	got, e := DecodeOutcomeReceipt(raw)
	if e != nil {
		t.Fatal(e)
	}
	if got.SubmitterID != r.SubmitterID || got.ActorID != r.ActorID || !reflect.DeepEqual(got.OnBehalfOf, r.OnBehalfOf) || got.Observation.Truth == nil || *got.Observation.Truth || got.Observation.EvidenceIDs[0] != "a" {
		t.Fatalf("lost bytes or false: %#v", got)
	}
	if r.Observation.EvidenceIDs[0] != "z" {
		t.Fatal("mutated input")
	}
	normalized, e := NormalizeOutcomeObservation(r.Observation)
	if e != nil {
		t.Fatal(e)
	}
	*normalized.Truth = true
	normalized.EvidenceIDs[0] = "changed"
	if *r.Observation.Truth || r.Observation.EvidenceIDs[0] != "z" {
		t.Fatal("aliased input")
	}
	request, e := EncodeOutcomeRequest(r.Observation)
	if e != nil {
		t.Fatal(e)
	}
	o, e := DecodeOutcomeRequest(request)
	if e != nil || !MatchOutcomeObservation(o, r.Observation) {
		t.Fatal(e)
	}
	offset := r.Observation
	offset.ObservedAt = offset.ObservedAt.In(time.FixedZone("offset", 3600))
	if !MatchOutcomeObservation(offset, r.Observation) {
		t.Fatal("timezone mismatch")
	}
	execution := r.Observation
	execution.Kind = "execution"
	execution.QuestionID = ""
	execution.Truth = nil
	execution.ActionID = "action"
	execution.ExecutionStatus = "unknown"
	if _, e := EncodeOutcomeRequest(execution); e != nil {
		t.Fatal(e)
	}
}
func TestOutcomeCodecRejectsAmbiguousWire(t *testing.T) {
	r := outcomeFixture()
	raw, _ := EncodeOutcomeRequest(r.Observation)
	for name, b := range map[string]string{
		"duplicate":    strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1),
		"case":         strings.Replace(string(raw), `"schema"`, `"Schema"`, 1),
		"unknown":      strings.Replace(string(raw), `"schema":1`, `"schema":1,"training_allowed":true`, 1),
		"null":         strings.Replace(string(raw), `"truth":false`, `"truth":null`, 1),
		"idpadding":    strings.Replace(string(raw), EncodeID("request"), EncodeID("request")+"=", 1),
		"missing":      strings.Replace(string(raw), `"truth":false,`, "", 1),
		"futureSchema": strings.Replace(string(raw), `"schema":1`, `"schema":2`, 1),
		"trailing":     string(raw) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := DecodeOutcomeRequest([]byte(b)); e == nil {
				t.Fatal("accepted")
			}
		})
	}
	r.Observation.EvidenceIDs = []shoal.ID{"same", "same"}
	if _, e := EncodeOutcomeRequest(r.Observation); e == nil {
		t.Fatal("duplicate evidence")
	}
	r = outcomeFixture()
	r.Observation.Label = "yes"
	if _, e := EncodeOutcomeRequest(r.Observation); e == nil {
		t.Fatal("label and truth")
	}
	r = outcomeFixture()
	r.OnBehalfOf = []shoal.ID{""}
	if _, e := EncodeOutcomeReceipt(r); e == nil {
		t.Fatal("empty delegate")
	}
	r = outcomeFixture()
	r.ReceivedAt = r.Observation.ObservedAt.Add(-time.Second)
	if _, e := EncodeOutcomeReceipt(r); e == nil {
		t.Fatal("future observation")
	}
}

type outcomeTransport func(*http.Request) (*http.Response, error)

func (f outcomeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func outcomeClient(t *testing.T, fn outcomeTransport) *Client {
	t.Helper()
	c, e := NewClient(Config{BaseURL: "https://example.test", HTTPClient: &http.Client{Transport: fn}, Token: func(context.Context) (string, error) { return "token", nil }})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func outcomeHTTPResponse(code int, b []byte) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b)))}
}
func TestOutcomeClientBindingAndUncertainty(t *testing.T) {
	original := outcomeFixture()
	key := []byte{0, 255, 1}
	calls := 0
	c := outcomeClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Idempotency-Key") != EncodeKey(key) || req.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("headers")
		}
		if req.Method == http.MethodPost {
			if req.URL.Path != "/api/v1/outcomes" || req.GetBody != nil {
				t.Fatal("path/replay")
			}
			b, _ := io.ReadAll(req.Body)
			o, e := DecodeOutcomeRequest(b)
			if e != nil || !MatchOutcomeObservation(o, original.Observation) {
				t.Fatal("body")
			}
		} else if req.URL.Path != "/api/v1/outcomes/"+EncodeID("request")+"/"+EncodeID("prediction") {
			t.Fatal("read path")
		}
		b, _ := EncodeOutcomeReceipt(original)
		return outcomeHTTPResponse(200, b), nil
	})
	if _, e := c.AppendOutcome(context.Background(), original.Observation, key); e != nil {
		t.Fatal(e)
	}
	if _, e := c.ReadOutcome(context.Background(), "request", "prediction", key); e != nil {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	for _, kind := range []string{"transport", "malformed", "substituted", "error"} {
		t.Run(kind, func(t *testing.T) {
			c := outcomeClient(t, func(*http.Request) (*http.Response, error) {
				switch kind {
				case "transport":
					return nil, errors.New("lost")
				case "malformed":
					return outcomeHTTPResponse(200, []byte(`{}`)), nil
				case "error":
					return outcomeHTTPResponse(503, []byte(`{"code":"unavailable","message":"retry","indeterminate":true}`)), nil
				default:
					r := original
					r.Observation.SubjectID = "other"
					b, _ := EncodeOutcomeReceipt(r)
					return outcomeHTTPResponse(200, b), nil
				}
			})
			if _, e := c.AppendOutcome(context.Background(), original.Observation, key); !errors.Is(e, ErrIndeterminate) {
				t.Fatalf("write not uncertain: %v", e)
			}
			if _, e := c.ReadOutcome(context.Background(), "request", "prediction", key); errors.Is(e, ErrIndeterminate) {
				t.Fatalf("GET uncertain: %v", e)
			}
		})
	}
}

func TestOutcomeBoundsAndRejectedResponses(t *testing.T) {
	r := outcomeFixture()
	r.Observation.EvidenceIDs = make([]shoal.ID, 1024)
	for i := range r.Observation.EvidenceIDs {
		r.Observation.EvidenceIDs[i] = shoal.ID(strings.Repeat("x", 1018) + fmt.Sprintf("%06d", i))
	}
	raw, e := EncodeOutcomeRequest(r.Observation)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = DecodeOutcomeRequest(raw); e != nil {
		t.Fatal(e)
	}
	r.Observation.EvidenceIDs = append(r.Observation.EvidenceIDs, "extra")
	if _, e = EncodeOutcomeRequest(r.Observation); e == nil {
		t.Fatal("excess evidence")
	}
	if _, e = DecodeOutcomeRequest(make([]byte, MaxOutcomeRequestBytes+1)); e == nil {
		t.Fatal("oversize request")
	}
	if _, e = DecodeOutcomeReceipt(make([]byte, MaxOutcomeResponseBytes+1)); e == nil {
		t.Fatal("oversize response")
	}
	for _, status := range []int{301, 302, 307, 308, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			c := outcomeClient(t, func(*http.Request) (*http.Response, error) {
				calls++
				return outcomeHTTPResponse(status, []byte(`{"code":"unavailable","message":"no"}`)), nil
			})
			_, e := c.AppendOutcome(context.Background(), outcomeFixture().Observation, []byte("key"))
			if !errors.Is(e, ErrIndeterminate) {
				t.Fatalf("uncertain status %d: %v", status, e)
			}
			if calls != 1 {
				t.Fatal("retried")
			}
		})
	}
	c := outcomeClient(t, func(*http.Request) (*http.Response, error) {
		return outcomeHTTPResponse(400, []byte(`{"code":"invalid_argument","message":"bad input"}`)), nil
	})
	if _, e := c.AppendOutcome(context.Background(), outcomeFixture().Observation, []byte("key")); e == nil || errors.Is(e, ErrIndeterminate) {
		t.Fatalf("known prewrite failure: %v", e)
	}
}
