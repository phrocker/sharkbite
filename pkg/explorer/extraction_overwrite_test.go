// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package explorer_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestExtractionRecordOverwriteSurvivesReopen pins issue #692 at the
// Explorer level. Re-extracting a document revision with the same ontology
// version rewrites its extraction record in place (one stable row, written
// with PutLatest), so every version of the record is the same full key. A
// reopen must load the LATEST extraction, whether the overwrites shared a
// session (one memtable / one flushed file) or spanned sessions (one file
// per session, since Close flushes).
func TestExtractionRecordOverwriteSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	version := skillsOntologyVersion(t)
	namespaces := []string{"alpha", "beta", "gamma"}
	for _, reopenBetween := range []bool{false, true} {
		reopenBetween := reopenBetween
		t.Run(fmt.Sprintf("reopenBetween=%v", reopenBetween), func(t *testing.T) {
			dir := t.TempDir()
			corpus, err := explorer.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			ingested, err := corpus.Ingest(ctx, explorer.Source{
				URI: "file:///SKILL.md", MediaType: explorer.MediaTypeMarkdown,
				Content: sampleSkillMarkdown,
			})
			if err != nil {
				t.Fatal(err)
			}
			var want []shoal.ID
			for _, namespace := range namespaces {
				if _, err := corpus.ExtractDocument(ctx, explorer.ExtractionRequest{
					DocumentID: ingested.Document.ID, RevisionID: ingested.Revision.ID,
					Version: version, EntityNamespace: namespace,
				}); err != nil {
					t.Fatal(err)
				}
				records, err := corpus.ExtractionRecords(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(records) != 1 {
					t.Fatalf("namespace %s: %d extraction records, want 1", namespace, len(records))
				}
				if fmt.Sprint(records[0].NodeIDs) == fmt.Sprint(want) {
					t.Fatalf("namespace %s did not change the extraction's nodes; test cannot tell versions apart", namespace)
				}
				want = records[0].NodeIDs
				if reopenBetween {
					if err := corpus.Close(); err != nil {
						t.Fatal(err)
					}
					if corpus, err = explorer.Open(dir); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := corpus.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := explorer.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			records, err := reopened.ExtractionRecords(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("after reopen: %d extraction records, want 1", len(records))
			}
			if fmt.Sprint(records[0].NodeIDs) != fmt.Sprint(want) {
				t.Fatalf("after reopen: extraction nodes = %v, want the latest (%s) %v",
					records[0].NodeIDs, namespaces[len(namespaces)-1], want)
			}
		})
	}
}
