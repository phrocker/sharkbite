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

// Package canonical defines the storage-neutral canonical encoding for
// logical Explorer values. It is independent of the embedded Explorer
// persistence envelope.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// VersionV1 is the first canonical logical record format.
	VersionV1 uint16 = 1

	// MaxCanonicalRecordBytes bounds one complete envelope, including its
	// header and checksum.
	MaxCanonicalRecordBytes = 1 << 30
	// MaxGraphNodesPerRecord and MaxGraphEdgesPerRecord bound aggregate graph
	// values in one logical document revision.
	MaxGraphNodesPerRecord = 1_000_000
	MaxGraphEdgesPerRecord = 1_000_000
)

// Kind identifies the logical value carried by a canonical envelope.
type Kind uint16

const (
	// KindRecord identifies a RecordV1 payload.
	KindRecord Kind = 1
)

const (
	canonicalMagic       = "SHOALC\x00\x00"
	envelopeHeaderBytes  = len(canonicalMagic) + 2 + 2 + 8
	envelopeChecksumSize = sha256.Size
	maxPayloadBytes      = MaxCanonicalRecordBytes - envelopeHeaderBytes -
		envelopeChecksumSize
)

// SHA256 is a SHA-256 checksum.
type SHA256 [sha256.Size]byte

// String returns the lowercase hexadecimal checksum.
func (d SHA256) String() string {
	return hex.EncodeToString(d[:])
}

// PublicationV1 is optional publication metadata for a canonical envelope.
// Sequence is in [1, math.MaxInt64], and PublishedAt is a nonzero supported
// time. Publication metadata is deliberately separate from
// Revision.CreatedAt, which remains source metadata.
type PublicationV1 struct {
	Sequence    uint64
	PublishedAt time.Time
}

// RecordV1 is one immutable logical Explorer document revision. Ordered
// slices are encoded in the supplied order; metadata maps are encoded by
// unsigned byte-sorted key.
type RecordV1 struct {
	Source      []byte
	Document    document.Document
	Revision    document.Revision
	Sections    []document.Section
	Spans       []document.Span
	Nodes       []graph.Node
	Edges       []graph.Edge
	Publication *PublicationV1
}

// Validate checks the complete logical record using the public value
// validators plus codec aggregate and uniqueness bounds.
func (r RecordV1) Validate() error {
	if len(r.Nodes) > MaxGraphNodesPerRecord {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"canonical record has too many graph nodes",
		)
	}
	if len(r.Edges) > MaxGraphEdgesPerRecord {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"canonical record has too many graph edges",
		)
	}
	if err := document.ValidateRevisionContent(
		string(r.Source),
		r.Document,
		r.Revision,
		r.Sections,
		r.Spans,
	); err != nil {
		return err
	}

	nodeIDs := make(map[shoal.ID]struct{}, len(r.Nodes))
	for _, node := range r.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
		if _, duplicate := nodeIDs[node.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate graph node ID")
		}
		nodeIDs[node.ID] = struct{}{}
	}

	edgeIDs := make(map[shoal.ID]struct{}, len(r.Edges))
	for _, edge := range r.Edges {
		if err := edge.Validate(); err != nil {
			return err
		}
		if _, exists := nodeIDs[edge.From]; !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"graph edge from endpoint is missing from canonical record",
			)
		}
		if _, exists := nodeIDs[edge.To]; !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"graph edge to endpoint is missing from canonical record",
			)
		}
		if _, duplicate := edgeIDs[edge.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate graph edge ID")
		}
		edgeIDs[edge.ID] = struct{}{}
	}

	if r.Publication != nil {
		if r.Publication.Sequence == 0 ||
			r.Publication.Sequence > math.MaxInt64 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"publication sequence must be between 1 and MaxInt64",
			)
		}
		if r.Publication.PublishedAt.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "publication time is required")
		}
		if err := validateTime("publication time", r.Publication.PublishedAt); err != nil {
			return err
		}
	}
	return nil
}

// MarshalV1 validates and deterministically encodes one RecordV1.
func MarshalV1(record RecordV1) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}

	encoder := newEncoder()
	encodeRecordV1(encoder, record)
	if encoder.err != nil {
		return nil, encoder.err
	}

	payloadLength := len(encoder.data) - envelopeHeaderBytes
	binary.BigEndian.PutUint16(
		encoder.data[len(canonicalMagic):len(canonicalMagic)+2], VersionV1)
	binary.BigEndian.PutUint16(
		encoder.data[len(canonicalMagic)+2:envelopeHeaderBytes], uint16(KindRecord))
	binary.BigEndian.PutUint64(
		encoder.data[envelopeHeaderBytes-8:envelopeHeaderBytes],
		uint64(payloadLength),
	)
	copy(encoder.data, canonicalMagic)

	sum := sha256.Sum256(encoder.data)
	if len(encoder.data) > MaxCanonicalRecordBytes-envelopeChecksumSize {
		return nil, invalid("canonical record exceeds the aggregate byte bound")
	}
	encoder.data = append(encoder.data, sum[:]...)
	return encoder.data, nil
}

