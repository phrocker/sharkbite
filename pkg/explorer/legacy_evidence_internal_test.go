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
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestStoredLegacyEvidenceStaysReadable: a session (and its fold) recorded
// before shared extraction content was canonical (#700) hashed its graph
// anchor over the old rendering: a "Zeta" label, a confidence property, a
// relation weighed by its confidence. After the upgrade it must stay listed
// and readable, across a reopen. A NEW recording with that legacy anchor is
// refused; one over the canonical rendering is accepted.
func TestStoredLegacyEvidenceStaysReadable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := corpus.Ingest(ctx, Source{
		URI: "file:///zeta.md", MediaType: MediaTypeMarkdown, Content: "# Zeta Skill\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The binary from before the rule.
	canonicalSharedContent = false
	defer func() { canonicalSharedContent = true }()
	hex := strings.Repeat("0", 63)
	skill := shoal.ID("inferred-entity:" + hex + "1")
	tool := shoal.ID("inferred-entity:" + hex + "2")
	relation := shoal.ID("inferred-edge:" + hex + "3")
	presented := func(id shoal.ID, key, display string) graph.Node {
		return graph.Node{
			ID: id, Kind: "ontology:skill", Labels: []string{display, "skill"},
			Properties: shoal.Metadata{
				extraction.GraphPropertyOntologyConceptID:  "concept:skill",
				extraction.GraphPropertyOntologyConceptKey: "skill",
				extraction.GraphPropertyEntityKey:          key,
				"confidence":                               "0.500000",
			},
		}
	}
	record := persistedExtraction{
		ID: "document-extraction:legacy", DocumentID: receipt.Document.ID,
		RevisionID: receipt.Revision.ID, OntologySchemaID: "schema",
		OntologyVersionID: "version", PublishedAt: time.Unix(1700000000, 0).UTC(),
		Nodes: []graph.Node{presented(skill, "zeta", "Zeta"), presented(tool, "zeta-cli", "Zeta-cli")},
		Edges: []graph.Edge{{
			ID: relation, From: skill, To: tool, Type: "provides_tool", Weight: 0.55,
			Properties: shoal.Metadata{"ontology_relationship_id": "relationship:provides"},
		}},
	}
	corpus.mu.Lock()
	err = corpus.writeRecord(extractionRecordRow(record.ID), embeddedRecordExtraction, record)
	if err == nil {
		corpus.extractions[record.ID] = &record
		err = corpus.rebuildCurrentGraphLocked()
	}
	corpus.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	session := func(c *Explorer, id string) interaction.Session {
		t.Helper()
		pin, err := c.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c.mu.RLock()
		path := graph.Path{
			Nodes: []graph.Node{cloneNode(c.graphNodes[skill]), cloneNode(c.graphNodes[tool])},
			Edges: []graph.Edge{cloneEdge(c.graphEdges[relation])},
		}
		c.mu.RUnlock()
		anchor, err := inference.NewGraphAnchor(path)
		if err != nil {
			t.Fatal(err)
		}
		return interaction.Session{
			ID: interaction.DerivedID("session", id), RecordedAt: pin.AsOf.Add(time.Second),
			Operation:  interaction.OperationRetrieval,
			SnapshotID: shoal.ID(pin.ID), SnapshotAsOf: pin.AsOf,
			AuthorizationFingerprint: "auth-sha256:legacy",
			AuthorizationExpiresAt:   pin.AsOf.Add(time.Hour),
			SeedNodeIDs:              []shoal.ID{skill, tool},
			SeedEvidence: []interaction.EvidenceReference{{
				AnchorID: anchor.ID(), Kind: interaction.EvidenceGraph,
				NodeIDs: []shoal.ID{skill, tool}, EdgeIDs: []shoal.ID{relation},
			}},
		}
	}
	legacy := session(corpus, "legacy")
	if err := corpus.RecordInteraction(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	fold, err := corpus.FoldInteractions(ctx, FoldRequest{SessionIDs: []shoal.ID{legacy.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	// The upgrade.
	canonicalSharedContent = true
	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	corpus.mu.Lock()
	graphErr := corpus.ensureGraphLocked()
	labels := corpus.graphNodes[skill].Labels
	corpus.mu.Unlock()
	if graphErr != nil {
		t.Fatal(graphErr)
	}
	if strings.Join(labels, ",") != "skill,zeta" {
		t.Fatalf("the upgraded graph is not canonical: %v", labels)
	}
	if _, err := corpus.InteractionRecord(ctx, legacy.ID); err != nil {
		t.Fatalf("a session recorded before the upgrade is unreadable: %v", err)
	}
	summaries, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, summary := range summaries {
		listed = listed || summary.SessionID == legacy.ID
	}
	if !listed {
		t.Fatal("a session recorded before the upgrade is no longer listed")
	}
	if _, err := corpus.RehydrateFold(ctx, fold.FoldID); err != nil {
		t.Fatalf("a fold recorded before the upgrade is unreadable: %v", err)
	}

	// New recordings: the legacy rendering is not authoritative any more.
	replay := legacy
	replay.ID = interaction.DerivedID("session", "legacy-replay")
	pinned := session(corpus, "pin")
	replay.SnapshotID, replay.SnapshotAsOf = pinned.SnapshotID, pinned.SnapshotAsOf
	replay.RecordedAt = pinned.RecordedAt
	replay.AuthorizationExpiresAt = pinned.AuthorizationExpiresAt
	if err := corpus.RecordInteraction(ctx, replay); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("a new recording with a legacy anchor = %v, want refused", err)
	}
	if err := corpus.RecordInteraction(ctx, session(corpus, "canonical")); err != nil {
		t.Fatalf("a new recording over the canonical rendering: %v", err)
	}
}

// TestStoredLegacyEvidenceFollowsTheOldBuild: with two pre-upgrade records
// presenting the entity differently ("Zeta", "ZETA"), the old graph showed
// the first record's, in extraction-ID order, and a session hashed over
// that stays readable after the upgrade; so it does when a canonical record
// written after the upgrade (from any document) sorts before both.
func TestStoredLegacyEvidenceFollowsTheOldBuild(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []IngestResult
	for _, uri := range []string{"file:///a.md", "file:///b.md", "file:///c.md"} {
		receipt, err := corpus.Ingest(ctx, Source{
			URI: uri, MediaType: MediaTypeMarkdown, Content: "# Zeta Skill\n" + uri,
		})
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	canonicalSharedContent = false
	defer func() { canonicalSharedContent = true }()
	hex := strings.Repeat("0", 63)
	skill := shoal.ID("inferred-entity:" + hex + "1")
	presented := func(display string) graph.Node {
		return graph.Node{
			ID: skill, Kind: "ontology:skill", Labels: []string{display, "skill"},
			Properties: shoal.Metadata{
				extraction.GraphPropertyOntologyConceptID:  "concept:skill",
				extraction.GraphPropertyOntologyConceptKey: "skill",
				extraction.GraphPropertyEntityKey:          "zeta",
				"confidence":                               "0.500000",
			},
		}
	}
	inject := func(c *Explorer, id shoal.ID, receipt IngestResult, node graph.Node) {
		t.Helper()
		record := persistedExtraction{
			ID: id, DocumentID: receipt.Document.ID, RevisionID: receipt.Revision.ID,
			OntologySchemaID: "schema", OntologyVersionID: "version",
			PublishedAt: time.Unix(1700000000, 0).UTC(), Nodes: []graph.Node{node},
		}
		c.mu.Lock()
		err := c.writeRecord(extractionRecordRow(record.ID), embeddedRecordExtraction, record)
		if err == nil {
			c.extractions[record.ID] = &record
			err = c.rebuildCurrentGraphLocked()
		}
		c.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	inject(corpus, "document-extraction:b", receipts[1], presented("ZETA"))
	inject(corpus, "document-extraction:a", receipts[0], presented("Zeta"))
	corpus.mu.RLock()
	shown := cloneNode(corpus.graphNodes[skill])
	corpus.mu.RUnlock()
	if strings.Join(shown.Labels, ",") != "Zeta,skill" {
		t.Fatalf("fixture: the old build showed %v, want the first record's", shown.Labels)
	}
	pin, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := inference.NewGraphAnchor(graph.Path{Nodes: []graph.Node{shown}})
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID: interaction.DerivedID("session", "two-legacy"), RecordedAt: pin.AsOf.Add(time.Second),
		Operation:  interaction.OperationRetrieval,
		SnapshotID: shoal.ID(pin.ID), SnapshotAsOf: pin.AsOf,
		AuthorizationFingerprint: "auth-sha256:legacy",
		AuthorizationExpiresAt:   pin.AsOf.Add(time.Hour),
		SeedNodeIDs:              []shoal.ID{skill},
		SeedEvidence: []interaction.EvidenceReference{{
			AnchorID: anchor.ID(), Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{skill},
		}},
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	canonicalSharedContent = true
	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	if _, err := corpus.InteractionRecord(ctx, session.ID); err != nil {
		t.Fatalf("the session over the old build's rendering is unreadable: %v", err)
	}
	canonical, _ := CanonicalExtractedNode(presented("zeta"))
	inject(corpus, "document-extraction:0", receipts[2], canonical)
	if _, err := corpus.InteractionRecord(ctx, session.ID); err != nil {
		t.Fatalf("a later canonical record closed a pre-upgrade session: %v", err)
	}
}
