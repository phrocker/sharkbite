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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The change feed is built on the durable, monotonic publication sequence that
// Ingest already assigns to every applied revision (see lastPublicationSequence
// in IngestWithOptions and its recovery in loadDocumentRecord). That sequence,
// not the content-digest Snapshot.Frontier, is the feed's clock: the digest can
// only tell two corpus states apart, it cannot order them or answer "everything
// after X". The publication sequence is a total order over document
// publications and survives restart, which is exactly what a resumable cursor
// requires.
//
// The feed reports document publications only. Application edges (Connect) are
// stamped with wall-clock PublishedAt, which is neither monotonic nor
// restart-distinct, so there is no authoritative order across edges to fold
// into the same cursor; interaction and fold records are deliberately excluded
// from the corpus frontier entirely. Inventing a single global order across
// those kinds would be a fiction, so this is an honest per-kind (document)
// feed.

const (
	// DefaultChangeLimit bounds a page when the caller requests none.
	DefaultChangeLimit = 128
	// MaxChangeLimit is the largest raw window a single call will materialize.
	MaxChangeLimit = 1024
	// changeCursorKeyBytes is the size of the durable AES-256 key that seals
	// change-feed cursors.
	changeCursorKeyBytes = 32
)

// ChangeKind classifies one reported change. Only document publications are
// reported today; the kind is explicit so future kinds cannot be mistaken for
// publications by an older client.
type ChangeKind string

// ChangeKindDocumentPublished marks a newly published document revision.
const ChangeKindDocumentPublished ChangeKind = "document_published"

// DocumentChange is one ordered document publication. Sequence is the durable
// publication-sequence position that a cursor resumes from.
type DocumentChange struct {
	Sequence        uint64
	Kind            ChangeKind
	Document        document.Document
	Revision        document.Revision
	SourceURI       string
	SourceMediaType string
}

// ChangeRequest asks for publications strictly after Since, bounded by Limit.
// ExpectedIncarnation, when set, must equal the corpus incarnation the cursor
// was minted against; a mismatch is reported as a resynchronise conflict rather
// than silently answered from an unrelated corpus.
type ChangeRequest struct {
	Since               uint64
	Limit               int
	ExpectedIncarnation string
}

// ChangeFeed is an ordered, resumable window of document publications.
type ChangeFeed struct {
	// Changes are ascending by Sequence and all have Sequence > request.Since.
	Changes []DocumentChange
	// Cursor is the highest sequence examined by this call: the last returned
	// change's sequence, or request.Since when the window is empty. A caller
	// paging raw changes resumes from Cursor.
	Cursor uint64
	// More reports whether at least one further publication exists beyond
	// Cursor. It is derived from retained records, not from Head, so a run of
	// never-committed (holed) sequence numbers does not fabricate a change.
	More bool
	// Head is the highest publication sequence ever assigned.
	Head uint64
	// Floor is the lowest sequence the feed can still answer. request.Since must
	// be at least Floor-1; a lower cursor cannot be served without a silent gap
	// and is refused. Today nothing is pruned, so Floor is 1.
	Floor uint64
	// Incarnation is a stable, opaque identity for this corpus. A cursor is
	// bound to it so a cursor minted against a different corpus is refused
	// instead of being answered from unrelated data.
	Incarnation string
}

// ChangeReader is the optional embedded feed capability. It is a separate
// interface, not part of Client, so remote or reduced backends that cannot
// serve an ordered feed simply do not implement it and callers fail closed.
// ChangeCursorSealKey returns the durable per-corpus secret an authorization
// layer uses to seal opaque, unforgeable cursors; it is exposed here so the raw
// sequence positions never cross the API boundary in the clear.
type ChangeReader interface {
	Changes(context.Context, ChangeRequest) (ChangeFeed, error)
	ChangeCursorSealKey(context.Context) ([]byte, error)
}

