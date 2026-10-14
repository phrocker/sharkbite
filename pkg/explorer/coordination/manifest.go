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
	"crypto/sha256"
	"math"
)

type EpochSlot uint8

const (
	EpochSlotNone EpochSlot = iota
	EpochSlotContent
)

type ManifestEntry struct {
	Table              []byte
	Row                []byte
	ColumnFamily       []byte
	ColumnQualifier    []byte
	EpochSlot          EpochSlot
	ExplicitTimestamp  Epoch
	ValueLength        uint32
	ValueDigest        Digest
	LPART              LPART
	CopyGeneration     Generation
	VisibilityDigest   Digest
	LogicalDigest      Digest
	PhysicalCopyDigest Digest
	IGEN               IGEN
	Family             Family
}

func (m ManifestEntry) Validate() error {
	for _, field := range []struct {
		name  string
		value []byte
	}{
		{"table", m.Table}, {"row", m.Row},
		{"column family", m.ColumnFamily},
		{"column qualifier", m.ColumnQualifier},
	} {
		if err := validateOpaque(field.name, field.value, MaxCoordinateBytes, true); err != nil {
			return err
		}
	}
	if m.ValueLength > MaxManifestValueBytes {
		return invalid("manifest value length exceeds the Accumulo cell bound")
	}
	for _, field := range []struct {
		name   string
		digest Digest
	}{
		{"value digest", m.ValueDigest}, {"logical digest", m.LogicalDigest},
		{"visibility digest", m.VisibilityDigest},
		{"physical copy digest", m.PhysicalCopyDigest},
	} {
		if err := field.digest.Validate(field.name); err != nil {
			return err
		}
	}
	if err := m.LPART.Validate(); err != nil {
		return err
	}
	if err := m.CopyGeneration.Validate(); err != nil {
		return err
	}
	switch m.EpochSlot {
	case EpochSlotContent:
		if m.ExplicitTimestamp != 0 {
			return invalid("symbolic epoch slot and explicit timestamp are mutually exclusive")
		}
	case EpochSlotNone:
		if err := m.ExplicitTimestamp.Validate(); err != nil {
			return err
		}
	default:
		return invalid("manifest epoch slot is invalid")
	}
	if len(m.IGEN) == 0 != (len(m.Family) == 0) {
		return invalid("manifest IGEN and family must both be present or absent")
	}
	if len(m.IGEN) != 0 {
		if err := m.IGEN.Validate(); err != nil {
			return err
		}
		if err := m.Family.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func encodeEntry(e *encoder, m ManifestEntry, logicalOnly bool) {
	e.bytes("table", m.Table)
	e.bytes("row", m.Row)
	e.bytes("column family", m.ColumnFamily)
	e.bytes("column qualifier", m.ColumnQualifier)
	e.byte(byte(m.EpochSlot))
	e.optionalEpoch(m.ExplicitTimestamp)
	e.u32(m.ValueLength)
	e.digest(m.ValueDigest)
	e.bytes("LPART", m.LPART)
	e.digest(m.LogicalDigest)
	e.bytes("index generation", m.IGEN)
	e.bytes("index family", m.Family)
	if logicalOnly {
		return
	}
	e.u64(uint64(m.CopyGeneration))
	e.digest(m.VisibilityDigest)
	e.digest(m.PhysicalCopyDigest)
}

func decodeEntry(d *decoder) ManifestEntry {
	return ManifestEntry{
		Table:              d.bytes("table", MaxCoordinateBytes, true),
		Row:                d.bytes("row", MaxCoordinateBytes, true),
		ColumnFamily:       d.bytes("column family", MaxCoordinateBytes, true),
		ColumnQualifier:    d.bytes("column qualifier", MaxCoordinateBytes, true),
		EpochSlot:          EpochSlot(d.byte("epoch slot")),
		ExplicitTimestamp:  Epoch(d.optionalEpoch("explicit timestamp")),
		ValueLength:        d.u32("value length"),
		ValueDigest:        d.digest("value digest"),
		LPART:              LPART(d.bytes("LPART", MaxOpaqueIDBytes, true)),
		LogicalDigest:      d.digest("logical digest"),
		IGEN:               IGEN(d.bytes("index generation", MaxOpaqueIDBytes, false)),
		Family:             Family(d.bytes("index family", MaxOpaqueIDBytes, false)),
		CopyGeneration:     Generation(d.positive("copy generation")),
		VisibilityDigest:   d.digest("visibility digest"),
		PhysicalCopyDigest: d.digest("physical copy digest"),
	}
}

func entryDigest(entries []ManifestEntry, logical bool) Digest {
	h := sha256.New()
	var length [4]byte
	for _, entry := range entries {
		e := newPayloadEncoder(MaxChunkBytes)
		encodeEntry(e, entry, logical)
		length[0] = byte(len(e.data) >> 24)
		length[1] = byte(len(e.data) >> 16)
		length[2] = byte(len(e.data) >> 8)
		length[3] = byte(len(e.data))
		_, _ = h.Write(length[:])
		_, _ = h.Write(e.data)
	}
	var digest Digest
	copy(digest[:], h.Sum(nil))
	return digest
}

type ManifestChunkV2 struct {
	Index                 uint32
	Entries               []ManifestEntry
	EncodedBytes          uint32
	LogicalEntriesDigest  Digest
	PhysicalEntriesDigest Digest
	PreviousDigest        Digest
}

func (c ManifestChunkV2) ChainDigest() Digest {
	e := newPayloadEncoder(256)
	e.u32(c.Index)
	e.u32(uint32(len(c.Entries)))
	e.u32(c.EncodedBytes)
	e.digest(c.LogicalEntriesDigest)
	e.digest(c.PhysicalEntriesDigest)
	e.digest(c.PreviousDigest)
	return sha256.Sum256(e.data)
}

func (c ManifestChunkV2) Validate() error {
	if len(c.Entries) == 0 || len(c.Entries) > MaxChunkEntries {
		return invalid("manifest chunk entry count is outside its bound")
	}
	for index, entry := range c.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if index > 0 && CompareManifestEntries(c.Entries[index-1], entry) >= 0 {
			return invalid("manifest entries must be strictly ordered")
		}
	}
	if c.LogicalEntriesDigest != entryDigest(c.Entries, true) {
		return invalid("manifest logical entries digest mismatch")
	}
	if c.PhysicalEntriesDigest != entryDigest(c.Entries, false) {
		return invalid("manifest physical entries digest mismatch")
	}
	if c.Index == 0 && c.PreviousDigest != (Digest{}) {
		return invalid("first manifest chunk must have no predecessor")
	}
	if c.Index != 0 && c.PreviousDigest == (Digest{}) {
		return invalid("non-first manifest chunk requires a predecessor")
	}
	e := newPayloadEncoder(MaxChunkBytes)
	encodeChunk(e, c)
	if e.err != nil || len(e.data)+envelopeHeaderSize+checksumSize > MaxChunkBytes {
		return invalid("manifest chunk exceeds 1 MiB")
	}
	if c.EncodedBytes != uint32(len(e.data)+envelopeHeaderSize+checksumSize) {
		return invalid("manifest chunk encoded byte count mismatch")
	}
	return nil
}

func encodeChunk(e *encoder, c ManifestChunkV2) {
	e.u32(c.Index)
	e.u32(uint32(len(c.Entries)))
	e.u32(c.EncodedBytes)
	e.digest(c.LogicalEntriesDigest)
	e.digest(c.PhysicalEntriesDigest)
	e.digest(c.PreviousDigest)
	for _, entry := range c.Entries {
		encodeEntry(e, entry, false)
	}
}

func newManifestChunk(index uint32, entries []ManifestEntry, previous Digest) (ManifestChunkV2, error) {
	c := ManifestChunkV2{Index: index, Entries: append([]ManifestEntry(nil), entries...), PreviousDigest: previous}
	c.LogicalEntriesDigest = entryDigest(c.Entries, true)
	c.PhysicalEntriesDigest = entryDigest(c.Entries, false)
	e := newPayloadEncoder(MaxChunkBytes)
	encodeChunk(e, c)
	if e.err != nil || len(e.data)+envelopeHeaderSize+checksumSize > MaxChunkBytes {
		return ManifestChunkV2{}, invalid("manifest chunk exceeds 1 MiB")
	}
	c.EncodedBytes = uint32(len(e.data) + envelopeHeaderSize + checksumSize)
	return c, nil
}

func MarshalManifestChunkV2(c ManifestChunkV2) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	data, err := marshalEnvelope(KindManifestChunk, VersionManifestChunkV2, MaxChunkBytes, func(e *encoder) {
		encodeChunk(e, c)
	})
	if err != nil {
		return nil, err
	}
	if c.EncodedBytes != uint32(len(data)) {
		return nil, invalid("manifest chunk encoded byte count mismatch")
	}
	return data, nil
}

