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
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The v2 embedded compatibility envelope is magic, envelope version, record
// kind, big-endian payload length, SHA-256 payload checksum, and gob payload.
// It is intentionally separate from the future cross-adapter canonical codec.
const (
	explorerTable           = EmbeddedTableName
	recordCF                = "record"
	recordCQV1              = "v1"
	recordCQV2              = "v2"
	recordDeleteCQ          = "deleted"
	documentRow             = "document/"
	edgeRow                 = "edge/"
	interactionRow          = "interaction/"
	foldRow                 = "interaction-fold/"
	extractionRow           = "extraction/"
	graphMaterializationRow = "graph-materialization/"
	snapshotRow             = "snapshot/"
	proposalRow             = "ontology-proposal/"
	transitionRow           = "ontology-proposal-transition/"

	embeddedRecordMagic                  = "SHOALX2\x00"
	embeddedEnvelopeVersion              = byte(1)
	embeddedRecordDocument               = byte(1)
	embeddedRecordEdge                   = byte(2)
	embeddedRecordSnapshotAnchor         = byte(3)
	embeddedRecordInteraction            = byte(4)
	embeddedRecordInteractionSink        = byte(5)
	embeddedRecordFold                   = byte(6)
	embeddedRecordCursorKey              = byte(7)
	embeddedRecordOntologyProposal       = byte(8)
	embeddedRecordProposalTransition     = byte(9)
	embeddedRecordExtraction             = byte(10)
	embeddedRecordSnapshot               = byte(11)
	embeddedRecordGraphMaterialization   = byte(12)
	embeddedEnvelopeHeader               = 8 + 1 + 1 + 8 + sha256.Size
	maxEmbeddedDocumentBytes             = uint64(document.MaxRevisionSourceBytes) * 8
	maxEmbeddedEdgeBytes                 = uint64(2 * 1024 * 1024)
	maxEmbeddedSnapshotAnchorBytes       = uint64(1024)
	maxEmbeddedInteractionBytes          = uint64(64 * 1024 * 1024)
	maxEmbeddedInteractionSinkBytes      = uint64(1024)
	maxEmbeddedFoldBytes                 = uint64(64 * 1024 * 1024)
	maxEmbeddedCursorKeyBytes            = uint64(1024)
	maxEmbeddedOntologyProposalBytes     = uint64(16 * 1024 * 1024)
	maxEmbeddedProposalTransitionBytes   = uint64(64 * 1024)
	maxEmbeddedExtractionBytes           = uint64(16 * 1024 * 1024)
	maxEmbeddedSnapshotBytes             = uint64(64 * 1024 * 1024)
	maxEmbeddedGraphMaterializationBytes = uint64(4 * 1024 * 1024)
)

var snapshotAnchorRow = []byte("meta/snapshot-anchor")

var cursorKeyRow = []byte("meta/change-cursor-key")

var interactionSinkRow = []byte("meta/interaction-sink")

type persistedSnapshotAnchor struct {
	CreatedAt time.Time
}

type persistedSnapshot struct {
	ID             shoal.ID
	AsOf           time.Time
	ParentID       shoal.ID
	AddedNodeIDs   []shoal.ID
	RemovedNodeIDs []shoal.ID
	NodeStates     []persistedSnapshotObject
	RemovedEdgeIDs []shoal.ID
	EdgeStates     []persistedSnapshotObject
	// Assertion states are keyed by their mapped source edge, like graphAssertions.
	AssertionStates         []persistedSnapshotObject
	RemovedAssertionEdgeIDs []shoal.ID
}

type persistedSnapshotObject struct {
	ID     shoal.ID
	Digest string
}

// persistedCursorKey holds the durable, per-corpus secret that seals change-feed
// cursors. It is a random key generated once at corpus creation and persisted
// with the corpus state, so cursors stay valid across restart and travel with a
// backup, while a fresh corpus mints an independent key that cannot open another
// corpus's cursors.
type persistedCursorKey struct {
	Key []byte
}

type documentRevisionKey struct {
	documentID shoal.ID
	revisionID shoal.ID
}

func documentRecordRow(documentID, revisionID shoal.ID) []byte {
	row := make([]byte, 0, len(documentRow)+len(documentID)+1+len(revisionID))
	row = append(row, documentRow...)
	row = append(row, []byte(documentID)...)
	row = append(row, '/')
	row = append(row, []byte(revisionID)...)
	return row
}

func edgeRecordRow(edgeID shoal.ID) []byte {
	row := make([]byte, 0, len(edgeRow)+len(edgeID))
	row = append(row, edgeRow...)
	row = append(row, []byte(edgeID)...)
	return row
}

func graphMaterializationRecordRow(id shoal.ID) []byte {
	return append([]byte(graphMaterializationRow), []byte(id)...)
}

func interactionRecordRow(sessionID shoal.ID) []byte {
	row := make([]byte, 0, len(interactionRow)+len(sessionID))
	row = append(row, interactionRow...)
	row = append(row, []byte(sessionID)...)
	return row
}

