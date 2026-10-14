// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package ed25519key

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"

	"filippo.io/edwards25519"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func ff(first, last byte) []byte {
	b := bytes.Repeat([]byte{0xff}, 32)
	b[0], b[31] = first, last
	return b
}

// order8 is a canonical order-8 point; its multiples are all eight torsion
// points.
const order8 = "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05"

func torsion(t *testing.T) [][]byte {
	t.Helper()
	gen, e := new(edwards25519.Point).SetBytes(mustHex(t, order8))
	if e != nil {
		t.Fatal(e)
	}
	var out [][]byte
	p := edwards25519.NewIdentityPoint()
	for range 8 {
		out = append(out, p.Bytes())
		p = new(edwards25519.Point).Add(p, gen)
	}
	return out
}

func TestRealKeysAccepted(t *testing.T) {
	for seed := range 16 {
		private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(seed)}, ed25519.SeedSize))
		if e := Validate(private.Public().(ed25519.PublicKey)); e != nil {
			t.Fatalf("seed %d: %v", seed, e)
		}
	}
}

// The forgery this package exists to stop: with the identity as the key, R =
// the base point and S = 1 verify for any message under Go's ed25519.Verify.
func TestIdentityKeyForgesWithoutValidate(t *testing.T) {
	identity := make([]byte, 32)
	identity[0] = 1
	signature := append(edwards25519.NewGeneratorPoint().Bytes(), make([]byte, 32)...)
	signature[32] = 1
	if !ed25519.Verify(identity, []byte("any message"), signature) {
		t.Skip("crypto/ed25519 now refuses small-order keys itself")
	}
	if !errors.Is(Validate(identity), ErrSmallOrder) {
		t.Fatal("identity key accepted")
	}
}

func TestEverySmallOrderPointRefused(t *testing.T) {
	points := torsion(t)
	seen := map[string]bool{}
	for i, p := range points {
		seen[string(p)] = true
		if e := Validate(p); !errors.Is(e, ErrSmallOrder) {
			t.Errorf("torsion point %d (%x): %v", i, p, e)
		}
	}
	if len(seen) != 8 {
		t.Fatalf("expected 8 distinct torsion points, got %d", len(seen))
	}
	// The libsodium blocklist, canonical and non-canonical, each also with the
	// sign bit flipped.
	list := [][]byte{
		make([]byte, 32), append([]byte{1}, make([]byte, 31)...),
		mustHex(t, order8), mustHex(t, "c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"),
		ff(0xec, 0x7f), ff(0xed, 0x7f), ff(0xee, 0x7f),
	}
	for _, b := range list {
		flipped := bytes.Clone(b)
		flipped[31] ^= 0x80
		for _, k := range [][]byte{b, flipped} {
			if Validate(k) == nil {
				t.Errorf("blocklisted encoding %x accepted", k)
			}
		}
	}
}

func TestNonCanonicalEncodingsRefused(t *testing.T) {
	rows := map[string][]byte{
		"identity with sign bit": append(append([]byte{1}, make([]byte, 30)...), 0x80),
		"order 2 with sign bit":  ff(0xec, 0xff),
		"y = p":                  ff(0xed, 0x7f),
		"y = p + 1":              ff(0xee, 0x7f),
	}
	for name, k := range rows {
		if e := Validate(k); !errors.Is(e, ErrEncoding) {
			t.Errorf("%s: %v", name, e)
		}
	}
	// Every y in [p, 2^255) is non-canonical, whatever point it decodes to.
	for i := range 19 {
		k := ff(0xed+byte(i), 0x7f)
		if e := Validate(k); !errors.Is(e, ErrEncoding) {
			t.Errorf("y = p + %d: %v", i, e)
		}
	}
	// A y with no matching x is not a point.
	offCurve := false
	for y := byte(2); y < 64; y++ {
		k := append([]byte{y}, make([]byte, 31)...)
		if _, e := new(edwards25519.Point).SetBytes(k); e != nil {
			offCurve = true
			if !errors.Is(Validate(k), ErrEncoding) {
				t.Errorf("off-curve y=%d accepted", y)
			}
		}
	}
	if !offCurve {
		t.Fatal("no off-curve row exercised")
	}
	if !errors.Is(Validate(make([]byte, 31)), ErrLength) {
		t.Fatal("short key accepted")
	}
}

func TestMixedOrderKeyRefused(t *testing.T) {
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	a, _ := new(edwards25519.Point).SetBytes(private.Public().(ed25519.PublicKey))
	tp, _ := new(edwards25519.Point).SetBytes(mustHex(t, order8))
	mixed := new(edwards25519.Point).Add(a, tp).Bytes()
	if e := Validate(mixed); !errors.Is(e, ErrNotPrimeOrder) {
		t.Fatalf("mixed-order key: %v", e)
	}
}
