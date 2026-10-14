// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package executorattest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/ed25519key"
	"github.com/phrocker/shoal-oss/internal/strictjson"
	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxValidityCeiling bounds every verifier's max_validity.
	MaxValidityCeiling = time.Hour
	// ClockSkewCeiling bounds every verifier's clock_skew.
	ClockSkewCeiling = time.Minute
	// MaxExecutorRefBytes matches the fleet's executor ref bound.
	MaxExecutorRefBytes = executorref.MaxExecutorRefBytes
	maxTrustBytes       = 1 << 20
	digestPrefix        = "sha256:"
)

// VerifierTrust is one operator verifier: an identity bound to an Ed25519
// key. The VerifierID is the signer identity recorded with every result.
type VerifierTrust struct {
	ID          shoal.ID
	PublicKey   ed25519.PublicKey
	MaxValidity time.Duration
	ClockSkew   time.Duration
}

// ExecutorTrust is the trust root for one executor ref.
type ExecutorTrust struct {
	Verifiers         []VerifierTrust
	ImageDigests      []string
	ProvenanceDigests []string
}

// Trust is a validated, immutable trust configuration keyed by executor ref.
// A nil *Trust trusts nothing.
type Trust struct{ executors map[string]ExecutorTrust }

// ValidDigest reports whether s is "sha256:" followed by 64 lowercase hex
// characters.
func ValidDigest(s string) bool {
	h, ok := strings.CutPrefix(s, digestPrefix)
	if !ok {
		return false
	}
	b, e := hex.DecodeString(h)
	return e == nil && len(b) == sha256.Size && hex.EncodeToString(b) == h
}

// validRef bounds a ref looked up at run time (a claim's attestation check,
// an expectation). It is deliberately the old, looser bound and not
// executorref.ValidExecutorRef: a descriptor stored before that rule may name
// a ref it refuses, and looking one up must answer "not attested" (no trust
// root can name it, since NewTrust applies the rule) rather than an argument
// error the claim gate would surface as unavailable.
func validRef(ref string) bool {
	return ref != "" && len(ref) <= MaxExecutorRefBytes && utf8.ValidString(ref)
}

func invalidTrust(why string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid executor attestation trust: "+why)
}

// NewTrust validates and copies executors. Within one ref, verifier IDs and
// keys are unique; across refs, a verifier ID is always bound to the same key
// and a key to the same ID, so the ID names one signer everywhere.
func NewTrust(executors map[string]ExecutorTrust) (*Trust, error) {
	out := &Trust{executors: make(map[string]ExecutorTrust, len(executors))}
	keyOf := map[shoal.ID]string{}
	idOf := map[string]shoal.ID{}
	refs := slices.Sorted(maps.Keys(executors))
	for position, ref := range refs {
		t := executors[ref]
		// The trust file is operator configuration, so it is held to the
		// one executor-reference rule and refused at startup. The error
		// names the ref's position in sorted order, never its bytes.
		if err := executorref.ValidExecutorRef(ref); err != nil {
			return nil, invalidTrust(fmt.Sprintf(
				"executor ref %d of %d (in sorted order): %v",
				position+1, len(refs), err))
		}
		if len(t.Verifiers) == 0 {
			return nil, invalidTrust("no verifiers for " + ref)
		}
		var c ExecutorTrust
		seenID, seenKey := map[shoal.ID]bool{}, map[string]bool{}
		for _, v := range t.Verifiers {
			if shoal.ValidateRequiredID("verifier ID", v.ID) != nil || ed25519key.Validate(v.PublicKey) != nil {
				return nil, invalidTrust("verifier")
			}
			if v.MaxValidity <= 0 || v.MaxValidity > MaxValidityCeiling || v.ClockSkew < 0 || v.ClockSkew > ClockSkewCeiling {
				return nil, invalidTrust("verifier bounds")
			}
			key := string(v.PublicKey)
			if seenID[v.ID] || seenKey[key] {
				return nil, invalidTrust("duplicate verifier")
			}
			seenID[v.ID], seenKey[key] = true, true
			if k, ok := keyOf[v.ID]; ok && k != key {
				return nil, invalidTrust("verifier ID bound to two keys")
			}
			if id, ok := idOf[key]; ok && id != v.ID {
				return nil, invalidTrust("key bound to two verifier IDs")
			}
			keyOf[v.ID], idOf[key] = key, v.ID
			v.PublicKey = bytes.Clone(v.PublicKey)
			c.Verifiers = append(c.Verifiers, v)
		}
		var e error
		if c.ImageDigests, e = digests(t.ImageDigests, true); e != nil {
			return nil, e
		}
		if c.ProvenanceDigests, e = digests(t.ProvenanceDigests, false); e != nil {
			return nil, e
		}
		out.executors[ref] = c
	}
	return out, nil
}