func UnmarshalManifestChunkV2(data []byte) (ManifestChunkV2, error) {
	payload, err := verifyEnvelope(data, KindManifestChunk, VersionManifestChunkV2, MaxChunkBytes)
	if err != nil {
		return ManifestChunkV2{}, err
	}
	d := &decoder{data: payload}
	c := ManifestChunkV2{
		Index: d.u32("index"),
	}
	count := d.u32("entry count")
	c.EncodedBytes = d.u32("encoded bytes")
	c.LogicalEntriesDigest = d.digest("logical entries digest")
	c.PhysicalEntriesDigest = d.digest("physical entries digest")
	c.PreviousDigest = d.digest("previous digest")
	// Count precedes allocation and every entry requires substantially more
	// than four bytes. The coarse remaining/4 check rejects forged counts early.
	if count == 0 || count > MaxChunkEntries || uint64(count) > uint64(d.remaining())/4 {
		return ManifestChunkV2{}, invalid("manifest entry count exceeds its bound or remaining payload")
	}
	c.Entries = make([]ManifestEntry, int(count))
	for i := range c.Entries {
		c.Entries[i] = decodeEntry(d)
	}
	if d.err != nil {
		return ManifestChunkV2{}, d.err
	}
	if c.EncodedBytes != uint32(len(data)) {
		return ManifestChunkV2{}, invalid("manifest chunk encoded byte count mismatch")
	}
	if err := c.Validate(); err != nil {
		return ManifestChunkV2{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeChunk(e, c) }, MaxChunkBytes); err != nil {
		return ManifestChunkV2{}, err
	}
	return c, nil
}

