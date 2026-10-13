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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/dirlock"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DurablePolicyStore is a durable PolicyStore backed by Shoal's own storage
// engine. It mirrors the on-disk persistence pattern of the Explorer document
// store (pkg/explorer/storage.go): every record is a SHA-256-checksummed gob
// envelope written to a single engine table.
//
// All authorization decision logic — including the batched, fully-interleaved
// validation of Nodes and Edges, the source-claim compare-and-swap protocol,
// and the replaceCurrent revision projection — is delegated unchanged to an
// embedded MemoryPolicyStore. The durable layer only reconstructs that store's
// state on open and writes each successful mutation through to the engine, so
// the PolicyStore contract is preserved exactly rather than reimplemented.
//
// A corrupt or truncated persisted record makes OpenDurablePolicyStore fail:
// the store never opens on partially-decodable state, so a caller that cannot
// build the store cannot serve the corpus. Persistence therefore fails closed
// (deny), never open.
type DurablePolicyStore struct {
	// mu serializes every mutating operation together with its write-through
	// persistence so the record written to the engine always reflects the
	// exact in-memory transition that produced it. Reads do not take mu; they
	// delegate straight to the memory store's own lock.
	mu     sync.Mutex
	memory *MemoryPolicyStore
	engine *engine.Engine
	// lock is the catalog directory's exclusive ownership (policyLockFile).
	lock   *dirlock.Lock
	seq    uint64
	closed bool
	// formatFloor reports that the catalog holds the format floor revision
	// bindings require (revision_binding_durable.go).
	formatFloor bool
	// diverged is set once a write-through fails after the memory store was
	// already mutated, so memory may hold state the engine does not. From
	// then on the store fails closed: every read and write reports the
	// catalog unavailable until the process reopens it from the engine.
	// Serving the in-memory state would let authorization rely on a
	// registration or claim that is not durable (#665: a registration
	// that never persisted could otherwise stand in for the rule it
	// replaced).
	diverged atomic.Bool
	// shut mirrors closed for the lock-free read path: a closed store
	// serves no reads, so a write refused because it raced Close (after
	// changing memory) is never observable.
	shut atomic.Bool
	// divergedOnce runs the OnDiverged hook at most once; divergedHook is
	// what OnDiverged registered.
	divergedOnce sync.Once
	divergedMu   sync.Mutex
	divergedHook func()
}

// Diverged reports whether a failed write-through has made the store fail
// closed. It is the operational signal for "this process must restart": the
// state on disk is intact, and reopening reloads it.
func (s *DurablePolicyStore) Diverged() bool {
	return s != nil && s.diverged.Load()
}

// OnDiverged registers hook to run once, the first time the store diverges,
// or immediately if it already has. Hosts use it to log the event and drop
// readiness, so an orchestrator restarts or drains an instance whose catalog
// now refuses every request. The hook must not call back into the store.
func (s *DurablePolicyStore) OnDiverged(hook func()) {
	if s == nil || hook == nil {
		return
	}
	s.divergedMu.Lock()
	s.divergedHook = hook
	s.divergedMu.Unlock()
	if s.diverged.Load() {
		s.fireDiverged()
	}
}

// markDiverged makes the store fail closed and signals it once.
func (s *DurablePolicyStore) markDiverged() {
	s.diverged.Store(true)
	s.fireDiverged()
}

func (s *DurablePolicyStore) fireDiverged() {
	s.divergedMu.Lock()
	hook := s.divergedHook
	s.divergedMu.Unlock()
	if hook == nil {
		return
	}
	s.divergedOnce.Do(hook)
}

const (
	policyTable    = "_shoal_policy"
	policyRecordCF = "record"
	policyRecordCQ = "v1"

	policyRowSourceClaim   = "sourceclaim/"
	policyRowRevision      = "revision/"
	policyRowCurrent       = "current/"
	policyRowEdge          = "edge/"
	policyRowEdgeClaim     = "edgeclaim/"
	policyRowNode          = "node/"
	policyRowSourceVersion = "meta/source-version"
	policyRowCoOccurrence  = "mosaic/"

	// The policy envelope is deliberately separate from the Explorer document
	// envelope (SHOALX2). Its magic, envelope version, record kind, big-endian
	// payload length, SHA-256 payload checksum, and gob payload mirror that
	// layout so the two stores read as siblings.
	policyRecordMagic     = "SHOALP1\x00"
	policyEnvelopeVersion = byte(1)

	policyKindSourceClaim   = byte(1)
	policyKindRevision      = byte(2)
	policyKindCurrent       = byte(3)
	policyKindEdge          = byte(4)
	policyKindEdgeClaim     = byte(5)
	policyKindSourceVersion = byte(6)
	policyKindNode          = byte(7)
	policyKindCoOccurrence  = byte(8)

	policyEnvelopeHeader = 8 + 1 + 1 + 8 + sha256.Size
	maxPolicyRecordBytes = uint64(64 << 20)
)

type persistedRule struct {
	// Policies holds each conjunction component as the canonical visibility
	// expression produced by auth.Policy.Encode, which auth.DecodePolicy
	// losslessly reverses (the same round trip AccessRule.clone relies on).
	Policies [][]byte
}

type persistedGraphEdge struct {
	ID, From, To, Type string
	Weight             float64
	Properties         map[string]string
}

type persistedSourceClaim struct {
	Seq          uint64
	Tombstone    bool
	SourceURI    string
	Rule         persistedRule
	Pending      bool
	PreviousRule *persistedRule
	Version      uint64
}

type persistedRevision struct {
	Seq            uint64
	DocumentID     string
	RevisionID     string
	NodeIDs        []string
	IntrinsicEdges []persistedGraphEdge
	ContentDigest  [32]byte
	Rule           persistedRule
}

type persistedCurrent struct {
	Seq        uint64
	Tombstone  bool
	DocumentID string
	RevisionID string
}

// Kind is RegistrationKind. Records written before it existed decode with a
// zero Kind and take the explicit legacy path (legacyEdgeKind,
// legacyNodeKind) once, at load.
//
// SourceRevisions and SourceRevisionsBound are EdgeRegistration's revision
// binding (#695). A record written before they existed decodes with neither,
// which is exactly an unbound (legacy) registration. A bound record is only
// ever written after the catalog's format floor (revision_binding_durable.go),
// which a binary that predates the binding refuses to open over.
type persistedEdge struct {
	Seq                  uint64
	Tombstone            bool
	Edge                 persistedGraphEdge
	DocumentID           string
	RevisionID           string
	Rule                 persistedRule
	Kind                 uint8
	SourceRevisions      []persistedRevisionReference
	SourceRevisionsBound bool
}

type persistedNode struct {
	Seq        uint64
	NodeID     string
	DocumentID string
	RevisionID string
	Node       persistedGraphNode
	Rule       persistedRule
	Kind       uint8
}

type persistedGraphNode struct {
	ID         string
	Kind       string
	Labels     []string
	Properties map[string]string
}

type persistedSourceVersion struct {
	Seq     uint64
	Version uint64
}

type persistedCoOccurrence struct {
	Seq                 uint64
	Key                 string
	WindowStartUnixNano int64
	Domains             []string
}

