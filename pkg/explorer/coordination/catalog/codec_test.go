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
 */

package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"
)

func TestCatalogTimestampRoundTripsSupportedUTCYears(t *testing.T) {
	for _, value := range []time.Time{
		time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(1900, 2, 3, 4, 5, 6, 7, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2500, 6, 7, 8, 9, 10, 11, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
	} {
		encoded, err := marshalCounter(1, value, []byte("reservation"))
		if err != nil {
			t.Fatalf("marshal %s: %v", value, err)
		}
		_, decoded, _, err := unmarshalCounter(encoded)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", value, err)
		}
		if !decoded.Equal(value) {
			t.Fatalf("round trip = %s, want %s", decoded, value)
		}
		canonical, err := marshalCounter(1, decoded, []byte("reservation"))
		if err != nil || !bytes.Equal(canonical, encoded) {
			t.Fatalf("noncanonical timestamp encoding for %s: %v", value, err)
		}
	}
}

func TestCatalogTimestampRejectsInvalidAndNonUTCEncodings(t *testing.T) {
	if _, err := marshalCounter(
		1,
		time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("offset", 3600)),
		[]byte("reservation"),
	); err == nil {
		t.Fatal("non-UTC timestamp accepted")
	}
	encoded, err := marshalCounter(
		1, time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), []byte("reservation"),
	)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(encoded[25:29], uint32(time.Second))
	rechecksum(encoded)
	if _, _, _, err := unmarshalCounter(encoded); err == nil {
		t.Fatal("invalid nanoseconds accepted")
	}
}

func TestCatalogFenceRejectsNoncanonicalBoolean(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	fence := PolicyFence{
		Request:          fenceRequest(now, 2, "owner", 5),
		RecordGeneration: 1,
		UpdatedAt:        now,
		Active:           true,
	}
	encoded, err := marshalFence(fence)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalFence(encoded)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := marshalFence(decoded)
	if err != nil || !bytes.Equal(canonical, encoded) {
		t.Fatalf("valid fence is not fixed-point canonical: %v", err)
	}
	encoded[len(encoded)-sha256.Size-1] = 2
	rechecksum(encoded)
	if _, err := unmarshalFence(encoded); err == nil {
		t.Fatal("checksum-valid active flag 2 accepted")
	}
}

func rechecksum(encoded []byte) {
	sum := sha256.Sum256(encoded[:len(encoded)-sha256.Size])
	copy(encoded[len(encoded)-sha256.Size:], sum[:])
}
