/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorerfleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

func effectDescriptor(effects fleet.Effects) fleet.Descriptor {
	schema := json.RawMessage(`{"type":"object"}`)
	return fleet.Descriptor{
		ID: "agent", Generation: 1, Subject: "owner", Actor: "operator",
		AuthorizationDomain: []byte("domain"),
		Scopes: []fleet.Scope{{
			SourceID: []byte("source"), PolicyID: []byte("policy"),
		}},
		ExecutorRef: "executor",
		Capabilities: []fleet.Capability{{Name: "deploy", Actions: []fleet.Action{{
			Name: "ship", Effects: effects,
			InputSchema: schema, OutputSchema: schema,
		}}}},
		LeaseExpiresAt: time.Unix(1, 0).UTC(),
		UpdatedAt:      time.Unix(1, 0).UTC(),
	}
}

func sameEffects(a, b fleet.Effects) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEffectsSurviveTheDurableCodec is the regression test for the bug that
// made the effect boundary bypassable by persisting it. The field was validated
// at registration and then dropped on encode, so a stored external action read
// back as evidence-only and the resolution check saw the zero value.
func TestEffectsSurviveTheDurableCodec(t *testing.T) {
	for _, effects := range []fleet.Effects{
		nil,
		{fleet.EffectReadsCorpus},
		{fleet.EffectMutatesExternal},
		{fleet.EffectEgressesContent, fleet.EffectReadsCorpus},
		{
			fleet.EffectEgressesContent,
			fleet.EffectMutatesExternal,
			fleet.EffectReadsCorpus,
		},
	} {
		descriptor := effectDescriptor(effects)
		encoded, err := encodeDescriptor(descriptor, [sha256.Size]byte{})
		if err != nil {
			t.Fatalf("encode %v: %v", effects, err)
		}
		decoded, _, err := decodeDescriptor(encoded)
		if err != nil {
			t.Fatalf("decode %v: %v", effects, err)
		}
		got := decoded.Capabilities[0].Actions[0].Effects
		if !sameEffects(got, effects) {
			t.Fatalf("effects %v did not survive the codec, got %v", effects, got)
		}
	}
}