// Changes returns an ordered, resumable window of document publications with
// Sequence strictly greater than request.Since.
func (e *Explorer) Changes(
	ctx context.Context, request ChangeRequest,
) (ChangeFeed, error) {
	if err := contextError(ctx); err != nil {
		return ChangeFeed{}, err
	}
	limit := request.Limit
	if limit <= 0 {
		limit = DefaultChangeLimit
	}
	if limit > MaxChangeLimit {
		limit = MaxChangeLimit
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return ChangeFeed{}, err
	}

	head := e.lastPublicationSequence
	floor := e.changeHistoryFloorLocked()
	incarnation := e.incarnationLocked()

	if request.ExpectedIncarnation != "" &&
		request.ExpectedIncarnation != incarnation {
		return ChangeFeed{}, shoal.NewError(
			shoal.ErrorConflict,
			"change cursor belongs to another corpus; resynchronise",
		)
	}
	if request.Since > head {
		return ChangeFeed{}, shoal.NewError(
			shoal.ErrorConflict,
			"change cursor is ahead of the corpus; resynchronise",
		)
	}
	// Since is an exclusive lower bound, so the earliest change it can name is
	// Since+1. If retention has advanced the floor past that point, the changes
	// between the cursor and the floor are gone and answering would silently
	// drop them.
	if request.Since+1 < floor {
		return ChangeFeed{}, shoal.NewError(
			shoal.ErrorConflict,
			"change cursor is older than the retained history floor; resynchronise",
		)
	}

	pending := make([]DocumentChange, 0, limit+1)
	for _, revisions := range e.documents {
		for _, record := range revisions {
			// Legacy revisions predate the publication sequence and carry 0.
			// They cannot be ordered and are never part of the feed; the floor
			// is the first real sequence, so a cursor never expects them.
			if record.PublicationSequence == 0 {
				continue
			}
			if record.PublicationSequence <= request.Since {
				continue
			}
			pending = append(pending, DocumentChange{
				Sequence:        record.PublicationSequence,
				Kind:            ChangeKindDocumentPublished,
				Document:        cloneDocument(record.Document),
				Revision:        cloneRevision(record.Revision),
				SourceURI:       record.Source.URI,
				SourceMediaType: record.Source.MediaType,
			})
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Sequence < pending[j].Sequence
	})

	feed := ChangeFeed{
		Cursor:      request.Since,
		Head:        head,
		Floor:       floor,
		Incarnation: incarnation,
	}
	if len(pending) > limit {
		feed.Changes = pending[:limit]
		feed.More = true
	} else {
		feed.Changes = pending
	}
	if n := len(feed.Changes); n > 0 {
		feed.Cursor = feed.Changes[n-1].Sequence
	}
	return feed, nil
}

// changeHistoryFloorLocked reports the lowest publication sequence the feed can
// still fully answer. The embedded corpus never prunes document revisions, so
// the floor is the first assigned sequence. It is a field rather than a
// constant so a future retention pass can raise it and the cursor-too-old path
// stays honest.
func (e *Explorer) changeHistoryFloorLocked() uint64 {
	if e.changeHistoryFloor == 0 {
		return 1
	}
	return e.changeHistoryFloor
}

// incarnationLocked derives a stable, opaque corpus identity from the durable
// snapshot anchor written once at corpus creation. Hashing hides the raw
// creation time while keeping the value stable across restarts and distinct
// across corpora.
func (e *Explorer) incarnationLocked() string {
	sum := sha256.Sum256(append(
		[]byte("explorer-change-feed-incarnation-v1\x00"),
		[]byte(e.snapshotAnchor.UTC().Format(time.RFC3339Nano))...,
	))
	return hex.EncodeToString(sum[:])
}

// ChangeCursorSealKey returns a copy of the durable per-corpus key used to seal
// change-feed cursors. It is corpus state, generated once at creation and
// stable across restart, so an authorization layer can mint opaque cursors that
// survive reconnects yet cannot be read or forged by a client.
//
// What this key is and is not, for the operator who finds a 32-byte secret in
// the state directory: it protects only the confidentiality and integrity of
// change-feed cursors. It is NOT an access-control secret. Possessing, losing,
// or leaking it grants no access to any document -- the per-change
// authorization gate in the authorized layer runs on every reported change
// regardless of the cursor, so a forged or replayed cursor still cannot surface
// a change the caller may not see. Its only job is to stop a client from
// reading or forging the sequence positions a cursor encodes. Consequently a
// wiped or recreated corpus mints a fresh key, and stale cursors sealed under
// the old key then fail authentication and are told to resynchronise rather
// than being answered from unrelated data.
func (e *Explorer) ChangeCursorSealKey(ctx context.Context) ([]byte, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	if len(e.changeCursorKey) != changeCursorKeyBytes {
		return nil, shoal.NewError(
			shoal.ErrorInternal, "change cursor key is unavailable")
	}
	return append([]byte(nil), e.changeCursorKey...), nil
}