func foldRecordRow(foldID shoal.ID) []byte {
	row := make([]byte, 0, len(foldRow)+len(foldID))
	row = append(row, foldRow...)
	row = append(row, []byte(foldID)...)
	return row
}

func snapshotRecordRow(snapshotID shoal.ID) []byte {
	row := make([]byte, 0, len(snapshotRow)+len(snapshotID))
	row = append(row, snapshotRow...)
	row = append(row, snapshotID...)
	return row
}

func extractionRecordRow(extractionID shoal.ID) []byte {
	row := make([]byte, 0, len(extractionRow)+len(extractionID))
	row = append(row, []byte(extractionRow)...)
	row = append(row, []byte(extractionID)...)
	return row
}

func ontologyProposalRecordRow(proposalID shoal.ID) []byte {
	row := make([]byte, 0, len(proposalRow)+len(proposalID))
	row = append(row, []byte(proposalRow)...)
	row = append(row, []byte(proposalID)...)
	return row
}

func ontologyProposalTransitionRecordRow(proposalID shoal.ID, sequence uint32) []byte {
	row := make([]byte, 0, len(transitionRow)+len(proposalID)+1+10)
	row = append(row, []byte(transitionRow)...)
	row = append(row, []byte(proposalID)...)
	row = append(row, '/')
	row = strconv.AppendUint(row, uint64(sequence), 10)
	return row
}

func (e *Explorer) load() error {
	scanner, err := e.engine.Scan(explorerTable, iterrt.InfiniteRange(), engine.ScanOptions{
		ColumnFamilies: [][]byte{[]byte(recordCF)}, ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "scan explorer records", err)
	}
	defer scanner.Close()

	documentFormats := make(map[documentRevisionKey]byte)
	edgeFormats := make(map[shoal.ID]byte)
	publicationSequences := make(map[uint64]documentRevisionKey)
	for scanner.Next() {
		key := scanner.Key()
		qualifier := key.ColumnQualifier
		if bytes.Equal(qualifier, []byte(recordCQV1)) &&
			!bytes.HasPrefix(key.Row, []byte(documentRow)) {
			if err := validateStrictJSONStringEncoding(scanner.Value()); err != nil {
				return shoal.WrapError(
					shoal.ErrorInternal,
					"stored explorer record has invalid JSON string encoding",
					err,
				)
			}
		}
		switch {
		case bytes.Equal(key.Row, snapshotAnchorRow):
			if err := e.loadSnapshotAnchor(qualifier, scanner.Value()); err != nil {
				return err
			}
		case bytes.Equal(key.Row, cursorKeyRow):
			if err := e.loadCursorKey(qualifier, scanner.Value()); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(documentRow)):
			if err := e.loadDocumentRecord(
				key.Row,
				qualifier,
				scanner.Value(),
				documentFormats,
				publicationSequences,
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(edgeRow)):
			if err := e.loadEdgeRecord(
				key.Row, qualifier, scanner.Value(), edgeFormats,
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(foldRow)):
			if err := e.loadFoldRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(extractionRow)):
			if err := e.loadExtractionRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(graphMaterializationRow)):
			if err := e.loadGraphMaterializationRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(snapshotRow)):
			if err := e.loadSnapshotRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(proposalRow)):
			if err := e.loadOntologyProposalRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(transitionRow)):
			if err := e.loadOntologyProposalTransitionRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		case bytes.HasPrefix(key.Row, []byte(interactionRow)):
			if err := e.loadInteractionRecord(
				key.Row, qualifier, scanner.Value(),
			); err != nil {
				return err
			}
		}
		if err := scanner.Advance(); err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "advance explorer scan", err)
		}
	}
	for _, proposal := range e.ontologyProposals {
		if _, err := proposal.proposal(); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "stored ontology proposal is invalid", err)
		}
	}
	space, found, err := e.embeddingSpaceLocked()
	if err != nil {
		return err
	}
	e.embeddingSpace = embeddingSpaceCache{provenance: space, found: found}
	if err := e.restoreLatestSnapshotLocked(); err != nil {
		return err
	}
	return nil
}

func (e *Explorer) loadSnapshotAnchor(qualifier, encoded []byte) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedSnapshotAnchor
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordSnapshotAnchor, &record,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode snapshot anchor", err)
	}
	if record.CreatedAt.IsZero() {
		return shoal.NewError(shoal.ErrorInternal, "snapshot anchor time is missing")
	}
	e.snapshotAnchor = record.CreatedAt.UTC()
	return nil
}

func (e *Explorer) loadSnapshotRecord(
	row, qualifier, encoded []byte,
) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedSnapshot
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordSnapshot, &record,
	); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "decode explorer snapshot", err)
	}
	if err := shoal.ValidateRequiredID("snapshot ID", record.ID); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored snapshot is invalid", err)
	}
	if record.AsOf.IsZero() ||
		!bytes.Equal(row, snapshotRecordRow(record.ID)) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored snapshot record is invalid")
	}
	if err := validateSnapshotDelta(record); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored snapshot is invalid", err)
	}
	record.AsOf = record.AsOf.UTC()
	if existing, ok := e.snapshotHistory[string(record.ID)]; ok &&
		!persistedSnapshotsEqual(existing, record) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored snapshot ID has conflicting content")
	}
	e.snapshotHistory[string(record.ID)] = record
	return nil
}

