// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package executorattest verifies attestation statements presented by fleet
// executors and records the latest verified result per principal and executor
// ref.
//
// It is the verifier half of #446 slice 3. The fleet consumes it through
// interfaces it defines (fleet.ExecutorAttestations, fleet.AttestationTrust,
// fleet.AttestationPresenter), adapted in cmd/shoal-explore-web; see
// docs/executor-attestation.md, "Enforcement".
//
// A statement is an Ed25519 signature, under a signing context distinct from
// collectorattest's, over canonical JSON binding the caller's authorization
// domain, subject and client, the executor ref, an image digest, an optional
// provenance digest, a validity window and a nonce derived from the
// presentation's idempotency key. The trust file pins, per executor ref, the
// verifiers (an ID bound to a key) and the accepted digests.
//
// Residual trust, as for collectorattest: a statement proves only that the
// holder of an operator key vouched for these bindings. It does not prove what
// code runs; the operator's signing process and key custody are trusted.
package executorattest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// Format names the statement envelope.
	Format         = "shoal-executor-statement-v1"
	signingContext = "shoal-executor-statement-v1\x00"
	nonceTag       = "shoal-executor-attestation-nonce-v1"
	idTag          = "shoal-executor-attestation-id-v1"
	maxEvidence    = 64 << 10
)

// ErrRefused is the one public error for every verification refusal. The
// reason is available to audit through ReasonOf and never to the presenter.
var ErrRefused = errors.New("executor attestation refused")

// Reason classifies a refusal for audit.
type Reason string

const (
	ReasonMalformed          Reason = "malformed"
	ReasonNonCanonical       Reason = "non-canonical"
	ReasonUnknownExecutor    Reason = "unknown-executor"
	ReasonUnknownVerifier    Reason = "unknown-verifier"
	ReasonSignature          Reason = "signature"
	ReasonDomainMismatch     Reason = "domain-mismatch"
	ReasonSubjectMismatch    Reason = "subject-mismatch"
	ReasonClientMismatch     Reason = "client-mismatch"
	ReasonExecutorMismatch   Reason = "executor-mismatch"
	ReasonNonceMismatch      Reason = "nonce-mismatch"
	ReasonImageUnpinned      Reason = "image-unpinned"
	ReasonProvenanceUnpinned Reason = "provenance-unpinned"
	ReasonValidity           Reason = "validity"
	ReasonNotYetValid        Reason = "not-yet-valid"
	ReasonExpired            Reason = "expired"
	ReasonRollback           Reason = "rollback"
)

// Refusal is a verification failure. Its message is the public one.
type Refusal struct{ Reason Reason }

func (r *Refusal) Error() string { return ErrRefused.Error() }
func (r *Refusal) Unwrap() error { return ErrRefused }

func refuse(r Reason) error { return &Refusal{Reason: r} }

// ReasonOf returns the audit reason of a refusal.
func ReasonOf(err error) (Reason, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason, true
	}
	return "", false
}

// Public maps a refusal to the opaque ErrRefused and leaves other errors as
// they are.
func Public(err error) error {
	if _, ok := ReasonOf(err); ok {
		return ErrRefused
	}
	return err
}

// Principal is who an attestation is for. It comes from the authentication
// decision, never from a request body.
type Principal struct {
	Domain   []byte
	Subject  shoal.ID
	ClientID shoal.ID
}

func (p Principal) validate() error {
	if len(p.Domain) == 0 || len(p.Domain) > 4096 || shoal.ValidateRequiredID("subject", p.Subject) != nil || shoal.ValidateRequiredID("client", p.ClientID) != nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid attestation principal")
	}
	return nil
}

// Expectation is what the server, not the statement, says it must bind. Key is
// the presentation's idempotency key; Now is the server clock.
type Expectation struct {
	Principal   Principal
	ExecutorRef string
	Key         []byte
	Now         time.Time
}

func (w Expectation) validate() error {
	if e := w.Principal.validate(); e != nil {
		return e
	}
	if !validRef(w.ExecutorRef) || len(w.Key) == 0 || len(w.Key) > shoal.MaxIDBytes || w.Now.IsZero() {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid attestation expectation")
	}
	return nil
}

