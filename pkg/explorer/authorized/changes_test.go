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

package authorized_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// seedInterleaved ingests a visible (policyA) document, a hidden (policyB)
// document, then another visible document, so the shared publication sequence
// is 1=visible, 2=hidden, 3=visible. It returns the two visible document IDs.
func seedInterleaved(t *testing.T, f *fixture) (shoal.ID, shoal.ID) {
	t.Helper()
	first, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///feed-visible-1.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "visible one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///feed-hidden.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "hidden secret",
	}); err != nil {
		t.Fatal(err)
	}
	second, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///feed-visible-2.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "visible two",
	})
	if err != nil {
		t.Fatal(err)
	}
	return first.Document.ID, second.Document.ID
}

func TestChangesExcludesUnauthorizedPublicationsWithoutLeakingThem(t *testing.T) {
	f := newFixture(t)
	visible1, visible2 := seedInterleaved(t, f)

	page, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// Alice sees exactly the two policyA publications and nothing about the
	// interleaved policyB publication at sequence 2.
	if len(page.Changes) != 2 {
		t.Fatalf("visible changes = %d, want 2 (%+v)", len(page.Changes), page.Changes)
	}
	if page.Changes[0].Document.ID != visible1 ||
		page.Changes[1].Document.ID != visible2 {
		t.Fatalf("visible docs = %s,%s want %s,%s",
			page.Changes[0].Document.ID, page.Changes[1].Document.ID,
			visible1, visible2)
	}
	// The delivered changes carry the true underlying sequences 1 and 3; the
	// withheld sequence 2 is simply skipped, never surfaced.
	if page.Changes[0].Sequence != 1 || page.Changes[1].Sequence != 3 {
		t.Fatalf("visible sequences = %d,%d want 1,3",
			page.Changes[0].Sequence, page.Changes[1].Sequence)
	}
	if page.More {
		t.Fatalf("More = true, want false: alice has seen every visible change")
	}
	// The cursor is a sealed ciphertext, not a readable number. Decoding it as
	// the old base64(JSON) envelope must not reveal a sequence: that was the
	// differencing oracle this design exists to close.
	if seq, ok := peekCursorSequence(page.Cursor); ok {
		t.Fatalf("cursor decoded to a readable sequence %d; it must be opaque", seq)
	}

	// Resuming from the returned cursor yields nothing new and still reports no
	// withheld activity: a caught-up caller cannot distinguish "nothing changed"
	// from "changes it may not see occurred".
	resumed, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{
		Cursor: page.Cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Changes) != 0 || resumed.More {
		t.Fatalf("resumed = %+v, want empty and More=false", resumed)
	}
}

func TestChangesResumesExactlyAcrossWithheldPublications(t *testing.T) {
	f := newFixture(t)
	visible1, visible2 := seedInterleaved(t, f)

	// A one-item page must return the first visible change and, because a second
	// visible change (sequence 3) still exists beyond the withheld sequence 2,
	// report More without disclosing the withheld item.
	first, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{
		Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Changes) != 1 || first.Changes[0].Document.ID != visible1 {
		t.Fatalf("first page = %+v, want single visible doc %s", first.Changes, visible1)
	}
	if first.Changes[0].Sequence != 1 {
		t.Fatalf("first change sequence = %d, want 1", first.Changes[0].Sequence)
	}
	if !first.More {
		t.Fatal("More = false, want true: a later visible change remains")
	}

	// Resuming from the visible cursor delivers the next visible change exactly,
	// skipping the withheld sequence 2 with no gap and no duplicate.
	second, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{
		Cursor: first.Cursor,
		Limit:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Changes) != 1 || second.Changes[0].Document.ID != visible2 {
		t.Fatalf("second page = %+v, want single visible doc %s", second.Changes, visible2)
	}
	if second.Changes[0].Sequence != 3 {
		t.Fatalf("second change sequence = %d, want 3 (withheld 2 skipped)",
			second.Changes[0].Sequence)
	}
	if second.More {
		t.Fatal("More = true, want false: no visible change remains")
	}
}