func validateSnapshotDelta(record persistedSnapshot) error {
	if err := shoal.ValidateOptionalID(
		"snapshot parent ID", record.ParentID); err != nil {
		return err
	}
	for groupIndex, ids := range [][]shoal.ID{
		record.AddedNodeIDs, record.RemovedNodeIDs, record.RemovedEdgeIDs,
		record.RemovedAssertionEdgeIDs,
	} {
		name := "snapshot node ID"
		if groupIndex >= 2 {
			name = "snapshot edge ID"
		}
		for index, id := range ids {
			if err := shoal.ValidateRequiredID(name, id); err != nil {
				return err
			}
			if index > 0 && shoal.CompareID(ids[index-1], id) >= 0 {
				return fmt.Errorf("snapshot object IDs are not canonical")
			}
		}
	}
	for _, states := range [][]persistedSnapshotObject{
		record.NodeStates, record.EdgeStates, record.AssertionStates,
	} {
		for index, state := range states {
			if err := shoal.ValidateRequiredID(
				"snapshot object ID", state.ID); err != nil {
				return err
			}
			decoded, err := hex.DecodeString(state.Digest)
			if err != nil || len(decoded) != sha256.Size ||
				hex.EncodeToString(decoded) != state.Digest {
				return fmt.Errorf("snapshot object digest is invalid")
			}
			if index > 0 &&
				shoal.CompareID(states[index-1].ID, state.ID) >= 0 {
				return fmt.Errorf("snapshot object states are not canonical")
			}
		}
	}
	for _, id := range record.AddedNodeIDs {
		index := sort.Search(len(record.RemovedNodeIDs), func(index int) bool {
			return shoal.CompareID(record.RemovedNodeIDs[index], id) >= 0
		})
		if index < len(record.RemovedNodeIDs) &&
			record.RemovedNodeIDs[index] == id {
			return fmt.Errorf("snapshot node cannot be both added and removed")
		}
	}
	if len(record.NodeStates) > 0 {
		if len(record.NodeStates) != len(record.AddedNodeIDs) {
			return fmt.Errorf("snapshot node states do not match node additions")
		}
		for index, state := range record.NodeStates {
			if state.ID != record.AddedNodeIDs[index] {
				return fmt.Errorf(
					"snapshot node states do not match node additions")
			}
		}
	}
	for _, state := range record.EdgeStates {
		index := sort.Search(
			len(record.RemovedEdgeIDs), func(index int) bool {
				return shoal.CompareID(
					record.RemovedEdgeIDs[index], state.ID) >= 0
			})
		if index < len(record.RemovedEdgeIDs) &&
			record.RemovedEdgeIDs[index] == state.ID {
			return fmt.Errorf(
				"snapshot edge cannot be both updated and removed")
		}
	}
	for _, state := range record.AssertionStates {
		index := sort.Search(
			len(record.RemovedAssertionEdgeIDs), func(index int) bool {
				return shoal.CompareID(
					record.RemovedAssertionEdgeIDs[index], state.ID) >= 0
			})
		if index < len(record.RemovedAssertionEdgeIDs) &&
			record.RemovedAssertionEdgeIDs[index] == state.ID {
			return fmt.Errorf(
				"snapshot assertion cannot be both updated and removed")
		}
	}
	return nil
}

func (e *Explorer) loadCursorKey(qualifier, encoded []byte) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedCursorKey
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordCursorKey, &record,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode change cursor key", err)
	}
	if len(record.Key) != changeCursorKeyBytes {
		return shoal.NewError(shoal.ErrorInternal, "change cursor key has an invalid length")
	}
	e.changeCursorKey = append([]byte(nil), record.Key...)
	return nil
}

