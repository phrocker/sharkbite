package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func layaTestIdentity() TypedIdentity {
	return TypedIdentity{Provider: "laya", ModelAlias: "english", Revision: "r1", WeightsDigest: "w", TokenizerDigest: "t", CriteriaID: "c", PreprocessingID: "p", CalibrationID: "u", RuntimeID: "r", Device: "cpu", DistributionTolerance: 0.01}
}

func layaTestRequest() TypedRequest {
	return TypedRequest{ModelAlias: "english", QuestionSetID: "set-v1", State: []byte(`{"sample":"x"}`), Questions: []TypedQuestion{{ID: "risk", Kind: TypedChoice, Instructions: "Classify risk", Options: []string{"low", "high"}, OptionDescriptions: map[string]string{"low": "routine", "high": "requires review"}}, {ID: "score", Kind: TypedOrdinalScore, Options: []string{"routine", "urgent"}}, {ID: "safe", Kind: TypedPropositionProbability}}}
}

func TestLayaPredictorMapsBoundedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var request struct {
			Model     string `json:"model"`
			Questions map[string]struct {
				Instructions string `json:"instructions"`
				Criteria     any    `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "english" || len(request.Questions) != 3 {
			t.Fatalf("request = %+v", request)
		}
		if request.Questions["risk"].Instructions != "Classify risk" {
			t.Fatalf("instructions = %+v", request.Questions["risk"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"risk":{"type":"choice","choice":"high","confidence":0.8,"answer_confidence":0.8,"probabilities":{"low":0.2,"high":0.8}},"score":{"type":"score","score":1,"confidence":0.8,"answer_confidence":0.8,"probabilities":{"0":0.2,"1":0.8}},"safe":{"type":"noul","noul":0.1,"confidence":0.1,"answer_confidence":0.1}}}`))
	}))
	defer server.Close()
	p, err := NewLayaPredictor(LayaConfig{BaseURL: server.URL, BearerToken: "token", Identity: layaTestIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := PredictTyped(context.Background(), p, layaTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Answers[0].Choice != "high" || result.Answers[1].Score != 1 || result.Answers[2].PTrue != 0.1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestLayaPredictorFailsClosedOnMalformedOrOversizedResponse(t *testing.T) {
	for _, body := range []string{`{"answers":{}}`, `{"answers":{"risk":{}}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		p, err := NewLayaPredictor(LayaConfig{BaseURL: server.URL, BearerToken: "token", Identity: layaTestIdentity(), MaxResponseBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		_, err = PredictTyped(context.Background(), p, layaTestRequest())
		if !errors.Is(err, ErrMalformedResponse) && !errors.Is(err, ErrOversizedResponse) {
			t.Fatalf("body %s: error = %v", body, err)
		}
		server.Close()
	}
}

func TestLayaPredictorCheckHealthIsAuthenticatedAndBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("health request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	p, err := NewLayaPredictor(LayaConfig{BaseURL: server.URL, BearerToken: "token", Identity: layaTestIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckHealth(context.Background()); err != nil {
		t.Fatal(err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "starting", http.StatusServiceUnavailable) }))
	defer bad.Close()
	p, err = NewLayaPredictor(LayaConfig{BaseURL: bad.URL, BearerToken: "token", Identity: layaTestIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckHealth(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("health error = %v", err)
	}
}
