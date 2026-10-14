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
	"fmt"
	"log/slog"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Durable revision bindings (#695) and their rollback behaviour.
//
// Two things are persisted: the SourceRevisions of a bound edge (two new
// fields of persistedEdge, on edge and edge-reservation rows), and one row per
// interaction session that recorded revisions (policyRowInteractionRevisions).
//
// A binary that predates them would not refuse either one. gob ignores fields
// its destination lacks, so it would load a bound edge as an unbound one, and
// its loader skips row prefixes it does not know. Either way it would serve
// the very reads this binding closes, silently. So the first read-write open
// by this binary (OpenDurablePolicyStore), and in any case the first record of
// either kind, writes a format floor first: a row under the
// source-claim prefix, which every durable binary since the catalog existed
// decodes strictly as a source claim. Its record kind is not the source-claim
// kind, so such a binary refuses to open the catalog ("stored policy source
// claim record is corrupt") instead of opening it without the binding: a
// rollback past this change fails closed. The row key ends in a byte that is
// never valid UTF-8, and a source URI must be valid UTF-8, so no source claim
// can ever share it. Rolling back therefore needs the catalog restored from
// before the floor was written; rolling forward again needs nothing.
//
// This binary, in turn, decodes all three strictly: the floor must be exactly
// one it knows (floor 1), a bound edge's revisions must be canonical, an
// interaction row must match its key and hold a nonempty canonical set, and a
// bound edge or interaction row in a catalog with no floor is corrupt, since
// this binary never writes one without the other.
const (
	policyRowFormatFloor  = policyRowSourceClaim + "\xffformat-floor"
	policyKindFormatFloor = byte(12)

	policyRowInteractionRevisions  = "derived/interaction/"
	policyKindInteractionRevisions = byte(13)

	// formatFloorRevisionBindings is the floor revision bindings require.
	formatFloorRevisionBindings = uint64(1)
)

type persistedFormatFloor struct {
	Seq   uint64
	Floor uint64
}

type persistedRevisionReference struct {
	DocumentID string
	RevisionID string
}

type persistedInteractionRevisions struct {
	Seq       uint64
	SessionID string
	Revisions []persistedRevisionReference
}

func validateFormatFloor(record persistedFormatFloor) error {
	if record.Floor != formatFloorRevisionBindings {
		return shoal.NewError(shoal.ErrorInternal, fmt.Sprintf(
			"policy catalog format floor %d is not supported by this binary",
			record.Floor))
	}
	return nil
}

func policyInteractionRevisionsRow(sessionID shoal.ID) []byte {
	return append([]byte(policyRowInteractionRevisions), string(sessionID)...)
}

func revisionReferencesToPersisted(
	references []RevisionReference,
) []persistedRevisionReference {
	if len(references) == 0 {
		return nil
	}
	persisted := make([]persistedRevisionReference, len(references))
	for index, reference := range references {
		persisted[index] = persistedRevisionReference{
			DocumentID: string(reference.DocumentID),
			RevisionID: string(reference.RevisionID),
		}
	}
	return persisted
}

func revisionReferencesFromPersisted(
	records []persistedRevisionReference,
) []RevisionReference {
	if len(records) == 0 {
		return nil
	}
	references := make([]RevisionReference, len(records))
	for index, record := range records {
		references[index] = RevisionReference{
			DocumentID: shoal.ID(record.DocumentID),
			RevisionID: shoal.ID(record.RevisionID),
		}
	}
	return references
}

// reconstructRevisionBindings validates and restores the revision bindings
// after reconstruct has restored the edges.
func (s *DurablePolicyStore) reconstructRevisionBindings(
	haveFloor bool,
	edges map[string]persistedEdge,
	edgeClaims map[string]persistedEdge,
	interactionRevisions map[string]persistedInteractionRevisions,
) error {
	bound := len(interactionRevisions) > 0
	for _, records := range []map[string]persistedEdge{edges, edgeClaims} {
		for _, record := range records {
			if !record.Tombstone &&
				(record.SourceRevisionsBound || len(record.SourceRevisions) > 0) {
				bound = true
			}
		}
	}
	if bound && !haveFloor {
		return corruptPolicyRecord("revision binding",
			fmt.Errorf("revision bindings were written without the format floor"))
	}
	s.formatFloor = haveFloor
	for _, record := range interactionRevisions {
		references := revisionReferencesFromPersisted(record.Revisions)
		canonical, err := canonicalRevisionReferences(references)
		if err != nil || len(canonical) == 0 ||
			!revisionReferencesEqual(canonical, references) {
			return corruptPolicyRecord("interaction revisions",
				fmt.Errorf("interaction revisions are not a canonical nonempty set"))
		}
		s.memory.interactionRevisions[shoal.ID(record.SessionID)] = canonical
	}
	return nil
}

// writeFormatFloorAtOpen writes the format floor when a read-write open
// finds none, and says so: from then on an older binary refuses the catalog
// with a "corrupt" error that is not corruption.
func (s *DurablePolicyStore) writeFormatFloorAtOpen(dir string) error {
	if s.formatFloor {
		return nil
	}
	if err := s.ensureFormatFloor(); err != nil {
		return shoal.WrapError(shoal.ErrorUnavailable,
			"write the policy catalog format floor", err)
	}
	slog.Info("policy catalog format floor written: binaries from before "+
		"#695 now refuse this catalog (\"stored policy source claim record "+
		"is corrupt\"); it is not corrupt, and rollback means restoring a "+
		"backup taken before this upgrade",
		"dir", dir, "row", policyRowFormatFloor,
		"floor", formatFloorRevisionBindings)
	return nil
}

// ensureFormatFloor writes the format floor before the first bound record.
// A read-write open already wrote it; this covers a store opened without
// writing it (OpenExistingDurablePolicyStore) that then binds.
// It runs under s.mu and before the memory store changes, so a failure is
// only an error.
func (s *DurablePolicyStore) ensureFormatFloor() error {
	if s.formatFloor {
		return nil
	}
	if err := s.writeRowBeforeApply(
		[]byte(policyRowFormatFloor), policyKindFormatFloor,
		persistedFormatFloor{
			Seq: s.nextSeq(), Floor: formatFloorRevisionBindings,
		},
	); err != nil {
		return err
	}
	s.formatFloor = true
	return nil
}

func edgeRegistrationBound(registration EdgeRegistration) bool {
	return registration.SourceRevisionsBound ||
		len(registration.SourceRevisions) > 0
}

// DocumentRevisionRules is a pure read delegated to the memory store.
func (s *DurablePolicyStore) DocumentRevisionRules(
	ctx context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID][]RevisionRule, error) {
	return s.memoryStore().DocumentRevisionRules(ctx, documentIDs)
}

