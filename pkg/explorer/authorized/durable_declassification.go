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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Declassification audit records (#630) are append-only rows under their own
// prefix and record kind. No existing row or kind changes, and a binary that
// predates them skips the prefix on load.
const (
	policyRowDeclassification  = "audit/declassification/"
	policyKindDeclassification = byte(10)

	// Claim-lineage bindings: which intent a source claim version descends
	// from. A separate append-only prefix and kind; the source-claim record
	// itself is unchanged.
	policyRowDeclassificationClaim  = "audit/declassification-claim/"
	policyKindDeclassificationClaim = byte(11)
)

// persistedDeclassificationClaim is a binding, or with Closed the end of a
// bound version's lineage (written under its own row).
type persistedDeclassificationClaim struct {
	Seq       uint64
	SourceURI string
	Version   uint64
	AttemptID string
	Closed    bool
}

func policyDeclassificationClaimRow(sourceURI string, version uint64) []byte {
	digest := sha256.Sum256([]byte(sourceURI))
	return []byte(fmt.Sprintf("%s%s/%020d", policyRowDeclassificationClaim,
		hex.EncodeToString(digest[:]), version))
}

func policyDeclassificationClaimRowFor(
	record persistedDeclassificationClaim,
) []byte {
	row := policyDeclassificationClaimRow(record.SourceURI, record.Version)
	if record.Closed {
		row = append(row, "/closed"...)
	}
	return row
}

// CloseDeclassificationClaim validates the closure, writes it through to the
// engine, and only then makes it observable in memory.
func (s *DurablePolicyStore) CloseDeclassificationClaim(
	ctx context.Context,
	sourceURI string,
	version uint64,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).CloseDeclassificationClaim(
			ctx, sourceURI, version)
	}
	if err := contextFailure(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return catalogUnavailable()
	}
	s.memory.mu.RLock()
	closed, err := s.memory.checkCloseDeclassificationClaimLocked(
		sourceURI, version)
	s.memory.mu.RUnlock()
	if err != nil || closed {
		return err
	}
	record := persistedDeclassificationClaim{
		Seq: s.nextSeq(), SourceURI: sourceURI, Version: version, Closed: true,
	}
	if err := s.writeRow(
		policyDeclassificationClaimRowFor(record),
		policyKindDeclassificationClaim, record,
	); err != nil {
		return err
	}
	return s.memory.CloseDeclassificationClaim(
		context.WithoutCancel(ctx), sourceURI, version)
}

// BindDeclassificationClaim validates the binding, writes it through to the
// engine, and only then makes it observable in memory.
func (s *DurablePolicyStore) BindDeclassificationClaim(
	ctx context.Context,
	sourceURI string,
	version uint64,
	attemptID shoal.ID,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).BindDeclassificationClaim(
			ctx, sourceURI, version, attemptID)
	}
	if err := contextFailure(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return catalogUnavailable()
	}
	s.memory.mu.RLock()
	bound, err := s.memory.checkDeclassificationClaimLocked(
		sourceURI, version, attemptID)
	s.memory.mu.RUnlock()
	if err != nil || bound {
		return err
	}
	if err := s.writeRow(
		policyDeclassificationClaimRow(sourceURI, version),
		policyKindDeclassificationClaim,
		persistedDeclassificationClaim{
			Seq: s.nextSeq(), SourceURI: sourceURI, Version: version,
			AttemptID: string(attemptID),
		},
	); err != nil {
		return err
	}
	return s.memory.BindDeclassificationClaim(
		context.WithoutCancel(ctx), sourceURI, version, attemptID)
}

// DeclassificationClaimIntent is a pure read delegated to the memory store.
func (s *DurablePolicyStore) DeclassificationClaimIntent(
	ctx context.Context,
	sourceURI string,
	version uint64,
) (DeclassificationRecord, bool, error) {
	return s.memoryStore().DeclassificationClaimIntent(ctx, sourceURI, version)
}

// restoreDeclassifications rebuilds the records, their intent index and the
// claim bindings on open. A binding naming no recorded intent for its source
// fails closed.
func (s *MemoryPolicyStore) restoreDeclassifications(
	records []DeclassificationRecord,
	claims []persistedDeclassificationClaim,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declassifications = nil
	s.declassificationIntents = nil
	s.declassificationClaims = nil
	s.closedDeclassificationClaims = nil
	for _, record := range records {
		s.appendDeclassificationLocked(record)
	}
	for _, claim := range claims {
		if claim.Closed {
			continue
		}
		if err := s.bindDeclassificationClaimLocked(
			claim.SourceURI, claim.Version, shoal.ID(claim.AttemptID),
		); err != nil {
			return err
		}
	}
	for _, claim := range claims {
		if !claim.Closed {
			continue
		}
		if err := s.closeDeclassificationClaimLocked(
			claim.SourceURI, claim.Version); err != nil {
			return err
		}
	}
	return nil
}

type persistedDeclassification struct {
	Seq                      uint64
	AttemptID                string
	Phase                    string
	OccurredAtUnixNano       int64
	SourceURI                string
	DocumentID               string
	PriorRevisionID          string
	RevisionID               string
	PriorRules               []persistedRule
	Rule                     persistedRule
	Subject                  string
	Actor                    string
	ClientID                 string
	OnBehalfOf               []string
	RequestID                string
	CorrelationID            string
	AuthorizationFingerprint [32]byte
	ReasonCode               string
}

