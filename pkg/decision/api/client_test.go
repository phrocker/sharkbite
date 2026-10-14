// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixture() Response {
	now := time.Date(2026, 10, 8, 0, 0, 0, 123, time.UTC)
	return Response{Schema: 1, Receipt: Receipt{ID: "receipt:" + strings.Repeat("a", 64), Version: 1, State: "pending", RequestID: EncodeID("request"), TaskID: EncodeID("task"), PictureID: EncodeID("picture"), PredictorID: EncodeID("predictor"), CreatedAt: now, UpdatedAt: now, LeaseUntil: now.Add(time.Minute)}}
}
func clientFor(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c, e := NewClient(Config{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "secret", nil }})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestClientWireAndTimestamp(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Idempotency-Key") != EncodeKey([]byte{0, 255}) {
			t.Error("incorrect authentication or key")
		}
		if r.Method == "POST" {
			var request EvaluateRequest
			if e := json.NewDecoder(r.Body).Decode(&request); e != nil || request.RequestID != EncodeID("request") {
				t.Error("incorrect request")
			}
		}
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(fixture())
	}))
	defer s.Close()
	c := clientFor(t, s)
	for _, call := range []func(context.Context, shoal.ID, []byte) (Response, error){c.Evaluate, c.Read} {
		r, e := call(context.Background(), "request", []byte{0, 255})
		if e != nil {
			t.Fatal(e)
		}
		if !r.Receipt.CreatedAt.Equal(fixture().Receipt.CreatedAt) {
			t.Fatal("timestamp changed")
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
func TestClientRejectsSubstitutionAndAmbiguousJSON(t *testing.T) {
	raw, _ := json.Marshal(fixture())
	for _, body := range []string{strings.Replace(string(raw), EncodeID("request"), EncodeID("other"), 1), strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1), string(raw) + ` {}`, strings.Replace(string(raw), `"schema":1`, `"unknown":1,"schema":1`, 1), strings.Repeat(" ", MaxResponseBytes+1)} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202); io.WriteString(w, body) }))
		_, e := clientFor(t, s).Evaluate(context.Background(), "request", []byte("key"))
		s.Close()
		if !errors.Is(e, ErrIndeterminate) {
			t.Fatalf("missing uncertainty: %v", e)
		}
	}
}
func TestClientRefusesRedirectAndNeverRetriesPost(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++ }))
	defer destination.Close()
	calls := 0
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Redirect(w, r, destination.URL, 307) }))
	defer source.Close()
	_, e := clientFor(t, source).Evaluate(context.Background(), "request", []byte("key"))
	if e == nil || destinationCalls != 0 || calls != 1 {
		t.Fatalf("redirect followed: %v %d %d", e, calls, destinationCalls)
	}
}
func TestTransportLossAndCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, _ := w.(http.Hijacker).Hijack()
		connection.Close()
	}))
	defer s.Close()
	c := clientFor(t, s)
	_, e := c.Evaluate(context.Background(), "request", []byte("key"))
	if !errors.Is(e, ErrIndeterminate) {
		t.Fatalf("%v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = c.Evaluate(ctx, "request", []byte("key"))
	if !errors.Is(e, context.Canceled) || errors.Is(e, ErrIndeterminate) {
		t.Fatalf("preflight cancellation: %v", e)
	}
}
func TestServerErrorUncertainty(t *testing.T) {
	for _, test := range []struct {
		status int
		flag   bool
		want   bool
	}{{401, false, false}, {503, false, true}, {409, true, true}} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(test.status)
			json.NewEncoder(w).Encode(ErrorResponse{Code: "unavailable", Message: "unavailable", Indeterminate: test.flag})
		}))
		_, e := clientFor(t, s).Evaluate(context.Background(), "request", []byte("key"))
		s.Close()
		if errors.Is(e, ErrIndeterminate) != test.want {
			t.Fatalf("%+v: %v", test, e)
		}
	}
}
func TestCanonicalKeyAndURLValidation(t *testing.T) {
	for _, key := range []string{"", "YQ==", "YR", strings.Repeat("a", 1500)} {
		if _, e := DecodeKey(key); e == nil {
			t.Fatalf("accepted %q", key)
		}
	}
	for _, base := range []string{"ftp://localhost", "https://user:secret@host", "https://host?x=1", "https://host/prefix", "https://host/#fragment"} {
		if _, e := NewClient(Config{BaseURL: base, Token: func(context.Context) (string, error) { return "x", nil }}); e == nil {
			t.Fatalf("accepted %q", base)
		}
	}
}

func TestStrictResponseRejectsLossyUnicode(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"message":"\ud800"}`), []byte(`{"message":"\udc00"}`), {'"', 0xff, '"'}} {
		var v any
		if err := decodeStrict(raw, &v); err == nil {
			t.Fatalf("accepted lossy Unicode %q", raw)
		}
	}
	var v any
	if err := decodeStrict([]byte(`{"message":"\ud83d\ude00"}`), &v); err != nil {
		t.Fatal(err)
	}
}

func TestExactRequiredJSONShape(t *testing.T) {
	r := fixture()
	r.Receipt.State = "committed"
	r.Receipt.PredictionID = EncodeID("prediction")
	r.Receipt.Result = &Result{RequestID: r.Receipt.RequestID, PredictorID: r.Receipt.PredictorID, EffectiveDevice: "cpu", Status: "completed", CompletedAt: r.Receipt.UpdatedAt, Answers: []Answer{{SubjectID: EncodeID("subject"), QuestionID: EncodeID("question"), Status: "answered", Label: "retain", Distribution: []LabelProbability{{Label: "retain", Probability: 1}}}}}
	raw, _ := json.Marshal(r)
	if _, err := DecodeResponse(raw, "request"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"schema":1`, `"Schema":1`, 1),
		strings.Replace(string(raw), `"probability":1`, `"probability":null`, 1),
		strings.Replace(string(raw), `"probability":1`, `"Probability":1`, 1),
		strings.Replace(string(raw), `"probability":1`, `"unused":1`, 1),
		strings.Replace(string(raw), `"version":1,`, ``, 1),
		strings.Replace(string(raw), `"version":1`, `"version":null`, 1),
		strings.Replace(string(raw), `"created_at":`, `"Created_At":`, 1),
		strings.Replace(string(raw), `"created_at":"2026-10-08T00:00:00.000000123Z",`, ``, 1),
	} {
		if _, err := DecodeResponse([]byte(bad), "request"); err == nil {
			t.Fatalf("accepted malformed response %s", bad)
		}
	}
	for _, bad := range []string{`{"code":"unauthorized","message":"denied","indeterminate":null}`, `{"Code":"unauthorized","message":"denied"}`} {
		var detail ErrorResponse
		if err := decodeStrict([]byte(bad), &detail); err == nil {
			t.Fatal("accepted malformed error")
		}
	}
	var detail ErrorResponse
	if err := decodeStrict([]byte(`{"code":"unauthorized","message":"denied"}`), &detail); err != nil {
		t.Fatal(err)
	}
}