func TestChangesAdminSeesBothPartitions(t *testing.T) {
	f := newFixture(t)
	seedInterleaved(t, f)

	page, err := f.clientA.Changes(f.admin(t), authorized.ChangeFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Admin holds both policies, so every publication is visible and the cursor
	// reaches the true head. This proves the withholding above is authorization,
	// not an artifact of the feed dropping changes.
	if len(page.Changes) != 3 {
		t.Fatalf("admin changes = %d, want 3", len(page.Changes))
	}
	if page.Changes[2].Sequence != 3 {
		t.Fatalf("admin last sequence = %d, want 3", page.Changes[2].Sequence)
	}
}

// TestChangesRejectsForgedPlaintextCursor proves the forgery oracle is closed.
// The old wire format was base64(JSON{incarnation,sequence}); a client could
// mint any sequence and binary-search it against the head-conflict boundary to
// recover the global write count. A hand-crafted cursor of that exact shape now
// fails authentication and is refused, so no attacker-chosen Since ever reaches
// the base feed.
func TestChangesRejectsForgedPlaintextCursor(t *testing.T) {
	f := newFixture(t)
	seedInterleaved(t, f)

	forged := forgePlaintextCursor(t, 1<<40)
	_, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{
		Cursor: forged,
	})
	if err == nil {
		t.Fatal("expected a forged plaintext cursor to be refused, got nil")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error code = %v, want invalid argument", err)
	}
}

// TestChangesRejectsTamperedCursor proves the integrity guard: a single flipped
// byte in a legitimately sealed cursor fails the GCM authentication tag and is
// refused with the uniform invalid-cursor error.
func TestChangesRejectsTamperedCursor(t *testing.T) {
	f := newFixture(t)
	seedInterleaved(t, f)

	page, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Cursor == "" {
		t.Fatal("expected a non-empty sealed cursor to tamper with")
	}

	tampered := flipLastBase64Byte(t, page.Cursor)
	if tampered == page.Cursor {
		t.Fatal("tampering did not change the cursor")
	}
	_, err = f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{
		Cursor: tampered,
	})
	if err == nil {
		t.Fatal("expected a tampered cursor to be refused, got nil")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error code = %v, want invalid argument", err)
	}
}

// TestChangesRejectsGarbageCursor proves an arbitrary, unsealed token is
// refused with the same uniform error, so a caller probing with random cursors
// learns nothing that distinguishes one rejection from another.
func TestChangesRejectsGarbageCursor(t *testing.T) {
	f := newFixture(t)
	seedInterleaved(t, f)

	_, err := f.clientA.Changes(f.alice(t), authorized.ChangeFeedRequest{
		Cursor: "not-a-real-cursor",
	})
	if err == nil {
		t.Fatal("expected a garbage cursor to be refused, got nil")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error code = %v, want invalid argument", err)
	}
}

// peekCursorSequence attempts to read a cursor as the old, readable
// base64(JSON) envelope. A sealed ciphertext will not parse, so ok is false. If
// this ever succeeds, the cursor has stopped being opaque.
func peekCursorSequence(cursor string) (uint64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	var payload struct {
		Sequence string `json:"sequence"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, false
	}
	if payload.Sequence == "" {
		return 0, false
	}
	var seq uint64
	for _, r := range payload.Sequence {
		if r < '0' || r > '9' {
			return 0, false
		}
		seq = seq*10 + uint64(r-'0')
	}
	return seq, true
}

func forgePlaintextCursor(t *testing.T, sequence uint64) string {
	t.Helper()
	payload := struct {
		Incarnation string `json:"incarnation"`
		Sequence    string `json:"sequence"`
	}{
		Incarnation: "forged",
		Sequence:    itoa(sequence),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func flipLastBase64Byte(t *testing.T, cursor string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("cursor is not valid base64: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("cursor decoded to zero bytes")
	}
	raw[len(raw)-1] ^= 0x01
	return base64.RawURLEncoding.EncodeToString(raw)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}
