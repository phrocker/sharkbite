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
	"fmt"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestChangesBelowRetentionFloorResynchronises exercises the retention-floor
// branch directly. The embedded corpus never prunes today, so the floor is
// raised manually to simulate a retention pass. A cursor that names changes
// below the floor cannot be answered without a silent gap and must be refused
// with a resynchronise conflict rather than returning a partial answer.
func TestChangesBelowRetentionFloorResynchronises(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	for i := 1; i <= 5; i++ {
		source := Source{
			URI:       fmt.Sprintf("file:///floor-%02d.md", i),
			MediaType: MediaTypeMarkdown,
			Content:   fmt.Sprintf("# Floor %02d\n\nSynthetic body.\n", i),
		}
		if _, err := corpus.Ingest(ctx, source); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	corpus.mu.Lock()
	corpus.changeHistoryFloor = 3
	corpus.mu.Unlock()

	// Since 1 asks for changes starting at sequence 2, which is below the floor
	// of 3: sequence 2 has been pruned, so answering would drop it silently.
	_, err = corpus.Changes(ctx, ChangeRequest{Since: 1})
	if err == nil {
		t.Fatal("expected resynchronise conflict for an aged-out cursor, got nil")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("error code = %v, want conflict", err)
	}
	if !strings.Contains(err.Error(), "retained history floor") {
		t.Fatalf("error %q does not name the retained history floor", err.Error())
	}

	// A cursor exactly at the floor boundary (Since 2 asks for sequence 3) is
	// still fully answerable and must succeed.
	feed, err := corpus.Changes(ctx, ChangeRequest{Since: 2})
	if err != nil {
		t.Fatalf("cursor at the floor boundary must succeed, got %v", err)
	}
	if len(feed.Changes) == 0 || feed.Changes[0].Sequence != 3 {
		t.Fatalf("boundary feed = %+v, want first change at sequence 3", feed.Changes)
	}
}

// TestChangeCursorKeyIsDurableAcrossRestart proves the per-corpus cursor seal
// key is generated once and persisted, not minted afresh each process. A
// per-process key would silently invalidate every outstanding cursor on
// restart -- exactly the resumability property TestChangesCursorSurvivesRestart
// pins -- so the key must survive a close and reopen byte-for-byte.
func TestChangeCursorKeyIsDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := corpus.ChangeCursorSealKey(ctx)
	if err != nil {
		t.Fatalf("seal key before restart: %v", err)
	}
	if len(before) != changeCursorKeyBytes {
		t.Fatalf("seal key length = %d, want %d", len(before), changeCursorKeyBytes)
	}
	// A freshly generated key must not be all zeros: that would mean generation
	// silently failed and every corpus shared a predictable key.
	if allZero(before) {
		t.Fatal("seal key is all zeros; key generation did not run")
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.ChangeCursorSealKey(ctx)
	if err != nil {
		t.Fatalf("seal key after restart: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("seal key changed across restart: %x -> %x", before, after)
	}

	// The accessor must hand back a copy, never the live slice, so a caller
	// cannot mutate the key held under lock.
	after[0] ^= 0xFF
	again, err := reopened.ChangeCursorSealKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again, after) {
		t.Fatal("accessor returned the live key slice; mutation leaked back in")
	}
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
