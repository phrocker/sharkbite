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

package explorer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// EmbeddedTableName is the durable table used by the embedded Explorer.
const EmbeddedTableName = "_shoal_explorer"

// RecordPublication is the narrow immutable-record seam used by the embedded
// Explorer. The adapter owns transaction IDs, manifests, fencing, and recovery;
// Explorer continues to own its existing record envelope and row layout.
type RecordPublication struct {
	Operation       []byte
	RecordKey       []byte
	Token           []byte
	Table           string
	Row             []byte
	Family          []byte
	Qualifier       []byte
	Visibility      []byte
	Value           []byte
	EntityKind      byte
	EntityID        []byte
	WinnerID        []byte
	Partition       []byte
	LogicalPolicyID []byte
	ResultKind      []byte
	ResultID        []byte
	ExpectedEpoch   coordination.Epoch
	ExpectedDigest  coordination.Digest
}

type RecordPublicationResult struct {
	Epoch     coordination.Epoch
	Unchanged bool
}

type RecordPublicationHead struct {
	Epoch         coordination.Epoch
	LogicalDigest coordination.Digest
	WinnerID      []byte
}

type RecordPublicationAttempt struct {
	Value          []byte
	ExpectedEpoch  coordination.Epoch
	ExpectedDigest coordination.Digest
}

// RecordPublicationAdapter durably publishes one immutable Explorer record.
// Implementations must persist their canonical logical intent before making
// any physical write and must resolve ambiguous commits by authoritative
// readback or return an indeterminate-commit error.
type RecordPublicationAdapter interface {
	PublishRecord(context.Context, RecordPublication) (RecordPublicationResult, error)
	RecordCommitted(context.Context, RecordPublication) (bool, error)
	RecordHead(context.Context, byte, []byte) (*RecordPublicationHead, error)
	RecordAttempt(context.Context, RecordPublication) (*RecordPublicationAttempt, error)
	PendingPublications(context.Context) (bool, error)
}

var embeddedDefaultPolicy = []byte("embedded/default")

// documentIDKind is the stableID kind of a document ID, so every document ID
// is "doc_" and then 32 lowercase hex digits.
const documentIDKind = "doc"

// IsDocumentID reports whether id has the one shape a document ID takes:
// "doc_" and then 32 lowercase hex digits.
//
// A document record is guarded under kind 'D' at its document ID, and fleet
// dispatch guards its actions under 'D' at their raw action IDs. The guard
// keeps either store from adopting the other's head, but not from occupying
// an ID first (#669). A document ID is a digest of its source URI, so anyone
// who knows a URI can compute the ID. A dispatcher could therefore enqueue an
// action at that ID before the URI is ingested, and the ingestion would
// conflict permanently. Dispatch refuses every new action ID of this shape,
// which keeps the two ID spaces disjoint in both directions: a document can
// only ever occupy this shape, and dispatch never mints it.
func IsDocumentID(id []byte) bool {
	const digits = 32
	prefix := documentIDKind + "_"
	if len(id) != len(prefix)+digits || !bytes.HasPrefix(id, []byte(prefix)) {
		return false
	}
	for _, c := range id[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (e *Explorer) publicationPending(ctx context.Context) (bool, error) {
	if e.publication == nil {
		return false, nil
	}
	pending, err := e.publication.PendingPublications(ctx)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorUnavailable, "inspect transactional publications", err,
		)
	}
	return pending, nil
}

func documentStableKey(row []byte) []byte {
	key := sha256.Sum256(append([]byte("explorer-document-record-v1\x00"), row...))
	return append([]byte(nil), key[:]...)
}

func documentRecordKey(row []byte, head *RecordPublicationHead) []byte {
	hash := sha256.New()
	_, _ = hash.Write(documentStableKey(row))
	if head != nil {
		var epoch [8]byte
		binary.BigEndian.PutUint64(epoch[:], uint64(head.Epoch))
		_, _ = hash.Write(epoch[:])
		_, _ = hash.Write(head.LogicalDigest[:])
	}
	return hash.Sum(nil)
}

