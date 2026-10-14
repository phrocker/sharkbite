// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package collector defines protocol-neutral contracts for an external
// evidence collector: its server-assigned registration, the enrollment it
// requests, the raw artifacts it reports and the extractor-versioned
// observations it derives from them.
//
// Nothing here grants authority. Authority policy IDs and control come from
// operator provisioning; an enrollment may only request a subset of them.
// No field carries a model score, and extraction confidence never maps to
// authority. This package imports only the standard library and pkg/shoal so
// extensions can depend on it (see docs/collectors.md).
package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxAuthorityPolicyIDs bounds provisioned and requested authority sets.
	MaxAuthorityPolicyIDs = 32
	// MaxExtractors bounds the extractors one enrollment may declare.
	MaxExtractors = 32
	// MaxPayloadBytes bounds one observation's extracted payload.
	MaxPayloadBytes = 64 * 1024
	// MaxEvidenceBytes bounds one attestation report's evidence.
	MaxEvidenceBytes = 16 * 1024
	// MaxTokenBytes bounds short printable strings: versions, kinds, formats
	// and media types.
	MaxTokenBytes = 128
	// MaxArtifactSize bounds the declared size of a raw artifact (1 TiB).
	MaxArtifactSize = int64(1) << 40
)

// Control mirrors decision.Control by value. It records who controls the
// content the collector observes; it is not a trust level.
type Control string

const (
	CandidateControlled Control = "candidate_controlled"
	ExternalControlled  Control = "external_controlled"
	RegistryControlled  Control = "registry_controlled"
	UnknownControl      Control = "unknown"
)

func (c Control) valid() bool {
	switch c {
	case CandidateControlled, ExternalControlled, RegistryControlled, UnknownControl:
		return true
	}
	return false
}

// Mode records how the collector came by what it reports: by observing it as
// it happened on infrastructure the server trusts, or by importing records
// produced elsewhere. Mode is provisioned, never requested.
type Mode string

const (
	ServerObserved Mode = "server_observed"
	Imported       Mode = "imported"
)

func (m Mode) valid() bool { return m == ServerObserved || m == Imported }

// State is a registration's lifecycle position.
type State string

const (
	Provisioned State = "provisioned"
	Enrolled    State = "enrolled"
	Revoked     State = "revoked"
)

func (s State) valid() bool { return s == Provisioned || s == Enrolled || s == Revoked }

func invalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid collector contract: "+message)
}

func requiredID(name string, id shoal.ID) error {
	if len(id) == 0 || len(id) > shoal.MaxIDBytes || !utf8.ValidString(string(id)) {
		return invalid(name)
	}
	return nil
}

// token accepts short printable ASCII without spaces.
func token(name, value string, required bool) error {
	if value == "" {
		if required {
			return invalid(name + " is required")
		}
		return nil
	}
	if len(value) > MaxTokenBytes {
		return invalid(name + " exceeds limit")
	}
	for _, r := range value {
		if r > unicode.MaxASCII || r <= ' ' || r == 0x7f {
			return invalid(name + " must be printable ASCII")
		}
	}
	return nil
}

// validTime requires a UTC, monotonic-free, representable instant.
func validTime(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Equal(t.Round(0)) && t == t.Round(0) && t.Year() >= 1 && t.Year() <= 9999
}

// ValidDigest reports whether s is a lowercase hex SHA-256 digest.
func ValidDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

