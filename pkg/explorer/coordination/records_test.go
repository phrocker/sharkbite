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

package coordination

import (
	"bytes"
	"encoding/binary"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "update coordination golden fixtures")

func testDigest(value string) Digest { return Sum([]byte(value)) }

func fixtureRoot() TxnRootV3 {
	return TxnRootV3{
		State: StatePrepared, LogicalDigest: testDigest("logical"),
		TokenHash: testDigest("token"), Epoch: 17, Owner: OwnerID{0, 0xff, 'o'},
		Fence: 4, ManifestRoot: testDigest("manifest"), ChunkCount: 2,
		TotalEntries: 3, TotalEncodedBytes: 987, LPARTs: []LPART{{0}, {'z'}},
		ResultIdentities: []ResultIdentity{
			{Kind: []byte("document"), ID: []byte{0, 0xff}},
			{Kind: []byte("revision"), ID: []byte("r1")},
		},
		StateGeneration: 8, WriterAuthorityGeneration: 9, RetentionGeneration: 10,
	}
}

func fixtureReservation() ReservationV1 {
	return ReservationV1{
		ReservationGeneration: 2, Epoch: 17, TXN: TXN{0, 0xff, 't'}, Owner: OwnerID("owner"),
		LeaseUntil: time.Date(2026, 8, 27, 13, 59, 20, 123456789, time.UTC),
		Fence:      4, AuthorityGeneration: 9, State: StateWriting,
	}
}

func fixtureOutcome(t *testing.T) EpochOutcomeV1 {
	t.Helper()
	value, err := NewEpochOutcomeV1(17, TXN{0, 0xff, 't'}, StateCommitted, 4, 9)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixtureCheckpoint(t *testing.T) FrontierCheckpointV1 {
	t.Helper()
	value, err := NewFrontierCheckpointV1(
		17,
		time.Date(2026, 8, 27, 13, 59, 20, 987654321, time.UTC),
		testDigest("previous"),
		testDigest("outcomes"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestReservationSuccessorGeneration(t *testing.T) {
	previous := fixtureReservation()
	previous.State = StateWriting
	next := previous
	next.ReservationGeneration++
	next.State = StateCommitted
	if err := ValidateReservationSuccessor(previous, next); err != nil {
		t.Fatal(err)
	}
	next.ReservationGeneration++
	if err := ValidateReservationSuccessor(previous, next); err == nil {
		t.Fatal("skipped reservation generation accepted")
	}
	next = previous
	next.ReservationGeneration++
	next.TXN = TXN("other")
	if err := ValidateReservationSuccessor(previous, next); err == nil {
		t.Fatal("changed reservation identity accepted")
	}
	previous.ReservationGeneration = Generation(^uint64(0) >> 1)
	next = previous
	if err := ValidateReservationSuccessor(previous, next); err == nil {
		t.Fatal("exhausted reservation generation accepted")
	}
}

func golden(t *testing.T, name string, encoded []byte) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s differs from golden:\ngot  %x\nwant %x", name, encoded, want)
	}
	return want
}

func TestRecordGoldensAndRoundTrips(t *testing.T) {
	tests := []struct {
		name      string
		marshal   func() ([]byte, error)
		unmarshal func([]byte) (any, error)
		want      any
	}{
		{"txn_root_v3.bin", func() ([]byte, error) { return MarshalTxnRootV3(fixtureRoot()) },
			func(data []byte) (any, error) { return UnmarshalTxnRootV3(data) }, fixtureRoot()},
		{"reservation_v1.bin", func() ([]byte, error) { return MarshalReservationV1(fixtureReservation()) },
			func(data []byte) (any, error) { return UnmarshalReservationV1(data) }, fixtureReservation()},
		{"epoch_outcome_v1.bin", func() ([]byte, error) { return MarshalEpochOutcomeV1(fixtureOutcome(t)) },
			func(data []byte) (any, error) { return UnmarshalEpochOutcomeV1(data) }, fixtureOutcome(t)},
		{"frontier_checkpoint_v1.bin", func() ([]byte, error) { return MarshalFrontierCheckpointV1(fixtureCheckpoint(t)) },
			func(data []byte) (any, error) { return UnmarshalFrontierCheckpointV1(data) }, fixtureCheckpoint(t)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := test.marshal()
			if err != nil {
				t.Fatal(err)
			}
			for range 5 {
				repeated, err := test.marshal()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(encoded, repeated) {
					t.Fatal("marshal is not deterministic")
				}
			}
			decoded, err := test.unmarshal(golden(t, test.name, encoded))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, test.want) {
				t.Fatalf("round trip differs:\ngot  %#v\nwant %#v", decoded, test.want)
			}
		})
	}
}

