// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxAdjudicationRequestBytes = 2 << 20
const MaxAdjudicationResponseBytes = 4 << 20
const MaxAdjudicationHistoryBytes = 32 << 20

// AdjudicationProposal requests a disposition for registered evidence. Even
// "verified" is an assertion until the trusted service admits it. Callers cannot
// supply a basis, role membership, policy configuration, or training grant.
// All Go identifiers are decoded; codecs handle canonical base64 wire values.
type AdjudicationProposal struct {
	RequestID, PredictionID, SubjectID, QuestionID shoal.ID
	ObservationReceiptIDs, WitnessIDs              []shoal.ID
	Disposition, Label                             string
	Truth                                          *bool
	Reason                                         string
	ExpectedHeadID                                 shoal.ID
	ExpectedVersion                                int64
}

// AdjudicationAttribution contains original authenticated, opaque identifiers.
type AdjudicationAttribution struct {
	SubjectID, ActorID, ClientID shoal.ID
	OnBehalfOf                   []shoal.ID
	AuthorizationFingerprint     string
}

// AdjudicationReceipt references the retained basis without disclosing its
// contents. An admitted disposition alone does not grant training eligibility.
type AdjudicationReceipt struct {
	BasisID, ID                                       shoal.ID
	Version                                           int64
	TargetID, TaskID, PictureID, PolicyID, ProposalID shoal.ID
	Proposal                                          AdjudicationProposal
	Adjudicator                                       AdjudicationAttribution
	ReceivedAt                                        time.Time
}

// AdjudicationHistory is one complete authorized target history, never a page.
type AdjudicationHistory struct {
	TargetID shoal.ID
	Receipts []AdjudicationReceipt
}
type AdjudicationProvider interface {
	Adjudicate(context.Context, AdjudicationProposal, []byte) (AdjudicationReceipt, error)
	AdjudicationHistory(context.Context, shoal.ID) (AdjudicationHistory, error)
}
type adjudicationProposalWire struct {
	RequestID             string   `json:"request_id"`
	PredictionID          string   `json:"prediction_id"`
	SubjectID             string   `json:"subject_id"`
	QuestionID            string   `json:"question_id"`
	ObservationReceiptIDs []string `json:"observation_receipt_ids"`
	WitnessIDs            []string `json:"witness_ids"`
	Disposition           string   `json:"disposition"`
	Label                 string   `json:"label,omitempty"`
	Truth                 *bool    `json:"truth,omitempty"`
	Reason                string   `json:"reason,omitempty"`
	ExpectedHeadID        string   `json:"expected_head_id,omitempty"`
	ExpectedVersion       int64    `json:"expected_version"`
}
type adjudicationAttributionWire struct {
	SubjectID                string   `json:"subject_id"`
	ActorID                  string   `json:"actor_id"`
	ClientID                 string   `json:"client_id,omitempty"`
	OnBehalfOf               []string `json:"on_behalf_of"`
	AuthorizationFingerprint string   `json:"authorization_fingerprint"`
}
type adjudicationReceiptWire struct {
	BasisID     string                      `json:"basis_id"`
	ID          string                      `json:"id"`
	Version     int64                       `json:"version"`
	TargetID    string                      `json:"target_id"`
	TaskID      string                      `json:"task_id"`
	PictureID   string                      `json:"picture_id"`
	PolicyID    string                      `json:"policy_id"`
	ProposalID  string                      `json:"proposal_id"`
	Proposal    adjudicationProposalWire    `json:"proposal"`
	Adjudicator adjudicationAttributionWire `json:"adjudicator"`
	ReceivedAt  time.Time                   `json:"received_at"`
}
type adjudicationRequestWire struct {
	Schema   int                      `json:"schema"`
	Proposal adjudicationProposalWire `json:"proposal"`
}
type adjudicationResponseWire struct {
	Schema  int                     `json:"schema"`
	Receipt adjudicationReceiptWire `json:"receipt"`
}
type adjudicationHistoryWire struct {
	Schema   int                       `json:"schema"`
	TargetID string                    `json:"target_id"`
	Receipts []adjudicationReceiptWire `json:"receipts"`
}

