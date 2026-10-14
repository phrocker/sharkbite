// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package api defines the public authenticated collector HTTP protocol and a
// Go client for it. It imports only pkg/collector and pkg/shoal.
//
// Commit-bearing routes (enroll, artifacts, observations) return small
// receipts of identifiers and counters only, never submitted content, so a
// narrow workspace output budget cannot make a committed write unreadable.
// Content is read back through the observation GET route.
package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// Schema is the wire schema of every request and response body.
	Schema = 1

	Route             = "/api/v1/collectors"
	EnrollRoute       = Route + "/enroll"
	ArtifactsRoute    = Route + "/artifacts"
	ObservationsRoute = Route + "/observations"

	// MaxRequestBytes bounds one request body: a full observation payload in
	// base64 plus framing.
	MaxRequestBytes = 128 * 1024
	// MaxResponseBytes bounds one response body.
	MaxResponseBytes = 256 * 1024
	// MaxReceiptBytes bounds every write receipt, with all IDs at
	// shoal.MaxIDBytes. A workspace output budget of at least this size always
	// returns a readable receipt from a committed write; below it a write that
	// committed may report indeterminate.
	MaxReceiptBytes = 3 * 1024
)

var (
	// ErrIndeterminate marks a write whose durable outcome is unknown. Retry
	// with the same request: every collector write is idempotent.
	ErrIndeterminate = errors.New("collector write outcome indeterminate")
	// ErrPermissionDenied marks a definite refusal by a bound collector, such
	// as requested authority beyond provisioning or a refused attestation.
	// Nothing was written.
	ErrPermissionDenied = errors.New("collector permission denied")
)

// CodePermissionDenied is the wire code for ErrPermissionDenied.
const CodePermissionDenied = "permission_denied"

// Provider is the server-side seam the HTTP handler calls. Implementations
// must resolve the caller from the trusted request context; nothing in these
// arguments carries authority.
type Provider interface {
	Enroll(context.Context, []byte, collector.EnrollRequest) (EnrollReceipt, error)
	SubmitArtifact(context.Context, shoal.ID, collector.ArtifactRef) (ArtifactReceipt, error)
	SubmitObservation(context.Context, collector.Observation) (ObservationReceipt, error)
	ReadObservation(context.Context, shoal.ID) (ObservationRecord, error)
}

type ErrorResponse struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Indeterminate bool   `json:"indeterminate,omitempty"`
}

type Extractor struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}
type Attestation struct {
	Kind     string `json:"kind"`
	Format   string `json:"format"`
	Evidence string `json:"evidence"`
}
type EnrollRequest struct {
	CollectorID                 string       `json:"collector_id"`
	RequestedAuthorityPolicyIDs []string     `json:"requested_authority_policy_ids"`
	Extractors                  []Extractor  `json:"extractors"`
	Attestation                 *Attestation `json:"attestation,omitempty"`
}

// EnrollReceipt deliberately omits the granted authority set: it is exactly
// the requested set, which the caller already holds.
type EnrollReceipt struct {
	Schema            int    `json:"schema"`
	CollectorID       string `json:"collector_id"`
	EnrollmentID      string `json:"enrollment_id"`
	Generation        int64  `json:"generation"`
	State             string `json:"state"`
	AttestationStatus string `json:"attestation_status,omitempty"`
}
type Artifact struct {
	ID         string    `json:"id"`
	Digest     string    `json:"digest"`
	Size       int64     `json:"size"`
	MediaType  string    `json:"media_type"`
	ObservedAt time.Time `json:"observed_at"`
}
type ArtifactRequest struct {
	CollectorID string   `json:"collector_id"`
	Artifact    Artifact `json:"artifact"`
}
type ArtifactReceipt struct {
	Schema      int       `json:"schema"`
	CollectorID string    `json:"collector_id"`
	ArtifactID  string    `json:"artifact_id"`
	Generation  int64     `json:"generation"`
	ReceivedAt  time.Time `json:"received_at"`
}
type Confidence struct {
	Disposition string   `json:"disposition"`
	Value       *float64 `json:"value,omitempty"`
}
type Observation struct {
	CollectorID string     `json:"collector_id"`
	ArtifactID  string     `json:"artifact_id"`
	Extractor   Extractor  `json:"extractor"`
	Confidence  Confidence `json:"confidence"`
	SubjectID   string     `json:"subject_id,omitempty"`
	Kind        string     `json:"kind"`
	Payload     string     `json:"payload,omitempty"`
	ObservedAt  time.Time  `json:"observed_at"`
}
type ObservationRequest struct {
	Observation Observation `json:"observation"`
}
type ObservationReceipt struct {
	Schema        int       `json:"schema"`
	ObservationID string    `json:"observation_id"`
	CollectorID   string    `json:"collector_id"`
	ArtifactID    string    `json:"artifact_id"`
	Generation    int64     `json:"generation"`
	ReceivedAt    time.Time `json:"received_at"`
}