type sequencedDeclassification struct {
	seq    uint64
	record DeclassificationRecord
}

// orderDeclassifications restores recording order from the write sequence,
// since the engine scans rows in key order.
func orderDeclassifications(
	records []sequencedDeclassification,
) []DeclassificationRecord {
	sort.SliceStable(records, func(left, right int) bool {
		return records[left].seq < records[right].seq
	})
	ordered := make([]DeclassificationRecord, 0, len(records))
	for _, entry := range records {
		ordered = append(ordered, entry.record)
	}
	return ordered
}

func policyDeclassificationRow(record DeclassificationRecord) []byte {
	return []byte(policyRowDeclassification + string(record.AttemptID) +
		"/" + string(record.Phase))
}

// RecordDeclassification validates the record, writes it through to the
// engine, and only then makes it observable in memory, so a record that
// failed to persist is never reported as recorded.
func (s *DurablePolicyStore) RecordDeclassification(
	ctx context.Context,
	record DeclassificationRecord,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).RecordDeclassification(ctx, record)
	}
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if err := record.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return catalogUnavailable()
	}
	// s.mu serializes every durable write, so no record for this pair can
	// appear between this check and the write below.
	if s.memory.hasDeclassification(record.AttemptID, record.Phase) {
		return duplicateDeclassification()
	}
	persisted, err := declassificationToPersisted(record, s.nextSeq())
	if err != nil {
		return catalogUnavailable()
	}
	if err := s.writeRow(
		policyDeclassificationRow(record), policyKindDeclassification,
		persisted,
	); err != nil {
		return err
	}
	return s.memory.RecordDeclassification(context.WithoutCancel(ctx), record)
}

// Declassifications is a pure read delegated to the memory store, whose
// records were reconstructed from the engine on open.
func (s *DurablePolicyStore) Declassifications(
	ctx context.Context,
) ([]DeclassificationRecord, error) {
	return s.memoryStore().Declassifications(ctx)
}

func declassificationToPersisted(
	record DeclassificationRecord,
	seq uint64,
) (persistedDeclassification, error) {
	rule, err := ruleToPersisted(record.Rule)
	if err != nil {
		return persistedDeclassification{}, err
	}
	persisted := persistedDeclassification{
		Seq:                      seq,
		AttemptID:                string(record.AttemptID),
		Phase:                    string(record.Phase),
		OccurredAtUnixNano:       record.OccurredAt.UnixNano(),
		SourceURI:                record.SourceURI,
		DocumentID:               string(record.DocumentID),
		PriorRevisionID:          string(record.PriorRevisionID),
		RevisionID:               string(record.RevisionID),
		Rule:                     rule,
		Subject:                  string(record.Subject),
		Actor:                    string(record.Actor),
		ClientID:                 string(record.ClientID),
		OnBehalfOf:               stringsFromIDs(record.OnBehalfOf),
		RequestID:                string(record.RequestID),
		CorrelationID:            string(record.CorrelationID),
		AuthorizationFingerprint: [32]byte(record.AuthorizationFingerprint),
		ReasonCode:               record.ReasonCode,
	}
	for _, prior := range record.PriorRules {
		encoded, err := ruleToPersisted(prior)
		if err != nil {
			return persistedDeclassification{}, err
		}
		persisted.PriorRules = append(persisted.PriorRules, encoded)
	}
	return persisted, nil
}

func declassificationFromPersisted(
	persisted persistedDeclassification,
) (DeclassificationRecord, error) {
	rule, err := ruleFromPersisted(persisted.Rule)
	if err != nil {
		return DeclassificationRecord{}, err
	}
	record := DeclassificationRecord{
		AttemptID:                shoal.ID(persisted.AttemptID),
		Phase:                    DeclassificationPhase(persisted.Phase),
		OccurredAt:               time.Unix(0, persisted.OccurredAtUnixNano).UTC(),
		SourceURI:                persisted.SourceURI,
		DocumentID:               shoal.ID(persisted.DocumentID),
		PriorRevisionID:          shoal.ID(persisted.PriorRevisionID),
		RevisionID:               shoal.ID(persisted.RevisionID),
		Rule:                     rule,
		Subject:                  shoal.ID(persisted.Subject),
		Actor:                    shoal.ID(persisted.Actor),
		ClientID:                 shoal.ID(persisted.ClientID),
		OnBehalfOf:               idsFromStrings(persisted.OnBehalfOf),
		RequestID:                shoal.ID(persisted.RequestID),
		CorrelationID:            shoal.ID(persisted.CorrelationID),
		AuthorizationFingerprint: auth.Fingerprint(persisted.AuthorizationFingerprint),
		ReasonCode:               persisted.ReasonCode,
	}
	for _, prior := range persisted.PriorRules {
		decoded, err := ruleFromPersisted(prior)
		if err != nil {
			return DeclassificationRecord{}, err
		}
		record.PriorRules = append(record.PriorRules, decoded)
	}
	if err := record.validate(); err != nil {
		return DeclassificationRecord{}, err
	}
	return record, nil
}