// OpenDurablePolicyStore opens or creates a durable policy catalog rooted at
// dir. It reconstructs the catalog from any previously persisted records; a
// record that fails to decode or validate fails closed by refusing to open.
//
// The store owns dir exclusively until Close: it holds an OS lock on
// policyLockFile (internal/dirlock) from before the engine opens until after
// it closes, so a second opener in this or any other process, the offline
// listings included (OpenExistingDurablePolicyStore), is refused instead of
// replaying, flushing or truncating the owner's WAL (#686). The kernel drops
// the lock when its holder exits, however it exits, so the lock file a crash
// leaves behind does not block the next start.
//
// It writes the catalog's format floor (revision_binding_durable.go) if the
// catalog has none, so from the first read-write open by this binary on, a
// binary from before #695 refuses the catalog.
func OpenDurablePolicyStore(dir string) (*DurablePolicyStore, error) {
	lock, err := dirlock.Acquire(dir, policyLockFile)
	if err != nil {
		return nil, policyCatalogLockError(dir, err)
	}
	return openLockedDurablePolicyStore(dir, lock, true)
}

// policyLockFile is the catalog directory's ownership lock file. It is a
// plain file, which the storage engine (it treats every subdirectory as a
// table) ignores.
const policyLockFile = ".shoal-policy-catalog.lock"

// policyCatalogLockError words a failure to take dir's lock for the operator.
// Only contention says another process has the catalog; any other failure
// (a lock file this user may not open, a filesystem without locks) carries
// its cause in the message, which shoal.Error does not print otherwise.
func policyCatalogLockError(dir string, err error) error {
	if errors.Is(err, dirlock.ErrLocked) {
		return shoal.WrapError(shoal.ErrorUnavailable,
			"policy catalog "+dir+" is already open, in a running workspace "+
				"or another tool: stop it and try again", err)
	}
	return shoal.WrapError(shoal.ErrorUnavailable,
		"acquire policy catalog "+dir+": "+err.Error(), err)
}

// openLockedDurablePolicyStore opens the catalog in dir under lock, which the
// returned store then owns. On failure it releases the lock.
func openLockedDurablePolicyStore(
	dir string,
	lock *dirlock.Lock,
	writeFloor bool,
) (*DurablePolicyStore, error) {
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		_ = lock.Close()
		return nil, shoal.WrapError(
			shoal.ErrorUnavailable, "open policy catalog storage", err)
	}
	found := false
	for _, table := range eng.TableNames() {
		if table == policyTable {
			found = true
			break
		}
	}
	if !found {
		if err := eng.CreateTable(policyTable, engine.TableOptions{}); err != nil {
			_ = eng.Close()
			_ = lock.Close()
			return nil, shoal.WrapError(
				shoal.ErrorInternal, "create policy catalog table", err)
		}
	}
	store := &DurablePolicyStore{
		memory: NewMemoryPolicyStore(),
		engine: eng,
		lock:   lock,
	}
	if err := store.load(); err != nil {
		_ = eng.Close()
		_ = lock.Close()
		return nil, err
	}
	if writeFloor {
		if err := store.writeFormatFloorAtOpen(dir); err != nil {
			_ = eng.Close()
			_ = lock.Close()
			return nil, err
		}
	}
	return store, nil
}

// Close flushes and closes the durable catalog's storage, then releases its
// directory lock.
func (s *DurablePolicyStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.shut.Store(true)
	// The lock goes only once the engine has closed, and even if closing
	// failed: either way this process writes the directory no more.
	engineErr := s.engine.Close()
	lockErr := s.lock.Close()
	if engineErr != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "close policy catalog storage", engineErr)
	}
	if lockErr != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "release policy catalog lock", lockErr)
	}
	return nil
}