func ChunkManifest(entries []ManifestEntry) ([]ManifestChunkV2, error) {
	return chunkManifestWithSizer(entries, manifestEntryEncodedSize)
}

func manifestEntryEncodedSize(entry ManifestEntry) (int, error) {
	if err := entry.Validate(); err != nil {
		return 0, err
	}
	e := newPayloadEncoder(MaxChunkBytes)
	encodeEntry(e, entry, false)
	if e.err != nil {
		return 0, e.err
	}
	return len(e.data), nil
}

func chunkManifestWithSizer(
	entries []ManifestEntry,
	entrySize func(ManifestEntry) (int, error),
) ([]ManifestChunkV2, error) {
	if len(entries) > MaxManifestEntries {
		return nil, invalid("manifest has too many entries")
	}
	if len(entries) == 0 {
		return nil, nil
	}
	for index := 1; index < len(entries); index++ {
		if CompareManifestEntries(entries[index-1], entries[index]) >= 0 {
			return nil, invalid("manifest entries must be strictly ordered")
		}
	}
	sizes := make([]int, len(entries))
	for index, entry := range entries {
		size, err := entrySize(entry)
		if err != nil {
			return nil, err
		}
		sizes[index] = size
	}
	const fixedChunkBytes = envelopeHeaderSize + checksumSize + 4 + 4 + 4 + 3*sha256.Size
	chunks := make([]ManifestChunkV2, 0, (len(entries)+MaxChunkEntries-1)/MaxChunkEntries)
	start := 0
	var previous Digest
	for start < len(entries) {
		end := start
		encodedBytes := fixedChunkBytes
		for end < len(entries) && end-start < MaxChunkEntries &&
			encodedBytes+sizes[end] <= MaxChunkBytes {
			encodedBytes += sizes[end]
			end++
		}
		if end == start {
			return nil, invalid("single manifest entry exceeds 1 MiB chunk bound")
		}
		chunk, err := newManifestChunk(uint32(len(chunks)), entries[start:end], previous)
		if err != nil {
			return nil, err
		}
		if int(chunk.EncodedBytes) != encodedBytes {
			return nil, invalid("manifest chunk sizing mismatch")
		}
		chunks = append(chunks, chunk)
		previous = chunk.ChainDigest()
		start = end
	}
	return chunks, nil
}

