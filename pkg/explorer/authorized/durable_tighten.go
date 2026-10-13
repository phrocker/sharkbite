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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/dirlock"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	policyRowLabelMigration  = "meta/label-migration"
	policyKindLabelMigration = byte(9)
)

type persistedLabelMigration struct {
	Seq                 uint64
	Version             uint32
	Documents           int
	Unregistered        int
	Unlabelled          int
	Tightened           int
	AlreadyTightened    int
	HistoricalTightened int
	Untranslatable      []persistedUntranslatable
	Drift               []persistedRevisionDrift
}

type persistedRevisionDrift struct {
	DocumentID        string
	SourceURI         string
	CatalogRevisionID string
	BaseRevisionID    string
	Reason            string
}

type persistedUntranslatable struct {
	DocumentID   string
	RevisionID   string
	SourceURI    string
	EscapedLabel string
	Reason       string
}

// OpenExistingDurablePolicyStore opens a durable policy catalog that already
// exists and was cleanly closed, for read-only tooling such as
// -list-untranslatable-labels. Opening the storage engine is not read-only in
// general: it creates a missing table, gives a bare table directory a WAL,
// and replays an unflushed WAL into a new file (rewriting the file manifest
// and truncating the WAL). So everything that would make it write is refused
// BEFORE the engine is opened:
//
//   - a missing directory, or one without the catalog's table;
//   - a table whose tablet holds a non-empty wal.log: the catalog is open in
//     a running workspace, or one that stopped without flushing;
//   - a table with no committed file manifest (files.json): a bare or
//     never-flushed table.
//
// What remains is a cleanly closed catalog, which the engine opens without
// writing.
//
// Those checks alone would race a running workspace: one idle with an empty
// WAL passes them and can write before the engine replays its WAL here (#686).
// So the catalog's directory lock (see OpenDurablePolicyStore), which every
// open store holds for its whole life, is tried without blocking and a held
// lock is refused; the checks then run again under the lock, where no other
// opener can change the directory, and the engine is opened only after they
// pass. (The unlocked round only gives a refused directory its precise
// error.)
//
// The lock file is only opened and locked, never created, so the listing
// writes nothing at all. A catalog without one has not been opened since the
// lock was introduced, possibly not by anything that takes it, and is refused
// like a catalog with no committed records: start and cleanly stop the
// workspace once. Creating the file here instead would also leave it owned by
// whoever ran the listing, root included, which could lock the workspace out.
func OpenExistingDurablePolicyStore(dir string) (*DurablePolicyStore, error) {
	if err := requireCleanlyClosedCatalog(dir); err != nil {
		return nil, err
	}
	openExistingBeforeLock()
	lock, err := dirlock.AcquireExistingFile(dir, policyLockFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, shoal.WrapError(shoal.ErrorUnavailable,
			"policy catalog "+dir+" has no "+policyLockFile+": it has not "+
				"been opened by a workspace of this version. Start and "+
				"cleanly stop the workspace once, then try again", err)
	}
	if err != nil {
		return nil, policyCatalogLockError(dir, err)
	}
	if err := requireCleanlyClosedCatalog(dir); err != nil {
		_ = lock.Close()
		return nil, err
	}
	// An offline listing never writes, so it leaves the floor alone.
	return openLockedDurablePolicyStore(dir, lock, false)
}

// openExistingBeforeLock runs between OpenExistingDurablePolicyStore's
// unlocked checks and its lock. It does nothing; a test replaces it to change
// the directory inside that window.
var openExistingBeforeLock = func() {}

// requireCleanlyClosedCatalog refuses, without writing, every directory that
// OpenExistingDurablePolicyStore must not open.
func requireCleanlyClosedCatalog(dir string) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return shoal.NewError(shoal.ErrorNotFound,
			"no policy catalog at "+dir)
	}
	tableDir := filepath.Join(dir, policyTable)
	table, err := os.Stat(tableDir)
	if err != nil || !table.IsDir() {
		return shoal.NewError(shoal.ErrorNotFound,
			"no policy catalog at "+dir+": it holds no "+policyTable+" table")
	}
	tabletDirs := []string{tableDir}
	entries, err := os.ReadDir(tableDir)
	if err != nil {
		return shoal.WrapError(shoal.ErrorUnavailable,
			"read policy catalog "+dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			tabletDirs = append(tabletDirs, filepath.Join(tableDir, entry.Name()))
		}
	}
	committed := false
	for _, tabletDir := range tabletDirs {
		wal, err := os.Stat(filepath.Join(tabletDir, "wal.log"))
		if err == nil && wal.Size() > 0 {
			return shoal.NewError(shoal.ErrorUnavailable,
				"policy catalog "+dir+" has unflushed writes: stop the "+
					"workspace (a clean shutdown flushes them) and try again")
		}
		if _, err := os.Stat(filepath.Join(tabletDir, "files.json")); err == nil {
			committed = true
		}
	}
	if !committed {
		return shoal.NewError(shoal.ErrorUnavailable,
			"policy catalog "+dir+" holds no committed records: start and "+
				"cleanly stop the workspace so the migration's report is "+
				"flushed, then try again")
	}
	return nil
}