func (s *DurablePolicyStore) load() error {
	scanner, err := s.engine.Scan(policyTable, iterrt.InfiniteRange(), engine.ScanOptions{
		ColumnFamilies: [][]byte{[]byte(policyRecordCF)}, ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "scan policy records", err)
	}
	defer scanner.Close()

	sourceClaims := make(map[string]persistedSourceClaim)
	revisions := make(map[string]persistedRevision)
	currents := make(map[string]persistedCurrent)
	edges := make(map[string]persistedEdge)
	edgeClaims := make(map[string]persistedEdge)
	nodes := make(map[string]persistedNode)
	coOccurrences := make(map[string]persistedCoOccurrence)
	var declassifications []sequencedDeclassification
	declassificationSeqs := make(map[string]struct{})
	interactionRevisions := make(map[string]persistedInteractionRevisions)
	var (
		floorRecord     persistedFormatFloor
		haveFloor       bool
		versionRecord   persistedSourceVersion
		haveVersion     bool
		migrationRecord persistedLabelMigration
		haveMigration   bool
		maxSeq          uint64
	)

	for scanner.Next() {
		key := scanner.Key()
		if !bytes.Equal(key.ColumnQualifier, []byte(policyRecordCQ)) {
			if err := scanner.Advance(); err != nil {
				return shoal.WrapError(shoal.ErrorInternal, "advance policy scan", err)
			}
			continue
		}
		row := key.Row
		rowStr := string(row)
		value := scanner.Value()
		// Every cell is decoded, not just the winning one, so a corrupt or
		// truncated record that a newer write shadows still fails closed.
		switch {
		case bytes.Equal(row, []byte(policyRowFormatFloor)):
			// Before the source-claim prefix it shares (see
			// policyRowFormatFloor).
			var record persistedFormatFloor
			if err := decodePolicyRecord(
				value, policyKindFormatFloor, &record,
			); err != nil {
				return corruptPolicyRecord("format floor", err)
			}
			if err := validateFormatFloor(record); err != nil {
				return err
			}
			if !haveFloor || record.Seq >= floorRecord.Seq {
				floorRecord = record
				haveFloor = true
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowInteractionRevisions)):
			var record persistedInteractionRevisions
			if err := decodePolicyRecord(
				value, policyKindInteractionRevisions, &record,
			); err != nil {
				return corruptPolicyRecord("interaction revisions", err)
			}
			if string(policyInteractionRevisionsRow(shoal.ID(record.SessionID))) != rowStr {
				return corruptPolicyRecord("interaction revisions",
					fmt.Errorf("interaction revisions row does not match its record"))
			}
			if prev, ok := interactionRevisions[rowStr]; !ok || record.Seq >= prev.Seq {
				interactionRevisions[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.Equal(row, []byte(policyRowSourceVersion)):
			var record persistedSourceVersion
			if err := decodePolicyRecord(
				value, policyKindSourceVersion, &record,
			); err != nil {
				return corruptPolicyRecord("source version", err)
			}
			if !haveVersion || record.Seq >= versionRecord.Seq {
				versionRecord = record
				haveVersion = true
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.Equal(row, []byte(policyRowLabelMigration)):
			var record persistedLabelMigration
			if err := decodePolicyRecord(
				value, policyKindLabelMigration, &record,
			); err != nil {
				return corruptPolicyRecord("label migration", err)
			}
			if !haveMigration || record.Seq >= migrationRecord.Seq {
				migrationRecord = record
				haveMigration = true
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(retiredPolicyRowDeclassificationClaim)):
			// Retired lineage rows (see retiredPolicyKindDeclassificationClaim):
			// validated strictly, then ignored.
			var record retiredPersistedDeclassificationClaim
			if err := decodePolicyRecord(
				value, retiredPolicyKindDeclassificationClaim, &record,
			); err != nil {
				return corruptPolicyRecord("retired declassification claim binding", err)
			}
			if err := validateRetiredDeclassificationClaim(rowStr, record); err != nil {
				return corruptPolicyRecord("retired declassification claim binding", err)
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowDeclassification)):
			var record persistedDeclassification
			if err := decodePolicyRecord(
				value, policyKindDeclassification, &record,
			); err != nil {
				return corruptPolicyRecord("declassification audit", err)
			}
			decoded, err := declassificationFromPersisted(record)
			if err != nil {
				return corruptPolicyRecord("declassification audit", err)
			}
			if string(policyDeclassificationRow(decoded)) != rowStr {
				return corruptPolicyRecord("declassification audit",
					fmt.Errorf("declassification audit row does not match its record"))
			}
			if _, duplicate := declassificationSeqs[rowStr]; !duplicate {
				declassifications = append(declassifications,
					sequencedDeclassification{seq: record.Seq, record: decoded})
				declassificationSeqs[rowStr] = struct{}{}
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowSourceClaim)):
			var record persistedSourceClaim
			if err := decodePolicyRecord(
				value, policyKindSourceClaim, &record,
			); err != nil {
				return corruptPolicyRecord("source claim", err)
			}
			if prev, ok := sourceClaims[rowStr]; !ok || record.Seq >= prev.Seq {
				sourceClaims[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowRevision)):
			var record persistedRevision
			if err := decodePolicyRecord(
				value, policyKindRevision, &record,
			); err != nil {
				return corruptPolicyRecord("revision", err)
			}
			if prev, ok := revisions[rowStr]; !ok || record.Seq >= prev.Seq {
				revisions[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowCurrent)):
			var record persistedCurrent
			if err := decodePolicyRecord(
				value, policyKindCurrent, &record,
			); err != nil {
				return corruptPolicyRecord("current revision", err)
			}
			if prev, ok := currents[rowStr]; !ok || record.Seq >= prev.Seq {
				currents[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowEdgeClaim)):
			var record persistedEdge
			if err := decodePolicyRecord(
				value, policyKindEdgeClaim, &record,
			); err != nil {
				return corruptPolicyRecord("edge reservation", err)
			}
			if prev, ok := edgeClaims[rowStr]; !ok || record.Seq >= prev.Seq {
				edgeClaims[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowEdge)):
			var record persistedEdge
			if err := decodePolicyRecord(
				value, policyKindEdge, &record,
			); err != nil {
				return corruptPolicyRecord("edge", err)
			}
			if prev, ok := edges[rowStr]; !ok || record.Seq >= prev.Seq {
				edges[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowCoOccurrence)):
			var record persistedCoOccurrence
			if err := decodePolicyRecord(
				value, policyKindCoOccurrence, &record,
			); err != nil {
				return corruptPolicyRecord("co-occurrence", err)
			}
			if prev, ok := coOccurrences[rowStr]; !ok || record.Seq >= prev.Seq {
				coOccurrences[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		case bytes.HasPrefix(row, []byte(policyRowNode)):
			var record persistedNode
			if err := decodePolicyRecord(
				value, policyKindNode, &record,
			); err != nil {
				return corruptPolicyRecord("node", err)
			}
			if prev, ok := nodes[rowStr]; !ok || record.Seq >= prev.Seq {
				nodes[rowStr] = record
			}
			maxSeq = maxUint64(maxSeq, record.Seq)
		}
		if err := scanner.Advance(); err != nil {
			return shoal.WrapError(shoal.ErrorInternal, "advance policy scan", err)
		}
	}

	if err := s.reconstruct(
		sourceClaims, revisions, currents, edges, edgeClaims, nodes,
		coOccurrences, versionRecord, haveVersion,
	); err != nil {
		return err
	}
	if err := s.reconstructRevisionBindings(
		haveFloor, edges, edgeClaims, interactionRevisions,
	); err != nil {
		return err
	}
	if haveMigration {
		migration, err := labelMigrationFromPersisted(migrationRecord)
		if err != nil {
			return corruptPolicyRecord("label migration", err)
		}
		s.memory.labelMigration = &migration
	}
	s.memory.declassifications = orderDeclassifications(declassifications)
	s.seq = maxSeq + 1
	return nil
}

// reconstruct rebuilds the embedded MemoryPolicyStore's maps directly from the
// resolved persisted records. It runs before the store is returned, so no lock
// is required. It mirrors what the memory store's own Put paths would leave
// behind, including the additive half of replaceCurrent: current revisions are
// the sole source of the node and intrinsic-edge projections.
func (s *DurablePolicyStore) reconstruct(
	sourceClaims map[string]persistedSourceClaim,
	revisions map[string]persistedRevision,
	currents map[string]persistedCurrent,
	edges map[string]persistedEdge,
	edgeClaims map[string]persistedEdge,
	nodes map[string]persistedNode,
	coOccurrences map[string]persistedCoOccurrence,
	versionRecord persistedSourceVersion,
	haveVersion bool,
) error {
	memory := s.memory

	for _, record := range revisions {
		rule, err := ruleFromPersisted(record.Rule)
		if err != nil {
			return corruptPolicyRecord("revision rule", err)
		}
		key := revisionKey{
			documentID: shoal.ID(record.DocumentID),
			revisionID: shoal.ID(record.RevisionID),
		}
		if err := shoal.ValidateRequiredID("document ID", key.documentID); err != nil {
			return corruptPolicyRecord("revision", err)
		}
		if err := shoal.ValidateRequiredID("revision ID", key.revisionID); err != nil {
			return corruptPolicyRecord("revision", err)
		}
		if owner, ok := memory.revisionIDs[key.revisionID]; ok && owner != key {
			return corruptPolicyRecord(
				"revision", fmt.Errorf("revision identity is reused"))
		}
		memory.revisions[key] = RevisionRegistration{
			DocumentID:     key.documentID,
			RevisionID:     key.revisionID,
			NodeIDs:        idsFromStrings(record.NodeIDs),
			IntrinsicEdges: edgesFromPersisted(record.IntrinsicEdges),
			ContentDigest:  auth.Digest(record.ContentDigest),
			Rule:           rule,
		}
		memory.revisionIDs[key.revisionID] = key
		memory.documentRevisions[key.documentID] = append(
			memory.documentRevisions[key.documentID], key.revisionID)
	}

	for _, record := range currents {
		if record.Tombstone {
			continue
		}
		key := revisionKey{
			documentID: shoal.ID(record.DocumentID),
			revisionID: shoal.ID(record.RevisionID),
		}
		registration, ok := memory.revisions[key]
		if !ok {
			return corruptPolicyRecord(
				"current revision",
				fmt.Errorf("current revision points to an unknown revision"))
		}
		for _, nodeID := range registration.NodeIDs {
			memory.nodes[nodeID] = NodeRegistration{
				DocumentID: registration.DocumentID,
				RevisionID: registration.RevisionID,
				Rule:       mustCloneRule(registration.Rule),
				Kind:       RegistrationDocument,
			}
		}
		for _, edge := range registration.IntrinsicEdges {
			memory.intrinsicEdges[edge.ID] = EdgeRegistration{
				Kind:       RegistrationDocument,
				Edge:       cloneGraphEdge(edge),
				DocumentID: registration.DocumentID,
				RevisionID: registration.RevisionID,
				Rule:       mustCloneRule(registration.Rule),
			}
		}
		memory.current[registration.DocumentID] = key
	}

	var maxVersion uint64
	for _, record := range sourceClaims {
		maxVersion = maxUint64(maxVersion, record.Version)
		if record.Tombstone {
			continue
		}
		claim, err := sourceClaimFromPersisted(record)
		if err != nil {
			return corruptPolicyRecord("source claim", err)
		}
		memory.sourceClaims[record.SourceURI] = sourceClaimState{claim: claim}
	}
	if haveVersion {
		maxVersion = maxUint64(maxVersion, versionRecord.Version)
	}
	memory.sourceVersion = maxVersion

	for _, record := range edges {
		if record.Tombstone {
			continue
		}
		registration, err := edgeRegistrationFromPersisted(record)
		if err != nil {
			return corruptPolicyRecord("edge", err)
		}
		memory.edges[registration.Edge.ID] = registration
	}

	for _, record := range edgeClaims {
		if record.Tombstone {
			continue
		}
		registration, err := edgeRegistrationFromPersisted(record)
		if err != nil {
			return corruptPolicyRecord("edge reservation", err)
		}
		memory.edgeClaims[registration.Edge.ID] = registration
	}
	for _, record := range nodes {
		registration, err := nodeRegistrationFromPersisted(record)
		if err != nil {
			return corruptPolicyRecord("node", err)
		}
		memory.nodes[shoal.ID(record.NodeID)] = registration
	}
	for _, record := range coOccurrences {
		memory.coOccurrence[record.Key] = CoOccurrenceRecord{
			WindowStart: time.Unix(0, record.WindowStartUnixNano).UTC(),
			Domains:     append([]string(nil), record.Domains...),
		}
	}
	return nil
}

// AcquireMutation delegates to the embedded memory store, which serializes base
// mutations across every wrapper sharing this catalog.
func (s *DurablePolicyStore) AcquireMutation(
	ctx context.Context,
) (MutationLease, error) {
	return s.memoryStore().AcquireMutation(ctx)
}

// SourceClaim returns the observable claim for a source URI. It is a pure read
// and delegates unchanged to the memory store.
func (s *DurablePolicyStore) SourceClaim(
	ctx context.Context,
	sourceURI string,
) (SourcePolicyClaim, bool, error) {
	return s.memoryStore().SourceClaim(ctx, sourceURI)
}

// CompareAndSwapSourceClaim performs the in-memory CAS and, on success,
// persists the resulting observable claim so an interrupted mutation recovers
// as a pending claim: a fail-closed state that a future ingest must reauthorize.
func (s *DurablePolicyStore) CompareAndSwapSourceClaim(
	ctx context.Context,
	sourceURI string,
	expected *SourcePolicyClaim,
	desired AccessRule,
) (SourcePolicyClaim, error) {
	if s == nil {
		return (*MemoryPolicyStore)(nil).CompareAndSwapSourceClaim(
			ctx, sourceURI, expected, desired)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The claim this CAS persists carries the URI, the desired rule and,
	// as its previous rule, the expected claim's rule or previous rule.
	admitted := persistedSourceClaim{
		SourceURI: sourceURI, Rule: ruleToPersistedOrEmpty(desired),
		Pending: true,
	}
	if expected != nil {
		previous := ruleToPersistedOrEmpty(expected.Rule)
		if expected.Pending && expected.PreviousRule != nil {
			previous = ruleToPersistedOrEmpty(*expected.PreviousRule)
		}
		admitted.PreviousRule = &previous
	}
	if err := admitPolicyRecord(policyKindSourceClaim, admitted); err != nil {
		return SourcePolicyClaim{}, err
	}
	token, err := s.memory.CompareAndSwapSourceClaim(
		ctx, sourceURI, expected, desired)
	if err != nil {
		return SourcePolicyClaim{}, err
	}
	if err := s.persisted(s.persistSourceClaim(sourceURI)); err != nil {
		return SourcePolicyClaim{}, err
	}
	return token, nil
}

// CommitSourceClaim finalizes the claim in memory and persists the committed
// state.
func (s *DurablePolicyStore) CommitSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).CommitSourceClaim(ctx, claim)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.memory.CommitSourceClaim(ctx, claim); err != nil {
		return err
	}
	return s.persisted(s.persistSourceClaim(claim.SourceURI))
}

// PendSourceClaim finalizes the claim in memory and persists the pending state.
func (s *DurablePolicyStore) PendSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).PendSourceClaim(ctx, claim)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.memory.PendSourceClaim(ctx, claim); err != nil {
		return err
	}
	return s.persisted(s.persistSourceClaim(claim.SourceURI))
}