func (e *Explorer) loadDocumentRecord(
	row, qualifier, encoded []byte,
	formats map[documentRevisionKey]byte,
	publicationSequences map[uint64]documentRevisionKey,
) error {
	var (
		record persistedDocument
		format byte
	)
	if bytes.Equal(qualifier, []byte(recordCQV1)) ||
		bytes.Equal(qualifier, []byte(recordCQV2)) {
		committed, err := e.documentRecordCommitted(
			context.Background(), row, qualifier, encoded,
		)
		if err != nil {
			return err
		}
		if !committed {
			return nil
		}
	}
	switch {
	case bytes.Equal(qualifier, []byte(recordCQV1)):
		if err := validateStrictJSONStringEncoding(encoded); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal,
				"stored explorer record has invalid JSON string encoding",
				err,
			)
		}
		if err := json.Unmarshal(encoded, &record); err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "decode explorer document", err)
		}
		record.PublicationSequence = 0
		if err := validateLegacyPersistedDocument(record); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "stored explorer document is invalid", err)
		}
		format = 1
	case bytes.Equal(qualifier, []byte(recordCQV2)):
		if err := decodeEmbeddedRecord(
			encoded, embeddedRecordDocument, &record,
		); err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "decode explorer document", err)
		}
		if err := validatePersistedDocument(record); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "stored explorer document is invalid", err)
		}
		if !bytes.Equal(row, documentRecordRow(record.Document.ID, record.Revision.ID)) {
			return shoal.NewError(
				shoal.ErrorInternal, "stored explorer document row is invalid")
		}
		format = 2
	default:
		return nil
	}

	key := documentRevisionKey{
		documentID: record.Document.ID,
		revisionID: record.Revision.ID,
	}
	if record.PublicationSequence != 0 {
		if owner, exists := publicationSequences[record.PublicationSequence]; exists &&
			owner != key {
			return shoal.NewError(
				shoal.ErrorInternal, "stored publication sequences are not unique")
		}
		publicationSequences[record.PublicationSequence] = key
	}
	if formats[key] >= format {
		return nil
	}
	formats[key] = format
	if e.documents[record.Document.ID] == nil {
		e.documents[record.Document.ID] = make(map[shoal.ID]*persistedDocument)
	}
	copy := record
	e.documents[record.Document.ID][record.Revision.ID] = &copy
	if record.PublicationSequence > e.lastPublicationSequence {
		e.lastPublicationSequence = record.PublicationSequence
	}
	return nil
}

func (e *Explorer) loadEdgeRecord(
	row, qualifier, encoded []byte, formats map[shoal.ID]byte,
) error {
	var (
		record persistedEdge
		format byte
	)
	switch {
	case bytes.Equal(qualifier, []byte(recordCQV1)):
		if err := json.Unmarshal(encoded, &record); err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "decode explorer edge", err)
		}
		if err := validateLegacyPersistedEdge(record.Edge); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "stored explorer edge is invalid", err)
		}
		format = 1
	case bytes.Equal(qualifier, []byte(recordCQV2)):
		if err := decodeEmbeddedRecord(
			encoded, embeddedRecordEdge, &record,
		); err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "decode explorer edge", err)
		}
		if err := validatePersistedEdge(record.Edge); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "stored explorer edge is invalid", err)
		}
		if !bytes.Equal(row, edgeRecordRow(record.Edge.ID)) {
			return shoal.NewError(
				shoal.ErrorInternal, "stored explorer edge row is invalid")
		}
		format = 2
	default:
		return nil
	}
	if formats[record.Edge.ID] >= format {
		return nil
	}
	formats[record.Edge.ID] = format
	e.edges[record.Edge.ID] = record
	return nil
}

func (e *Explorer) loadInteractionRecord(row, qualifier, encoded []byte) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedInteraction
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordInteraction, &record,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode explorer interaction", err)
	}
	if err := validatePersistedInteraction(record); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored explorer interaction is invalid", err)
	}
	if !bytes.Equal(row, interactionRecordRow(record.SessionID)) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored explorer interaction row is invalid")
	}
	e.reserveInteractionRecordGraphIDsLocked(
		record.SessionID, record.Nodes, record.Edges)
	if !record.Deleted {
		if live, ok := e.interactionLiveRecords[record.SessionID]; ok {
			if !reflect.DeepEqual(*live, record) {
				return shoal.NewError(
					shoal.ErrorInternal,
					"stored interaction session has conflicting live versions",
				)
			}
		} else {
			live := record
			e.interactionLiveRecords[record.SessionID] = &live
		}
	}
	// A session row is written at most twice: once when the interaction is
	// recorded and once when it is explicitly deleted. The scan returns raw
	// cells, so both versions arrive here. Resolve without depending on scan
	// order: a tombstone is terminal because a deleted session ID can never
	// be reused, and otherwise the first cell seen wins.
	if existing, ok := e.interactions[record.SessionID]; ok {
		if existing.Deleted || !record.Deleted {
			return nil
		}
	}
	copy := record
	e.interactions[record.SessionID] = &copy
	return nil
}

func (e *Explorer) loadFoldRecord(row, qualifier, encoded []byte) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedFold
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordFold, &record,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode explorer fold", err)
	}
	if err := validatePersistedFold(record); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored explorer fold is invalid", err)
	}
	if !bytes.Equal(row, foldRecordRow(record.FoldID)) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored explorer fold row is invalid")
	}
	e.reserveInteractionRecordGraphIDsLocked(
		record.FoldID, record.Nodes, record.Edges)
	if !record.Deleted {
		if live, ok := e.foldLiveRecords[record.FoldID]; ok {
			if !persistedFoldsEqual(*live, record) {
				return shoal.NewError(
					shoal.ErrorInternal,
					"stored fold has conflicting live versions",
				)
			}
		} else {
			live := record
			e.foldLiveRecords[record.FoldID] = &live
		}
	}
	// A fold row is written at most twice: once when it is folded and once
	// when it is explicitly deleted. The scan returns raw cells, so both
	// versions arrive here. A tombstone is terminal because a deleted fold ID
	// can never be reused; otherwise the first cell seen wins.
	if existing, ok := e.folds[record.FoldID]; ok {
		if existing.Deleted || !record.Deleted {
			return nil
		}
	}
	copy := record
	e.folds[record.FoldID] = &copy
	return nil
}

