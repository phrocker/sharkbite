// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package executorattest

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"filippo.io/edwards25519"

	"github.com/phrocker/shoal-oss/internal/collectorattest"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

const (
	ref        = "executor:deploy"
	verifierID = shoal.ID("operator-key:1")
)

var (
	image      = "sha256:" + strings.Repeat("c", 64)
	otherImage = "sha256:" + strings.Repeat("d", 64)
	provenance = "sha256:" + strings.Repeat("e", 64)
)

func keys(seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune(seed)), ed25519.SeedSize)))
	return private.Public().(ed25519.PublicKey), private
}

func trustWith(t *testing.T, mutate func(*ExecutorTrust)) *Trust {
	t.Helper()
	public, _ := keys('a')
	x := ExecutorTrust{Verifiers: []VerifierTrust{{ID: verifierID, PublicKey: public, MaxValidity: time.Hour, ClockSkew: time.Minute}}, ImageDigests: []string{image}}
	if mutate != nil {
		mutate(&x)
	}
	trust, e := NewTrust(map[string]ExecutorTrust{ref: x})
	if e != nil {
		t.Fatal(e)
	}
	return trust
}

func expectation() Expectation {
	return Expectation{Principal: Principal{Domain: []byte("domain"), Subject: "worker:1", ClientID: "client"}, ExecutorRef: ref, Key: []byte("present-1"), Now: now}
}

func statement(w Expectation) Statement {
	return StatementFor(w, image, now.Add(-time.Minute), now.Add(30*time.Minute))
}

func sign(t *testing.T, key ed25519.PrivateKey, s Statement) []byte {
	t.Helper()
	evidence, e := SignStatement(verifierID, key, s)
	if e != nil {
		t.Fatal(e)
	}
	return evidence
}

// signBody signs arbitrary bytes as a statement under context.
func signBody(key ed25519.PrivateKey, id shoal.ID, context string, body []byte) []byte {
	signature := ed25519.Sign(key, append([]byte(context), body...))
	// Built by hand: json.Marshal would compact a non-canonical body.
	idJSON, _ := json.Marshal(string(id))
	return []byte(`{"verifier_id":` + string(idJSON) + `,"statement":` + string(body) + `,"signature":"` + base64.RawURLEncoding.EncodeToString(signature) + `"}`)
}

func TestStatementVerifies(t *testing.T) {
	_, private := keys('a')
	want := expectation()
	s := statement(want)
	result, e := Verify(trustWith(t, nil), want, sign(t, private, s))
	if e != nil {
		t.Fatal(e)
	}
	if result.VerifierID != verifierID || result.ImageDigest != image || result.ExecutorRef != ref || !result.IssuedAt.Equal(s.IssuedAt) || !result.ExpiresAt.Equal(s.ExpiresAt) || !strings.HasPrefix(string(result.AttestationID), "exattest:") {
		t.Fatalf("unexpected result %+v", result)
	}
	// Content-addressed: the same statement yields the same ID.
	again, _ := Verify(trustWith(t, nil), want, sign(t, private, s))
	if again.AttestationID != result.AttestationID {
		t.Fatal("attestation ID not content-addressed")
	}
}