// RollbackSourceClaim reverts the claim in memory and persists the reverted
// state, writing a tombstone when the claim rolled back to absence.
func (s *DurablePolicyStore) RollbackSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).RollbackSourceClaim(ctx, claim)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.memory.RollbackSourceClaim(ctx, claim); err != nil {
		return err
	}
	return s.persisted(s.persistSourceClaim(claim.SourceURI))
}

// PutRevision registers the revision in memory and persists the revision record
// together with the document's current pointer.
func (s *DurablePolicyStore) PutRevision(
	ctx context.Context,
	registration RevisionRegistration,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).PutRevision(ctx, registration)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := admitRevision(registration); err != nil {
		return err
	}
	if err := s.memory.PutRevision(ctx, registration); err != nil {
		return err
	}
	return s.persisted(s.persistRevision(registration.DocumentID, registration.RevisionID))
}

// PutNode registers an extracted graph node in memory and persists the node
// registration.
func (s *DurablePolicyStore) PutNode(
	ctx context.Context,
	nodeID shoal.ID,
	registration NodeRegistration,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).PutNode(ctx, nodeID, registration)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := admitPolicyRecord(policyKindNode, persistedNode{
		NodeID: string(nodeID), DocumentID: string(registration.DocumentID),
		RevisionID: string(registration.RevisionID),
		Node:       graphNodeToPersisted(registration.Node),
		Rule:       ruleToPersistedOrEmpty(registration.Rule),
		Kind:       uint8(registration.Kind),
	}); err != nil {
		return err
	}
	if err := s.memory.PutNode(ctx, nodeID, registration); err != nil {
		return err
	}
	return s.persisted(s.persistNode(nodeID))
}

// Revision is a pure read delegated to the memory store.
func (s *DurablePolicyStore) Revision(
	ctx context.Context,
	documentID, revisionID shoal.ID,
) (RevisionRegistration, bool, error) {
	return s.memoryStore().Revision(ctx, documentID, revisionID)
}

// CurrentRevision is a pure read delegated to the memory store.
func (s *DurablePolicyStore) CurrentRevision(
	ctx context.Context,
	documentID shoal.ID,
) (RevisionRegistration, bool, error) {
	return s.memoryStore().CurrentRevision(ctx, documentID)
}

