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
)

func fixtureEntry(index int, padding int) ManifestEntry {
	return ManifestEntry{
		Table: []byte("documents"), Row: []byte{byte(index >> 8), byte(index), 0, 0xff},
		ColumnFamily: []byte("o"), ColumnQualifier: []byte("document"),
		EpochSlot: EpochSlotContent, ValueLength: uint32(padding),
		ValueDigest: testDigest("value"), LPART: LPART("partition"),
		CopyGeneration: 3, VisibilityDigest: testDigest("visibility"),
		LogicalDigest: testDigest("logical"), PhysicalCopyDigest: testDigest("physical"),
	}
}

func TestManifestGoldenRoundTripAndVerification(t *testing.T) {
	chunks, err := ChunkManifest([]ManifestEntry{fixtureEntry(0, 7), fixtureEntry(1, 8)})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	encoded, err := MarshalManifestChunkV2(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalManifestChunkV2(golden(t, "manifest_chunk_v2.bin", encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, chunks[0]) {
		t.Fatal("manifest chunk round trip differs")
	}
	summary, err := VerifyManifest(chunks)
	if err != nil {
		t.Fatal(err)
	}
	root := fixtureRoot()
	root.ChunkCount = summary.ChunkCount
	root.TotalEntries = summary.TotalEntries
	root.TotalEncodedBytes = summary.TotalEncodedBytes
	root.ManifestRoot = summary.RootDigest
	if err := VerifyManifestRoot(root, chunks); err != nil {
		t.Fatal(err)
	}
}

func TestManifestCountAndByteBoundaries(t *testing.T) {
	entries := make([]ManifestEntry, MaxChunkEntries)
	for i := range entries {
		entries[i] = fixtureEntry(i, 1)
	}
	chunks, err := ChunkManifest(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || len(chunks[0].Entries) != MaxChunkEntries {
		t.Fatalf("4096-entry boundary split unexpectedly: %#v", len(chunks))
	}
	entries = append(entries, fixtureEntry(MaxChunkEntries, 1))
	chunks, err = ChunkManifest(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || len(chunks[1].Entries) != 1 {
		t.Fatalf("4097-entry manifest did not split 4096/1: %d", len(chunks))
	}

	large := make([]ManifestEntry, 80)
	for i := range large {
		large[i] = fixtureEntry(i, MaxManifestValueBytes)
		large[i].Row = append(large[i].Row, bytes.Repeat([]byte{'x'}, 15_000)...)
	}
	chunks, err = ChunkManifest(large)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatal("1 MiB boundary did not split the manifest")
	}
	for _, chunk := range chunks {
		encoded, err := MarshalManifestChunkV2(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > MaxChunkBytes {
			t.Fatal("chunk exceeds 1 MiB")
		}
	}
}

func TestManifestRejectsChainAndDigestFailures(t *testing.T) {
	entries := make([]ManifestEntry, MaxChunkEntries+1)
	for i := range entries {
		entries[i] = fixtureEntry(i, 1)
	}
	chunks, err := ChunkManifest(entries)
	if err != nil {
		t.Fatal(err)
	}
	reordered := []ManifestChunkV2{chunks[1], chunks[0]}
	if _, err := VerifyManifest(reordered); err == nil {
		t.Fatal("reordered chunks accepted")
	}
	missing := chunks[1:]
	if _, err := VerifyManifest(missing); err == nil {
		t.Fatal("missing first chunk accepted")
	}
	duplicate := []ManifestChunkV2{chunks[0], chunks[0]}
	if _, err := VerifyManifest(duplicate); err == nil {
		t.Fatal("duplicate chunk accepted")
	}
	broken := append([]ManifestChunkV2(nil), chunks...)
	broken[0].LogicalEntriesDigest[0] ^= 1
	if _, err := VerifyManifest(broken); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	broken = append([]ManifestChunkV2(nil), chunks...)
	broken[1].PreviousDigest[0] ^= 1
	if _, err := VerifyManifest(broken); err == nil {
		t.Fatal("chain mismatch accepted")
	}
}

func TestManifestRejectsUnorderedAndDuplicateEntries(t *testing.T) {
	first := fixtureEntry(0, 1)
	second := fixtureEntry(1, 1)
	if _, err := ChunkManifest([]ManifestEntry{second, first}); err == nil {
		t.Fatal("unordered manifest entries accepted")
	}
	if _, err := ChunkManifest([]ManifestEntry{first, first}); err == nil {
		t.Fatal("duplicate manifest entries accepted")
	}

	chunks, err := ChunkManifest([]ManifestEntry{first, second})
	if err != nil {
		t.Fatal(err)
	}
	chunks[0].Entries[0], chunks[0].Entries[1] = chunks[0].Entries[1], chunks[0].Entries[0]
	chunks[0].LogicalEntriesDigest = entryDigest(chunks[0].Entries, true)
	chunks[0].PhysicalEntriesDigest = entryDigest(chunks[0].Entries, false)
	if _, err := VerifyManifest(chunks); err == nil {
		t.Fatal("unordered encoded manifest entries accepted")
	}
}

func TestManifestRejectsCorruptionAndTruncation(t *testing.T) {
	chunks, err := ChunkManifest([]ManifestEntry{fixtureEntry(0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalManifestChunkV2(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), encoded...)
	corrupt[envelopeHeaderSize+20] ^= 1
	if _, err := UnmarshalManifestChunkV2(corrupt); err == nil {
		t.Fatal("corruption accepted")
	}
	if _, err := UnmarshalManifestChunkV2(encoded[:len(encoded)-10]); err == nil {
		t.Fatal("truncation accepted")
	}
}

func TestManifestEntryDoesNotStoreValueBytes(t *testing.T) {
	entryType := reflect.TypeFor[ManifestEntry]()
	if _, exists := entryType.FieldByName("Value"); exists {
		t.Fatal("manifest entries must describe values by length/digest, not store values")
	}
}

func TestChunkManifestSizesEachEntryOnce(t *testing.T) {
	entries := make([]ManifestEntry, MaxChunkEntries)
	for i := range entries {
		entries[i] = fixtureEntry(i, MaxManifestValueBytes)
		entries[i].Row = append(entries[i].Row, bytes.Repeat([]byte{'x'}, 15_000)...)
	}
	calls := 0
	chunks, err := chunkManifestWithSizer(entries, func(entry ManifestEntry) (int, error) {
		calls++
		return manifestEntryEncodedSize(entry)
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != len(entries) {
		t.Fatalf("entry sizing calls = %d, want %d", calls, len(entries))
	}
	if len(chunks) < 2 {
		t.Fatalf("near-large entries produced only %d chunk", len(chunks))
	}
}

func TestChunkManifestRejectsSingleOversizedEntry(t *testing.T) {
	entry := fixtureEntry(0, 1)
	_, err := chunkManifestWithSizer([]ManifestEntry{entry}, func(ManifestEntry) (int, error) {
		return MaxChunkBytes, nil
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("single manifest entry")) {
		t.Fatalf("oversized entry error = %v", err)
	}
}
