// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package collectorattest verifies runtime attestation reports presented by
// collectors at enrollment.
//
// A report whose kind has a configured verifier is either verified or
// refused; it is never downgraded to a claim. A report whose kind has no
// verifier is recorded as a claim, which is not attestation and never yields
// an attestation ID.
//
// Residual trust: the one verifier here, Ed25519Statement, checks a statement
// signed by an operator-held key. It proves that whoever holds that key vouched
// for the collector ID, image digest and validity window of this enrollment.
// It does not prove which code is running: the operator's signing process and
// key custody are trusted. Hardware and code attestation (Nitro, SGX/TDX,
// cosign) are later verifiers behind the same interface.
package collectorattest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/internal/ed25519key"
	"github.com/phrocker/shoal-oss/internal/strictjson"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ErrRefused reports a definite verification failure.
var ErrRefused = errors.New("attestation refused")

// Expectation is what the server, not the report, says the report must bind.
type Expectation struct {
	CollectorID shoal.ID
	Nonce       string
	Now         time.Time
}

// Verifier verifies one report kind. It returns a verified result or an
// error; it must never return a claim.
type Verifier interface {
	Kind() string
	Verify(context.Context, collector.AttestationReport, Expectation) (collector.AttestationResult, error)
}

// Set dispatches reports to verifiers by kind.
type Set struct{ verifiers map[string]Verifier }

func NewSet(verifiers ...Verifier) (Set, error) {
	s := Set{verifiers: map[string]Verifier{}}
	for _, v := range verifiers {
		if v == nil || v.Kind() == "" || s.verifiers[v.Kind()] != nil {
			return Set{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid or duplicate attestation verifier")
		}
		s.verifiers[v.Kind()] = v
	}
	return s, nil
}

// Evaluate returns the result to record for report, or ErrRefused.
func (s Set) Evaluate(ctx context.Context, report collector.AttestationReport, want Expectation) (collector.AttestationResult, error) {
	if report.Validate() != nil {
		return collector.AttestationResult{}, ErrRefused
	}
	v, ok := s.verifiers[report.Kind]
	if !ok {
		return collector.ClaimFor(want.CollectorID, report), nil
	}
	result, e := v.Verify(ctx, report, want)
	if e != nil || result.Status != collector.AttestationVerified || result.Kind != report.Kind || result.Subject != want.CollectorID || result.Validate() != nil {
		return collector.AttestationResult{}, ErrRefused
	}
	return result, nil
}

const (
	Ed25519StatementKind   = "ed25519-statement"
	Ed25519StatementFormat = "shoal-collector-statement-v1"
	signingContext         = "shoal-collector-statement-v1\x00"
)

// Statement is the signed body of an Ed25519 statement. CollectorID is
// base64url like every wire ID; ImageDigest is lowercase hex SHA-256; Nonce is
// collector.EnrollNonce for the enrollment it authorizes.
type Statement struct {
	CollectorID string    `json:"collector_id"`
	ImageDigest string    `json:"image_digest"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Nonce       string    `json:"nonce"`
}

type envelope struct {
	Statement json.RawMessage `json:"statement"`
	Signature string          `json:"signature"`
}

// SignStatement produces report evidence for s. Operators run this where the
// private key lives; it is here for tests and operator tooling.
func SignStatement(key ed25519.PrivateKey, s Statement) ([]byte, error) {
	body, e := json.Marshal(s)
	if e != nil {
		return nil, e
	}
	signature := ed25519.Sign(key, append([]byte(signingContext), body...))
	return json.Marshal(envelope{Statement: body, Signature: base64.RawURLEncoding.EncodeToString(signature)})
}

// Ed25519Config configures an Ed25519 statement verifier.
type Ed25519Config struct {
	VerifierID  shoal.ID
	PublicKey   ed25519.PublicKey
	MaxValidity time.Duration
	ClockSkew   time.Duration
}

type ed25519Statement struct{ config Ed25519Config }

func NewEd25519Statement(c Ed25519Config) (Verifier, error) {
	if shoal.ValidateRequiredID("verifier ID", c.VerifierID) != nil || ed25519key.Validate(c.PublicKey) != nil || c.MaxValidity <= 0 || c.ClockSkew < 0 || c.ClockSkew > time.Hour {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "invalid Ed25519 statement verifier")
	}
	c.PublicKey = bytes.Clone(c.PublicKey)
	return ed25519Statement{c}, nil
}

func (ed25519Statement) Kind() string { return Ed25519StatementKind }

// strictDecode refuses duplicate keys and case-insensitive key matches at
// every level, unknown fields and trailing data.
func strictDecode(raw []byte, out any) error { return strictjson.Decode(raw, out) }

func (v ed25519Statement) Verify(ctx context.Context, report collector.AttestationReport, want Expectation) (collector.AttestationResult, error) {
	var zero collector.AttestationResult
	if ctx.Err() != nil || report.Kind != Ed25519StatementKind || report.Format != Ed25519StatementFormat || want.Now.IsZero() {
		return zero, ErrRefused
	}
	var env envelope
	if strictDecode(report.Evidence, &env) != nil {
		return zero, ErrRefused
	}
	signature, e := base64.RawURLEncoding.Strict().DecodeString(env.Signature)
	if e != nil || len(signature) != ed25519.SignatureSize {
		return zero, ErrRefused
	}
	if !ed25519.Verify(v.config.PublicKey, append([]byte(signingContext), env.Statement...), signature) {
		return zero, ErrRefused
	}
	// Only signed bytes are interpreted, and only in their canonical form.
	var s Statement
	if strictDecode(env.Statement, &s) != nil {
		return zero, ErrRefused
	}
	if canonical, e := json.Marshal(s); e != nil || !bytes.Equal(canonical, env.Statement) {
		return zero, ErrRefused
	}
	subject, e := base64.RawURLEncoding.Strict().DecodeString(s.CollectorID)
	if e != nil || shoal.ID(subject) != want.CollectorID || s.Nonce == "" || s.Nonce != want.Nonce {
		return zero, ErrRefused
	}
	issued, expires := s.IssuedAt.UTC().Round(0), s.ExpiresAt.UTC().Round(0)
	now := want.Now.UTC()
	if !collector.ValidDigest(s.ImageDigest) || !expires.After(issued) || expires.Sub(issued) > v.config.MaxValidity || now.Add(v.config.ClockSkew).Before(issued) || !now.Before(expires) {
		return zero, ErrRefused
	}
	return collector.AttestationResult{Status: collector.AttestationVerified, Kind: Ed25519StatementKind, VerifierID: v.config.VerifierID, Subject: want.CollectorID, Digest: s.ImageDigest, IssuedAt: issued, ExpiresAt: expires}, nil
}