// CurrentRevisions is a pure read delegated unchanged to the memory store.
func (s *DurablePolicyStore) CurrentRevisions(
	ctx context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID]RevisionRegistration, error) {
	return s.memoryStore().CurrentRevisions(ctx, documentIDs)
}

// Node is a pure read delegated to the memory store.
func (s *DurablePolicyStore) Node(
	ctx context.Context,
	nodeID shoal.ID,
) (NodeRegistration, bool, error) {
	return s.memoryStore().Node(ctx, nodeID)
}

// Nodes is a pure read delegated unchanged to the memory store, preserving its
// batched, fully-interleaved validation semantics exactly.
func (s *DurablePolicyStore) Nodes(
	ctx context.Context,
	nodeIDs []shoal.ID,
) (map[shoal.ID]NodeRegistration, error) {
	return s.memoryStore().Nodes(ctx, nodeIDs)
}

// ReserveEdge reserves an application edge in memory and persists the
// reservation.
func (s *DurablePolicyStore) ReserveEdge(
	ctx context.Context,
	registration EdgeRegistration,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).ReserveEdge(ctx, registration)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := admitEdge(registration); err != nil {
		return err
	}
	if edgeRegistrationBound(registration) {
		if err := s.ensureFormatFloor(); err != nil {
			return err
		}
	}
	if err := s.memory.ReserveEdge(ctx, registration); err != nil {
		return err
	}
	return s.persisted(s.persistEdgeClaim(registration.Edge.ID))
}

// RollbackEdgeReservation removes the reservation in memory and persists a
// tombstone when the reservation is gone.
func (s *DurablePolicyStore) RollbackEdgeReservation(
	ctx context.Context,
	registration EdgeRegistration,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).RollbackEdgeReservation(ctx, registration)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.memory.RollbackEdgeReservation(ctx, registration); err != nil {
		return err
	}
	return s.persisted(s.persistEdgeClaim(registration.Edge.ID))
}

// PutEdge registers an application edge in memory and persists the edge while
// tombstoning any consumed reservation.
func (s *DurablePolicyStore) PutEdge(
	ctx context.Context,
	registration EdgeRegistration,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).PutEdge(ctx, registration)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := admitEdge(registration); err != nil {
		return err
	}
	if edgeRegistrationBound(registration) {
		if err := s.ensureFormatFloor(); err != nil {
			return err
		}
	}
	if err := s.memory.PutEdge(ctx, registration); err != nil {
		return err
	}
	if err := s.persisted(s.persistApplicationEdge(registration.Edge.ID)); err != nil {
		return err
	}
	return s.persisted(s.persistEdgeClaim(registration.Edge.ID))
}

// Edge is a pure read delegated to the memory store.
func (s *DurablePolicyStore) Edge(
	ctx context.Context,
	edgeID shoal.ID,
) (EdgeRegistration, bool, error) {
	return s.memoryStore().Edge(ctx, edgeID)
}

// Edges is a pure read delegated unchanged to the memory store, preserving its
// batched, fully-interleaved validation semantics exactly.
func (s *DurablePolicyStore) Edges(
	ctx context.Context,
	edgeIDs []shoal.ID,
) (map[shoal.ID]EdgeRegistration, error) {
	return s.memoryStore().Edges(ctx, edgeIDs)
}

// LoadCoOccurrence is a pure read delegated to the memory store, whose state was
// reconstructed from the engine on open.
func (s *DurablePolicyStore) LoadCoOccurrence(
	ctx context.Context,
	key string,
) (CoOccurrenceRecord, bool, error) {
	return s.memoryStore().LoadCoOccurrence(ctx, key)
}

