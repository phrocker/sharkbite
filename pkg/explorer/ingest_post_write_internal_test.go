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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// poisonGraphRebuild makes the next graph rebuild that includes source fail
// with a conflict: a graph materialization claims the source document's node
// ID with different content.
func poisonGraphRebuild(t *testing.T, corpus *Explorer, source Source) {
	t.Helper()
	analyzed, err := AnalyzeSource(source)
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	defer corpus.mu.Unlock()
	if err := corpus.ensureGraphLocked(); err != nil {
		t.Fatal(err)
	}
	corpus.graphMaterializations["poison"] = &persistedGraphMaterialization{
		ID: "poison",
		Nodes: []graph.Node{{
			ID: shoal.ID(analyzed.Document.ID), Kind: "poison",
		}},
	}
}

// TestIngestRebuildFailureAfterTheWriteIsIndeterminate pins that once the
// document record is durably written, a graph rebuild failure is reported
// indeterminate with its own code: the revision is published (a retry finds
// it), so a definite error would misreport it as not written (#687).
func TestIngestRebuildFailureAfterTheWriteIsIndeterminate(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := Source{
		URI: "file:///post-write/rebuild.txt", MediaType: MediaTypeText,
		Content: "rebuild after the write\n",
	}
	poisonGraphRebuild(t, corpus, source)
	_, err = corpus.Ingest(ctx, source)
	if !IsIndeterminateCommit(err) || !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("rebuild failure after the write = %v, want an indeterminate conflict", err)
	}
	corpus.mu.RLock()
	_, published := corpus.documents[SourceDocumentID(source.URI)]
	corpus.mu.RUnlock()
	if !published {
		t.Fatal("the written revision is not published")
	}

	// A failure before the write stays definite.
	if _, err := corpus.Ingest(ctx, Source{
		URI: "file:///post-write/invalid.json", MediaType: "application/json", Content: "{}",
	}); err == nil || IsIndeterminateCommit(err) {
		t.Fatalf("pre-write failure = %v, want it definite", err)
	}
}