// policyRow is one record of a batched durable write.
type policyRow struct {
	row   []byte
	kind  byte
	value any
}

// TightenRule tightens the in-memory catalog and persists every record it
// rewrote in ONE engine write: the policy table is a single tablet, which
// appends the whole batch to its WAL in one write before applying any of it.
// A crash can still tear that write and persist only a prefix of the batch.
// That is narrowing-only (every persisted record is tighter, none wider), and
// the label migration does not rely on more: it re-sweeps every labelled
// document on every start, and TightenRule completes whatever a torn write
// left behind.
func (s *DurablePolicyStore) TightenRule(
	ctx context.Context,
	index *TighteningIndex,
	tightening RuleTightening,
) (bool, error) {
	if s == nil {
		return (*MemoryPolicyStore)(nil).TightenRule(ctx, index, tightening)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diverged.Load() {
		return false, catalogUnavailable()
	}
	changes, err := s.memory.tightenRule(ctx, index, tightening)
	if err != nil {
		return false, err
	}
	if !changes.changed() {
		return false, nil
	}
	// The memory store has changed; any failure from here on leaves it
	// ahead of the engine.
	persistedAll := false
	defer func() {
		if !persistedAll && !s.closed {
			s.markDiverged()
		}
	}()
	rows := make([]policyRow, 0,
		len(changes.revisions)+len(changes.nodes)+len(changes.edges)+2)
	for _, key := range changes.revisions {
		row, err := s.revisionRow(key)
		if err != nil {
			return false, err
		}
		rows = append(rows, row)
	}
	for _, nodeID := range changes.nodes {
		row, err := s.nodeRow(nodeID)
		if err != nil {
			return false, err
		}
		rows = append(rows, row)
	}
	for _, edgeID := range changes.edges {
		registration, ok := s.memory.snapshotApplicationEdge(edgeID)
		if !ok {
			return false, catalogUnavailable()
		}
		record, err := edgeToPersisted(registration, s.nextSeq(), false)
		if err != nil {
			return false, catalogUnavailable()
		}
		rows = append(rows, policyRow{policyEdgeRow(edgeID), policyKindEdge, record})
	}
	if changes.sourceClaim != "" {
		claimRows, err := s.sourceClaimRows(changes.sourceClaim)
		if err != nil {
			return false, err
		}
		rows = append(rows, claimRows...)
	}
	if err := s.writeRows(rows); err != nil {
		return false, err
	}
	persistedAll = true
	return true, nil
}

// TighteningIndex is a pure read delegated to the memory store.
func (s *DurablePolicyStore) TighteningIndex(
	ctx context.Context,
) (*TighteningIndex, error) {
	return s.memoryStore().TighteningIndex(ctx)
}

// LabelMigration is a pure read delegated to the memory store, whose marker
// was reconstructed from the engine on open.
func (s *DurablePolicyStore) LabelMigration(
	ctx context.Context,
) (LabelMigrationRecord, bool, error) {
	return s.memoryStore().LabelMigration(ctx)
}

// PutLabelMigration records the marker in memory and persists it.
func (s *DurablePolicyStore) PutLabelMigration(
	ctx context.Context,
	record LabelMigrationRecord,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).PutLabelMigration(ctx, record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	persisted := persistedLabelMigration{
		Version:             record.Version,
		Documents:           record.Documents,
		Unregistered:        record.Unregistered,
		Unlabelled:          record.Unlabelled,
		Tightened:           record.Tightened,
		AlreadyTightened:    record.AlreadyTightened,
		HistoricalTightened: record.HistoricalTightened,
	}
	for _, entry := range record.Untranslatable {
		persisted.Untranslatable = append(persisted.Untranslatable,
			persistedUntranslatable{
				DocumentID:   string(entry.DocumentID),
				RevisionID:   string(entry.RevisionID),
				SourceURI:    entry.SourceURI,
				EscapedLabel: entry.EscapedLabel,
				Reason:       entry.Reason,
			})
	}
	for _, entry := range record.Drift {
		persisted.Drift = append(persisted.Drift, persistedRevisionDrift{
			DocumentID:        string(entry.DocumentID),
			SourceURI:         entry.SourceURI,
			CatalogRevisionID: string(entry.CatalogRevisionID),
			BaseRevisionID:    string(entry.BaseRevisionID),
			Reason:            entry.Reason,
		})
	}
	// Admit the report before memory changes; see admitPolicyRecord.
	if err := admitPolicyRecord(policyKindLabelMigration, persisted); err != nil {
		return err
	}
	if err := s.memory.PutLabelMigration(ctx, record); err != nil {
		return err
	}
	persisted.Seq = s.nextSeq()
	return s.persisted(s.writeRow(
		[]byte(policyRowLabelMigration), policyKindLabelMigration, persisted))
}

func labelMigrationFromPersisted(
	record persistedLabelMigration,
) (LabelMigrationRecord, error) {
	if record.Version == 0 {
		return LabelMigrationRecord{}, fmt.Errorf("label migration version is zero")
	}
	migration := LabelMigrationRecord{
		Version:             record.Version,
		Documents:           record.Documents,
		Unregistered:        record.Unregistered,
		Unlabelled:          record.Unlabelled,
		Tightened:           record.Tightened,
		AlreadyTightened:    record.AlreadyTightened,
		HistoricalTightened: record.HistoricalTightened,
	}
	for _, entry := range record.Untranslatable {
		migration.Untranslatable = append(migration.Untranslatable,
			UntranslatableLabel{
				DocumentID:   shoal.ID(entry.DocumentID),
				RevisionID:   shoal.ID(entry.RevisionID),
				SourceURI:    entry.SourceURI,
				EscapedLabel: entry.EscapedLabel,
				Reason:       entry.Reason,
			})
	}
	for _, entry := range record.Drift {
		migration.Drift = append(migration.Drift, RevisionDrift{
			DocumentID:        shoal.ID(entry.DocumentID),
			SourceURI:         entry.SourceURI,
			CatalogRevisionID: shoal.ID(entry.CatalogRevisionID),
			BaseRevisionID:    shoal.ID(entry.BaseRevisionID),
			Reason:            entry.Reason,
		})
	}
	return migration, nil
}

func (s *DurablePolicyStore) revisionRow(key revisionKey) (policyRow, error) {
	stored, ok := s.memory.snapshotRevision(key)
	if !ok {
		return policyRow{}, catalogUnavailable()
	}
	rule, err := ruleToPersisted(stored.Rule)
	if err != nil {
		return policyRow{}, catalogUnavailable()
	}
	return policyRow{policyRevisionRow(key), policyKindRevision, persistedRevision{
		Seq:            s.nextSeq(),
		DocumentID:     string(key.documentID),
		RevisionID:     string(key.revisionID),
		NodeIDs:        stringsFromIDs(stored.NodeIDs),
		IntrinsicEdges: edgesToPersisted(stored.IntrinsicEdges),
		ContentDigest:  [32]byte(stored.ContentDigest),
		Rule:           rule,
	}}, nil
}

func (s *DurablePolicyStore) nodeRow(nodeID shoal.ID) (policyRow, error) {
	registration, ok := s.memory.snapshotNode(nodeID)
	if !ok {
		return policyRow{}, catalogUnavailable()
	}
	rule, err := ruleToPersisted(registration.Rule)
	if err != nil {
		return policyRow{}, catalogUnavailable()
	}
	return policyRow{policyNodeRow(nodeID), policyKindNode, persistedNode{
		Seq:        s.nextSeq(),
		NodeID:     string(nodeID),
		DocumentID: string(registration.DocumentID),
		RevisionID: string(registration.RevisionID),
		Node:       graphNodeToPersisted(registration.Node),
		Rule:       rule,
		Kind:       uint8(registration.Kind),
	}}, nil
}

// sourceClaimRows mirrors persistSourceClaim for a present claim, as rows of
// a batch: the claim and the refreshed source-version counter.
func (s *DurablePolicyStore) sourceClaimRows(sourceURI string) ([]policyRow, error) {
	claim, present := s.memory.snapshotSourceClaim(sourceURI)
	if !present {
		return nil, catalogUnavailable()
	}
	rule, err := ruleToPersisted(claim.Rule)
	if err != nil {
		return nil, catalogUnavailable()
	}
	record := persistedSourceClaim{
		Seq:       s.nextSeq(),
		SourceURI: sourceURI,
		Rule:      rule,
		Pending:   claim.Pending,
		Version:   claim.Version,
	}
	if claim.PreviousRule != nil {
		previous, err := ruleToPersisted(*claim.PreviousRule)
		if err != nil {
			return nil, catalogUnavailable()
		}
		record.PreviousRule = &previous
	}
	version := persistedSourceVersion{
		Seq:     s.nextSeq(),
		Version: s.memory.snapshotSourceVersion(),
	}
	return []policyRow{
		{policySourceClaimRow(sourceURI), policyKindSourceClaim, record},
		{[]byte(policyRowSourceVersion), policyKindSourceVersion, version},
	}, nil
}

// writeRows encodes every row first and then writes them in one engine
// write, so an encoding failure writes nothing.
func (s *DurablePolicyStore) writeRows(rows []policyRow) error {
	if s.closed || s.diverged.Load() {
		return catalogUnavailable()
	}
	mutations := make([]*cclient.Mutation, 0, len(rows))
	for _, row := range rows {
		encoded, err := encodePolicyRecord(row.kind, row.value)
		if err != nil {
			return catalogUnavailable()
		}
		mutation, err := cclient.NewMutation(row.row)
		if err != nil {
			return catalogUnavailable()
		}
		mutation.PutLatest(
			[]byte(policyRecordCF), []byte(policyRecordCQ), nil, encoded)
		mutations = append(mutations, mutation)
	}
	if err := s.engine.Write(policyTable, mutations); err != nil {
		return catalogUnavailable()
	}
	return nil
}