func invalidAdjudication() error { return errors.New("invalid adjudication protocol value") }
func adjudicationHashID(id shoal.ID, prefix string) bool {
	return strings.HasPrefix(string(id), prefix) && outcomeDigest(strings.TrimPrefix(string(id), prefix))
}
func adjudicationText(s string) bool {
	return len(s) > 0 && len(s) <= shoal.MaxSemanticStringBytes && utf8.ValidString(s) && strings.TrimSpace(s) != ""
}

// NormalizeAdjudicationProposal copies mutable fields and sorts both reference
// sets; it does not resolve a registered task or authenticate any witness.
func NormalizeAdjudicationProposal(p AdjudicationProposal) (AdjudicationProposal, error) {
	for _, id := range []shoal.ID{p.RequestID, p.PredictionID, p.SubjectID, p.QuestionID} {
		if !outcomeID(id) {
			return AdjudicationProposal{}, invalidAdjudication()
		}
	}
	if len(p.ObservationReceiptIDs) < 1 || len(p.ObservationReceiptIDs) > 256 || len(p.WitnessIDs) > 1024 || p.ExpectedVersion < 0 || p.ExpectedVersion >= 128 {
		return AdjudicationProposal{}, invalidAdjudication()
	}
	if (p.ExpectedHeadID == "") != (p.ExpectedVersion == 0) || (p.ExpectedHeadID != "" && !adjudicationHashID(p.ExpectedHeadID, "adjudication-receipt:")) {
		return AdjudicationProposal{}, invalidAdjudication()
	}
	switch p.Disposition {
	case "verified":
		if p.Reason != "" || len(p.WitnessIDs) == 0 || (p.Truth == nil) == (p.Label == "") || (p.Label != "" && !adjudicationText(p.Label)) {
			return AdjudicationProposal{}, invalidAdjudication()
		}
	case "disputed", "unresolved":
		if p.Label != "" || p.Truth != nil || !adjudicationText(p.Reason) || (p.Disposition == "disputed" && len(p.WitnessIDs) == 0) {
			return AdjudicationProposal{}, invalidAdjudication()
		}
	default:
		return AdjudicationProposal{}, invalidAdjudication()
	}
	p.ObservationReceiptIDs = append([]shoal.ID{}, p.ObservationReceiptIDs...)
	p.WitnessIDs = append([]shoal.ID{}, p.WitnessIDs...)
	for _, set := range []struct {
		ids      []shoal.ID
		receipts bool
	}{{p.ObservationReceiptIDs, true}, {p.WitnessIDs, false}} {
		sort.Slice(set.ids, func(i, j int) bool { return set.ids[i] < set.ids[j] })
		for i, id := range set.ids {
			if !outcomeID(id) || (set.receipts && !outcomeReceiptID(id)) || (i > 0 && set.ids[i-1] == id) {
				return AdjudicationProposal{}, invalidAdjudication()
			}
		}
	}
	if p.Truth != nil {
		v := *p.Truth
		p.Truth = &v
	}
	return p, nil
}
func MatchAdjudicationProposal(a, b AdjudicationProposal) bool {
	a, ea := NormalizeAdjudicationProposal(a)
	b, eb := NormalizeAdjudicationProposal(b)
	return ea == nil && eb == nil && reflect.DeepEqual(a, b)
}
func adjudicationProposalToWire(p AdjudicationProposal) adjudicationProposalWire {
	w := adjudicationProposalWire{RequestID: EncodeID(p.RequestID), PredictionID: EncodeID(p.PredictionID), SubjectID: EncodeID(p.SubjectID), QuestionID: EncodeID(p.QuestionID), ObservationReceiptIDs: []string{}, WitnessIDs: []string{}, Disposition: p.Disposition, Label: p.Label, Truth: p.Truth, Reason: p.Reason, ExpectedHeadID: EncodeID(p.ExpectedHeadID), ExpectedVersion: p.ExpectedVersion}
	for _, id := range p.ObservationReceiptIDs {
		w.ObservationReceiptIDs = append(w.ObservationReceiptIDs, EncodeID(id))
	}
	for _, id := range p.WitnessIDs {
		w.WitnessIDs = append(w.WitnessIDs, EncodeID(id))
	}
	return w
}
func adjudicationProposalFromWire(w adjudicationProposalWire) (AdjudicationProposal, error) {
	p := AdjudicationProposal{Disposition: w.Disposition, Label: w.Label, Truth: w.Truth, Reason: w.Reason, ExpectedVersion: w.ExpectedVersion}
	for _, v := range []struct {
		s        string
		id       *shoal.ID
		optional bool
	}{{w.RequestID, &p.RequestID, false}, {w.PredictionID, &p.PredictionID, false}, {w.SubjectID, &p.SubjectID, false}, {w.QuestionID, &p.QuestionID, false}, {w.ExpectedHeadID, &p.ExpectedHeadID, true}} {
		if v.optional && v.s == "" {
			continue
		}
		id, e := DecodeID(v.s)
		if e != nil {
			return p, invalidAdjudication()
		}
		*v.id = id
	}
	if len(w.ObservationReceiptIDs) > 256 || len(w.WitnessIDs) > 1024 {
		return p, invalidAdjudication()
	}
	for _, set := range []struct {
		wire []string
		ids  *[]shoal.ID
	}{{w.ObservationReceiptIDs, &p.ObservationReceiptIDs}, {w.WitnessIDs, &p.WitnessIDs}} {
		for _, s := range set.wire {
			id, e := DecodeID(s)
			if e != nil {
				return p, invalidAdjudication()
			}
			*set.ids = append(*set.ids, id)
		}
	}
	return NormalizeAdjudicationProposal(p)
}
func EncodeAdjudicationRequest(p AdjudicationProposal) ([]byte, error) {
	p, e := NormalizeAdjudicationProposal(p)
	if e != nil {
		return nil, e
	}
	b, e := json.Marshal(adjudicationRequestWire{1, adjudicationProposalToWire(p)})
	if e != nil || len(b) > MaxAdjudicationRequestBytes {
		return nil, invalidAdjudication()
	}
	return b, nil
}
func DecodeAdjudicationRequest(b []byte) (p AdjudicationProposal, err error) {
	defer func() {
		if err != nil {
			p = AdjudicationProposal{}
		}
	}()
	var w adjudicationRequestWire
	if len(b) == 0 || len(b) > MaxAdjudicationRequestBytes {
		return p, invalidAdjudication()
	}
	if e := preflightAdjudication(b); e != nil {
		return p, invalidAdjudication()
	}
	if e := decodeStrict(b, &w); e != nil || w.Schema != 1 {
		return p, invalidAdjudication()
	}
	return adjudicationProposalFromWire(w.Proposal)
}
func ValidateAdjudicationReceipt(r AdjudicationReceipt) error {
	if _, e := NormalizeAdjudicationProposal(r.Proposal); e != nil {
		return e
	}
	if !adjudicationHashID(r.ID, "adjudication-receipt:") || !adjudicationHashID(r.BasisID, "decision:adjudication-basis:v1:") || !adjudicationHashID(r.TargetID, "decision:adjudication-target:v1:") || !adjudicationHashID(r.PolicyID, "decision:label-policy:v1:") || !adjudicationHashID(r.ProposalID, "decision:adjudication-proposal:v1:") || !outcomeID(r.TaskID) || !outcomeID(r.PictureID) || r.Version < 1 || r.Version > 128 || r.Version != r.Proposal.ExpectedVersion+1 || !outcomeTime(r.ReceivedAt) {
		return invalidAdjudication()
	}
	a := r.Adjudicator
	if len(a.SubjectID) == 0 || len(a.ActorID) == 0 || len(a.OnBehalfOf) > 64 || !strings.HasPrefix(a.AuthorizationFingerprint, "auth-sha256:") || !outcomeDigest(strings.TrimPrefix(a.AuthorizationFingerprint, "auth-sha256:")) {
		return invalidAdjudication()
	}
	for _, id := range []shoal.ID{a.SubjectID, a.ActorID, a.ClientID} {
		if len(id) > shoal.MaxIDBytes {
			return invalidAdjudication()
		}
	}
	for _, id := range a.OnBehalfOf {
		if len(id) == 0 || len(id) > shoal.MaxIDBytes {
			return invalidAdjudication()
		}
	}
	return nil
}
func adjudicationReceiptToWire(r AdjudicationReceipt) adjudicationReceiptWire {
	p, _ := NormalizeAdjudicationProposal(r.Proposal)
	a := r.Adjudicator
	w := adjudicationReceiptWire{BasisID: EncodeID(r.BasisID), ID: string(r.ID), Version: r.Version, TargetID: EncodeID(r.TargetID), TaskID: EncodeID(r.TaskID), PictureID: EncodeID(r.PictureID), PolicyID: EncodeID(r.PolicyID), ProposalID: EncodeID(r.ProposalID), Proposal: adjudicationProposalToWire(p), Adjudicator: adjudicationAttributionWire{SubjectID: EncodeKey([]byte(a.SubjectID)), ActorID: EncodeKey([]byte(a.ActorID)), ClientID: EncodeKey([]byte(a.ClientID)), OnBehalfOf: []string{}, AuthorizationFingerprint: a.AuthorizationFingerprint}, ReceivedAt: r.ReceivedAt.UTC().Round(0)}
	for _, id := range a.OnBehalfOf {
		w.Adjudicator.OnBehalfOf = append(w.Adjudicator.OnBehalfOf, EncodeKey([]byte(id)))
	}
	return w
}
func adjudicationReceiptFromWire(w adjudicationReceiptWire) (AdjudicationReceipt, error) {
	r := AdjudicationReceipt{ID: shoal.ID(w.ID), Version: w.Version, ReceivedAt: w.ReceivedAt.UTC().Round(0)}
	for _, v := range []struct {
		s  string
		id *shoal.ID
	}{{w.BasisID, &r.BasisID}, {w.TargetID, &r.TargetID}, {w.TaskID, &r.TaskID}, {w.PictureID, &r.PictureID}, {w.PolicyID, &r.PolicyID}, {w.ProposalID, &r.ProposalID}} {
		id, e := DecodeID(v.s)
		if e != nil {
			return r, invalidAdjudication()
		}
		*v.id = id
	}
	var e error
	r.Proposal, e = adjudicationProposalFromWire(w.Proposal)
	if e != nil {
		return r, e
	}
	for _, v := range []struct {
		s        string
		id       *shoal.ID
		optional bool
	}{{w.Adjudicator.SubjectID, &r.Adjudicator.SubjectID, false}, {w.Adjudicator.ActorID, &r.Adjudicator.ActorID, false}, {w.Adjudicator.ClientID, &r.Adjudicator.ClientID, true}} {
		if v.optional && v.s == "" {
			continue
		}
		b, e := DecodeKey(v.s)
		if e != nil {
			return r, invalidAdjudication()
		}
		*v.id = shoal.ID(b)
	}
	if len(w.Adjudicator.OnBehalfOf) > 64 {
		return r, invalidAdjudication()
	}
	for _, s := range w.Adjudicator.OnBehalfOf {
		b, e := DecodeKey(s)
		if e != nil {
			return r, invalidAdjudication()
		}
		r.Adjudicator.OnBehalfOf = append(r.Adjudicator.OnBehalfOf, shoal.ID(b))
	}
	r.Adjudicator.AuthorizationFingerprint = w.Adjudicator.AuthorizationFingerprint
	return r, ValidateAdjudicationReceipt(r)
}
func EncodeAdjudicationReceipt(r AdjudicationReceipt) ([]byte, error) {
	if e := ValidateAdjudicationReceipt(r); e != nil {
		return nil, e
	}
	b, e := json.Marshal(adjudicationResponseWire{1, adjudicationReceiptToWire(r)})
	if e != nil || len(b) > MaxAdjudicationResponseBytes {
		return nil, invalidAdjudication()
	}
	return b, nil
}
func DecodeAdjudicationReceipt(b []byte) (r AdjudicationReceipt, err error) {
	defer func() {
		if err != nil {
			r = AdjudicationReceipt{}
		}
	}()
	var w adjudicationResponseWire
	if len(b) == 0 || len(b) > MaxAdjudicationResponseBytes {
		return r, invalidAdjudication()
	}
	if e := preflightAdjudication(b); e != nil {
		return r, invalidAdjudication()
	}
	if e := decodeStrict(b, &w); e != nil || w.Schema != 1 {
		return r, invalidAdjudication()
	}
	return adjudicationReceiptFromWire(w.Receipt)
}

