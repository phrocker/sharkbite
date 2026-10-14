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
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Durable co-owners of shared extracted registrations (#700).
//
// Each co-owner is its own row, so recording one more extractor of a popular
// entity writes one small record instead of rewriting an ever-growing node
// row. Owners are kept per document, so the row key is a digest of
// (registration, document), a newer revision overwrites its row, and
// load requires the record to name exactly that key.
//
// Existing catalogs need no migration: a registration with no co-owner rows
// is a registration owned by its first extractor alone, which is what it was.
//
// Rollback. A binary from before #695 refuses this catalog at its format
// floor (revision_binding_durable.go), as before. A binary with the floor but
// from before #700 skips these rows, as its loader skips every row prefix it
// does not know, and serves each shared registration under its first owner
// alone: narrower, never wider, so that rollback fails closed (the oracle
// #700 closes comes back, and nothing opens). Rolling forward again needs
// nothing: a co-owner's effective rule also conjoins the rule its revision is
// registered under now (coOwnerRule), so a TightenRule the older binary made
// without rewriting these rows is still enforced, and these rows are never
// deleted, so they cannot have gone stale in any other way. So the format
// floor does not need to move.
//
// Strict decoding: the record must match its row, name a registered
// extracted node or edge, differ from that registration's first owner, and
// carry a valid rule; and, since this binary writes a co-owner only after the
// floor, a co-owner row in a catalog without the floor is corrupt.
const (
	policyRowNodeOwner  = "owner/node/"
	policyKindNodeOwner = byte(14)
	policyRowEdgeOwner  = "owner/edge/"
	policyKindEdgeOwner = byte(15)
)

type persistedOwner struct {
	Seq        uint64
	ObjectID   string
	DocumentID string
	RevisionID string
	Rule       persistedRule
}

// policyOwnerRow is one document's co-owner row of one registration: owners
// are kept per document, so a newer revision of it overwrites the row.
func policyOwnerRow(prefix string, key ownerKey) []byte {
	encoded := make([]byte, 0, 16+len(key.objectID)+len(key.documentID))
	for _, part := range []shoal.ID{key.objectID, key.documentID} {
		encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(part)))
		encoded = append(encoded, part...)
	}
	digest := auth.DigestBytes("explorer-policy-extraction-owner-v1", encoded)
	return append([]byte(prefix), hex.EncodeToString(digest[:])...)
}

func ownerToPersisted(
	seq uint64, objectID shoal.ID, owner ExtractionOwner,
) (persistedOwner, error) {
	rule, err := ruleToPersisted(owner.Rule)
	if err != nil {
		return persistedOwner{}, err
	}
	return persistedOwner{
		Seq: seq, ObjectID: string(objectID),
		DocumentID: string(owner.DocumentID),
		RevisionID: string(owner.RevisionID),
		Rule:       rule,
	}, nil
}

// admitOwner refuses, before memory changes, a co-owner record that could
// not be persisted (see admitPolicyRecord).
func admitOwner(kind byte, objectID shoal.ID, owner ExtractionOwner) error {
	record, err := ownerToPersisted(0, objectID, owner)
	if err != nil {
		return err
	}
	return admitPolicyRecord(kind, record)
}

// ownerRow is the batch row persisting one co-owner as memory holds it.
func (s *DurablePolicyStore) ownerRow(
	edge bool, key ownerKey,
) (policyRow, error) {
	owner, ok := s.memory.snapshotOwner(edge, key)
	if !ok {
		return policyRow{}, catalogUnavailable()
	}
	record, err := ownerToPersisted(s.nextSeq(), key.objectID, owner)
	if err != nil {
		return policyRow{}, catalogUnavailable()
	}
	prefix, kind := policyRowNodeOwner, policyKindNodeOwner
	if edge {
		prefix, kind = policyRowEdgeOwner, policyKindEdgeOwner
	}
	return policyRow{policyOwnerRow(prefix, key), kind, record}, nil
}

// persistOwner writes one co-owner the memory store already recorded.
func (s *DurablePolicyStore) persistOwner(edge bool, key ownerKey) error {
	row, err := s.ownerRow(edge, key)
	if err != nil {
		return err
	}
	return s.writeRow(row.row, row.kind, row.value)
}

