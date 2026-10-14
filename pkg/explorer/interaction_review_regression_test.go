/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
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
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func reviewSession(t *testing.T, corpus *Explorer) (interaction.Session, shoal.ID) {
	t.Helper()
	receipt, err := corpus.Ingest(context.Background(), Source{
		URI: "file:///interaction-source.txt", MediaType: MediaTypeText,
		Content: "immutable source evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	document, err := corpus.Document(
		context.Background(), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	return interaction.Session{
		ID: "interaction.session_review", Operation: interaction.OperationRetrieval,
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		SeedNodeIDs: []shoal.ID{document.Root.Spans[0].ID},
	}, receipt.Document.ID
}

func TestReviewInteractionCannotReplaceSourceNode(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	session, sourceID := reviewSession(t, corpus)
	session.ID = sourceID
	if err := corpus.RecordInteraction(
		context.Background(), session,
	); err == nil {
		t.Fatalf(
			"source ID collision was committed; source node kind is now %q",
			corpus.graphNodes[sourceID].Kind,
		)
	}
}

func TestReviewInteractionSameIDRetryAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	corpus, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := reviewSession(t, corpus)
	if err := corpus.RecordInteraction(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := corpus.RecordInteraction(context.Background(), session); err != nil {
		t.Errorf("identical live retry failed: %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RecordInteraction(
		context.Background(), session,
	); err != nil {
		t.Errorf("identical retry after restart failed: %v", err)
	}
}

func TestReviewRetrievalSummaryDoesNotInventInference(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	session, _ := reviewSession(t, corpus)
	if err := corpus.RecordInteraction(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summary count = %d", len(summaries))
	}
	if summaries[0].InferenceID != "" {
		t.Fatalf(
			"retrieval summary advertises absent inference %q",
			summaries[0].InferenceID,
		)
	}
}