// StoreCoOccurrence updates the identity's mosaic co-occurrence state in memory
// and writes it through to the engine so the co-occurrence budget survives
// restarts. The mutation and its persistence are serialized together under mu,
// exactly like every other durable write.
func (s *DurablePolicyStore) StoreCoOccurrence(
	ctx context.Context,
	key string,
	record CoOccurrenceRecord,
) error {
	if s == nil {
		return (*MemoryPolicyStore)(nil).StoreCoOccurrence(ctx, key, record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := admitPolicyRecord(policyKindCoOccurrence, persistedCoOccurrence{
		Key: key, WindowStartUnixNano: record.WindowStart.UnixNano(),
		Domains: record.Domains,
	}); err != nil {
		return err
	}
	if err := s.memory.StoreCoOccurrence(ctx, key, record); err != nil {
		return err
	}
	return s.persisted(s.persistCoOccurrence(key))
}

// persisted marks the store diverged when the write-through of a change the
// memory store already applied failed, at any step.
func (s *DurablePolicyStore) persisted(err error) error {
	// A write racing Close is refused, not a divergence: the process is
	// shutting down and the next open reloads from disk anyway.
	if err != nil && !s.closed {
		s.markDiverged()
	}
	return err
}

func (s *DurablePolicyStore) memoryStore() *MemoryPolicyStore {
	if s == nil || s.diverged.Load() || s.shut.Load() {
		return nil
	}
	return s.memory
}

// HasRegistrations reports whether the catalog holds any revision
// registration. Immediately after open it reflects what was reconstructed from
// disk: false on a non-empty corpus is the signature of a lost or unmounted
// policy volume, which the caller uses to refuse to serve rather than present a
// silently under-authorized workspace.
func (s *DurablePolicyStore) HasRegistrations() bool {
	return s.memoryStore().registrationCount() > 0
}

func (s *DurablePolicyStore) nextSeq() uint64 {
	s.seq++
	return s.seq
}

// persistSourceClaim writes the current in-memory claim state for a URI, or a
// tombstone when the URI is no longer claimed, and refreshes the persisted
// source-version counter so versions stay monotonic across restarts.
func (s *DurablePolicyStore) persistSourceClaim(sourceURI string) error {
	claim, present := s.memory.snapshotSourceClaim(sourceURI)
	record := persistedSourceClaim{
		Seq:       s.nextSeq(),
		SourceURI: sourceURI,
	}
	if !present {
		record.Tombstone = true
	} else {
		rule, err := ruleToPersisted(claim.Rule)
		if err != nil {
			return catalogUnavailable()
		}
		record.Rule = rule
		record.Pending = claim.Pending
		record.Version = claim.Version
		if claim.PreviousRule != nil {
			previous, err := ruleToPersisted(*claim.PreviousRule)
			if err != nil {
				return catalogUnavailable()
			}
			record.PreviousRule = &previous
		}
	}
	if err := s.writeRow(
		policySourceClaimRow(sourceURI), policyKindSourceClaim, record,
	); err != nil {
		return err
	}
	version := persistedSourceVersion{
		Seq:     s.nextSeq(),
		Version: s.memory.snapshotSourceVersion(),
	}
	return s.writeRow(
		[]byte(policyRowSourceVersion), policyKindSourceVersion, version)
}

func (s *DurablePolicyStore) persistRevision(
	documentID, revisionID shoal.ID,
) error {
	key := revisionKey{documentID: documentID, revisionID: revisionID}
	stored, ok := s.memory.snapshotRevision(key)
	if !ok {
		return catalogUnavailable()
	}
	rule, err := ruleToPersisted(stored.Rule)
	if err != nil {
		return catalogUnavailable()
	}
	record := persistedRevision{
		Seq:            s.nextSeq(),
		DocumentID:     string(documentID),
		RevisionID:     string(revisionID),
		NodeIDs:        stringsFromIDs(stored.NodeIDs),
		IntrinsicEdges: edgesToPersisted(stored.IntrinsicEdges),
		ContentDigest:  [32]byte(stored.ContentDigest),
		Rule:           rule,
	}
	if err := s.writeRow(
		policyRevisionRow(key), policyKindRevision, record,
	); err != nil {
		return err
	}
	currentRevision, ok := s.memory.snapshotCurrent(documentID)
	if !ok {
		return nil
	}
	current := persistedCurrent{
		Seq:        s.nextSeq(),
		DocumentID: string(documentID),
		RevisionID: string(currentRevision),
	}
	return s.writeRow(
		policyCurrentRow(documentID), policyKindCurrent, current)
}

func (s *DurablePolicyStore) persistNode(nodeID shoal.ID) error {
	registration, ok := s.memory.snapshotNode(nodeID)
	if !ok {
		return catalogUnavailable()
	}
	rule, err := ruleToPersisted(registration.Rule)
	if err != nil {
		return catalogUnavailable()
	}
	record := persistedNode{
		Seq:        s.nextSeq(),
		NodeID:     string(nodeID),
		DocumentID: string(registration.DocumentID),
		RevisionID: string(registration.RevisionID),
		Node:       graphNodeToPersisted(registration.Node),
		Rule:       rule,
		Kind:       uint8(registration.Kind),
	}
	return s.writeRow(policyNodeRow(nodeID), policyKindNode, record)
}

func (s *DurablePolicyStore) persistCoOccurrence(key string) error {
	record, ok := s.memory.snapshotCoOccurrence(key)
	if !ok {
		return catalogUnavailable()
	}
	return s.writeRow(policyCoOccurrenceRow(key), policyKindCoOccurrence,
		persistedCoOccurrence{
			Seq:                 s.nextSeq(),
			Key:                 key,
			WindowStartUnixNano: record.WindowStart.UnixNano(),
			Domains:             append([]string(nil), record.Domains...),
		})
}

func (s *DurablePolicyStore) persistApplicationEdge(edgeID shoal.ID) error {
	registration, ok := s.memory.snapshotApplicationEdge(edgeID)
	if !ok {
		return catalogUnavailable()
	}
	record, err := edgeToPersisted(registration, s.nextSeq(), false)
	if err != nil {
		return catalogUnavailable()
	}
	return s.writeRow(policyEdgeRow(edgeID), policyKindEdge, record)
}

func (s *DurablePolicyStore) persistEdgeClaim(edgeID shoal.ID) error {
	registration, present := s.memory.snapshotEdgeClaim(edgeID)
	if !present {
		tombstone := persistedEdge{Seq: s.nextSeq(), Tombstone: true}
		return s.writeRow(
			policyEdgeClaimRow(edgeID), policyKindEdgeClaim, tombstone)
	}
	record, err := edgeToPersisted(registration, s.nextSeq(), false)
	if err != nil {
		return catalogUnavailable()
	}
	return s.writeRow(policyEdgeClaimRow(edgeID), policyKindEdgeClaim, record)
}

// writeRow persists one record whose change the memory store has already
// applied, so any failure here marks the store diverged, and it fails closed
// from then on (see DurablePolicyStore.diverged).
func (s *DurablePolicyStore) writeRow(row []byte, kind byte, value any) error {
	if err := s.writeRowBeforeApply(row, kind, value); err != nil {
		if !s.closed && !s.diverged.Load() {
			s.markDiverged()
		}
		return err
	}
	return nil
}

// writeRowBeforeApply persists one record before the memory store applies
// it, so a failure leaves memory and disk in agreement and is only an error.
// A store that has already diverged, or has been closed, refuses.
func (s *DurablePolicyStore) writeRowBeforeApply(
	row []byte, kind byte, value any,
) error {
	if s.closed || s.diverged.Load() {
		return catalogUnavailable()
	}
	encoded, err := encodePolicyRecord(kind, value)
	if err != nil {
		return catalogUnavailable()
	}
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		return catalogUnavailable()
	}
	mutation.PutLatest([]byte(policyRecordCF), []byte(policyRecordCQ), nil, encoded)
	if err := s.engine.Write(policyTable, []*cclient.Mutation{mutation}); err != nil {
		return catalogUnavailable()
	}
	return nil
}

// --- snapshot helpers reading the memory store under its own lock ---

func (s *MemoryPolicyStore) registrationCount() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.revisions)
}

func (s *MemoryPolicyStore) snapshotSourceClaim(
	sourceURI string,
) (SourcePolicyClaim, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.sourceClaims[sourceURI]
	if !ok {
		return SourcePolicyClaim{}, false
	}
	return state.claim, true
}

func (s *MemoryPolicyStore) snapshotSourceVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sourceVersion
}

func (s *MemoryPolicyStore) snapshotRevision(
	key revisionKey,
) (RevisionRegistration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registration, ok := s.revisions[key]
	if !ok {
		return RevisionRegistration{}, false
	}
	return cloneRevisionRegistration(registration), true
}

func (s *MemoryPolicyStore) snapshotCurrent(
	documentID shoal.ID,
) (shoal.ID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.current[documentID]
	if !ok {
		return "", false
	}
	return key.revisionID, true
}

func (s *MemoryPolicyStore) snapshotNode(
	nodeID shoal.ID,
) (NodeRegistration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registration, ok := s.nodes[nodeID]
	if !ok {
		return NodeRegistration{}, false
	}
	cloned, err := cloneNodeRegistration(registration)
	if err != nil {
		return NodeRegistration{}, false
	}
	return cloned, true
}

func (s *MemoryPolicyStore) snapshotApplicationEdge(
	edgeID shoal.ID,
) (EdgeRegistration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registration, ok := s.edges[edgeID]
	if !ok {
		return EdgeRegistration{}, false
	}
	return cloneEdgeRegistration(registration), true
}

func (s *MemoryPolicyStore) snapshotEdgeClaim(
	edgeID shoal.ID,
) (EdgeRegistration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registration, ok := s.edgeClaims[edgeID]
	if !ok {
		return EdgeRegistration{}, false
	}
	return cloneEdgeRegistration(registration), true
}

// --- serialization ---

// Admission. The diverged mode means only "memory changed and the disk write
// genuinely failed". It must never be the outcome of input that could have
// been validated first, or one oversized ingest would take the whole catalog
// down until restart. So every write that applies to memory before it
// persists first encodes the record its input will persist as, and refuses
// it with an ordinary per-request error when it cannot be encoded or does not
// fit.
//
// The admission bound is below the persistence bound by a headroom. A record
// that is later rewritten from memory with a grown rule (TightenRule conjoins
// a label policy into a revision's, node's or edge's rule; a rule is at most
// auth.MaxPolicyTerms components of bounded size, far below the headroom)
// therefore still fits when it is persisted. A source claim is admitted when
// it is acquired (its URI is caller input); finishing, pending or rolling it
// back rewrites it with no more than that.
//
// Both are variables only so tests can lower them together.
var (
	// policyRecordPersistBytes is the bound encodePolicyRecord enforces on
	// write. Load always accepts up to maxPolicyRecordBytes.
	policyRecordPersistBytes      = maxPolicyRecordBytes
	policyRecordAdmissionHeadroom = uint64(1 << 20)
)