// snapshotOwner returns one co-owner of a node (or edge) registration.
func (s *MemoryPolicyStore) snapshotOwner(
	edge bool, key ownerKey,
) (ExtractionOwner, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var owners []ExtractionOwner
	if edge {
		registration, ok := s.edges[key.objectID]
		if !ok {
			return ExtractionOwner{}, false
		}
		owners = registration.CoOwners
	} else {
		registration, ok := s.nodes[key.objectID]
		if !ok {
			return ExtractionOwner{}, false
		}
		owners = registration.CoOwners
	}
	for _, owner := range owners {
		if owner.DocumentID == key.documentID {
			rule, err := owner.Rule.clone()
			if err != nil {
				return ExtractionOwner{}, false
			}
			owner.Rule = rule
			return owner, true
		}
	}
	return ExtractionOwner{}, false
}

// decodeOwnerRecord decodes one co-owner cell and checks it names its row.
func decodeOwnerRecord(
	prefix string, kind byte, row string, value []byte,
) (persistedOwner, error) {
	var record persistedOwner
	if err := decodePolicyRecord(value, kind, &record); err != nil {
		return persistedOwner{}, err
	}
	key := ownerKey{
		objectID:   shoal.ID(record.ObjectID),
		documentID: shoal.ID(record.DocumentID),
		revisionID: shoal.ID(record.RevisionID),
	}
	if string(policyOwnerRow(prefix, key)) != row {
		return persistedOwner{}, fmt.Errorf("co-owner row does not match its record")
	}
	return record, nil
}

// reconstructOwners attaches the co-owner rows to the registrations
// reconstruct restored.
func (s *DurablePolicyStore) reconstructOwners(
	haveFloor bool,
	nodeOwners, edgeOwners map[string]persistedOwner,
) error {
	if (len(nodeOwners) > 0 || len(edgeOwners) > 0) && !haveFloor {
		return corruptPolicyRecord("extraction co-owner",
			fmt.Errorf("co-owners were written without the format floor"))
	}
	attach := func(record persistedOwner) (shoal.ID, ExtractionOwner, error) {
		rule, err := ruleFromPersisted(record.Rule)
		if err != nil {
			return "", ExtractionOwner{}, err
		}
		owner := ExtractionOwner{
			DocumentID: shoal.ID(record.DocumentID),
			RevisionID: shoal.ID(record.RevisionID),
			Rule:       rule,
		}
		objectID := shoal.ID(record.ObjectID)
		if err := shoal.ValidateRequiredID("co-owned object ID", objectID); err != nil {
			return "", ExtractionOwner{}, err
		}
		return objectID, owner, nil
	}
	nodes := make(map[shoal.ID][]ExtractionOwner)
	for _, record := range nodeOwners {
		objectID, owner, err := attach(record)
		if err != nil {
			return corruptPolicyRecord("extraction co-owner", err)
		}
		nodes[objectID] = append(nodes[objectID], owner)
	}
	for nodeID, owners := range nodes {
		registration, ok := s.memory.nodes[nodeID]
		if !ok || registration.Kind != RegistrationExtracted {
			return corruptPolicyRecord("extraction co-owner",
				fmt.Errorf("a co-owner names no extracted node"))
		}
		sortOwners(owners)
		if !canonicalOwners(registration.DocumentID, owners) {
			return corruptPolicyRecord("extraction co-owner",
				fmt.Errorf("node co-owners are not a canonical set"))
		}
		registration.CoOwners = owners
		s.memory.nodes[nodeID] = registration
	}
	edges := make(map[shoal.ID][]ExtractionOwner)
	for _, record := range edgeOwners {
		objectID, owner, err := attach(record)
		if err != nil {
			return corruptPolicyRecord("extraction co-owner", err)
		}
		edges[objectID] = append(edges[objectID], owner)
	}
	for edgeID, owners := range edges {
		registration, ok := s.memory.edges[edgeID]
		if !ok || registration.Kind != RegistrationExtracted {
			return corruptPolicyRecord("extraction co-owner",
				fmt.Errorf("a co-owner names no extracted edge"))
		}
		sortOwners(owners)
		if !canonicalOwners(registration.DocumentID, owners) {
			return corruptPolicyRecord("extraction co-owner",
				fmt.Errorf("edge co-owners are not a canonical set"))
		}
		registration.CoOwners = owners
		s.memory.edges[edgeID] = registration
	}
	return nil
}

func sortOwners(owners []ExtractionOwner) {
	sort.Slice(owners, func(left, right int) bool {
		return compareOwners(owners[left], owners[right]) < 0
	})
}