func documentRecordPublication(
	row, encoded []byte,
	record *persistedDocument,
	head *RecordPublicationHead,
) RecordPublication {
	recordKey := documentStableKey(row)
	request := RecordPublication{
		Operation:       []byte("explorer-document-record-v1"),
		RecordKey:       recordKey,
		Token:           documentRecordKey(row, head),
		Table:           EmbeddedTableName,
		Row:             append([]byte(nil), row...),
		Family:          []byte(recordCF),
		Qualifier:       []byte(recordCQV2),
		Value:           append([]byte(nil), encoded...),
		EntityKind:      'D',
		EntityID:        []byte(record.Document.ID),
		WinnerID:        []byte(record.Revision.ID),
		Partition:       []byte(record.Document.ID),
		LogicalPolicyID: append([]byte(nil), embeddedDefaultPolicy...),
		ResultKind:      []byte("document-revision"),
		ResultID:        []byte(record.Revision.ID),
	}
	if head != nil {
		request.ExpectedEpoch = head.Epoch
		request.ExpectedDigest = head.LogicalDigest
	}
	return request
}

func documentRecordCommitProbe(row, qualifier, encoded []byte) RecordPublication {
	return RecordPublication{
		Operation: []byte("explorer-document-record-v1"),
		RecordKey: documentStableKey(row),
		Table:     EmbeddedTableName,
		Row:       append([]byte(nil), row...),
		Family:    []byte(recordCF),
		Qualifier: append([]byte(nil), qualifier...),
		Value:     append([]byte(nil), encoded...),
	}
}

func documentRecordAttemptProbe(row []byte) RecordPublication {
	return RecordPublication{
		Operation: []byte("explorer-document-record-v1"),
		RecordKey: documentStableKey(row),
		Table:     EmbeddedTableName,
		Row:       append([]byte(nil), row...),
		Family:    []byte(recordCF),
		Qualifier: []byte(recordCQV2),
	}
}

func (e *Explorer) documentRecordAttempt(
	ctx context.Context,
	row []byte,
) (*persistedDocument, *RecordPublicationHead, []byte, bool, error) {
	if e.publication == nil {
		return nil, nil, nil, false, nil
	}
	attempt, err := e.publication.RecordAttempt(
		ctx, documentRecordAttemptProbe(row),
	)
	if err != nil {
		return nil, nil, nil, false, shoal.WrapError(
			shoal.ErrorUnavailable, "read transactional document attempt", err,
		)
	}
	if attempt == nil {
		return nil, nil, nil, false, nil
	}
	var record persistedDocument
	if err := decodeEmbeddedRecord(
		attempt.Value, embeddedRecordDocument, &record,
	); err != nil {
		return nil, nil, nil, false, shoal.WrapError(
			shoal.ErrorInternal, "decode transactional document attempt", err,
		)
	}
	if err := validatePersistedDocument(record); err != nil ||
		!bytes.Equal(row, documentRecordRow(record.Document.ID, record.Revision.ID)) {
		return nil, nil, nil, false, shoal.NewError(
			shoal.ErrorInternal, "transactional document attempt is invalid",
		)
	}
	var head *RecordPublicationHead
	if attempt.ExpectedEpoch != 0 {
		head = &RecordPublicationHead{
			Epoch: attempt.ExpectedEpoch, LogicalDigest: attempt.ExpectedDigest,
		}
	}
	return &record, head, append([]byte(nil), attempt.Value...), true, nil
}

func (e *Explorer) documentRecordHead(
	ctx context.Context,
	documentID []byte,
) (*RecordPublicationHead, error) {
	if e.publication == nil {
		return nil, nil
	}
	head, err := e.publication.RecordHead(ctx, 'D', documentID)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorUnavailable, "read transactional document head", err,
		)
	}
	return head, nil
}

func (e *Explorer) writeDocumentRecord(
	ctx context.Context,
	row []byte,
	record *persistedDocument,
	head *RecordPublicationHead,
	attemptedValue []byte,
) error {
	encoded := append([]byte(nil), attemptedValue...)
	if len(encoded) == 0 {
		var err error
		encoded, err = encodeEmbeddedRecord(embeddedRecordDocument, record)
		if err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "encode explorer record", err)
		}
	}
	if e.publication == nil {
		return e.writeEncodedRecord(row, encoded)
	}
	if len(encoded) > coordination.MaxManifestValueBytes {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"transactional document record exceeds the embedded publication bound",
		)
	}
	_, err := e.publication.PublishRecord(
		ctx, documentRecordPublication(row, encoded, record, head),
	)
	return err
}

func (e *Explorer) documentRecordCommitted(
	ctx context.Context,
	row, qualifier, encoded []byte,
) (bool, error) {
	if e.publication == nil {
		return true, nil
	}
	committed, err := e.publication.RecordCommitted(
		ctx, documentRecordCommitProbe(row, qualifier, encoded),
	)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorInternal, "verify transactional document record", err,
		)
	}
	return committed, nil
}