// TestVersionTwoExternalRecordsKeepTheirMeaning is the upgrade that matters.
//
// A version-2 record holds one class string. The external class deliberately
// kept its wire value across the move to a set, so a descriptor registered
// before this change decodes to exactly {EffectMutatesExternal} — keeping both
// its meaning and, through Effects.digestBytes, its mutation identity. A
// heartbeat or revoke retry spanning the upgrade must not be rejected as a
// divergent mutation.
func TestVersionTwoExternalRecordsKeepTheirMeaning(t *testing.T) {
	encoded, err := encodeDescriptor(
		effectDescriptor(fleet.Effects{fleet.EffectMutatesExternal}),
		[sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyVersionTwo(t, encoded)
	decoded, _, err := decodeDescriptor(legacy)
	if err != nil {
		t.Fatalf("a version-2 record must still decode: %v", err)
	}
	got := decoded.Capabilities[0].Actions[0].Effects
	want := fleet.Effects{fleet.EffectMutatesExternal}
	if !sameEffects(got, want) {
		t.Fatalf("version-2 external decoded as %v, want %v", got, want)
	}
}

// TestVersionTwoEvidenceRecordsDecodeAsAnEmptySet covers the other version-2
// value: the empty string was the evidence zero value and must not become a
// one-element set holding "".
func TestVersionTwoEvidenceRecordsDecodeAsAnEmptySet(t *testing.T) {
	encoded, err := encodeDescriptor(effectDescriptor(nil), [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// A version-3 encoding of an empty set writes a four-byte zero count; a
	// version-2 encoding of the evidence zero value writes a four-byte zero
	// string length. The bytes are identical, so only the version word changes.
	legacy := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint16(legacy[0:2], 2)
	decoded, _, err := decodeDescriptor(legacy)
	if err != nil {
		t.Fatalf("a version-2 record must still decode: %v", err)
	}
	if got := decoded.Capabilities[0].Actions[0].Effects; len(got) != 0 {
		t.Fatalf("version-2 evidence decoded as %v, want the empty set", got)
	}
}

// TestVersionOneRecordsDecodeAsAnEmptySet proves the compatibility claim rather
// than asserting it. A record written before the effect class existed has no
// field for it, and must decode with the zero value it was written with rather
// than being rejected.
func TestVersionOneRecordsDecodeAsAnEmptySet(t *testing.T) {
	encoded, err := encodeDescriptor(effectDescriptor(nil), [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyVersionOne(t, encoded)
	decoded, _, err := decodeDescriptor(legacy)
	if err != nil {
		t.Fatalf("a version-1 record must still decode: %v", err)
	}
	if got := decoded.Capabilities[0].Actions[0].Effects; len(got) != 0 {
		t.Fatalf("version-1 effects = %v, want the empty set", got)
	}
}

// TestAnOversizedEffectCountIsRefused bounds what a malformed or hostile record
// can make the decoder allocate. The taxonomy has three classes.
func TestAnOversizedEffectCountIsRefused(t *testing.T) {
	encoded, err := encodeDescriptor(effectDescriptor(nil), [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	marker := append([]byte{0, 0, 0, 4}, []byte("ship")...)
	index := bytes.Index(encoded, marker)
	if index < 0 {
		t.Fatal("could not locate the action name in the encoding")
	}
	tampered := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint32(tampered[index+len(marker):], 1<<20)
	_, _, err = decodeDescriptor(tampered)
	if err == nil {
		t.Fatal("a record declaring a million effects was accepted")
	}
	// The error must be the bound, not a read failure further in. Without the
	// bound the decoder still fails — it runs out of buffer — but only after
	// allocating a slice for a million entries, so asserting "some error"
	// would pass whether or not the bound exists.
	if !strings.Contains(err.Error(), "more effects than the taxonomy has") {
		t.Fatalf("oversized count was rejected by a later read rather than the "+
			"bound, so a hostile record can still force the allocation: %v", err)
	}
}

// legacyVersionTwo turns a version-3 encoding of a single-class descriptor into
// the version-2 layout: version word set to 2, and the four-byte set count
// removed so the class string follows the action name directly.
func legacyVersionTwo(t *testing.T, encoded []byte) []byte {
	t.Helper()
	marker := append([]byte{0, 0, 0, 4}, []byte("ship")...)
	index := bytes.Index(encoded, marker)
	if index < 0 {
		t.Fatal("could not locate the action name in the encoding")
	}
	countAt := index + len(marker)
	if !bytes.Equal(encoded[countAt:countAt+4], []byte{0, 0, 0, 1}) {
		t.Fatalf("expected a one-element set count at %d", countAt)
	}
	legacy := make([]byte, 0, len(encoded)-4)
	legacy = append(legacy, encoded[:countAt]...)
	legacy = append(legacy, encoded[countAt+4:]...)
	binary.BigEndian.PutUint16(legacy[0:2], 2)
	return legacy
}

// legacyVersionOne turns a version-3 encoding of a descriptor declaring
// nothing into the version-1 layout: version word set to 1, and the four zero
// bytes of the empty set count removed.
func legacyVersionOne(t *testing.T, encoded []byte) []byte {
	t.Helper()
	// The effect set immediately follows the action name, which is "ship".
	marker := append([]byte{0, 0, 0, 4}, []byte("ship")...)
	index := bytes.Index(encoded, marker)
	if index < 0 {
		t.Fatal("could not locate the action name in the encoding")
	}
	effectAt := index + len(marker)
	if !bytes.Equal(encoded[effectAt:effectAt+4], []byte{0, 0, 0, 0}) {
		t.Fatalf("expected an empty effect set count at %d", effectAt)
	}
	legacy := make([]byte, 0, len(encoded)-4)
	legacy = append(legacy, encoded[:effectAt]...)
	legacy = append(legacy, encoded[effectAt+4:]...)
	binary.BigEndian.PutUint16(legacy[0:2], 1)
	return legacy
}