// canonicalIDs validates, rejects duplicates and returns a sorted copy.
func canonicalIDs(name string, ids []shoal.ID, required bool) ([]shoal.ID, error) {
	if len(ids) == 0 {
		if required {
			return nil, invalid(name + " is required")
		}
		return nil, nil
	}
	if len(ids) > MaxAuthorityPolicyIDs {
		return nil, invalid(name + " exceeds limit")
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	for i, id := range out {
		if err := requiredID(name, id); err != nil {
			return nil, err
		}
		if i > 0 && out[i-1] == id {
			return nil, invalid(name + " contains duplicates")
		}
	}
	return out, nil
}

// Registration is the server-owned record of one collector. Only operator
// provisioning sets Subject, ClientID, Domain, AuthorityPolicyIDs, Control and
// Mode; the collector cannot change them. Generation increases on revocation,
// so anything recorded under an earlier generation is quarantined.
type Registration struct {
	CollectorID        shoal.ID
	Subject            shoal.ID
	ClientID           shoal.ID
	Domain             []byte
	AuthorityPolicyIDs []shoal.ID
	Control            Control
	Mode               Mode
	Generation         int64
	State              State
}

// Canonical validates r and returns an independently owned canonical copy.
func (r Registration) Canonical() (Registration, error) {
	if err := requiredID("collector ID", r.CollectorID); err != nil {
		return Registration{}, err
	}
	if err := requiredID("subject", r.Subject); err != nil {
		return Registration{}, err
	}
	if len(r.ClientID) > shoal.MaxIDBytes || !utf8.ValidString(string(r.ClientID)) || len(r.Domain) == 0 || len(r.Domain) > shoal.MaxIDBytes {
		return Registration{}, invalid("client or domain")
	}
	ids, err := canonicalIDs("authority policy IDs", r.AuthorityPolicyIDs, true)
	if err != nil {
		return Registration{}, err
	}
	if !r.Control.valid() || !r.Mode.valid() || !r.State.valid() || r.Generation <= 0 {
		return Registration{}, invalid("control, mode, state or generation")
	}
	r.Domain = bytes.Clone(r.Domain)
	r.AuthorityPolicyIDs = ids
	return r, nil
}

// Permits reports whether every requested authority ID was provisioned.
func (r Registration) Permits(requested []shoal.ID) bool {
	for _, id := range requested {
		if !slices.Contains(r.AuthorityPolicyIDs, id) {
			return false
		}
	}
	return true
}

// ExtractorRef names the code that turned an artifact into observations.
// A new version is a new extractor identity for lineage purposes.
type ExtractorRef struct {
	ID      shoal.ID
	Version string
}

func (e ExtractorRef) Validate() error {
	if err := requiredID("extractor ID", e.ID); err != nil {
		return err
	}
	return token("extractor version", e.Version, true)
}

// AttestationReport is what a collector presents about its runtime. It is a
// claim until a configured verifier for its Kind accepts it.
type AttestationReport struct {
	Kind     string
	Format   string
	Evidence []byte
}

func (a AttestationReport) Validate() error {
	if err := token("attestation kind", a.Kind, true); err != nil {
		return err
	}
	if err := token("attestation format", a.Format, true); err != nil {
		return err
	}
	if len(a.Evidence) == 0 || len(a.Evidence) > MaxEvidenceBytes {
		return invalid("attestation evidence size")
	}
	return nil
}

// AttestationStatus distinguishes verified evidence from an unverifiable
// claim. A refused report never produces a stored result.
type AttestationStatus string

const (
	AttestationVerified AttestationStatus = "verified"
	AttestationClaim    AttestationStatus = "claim"
	AttestationRefused  AttestationStatus = "refused"
)

// AttestationResult is the server's record of an attestation report. Only a
// verified result names a VerifierID; a claim records the evidence digest so
// later policy can see what was asserted without treating it as attestation.
type AttestationResult struct {
	Status     AttestationStatus
	Kind       string
	VerifierID shoal.ID
	Subject    shoal.ID
	Digest     string
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

func (a AttestationResult) Validate() error {
	if err := token("attestation kind", a.Kind, true); err != nil {
		return err
	}
	if err := requiredID("attestation subject", a.Subject); err != nil {
		return err
	}
	switch a.Status {
	case AttestationVerified:
		if requiredID("verifier ID", a.VerifierID) != nil || token("attested digest", a.Digest, true) != nil || !validTime(a.IssuedAt) || !validTime(a.ExpiresAt) || !a.ExpiresAt.After(a.IssuedAt) {
			return invalid("verified attestation")
		}
	case AttestationClaim:
		if a.VerifierID != "" || !ValidDigest(a.Digest) || !a.IssuedAt.IsZero() || !a.ExpiresAt.IsZero() {
			return invalid("attestation claim")
		}
	default:
		return invalid("attestation status")
	}
	return nil
}

// ID is a stable identity for a verified result, suitable for
// decision.Source.AttestationID. A claim has no attestation identity.
func (a AttestationResult) ID() shoal.ID {
	if a.Status != AttestationVerified || a.Validate() != nil {
		return ""
	}
	return shoal.ID("attestation:" + digestOf("collector-attestation-v1", string(a.Kind), string(a.VerifierID), string(a.Subject), a.Digest, a.IssuedAt.Format(time.RFC3339Nano), a.ExpiresAt.Format(time.RFC3339Nano)))
}

// ClaimFor records an unverifiable report as a claim about subject.
func ClaimFor(subject shoal.ID, report AttestationReport) AttestationResult {
	sum := sha256.Sum256(report.Evidence)
	return AttestationResult{Status: AttestationClaim, Kind: report.Kind, Subject: subject, Digest: hex.EncodeToString(sum[:])}
}

// EnrollRequest is everything a collector may say about itself. Requested
// authority is checked against provisioning and refused, never trimmed.
type EnrollRequest struct {
	CollectorID                 shoal.ID
	RequestedAuthorityPolicyIDs []shoal.ID
	Extractors                  []ExtractorRef
	Attestation                 *AttestationReport
}

// Canonical validates the request and returns an owned canonical copy with
// sorted authority IDs and extractors.
func (r EnrollRequest) Canonical() (EnrollRequest, error) {
	if err := requiredID("collector ID", r.CollectorID); err != nil {
		return EnrollRequest{}, err
	}
	ids, err := canonicalIDs("requested authority policy IDs", r.RequestedAuthorityPolicyIDs, true)
	if err != nil {
		return EnrollRequest{}, err
	}
	if len(r.Extractors) == 0 || len(r.Extractors) > MaxExtractors {
		return EnrollRequest{}, invalid("extractor count")
	}
	extractors := slices.Clone(r.Extractors)
	slices.SortFunc(extractors, func(a, b ExtractorRef) int {
		if c := strings.Compare(string(a.ID), string(b.ID)); c != 0 {
			return c
		}
		return strings.Compare(a.Version, b.Version)
	})
	for i, e := range extractors {
		if err := e.Validate(); err != nil {
			return EnrollRequest{}, err
		}
		if i > 0 && extractors[i-1] == e {
			return EnrollRequest{}, invalid("duplicate extractor")
		}
	}
	out := EnrollRequest{CollectorID: r.CollectorID, RequestedAuthorityPolicyIDs: ids, Extractors: extractors}
	if r.Attestation != nil {
		if err := r.Attestation.Validate(); err != nil {
			return EnrollRequest{}, err
		}
		a := *r.Attestation
		a.Evidence = bytes.Clone(a.Evidence)
		out.Attestation = &a
	}
	return out, nil
}

// Digest identifies the canonical request so a retry under the same
// idempotency key can be distinguished from a conflicting one.
func (r EnrollRequest) Digest() (string, error) {
	c, err := r.Canonical()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", invalid("request encoding")
	}
	return digestOf("collector-enroll-request-v1", string(b)), nil
}

// EnrollNonce is the nonce an attestation statement must carry for one
// enrollment. It binds the statement to the collector and the enrollment's
// idempotency key, so a statement cannot be replayed under another key.
func EnrollNonce(collectorID shoal.ID, key []byte) string {
	return digestOf("collector-enroll-nonce-v1", string(collectorID), string(key))
}

// HasExtractor reports whether e is declared by the request.
func (r EnrollRequest) HasExtractor(e ExtractorRef) bool { return slices.Contains(r.Extractors, e) }

// ArtifactRef identifies one retained raw artifact by its collector-chosen ID
// and the digest of its bytes. Shoal records the reference; byte retention is
// a later slice.
type ArtifactRef struct {
	ID         shoal.ID
	Digest     string
	Size       int64
	MediaType  string
	ObservedAt time.Time
}

func (a ArtifactRef) Validate() error {
	if err := requiredID("artifact ID", a.ID); err != nil {
		return err
	}
	if !ValidDigest(a.Digest) || a.Size < 0 || a.Size > MaxArtifactSize || !strings.Contains(a.MediaType, "/") || !validTime(a.ObservedAt) {
		return invalid("artifact reference")
	}
	return token("media type", a.MediaType, true)
}

// Disposition reports what an extractor could do with its input.
type Disposition string

const (
	Extracted     Disposition = "extracted"
	LowConfidence Disposition = "low_confidence"
	Unextractable Disposition = "unextractable"
)

// Confidence is extraction confidence: how sure an extractor is that it read
// its input correctly. It is not a judgement about the content and never
// assigns authority.
type Confidence struct {
	Disposition Disposition
	Value       *float64
}

func (c Confidence) Validate() error {
	switch c.Disposition {
	case Extracted, LowConfidence:
		if c.Value != nil && (math.IsNaN(*c.Value) || *c.Value < 0 || *c.Value > 1) {
			return invalid("confidence value")
		}
	case Unextractable:
		if c.Value != nil {
			return invalid("unextractable confidence carries a value")
		}
	default:
		return invalid("confidence disposition")
	}
	return nil
}

// ObservationConfig is what a collector asserts about one extraction.
type ObservationConfig struct {
	CollectorID shoal.ID
	ArtifactID  shoal.ID
	Extractor   ExtractorRef
	Confidence  Confidence
	SubjectID   shoal.ID
	Kind        string
	Payload     []byte
	ObservedAt  time.Time
}

// Observation is an immutable, content-addressed extraction. Its ID binds the
// collector, the artifact, the extractor ID and version and the content, so
// a new extractor version over the same artifact yields a new observation
// with shared artifact lineage and cannot overwrite an earlier one.
type Observation struct {
	id     shoal.ID
	config ObservationConfig
}

// NewObservation validates c and derives the observation identity.
func NewObservation(c ObservationConfig) (Observation, error) {
	c = cloneObservation(c)
	if err := requiredID("collector ID", c.CollectorID); err != nil {
		return Observation{}, err
	}
	if err := requiredID("artifact ID", c.ArtifactID); err != nil {
		return Observation{}, err
	}
	if err := c.Extractor.Validate(); err != nil {
		return Observation{}, err
	}
	if err := c.Confidence.Validate(); err != nil {
		return Observation{}, err
	}
	if len(c.SubjectID) > shoal.MaxIDBytes || !utf8.ValidString(string(c.SubjectID)) {
		return Observation{}, invalid("subject ID")
	}
	if err := token("kind", c.Kind, true); err != nil {
		return Observation{}, err
	}
	if len(c.Payload) > MaxPayloadBytes || (c.Confidence.Disposition == Unextractable && len(c.Payload) != 0) {
		return Observation{}, invalid("payload")
	}
	if !validTime(c.ObservedAt) {
		return Observation{}, invalid("observed time")
	}
	value := ""
	if c.Confidence.Value != nil {
		value = fmt.Sprintf("%x", math.Float64bits(*c.Confidence.Value))
	}
	payload := sha256.Sum256(c.Payload)
	content := digestOf("collector-observation-content-v1", string(c.Confidence.Disposition), value, string(c.SubjectID), c.Kind, hex.EncodeToString(payload[:]), c.ObservedAt.Format(time.RFC3339Nano))
	id := "observation:" + digestOf("collector-observation-v1", string(c.CollectorID), string(c.ArtifactID), string(c.Extractor.ID), c.Extractor.Version, content)
	return Observation{id: shoal.ID(id), config: c}, nil
}

func (o Observation) ID() shoal.ID              { return o.id }
func (o Observation) Config() ObservationConfig { return cloneObservation(o.config) }

// Validate rebuilds the observation and confirms its identity.
func (o Observation) Validate() error {
	rebuilt, err := NewObservation(o.config)
	if err != nil {
		return err
	}
	if rebuilt.id != o.id {
		return invalid("observation identity mismatch")
	}
	return nil
}

// ValidObservationID reports whether id has the derived observation shape.
func ValidObservationID(id shoal.ID) bool {
	s, ok := strings.CutPrefix(string(id), "observation:")
	return ok && ValidDigest(s)
}

func cloneObservation(c ObservationConfig) ObservationConfig {
	// Empty and absent payloads are the same observation.
	c.Payload = bytes.Clone(c.Payload)
	if len(c.Payload) == 0 {
		c.Payload = nil
	}
	if c.Confidence.Value != nil {
		v := *c.Confidence.Value
		if v == 0 {
			v = 0 // Normalize -0 so it cannot mint a second identity.
		}
		c.Confidence.Value = &v
	}
	return c
}

// digestOf hashes length-prefixed parts so no two part lists collide.
func digestOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
