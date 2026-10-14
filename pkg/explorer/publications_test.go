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

package explorer_test

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPublicationsReportRevisionsAndConnectedEdges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ingest := func(uri, content string) explorer.IngestResult {
		t.Helper()
		result, err := corpus.Ingest(ctx, explorer.Source{
			URI: uri, MediaType: explorer.MediaTypeText, Content: content,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := ingest("file:///publications/a.txt", "first")
	second := ingest("file:///publications/a.txt", "second")
	other := ingest("file:///publications/b.txt", "other")
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "connected", From: first.Document.ID, To: other.Document.ID,
		Type: "links", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	check := func(corpus *explorer.Explorer) {
		t.Helper()
		published, err := corpus.RevisionPublications(ctx,
			[]shoal.ID{first.Document.ID, "absent", first.Document.ID})
		if err != nil {
			t.Fatal(err)
		}
		revisions := published[first.Document.ID]
		if len(published) != 1 || len(revisions) != 2 {
			t.Fatalf("RevisionPublications = %v", published)
		}
		early, late := revisions[first.Revision.ID], revisions[second.Revision.ID]
		if early.Sequence == 0 || early.Sequence >= late.Sequence ||
			early.PublishedAt.IsZero() || late.PublishedAt.IsZero() {
			t.Fatalf("revisions are not ordered by publication: %+v", revisions)
		}
		connected, err := corpus.ConnectedEdges(ctx,
			[]shoal.ID{"connected", "never-connected"})
		if err != nil {
			t.Fatal(err)
		}
		if !connected["connected"] || connected["never-connected"] {
			t.Fatalf("ConnectedEdges = %v", connected)
		}
	}
	check(corpus)
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}