// UnmarshalV1 verifies and decodes one canonical RecordV1 envelope.
func UnmarshalV1(canonical []byte) (RecordV1, error) {
	payload, _, err := verifyEnvelope(canonical)
	if err != nil {
		return RecordV1{}, err
	}
	return decodePayloadV1(payload)
}

func decodePayloadV1(payload []byte) (RecordV1, error) {
	decoder := newDecoder(payload)
	record := decodeRecordV1(decoder)
	if decoder.err != nil {
		return RecordV1{}, decoder.err
	}
	if decoder.remaining() != 0 {
		return RecordV1{}, invalid("canonical record payload has trailing bytes")
	}
	if err := record.Validate(); err != nil {
		return RecordV1{}, err
	}
	// A decodable payload is not necessarily the canonical encoding of what it
	// decodes to: field orderings and alternate presence forms can differ while
	// still decoding to an equal record. Re-encoding and requiring an exact
	// match makes the encoding injective, so one record has exactly one valid
	// byte representation and therefore exactly one digest.
	if err := verifyCanonicalPayload(record, payload); err != nil {
		return RecordV1{}, err
	}
	return record, nil
}

func verifyCanonicalPayload(record RecordV1, payload []byte) error {
	encoder := newComparingEncoder(payload)
	encodeRecordV1(encoder, record)
	// The record already validated, so an encoder failure is a violation of
	// this package's own invariants rather than a malformed caller payload.
	if encoder.err != nil {
		return shoal.NewError(
			shoal.ErrorInternal, "re-encoding a validated canonical record failed")
	}
	if encoder.mismatch || encoder.offset != len(payload) {
		return invalid("canonical record is not canonically encoded")
	}
	return nil
}

// Digest validates canonical bytes and returns the embedded SHA-256 checksum
// over their magic, version, kind, length, and payload.
func Digest(canonical []byte) (SHA256, error) {
	payload, checksum, err := verifyEnvelope(canonical)
	if err != nil {
		return SHA256{}, err
	}
	if _, err := decodePayloadV1(payload); err != nil {
		return SHA256{}, err
	}
	return checksum, nil
}

// DigestV1 marshals a record and returns its canonical digest.
func DigestV1(record RecordV1) (SHA256, error) {
	canonical, err := MarshalV1(record)
	if err != nil {
		return SHA256{}, err
	}
	return Digest(canonical)
}

// VerifyChecksum validates the envelope structure and embedded checksum.
func VerifyChecksum(canonical []byte) error {
	_, _, err := verifyEnvelope(canonical)
	return err
}

// SourceSHA256 returns the SHA-256 checksum of exact source bytes.
func SourceSHA256(source []byte) SHA256 {
	return sha256.Sum256(source)
}

func verifyEnvelope(canonical []byte) ([]byte, SHA256, error) {
	if len(canonical) < envelopeHeaderBytes {
		return nil, SHA256{}, invalid("canonical record is truncated")
	}
	if !bytes.Equal(canonical[:len(canonicalMagic)], []byte(canonicalMagic)) {
		return nil, SHA256{}, invalid("canonical record magic is invalid")
	}

	version := binary.BigEndian.Uint16(
		canonical[len(canonicalMagic) : len(canonicalMagic)+2])
	if version != VersionV1 {
		return nil, SHA256{}, invalid(
			fmt.Sprintf("canonical record version %d is unsupported", version))
	}
	kind := Kind(binary.BigEndian.Uint16(
		canonical[len(canonicalMagic)+2 : envelopeHeaderBytes-8]))
	if kind != KindRecord {
		return nil, SHA256{}, invalid(
			fmt.Sprintf("canonical record kind %d is unsupported", kind))
	}

	payloadLength := binary.BigEndian.Uint64(
		canonical[envelopeHeaderBytes-8 : envelopeHeaderBytes])
	if payloadLength > uint64(maxPayloadBytes) {
		return nil, SHA256{}, invalid(
			"canonical record exceeds the aggregate byte bound")
	}
	expectedLength := uint64(envelopeHeaderBytes) + payloadLength +
		envelopeChecksumSize
	switch {
	case uint64(len(canonical)) < expectedLength:
		return nil, SHA256{}, invalid("canonical record is truncated")
	case uint64(len(canonical)) > expectedLength:
		return nil, SHA256{}, invalid("canonical record has trailing bytes")
	}

	payloadEnd := envelopeHeaderBytes + int(payloadLength)
	calculated := sha256.Sum256(canonical[:payloadEnd])
	stored := canonical[payloadEnd:]
	if subtle.ConstantTimeCompare(calculated[:], stored) != 1 {
		return nil, SHA256{}, invalid("canonical record checksum mismatch")
	}
	var checksum SHA256
	copy(checksum[:], stored)
	return canonical[envelopeHeaderBytes:payloadEnd], checksum, nil
}

func validateTime(name string, value time.Time) error {
	if value.IsZero() {
		return nil
	}
	year := value.UTC().Year()
	if year < 1 || year > 9999 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" is outside the supported range")
	}
	return nil
}

func invalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, message)
}