func TestVerifierRefusals(t *testing.T) {
	_, private := keys('a')
	_, otherKey := keys('b')
	base := expectation()
	trust := trustWith(t, nil)
	withProvenance := trustWith(t, func(x *ExecutorTrust) { x.ProvenanceDigests = []string{provenance} })
	type c struct {
		trust    *Trust
		want     Expectation
		evidence []byte
		reason   Reason
	}
	mod := func(f func(*Statement)) []byte {
		s := statement(base)
		f(&s)
		return sign(t, private, s)
	}
	other := func(f func(*Expectation)) Expectation {
		w := base
		w.Principal.Domain = []byte("domain")
		f(&w)
		return w
	}
	collectorEvidence := func() []byte {
		// A real collector statement signed by the same operator key.
		evidence, e := collectorattest.SignStatement(private, collectorattest.Statement{CollectorID: "Y29sbGVjdG9y", ImageDigest: strings.Repeat("c", 64), IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "n"})
		if e != nil {
			t.Fatal(e)
		}
		return evidence
	}
	rewrapped := func() []byte {
		var env struct {
			Statement json.RawMessage `json:"statement"`
			Signature string          `json:"signature"`
		}
		_ = json.Unmarshal(collectorEvidence(), &env)
		out, _ := json.Marshal(envelope{VerifierID: string(verifierID), Statement: env.Statement, Signature: env.Signature})
		return out
	}
	canonicalBody, _ := json.Marshal(statement(base))
	cases := map[string]c{
		"forged signature":           {trust, base, sign(t, otherKey, statement(base)), ReasonSignature},
		"tampered statement":         {trust, base, []byte(strings.Replace(string(sign(t, private, statement(base))), strings.Repeat("c", 64), strings.Repeat("d", 64), 1)), ReasonSignature},
		"collector context":          {trust, base, signBody(private, verifierID, "shoal-collector-statement-v1\x00", canonicalBody), ReasonSignature},
		"real collector statement":   {trust, base, rewrapped(), ReasonSignature},
		"raw collector evidence":     {trust, base, collectorEvidence(), ReasonUnknownVerifier},
		"unknown verifier":           {trust, base, signBody(private, "operator-key:9", signingContext, canonicalBody), ReasonUnknownVerifier},
		"unknown executor":           {trust, other(func(w *Expectation) { w.ExecutorRef = "executor:other" }), sign(t, private, statement(base)), ReasonUnknownExecutor},
		"non-canonical whitespace":   {trust, base, signBody(private, verifierID, signingContext, []byte(strings.Replace(string(canonicalBody), `","`, `", "`, 1))), ReasonNonCanonical},
		"non-canonical field order":  {trust, base, signBody(private, verifierID, signingContext, reorder(t, canonicalBody)), ReasonNonCanonical},
		"unknown statement field":    {trust, base, signBody(private, verifierID, signingContext, append(canonicalBody[:len(canonicalBody)-1], []byte(`,"extra":1}`)...)), ReasonMalformed},
		"unknown envelope field":     {trust, base, append(sign(t, private, statement(base))[:len(sign(t, private, statement(base)))-1], []byte(`,"extra":1}`)...), ReasonMalformed},
		"wrong domain":               {trust, other(func(w *Expectation) { w.Principal.Domain = []byte("other") }), sign(t, private, statement(base)), ReasonDomainMismatch},
		"wrong subject":              {trust, other(func(w *Expectation) { w.Principal.Subject = "worker:2" }), sign(t, private, statement(base)), ReasonSubjectMismatch},
		"wrong client":               {trust, other(func(w *Expectation) { w.Principal.ClientID = "client-2" }), sign(t, private, statement(base)), ReasonClientMismatch},
		"wrong executor ref":         {trust, base, mod(func(s *Statement) { s.ExecutorRef = "executor:other" }), ReasonExecutorMismatch},
		"replayed under another key": {trust, other(func(w *Expectation) { w.Key = []byte("present-2") }), sign(t, private, statement(base)), ReasonNonceMismatch},
		"empty nonce":                {trust, base, mod(func(s *Statement) { s.Nonce = "" }), ReasonNonceMismatch},
		"padded subject encoding":    {trust, base, mod(func(s *Statement) { s.Subject = base64.URLEncoding.EncodeToString([]byte("worker:1")) }), ReasonSubjectMismatch},
		"unpinned image":             {trust, base, mod(func(s *Statement) { s.ImageDigest = otherImage }), ReasonImageUnpinned},
		"bare hex image":             {trust, base, mod(func(s *Statement) { s.ImageDigest = strings.Repeat("c", 64) }), ReasonMalformed},
		"provenance missing":         {withProvenance, base, sign(t, private, statement(base)), ReasonProvenanceUnpinned},
		"provenance unpinned":        {withProvenance, base, mod(func(s *Statement) { s.ProvenanceDigest = otherImage }), ReasonProvenanceUnpinned},
		"over max validity":          {trust, base, mod(func(s *Statement) { s.ExpiresAt = s.IssuedAt.Add(time.Hour + time.Second) }), ReasonValidity},
		"expires before issue":       {trust, base, mod(func(s *Statement) { s.ExpiresAt = s.IssuedAt }), ReasonValidity},
		"issued beyond skew": {trust, base, mod(func(s *Statement) {
			s.IssuedAt, s.ExpiresAt = now.Add(time.Minute+time.Second), now.Add(10*time.Minute)
		}), ReasonNotYetValid},
		"expired at expiry":             {trust, base, mod(func(s *Statement) { s.ExpiresAt = now }), ReasonExpired},
		"expired within skew of expiry": {trust, base, mod(func(s *Statement) { s.ExpiresAt = now.Add(-30 * time.Second) }), ReasonExpired},
		"no trust at all":               {nil, base, sign(t, private, statement(base)), ReasonUnknownExecutor},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, e := Verify(tc.trust, tc.want, tc.evidence)
			reason, ok := ReasonOf(e)
			if !ok || reason != tc.reason || result.AttestationID != "" {
				t.Fatalf("got %+v %v (%q), want refusal %q", result, e, reason, tc.reason)
			}
			if !errors.Is(e, ErrRefused) || e.Error() != ErrRefused.Error() || Public(e) != ErrRefused {
				t.Fatalf("refusal leaks detail: %v", e)
			}
		})
	}
}

