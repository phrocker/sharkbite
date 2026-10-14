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

package authorized

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ChangeFeedRequest asks for the caller's authorized document change feed.
// Cursor is the opaque, sealed token returned by a prior page; an empty cursor
// starts from the beginning of retained history. It is a ciphertext, not a
// number: a client can neither read the sequence it carries nor forge one, so
// the feed cannot be turned into an oracle over other users' write sequence.
type ChangeFeedRequest struct {
	Cursor string
	Limit  int
}

// ChangeFeedPage is one authorized, ordered, resumable window.
type ChangeFeedPage struct {
	// Changes are visible to this caller, ascending by Sequence, all beyond the
	// request cursor's position.
	Changes []explorer.DocumentChange
	// Cursor is the sealed resume token. It advances only along changes the
	// caller can see, so it never encodes how many withheld changes were
	// skipped, and it is authenticated so it can be neither tampered with nor
	// forged. Replaying it resumes exactly at the first undelivered visible
	// change.
	Cursor string
	// More reports that at least one further visible change exists beyond this
	// page. It is a bounded liveness hint and never a count.
	More bool
}

// changeScanBatch bounds one base feed pull. The authorized feed may pull
// several batches to fill a page across withheld runs.
const changeScanBatch = explorer.MaxChangeLimit

// changeCursorPayload is the plaintext sealed inside an opaque cursor. Sequence
// is a decimal string so it round-trips exactly. Incarnation is retained for a
// defence-in-depth corpus-identity binding even though the per-corpus seal key
// already prevents a cursor from opening against another corpus.
type changeCursorPayload struct {
	Incarnation string `json:"incarnation"`
	Sequence    string `json:"sequence"`
}

// Changes returns the caller's authorized document change feed.
//
// Authorization posture. Every candidate change is passed through the same
// exact-revision catalog gate the read path enforces: a change is delivered
// only when this identity holds a policy registration for that precise revision
// and its rule authorizes listing. A caller therefore learns nothing about
// publications to documents it cannot see.
//
// No withheld count is reported, and that is a deliberate departure from the
// Documents and Retrieve listings, which disclose a corpus-wide suppressed
// count (PR #292). A feed is polled repeatedly with an advancing cursor, so a
// per-page withheld count -- or, equivalently, a cursor that advanced past
// withheld changes -- would let a caller reconstruct the timing and volume of
// other users' private writes at sequence resolution, a far sharper oracle than
// the one-shot corpus count #292 accepted. Instead the cursor advances only
// along visible changes and More is a bare liveness hint, so a caught-up caller
// (More false, no visible tail) can never mistake withheld activity for its own
// silence, and can never quantify it.
//
// That property only holds if the cursor is genuinely opaque. The cursor is
// therefore sealed with authenticated encryption (AES-256-GCM) under a durable
// per-corpus key: the client cannot read the sequence it carries (closing the
// differencing oracle where cursor deltas reveal withheld volume) and cannot
// forge one (closing the oracle where a hand-crafted Since is binary-searched
// against the global head-conflict boundary). The only Since values that reach
// the base feed come from cursors this layer itself sealed.
func (c *Client) Changes(
	ctx context.Context, request ChangeFeedRequest,
) (ChangeFeedPage, error) {
	reader, ok := c.base.(explorer.ChangeReader)
	if !ok {
		return ChangeFeedPage{}, shoal.NewError(
			shoal.ErrorUnavailable, "change feed is unavailable")
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationList)
	if err != nil {
		return ChangeFeedPage{}, err
	}

	sealer, err := c.changeCursorSealer(ctx, reader)
	if err != nil {
		return ChangeFeedPage{}, err
	}
	since, incarnation, err := sealer.open(request.Cursor)
	if err != nil {
		return ChangeFeedPage{}, err
	}

	pageSize := request.Limit
	if pageSize <= 0 {
		pageSize = explorer.DefaultChangeLimit
	}
	if pageSize > explorer.MaxChangeLimit {
		pageSize = explorer.MaxChangeLimit
	}

	changes := make([]explorer.DocumentChange, 0, pageSize)
	next := since
	feedIncarnation := incarnation
	rawSince := since
	more := false
scan:
	for {
		feed, err := reader.Changes(ctx, explorer.ChangeRequest{
			Since:               rawSince,
			Limit:               changeScanBatch,
			ExpectedIncarnation: incarnation,
		})
		if err != nil {
			return ChangeFeedPage{}, err
		}
		feedIncarnation = feed.Incarnation
		for index := range feed.Changes {
			change := feed.Changes[index]
			visible, err := c.changeVisible(ctx, decision, change, now)
			if err != nil {
				return ChangeFeedPage{}, err
			}
			if !visible {
				continue
			}
			// A visible change found once the page is full proves more visible
			// changes remain. It is not delivered, so next stays at the last
			// delivered change and the next poll resumes exactly here.
			if len(changes) == pageSize {
				more = true
				break scan
			}
			changes = append(changes, cloneDocumentChange(change))
			next = change.Sequence
		}
		rawSince = feed.Cursor
		if !feed.More {
			// The base feed is exhausted to Head, so the page holds every
			// visible change and the caller is caught up on what it can see.
			break
		}
	}
	if err := guard.Check(ctx); err != nil {
		return ChangeFeedPage{}, err
	}

	cursor, err := sealer.seal(feedIncarnation, next)
	if err != nil {
		return ChangeFeedPage{}, err
	}
	return ChangeFeedPage{Changes: changes, Cursor: cursor, More: more}, nil
}

