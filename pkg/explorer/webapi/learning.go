// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// LearningRoute is the collection root of the Learning console's routes.
const LearningRoute = "/api/v1/learning"

// LearningSurface is the complete host composition for the Learning
// console: the provider and the same resolver the authenticated transport
// uses. An incomplete surface mounts nothing.
type LearningSurface struct {
	Resolver auth.Resolver
	Provider decisionapi.LearningProvider
}

func (s LearningSurface) validate() error {
	if isAbsentInterface(s.Resolver) || isAbsentInterface(s.Provider) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "complete learning surface and trusted resolver required")
	}
	return nil
}

// MountLearningSurface mounts every learning route atomically behind the
// host's trusted authentication and origin gates, then advertises the
// decision_learning capability.
func (h *Handler) MountLearningSurface(surface LearningSurface) error {
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "learning surface requires authenticated transport")
	}
	if err := surface.validate(); err != nil {
		return err
	}
	handler := &learningHTTP{provider: surface.Provider, resolver: surface.Resolver}
	if err := h.MountAuthenticated(LearningRoute+"/", handler); err != nil {
		return err
	}
	h.decisionCapabilities.Learning = true
	return nil
}

type learningHTTP struct {
	provider decisionapi.LearningProvider
	resolver auth.Resolver
}

func (h *learningHTTP) ValidatePreAuthentication(r *http.Request) int {
	origins, sites := []string{}, []string{}
	for key, values := range r.Header {
		if strings.EqualFold(key, "Origin") {
			origins = append(origins, values...)
		}
		if strings.EqualFold(key, "Sec-Fetch-Site") {
			sites = append(sites, values...)
		}
	}
	if len(origins) > 1 || len(sites) > 1 {
		return http.StatusForbidden
	}
	if len(sites) == 1 && sites[0] != "same-origin" && sites[0] != "none" {
		return http.StatusForbidden
	}
	if len(origins) == 1 {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if origins[0] != scheme+"://"+r.Host {
			return http.StatusForbidden
		}
	}
	if !strings.HasPrefix(r.URL.Path, LearningRoute+"/") || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return http.StatusBadRequest
	}
	return 0
}