func reorder(t *testing.T, body []byte) []byte {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(body, &m); e != nil {
		t.Fatal(e)
	}
	out, _ := json.Marshal(m) // map keys marshal sorted, unlike struct order
	if string(out) == string(body) {
		t.Fatal("reorder produced the canonical form")
	}
	return out
}

func TestVerifierAccepts(t *testing.T) {
	_, private := keys('a')
	base := expectation()
	withProvenance := trustWith(t, func(x *ExecutorTrust) { x.ProvenanceDigests = []string{provenance} })
	for name, tc := range map[string]struct {
		trust *Trust
		s     func(*Statement)
	}{
		"issued at skew edge":             {trustWith(t, nil), func(s *Statement) { s.IssuedAt, s.ExpiresAt = now.Add(time.Minute), now.Add(10*time.Minute) }},
		"exactly max validity":            {trustWith(t, nil), func(s *Statement) { s.ExpiresAt = s.IssuedAt.Add(time.Hour) }},
		"one instant before expiry":       {trustWith(t, nil), func(s *Statement) { s.ExpiresAt = now.Add(time.Nanosecond) }},
		"pinned provenance":               {withProvenance, func(s *Statement) { s.ProvenanceDigest = provenance }},
		"provenance carried, none pinned": {trustWith(t, nil), func(s *Statement) { s.ProvenanceDigest = provenance }},
	} {
		t.Run(name, func(t *testing.T) {
			s := statement(base)
			tc.s(&s)
			if _, e := Verify(tc.trust, base, sign(t, private, s)); e != nil {
				t.Fatal(e)
			}
		})
	}
}