func (e *Explorer) writeRecord(row []byte, kind byte, value any) error {
	encoded, err := encodeEmbeddedRecord(kind, value)
	if err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "encode explorer record", err)
	}
	return e.writeEncodedRecord(row, encoded)
}

func (e *Explorer) writeEncodedRecord(row, encoded []byte) error {
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "create explorer mutation", err)
	}
	mutation.PutLatest([]byte(recordCF), []byte(recordCQV2), nil, encoded)
	if err := e.engine.Write(explorerTable, []*cclient.Mutation{mutation}); err != nil {
		return MarkIndeterminateCommit(
			shoal.WrapError(shoal.ErrorUnavailable, "write explorer record", err),
		)
	}
	return nil
}

// writeInteractionRecord appends an interaction-family record and resolves a
// storage error whose commit outcome is indeterminate by reading the exact
// cell back. A byte-identical durable value is success; absence or a different
// value preserves the indeterminate error so inference still fails closed.
func (e *Explorer) writeInteractionRecord(
	row []byte, kind byte, value any,
) error {
	writer := e.interactionRecordWriter
	if writer == nil {
		writer = e.writeRecord
	}
	err := writer(row, kind, value)
	if err == nil || !IsIndeterminateCommit(err) {
		return err
	}
	expected, encodeErr := encodeEmbeddedRecord(kind, value)
	if encodeErr != nil {
		result := errors.Join(err, encodeErr)
		e.poisonIndeterminateInteractionStateLocked(result)
		return result
	}
	committed, readErr := e.hasExactRecord(row, expected)
	if readErr != nil {
		result := errors.Join(err, readErr)
		e.poisonIndeterminateInteractionStateLocked(result)
		return result
	}
	if committed {
		return nil
	}
	e.poisonIndeterminateInteractionStateLocked(err)
	return err
}

func (e *Explorer) createInteractionRecord(
	row []byte, kind byte, value any,
) (bool, error) {
	if e.interactionRecordWriter != nil {
		err := e.writeInteractionRecord(row, kind, value)
		return err == nil, err
	}
	return e.conditionalInteractionRecord(
		row, kind, value, recordCQV2, false)
}

func (e *Explorer) deleteInteractionRecord(
	row []byte, kind byte, value any,
) (bool, error) {
	if e.interactionRecordWriter != nil {
		err := e.writeInteractionRecord(row, kind, value)
		return err == nil, err
	}
	return e.conditionalInteractionRecord(
		row, kind, value, recordDeleteCQ, true)
}

func (e *Explorer) conditionalInteractionRecord(
	row []byte,
	kind byte,
	value any,
	conditionQualifier string,
	writeDeleteMarker bool,
) (bool, error) {
	encoded, err := encodeEmbeddedRecord(kind, value)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorInternal, "encode interaction record", err)
	}
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorInternal, "create interaction mutation", err)
	}
	mutation.PutLatest(
		[]byte(recordCF), []byte(recordCQV2), nil, encoded)
	if writeDeleteMarker {
		mutation.PutLatest(
			[]byte(recordCF), []byte(recordDeleteCQ), nil, []byte{1})
	}
	accepted, err := e.engine.ConditionalWrite(
		explorerTable,
		[]engine.ConditionalMutation{{
			Mutation: mutation,
			Conditions: []engine.Condition{{
				ColumnFamily:    []byte(recordCF),
				ColumnQualifier: []byte(conditionQualifier),
				Kind:            engine.ConditionAbsent,
			}},
		}},
	)
	if err == nil {
		if len(accepted) != 1 {
			return false, shoal.NewError(
				shoal.ErrorInternal,
				"interaction conditional write returned an invalid result",
			)
		}
		return accepted[0], nil
	}
	indeterminate := MarkIndeterminateCommit(
		shoal.WrapError(
			shoal.ErrorUnavailable,
			"conditionally write interaction record",
			err,
		),
	)
	committed, readErr := e.hasExactRecord(row, encoded)
	if readErr != nil {
		result := errors.Join(indeterminate, readErr)
		e.poisonIndeterminateInteractionStateLocked(result)
		return false, result
	}
	if committed {
		return true, nil
	}
	winnerQualifier := recordCQV2
	if writeDeleteMarker {
		winnerQualifier = recordDeleteCQ
	}
	found, readErr := e.hasCurrentQualifier(row, winnerQualifier)
	if readErr != nil {
		result := errors.Join(indeterminate, readErr)
		e.poisonIndeterminateInteractionStateLocked(result)
		return false, result
	}
	if found {
		return false, nil
	}
	e.poisonIndeterminateInteractionStateLocked(indeterminate)
	return false, indeterminate
}