// changeVisible applies the exact-revision authorization gate. It is the same
// decision the Documents listing and the Document read enforce: presence of a
// registration for this precise revision, then its rule under OperationList.
func (c *Client) changeVisible(
	ctx context.Context,
	decision auth.Decision,
	change explorer.DocumentChange,
	now time.Time,
) (bool, error) {
	if err := validateChange(change); err != nil {
		return false, inconsistentBase()
	}
	registration, ok, err := c.policyStore.Revision(
		ctx, change.Document.ID, change.Revision.ID)
	if err != nil {
		return false, policyCatalogReadError(ctx, err)
	}
	if !ok {
		// No registration for this exact revision: withheld from every caller,
		// exactly as an uncataloged current revision is withheld from the
		// Documents listing. Not counted, never named.
		return false, nil
	}
	if registration.DocumentID != change.Document.ID ||
		registration.RevisionID != change.Revision.ID {
		return false, inconsistentBase()
	}
	allowed, err := c.revisionAllows(
		ctx, registration, decision, auth.OperationList, now)
	if err != nil {
		return false, err
	}
	return allowed, nil
}

func validateChange(change explorer.DocumentChange) error {
	if change.Kind != explorer.ChangeKindDocumentPublished {
		return inconsistentBase()
	}
	if change.Sequence == 0 {
		return inconsistentBase()
	}
	if err := change.Document.Validate(); err != nil {
		return err
	}
	if err := change.Revision.Validate(); err != nil {
		return err
	}
	if change.Document.RevisionID != change.Revision.ID ||
		change.Revision.DocumentID != change.Document.ID {
		return inconsistentBase()
	}
	return nil
}

func cloneDocumentChange(change explorer.DocumentChange) explorer.DocumentChange {
	change.Document = cloneDocument(change.Document)
	change.Revision = cloneRevision(change.Revision)
	return change
}

// changeCursorSealer builds the authenticated-encryption sealer for this
// corpus's change cursors from the base reader's durable per-corpus key.
func (c *Client) changeCursorSealer(
	ctx context.Context, reader explorer.ChangeReader,
) (*cursorSealer, error) {
	key, err := reader.ChangeCursorSealKey(ctx)
	if err != nil {
		return nil, err
	}
	return newCursorSealer(key)
}

// cursorSealer seals and opens change cursors with AES-256-GCM.
type cursorSealer struct {
	aead cipher.AEAD
}

func newCursorSealer(key []byte) (*cursorSealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "initialize change cursor cipher", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "initialize change cursor sealer", err)
	}
	return &cursorSealer{aead: aead}, nil
}

// seal produces the opaque, authenticated resume token for a position. A random
// nonce is prepended so identical positions never produce identical tokens.
func (s *cursorSealer) seal(incarnation string, sequence uint64) (string, error) {
	plaintext, err := json.Marshal(changeCursorPayload{
		Incarnation: incarnation,
		Sequence:    strconv.FormatUint(sequence, 10),
	})
	if err != nil {
		return "", shoal.WrapError(
			shoal.ErrorInternal, "encode change cursor", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", shoal.WrapError(
			shoal.ErrorInternal, "generate change cursor nonce", err)
	}
	sealed := s.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open authenticates and decrypts a cursor into its resume position. An empty
// cursor starts from the beginning. Any cursor that fails to decode or
// authenticate -- a tampered token, or one minted against another corpus's key
// -- is refused with a single indistinguishable invalid-cursor error, so a
// client learns nothing from a rejected forgery attempt.
func (s *cursorSealer) open(cursor string) (uint64, string, error) {
	if cursor == "" {
		return 0, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", errInvalidChangeCursor()
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize {
		return 0, "", errInvalidChangeCursor()
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return 0, "", errInvalidChangeCursor()
	}
	var payload changeCursorPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return 0, "", errInvalidChangeCursor()
	}
	sequence, err := strconv.ParseUint(payload.Sequence, 10, 64)
	if err != nil {
		return 0, "", errInvalidChangeCursor()
	}
	return sequence, payload.Incarnation, nil
}

// errInvalidChangeCursor is returned for every malformed, tampered, or foreign
// cursor. It is deliberately uniform: distinguishing "bad base64" from "failed
// authentication" from "wrong corpus" would hand a forger a signal, so all
// open failures collapse to one error. This is distinct from the base feed's
// too-old and head-conflict resynchronise diagnostics, which only fire for
// cursors that opened successfully.
func errInvalidChangeCursor() error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument, "change cursor is invalid; resynchronise")
}
