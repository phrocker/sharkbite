// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxOutcomeRequestBytes = 2 << 20
const MaxOutcomeResponseBytes = 4 << 20

// OutcomeObservation is an assertion about a registered prediction. It never
// grants evidence access, reporting permission, or verified label status.
// Identifiers in these public Go values are decoded, not base64 strings.
type OutcomeObservation struct {
	RequestID, PredictionID, SubjectID shoal.ID
	Kind                               string
	QuestionID                         shoal.ID
	Label                              string
	Truth                              *bool
	ExecutionStatus                    string
	ActionID                           shoal.ID
	EvidenceIDs                        []shoal.ID
	ObservedAt                         time.Time
	AssertedProvenance                 OutcomeProvenance
	Supersedes                         shoal.ID
}
type OutcomeProvenance struct{ ReporterID, ModelID, PromptID, ToolID shoal.ID }

// OutcomeReceipt retains original attribution, including opaque authentication
// identifier bytes. Proposed receipts are not adjudications or proof that a
// later inventory publication has completed.
type OutcomeReceipt struct {
	ID, ObservationID              shoal.ID
	Observation                    OutcomeObservation
	SubmitterID, ActorID, ClientID shoal.ID
	OnBehalfOf                     []shoal.ID
	AuthorizationFingerprint       string
	ReceivedAt                     time.Time
	State                          string
}
type OutcomeProvider interface {
	AppendOutcome(context.Context, OutcomeObservation, []byte) (OutcomeReceipt, error)
	ReadOutcome(context.Context, shoal.ID, shoal.ID, []byte) (OutcomeReceipt, error)
}
type outcomeProvenanceWire struct {
	ReporterID string `json:"reporter_id,omitempty"`
	ModelID    string `json:"model_id,omitempty"`
	PromptID   string `json:"prompt_id,omitempty"`
	ToolID     string `json:"tool_id,omitempty"`
}
type outcomeObservationWire struct {
	RequestID          string                `json:"request_id"`
	PredictionID       string                `json:"prediction_id"`
	SubjectID          string                `json:"subject_id"`
	Kind               string                `json:"kind"`
	QuestionID         string                `json:"question_id,omitempty"`
	Label              string                `json:"label,omitempty"`
	Truth              *bool                 `json:"truth,omitempty"`
	ExecutionStatus    string                `json:"execution_status,omitempty"`
	ActionID           string                `json:"action_id,omitempty"`
	EvidenceIDs        []string              `json:"evidence_ids"`
	ObservedAt         time.Time             `json:"observed_at"`
	AssertedProvenance outcomeProvenanceWire `json:"asserted_provenance"`
	Supersedes         string                `json:"supersedes,omitempty"`
}
type outcomeRequestWire struct {
	Schema      int                    `json:"schema"`
	Observation outcomeObservationWire `json:"observation"`
}
type outcomeReceiptWire struct {
	ID                       string                 `json:"id"`
	ObservationID            string                 `json:"observation_id"`
	Observation              outcomeObservationWire `json:"observation"`
	SubmitterID              string                 `json:"submitter_id"`
	ActorID                  string                 `json:"actor_id,omitempty"`
	ClientID                 string                 `json:"client_id,omitempty"`
	OnBehalfOf               []string               `json:"on_behalf_of"`
	AuthorizationFingerprint string                 `json:"authorization_fingerprint"`
	ReceivedAt               time.Time              `json:"received_at"`
	State                    string                 `json:"state"`
}
type outcomeResponseWire struct {
	Schema  int                `json:"schema"`
	Receipt outcomeReceiptWire `json:"receipt"`
}

func invalidOutcome() error { return errors.New("invalid outcome protocol value") }
func outcomeID(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && shoal.ValidateRequiredID("outcome identifier", id) == nil
}
func outcomeTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func outcomeDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func outcomeReceiptID(id shoal.ID) bool {
	return strings.HasPrefix(string(id), "outcome-receipt:") && outcomeDigest(strings.TrimPrefix(string(id), "outcome-receipt:"))
}

