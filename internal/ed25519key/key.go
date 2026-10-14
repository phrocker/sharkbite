// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package ed25519key validates Ed25519 public keys that a verifier will trust.
//
// crypto/ed25519.Verify is cofactorless and accepts any 32-byte encoding that
// decodes to a curve point. If a small-order point is pinned as a key, anyone
// can forge a signature without a private key. For example, with the identity
// as the key, R = the base point encoding and S = 1 verifies for every message.
// Every honestly generated key is a*B, which lies in the prime-order subgroup,
// so Validate refuses everything else:
//   - a non-canonical encoding;
//   - a small-order point;
//   - a point with a torsion component.
package ed25519key

import (
	"bytes"
	"crypto/ed25519"
	"errors"

	"filippo.io/edwards25519"
)

var (
	ErrLength        = errors.New("ed25519 public key has the wrong length")
	ErrEncoding      = errors.New("ed25519 public key is not a canonical point encoding")
	ErrSmallOrder    = errors.New("ed25519 public key has small order")
	ErrNotPrimeOrder = errors.New("ed25519 public key is not in the prime-order subgroup")
)

// lMinusOne is the group order minus one (0 - 1 mod l).
var lMinusOne = func() *edwards25519.Scalar {
	one := [32]byte{1}
	s, e := edwards25519.NewScalar().SetCanonicalBytes(one[:])
	if e != nil {
		panic(e)
	}
	return edwards25519.NewScalar().Subtract(edwards25519.NewScalar(), s)
}()

// Validate returns nil only for a canonical encoding of a point in the
// prime-order subgroup other than the identity.
func Validate(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrLength
	}
	p, e := new(edwards25519.Point).SetBytes(pub)
	// SetBytes accepts some non-canonical encodings; re-encoding exposes them.
	if e != nil || !bytes.Equal(p.Bytes(), pub) {
		return ErrEncoding
	}
	identity := edwards25519.NewIdentityPoint()
	if new(edwards25519.Point).MultByCofactor(p).Equal(identity) == 1 {
		return ErrSmallOrder
	}
	// [l]P = [l-1]P + P is the identity exactly when P has no torsion.
	lp := new(edwards25519.Point).ScalarMult(lMinusOne, p)
	if lp.Add(lp, p).Equal(identity) != 1 {
		return ErrNotPrimeOrder
	}
	return nil
}