func (e *Explorer) poisonIndeterminateInteractionStateLocked(err error) {
	if err == nil {
		return
	}
	e.indeterminateInteractionErr = shoal.WrapError(
		shoal.ErrorUnavailable,
		"interaction storage outcome is indeterminate; close and reopen the corpus before further use",
		err,
	)
}

func (e *Explorer) hasExactRecord(row, expected []byte) (bool, error) {
	committed := false
	examined := false
	err := e.engine.LookupRows(
		explorerTable,
		[][]byte{append([]byte(nil), row...)},
		engine.ScanOptions{
			ColumnFamilies:          [][]byte{[]byte(recordCF)},
			ColumnFamiliesInclusive: true,
		},
		func(_ int, key *iterrt.Key, value []byte) {
			if examined ||
				!bytes.Equal(key.ColumnQualifier, []byte(recordCQV2)) {
				return
			}
			examined = true
			committed = equivalentEmbeddedRecord(
				key.Row, value, expected)
		},
	)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorUnavailable,
			"verify indeterminate interaction write",
			err,
		)
	}
	return committed, nil
}

func (e *Explorer) hasCurrentQualifier(
	row []byte, qualifier string,
) (bool, error) {
	found := false
	err := e.engine.LookupRows(
		explorerTable,
		[][]byte{append([]byte(nil), row...)},
		engine.ScanOptions{
			ColumnFamilies:          [][]byte{[]byte(recordCF)},
			ColumnFamiliesInclusive: true,
		},
		func(_ int, key *iterrt.Key, _ []byte) {
			if !found &&
				bytes.Equal(key.ColumnQualifier, []byte(qualifier)) {
				found = true
			}
		},
	)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorUnavailable,
			"read current interaction record",
			err,
		)
	}
	return found, nil
}

func (e *Explorer) lookupPersistedInteraction(
	sessionID shoal.ID,
) (persistedInteraction, bool, error) {
	var record persistedInteraction
	found, err := e.lookupEmbeddedRecord(
		interactionRecordRow(sessionID),
		embeddedRecordInteraction,
		&record,
	)
	if err != nil || !found {
		return persistedInteraction{}, found, err
	}
	if err := validatePersistedInteraction(record); err != nil {
		return persistedInteraction{}, false, shoal.WrapError(
			shoal.ErrorInternal,
			"stored explorer interaction is invalid",
			err,
		)
	}
	if record.SessionID != sessionID {
		return persistedInteraction{}, false, shoal.NewError(
			shoal.ErrorInternal,
			"stored explorer interaction row is invalid",
		)
	}
	return record, true, nil
}

func (e *Explorer) lookupPersistedLiveInteraction(
	sessionID shoal.ID,
) (persistedInteraction, bool, error) {
	var live persistedInteraction
	found := false
	var decodeErr error
	row := interactionRecordRow(sessionID)
	err := e.engine.LookupRows(
		explorerTable,
		[][]byte{append([]byte(nil), row...)},
		engine.ScanOptions{
			ColumnFamilies:          [][]byte{[]byte(recordCF)},
			ColumnFamiliesInclusive: true,
		},
		func(_ int, key *iterrt.Key, value []byte) {
			if decodeErr != nil ||
				!bytes.Equal(key.ColumnQualifier, []byte(recordCQV2)) {
				return
			}
			var candidate persistedInteraction
			if err := decodeEmbeddedRecord(
				value, embeddedRecordInteraction, &candidate,
			); err != nil {
				decodeErr = err
				return
			}
			if err := validatePersistedInteraction(candidate); err != nil {
				decodeErr = err
				return
			}
			if candidate.SessionID != sessionID ||
				!bytes.Equal(row, interactionRecordRow(candidate.SessionID)) {
				decodeErr = errors.New(
					"stored explorer interaction row is invalid")
				return
			}
			if candidate.Deleted {
				return
			}
			if found && !persistedInteractionsEqual(live, candidate) {
				decodeErr = errors.New(
					"stored interaction session has conflicting live versions")
				return
			}
			live = candidate
			found = true
		},
	)
	if err != nil {
		return persistedInteraction{}, false, shoal.WrapError(
			shoal.ErrorUnavailable,
			"read historical interaction record",
			err,
		)
	}
	if decodeErr != nil {
		return persistedInteraction{}, false, shoal.WrapError(
			shoal.ErrorInternal,
			"stored historical interaction is invalid",
			decodeErr,
		)
	}
	return live, found, nil
}

