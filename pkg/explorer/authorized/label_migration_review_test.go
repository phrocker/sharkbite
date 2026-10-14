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
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestLabelMigrationClosesLegacyRelationBetweenOtherDocumentsEntities is the
// review's repro. Under the legacy catalog every document shares the bare
// rule, so entities collapse across documents, and relations were persisted
// before RegistrationKind, so they decode as application edges naming no
// document. The relation alpha -provides_tool-> shared-cli is stated only by
// the labelled D2, while alpha belongs to the public D1 and shared-cli to the
// public D3: both endpoints stay visible after the migration, so only the
// base's extraction records can tie the relation to D2.
func TestLabelMigrationClosesLegacyRelationBetweenOtherDocumentsEntities(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		legacy := &legacyStore{PolicyStore: store, legacyEdgeKinds: true}
		legacy.setStrip(true)
		w := newExtractedEdgeWorld(t, legacy)
		d1 := w.extract(w.ingest("file:///alpha/SKILL.md", skillBody("alpha", "other-cli"), nil))
		d3 := w.extract(w.ingest("file:///beta/SKILL.md", skillBody("beta", "shared-cli"), nil))
		d2 := w.extract(w.ingest("file:///alpha2/SKILL.md", skillBody("alpha", "shared-cli"),
			shoal.Metadata{interaction.PropertyVisibility: "secret"}))
		legacy.mu.Lock()
		legacy.strip, legacy.legacyEdgeKinds = false, false
		legacy.mu.Unlock()

		owned := map[shoal.ID]bool{}
		for _, node := range append(d1.GraphNodes, d3.GraphNodes...) {
			owned[node.ID] = true
		}
		earlier := map[shoal.ID]bool{}
		for _, edge := range append(d1.GraphEdges, d3.GraphEdges...) {
			earlier[edge.ID] = true
		}
		var relation, from shoal.ID
		for _, edge := range d2.GraphEdges {
			if edge.Type == "provides_tool" && owned[edge.From] && owned[edge.To] && !earlier[edge.ID] {
				relation, from = edge.ID, edge.From
			}
		}
		if relation == "" {
			t.Fatal("fixture no longer produces a D2-only relation between other documents' entities")
		}
		registration, ok, err := store.Edge(context.Background(), relation)
		if err != nil || !ok || registration.Kind != authorized.RegistrationApplication ||
			registration.DocumentID != "" {
			t.Fatalf("the relation is not in its legacy shape: %+v, %v, %v", registration, ok, err)
		}
		if !w.edgeVisible(w.f.labelIngester(t, "before"), from, relation) {
			t.Fatal("the legacy relation is not open before the migration; the test would be vacuous")
		}

		migrate(t, w.client)

		if w.edgeVisible(w.f.labelIngester(t, "reader"), from, relation) {
			t.Fatal("a source-only reader still sees a relation only the labelled document states")
		}
		if !w.edgeVisible(w.f.labelIngester(t, "holder", "secret"), from, relation) {
			t.Fatal("the label holder lost the relation")
		}
		// The public document's own relation stays open.
		publicRelation, publicFrom := providesTool(t, d1)
		if !w.edgeVisible(w.f.labelIngester(t, "reader"), publicFrom, publicRelation) {
			t.Fatal("a public document's own relation closed")
		}
	})
}

// TestLabelMigrationClosesDocumentsWrittenAfterARollback pins why the
// migration runs on every start: after a first migration, a rollback to a
// binary from before #585 registers a labelled document under the bare
// source rule again, and the next start must close it.
func TestLabelMigrationClosesDocumentsWrittenAfterARollback(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w, legacy := newLegacyLabelWorld(t, store)
		migrate(t, w.clientA)

		// The rolled-back binary ingests a labelled document.
		legacy.setStrip(true)
		rolledBack, err := w.clientA.Ingest(w.labelAdmin(t), labelledSource(
			"file:///label/after-rollback.txt", "written by an old binary", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		legacy.setStrip(false)
		if !sourceOnlySees(t, w, rolledBack.Document.ID) {
			t.Fatal("the rollback did not reopen anything; the test would be vacuous")
		}

		// The re-upgraded binary starts again.
		record := migrate(t, w.clientA)
		if record.Tightened != 1 {
			t.Fatalf("report after the re-upgrade = %+v", record)
		}
		if sourceOnlySees(t, w, rolledBack.Document.ID) {
			t.Fatal("a document labelled under the rolled-back binary is still open")
		}
		summaries, err := w.clientA.Documents(w.secretHolder(t))
		requireNoError(t, "Documents", err)
		if !summariesContain(summaries, rolledBack.Document.ID) {
			t.Fatal("the label holder cannot read the re-tightened document")
		}
		runLabelConformanceTable(t, w)
	})
}
