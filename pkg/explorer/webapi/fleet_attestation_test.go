// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type stubAttestationProvider struct {
	err  error
	seen []fleet.AttestationPresentation
}

func (p *stubAttestationProvider) Present(_ context.Context, presentation fleet.AttestationPresentation) (fleet.AttestationReceipt, error) {
	p.seen = append(p.seen, presentation)
	if p.err != nil {
		return fleet.AttestationReceipt{}, p.err
	}
	return fleet.AttestationReceipt{AttestationID: "exattest:1",
		ExpiresAt: time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC)}, nil
}

func TestAttestationLimitsMatchTheFleet(t *testing.T) {
	if attestationapi.MaxExecutorRefBytes != fleet.MaxExecutorRefBytes ||
		attestationapi.MaxReportBytes != fleet.MaxAttestationReportBytes ||
		attestationapi.MaxIdempotencyKeyBytes != shoal.MaxIDBytes {
		t.Fatal("pkg/attestation/api limits drifted from the fleet's")
	}
}

func TestFleetAttestationRoute(t *testing.T) {
	post := func(t *testing.T, provider *stubAttestationProvider, body string) (*httptest.ResponseRecorder, attestationapi.ErrorResponse) {
		t.Helper()
		mux := http.NewServeMux()
		mountFleetAttestation(mux, provider)
		request := httptest.NewRequest(http.MethodPost, attestationapi.Route, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		var detail attestationapi.ErrorResponse
		_ = json.Unmarshal(recorder.Body.Bytes(), &detail)
		return recorder, detail
	}
	wire, err := attestationapi.NewRequest("exec", []byte("key"), []byte(`{"verifier_id":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := json.Marshal(wire)

	t.Run("accepted", func(t *testing.T) {
		provider := &stubAttestationProvider{}
		recorder, _ := post(t, provider, string(valid))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
		}
		var receipt attestationapi.Receipt
		if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil || receipt.AttestationID != "exattest:1" {
			t.Fatalf("receipt %s, %v", recorder.Body, err)
		}
		if len(provider.seen) != 1 || string(provider.seen[0].IdempotencyKey) != "key" ||
			!bytes.Equal(provider.seen[0].Report, []byte(`{"verifier_id":"v"}`)) {
			t.Fatalf("presentation = %+v", provider.seen)
		}
	})
	t.Run("the body cannot name a principal", func(t *testing.T) {
		for _, field := range []string{"subject", "client_id", "authorization_domain", "principal"} {
			body := strings.Replace(string(valid), "{", `{"`+field+`":"mallory",`, 1)
			recorder, _ := post(t, &stubAttestationProvider{}, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s accepted: %d", field, recorder.Code)
			}
		}
	})
	t.Run("every refusal is one opaque answer", func(t *testing.T) {
		for _, refusal := range []error{
			&fleet.AttestationRefusal{Reason: "signature"},
			&fleet.AttestationRefusal{Reason: "image-unpinned"},
			shoal.WrapError(shoal.ErrorUnauthorized, "attestation refused", fleet.ErrAttestationRefused),
		} {
			recorder, detail := post(t, &stubAttestationProvider{err: refusal}, string(valid))
			if recorder.Code != http.StatusUnauthorized || detail.Code != shoal.ErrorUnauthorized ||
				!strings.HasSuffix(detail.Message, attestationapi.RefusedMessage) {
				t.Fatalf("refusal answered %d %+v", recorder.Code, detail)
			}
			for _, leak := range []string{"signature", "image", "sha256", "exattest"} {
				if strings.Contains(recorder.Body.String(), leak) {
					t.Fatalf("refusal body leaks %q: %s", leak, recorder.Body)
				}
			}
		}
	})
	t.Run("a store failure is unavailable and unmarked", func(t *testing.T) {
		recorder, detail := post(t, &stubAttestationProvider{
			err: shoal.WrapError(shoal.ErrorUnavailable, "executor attestation is unavailable", fleet.ErrAttestationUnavailable),
		}, string(valid))
		if recorder.Code != http.StatusServiceUnavailable || detail.Indeterminate ||
			recorder.Header().Get(CommitOutcomeHeader) != "" {
			t.Fatalf("store failure answered %d %+v", recorder.Code, detail)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		recorder, _ := post(t, &stubAttestationProvider{}, `{"executor_ref":"exec","idempotency_key":"","report":"e30"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status %d", recorder.Code)
		}
	})
}