func (e *Explorer) lookupPersistedFold(
	foldID shoal.ID,
) (persistedFold, bool, error) {
	var record persistedFold
	found, err := e.lookupEmbeddedRecord(
		foldRecordRow(foldID),
		embeddedRecordFold,
		&record,
	)
	if err != nil || !found {
		return persistedFold{}, found, err
	}
	if err := validatePersistedFold(record); err != nil {
		return persistedFold{}, false, shoal.WrapError(
			shoal.ErrorInternal,
			"stored explorer fold is invalid",
			err,
		)
	}
	if record.FoldID != foldID {
		return persistedFold{}, false, shoal.NewError(
			shoal.ErrorInternal,
			"stored explorer fold row is invalid",
		)
	}
	return record, true, nil
}

func (e *Explorer) lookupEmbeddedRecord(
	row []byte, kind byte, target any,
) (bool, error) {
	found := false
	var decodeErr error
	err := e.engine.LookupRows(
		explorerTable,
		[][]byte{append([]byte(nil), row...)},
		engine.ScanOptions{
			ColumnFamilies:          [][]byte{[]byte(recordCF)},
			ColumnFamiliesInclusive: true,
		},
		func(_ int, key *iterrt.Key, value []byte) {
			if found || decodeErr != nil ||
				!bytes.Equal(key.ColumnQualifier, []byte(recordCQV2)) {
				return
			}
			decodeErr = decodeEmbeddedRecord(value, kind, target)
			found = decodeErr == nil
		},
	)
	if err != nil {
		return false, shoal.WrapError(
			shoal.ErrorUnavailable,
			"read committed interaction record",
			err,
		)
	}
	if decodeErr != nil {
		return false, shoal.WrapError(
			shoal.ErrorInternal,
			"decode committed interaction record",
			decodeErr,
		)
	}
	return found, nil
}

func equivalentEmbeddedRecord(row, stored, expected []byte) bool {
	if bytes.Equal(stored, expected) {
		return true
	}
	switch {
	case bytes.Equal(row, interactionSinkRow):
		var left, right persistedInteractionSink
		return decodeEmbeddedRecord(
			stored, embeddedRecordInteractionSink, &left,
		) == nil &&
			decodeEmbeddedRecord(
				expected, embeddedRecordInteractionSink, &right,
			) == nil &&
			reflect.DeepEqual(left, right)
	case bytes.HasPrefix(row, []byte(interactionRow)):
		var left, right persistedInteraction
		return decodeEmbeddedRecord(
			stored, embeddedRecordInteraction, &left,
		) == nil &&
			decodeEmbeddedRecord(
				expected, embeddedRecordInteraction, &right,
			) == nil &&
			reflect.DeepEqual(left, right)
	case bytes.HasPrefix(row, []byte(foldRow)):
		var left, right persistedFold
		return decodeEmbeddedRecord(
			stored, embeddedRecordFold, &left,
		) == nil &&
			decodeEmbeddedRecord(
				expected, embeddedRecordFold, &right,
			) == nil &&
			reflect.DeepEqual(left, right)
	case bytes.HasPrefix(row, []byte(snapshotRow)):
		var left, right persistedSnapshot
		return decodeEmbeddedRecord(
			stored, embeddedRecordSnapshot, &left,
		) == nil &&
			decodeEmbeddedRecord(
				expected, embeddedRecordSnapshot, &right,
			) == nil &&
			persistedSnapshotsEqual(left, right)
	default:
		return false
	}
}

func persistedSnapshotsEqual(left, right persistedSnapshot) bool {
	return left.ID == right.ID &&
		left.AsOf.UTC().Equal(right.AsOf.UTC()) &&
		left.ParentID == right.ParentID &&
		reflect.DeepEqual(left.AddedNodeIDs, right.AddedNodeIDs) &&
		reflect.DeepEqual(left.RemovedNodeIDs, right.RemovedNodeIDs) &&
		reflect.DeepEqual(left.NodeStates, right.NodeStates) &&
		reflect.DeepEqual(left.RemovedEdgeIDs, right.RemovedEdgeIDs) &&
		reflect.DeepEqual(left.EdgeStates, right.EdgeStates) &&
		reflect.DeepEqual(left.AssertionStates, right.AssertionStates) &&
		reflect.DeepEqual(left.RemovedAssertionEdgeIDs, right.RemovedAssertionEdgeIDs)
}

func encodeEmbeddedRecord(kind byte, value any) ([]byte, error) {
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(value); err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	maximum, err := embeddedRecordMaximum(kind)
	if err != nil {
		return nil, err
	}
	if uint64(payload.Len()) > maximum {
		return nil, fmt.Errorf("payload exceeds embedded record bound")
	}
	encoded := make([]byte, embeddedEnvelopeHeader+payload.Len())
	copy(encoded, embeddedRecordMagic)
	encoded[8] = embeddedEnvelopeVersion
	encoded[9] = kind
	binary.BigEndian.PutUint64(encoded[10:18], uint64(payload.Len()))
	checksum := sha256.Sum256(payload.Bytes())
	copy(encoded[18:18+sha256.Size], checksum[:])
	copy(encoded[embeddedEnvelopeHeader:], payload.Bytes())
	return encoded, nil
}