type ManifestSummary struct {
	ChunkCount        uint32
	TotalEntries      uint64
	TotalEncodedBytes uint64
	RootDigest        Digest
}

func VerifyManifest(chunks []ManifestChunkV2) (ManifestSummary, error) {
	if len(chunks) > math.MaxUint32 {
		return ManifestSummary{}, invalid("manifest has too many chunks")
	}
	var summary ManifestSummary
	var previous Digest
	var lastEntry *ManifestEntry
	for index, chunk := range chunks {
		if chunk.Index != uint32(index) {
			return ManifestSummary{}, invalid("manifest chunks are missing, duplicated, or reordered")
		}
		if chunk.PreviousDigest != previous {
			return ManifestSummary{}, invalid("manifest chunk chain mismatch")
		}
		if err := chunk.Validate(); err != nil {
			return ManifestSummary{}, err
		}
		if lastEntry != nil && CompareManifestEntries(*lastEntry, chunk.Entries[0]) >= 0 {
			return ManifestSummary{}, invalid("manifest entries must be strictly ordered across chunks")
		}
		if err := checkedAdd("manifest entry total", &summary.TotalEntries, uint64(len(chunk.Entries))); err != nil {
			return ManifestSummary{}, err
		}
		if summary.TotalEntries > MaxManifestEntries {
			return ManifestSummary{}, invalid("manifest has too many entries")
		}
		if err := checkedAdd("manifest byte total", &summary.TotalEncodedBytes, uint64(chunk.EncodedBytes)); err != nil {
			return ManifestSummary{}, err
		}
		lastEntry = &chunk.Entries[len(chunk.Entries)-1]
		previous = chunk.ChainDigest()
	}
	summary.ChunkCount = uint32(len(chunks))
	summary.RootDigest = previous
	return summary, nil
}

func VerifyManifestRoot(root TxnRootV3, chunks []ManifestChunkV2) error {
	summary, err := VerifyManifest(chunks)
	if err != nil {
		return err
	}
	if root.ChunkCount != summary.ChunkCount ||
		root.TotalEntries != summary.TotalEntries ||
		root.TotalEncodedBytes != summary.TotalEncodedBytes ||
		root.ManifestRoot != summary.RootDigest {
		return invalid("transaction root manifest totals or digest mismatch")
	}
	return nil
}

func CompareManifestEntries(left, right ManifestEntry) int {
	for _, pair := range [][2][]byte{
		{left.Table, right.Table}, {left.Row, right.Row},
		{left.ColumnFamily, right.ColumnFamily},
		{left.ColumnQualifier, right.ColumnQualifier},
	} {
		if order := bytes.Compare(pair[0], pair[1]); order != 0 {
			return order
		}
	}
	if left.EpochSlot < right.EpochSlot {
		return -1
	}
	if left.EpochSlot > right.EpochSlot {
		return 1
	}
	return bytes.Compare(left.ValueDigest[:], right.ValueDigest[:])
}
