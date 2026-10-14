// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorattest

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func keys(t *testing.T, seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune(seed)), ed25519.SeedSize)))
	return private.Public().(ed25519.PublicKey), private
}

func setup(t *testing.T) (Set, ed25519.PrivateKey, Expectation, Statement) {
	t.Helper()
	public, private := keys(t, 'a')
	v, e := NewEd25519Statement(Ed25519Config{VerifierID: "operator-key:1", PublicKey: public, MaxValidity: time.Hour, ClockSkew: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	set, e := NewSet(v)
	if e != nil {
		t.Fatal(e)
	}
	want := Expectation{CollectorID: "collector:tail", Nonce: collector.EnrollNonce("collector:tail", []byte("key-1")), Now: now}
	s := Statement{CollectorID: base64.RawURLEncoding.EncodeToString([]byte(want.CollectorID)), ImageDigest: strings.Repeat("c", 64), IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * time.Minute), Nonce: want.Nonce}
	return set, private, want, s
}

func report(t *testing.T, key ed25519.PrivateKey, s Statement) collector.AttestationReport {
	t.Helper()
	evidence, e := SignStatement(key, s)
	if e != nil {
		t.Fatal(e)
	}
	return collector.AttestationReport{Kind: Ed25519StatementKind, Format: Ed25519StatementFormat, Evidence: evidence}
}

func TestEd25519StatementVerifies(t *testing.T) {
	set, private, want, s := setup(t)
	result, e := set.Evaluate(context.Background(), report(t, private, s), want)
	if e != nil {
		t.Fatal(e)
	}
	if result.Status != collector.AttestationVerified || result.Subject != want.CollectorID || result.Digest != s.ImageDigest || result.VerifierID != "operator-key:1" || result.ID() == "" {
		t.Fatalf("unexpected result %+v", result)
	}
}

func TestEd25519StatementRefusalsAreNeverDowngraded(t *testing.T) {
	set, private, want, s := setup(t)
	_, otherKey := keys(t, 'b')
	cases := map[string]func() (collector.AttestationReport, Expectation){
		"forged signature": func() (collector.AttestationReport, Expectation) { return report(t, otherKey, s), want },
		"expired": func() (collector.AttestationReport, Expectation) {
			w := want
			w.Now = s.ExpiresAt
			return report(t, private, s), w
		},
		"not yet valid": func() (collector.AttestationReport, Expectation) {
			x := s
			x.IssuedAt, x.ExpiresAt = now.Add(10*time.Minute), now.Add(20*time.Minute)
			return report(t, private, x), want
		},
		"validity too long": func() (collector.AttestationReport, Expectation) {
			x := s
			x.ExpiresAt = x.IssuedAt.Add(2 * time.Hour)
			return report(t, private, x), want
		},
		"wrong subject": func() (collector.AttestationReport, Expectation) {
			w := want
			w.CollectorID = "collector:other"
			return report(t, private, s), w
		},
		// A statement minted for one enrollment key replayed under another.
		"nonce replay": func() (collector.AttestationReport, Expectation) {
			w := want
			w.Nonce = collector.EnrollNonce(want.CollectorID, []byte("key-2"))
			return report(t, private, s), w
		},
		"bad image digest": func() (collector.AttestationReport, Expectation) {
			x := s
			x.ImageDigest = "sha256:abc"
			return report(t, private, x), want
		},
		"wrong format": func() (collector.AttestationReport, Expectation) {
			r := report(t, private, s)
			r.Format = "other"
			return r, want
		},
		"tampered statement": func() (collector.AttestationReport, Expectation) {
			r := report(t, private, s)
			r.Evidence = []byte(strings.Replace(string(r.Evidence), strings.Repeat("c", 64), strings.Repeat("d", 64), 1))
			return r, want
		},
		"non-canonical signed statement": func() (collector.AttestationReport, Expectation) {
			body, _ := json.Marshal(s)
			body = append([]byte(" "), body...)
			signature := ed25519.Sign(private, append([]byte(signingContext), body...))
			evidence, _ := json.Marshal(envelope{Statement: body, Signature: base64.RawURLEncoding.EncodeToString(signature)})
			return collector.AttestationReport{Kind: Ed25519StatementKind, Format: Ed25519StatementFormat, Evidence: evidence}, want
		},
		"unknown envelope field": func() (collector.AttestationReport, Expectation) {
			r := report(t, private, s)
			r.Evidence = append(r.Evidence[:len(r.Evidence)-1], []byte(`,"extra":1}`)...)
			return r, want
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			r, w := build()
			result, e := set.Evaluate(context.Background(), r, w)
			if !errors.Is(e, ErrRefused) || result.Status != "" {
				t.Fatalf("got %+v %v, want refusal", result, e)
			}
		})
	}
}

func TestUnknownKindIsRecordedAsClaim(t *testing.T) {
	set, _, want, _ := setup(t)
	r := collector.AttestationReport{Kind: "nitro", Format: "cbor", Evidence: []byte("opaque")}
	result, e := set.Evaluate(context.Background(), r, want)
	if e != nil {
		t.Fatal(e)
	}
	if result.Status != collector.AttestationClaim || result.VerifierID != "" || result.Subject != want.CollectorID || result.ID() != "" || result.Validate() != nil {
		t.Fatalf("claim recorded as %+v", result)
	}
	// With no verifiers configured even the Ed25519 kind is only a claim.
	empty, _ := NewSet()
	result, e = empty.Evaluate(context.Background(), collector.AttestationReport{Kind: Ed25519StatementKind, Format: Ed25519StatementFormat, Evidence: []byte("x")}, want)
	if e != nil || result.Status != collector.AttestationClaim {
		t.Fatalf("unconfigured kind: %+v %v", result, e)
	}
}

func TestVerifierConfigurationIsValidated(t *testing.T) {
	public, _ := keys(t, 'a')
	for name, c := range map[string]Ed25519Config{
		"no id":       {PublicKey: public, MaxValidity: time.Hour},
		"short key":   {VerifierID: "v", PublicKey: public[:8], MaxValidity: time.Hour},
		"no validity": {VerifierID: "v", PublicKey: public},
		"huge skew":   {VerifierID: "v", PublicKey: public, MaxValidity: time.Hour, ClockSkew: 2 * time.Hour},
	} {
		if _, e := NewEd25519Statement(c); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	v, _ := NewEd25519Statement(Ed25519Config{VerifierID: shoal.ID("v"), PublicKey: public, MaxValidity: time.Hour})
	if _, e := NewSet(v, v); e == nil {
		t.Fatal("duplicate kind accepted")
	}
}