// Observation statuses on read. Quarantine is computed when read from the
// collector's current registration; stored rows are never rewritten.
const (
	StatusActive      = "active"
	StatusQuarantined = "quarantined"
)

type ObservationRecord struct {
	Schema         int         `json:"schema"`
	ObservationID  string      `json:"observation_id"`
	Observation    Observation `json:"observation"`
	ArtifactDigest string      `json:"artifact_digest"`
	Generation     int64       `json:"generation"`
	ReceivedAt     time.Time   `json:"received_at"`
	Status         string      `json:"status"`
}

func EncodeID(id shoal.ID) string { return base64.RawURLEncoding.EncodeToString([]byte(id)) }
func DecodeID(encoded string) (shoal.ID, error) {
	b, e := DecodeKey(encoded)
	if e == nil && !utf8.Valid(b) {
		e = fmt.Errorf("invalid UTF-8 identifier")
	}
	return shoal.ID(b), e
}
func EncodeKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }
func DecodeKey(encoded string) ([]byte, error) {
	if len(encoded) > base64.RawURLEncoding.EncodedLen(shoal.MaxIDBytes) {
		return nil, fmt.Errorf("encoded identifier exceeds limit")
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if e != nil || len(b) == 0 || len(b) > shoal.MaxIDBytes || EncodeKey(b) != encoded {
		return nil, fmt.Errorf("invalid canonical identifier")
	}
	return b, nil
}
func decodeBytes(encoded string, limit int) ([]byte, error) {
	if len(encoded) > base64.RawURLEncoding.EncodedLen(limit) {
		return nil, fmt.Errorf("encoded bytes exceed limit")
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if e != nil || base64.RawURLEncoding.EncodeToString(b) != encoded {
		return nil, fmt.Errorf("invalid canonical bytes")
	}
	return b, nil
}

func invalidWire() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid collector request")
}

// EncodeEnroll converts a contract request to its wire form.
func EncodeEnroll(r collector.EnrollRequest) EnrollRequest {
	out := EnrollRequest{CollectorID: EncodeID(r.CollectorID), RequestedAuthorityPolicyIDs: []string{}, Extractors: []Extractor{}}
	for _, id := range r.RequestedAuthorityPolicyIDs {
		out.RequestedAuthorityPolicyIDs = append(out.RequestedAuthorityPolicyIDs, EncodeID(id))
	}
	for _, e := range r.Extractors {
		out.Extractors = append(out.Extractors, Extractor{EncodeID(e.ID), e.Version})
	}
	if a := r.Attestation; a != nil {
		out.Attestation = &Attestation{Kind: a.Kind, Format: a.Format, Evidence: EncodeKey(a.Evidence)}
	}
	return out
}

// DecodeEnroll converts and validates a wire request.
func DecodeEnroll(w EnrollRequest) (collector.EnrollRequest, error) {
	var r collector.EnrollRequest
	var e error
	if r.CollectorID, e = DecodeID(w.CollectorID); e != nil {
		return r, invalidWire()
	}
	if len(w.RequestedAuthorityPolicyIDs) > collector.MaxAuthorityPolicyIDs || len(w.Extractors) > collector.MaxExtractors {
		return r, invalidWire()
	}
	for _, encoded := range w.RequestedAuthorityPolicyIDs {
		id, e := DecodeID(encoded)
		if e != nil {
			return r, invalidWire()
		}
		r.RequestedAuthorityPolicyIDs = append(r.RequestedAuthorityPolicyIDs, id)
	}
	for _, x := range w.Extractors {
		id, e := DecodeID(x.ID)
		if e != nil {
			return r, invalidWire()
		}
		r.Extractors = append(r.Extractors, collector.ExtractorRef{ID: id, Version: x.Version})
	}
	if a := w.Attestation; a != nil {
		evidence, e := decodeBytes(a.Evidence, collector.MaxEvidenceBytes)
		if e != nil {
			return r, invalidWire()
		}
		r.Attestation = &collector.AttestationReport{Kind: a.Kind, Format: a.Format, Evidence: evidence}
	}
	if r, e = r.Canonical(); e != nil {
		return r, invalidWire()
	}
	return r, nil
}

func EncodeArtifact(collectorID shoal.ID, a collector.ArtifactRef) ArtifactRequest {
	return ArtifactRequest{CollectorID: EncodeID(collectorID), Artifact: Artifact{ID: EncodeID(a.ID), Digest: a.Digest, Size: a.Size, MediaType: a.MediaType, ObservedAt: a.ObservedAt}}
}
func DecodeArtifact(w ArtifactRequest) (shoal.ID, collector.ArtifactRef, error) {
	collectorID, e := DecodeID(w.CollectorID)
	if e != nil {
		return "", collector.ArtifactRef{}, invalidWire()
	}
	id, e := DecodeID(w.Artifact.ID)
	if e != nil {
		return "", collector.ArtifactRef{}, invalidWire()
	}
	a := collector.ArtifactRef{ID: id, Digest: w.Artifact.Digest, Size: w.Artifact.Size, MediaType: w.Artifact.MediaType, ObservedAt: w.Artifact.ObservedAt}
	if a.Validate() != nil {
		return "", collector.ArtifactRef{}, invalidWire()
	}
	return collectorID, a, nil
}

func EncodeObservation(o collector.Observation) Observation {
	c := o.Config()
	out := Observation{CollectorID: EncodeID(c.CollectorID), ArtifactID: EncodeID(c.ArtifactID), Extractor: Extractor{EncodeID(c.Extractor.ID), c.Extractor.Version}, Confidence: Confidence{Disposition: string(c.Confidence.Disposition), Value: c.Confidence.Value}, Kind: c.Kind, ObservedAt: c.ObservedAt}
	if c.SubjectID != "" {
		out.SubjectID = EncodeID(c.SubjectID)
	}
	if len(c.Payload) != 0 {
		out.Payload = EncodeKey(c.Payload)
	}
	return out
}
func DecodeObservation(w Observation) (collector.Observation, error) {
	var c collector.ObservationConfig
	var e error
	bad := func() (collector.Observation, error) { return collector.Observation{}, invalidWire() }
	if c.CollectorID, e = DecodeID(w.CollectorID); e != nil {
		return bad()
	}
	if c.ArtifactID, e = DecodeID(w.ArtifactID); e != nil {
		return bad()
	}
	if c.Extractor.ID, e = DecodeID(w.Extractor.ID); e != nil {
		return bad()
	}
	c.Extractor.Version = w.Extractor.Version
	if w.SubjectID != "" {
		if c.SubjectID, e = DecodeID(w.SubjectID); e != nil {
			return bad()
		}
	}
	if w.Payload != "" {
		if c.Payload, e = decodeBytes(w.Payload, collector.MaxPayloadBytes); e != nil || len(c.Payload) == 0 {
			return bad()
		}
	}
	c.Confidence = collector.Confidence{Disposition: collector.Disposition(w.Confidence.Disposition), Value: w.Confidence.Value}
	c.Kind, c.ObservedAt = w.Kind, w.ObservedAt
	o, e := collector.NewObservation(c)
	if e != nil {
		return bad()
	}
	return o, nil
}

func validEncodedID(s string) bool { _, e := DecodeID(s); return e == nil }
func validTime(t time.Time) bool   { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func badResponse() error           { return fmt.Errorf("invalid collector response") }

func (r EnrollReceipt) Validate() error {
	if r.Schema != Schema || !validEncodedID(r.CollectorID) || r.Generation <= 0 || r.State != string(collector.Enrolled) {
		return badResponse()
	}
	id, e := DecodeID(r.EnrollmentID)
	if e != nil || len(id) == 0 {
		return badResponse()
	}
	switch collector.AttestationStatus(r.AttestationStatus) {
	case "", collector.AttestationVerified, collector.AttestationClaim:
	default:
		return badResponse()
	}
	return nil
}
func (r ArtifactReceipt) Validate() error {
	if r.Schema != Schema || !validEncodedID(r.CollectorID) || !validEncodedID(r.ArtifactID) || r.Generation <= 0 || !validTime(r.ReceivedAt) {
		return badResponse()
	}
	return nil
}
func (r ObservationReceipt) Validate() error {
	id, e := DecodeID(r.ObservationID)
	if r.Schema != Schema || e != nil || !collector.ValidObservationID(id) || !validEncodedID(r.CollectorID) || !validEncodedID(r.ArtifactID) || r.Generation <= 0 || !validTime(r.ReceivedAt) {
		return badResponse()
	}
	return nil
}

// Validate checks wire integrity and that the observation ID is the one its
// content derives. It does not establish that the content is true.
func (r ObservationRecord) Validate() error {
	id, e := DecodeID(r.ObservationID)
	if r.Schema != Schema || e != nil || r.Generation <= 0 || !validTime(r.ReceivedAt) || !collector.ValidDigest(r.ArtifactDigest) {
		return badResponse()
	}
	if r.Status != StatusActive && r.Status != StatusQuarantined {
		return badResponse()
	}
	o, e := DecodeObservation(r.Observation)
	if e != nil || o.ID() != id {
		return badResponse()
	}
	return nil
}
