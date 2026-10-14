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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestLabelRelabelClosesExtractedEntities pins that an entity extracted from
// an unlabelled revision closes when the document is relabelled: derived
// nodes are governed by their own registration rule AND the document's
// current rule, on every node read path (#570).
func TestLabelRelabelClosesExtractedEntities(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		selector := f.staticSelector(t, f.sourceA, f.policyA)
		client := f.labelClient(t, f.base, store, selector, selector)
		const uri = "file:///label/derived/SKILL.md"
		owner := f.labelIngester(t, "owner", "secret")
		public, err := client.Ingest(owner, explorer.Source{
			URI: uri, MediaType: explorer.MediaTypeMarkdown,
			Content: authorizedSkillMarkdown,
		})
		if err != nil {
			t.Fatal(err)
		}
		reader := f.labelIngester(t, "reader")
		extracted, err := client.ExtractDocument(reader, explorer.ExtractionRequest{
			DocumentID: public.Document.ID, RevisionID: public.Revision.ID,
			Version: authorizedSkillsOntologyVersion(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(extracted.EntityNodeIDs) == 0 {
			t.Fatal("extraction produced no entity")
		}
		entity := extracted.EntityNodeIDs[0]
		// Before the relabel the source holder sees the entity.
		if _, err := client.Neighborhood(reader, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{entity}, Depth: 1,
		}); err != nil {
			t.Fatalf("entity before relabel: %v", err)
		}

		// An interaction recorded on the entity while it was public.
		snapshot, err := f.base.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Set(snapshot.AsOf.Add(time.Second))
		recorder := f.decision(t, "recorder",
			[][]byte{f.sourceA, f.sourceB}, [][]byte{f.policyA, f.policyB},
			labelReaderOperations)
		fingerprint, err := auth.AuthorizationFingerprint(recorder)
		if err != nil {
			t.Fatal(err)
		}
		session := interaction.Session{
			ID:                       interaction.DerivedID("session", "derived-entity"),
			RecordedAt:               f.clock.Now(),
			SnapshotID:               shoal.ID(snapshot.ID),
			SnapshotAsOf:             snapshot.AsOf,
			AuthorizationFingerprint: shoal.ID(fingerprint.String()),
			AuthorizationExpiresAt:   recorder.AuthenticationExpires(),
			SeedNodeIDs:              []shoal.ID{entity},
		}
		if err := client.RecordInteraction(f.context(t, recorder), session); err != nil {
			t.Fatalf("record on the public entity: %v", err)
		}
		owner = f.labelIngester(t, "owner", "secret")

		relabelled := explorer.Source{
			URI: uri, MediaType: explorer.MediaTypeMarkdown,
			Content:  authorizedSkillMarkdown + "\nNow secret.\n",
			Metadata: shoal.Metadata{"shoal.visibility": "secret"},
		}
		if _, err := client.Ingest(owner, relabelled); err != nil {
			t.Fatal(err)
		}

		holderGraph, err := client.Neighborhood(owner, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{entity}, Depth: 1,
		})
		if err != nil {
			t.Fatalf("label holder lost the extracted entity: %v", err)
		}
		var entityNode graph.Node
		for _, node := range holderGraph.Nodes {
			if node.ID == entity {
				entityNode = node
			}
		}
		if entityNode.ID == "" {
			t.Fatal("label holder neighborhood is missing the entity node")
		}

		reader = f.labelIngester(t, "reader")
		paths := map[string]func(context.Context) error{
			"Neighborhood": func(ctx context.Context) error {
				_, err := client.Neighborhood(ctx, explorer.NeighborhoodRequest{
					NodeIDs: []shoal.ID{entity}, Depth: 1,
				})
				return err
			},
			"BoundedNeighborhood": func(ctx context.Context) error {
				_, err := client.BoundedNeighborhood(ctx, labelBoundedRequest(entity))
				return err
			},
			"MaterializeAnalytics": func(ctx context.Context) error {
				_, err := client.MaterializeAnalytics(ctx, labelBoundedRequest(entity), 64, 1<<20)
				return err
			},
			"Connect": func(ctx context.Context) error {
				id, err := client.ApplicationEdgeID(ctx,
					shoal.ID("entity-self-"+ctx.Value(labelProbeKey{}).(string)))
				if err != nil {
					return err
				}
				return client.Connect(ctx, graph.Edge{
					ID:   id,
					From: entity, To: entity, Type: "reader_connect", Weight: 1,
				})
			},
			"InteractionRecord": func(ctx context.Context) error {
				_, err := client.InteractionRecord(ctx, session.ID)
				return err
			},
			"LexiconScopeNodes": func(ctx context.Context) error {
				snapshot, err := f.base.Snapshot(context.Background())
				if err != nil {
					return err
				}
				scoped, err := client.LexiconScopeNodes(ctx, lexicon.Snapshot{
					ID: snapshot.ID, AsOf: snapshot.AsOf, Frontier: snapshot.Frontier,
				}, []graph.Node{entityNode})
				if err != nil {
					return err
				}
				if scoped.Len() == 0 {
					return shoal.NewError(shoal.ErrorNotFound, "entity not in scope")
				}
				return nil
			},
		}
		for name, path := range paths {
			denied := context.WithValue(reader, labelProbeKey{}, "reader")
			if err := path(denied); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Errorf("%s: source-only reader on a relabelled entity = %v, want not found", name, err)
			}
			allowed := context.WithValue(f.labelIngester(t, "holder", "secret"), labelProbeKey{}, "holder")
			if err := path(allowed); err != nil {
				t.Errorf("%s: label holder on the relabelled entity: %v", name, err)
			}
		}

		// The document node's derived edge to the entity is filtered with it.
		docGraph, err := client.Neighborhood(f.labelIngester(t, "holder", "secret"),
			explorer.NeighborhoodRequest{NodeIDs: []shoal.ID{public.Document.ID}, Depth: 2})
		if err != nil {
			t.Fatal(err)
		}
		if !hasNode(docGraph, entity) {
			t.Fatal("label holder does not reach the entity from its document")
		}
	})
}

type labelProbeKey struct{}
