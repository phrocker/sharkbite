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
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type rejectingPublicationAdapter struct {
	published         bool
	committed         bool
	committedRequests []RecordPublication
}

func (a *rejectingPublicationAdapter) PublishRecord(
	context.Context,
	RecordPublication,
) (RecordPublicationResult, error) {
	a.published = true
	return RecordPublicationResult{}, nil
}

func (a *rejectingPublicationAdapter) RecordCommitted(
	_ context.Context,
	request RecordPublication,
) (bool, error) {
	a.committedRequests = append(a.committedRequests, request)
	return a.committed, nil
}

func (*rejectingPublicationAdapter) RecordHead(
	context.Context,
	byte,
	[]byte,
) (*RecordPublicationHead, error) {
	return nil, nil
}

func (*rejectingPublicationAdapter) RecordAttempt(
	context.Context,
	RecordPublication,
) (*RecordPublicationAttempt, error) {
	return nil, nil
}

func (*rejectingPublicationAdapter) PendingPublications(
	context.Context,
) (bool, error) {
	return false, nil
}

func TestConfiguredPublicationRejectsOversizedDocumentWithoutDirectWrite(t *testing.T) {
	source := Source{
		URI: "file:///oversized.txt", MediaType: MediaTypeText,
		Content: strings.Repeat("x", 1<<20),
	}
	parsed, err := parseSource(source, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	record := &persistedDocument{
		Document: parsed.document, Revision: parsed.revision, Source: parsed.source,
		Sections: parsed.sections, Spans: parsed.spans, Nodes: parsed.nodes, Edges: parsed.edges,
		PublishedAt: time.Now().UTC(), PublicationSequence: 1,
	}
	adapter := &rejectingPublicationAdapter{}
	corpus := &Explorer{publication: adapter}
	err = corpus.writeDocumentRecord(
		context.Background(),
		documentRecordRow(record.Document.ID, record.Revision.ID),
		record,
		nil,
		nil,
	)
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("configured oversized write = %v", err)
	}
	if adapter.published {
		t.Fatal("oversized record reached the publication adapter")
	}

	unconfigured, err := Open(publicationTestDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	defer unconfigured.Close()
	if _, err := unconfigured.Ingest(context.Background(), source); err != nil {
		t.Fatalf("legacy unconfigured oversized ingest = %v", err)
	}
}

func TestLegacyDocumentRecordRequiresPublicationProof(t *testing.T) {
	source := Source{
		URI:       "file:///legacy.txt",
		MediaType: MediaTypeText,
		Content:   "legacy",
	}
	parsed, err := parseSource(source, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	record := persistedDocument{
		Document:    parsed.document,
		Revision:    parsed.revision,
		Source:      parsed.source,
		Sections:    parsed.sections,
		Spans:       parsed.spans,
		Nodes:       parsed.nodes,
		Edges:       parsed.edges,
		PublishedAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &rejectingPublicationAdapter{}
	corpus := &Explorer{
		publication: adapter,
		documents:   make(map[shoal.ID]map[shoal.ID]*persistedDocument),
	}
	row := documentRecordRow(record.Document.ID, record.Revision.ID)
	if err := corpus.loadDocumentRecord(
		row,
		[]byte(recordCQV1),
		encoded,
		make(map[documentRevisionKey]byte),
		make(map[uint64]documentRevisionKey),
	); err != nil {
		t.Fatal(err)
	}
	if len(adapter.committedRequests) != 1 ||
		!bytes.Equal(adapter.committedRequests[0].Qualifier, []byte(recordCQV1)) ||
		!bytes.Equal(adapter.committedRequests[0].Value, encoded) {
		t.Fatalf("legacy commit probes = %#v", adapter.committedRequests)
	}
	if len(corpus.documents) != 0 {
		t.Fatal("uncommitted legacy document was loaded")
	}
}

func TestLegacyDocumentCommitGatePrecedesJSONValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
		wantError bool
	}{
		{name: "uncommitted residue is ignored"},
		{name: "committed corruption fails", committed: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			eng, err := engine.Open(t.TempDir(), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			if err := eng.CreateTable(explorerTable, engine.TableOptions{}); err != nil {
				t.Fatal(err)
			}
			row := []byte(documentRow + "uncommitted/revision")
			mutation, err := cclient.NewMutation(row)
			if err != nil {
				t.Fatal(err)
			}
			invalid := []byte(`{"value":"\ud83d"}`)
			mutation.PutLatest(
				[]byte(recordCF),
				[]byte(recordCQV1),
				nil,
				invalid,
			)
			if err := eng.Write(
				explorerTable,
				[]*cclient.Mutation{mutation},
			); err != nil {
				t.Fatal(err)
			}
			adapter := &rejectingPublicationAdapter{
				committed: test.committed,
			}
			corpus, err := OpenWithEmbeddedEngine(
				eng,
				Options{},
				adapter,
			)
			if test.wantError {
				if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
					if corpus != nil {
						_ = corpus.Close()
					}
					t.Fatalf("committed corrupt v1 open = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("uncommitted corrupt v1 open = %v", err)
			}
			defer corpus.Close()
			documents, err := corpus.Documents(context.Background())
			if err != nil || len(documents) != 0 {
				t.Fatalf("uncommitted corrupt v1 documents = %#v, %v", documents, err)
			}
			if len(adapter.committedRequests) != 1 ||
				!bytes.Equal(
					adapter.committedRequests[0].Qualifier,
					[]byte(recordCQV1),
				) {
				t.Fatalf("v1 commitment probes = %#v", adapter.committedRequests)
			}
		})
	}
}

func publicationTestDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".publication-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
