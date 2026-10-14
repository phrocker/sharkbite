// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorattest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"filippo.io/edwards25519"

	"github.com/phrocker/shoal-oss/pkg/collector"
)

// Small-order keys let anyone forge: under the identity key, R = base point and
// S = 1 verify for any message with crypto/ed25519's cofactorless Verify.
func TestSmallOrderVerifierKeysRefused(t *testing.T) {
	ff := func(first, last byte) []byte {
		b := []byte(strings.Repeat("\xff", 32))
		b[0], b[31] = first, last
		return b
	}
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
	for name, key := range rows {
		if _, e := NewEd25519Statement(Ed25519Config{VerifierID: "v", PublicKey: key, MaxValidity: time.Hour}); e == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	public, _ := keys(t, 'a')
	if _, e := NewEd25519Statement(Ed25519Config{VerifierID: "v", PublicKey: public, MaxValidity: time.Hour}); e != nil {
		t.Fatalf("real key refused: %v", e)
	}
}

func TestEnvelopeDuplicateKeysRefused(t *testing.T) {
	set, private, want, s := setup(t)
	evidence := string(report(t, private, s).Evidence)
	for name, raw := range map[string]string{
		"duplicate signature":  strings.Replace(evidence, `"signature":`, `"signature":"AAAA","signature":`, 1),
		"case alias statement": strings.Replace(evidence, `"statement"`, `"Statement"`, 1),
	} {
		if raw == evidence {
			t.Fatal(name, "not mutated")
		}
		r := collector.AttestationReport{Kind: Ed25519StatementKind, Format: Ed25519StatementFormat, Evidence: []byte(raw)}
		if _, e := set.Evaluate(context.Background(), r, want); !errors.Is(e, ErrRefused) {
			t.Errorf("%s: %v", name, e)
		}
	}
}