// NormalizeOutcomeObservation detaches pointers/slices and normalizes the same
// evidence ordering and UTC timestamp used by the retained core observation.
// Task-specific label membership and access remain the provider's responsibility.
func NormalizeOutcomeObservation(o OutcomeObservation) (OutcomeObservation, error) {
	if !outcomeID(o.RequestID) || !outcomeID(o.PredictionID) || !outcomeID(o.SubjectID) || !outcomeTime(o.ObservedAt) || len(o.EvidenceIDs) == 0 || len(o.EvidenceIDs) > 1024 {
		return OutcomeObservation{}, invalidOutcome()
	}
	for _, id := range []shoal.ID{o.QuestionID, o.ActionID, o.AssertedProvenance.ReporterID, o.AssertedProvenance.ModelID, o.AssertedProvenance.PromptID, o.AssertedProvenance.ToolID} {
		if id != "" && !outcomeID(id) {
			return OutcomeObservation{}, invalidOutcome()
		}
	}
	if o.Supersedes != "" && !outcomeReceiptID(o.Supersedes) {
		return OutcomeObservation{}, invalidOutcome()
	}
	if len(o.Label) > shoal.MaxSemanticStringBytes || !utf8.ValidString(o.Label) {
		return OutcomeObservation{}, invalidOutcome()
	}
	switch o.Kind {
	case "correctness":
		if o.QuestionID == "" || o.ActionID != "" || o.ExecutionStatus != "" || (o.Truth == nil) == (o.Label == "") {
			return OutcomeObservation{}, invalidOutcome()
		}
	case "execution":
		if o.ActionID == "" || o.QuestionID != "" || o.Label != "" || o.Truth != nil || (o.ExecutionStatus != "succeeded" && o.ExecutionStatus != "failed" && o.ExecutionStatus != "unknown") {
			return OutcomeObservation{}, invalidOutcome()
		}
	default:
		return OutcomeObservation{}, invalidOutcome()
	}
	o.EvidenceIDs = append([]shoal.ID(nil), o.EvidenceIDs...)
	sort.Slice(o.EvidenceIDs, func(i, j int) bool { return o.EvidenceIDs[i] < o.EvidenceIDs[j] })
	for i, id := range o.EvidenceIDs {
		if !outcomeID(id) || (i > 0 && o.EvidenceIDs[i-1] == id) {
			return OutcomeObservation{}, invalidOutcome()
		}
	}
	if o.Truth != nil {
		v := *o.Truth
		o.Truth = &v
	}
	o.ObservedAt = o.ObservedAt.UTC().Round(0)
	return o, nil
}
func outcomeToWire(o OutcomeObservation) outcomeObservationWire {
	w := outcomeObservationWire{RequestID: EncodeID(o.RequestID), PredictionID: EncodeID(o.PredictionID), SubjectID: EncodeID(o.SubjectID), Kind: o.Kind, QuestionID: EncodeID(o.QuestionID), Label: o.Label, Truth: o.Truth, ExecutionStatus: o.ExecutionStatus, ActionID: EncodeID(o.ActionID), ObservedAt: o.ObservedAt, AssertedProvenance: outcomeProvenanceWire{EncodeID(o.AssertedProvenance.ReporterID), EncodeID(o.AssertedProvenance.ModelID), EncodeID(o.AssertedProvenance.PromptID), EncodeID(o.AssertedProvenance.ToolID)}, Supersedes: EncodeID(o.Supersedes)}
	for _, id := range o.EvidenceIDs {
		w.EvidenceIDs = append(w.EvidenceIDs, EncodeID(id))
	}
	return w
}
func outcomeFromWire(w outcomeObservationWire) (OutcomeObservation, error) {
	o := OutcomeObservation{Kind: w.Kind, Label: w.Label, Truth: w.Truth, ExecutionStatus: w.ExecutionStatus, ObservedAt: w.ObservedAt}
	for _, p := range []struct {
		s        string
		id       *shoal.ID
		optional bool
	}{{w.RequestID, &o.RequestID, false}, {w.PredictionID, &o.PredictionID, false}, {w.SubjectID, &o.SubjectID, false}, {w.QuestionID, &o.QuestionID, true}, {w.ActionID, &o.ActionID, true}, {w.Supersedes, &o.Supersedes, true}, {w.AssertedProvenance.ReporterID, &o.AssertedProvenance.ReporterID, true}, {w.AssertedProvenance.ModelID, &o.AssertedProvenance.ModelID, true}, {w.AssertedProvenance.PromptID, &o.AssertedProvenance.PromptID, true}, {w.AssertedProvenance.ToolID, &o.AssertedProvenance.ToolID, true}} {
		if p.s == "" && p.optional {
			continue
		}
		id, e := DecodeID(p.s)
		if e != nil {
			return o, invalidOutcome()
		}
		*p.id = id
	}
	if len(w.EvidenceIDs) > 1024 {
		return o, invalidOutcome()
	}
	for _, s := range w.EvidenceIDs {
		id, e := DecodeID(s)
		if e != nil {
			return o, invalidOutcome()
		}
		o.EvidenceIDs = append(o.EvidenceIDs, id)
	}
	return NormalizeOutcomeObservation(o)
}
func EncodeOutcomeRequest(o OutcomeObservation) ([]byte, error) {
	n, e := NormalizeOutcomeObservation(o)
	if e != nil {
		return nil, e
	}
	b, e := json.Marshal(outcomeRequestWire{1, outcomeToWire(n)})
	if e != nil || len(b) > MaxOutcomeRequestBytes {
		return nil, invalidOutcome()
	}
	return b, nil
}
func DecodeOutcomeRequest(b []byte) (out OutcomeObservation, err error) {
	defer func() {
		if err != nil {
			out = OutcomeObservation{}
		}
	}()
	var w outcomeRequestWire
	if len(b) == 0 || len(b) > MaxOutcomeRequestBytes {
		return OutcomeObservation{}, invalidOutcome()
	}
	if e := decodeStrict(b, &w); e != nil || w.Schema != 1 {
		return OutcomeObservation{}, invalidOutcome()
	}
	return outcomeFromWire(w.Observation)
}