// Nonce is the nonce a statement must carry for one presentation. It binds the
// principal, the executor ref and the presentation's idempotency key, so a
// statement cannot be replayed under another key or for another principal.
func Nonce(p Principal, ref string, key []byte) string {
	h := sha256.New()
	var n [8]byte
	for _, v := range [][]byte{[]byte(nonceTag), p.Domain, []byte(p.Subject), []byte(p.ClientID), []byte(ref), key} {
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write(v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Statement is the signed body. AuthorizationDomain, Subject and ClientID are
// base64url (no padding) like every wire ID; digests are "sha256:<hex>".
type Statement struct {
	AuthorizationDomain string    `json:"authorization_domain"`
	Subject             string    `json:"subject"`
	ClientID            string    `json:"client_id"`
	ExecutorRef         string    `json:"executor_ref"`
	ImageDigest         string    `json:"image_digest"`
	ProvenanceDigest    string    `json:"provenance_digest,omitempty"`
	IssuedAt            time.Time `json:"issued_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	Nonce               string    `json:"nonce"`
}

// StatementFor fills the bound fields of a statement for w. Operator tooling
// and tests use it.
func StatementFor(w Expectation, image string, issued, expires time.Time) Statement {
	enc := base64.RawURLEncoding.EncodeToString
	return Statement{
		AuthorizationDomain: enc(w.Principal.Domain), Subject: enc([]byte(w.Principal.Subject)), ClientID: enc([]byte(w.Principal.ClientID)),
		ExecutorRef: w.ExecutorRef, ImageDigest: image, IssuedAt: issued, ExpiresAt: expires,
		Nonce: Nonce(w.Principal, w.ExecutorRef, w.Key),
	}
}

type envelope struct {
	VerifierID string          `json:"verifier_id"`
	Statement  json.RawMessage `json:"statement"`
	Signature  string          `json:"signature"`
}

// SignStatement produces evidence for s signed by the verifier id with key.
// Operators run this where the private key lives.
func SignStatement(id shoal.ID, key ed25519.PrivateKey, s Statement) ([]byte, error) {
	body, e := json.Marshal(s)
	if e != nil {
		return nil, e
	}
	signature := ed25519.Sign(key, append([]byte(signingContext), body...))
	return json.Marshal(envelope{VerifierID: string(id), Statement: body, Signature: base64.RawURLEncoding.EncodeToString(signature)})
}

// Result is a verified statement.
type Result struct {
	// AttestationID is content-addressed over the verifier and the signed bytes.
	AttestationID    shoal.ID
	VerifierID       shoal.ID
	KeyDigest        string
	ExecutorRef      string
	ImageDigest      string
	ProvenanceDigest string
	IssuedAt         time.Time
	ExpiresAt        time.Time
}

func attestationID(verifier shoal.ID, body []byte) shoal.ID {
	h := sha256.New()
	var n [8]byte
	for _, v := range [][]byte{[]byte(idTag), []byte(verifier), body} {
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write(v)
	}
	return shoal.ID("exattest:" + hex.EncodeToString(h.Sum(nil)))
}

// Verify checks evidence against want under trust. A malformed expectation is
// an invalid-argument error; every other failure is a *Refusal.
func Verify(trust *Trust, want Expectation, evidence []byte) (Result, error) {
	if e := want.validate(); e != nil {
		return Result{}, e
	}
	executor, ok := trust.executor(want.ExecutorRef)
	if !ok {
		return Result{}, refuse(ReasonUnknownExecutor)
	}
	var env envelope
	if len(evidence) > maxEvidence || strictDecode(evidence, &env) != nil {
		return Result{}, refuse(ReasonMalformed)
	}
	verifier, ok := executor.verifier(shoal.ID(env.VerifierID))
	if !ok {
		return Result{}, refuse(ReasonUnknownVerifier)
	}
	signature, e := base64.RawURLEncoding.Strict().DecodeString(env.Signature)
	if e != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(verifier.PublicKey, append([]byte(signingContext), env.Statement...), signature) {
		return Result{}, refuse(ReasonSignature)
	}
	// Only signed bytes are interpreted, and only in their canonical form.
	var s Statement
	if strictDecode(env.Statement, &s) != nil {
		return Result{}, refuse(ReasonMalformed)
	}
	if canonical, e := json.Marshal(s); e != nil || string(canonical) != string(env.Statement) {
		return Result{}, refuse(ReasonNonCanonical)
	}
	if r := bindings(s, want); r != "" {
		return Result{}, refuse(r)
	}
	if !ValidDigest(s.ImageDigest) || (s.ProvenanceDigest != "" && !ValidDigest(s.ProvenanceDigest)) {
		return Result{}, refuse(ReasonMalformed)
	}
	if r := pinned(executor, s.ImageDigest, s.ProvenanceDigest); r != "" {
		return Result{}, refuse(r)
	}
	issued, expires := s.IssuedAt.UTC().Round(0), s.ExpiresAt.UTC().Round(0)
	now := want.Now.UTC().Round(0)
	if !expires.After(issued) || expires.Sub(issued) > verifier.MaxValidity {
		return Result{}, refuse(ReasonValidity)
	}
	// Skew tolerates a signer clock ahead of ours on issued_at only; expiry is
	// judged strictly by the server clock.
	if now.Add(verifier.ClockSkew).Before(issued) {
		return Result{}, refuse(ReasonNotYetValid)
	}
	if !now.Before(expires) {
		return Result{}, refuse(ReasonExpired)
	}
	return Result{
		AttestationID: attestationID(verifier.ID, env.Statement), VerifierID: verifier.ID, KeyDigest: keyDigest(verifier.PublicKey),
		ExecutorRef: want.ExecutorRef, ImageDigest: s.ImageDigest, ProvenanceDigest: s.ProvenanceDigest, IssuedAt: issued, ExpiresAt: expires,
	}, nil
}

func bindings(s Statement, want Expectation) Reason {
	decode := func(v string) (string, bool) {
		b, e := base64.RawURLEncoding.Strict().DecodeString(v)
		return string(b), e == nil
	}
	if d, ok := decode(s.AuthorizationDomain); !ok || d != string(want.Principal.Domain) {
		return ReasonDomainMismatch
	}
	if d, ok := decode(s.Subject); !ok || shoal.ID(d) != want.Principal.Subject {
		return ReasonSubjectMismatch
	}
	if d, ok := decode(s.ClientID); !ok || shoal.ID(d) != want.Principal.ClientID {
		return ReasonClientMismatch
	}
	if s.ExecutorRef != want.ExecutorRef {
		return ReasonExecutorMismatch
	}
	if s.Nonce == "" || s.Nonce != Nonce(want.Principal, want.ExecutorRef, want.Key) {
		return ReasonNonceMismatch
	}
	return ""
}

// pinned checks digests against executor's current pins. Provenance is
// required and pinned only when provenance digests are configured.
func pinned(executor ExecutorTrust, image, provenance string) Reason {
	if !contains(executor.ImageDigests, image) {
		return ReasonImageUnpinned
	}
	if len(executor.ProvenanceDigests) > 0 && !contains(executor.ProvenanceDigests, provenance) {
		return ReasonProvenanceUnpinned
	}
	return ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