// RevisionRules is a pure read delegated to the memory store.
func (s *DurablePolicyStore) RevisionRules(
	ctx context.Context,
	references []RevisionReference,
) (map[shoal.ID][]RevisionRule, error) {
	return s.memoryStore().RevisionRules(ctx, references)
}

// InteractionRevisions is a pure read delegated to the memory store.
func (s *DurablePolicyStore) InteractionRevisions(
	ctx context.Context,
	sessionIDs []shoal.ID,
) (map[shoal.ID][]RevisionReference, error) {
	return s.memoryStore().InteractionRevisions(ctx, sessionIDs)
}

// PutInteractionRevisions persists the union before applying it, so the
// recording is durable before the session it guards is written.
func (s *DurablePolicyStore) PutInteractionRevisions(
	ctx context.Context,
	sessionID shoal.ID,
	references []RevisionReference,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).PutInteractionRevisions(
			ctx, sessionID, references)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.diverged.Load() {
		return catalogUnavailable()
	}
	merged, err := s.memory.mergeInteractionRevisions(
		ctx, sessionID, references, false)
	if err != nil || len(merged) == 0 {
		return err
	}
	if recorded, ok := s.memory.snapshotInteractionRevisions(sessionID); ok &&
		revisionReferencesEqual(recorded, merged) {
		return nil
	}
	record := persistedInteractionRevisions{
		SessionID: string(sessionID),
		Revisions: revisionReferencesToPersisted(merged),
	}
	if err := admitPolicyRecord(policyKindInteractionRevisions, record); err != nil {
		return err
	}
	if err := s.ensureFormatFloor(); err != nil {
		return err
	}
	record.Seq = s.nextSeq()
	if err := s.writeRowBeforeApply(
		policyInteractionRevisionsRow(sessionID),
		policyKindInteractionRevisions, record,
	); err != nil {
		return err
	}
	// Durable now; applying it must not then fail on the caller's context.
	return s.persisted(s.memory.PutInteractionRevisions(
		context.WithoutCancel(ctx), sessionID, merged))
}