// ValidateOutcomeReceipt checks wire structure, not truth or authorization.
func ValidateOutcomeReceipt(r OutcomeReceipt) error {
	if _, e := NormalizeOutcomeObservation(r.Observation); e != nil {
		return e
	}
	if !outcomeReceiptID(r.ID) || !strings.HasPrefix(string(r.ObservationID), "decision:outcome:v1:") || !outcomeDigest(strings.TrimPrefix(string(r.ObservationID), "decision:outcome:v1:")) || r.State != "proposed" || (!strings.HasPrefix(r.AuthorizationFingerprint, "auth-sha256:") || !outcomeDigest(strings.TrimPrefix(r.AuthorizationFingerprint, "auth-sha256:"))) || !outcomeTime(r.ReceivedAt) || r.Observation.ObservedAt.After(r.ReceivedAt) || len(r.OnBehalfOf) > 64 {
		return invalidOutcome()
	}
	if len(r.SubmitterID) == 0 || len(r.SubmitterID) > shoal.MaxIDBytes {
		return invalidOutcome()
	}
	for _, id := range append([]shoal.ID{r.ActorID, r.ClientID}, r.OnBehalfOf...) {
		if len(id) > shoal.MaxIDBytes {
			return invalidOutcome()
		}
	}
	for _, id := range r.OnBehalfOf {
		if len(id) == 0 {
			return invalidOutcome()
		}
	}
	return nil
}
func EncodeOutcomeReceipt(r OutcomeReceipt) ([]byte, error) {
	if e := ValidateOutcomeReceipt(r); e != nil {
		return nil, e
	}
	o, _ := NormalizeOutcomeObservation(r.Observation)
	w := outcomeReceiptWire{ID: string(r.ID), ObservationID: EncodeID(r.ObservationID), Observation: outcomeToWire(o), SubmitterID: EncodeKey([]byte(r.SubmitterID)), ActorID: EncodeKey([]byte(r.ActorID)), ClientID: EncodeKey([]byte(r.ClientID)), OnBehalfOf: []string{}, AuthorizationFingerprint: r.AuthorizationFingerprint, ReceivedAt: r.ReceivedAt.UTC().Round(0), State: r.State}
	for _, id := range r.OnBehalfOf {
		w.OnBehalfOf = append(w.OnBehalfOf, EncodeKey([]byte(id)))
	}
	b, e := json.Marshal(outcomeResponseWire{1, w})
	if e != nil || len(b) > MaxOutcomeResponseBytes {
		return nil, invalidOutcome()
	}
	return b, nil
}
func DecodeOutcomeReceipt(b []byte) (out OutcomeReceipt, err error) {
	defer func() {
		if err != nil {
			out = OutcomeReceipt{}
		}
	}()
	var r OutcomeReceipt
	var w outcomeResponseWire
	if len(b) == 0 || len(b) > MaxOutcomeResponseBytes {
		return r, invalidOutcome()
	}
	if e := decodeStrict(b, &w); e != nil || w.Schema != 1 {
		return r, invalidOutcome()
	}
	var e error
	r.ID = shoal.ID(w.Receipt.ID)
	r.ObservationID, e = DecodeID(w.Receipt.ObservationID)
	if e != nil {
		return r, invalidOutcome()
	}
	r.Observation, e = outcomeFromWire(w.Receipt.Observation)
	if e != nil {
		return r, e
	}
	for _, p := range []struct {
		s        string
		id       *shoal.ID
		optional bool
	}{{w.Receipt.SubmitterID, &r.SubmitterID, false}, {w.Receipt.ActorID, &r.ActorID, true}, {w.Receipt.ClientID, &r.ClientID, true}} {
		if p.s == "" && p.optional {
			continue
		}
		raw, e := DecodeKey(p.s)
		if e != nil {
			return r, invalidOutcome()
		}
		*p.id = shoal.ID(raw)
	}
	if len(w.Receipt.OnBehalfOf) > 64 {
		return r, invalidOutcome()
	}
	for _, s := range w.Receipt.OnBehalfOf {
		raw, e := DecodeKey(s)
		if e != nil {
			return r, invalidOutcome()
		}
		r.OnBehalfOf = append(r.OnBehalfOf, shoal.ID(raw))
	}
	r.AuthorizationFingerprint = w.Receipt.AuthorizationFingerprint
	r.ReceivedAt = w.Receipt.ReceivedAt.UTC().Round(0)
	r.State = w.Receipt.State
	if e = ValidateOutcomeReceipt(r); e != nil {
		return OutcomeReceipt{}, e
	}
	return r, nil
}

// MatchOutcomeObservation compares canonical content without losing truth=false.
func MatchOutcomeObservation(a, b OutcomeObservation) bool {
	na, ea := NormalizeOutcomeObservation(a)
	nb, eb := NormalizeOutcomeObservation(b)
	return ea == nil && eb == nil && reflect.DeepEqual(na, nb)
}
