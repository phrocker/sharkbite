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
	"reflect"
	"testing"
	"time"
)

func TestEscapedComponentRoundTripPrefixFreeAndOrder(t *testing.T) {
	values := [][]byte{nil, {0}, {0, 0xff}, {'a'}, {'a', 0}, {0xff}}
	for _, value := range values {
		encoded := E(value)
		decoded, used, err := DecodeE(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if used != len(encoded) || !bytes.Equal(decoded, value) {
			t.Fatalf("E round trip failed for %x", value)
		}
		for _, other := range values {
			if !bytes.Equal(value, other) && bytes.HasPrefix(E(other), encoded) {
				t.Fatalf("E(%x) prefixes E(%x)", value, other)
			}
			if sign(bytes.Compare(value, other)) != sign(bytes.Compare(E(value), E(other))) {
				t.Fatalf("E does not preserve ordering for %x and %x", value, other)
			}
		}
	}
	for _, malformed := range [][]byte{{0}, {0, 1}, {'x'}} {
		if _, _, err := DecodeE(malformed); err == nil {
			t.Fatalf("malformed E accepted: %x", malformed)
		}
	}
}

func sign(value int) int {
	if value < 0 {
		return -1
	}
	if value > 0 {
		return 1
	}
	return 0
}

func TestIntegerAndTimeOrdering(t *testing.T) {
	if bytes.Compare(U32(1), U32(2)) >= 0 || bytes.Compare(U64(1), U64(2)) >= 0 {
		t.Fatal("unsigned integer encoding does not sort ascending")
	}
	if bytes.Compare(INV64(1), INV64(2)) <= 0 {
		t.Fatal("inverse epoch encoding does not sort newest first")
	}
	earlier := time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC)
	later := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	encodedEarlier, err := TIME(earlier)
	if err != nil {
		t.Fatal(err)
	}
	encodedLater, err := TIME(later)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(encodedEarlier, encodedLater) >= 0 {
		t.Fatal("TIME does not sort chronologically")
	}
	invEarlier, _ := INV_TIME(earlier)
	invLater, _ := INV_TIME(later)
	if bytes.Compare(invEarlier, invLater) <= 0 {
		t.Fatal("INV_TIME does not sort newest first")
	}
	decoded, err := DecodeTIME(encodedLater)
	if err != nil || !decoded.Equal(later) {
		t.Fatalf("TIME round trip failed: %v %v", decoded, err)
	}
	if _, err := DecodeTIME(append(encodedEarlier, 0)); err == nil {
		t.Fatal("TIME trailing byte accepted")
	}
}

func TestCoordinationRowGoldensAndRoundTrips(t *testing.T) {
	domain := DomainID{0, 0xff, 'd'}
	txn := TXN{'t', 0, 0xff}
	tests := []struct {
		name string
		make func() ([]byte, error)
		want CoordinationRow
	}{
		{"txn_row_v1.bin", func() ([]byte, error) { return TxnRow(domain, txn) },
			CoordinationRow{Kind: RowTxn, Domain: domain, TXN: txn}},
		{"allocator_row_v1.bin", func() ([]byte, error) { return AllocatorRow(domain) },
			CoordinationRow{Kind: RowAllocator, Domain: domain}},
		{"outcome_row_v1.bin", func() ([]byte, error) { return OutcomeRow(domain, 17) },
			CoordinationRow{Kind: RowOutcome, Domain: domain, Epoch: 17}},
		{"manifest_row_v1.bin", func() ([]byte, error) { return ManifestRow(domain, txn, 3) },
			CoordinationRow{Kind: RowManifest, Domain: domain, TXN: txn, ChunkIndex: 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			row = golden(t, test.name, row)
			decoded, err := ParseCoordinationRow(row)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, test.want) {
				t.Fatalf("row round trip differs: %#v != %#v", decoded, test.want)
			}
			if _, err := ParseCoordinationRow(append(append([]byte(nil), row...), 0)); err == nil {
				t.Fatal("trailing row component accepted")
			}
			corrupt := append([]byte(nil), row...)
			corrupt[2] ^= 1
			if _, err := ParseCoordinationRow(corrupt); err == nil {
				t.Fatal("partition band mismatch accepted")
			}
		})
	}
}

func TestOutcomeRowsOrderByEpoch(t *testing.T) {
	domain := DomainID("domain")
	first, _ := OutcomeRow(domain, 1)
	second, _ := OutcomeRow(domain, 2)
	if bytes.Compare(first, second) >= 0 {
		t.Fatal("outcome rows do not sort by increasing epoch")
	}
}

func FuzzEscapedComponentRoundTrip(f *testing.F) {
	f.Add([]byte{0, 0xff, 'x'})
	f.Fuzz(func(t *testing.T, value []byte) {
		decoded, used, err := DecodeE(E(value))
		if err != nil || used != len(E(value)) || !bytes.Equal(decoded, value) {
			t.Fatalf("round trip failed: %x %v", value, err)
		}
	})
}