func TestEnvelopeRejectsCorruptionAndShapeErrors(t *testing.T) {
	valid, err := MarshalTxnRootV3(fixtureRoot())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated": valid[:len(valid)-1],
		"trailing":  append(append([]byte(nil), valid...), 0),
		"magic":     append([]byte(nil), valid...),
		"kind":      append([]byte(nil), valid...),
		"version":   append([]byte(nil), valid...),
		"checksum":  append([]byte(nil), valid...),
	}
	cases["magic"][0] ^= 1
	binary.BigEndian.PutUint16(cases["kind"][len(envelopeMagic):], uint16(KindReservation))
	binary.BigEndian.PutUint16(cases["version"][len(envelopeMagic)+2:], 99)
	cases["checksum"][envelopeHeaderSize+1] ^= 1
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := UnmarshalTxnRootV3(data); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestUnmarshalRejectsNoncanonicalOrdering(t *testing.T) {
	root := fixtureRoot()
	root.LPARTs[0], root.LPARTs[1] = root.LPARTs[1], root.LPARTs[0]
	encoded, err := marshalEnvelope(
		KindTxnRoot, VersionTxnRootV3, MaxRootBytes,
		func(e *encoder) { encodeTxnRoot(e, root) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalTxnRootV3(encoded); err == nil {
		t.Fatal("noncanonical LPART ordering accepted")
	}
}

func TestTxnRootCanonicalOrderingAndBounds(t *testing.T) {
	root := fixtureRoot()
	root.LPARTs[0], root.LPARTs[1] = root.LPARTs[1], root.LPARTs[0]
	reordered, err := MarshalTxnRootV3(root)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := MarshalTxnRootV3(fixtureRoot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reordered, canonical) {
		t.Fatal("LPART insertion order changed encoded bytes")
	}
	root = fixtureRoot()
	root.ResultIdentities[0], root.ResultIdentities[1] =
		root.ResultIdentities[1], root.ResultIdentities[0]
	reordered, err = MarshalTxnRootV3(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reordered, canonical) {
		t.Fatal("result identity insertion order changed encoded bytes")
	}
	root = fixtureRoot()
	root.LPARTs = make([]LPART, MaxLPARTs+1)
	for i := range root.LPARTs {
		root.LPARTs[i] = LPART{byte(i + 1)}
	}
	if _, err := MarshalTxnRootV3(root); err == nil {
		t.Fatal("expected LPART bound rejection")
	}
	root = fixtureRoot()
	root.TotalEntries = MaxManifestEntries + 1
	if _, err := MarshalTxnRootV3(root); err == nil {
		t.Fatal("expected manifest total bound rejection")
	}
}

func TestTxnTransitions(t *testing.T) {
	chain := []TxnState{
		StateAbsent, StateClaimed, StatePlanned, StateGuardsAcquired,
		StateEpochReserved, StateWriting, StateVerified, StatePrepared,
		StateCommitted,
	}
	for i := 1; i < len(chain); i++ {
		if err := ValidateTransition(chain[i-1], chain[i]); err != nil {
			t.Fatalf("valid transition rejected: %v", err)
		}
	}
	for _, terminal := range []TxnState{StateAborted, StateConflicted, StatePoisoned} {
		if err := ValidateTransition(StateWriting, terminal); err != nil {
			t.Fatalf("terminal transition rejected: %v", err)
		}
		if err := ValidateTransition(terminal, StateClaimed); err == nil {
			t.Fatal("terminal state transitioned")
		}
	}
	for _, pair := range [][2]TxnState{
		{StateAbsent, StatePlanned}, {StateClaimed, StateWriting},
		{StatePrepared, StateVerified}, {StateCommitted, StateCommitted},
	} {
		if err := ValidateTransition(pair[0], pair[1]); err == nil {
			t.Fatalf("illegal transition accepted: %v", pair)
		}
	}
	if TxnState(255).ValidatePersisted() == nil {
		t.Fatal("unknown/RETRYABLE-like state accepted")
	}
}

func TestOutcomeAndCheckpointDigestAndSuccessor(t *testing.T) {
	outcome := fixtureOutcome(t)
	outcome.Digest[0] ^= 1
	if err := outcome.Validate(); err == nil {
		t.Fatal("outcome digest mismatch accepted")
	}
	first := fixtureCheckpoint(t)
	second, err := NewFrontierCheckpointV1(
		18, first.VisibleAt.Add(time.Nanosecond), first.Digest, testDigest("next"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckpointSuccessor(first, second); err != nil {
		t.Fatal(err)
	}
	second.Frontier = first.Frontier
	second.Digest = second.ComputeDigest()
	if err := ValidateCheckpointSuccessor(first, second); err == nil {
		t.Fatal("non-increasing frontier accepted")
	}
	second.Frontier = first.Frontier + 1
	second.VisibleAt = first.VisibleAt.Add(-time.Nanosecond)
	second.Digest = second.ComputeDigest()
	if err := ValidateCheckpointSuccessor(first, second); err == nil {
		t.Fatal("decreasing visible_at accepted")
	}
}

func TestReservationStateAndTimestampValidation(t *testing.T) {
	reservation := fixtureReservation()
	reservation.State = StateClaimed
	if err := reservation.Validate(); err == nil {
		t.Fatal("pre-reservation state accepted")
	}
	reservation = fixtureReservation()
	reservation.LeaseUntil = reservation.LeaseUntil.In(time.FixedZone("zero", 0))
	if err := reservation.Validate(); err == nil {
		t.Fatal("non-UTC timestamp representation accepted")
	}
}

func TestEarlyCountBoundBeforeAllocation(t *testing.T) {
	valid, err := MarshalTxnRootV3(fixtureRoot())
	if err != nil {
		t.Fatal(err)
	}
	// Locate LPART count after the fixed fields and owner.
	payload := valid[envelopeHeaderSize : len(valid)-checksumSize]
	d := &decoder{data: payload}
	_ = d.byte("state")
	_ = d.take("digests", 64)
	_ = d.optionalEpoch("epoch")
	_ = d.bytes("owner", MaxOwnerBytes, true)
	_ = d.take("fixed", 32+4+8+8+8)
	offset := d.offset
	forged := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(forged[envelopeHeaderSize+offset:], MaxLPARTs+1)
	sum := Sum(forged[:len(forged)-checksumSize])
	copy(forged[len(forged)-checksumSize:], sum[:])
	if _, err := UnmarshalTxnRootV3(forged); err == nil {
		t.Fatal("forged count accepted")
	}

	root := fixtureRoot()
	root.TotalEncodedBytes = ^uint64(0)
	if _, err := MarshalTxnRootV3(root); err == nil {
		t.Fatal("overflowing total encoded bytes accepted")
	}
}