// learningWireError keeps the provider's message verbatim: a refusal reason
// is the record's reason and the console must show it unchanged.
func learningWireError(w http.ResponseWriter, e error) {
	code, status := shoal.ErrorUnavailable, http.StatusServiceUnavailable
	message := "learning service unavailable"
	switch {
	case shoal.IsErrorCode(e, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, e.Error()
	case shoal.IsErrorCode(e, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(e, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, e.Error()
	case shoal.IsErrorCode(e, shoal.ErrorConflict):
		code, status, message = shoal.ErrorConflict, http.StatusConflict, e.Error()
	case shoal.IsErrorCode(e, shoal.ErrorUnavailable):
		message = e.Error()
	case shoal.IsErrorCode(e, shoal.ErrorInternal):
		code, status, message = shoal.ErrorInternal, http.StatusInternalServerError, e.Error()
	}
	if se := new(shoal.Error); errors.As(e, &se) {
		message = se.Message
	}
	writeResponse(w, status, struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}{code, message})
}

func (h *learningHTTP) resolve(ctx context.Context) (auth.Fingerprint, auth.Decision, error) {
	if ctx.Err() != nil {
		return auth.Fingerprint{}, auth.Decision{}, authenticationDenied()
	}
	d, e := h.resolver.Resolve(ctx)
	if e != nil || ctx.Err() != nil || !time.Now().Before(d.AuthenticationExpires()) {
		return auth.Fingerprint{}, auth.Decision{}, authenticationDenied()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return auth.Fingerprint{}, auth.Decision{}, authenticationDenied()
	}
	return fp, d, nil
}

// learningWriteDenied is the answer to a write by a principal whose decision
// does not carry learning_write. It is a refusal of the operation, not a
// failure of authentication, so the status is 403.
func learningWriteDenied(w http.ResponseWriter) {
	writeResponse(w, http.StatusForbidden, struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}{shoal.ErrorUnauthorized, "learning writes require the " + string(auth.OperationLearningWrite) + " operation"})
}

func allowsLearningWrite(d auth.Decision) bool {
	for _, op := range d.AllowedOperations() {
		if op == auth.OperationLearningWrite {
			return true
		}
	}
	return false
}

func invalidLearningRequest(detail string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid learning request: "+detail)
}

// readLearningBody reads one bounded JSON object body.
func readLearningBody(r *http.Request) (json.RawMessage, error) {
	for key := range r.Header {
		if strings.EqualFold(key, "Content-Encoding") {
			return nil, invalidLearningRequest("content encoding")
		}
	}
	contentType, e := decisionHeader(r, "Content-Type")
	if e != nil {
		return nil, invalidLearningRequest("content type")
	}
	kind, params, e := mime.ParseMediaType(contentType)
	if e != nil || kind != "application/json" {
		return nil, invalidLearningRequest("content type")
	}
	for key, value := range params {
		if key != "charset" || !strings.EqualFold(value, "utf-8") {
			return nil, invalidLearningRequest("content type")
		}
	}
	if r.Body == nil {
		return nil, invalidLearningRequest("body")
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, decisionapi.MaxLearningRequestBytes+1))
	if e != nil || len(raw) > decisionapi.MaxLearningRequestBytes || !utf8.Valid(raw) {
		return nil, invalidLearningRequest("body size or encoding")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var object map[string]json.RawMessage
	if e := d.Decode(&object); e != nil || object == nil {
		return nil, invalidLearningRequest("body must be a JSON object")
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, invalidLearningRequest("trailing data")
	}
	return json.RawMessage(bytes.TrimSpace(raw)), nil
}

func learningField(body json.RawMessage, name string) string {
	var object map[string]any
	_ = json.Unmarshal(body, &object)
	s, _ := object[name].(string)
	return s
}

// dispatch maps one method and path to a provider call. Paths are exact;
// identifiers are single path segments.
func (h *learningHTTP) dispatch(ctx context.Context, method string, segments []string, body func() (json.RawMessage, error)) (json.RawMessage, error, bool) {
	get := method == http.MethodGet
	post := method == http.MethodPost
	switch {
	case len(segments) == 1 && segments[0] == "datasets" && get:
		out, err := h.provider.Datasets(ctx)
		return out, err, true
	case len(segments) == 2 && segments[0] == "datasets" && segments[1] == "exports" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.RequestExport(ctx, learningField(raw, "cohort_id"))
		return out, err, true
	case len(segments) == 1 && segments[0] == "jobs" && get:
		out, err := h.provider.Jobs(ctx)
		return out, err, true
	case len(segments) == 1 && segments[0] == "jobs" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.SubmitJob(ctx, raw)
		return out, err, true
	case len(segments) == 2 && segments[0] == "jobs" && get:
		out, err := h.provider.Job(ctx, segments[1])
		return out, err, true
	case len(segments) == 3 && segments[0] == "jobs" && segments[2] == "predictions" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.SubmitPredictions(ctx, segments[1], raw)
		return out, err, true
	case len(segments) == 3 && segments[0] == "jobs" && segments[2] == "policy" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.DeclarePolicy(ctx, segments[1], raw)
		return out, err, true
	case len(segments) == 3 && segments[0] == "jobs" && segments[2] == "receipt" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.SubmitReceipt(ctx, segments[1], raw)
		return out, err, true
	case len(segments) == 3 && segments[0] == "jobs" && segments[2] == "run" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.RequestRun(ctx, segments[1], raw)
		return out, err, true
	case len(segments) == 3 && segments[0] == "jobs" && segments[2] == "claim" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.ClaimRun(ctx, segments[1], raw)
		return out, err, true
	case len(segments) == 3 && segments[0] == "jobs" && segments[2] == "progress" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.ReportProgress(ctx, segments[1], raw)
		return out, err, true
	case len(segments) == 1 && segments[0] == "candidates" && get:
		out, err := h.provider.Candidates(ctx)
		return out, err, true
	case len(segments) == 1 && segments[0] == "candidates" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.SubmitCandidate(ctx, raw)
		return out, err, true
	case len(segments) == 2 && segments[0] == "candidates" && segments[1] == "seal" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.SealCandidate(ctx, raw)
		return out, err, true
	case len(segments) == 2 && segments[0] == "serving" && segments[1] == "predict" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.Predict(ctx, raw)
		return out, err, true
	case len(segments) == 3 && segments[0] == "candidates" && segments[2] == "evaluations" && get:
		out, err := h.provider.Evaluations(ctx, segments[1])
		return out, err, true
	case len(segments) == 1 && segments[0] == "evaluations" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.SubmitEvaluation(ctx, raw)
		return out, err, true
	case len(segments) == 1 && segments[0] == "releases" && get:
		out, err := h.provider.Releases(ctx)
		return out, err, true
	case len(segments) == 2 && segments[0] == "releases" && segments[1] == "promote" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.Promote(ctx, raw)
		return out, err, true
	case len(segments) == 2 && segments[0] == "releases" && segments[1] == "rollback" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.Rollback(ctx, raw)
		return out, err, true
	case len(segments) == 2 && segments[0] == "evidence" && get:
		out, err := h.provider.Evidence(ctx, segments[1])
		return out, err, true
	case len(segments) == 1 && segments[0] == "bundles" && get:
		out, err := h.provider.Bundles(ctx)
		return out, err, true
	case len(segments) == 2 && segments[0] == "bundles" && segments[1] == "export" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.ExportBundle(ctx, raw)
		return out, err, true
	case len(segments) == 2 && segments[0] == "bundles" && segments[1] == "import" && post:
		raw, err := body()
		if err != nil {
			return nil, err, true
		}
		out, err := h.provider.ImportBundle(ctx, raw)
		return out, err, true
	}
	return nil, nil, false
}

func (h *learningHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, decision, e := h.resolve(r.Context())
	if e != nil {
		learningWireError(w, e)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeResponse(w, http.StatusMethodNotAllowed, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{"invalid_argument", "method not allowed"})
		return
	}
	ctx := r.Context()
	if r.Method == http.MethodPost {
		// Every write needs the operation, and is bound to the actor the
		// trusted decision names; a body's "owner" can only agree with it.
		if !allowsLearningWrite(decision) {
			learningWriteDenied(w)
			return
		}
		ctx = decisionapi.WithLearningActor(ctx, decisionapi.LearningActor{Subject: string(decision.Subject()), Actor: string(decision.Actor())})
	}
	rest := strings.TrimPrefix(r.URL.Path, LearningRoute+"/")
	segments := strings.Split(rest, "/")
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			learningWireError(w, invalidLearningRequest("path"))
			return
		}
	}
	result, e, known := h.dispatch(ctx, r.Method, segments, func() (json.RawMessage, error) { return readLearningBody(r) })
	if !known {
		learningWireError(w, shoal.NewError(shoal.ErrorNotFound, "no such learning route"))
		return
	}
	after, _, authErr := h.resolve(r.Context())
	if authErr != nil || after != before {
		learningWireError(w, authenticationDenied())
		return
	}
	if e != nil {
		learningWireError(w, e)
		return
	}
	if len(result) == 0 || !json.Valid(result) {
		learningWireError(w, shoal.NewError(shoal.ErrorInternal, "the learning service produced an invalid response"))
		return
	}
	if uint64(len(result)) > responseLimitFor(w) {
		// A permanent size problem, not an outage: the console must not
		// describe it as the service being unavailable.
		learningWireError(w, shoal.NewError(shoal.ErrorInternal, "the learning response exceeds this host's response limit; narrow the listing"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}