func policyRecordAdmissionBytes() uint64 {
	return policyRecordPersistBytes - policyRecordAdmissionHeadroom
}

// errPolicyRecordTooLarge marks an admission refusal, so the client can
// report it as the caller's input rather than an unavailable catalog.
var errPolicyRecordTooLarge = errors.New("policy catalog record exceeds its size bound")

func admitPolicyRecord(kind byte, value any) error {
	encoded, err := encodePolicyRecord(kind, value)
	if err != nil || uint64(len(encoded)-policyEnvelopeHeader) > policyRecordAdmissionBytes() {
		return shoal.WrapError(shoal.ErrorInvalidArgument,
			"the policy catalog record for this write exceeds its size bound",
			errPolicyRecordTooLarge)
	}
	return nil
}

func admitRevision(registration RevisionRegistration) error {
	return admitPolicyRecord(policyKindRevision, persistedRevision{
		DocumentID:     string(registration.DocumentID),
		RevisionID:     string(registration.RevisionID),
		NodeIDs:        stringsFromIDs(registration.NodeIDs),
		IntrinsicEdges: edgesToPersisted(registration.IntrinsicEdges),
		ContentDigest:  [32]byte(registration.ContentDigest),
		Rule:           ruleToPersistedOrEmpty(registration.Rule),
	})
}

func admitEdge(registration EdgeRegistration) error {
	record, err := edgeToPersisted(registration, 0, false)
	if err != nil {
		// An invalid registration is the memory store's to refuse.
		return nil
	}
	return admitPolicyRecord(policyKindEdge, record)
}

// ruleToPersistedOrEmpty is ruleToPersisted for admission: an invalid rule is
// the memory store's to refuse, so it measures as empty here.
func ruleToPersistedOrEmpty(rule AccessRule) persistedRule {
	persisted, err := ruleToPersisted(rule)
	if err != nil {
		return persistedRule{}
	}
	return persisted
}

func encodePolicyRecord(kind byte, value any) ([]byte, error) {
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(value); err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	if uint64(payload.Len()) > policyRecordPersistBytes {
		return nil, fmt.Errorf("payload exceeds policy record bound")
	}
	encoded := make([]byte, policyEnvelopeHeader+payload.Len())
	copy(encoded, policyRecordMagic)
	encoded[8] = policyEnvelopeVersion
	encoded[9] = kind
	binary.BigEndian.PutUint64(encoded[10:18], uint64(payload.Len()))
	checksum := sha256.Sum256(payload.Bytes())
	copy(encoded[18:18+sha256.Size], checksum[:])
	copy(encoded[policyEnvelopeHeader:], payload.Bytes())
	return encoded, nil
}

func decodePolicyRecord(encoded []byte, expectedKind byte, destination any) error {
	if len(encoded) < policyEnvelopeHeader {
		return fmt.Errorf("policy record envelope is truncated")
	}
	if !bytes.Equal(encoded[:8], []byte(policyRecordMagic)) {
		return fmt.Errorf("policy record magic is invalid")
	}
	if encoded[8] != policyEnvelopeVersion {
		return fmt.Errorf("policy record envelope version %d is unsupported", encoded[8])
	}
	if encoded[9] != expectedKind {
		return fmt.Errorf("policy record kind %d is invalid", encoded[9])
	}
	payloadLength := binary.BigEndian.Uint64(encoded[10:18])
	if payloadLength > maxPolicyRecordBytes {
		return fmt.Errorf("policy record payload exceeds its bound")
	}
	if payloadLength != uint64(len(encoded)-policyEnvelopeHeader) {
		return fmt.Errorf("policy record payload length is invalid")
	}
	payload := encoded[policyEnvelopeHeader:]
	checksum := sha256.Sum256(payload)
	if !bytes.Equal(encoded[18:18+sha256.Size], checksum[:]) {
		return fmt.Errorf("policy record checksum is invalid")
	}
	decoder := gob.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("policy record payload has trailing data")
	}
	return nil
}

func ruleToPersisted(rule AccessRule) (persistedRule, error) {
	if len(rule.policies) == 0 {
		return persistedRule{}, fmt.Errorf("access rule is empty")
	}
	policies := make([][]byte, 0, len(rule.policies))
	for _, policy := range rule.policies {
		encoded, err := policy.Encode()
		if err != nil {
			return persistedRule{}, err
		}
		policies = append(policies, encoded)
	}
	return persistedRule{Policies: policies}, nil
}

func ruleFromPersisted(record persistedRule) (AccessRule, error) {
	if len(record.Policies) == 0 {
		return AccessRule{}, fmt.Errorf("access rule is empty")
	}
	policies := make([]auth.Policy, 0, len(record.Policies))
	for _, encoded := range record.Policies {
		policy, err := auth.DecodePolicy(encoded)
		if err != nil {
			return AccessRule{}, err
		}
		policies = append(policies, policy)
	}
	return NewAccessRule(policies...)
}

func sourceClaimFromPersisted(
	record persistedSourceClaim,
) (SourcePolicyClaim, error) {
	if err := validateSourceURI(record.SourceURI); err != nil {
		return SourcePolicyClaim{}, err
	}
	rule, err := ruleFromPersisted(record.Rule)
	if err != nil {
		return SourcePolicyClaim{}, err
	}
	claim := SourcePolicyClaim{
		SourceURI: record.SourceURI,
		Rule:      rule,
		Pending:   record.Pending,
		Version:   record.Version,
	}
	if record.PreviousRule != nil {
		previous, err := ruleFromPersisted(*record.PreviousRule)
		if err != nil {
			return SourcePolicyClaim{}, err
		}
		claim.PreviousRule = &previous
	}
	// cloneSourcePolicyClaim enforces the same stable-claim invariants the
	// memory store relies on, so a nonsensical persisted claim fails closed.
	return cloneSourcePolicyClaim(claim)
}

func edgeToPersisted(
	registration EdgeRegistration,
	seq uint64,
	tombstone bool,
) (persistedEdge, error) {
	rule, err := ruleToPersisted(registration.Rule)
	if err != nil {
		return persistedEdge{}, err
	}
	return persistedEdge{
		Seq:                  seq,
		Tombstone:            tombstone,
		Edge:                 graphEdgeToPersisted(registration.Edge),
		DocumentID:           string(registration.DocumentID),
		RevisionID:           string(registration.RevisionID),
		Rule:                 rule,
		Kind:                 uint8(registration.Kind),
		SourceRevisions:      revisionReferencesToPersisted(registration.SourceRevisions),
		SourceRevisionsBound: registration.SourceRevisionsBound,
	}, nil
}