// ValidateAdjudicationHistory checks a complete bounded chain. This cannot prove
// freshness or authorization; only the trusted service may supply its contents.
func ValidateAdjudicationHistory(h AdjudicationHistory) error {
	if len(h.Receipts) == 0 || len(h.Receipts) > 128 || !adjudicationHashID(h.TargetID, "decision:adjudication-target:v1:") {
		return invalidAdjudication()
	}
	first := h.Receipts[0]
	var previous shoal.ID
	var timestamp time.Time
	seen := map[shoal.ID]bool{}
	for i, r := range h.Receipts {
		if e := ValidateAdjudicationReceipt(r); e != nil {
			return e
		}
		if r.TargetID != h.TargetID || r.TaskID != first.TaskID || r.PictureID != first.PictureID || r.PolicyID != first.PolicyID || r.Proposal.SubjectID != first.Proposal.SubjectID || r.Proposal.QuestionID != first.Proposal.QuestionID || r.Version != int64(i+1) || r.Proposal.ExpectedHeadID != previous || r.ReceivedAt.Before(timestamp) || seen[r.ID] {
			return invalidAdjudication()
		}
		seen[r.ID] = true
		previous = r.ID
		timestamp = r.ReceivedAt
	}
	return nil
}
func EncodeAdjudicationHistory(h AdjudicationHistory) ([]byte, error) {
	if e := ValidateAdjudicationHistory(h); e != nil {
		return nil, e
	}
	// Bound cumulatively before assembling another potentially large wire row.
	w := adjudicationHistoryWire{Schema: 1, TargetID: EncodeID(h.TargetID), Receipts: []adjudicationReceiptWire{}}
	used := 256
	for _, r := range h.Receipts {
		row := adjudicationReceiptToWire(r)
		raw, e := json.Marshal(row)
		if e != nil || len(raw) > MaxAdjudicationResponseBytes || len(raw) > MaxAdjudicationHistoryBytes-used {
			return nil, invalidAdjudication()
		}
		used += len(raw) + 1
		w.Receipts = append(w.Receipts, row)
	}
	b, e := json.Marshal(w)
	if e != nil || len(b) > MaxAdjudicationHistoryBytes {
		return nil, invalidAdjudication()
	}
	return b, nil
}
func DecodeAdjudicationHistory(b []byte) (h AdjudicationHistory, err error) {
	defer func() {
		if err != nil {
			h = AdjudicationHistory{}
		}
	}()
	var w adjudicationHistoryWire
	if len(b) == 0 || len(b) > MaxAdjudicationHistoryBytes {
		return h, invalidAdjudication()
	}
	if e := preflightAdjudication(b); e != nil {
		return h, invalidAdjudication()
	}
	if e := decodeStrict(b, &w); e != nil || w.Schema != 1 || len(w.Receipts) > 128 {
		return h, invalidAdjudication()
	}
	h.TargetID, err = DecodeID(w.TargetID)
	if err != nil {
		return h, err
	}
	for _, row := range w.Receipts {
		r, e := adjudicationReceiptFromWire(row)
		if e != nil {
			return h, e
		}
		h.Receipts = append(h.Receipts, r)
	}
	return h, ValidateAdjudicationHistory(h)
}