// An executor statement presented to the collector verifier under the same
// key is refused: the signing contexts differ.
func TestExecutorStatementRefusedByCollectorVerifier(t *testing.T) {
	public, private := keys('a')
	v, e := collectorattest.NewEd25519Statement(collectorattest.Ed25519Config{VerifierID: verifierID, PublicKey: public, MaxValidity: time.Hour, ClockSkew: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	set, _ := collectorattest.NewSet(v)
	var env envelope
	_ = json.Unmarshal(sign(t, private, statement(expectation())), &env)
	stripped, _ := json.Marshal(struct {
		Statement json.RawMessage `json:"statement"`
		Signature string          `json:"signature"`
	}{env.Statement, env.Signature})
	for _, evidence := range [][]byte{sign(t, private, statement(expectation())), stripped} {
		_, e := set.Evaluate(context.Background(), collector.AttestationReport{Kind: collectorattest.Ed25519StatementKind, Format: collectorattest.Ed25519StatementFormat, Evidence: evidence}, collectorattest.Expectation{CollectorID: "collector", Nonce: "n", Now: now})
		if !errors.Is(e, collectorattest.ErrRefused) {
			t.Fatalf("collector verifier accepted executor evidence: %v", e)
		}
	}
	if signingContext == "shoal-collector-statement-v1\x00" {
		t.Fatal("signing contexts must differ")
	}
}

func TestInvalidExpectationIsNotARefusal(t *testing.T) {
	_, private := keys('a')
	w := expectation()
	w.Key = nil
	_, e := Verify(trustWith(t, nil), w, sign(t, private, statement(expectation())))
	if _, ok := ReasonOf(e); ok || !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) {
		t.Fatalf("got %v", e)
	}
}

func TestNonceIsLengthPrefixed(t *testing.T) {
	p := Principal{Domain: []byte("ab"), Subject: "c", ClientID: "d"}
	q := Principal{Domain: []byte("a"), Subject: "bc", ClientID: "d"}
	if Nonce(p, ref, []byte("k")) == Nonce(q, ref, []byte("k")) {
		t.Fatal("nonce fields are ambiguous")
	}
	// Every bound input changes the nonce.
	base := Nonce(p, ref, []byte("k"))
	for name, n := range map[string]string{
		"domain":  Nonce(Principal{Domain: []byte("ax"), Subject: "c", ClientID: "d"}, ref, []byte("k")),
		"subject": Nonce(Principal{Domain: []byte("ab"), Subject: "x", ClientID: "d"}, ref, []byte("k")),
		"client":  Nonce(Principal{Domain: []byte("ab"), Subject: "c", ClientID: "x"}, ref, []byte("k")),
		"ref":     Nonce(p, "executor:other", []byte("k")),
		"key":     Nonce(p, ref, []byte("x")),
	} {
		if n == base {
			t.Errorf("nonce ignores %s", name)
		}
	}
}

const validTrust = `{"executors":{"executor:deploy":{
 "verifiers":[{"id":"operator-key:1","public_key":"%s","max_validity":"1h","clock_skew":"1m"}],
 "image_digests":["sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"],
 "provenance_digests":["sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"]}}}`

func TestParseTrust(t *testing.T) {
	public, _ := keys('a')
	other, _ := keys('b')
	key := base64.StdEncoding.EncodeToString(public)
	okFile := strings.Replace(validTrust, "%s", key, 1)
	trust, e := ParseTrust([]byte(okFile))
	if e != nil {
		t.Fatal(e)
	}
	if !trust.Configured(ref) || trust.Configured("executor:other") || (*Trust)(nil).Configured(ref) {
		t.Fatal("configured refs wrong")
	}
	verifier := func(id, k, validity, skew string) string {
		return `{"id":"` + id + `","public_key":"` + k + `","max_validity":"` + validity + `","clock_skew":"` + skew + `"}`
	}
	entry := func(verifiers, images string) string {
		return `{"verifiers":[` + verifiers + `],"image_digests":[` + images + `]}`
	}
	img := `"` + image + `"`
	v1 := verifier("operator-key:1", key, "1h", "1m")
	refused := map[string]string{
		"unknown top field":      `{"executors":{},"extra":1}`,
		"unknown entry field":    `{"executors":{"r":{"verifiers":[` + v1 + `],"image_digests":[` + img + `],"extra":1}}}`,
		"unknown verifier field": `{"executors":{"r":` + entry(strings.TrimSuffix(v1, "}")+`,"x":1}`, img) + `}}`,
		"missing executors":      `{}`,
		"trailing data":          `{"executors":{}} {}`,
		"duplicate verifier ID":  `{"executors":{"r":` + entry(v1+","+verifier("operator-key:1", base64.StdEncoding.EncodeToString(other), "1h", "1m"), img) + `}}`,
		"duplicate key":          `{"executors":{"r":` + entry(v1+","+verifier("operator-key:2", key, "1h", "1m"), img) + `}}`,
		"ID bound to two keys":   `{"executors":{"r":` + entry(v1, img) + `,"s":` + entry(verifier("operator-key:1", base64.StdEncoding.EncodeToString(other), "1h", "1m"), img) + `}}`,
		"key bound to two IDs":   `{"executors":{"r":` + entry(v1, img) + `,"s":` + entry(verifier("operator-key:2", key, "1h", "1m"), img) + `}}`,
		"validity over 1h":       `{"executors":{"r":` + entry(verifier("v", key, "61m", "1m"), img) + `}}`,
		"zero validity":          `{"executors":{"r":` + entry(verifier("v", key, "0s", "1m"), img) + `}}`,
		"skew over 1m":           `{"executors":{"r":` + entry(verifier("v", key, "1h", "61s"), img) + `}}`,
		"negative skew":          `{"executors":{"r":` + entry(verifier("v", key, "1h", "-1s"), img) + `}}`,
		"bad duration":           `{"executors":{"r":` + entry(verifier("v", key, "an hour", "1m"), img) + `}}`,
		"short key":              `{"executors":{"r":` + entry(verifier("v", "AAAA", "1h", "1m"), img) + `}}`,
		"empty verifier ID":      `{"executors":{"r":` + entry(verifier("", key, "1h", "1m"), img) + `}}`,
		"no verifiers":           `{"executors":{"r":` + entry("", img) + `}}`,
		"no image digests":       `{"executors":{"r":` + entry(v1, "") + `}}`,
		"bare hex digest":        `{"executors":{"r":` + entry(v1, `"`+strings.Repeat("c", 64)+`"`) + `}}`,
		"uppercase digest":       `{"executors":{"r":` + entry(v1, `"sha256:`+strings.Repeat("C", 64)+`"`) + `}}`,
		"short digest":           `{"executors":{"r":` + entry(v1, `"sha256:abc"`) + `}}`,
		"duplicate image digest": `{"executors":{"r":` + entry(v1, img+","+img) + `}}`,
		"bad provenance digest":  `{"executors":{"r":{"verifiers":[` + v1 + `],"image_digests":[` + img + `],"provenance_digests":["md5:x"]}}}`,
		"empty executor ref":     `{"executors":{"":` + entry(v1, img) + `}}`,
	}
	for name, raw := range refused {
		if _, e := ParseTrust([]byte(raw)); e == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The same verifier (ID and key) may serve several refs.
	if _, e := ParseTrust([]byte(`{"executors":{"r":` + entry(v1, img) + `,"s":` + entry(v1, img) + `}}`)); e != nil {
		t.Fatal(e)
	}
}

// smallOrderKeys are the eight torsion points plus non-canonical encodings of
// small-order points (libsodium's blocklist).
func smallOrderKeys(t *testing.T) map[string][]byte {
	t.Helper()
	ff := func(first, last byte) []byte {
		b := []byte(strings.Repeat("\xff", 32))
		b[0], b[31] = first, last
		return b
	}
	identity := append([]byte{1}, make([]byte, 31)...)
	order8, _ := hex.DecodeString("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05")
	gen, e := new(edwards25519.Point).SetBytes(order8)
	if e != nil {
		t.Fatal(e)
	}
	rows := map[string][]byte{
		"identity with sign bit": append(append([]byte{1}, make([]byte, 30)...), 0x80),
		"y = p":                  ff(0xed, 0x7f),
		"y = p + 1":              ff(0xee, 0x7f),
		"order 2 with sign bit":  ff(0xec, 0xff),
	}
	p := edwards25519.NewIdentityPoint()
	for k := range 8 {
		rows[fmt.Sprintf("torsion %d", k)] = p.Bytes()
		p = new(edwards25519.Point).Add(p, gen)
	}
	if string(rows["torsion 0"]) != string(identity) {
		t.Fatal("torsion 0 is not the identity")
	}
	return rows
}

func TestSmallOrderVerifierKeysRefused(t *testing.T) {
	for name, key := range smallOrderKeys(t) {
		x := ExecutorTrust{Verifiers: []VerifierTrust{{ID: verifierID, PublicKey: key, MaxValidity: time.Hour}}, ImageDigests: []string{image}}
		if _, e := NewTrust(map[string]ExecutorTrust{ref: x}); e == nil {
			t.Errorf("%s: accepted", name)
		}
		file := strings.Replace(validTrust, "%s", base64.StdEncoding.EncodeToString(key), 1)
		if _, e := ParseTrust([]byte(file)); e == nil {
			t.Errorf("%s: accepted from file", name)
		}
	}
	// The forgery the check prevents: under the identity key, R = base point
	// and S = 1 verify for any message.
	identity := smallOrderKeys(t)["torsion 0"]
	signature := append(edwards25519.NewGeneratorPoint().Bytes(), make([]byte, 32)...)
	signature[32] = 1
	body, _ := json.Marshal(statement(expectation()))
	if !ed25519.Verify(identity, append([]byte(signingContext), body...), signature) {
		t.Log("crypto/ed25519 refuses the identity key itself")
	}
}

func TestTrustFileDuplicateAndCaseKeysRefused(t *testing.T) {
	public, _ := keys('a')
	key := base64.StdEncoding.EncodeToString(public)
	v := `{"id":"operator-key:1","public_key":"` + key + `","max_validity":"1h","clock_skew":"1m"}`
	img := `"` + image + `"`
	entry := `{"verifiers":[` + v + `],"image_digests":[` + img + `]}`
	for name, raw := range map[string]string{
		"duplicate executor ref":      `{"executors":{"r":` + entry + `,"r":` + entry + `}}`,
		"duplicate executors key":     `{"executors":{"r":` + entry + `},"executors":{}}`,
		"repeated verifiers key":      `{"executors":{"r":{"verifiers":[` + v + `],"verifiers":[],"image_digests":[` + img + `]}}}`,
		"repeated image_digests key":  `{"executors":{"r":{"verifiers":[` + v + `],"image_digests":[` + img + `],"image_digests":["sha256:` + strings.Repeat("d", 64) + `"]}}}`,
		"repeated verifier field":     `{"executors":{"r":{"verifiers":[` + strings.TrimSuffix(v, "}") + `,"id":"operator-key:2"}],"image_digests":[` + img + `]}}}`,
		"case alias executors":        `{"Executors":{"r":` + entry + `}}`,
		"case alias image_digests":    `{"executors":{"r":{"verifiers":[` + v + `],"Image_Digests":[` + img + `]}}}`,
		"case alias verifier id":      `{"executors":{"r":{"verifiers":[` + strings.Replace(v, `"id"`, `"ID"`, 1) + `],"image_digests":[` + img + `]}}}`,
		"case alias beside exact key": `{"executors":{"r":` + entry + `},"EXECUTORS":{}}`,
	} {
		if _, e := ParseTrust([]byte(raw)); e == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Executor refs are map keys: refs differing only in case are distinct.
	if trust, e := ParseTrust([]byte(`{"executors":{"r":` + entry + `,"R":` + entry + `}}`)); e != nil || !trust.Configured("r") || !trust.Configured("R") {
		t.Fatalf("case-distinct refs: %v", e)
	}
}

func TestEnvelopeDuplicateKeysRefused(t *testing.T) {
	_, private := keys('a')
	want := expectation()
	evidence := string(sign(t, private, statement(want)))
	for name, raw := range map[string]string{
		"duplicate verifier_id": strings.Replace(evidence, `{"verifier_id":"operator-key:1",`, `{"verifier_id":"operator-key:9","verifier_id":"operator-key:1",`, 1),
		"case alias signature":  strings.Replace(evidence, `"signature"`, `"Signature"`, 1),
	} {
		if raw == evidence {
			t.Fatal(name, "not mutated")
		}
		if _, e := Verify(trustWith(t, nil), want, []byte(raw)); !reasonIs(e, ReasonMalformed) {
			t.Errorf("%s: %v", name, e)
		}
	}
}
