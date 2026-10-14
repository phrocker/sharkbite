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

func adjudicationTestID(prefix string, n int) shoal.ID {
	return shoal.ID(prefix + fmt.Sprintf("%064x", n))
}
func adjudicationFixture() AdjudicationReceipt {
	f := false
	return AdjudicationReceipt{ID: adjudicationTestID("adjudication-receipt:", 1), BasisID: adjudicationTestID("decision:adjudication-basis:v1:", 2), Version: 1, TargetID: adjudicationTestID("decision:adjudication-target:v1:", 3), TaskID: "task", PictureID: "picture", PolicyID: adjudicationTestID("decision:label-policy:v1:", 4), ProposalID: adjudicationTestID("decision:adjudication-proposal:v1:", 5), Proposal: AdjudicationProposal{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", QuestionID: "question", ObservationReceiptIDs: []shoal.ID{adjudicationTestID("outcome-receipt:", 2), adjudicationTestID("outcome-receipt:", 1)}, WitnessIDs: []shoal.ID{"z", "a"}, Disposition: "verified", Truth: &f}, Adjudicator: AdjudicationAttribution{SubjectID: shoal.ID(string([]byte{255, 0})), ActorID: shoal.ID(string([]byte{254, 0})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{253, 0}))}, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("a", 64)}, ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)}
}
func adjudicationHistoryFixture() AdjudicationHistory {
	r := adjudicationFixture()
	s := r
	s.ID = adjudicationTestID("adjudication-receipt:", 6)
	s.Version = 2
	s.Proposal.ExpectedHeadID = r.ID
	s.Proposal.ExpectedVersion = 1
	s.Proposal.RequestID = "second-request"
	s.Proposal.PredictionID = "second-prediction"
	s.ReceivedAt = r.ReceivedAt.Add(time.Second)
	return AdjudicationHistory{TargetID: r.TargetID, Receipts: []AdjudicationReceipt{r, s}}
}
func TestAdjudicationCodecCanonicalAndOpaque(t *testing.T) {
	r := adjudicationFixture()
	raw, e := EncodeAdjudicationReceipt(r)
	if e != nil {
		t.Fatal(e)
	}
	got, e := DecodeAdjudicationReceipt(raw)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(r.Adjudicator, got.Adjudicator) || got.Proposal.Truth == nil || *got.Proposal.Truth || got.Proposal.WitnessIDs[0] != "a" || r.Proposal.WitnessIDs[0] != "z" {
		t.Fatal("lost bytes, false or mutated input")
	}
	n, e := NormalizeAdjudicationProposal(r.Proposal)
	if e != nil {
		t.Fatal(e)
	}
	*n.Truth = true
	n.WitnessIDs[0] = "change"
	n.ObservationReceiptIDs[0] = "change"
	if *r.Proposal.Truth || r.Proposal.WitnessIDs[0] != "z" || !outcomeReceiptID(r.Proposal.ObservationReceiptIDs[0]) {
		t.Fatal("alias")
	}
	b, e := EncodeAdjudicationRequest(r.Proposal)
	if e != nil {
		t.Fatal(e)
	}
	p, e := DecodeAdjudicationRequest(b)
	if e != nil || !MatchAdjudicationProposal(p, r.Proposal) {
		t.Fatal(e)
	}
	h := adjudicationHistoryFixture()
	b, e = EncodeAdjudicationHistory(h)
	if e != nil {
		t.Fatal(e)
	}
	h2, e := DecodeAdjudicationHistory(b)
	if e != nil || len(h2.Receipts) != 2 || h2.Receipts[1].Proposal.RequestID != "second-request" {
		t.Fatal(e)
	}
}
func TestAdjudicationRejectMalformed(t *testing.T) {
	r := adjudicationFixture()
	raw, _ := EncodeAdjudicationRequest(r.Proposal)
	for name, s := range map[string]string{"duplicate": strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1), "case": strings.Replace(string(raw), `"schema"`, `"Schema"`, 1), "grant": strings.Replace(string(raw), `"schema":1`, `"schema":1,"basis_id":"claimed"`, 1), "null": strings.Replace(string(raw), `"truth":false`, `"truth":null`, 1), "missing": strings.Replace(string(raw), `"truth":false,`, "", 1), "base64": strings.Replace(string(raw), EncodeID("request"), EncodeID("request")+"=", 1), "trailing": string(raw) + "{}"} {
		t.Run(name, func(t *testing.T) {
			got, e := DecodeAdjudicationRequest([]byte(s))
			if e == nil || !reflect.DeepEqual(got, AdjudicationProposal{}) {
				t.Fatalf("accepted or partial: %v", e)
			}
		})
	}
	for name, mutate := range map[string]func(*AdjudicationProposal){"duplicate": func(p *AdjudicationProposal) { p.WitnessIDs = []shoal.ID{"same", "same"} }, "no witness": func(p *AdjudicationProposal) { p.WitnessIDs = nil }, "mixed": func(p *AdjudicationProposal) { p.Label = "yes" }, "disputed truth": func(p *AdjudicationProposal) { p.Disposition = "disputed"; p.Reason = "why" }, "headless version": func(p *AdjudicationProposal) { p.ExpectedVersion = 1 }, "too many witnesses": func(p *AdjudicationProposal) { p.WitnessIDs = make([]shoal.ID, 1025) }} {
		t.Run(name, func(t *testing.T) {
			p := adjudicationFixture().Proposal
			mutate(&p)
			if _, e := EncodeAdjudicationRequest(p); e == nil {
				t.Fatal("accepted")
			}
		})
	}
	p := r.Proposal
	p.Disposition = "unresolved"
	p.Reason = "unknown"
	p.Truth = nil
	p.WitnessIDs = nil
	if _, e := EncodeAdjudicationRequest(p); e != nil {
		t.Fatal(e)
	}
}
func TestAdjudicationHistoryChain(t *testing.T) {
	for name, mutate := range map[string]func(*AdjudicationHistory){"gap": func(h *AdjudicationHistory) { h.Receipts[1].Version = 3; h.Receipts[1].Proposal.ExpectedVersion = 2 }, "head": func(h *AdjudicationHistory) {
		h.Receipts[1].Proposal.ExpectedHeadID = adjudicationTestID("adjudication-receipt:", 99)
	}, "target": func(h *AdjudicationHistory) {
		h.Receipts[1].TargetID = adjudicationTestID("decision:adjudication-target:v1:", 99)
	}, "subject": func(h *AdjudicationHistory) { h.Receipts[1].Proposal.SubjectID = "other" }, "question": func(h *AdjudicationHistory) { h.Receipts[1].Proposal.QuestionID = "other" }, "policy": func(h *AdjudicationHistory) {
		h.Receipts[1].PolicyID = adjudicationTestID("decision:label-policy:v1:", 99)
	}, "time": func(h *AdjudicationHistory) { h.Receipts[1].ReceivedAt = h.Receipts[0].ReceivedAt.Add(-time.Second) }, "duplicateID": func(h *AdjudicationHistory) { h.Receipts[1].ID = h.Receipts[0].ID }, "empty": func(h *AdjudicationHistory) { h.Receipts = nil }, "count": func(h *AdjudicationHistory) { h.Receipts = make([]AdjudicationReceipt, 129) }} {
		t.Run(name, func(t *testing.T) {
			h := adjudicationHistoryFixture()
			mutate(&h)
			if _, e := EncodeAdjudicationHistory(h); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}
func TestAdjudicationBoundsPreflight(t *testing.T) {
	for field, count := range map[string]int{"receipts": 129, "observation_receipt_ids": 257, "witness_ids": 1025, "on_behalf_of": 65} {
		raw := []byte(`{"` + field + `":[` + strings.Repeat(`{},`, count-1) + `{}]}`)
		if preflightAdjudication(raw) == nil {
			t.Fatal(field)
		}
	}
	if _, e := DecodeAdjudicationRequest(make([]byte, MaxAdjudicationRequestBytes+1)); e == nil {
		t.Fatal("request limit")
	}
	if _, e := DecodeAdjudicationReceipt(make([]byte, MaxAdjudicationResponseBytes+1)); e == nil {
		t.Fatal("receipt limit")
	}
	if _, e := DecodeAdjudicationHistory(make([]byte, MaxAdjudicationHistoryBytes+1)); e == nil {
		t.Fatal("history limit")
	}
	p := adjudicationFixture().Proposal
	p.WitnessIDs = make([]shoal.ID, 1024)
	for i := range p.WitnessIDs {
		p.WitnessIDs[i] = shoal.ID(strings.Repeat("x", 1018) + fmt.Sprintf("%06d", i))
	}
	b, e := EncodeAdjudicationRequest(p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := DecodeAdjudicationRequest(b); e != nil {
		t.Fatal(e)
	}
}
func TestAdjudicationClientBindingsAndUncertainty(t *testing.T) {
	r := adjudicationFixture()
	h := adjudicationHistoryFixture()
	key := []byte{0, 255}
	calls := 0
	c := outcomeClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("auth")
		}
		if req.Method == http.MethodPost {
			if req.URL.Path != "/api/v1/adjudications" || req.GetBody != nil || req.Header.Get("Idempotency-Key") != EncodeKey(key) {
				t.Fatal("POST protocol")
			}
			b, _ := io.ReadAll(req.Body)
			p, e := DecodeAdjudicationRequest(b)
			if e != nil || !MatchAdjudicationProposal(p, r.Proposal) {
				t.Fatal(e)
			}
			b, _ = EncodeAdjudicationReceipt(r)
			return outcomeHTTPResponse(200, b), nil
		}
		if req.URL.Path != "/api/v1/adjudications/"+EncodeID(h.TargetID) || req.Header.Get("Idempotency-Key") != "" {
			t.Fatal("GET protocol")
		}
		b, _ := EncodeAdjudicationHistory(h)
		return outcomeHTTPResponse(200, b), nil
	})
	if _, e := c.Adjudicate(context.Background(), r.Proposal, key); e != nil {
		t.Fatal(e)
	}
	if _, e := c.AdjudicationHistory(context.Background(), h.TargetID); e != nil {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	for _, kind := range []string{"transport", "malformed", "substitution", "error", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			count := 0
			c := outcomeClient(t, func(*http.Request) (*http.Response, error) {
				count++
				switch kind {
				case "transport":
					return nil, errors.New("lost")
				case "malformed":
					return outcomeHTTPResponse(200, []byte(`{}`)), nil
				case "error":
					return outcomeHTTPResponse(503, []byte(`{"code":"unavailable","message":"retry","indeterminate":true}`)), nil
				case "redirect":
					return outcomeHTTPResponse(307, []byte(`{"code":"unavailable","message":"retry"}`)), nil
				default:
					r := adjudicationFixture()
					r.Proposal.SubjectID = "other"
					b, _ := EncodeAdjudicationReceipt(r)
					return outcomeHTTPResponse(200, b), nil
				}
			})
			if _, e := c.Adjudicate(context.Background(), r.Proposal, key); !errors.Is(e, ErrIndeterminate) {
				t.Fatalf("write certainty lost: %v", e)
			}
			if _, e := c.AdjudicationHistory(context.Background(), h.TargetID); errors.Is(e, ErrIndeterminate) {
				t.Fatal("GET uncertain")
			}
			if count != 2 {
				t.Fatal("retry")
			}
		})
	}
}

func TestAdjudicationClientRejectsWideErrorsAndKeys(t *testing.T) {
	r := adjudicationFixture()
	calls := 0
	c := outcomeClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		return outcomeHTTPResponse(503, []byte(`{"code":"unavailable","message":"`+strings.Repeat("x", 17000)+`","indeterminate":true}`)), nil
	})
	if _, e := c.Adjudicate(context.Background(), r.Proposal, make([]byte, shoal.MaxIDBytes+1)); e == nil || errors.Is(e, ErrIndeterminate) || calls != 0 {
		t.Fatalf("oversized key reached transport: %v", e)
	}
	if _, e := c.Adjudicate(context.Background(), r.Proposal, []byte("key")); !errors.Is(e, ErrIndeterminate) {
		t.Fatalf("write lost uncertainty: %v", e)
	}
	if _, e := c.AdjudicationHistory(context.Background(), r.TargetID); e == nil || errors.Is(e, ErrIndeterminate) {
		t.Fatalf("invalid read uncertainty: %v", e)
	}
}