func edgeRegistrationFromPersisted(
	record persistedEdge,
) (EdgeRegistration, error) {
	rule, err := ruleFromPersisted(record.Rule)
	if err != nil {
		return EdgeRegistration{}, err
	}
	edge := graphEdgeFromPersisted(record.Edge)
	if err := edge.Validate(); err != nil {
		return EdgeRegistration{}, err
	}
	kind := RegistrationKind(record.Kind)
	if kind == RegistrationUnspecified {
		kind = legacyEdgeKind()
	}
	sources := revisionReferencesFromPersisted(record.SourceRevisions)
	normalized, err := normalizeApplicationEdgeRegistration(EdgeRegistration{
		Edge:                 edge,
		DocumentID:           shoal.ID(record.DocumentID),
		RevisionID:           shoal.ID(record.RevisionID),
		Rule:                 rule,
		Kind:                 kind,
		SourceRevisions:      sources,
		SourceRevisionsBound: record.SourceRevisionsBound,
	})
	if err != nil {
		return EdgeRegistration{}, err
	}
	// Strict: a binding is stored exactly as normalization writes it, so a
	// reordered, duplicated or otherwise rewritten one is corrupt rather
	// than silently repaired.
	if !revisionReferencesEqual(normalized.SourceRevisions, sources) {
		return EdgeRegistration{}, fmt.Errorf("edge source revisions are not canonical")
	}
	return normalized, nil
}

// legacyEdgeKind is the kind of an edge record persisted before
// RegistrationKind existed. Every such record came through PutEdge, which
// stored no document for application, materialized or extracted edges alike,
// so they cannot be told apart and all keep their pre-kind meaning: an
// edge-local rule plus both endpoints re-checked. An extracted edge in such a
// store regains its document binding the next time it is extracted (see
// extractedEdgeMerge); PR4's migration covers the rest (#570).
func legacyEdgeKind() RegistrationKind { return RegistrationApplication }

// legacyNodeKind infers, once at load, the kind of a node record persisted
// before RegistrationKind existed. Only PutNode records are persisted, and
// only ExtractDocument and MaterializeGraph called PutNode; a materialization
// names its own identity as the owner.
func legacyNodeKind(documentID string) RegistrationKind {
	if strings.HasPrefix(documentID, legacyMaterializationOwnerPrefix) {
		return RegistrationMaterialized
	}
	return RegistrationExtracted
}

// legacyMaterializationOwnerPrefix opens every graph-materialization identity
// (explorer.materializedID("materialization", ...)). It is used only to
// classify records persisted before RegistrationKind existed.
const legacyMaterializationOwnerPrefix = "materialized-materialization-"

func nodeRegistrationFromPersisted(
	record persistedNode,
) (NodeRegistration, error) {
	rule, err := ruleFromPersisted(record.Rule)
	if err != nil {
		return NodeRegistration{}, err
	}
	kind := RegistrationKind(record.Kind)
	if kind == RegistrationUnspecified {
		kind = legacyNodeKind(record.DocumentID)
	}
	return normalizeNodeRegistration(shoal.ID(record.NodeID), NodeRegistration{
		DocumentID: shoal.ID(record.DocumentID),
		RevisionID: shoal.ID(record.RevisionID),
		Node:       graphNodeFromPersisted(record.Node),
		Rule:       rule,
		Kind:       kind,
	})
}

func graphNodeToPersisted(node graph.Node) persistedGraphNode {
	return persistedGraphNode{
		ID:         string(node.ID),
		Kind:       node.Kind,
		Labels:     append([]string(nil), node.Labels...),
		Properties: cloneStringMap(node.Properties),
	}
}

func graphNodeFromPersisted(record persistedGraphNode) graph.Node {
	return graph.Node{
		ID:         shoal.ID(record.ID),
		Kind:       record.Kind,
		Labels:     append([]string(nil), record.Labels...),
		Properties: shoal.Metadata(cloneStringMap(record.Properties)),
	}
}

func graphEdgeToPersisted(edge graph.Edge) persistedGraphEdge {
	return persistedGraphEdge{
		ID:         string(edge.ID),
		From:       string(edge.From),
		To:         string(edge.To),
		Type:       edge.Type,
		Weight:     float64(edge.Weight),
		Properties: cloneStringMap(edge.Properties),
	}
}

func graphEdgeFromPersisted(record persistedGraphEdge) graph.Edge {
	return graph.Edge{
		ID:         shoal.ID(record.ID),
		From:       shoal.ID(record.From),
		To:         shoal.ID(record.To),
		Type:       record.Type,
		Weight:     shoal.Score(record.Weight),
		Properties: metadataFromStringMap(record.Properties),
	}
}

func edgesToPersisted(edges []graph.Edge) []persistedGraphEdge {
	if len(edges) == 0 {
		return nil
	}
	persisted := make([]persistedGraphEdge, len(edges))
	for index, edge := range edges {
		persisted[index] = graphEdgeToPersisted(edge)
	}
	return persisted
}

func edgesFromPersisted(records []persistedGraphEdge) []graph.Edge {
	if len(records) == 0 {
		return nil
	}
	edges := make([]graph.Edge, len(records))
	for index, record := range records {
		edges[index] = graphEdgeFromPersisted(record)
	}
	return edges
}

func stringsFromIDs(ids []shoal.ID) []string {
	if len(ids) == 0 {
		return nil
	}
	values := make([]string, len(ids))
	for index, id := range ids {
		values[index] = string(id)
	}
	return values
}

func idsFromStrings(values []string) []shoal.ID {
	if len(values) == 0 {
		return nil
	}
	ids := make([]shoal.ID, len(values))
	for index, value := range values {
		ids[index] = shoal.ID(value)
	}
	return ids
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func metadataFromStringMap(values map[string]string) shoal.Metadata {
	if len(values) == 0 {
		return nil
	}
	metadata := make(shoal.Metadata, len(values))
	for key, value := range values {
		metadata[key] = value
	}
	return metadata
}

func policySourceClaimRow(sourceURI string) []byte {
	return append([]byte(policyRowSourceClaim), sourceURI...)
}

func policyCoOccurrenceRow(key string) []byte {
	return append([]byte(policyRowCoOccurrence), key...)
}

func policyRevisionRow(key revisionKey) []byte {
	row := make([]byte, 0, len(policyRowRevision)+
		len(key.documentID)+1+len(key.revisionID))
	row = append(row, policyRowRevision...)
	row = append(row, string(key.documentID)...)
	row = append(row, '/')
	row = append(row, string(key.revisionID)...)
	return row
}

func policyCurrentRow(documentID shoal.ID) []byte {
	return append([]byte(policyRowCurrent), string(documentID)...)
}

func policyNodeRow(nodeID shoal.ID) []byte {
	return append([]byte(policyRowNode), string(nodeID)...)
}

func policyEdgeRow(edgeID shoal.ID) []byte {
	return append([]byte(policyRowEdge), string(edgeID)...)
}

func policyEdgeClaimRow(edgeID shoal.ID) []byte {
	return append([]byte(policyRowEdgeClaim), string(edgeID)...)
}

func corruptPolicyRecord(kind string, err error) error {
	return shoal.WrapError(
		shoal.ErrorInternal,
		"stored policy "+kind+" record is corrupt",
		err,
	)
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

var _ PolicyStore = (*DurablePolicyStore)(nil)