func decodeEmbeddedRecord(encoded []byte, expectedKind byte, destination any) error {
	if len(encoded) < embeddedEnvelopeHeader {
		return fmt.Errorf("embedded record envelope is truncated")
	}
	if !bytes.Equal(encoded[:8], []byte(embeddedRecordMagic)) {
		return fmt.Errorf("embedded record magic is invalid")
	}
	if encoded[8] != embeddedEnvelopeVersion {
		return fmt.Errorf("embedded record envelope version %d is unsupported", encoded[8])
	}
	if encoded[9] != expectedKind {
		return fmt.Errorf("embedded record kind %d is invalid", encoded[9])
	}
	maximum, err := embeddedRecordMaximum(expectedKind)
	if err != nil {
		return err
	}
	payloadLength := binary.BigEndian.Uint64(encoded[10:18])
	if payloadLength > maximum {
		return fmt.Errorf("embedded record payload exceeds its bound")
	}
	if payloadLength != uint64(len(encoded)-embeddedEnvelopeHeader) {
		return fmt.Errorf("embedded record payload length is invalid")
	}
	payload := encoded[embeddedEnvelopeHeader:]
	checksum := sha256.Sum256(payload)
	if !bytes.Equal(encoded[18:18+sha256.Size], checksum[:]) {
		return fmt.Errorf("embedded record checksum is invalid")
	}
	decoder := gob.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("embedded record payload has trailing data")
	}
	return nil
}

func embeddedRecordMaximum(kind byte) (uint64, error) {
	switch kind {
	case embeddedRecordDocument:
		return maxEmbeddedDocumentBytes, nil
	case embeddedRecordEdge:
		return maxEmbeddedEdgeBytes, nil
	case embeddedRecordSnapshotAnchor:
		return maxEmbeddedSnapshotAnchorBytes, nil
	case embeddedRecordInteraction:
		return maxEmbeddedInteractionBytes, nil
	case embeddedRecordInteractionSink:
		return maxEmbeddedInteractionSinkBytes, nil
	case embeddedRecordFold:
		return maxEmbeddedFoldBytes, nil
	case embeddedRecordCursorKey:
		return maxEmbeddedCursorKeyBytes, nil
	case embeddedRecordOntologyProposal:
		return maxEmbeddedOntologyProposalBytes, nil
	case embeddedRecordProposalTransition:
		return maxEmbeddedProposalTransitionBytes, nil
	case embeddedRecordExtraction:
		return maxEmbeddedExtractionBytes, nil
	case embeddedRecordSnapshot:
		return maxEmbeddedSnapshotBytes, nil
	case embeddedRecordGraphMaterialization:
		return maxEmbeddedGraphMaterializationBytes, nil
	default:
		return 0, fmt.Errorf("embedded record kind %d is unsupported", kind)
	}
}

func validateLegacyPersistedDocument(record persistedDocument) error {
	if record.Document.ID == "" || record.Revision.ID == "" {
		return fmt.Errorf("document identity is incomplete")
	}
	return nil
}

func validatePersistedDocument(record persistedDocument) error {
	if record.PublicationSequence == 0 {
		return fmt.Errorf("publication sequence is missing")
	}
	if !utf8.ValidString(record.Source.URI) ||
		strings.TrimSpace(record.Source.URI) == "" ||
		!utf8.ValidString(record.Source.Title) ||
		!utf8.ValidString(record.Source.Content) ||
		!utf8.ValidString(record.Source.MediaType) {
		return fmt.Errorf("source semantic text is invalid")
	}
	switch record.Source.MediaType {
	case MediaTypeMarkdown, MediaTypeText, MediaTypeSource:
	default:
		return fmt.Errorf("source media type is invalid")
	}
	if err := shoal.ValidateMetadata("source metadata", record.Source.Metadata); err != nil {
		return err
	}
	if err := document.ValidateRevisionContent(
		record.Source.Content,
		record.Document,
		record.Revision,
		record.Sections,
		record.Spans,
	); err != nil {
		return err
	}
	for _, node := range record.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
		if !utf8.ValidString(node.Kind) {
			return fmt.Errorf("graph node kind is invalid")
		}
		if interaction.IsInteractionID(node.ID) {
			return fmt.Errorf(
				"content cannot use the reserved interaction node ID namespace")
		}
		if interaction.IsInteractionKind(node.Kind) {
			return fmt.Errorf("content cannot use the reserved interaction node kind namespace")
		}
		for _, label := range node.Labels {
			if !utf8.ValidString(label) {
				return fmt.Errorf("graph node label is invalid")
			}
		}
	}
	for _, edge := range record.Edges {
		if err := validatePersistedEdge(edge); err != nil {
			return err
		}
		if interaction.IsInteractionID(edge.ID) {
			return fmt.Errorf(
				"content cannot use the reserved interaction edge ID namespace")
		}
		if interaction.IsInteractionEdgeType(edge.Type) {
			return fmt.Errorf("content cannot use the reserved interaction edge type namespace")
		}
	}
	if err := validateEmbeddingSet(&record); err != nil {
		return err
	}
	return nil
}