func digests(in []string, required bool) ([]string, error) {
	if len(in) == 0 {
		if required {
			return nil, invalidTrust("no image digests")
		}
		return nil, nil
	}
	out := slices.Clone(in)
	for _, d := range out {
		if !ValidDigest(d) {
			return nil, invalidTrust("digest")
		}
	}
	slices.Sort(out)
	if len(slices.Compact(slices.Clone(out))) != len(out) {
		return nil, invalidTrust("duplicate digest")
	}
	return out, nil
}

type trustFile struct {
	Executors map[string]executorFile `json:"executors"`
}
type executorFile struct {
	Verifiers         []verifierFile `json:"verifiers"`
	ImageDigests      []string       `json:"image_digests"`
	ProvenanceDigests []string       `json:"provenance_digests,omitempty"`
}
type verifierFile struct {
	ID          string `json:"id"`
	PublicKey   string `json:"public_key"`
	MaxValidity string `json:"max_validity"`
	ClockSkew   string `json:"clock_skew"`
}

// ParseTrust reads an operator trust file. Duplicate JSON keys at any level
// (including a repeated executor ref), keys matching a field only up to case,
// unknown fields, trailing data, duplicate verifiers or keys, small-order or
// non-canonical public keys, out-of-bound durations and malformed digests
// are refused. Public keys are standard padded base64 of the 32-byte key;
// durations use Go syntax ("1h", "30s").
func ParseTrust(raw []byte) (*Trust, error) {
	if len(raw) > maxTrustBytes {
		return nil, invalidTrust("file too large")
	}
	var f trustFile
	if strictDecode(raw, &f) != nil || f.Executors == nil {
		return nil, invalidTrust("malformed file")
	}
	executors := make(map[string]ExecutorTrust, len(f.Executors))
	for ref, x := range f.Executors {
		t := ExecutorTrust{ImageDigests: x.ImageDigests, ProvenanceDigests: x.ProvenanceDigests}
		for _, v := range x.Verifiers {
			key, e := base64.StdEncoding.Strict().DecodeString(v.PublicKey)
			if e != nil {
				return nil, invalidTrust("public key")
			}
			maxValidity, e1 := time.ParseDuration(v.MaxValidity)
			skew, e2 := time.ParseDuration(v.ClockSkew)
			if e1 != nil || e2 != nil {
				return nil, invalidTrust("duration")
			}
			t.Verifiers = append(t.Verifiers, VerifierTrust{ID: shoal.ID(v.ID), PublicKey: key, MaxValidity: maxValidity, ClockSkew: skew})
		}
		executors[ref] = t
	}
	return NewTrust(executors)
}

// Configured reports whether ref has a trust root.
func (t *Trust) Configured(ref string) bool {
	_, ok := t.executor(ref)
	return ok
}

func (t *Trust) executor(ref string) (ExecutorTrust, bool) {
	if t == nil {
		return ExecutorTrust{}, false
	}
	x, ok := t.executors[ref]
	return x, ok
}

func (x ExecutorTrust) verifier(id shoal.ID) (VerifierTrust, bool) {
	for _, v := range x.Verifiers {
		if v.ID == id {
			return v, true
		}
	}
	return VerifierTrust{}, false
}

// strictDecode refuses duplicate keys and case-insensitive key matches at
// every level, unknown fields and trailing data.
func strictDecode(raw []byte, out any) error { return strictjson.Decode(raw, out) }

func keyDigest(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return digestPrefix + hex.EncodeToString(sum[:])
}

// Refs returns the configured executor refs, sorted. Hosts use it to check
// that every attested ref is also bound for external work.
func (t *Trust) Refs() []string {
	if t == nil {
		return nil
	}
	refs := make([]string, 0, len(t.executors))
	for ref := range t.executors {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs
}
