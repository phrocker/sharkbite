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
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestWriteRecordMarksOnlyEngineWriteFailureIndeterminate(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	encodingErr := corpus.writeRecord(
		[]byte("encoding-error"), embeddedRecordEdge, make(chan int))
	if encodingErr == nil ||
		!shoal.IsErrorCode(encodingErr, shoal.ErrorInternal) ||
		IsIndeterminateCommit(encodingErr) {
		t.Fatalf("encoding error = %v", encodingErr)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	writeErr := corpus.writeRecord(
		edgeRecordRow("closed-engine-edge"),
		embeddedRecordEdge,
		persistedEdge{Edge: graph.Edge{
			ID: "closed-engine-edge", From: "from", To: "to",
			Type: "links", Weight: 1,
		}},
	)
	if writeErr == nil ||
		!shoal.IsErrorCode(writeErr, shoal.ErrorUnavailable) ||
		!IsIndeterminateCommit(writeErr) {
		t.Fatalf("engine write error = %v", writeErr)
	}
	if errors.Unwrap(writeErr) == nil {
		t.Fatalf("engine write marker lost cause: %v", writeErr)
	}
}

func TestPublicationSequenceDrivesCurrentSelectionAndPersists(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	firstSource := Source{
		URI: "file:///publication.txt", MediaType: MediaTypeText,
		Content: "oldertoken content",
	}
	first, err := corpus.Ingest(ctx, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := corpus.Ingest(ctx, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Disposition != IngestUnchanged ||
		corpus.lastPublicationSequence != 1 {
		t.Fatalf(
			"idempotent ingest = %+v, sequence = %d",
			unchanged, corpus.lastPublicationSequence,
		)
	}
	secondSource := Source{
		URI: "file:///publication.txt", MediaType: MediaTypeText,
		Content: "newesttoken content",
	}
	second, err := corpus.Ingest(ctx, secondSource)
	if err != nil {
		t.Fatal(err)
	}
	if corpus.lastPublicationSequence != 2 {
		t.Fatalf("publication sequence = %d", corpus.lastPublicationSequence)
	}

	corpus.mu.Lock()
	records := corpus.documents[first.Document.ID]
	records[first.Revision.ID].Revision.CreatedAt =
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	records[second.Revision.ID].Revision.CreatedAt =
		time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, revisionID := range []shoal.ID{first.Revision.ID, second.Revision.ID} {
		record := records[revisionID]
		if err := corpus.writeRecord(
			documentRecordRow(record.Document.ID, record.Revision.ID),
			embeddedRecordDocument,
			record,
		); err != nil {
			corpus.mu.Unlock()
			t.Fatal(err)
		}
	}
	corpus.mu.Unlock()

	secondView, err := corpus.Document(ctx, second.Document.ID, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondView.Root.Spans) != 1 {
		t.Fatalf("second revision spans = %+v", secondView.Root.Spans)
	}
	currentSpanID := secondView.Root.Spans[0].ID
	assertCurrentRevision(t, corpus, second.Revision.ID, "newesttoken")
	currentEdge := graph.Edge{
		ID: "current-sequence-edge", From: currentSpanID, To: currentSpanID,
		Type: "references", Weight: 1,
	}
	if err := corpus.Connect(ctx, currentEdge); err != nil {
		t.Fatalf("connect to current revision node: %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.lastPublicationSequence != 2 {
		t.Fatalf("restored publication sequence = %d", reopened.lastPublicationSequence)
	}
	assertCurrentRevision(t, reopened, second.Revision.ID, "newesttoken")
	neighborhood, err := reopened.Neighborhood(ctx, NeighborhoodRequest{
		NodeIDs: []shoal.ID{currentSpanID}, EdgeTypes: []string{"references"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) != 1 || neighborhood.Edges[0].ID != currentEdge.ID {
		t.Fatalf("reopened current graph = %+v", neighborhood)
	}
	unchanged, err = reopened.Ingest(ctx, secondSource)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Disposition != IngestUnchanged ||
		reopened.lastPublicationSequence != 2 {
		t.Fatalf(
			"reopened idempotent ingest = %+v, sequence = %d",
			unchanged, reopened.lastPublicationSequence,
		)
	}
	third, err := reopened.Ingest(ctx, Source{
		URI: "file:///publication.txt", MediaType: MediaTypeText,
		Content: "thirdtoken content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.documents[third.Document.ID][third.Revision.ID].PublicationSequence != 3 {
		t.Fatalf(
			"third publication sequence = %d",
			reopened.documents[third.Document.ID][third.Revision.ID].PublicationSequence,
		)
	}
}

func TestAmbiguousLegacyRevisionOrderFailsImplicitCurrent(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := parseSource(Source{
		URI: "file:///legacy.txt", MediaType: MediaTypeText, Content: "first legacy",
	}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseSource(Source{
		URI: "file:///legacy.txt", MediaType: MediaTypeText, Content: "second legacy",
	}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyDocument(t, corpus, persistedFromParsed(first))
	writeLegacyDocument(t, corpus, persistedFromParsed(second))
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.Document(
		ctx, first.document.ID, first.revision.ID,
	); err != nil {
		t.Fatalf("explicit legacy revision: %v", err)
	}
	for name, operation := range map[string]func() error{
		"documents": func() error {
			_, err := corpus.Documents(ctx)
			return err
		},
		"implicit document": func() error {
			_, err := corpus.Document(ctx, first.document.ID, "")
			return err
		},
		"retrieve": func() error {
			_, err := corpus.Retrieve(ctx, retrieval.Request{Text: "legacy"})
			return err
		},
		"current graph": func() error {
			_, err := corpus.Neighborhood(ctx, NeighborhoodRequest{
				NodeIDs: []shoal.ID{first.document.ID},
			})
			return err
		},
	} {
		if err := operation(); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

// TestReingestOfAmbiguousLegacyRevisionIsUnavailable pins #674's legacy
// rule: re-ingesting the exact content of a document whose revisions are all
// legacy cannot say whether that revision is current, so it reports the
// unavailable publication-order error rather than unchanged or historical,
// and writes nothing. A document with a single legacy revision is still
// unambiguous and reports unchanged.
func TestReingestOfAmbiguousLegacyRevisionIsUnavailable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	sources := []Source{
		{URI: "file:///legacy.txt", MediaType: MediaTypeText, Content: "first legacy"},
		{URI: "file:///legacy.txt", MediaType: MediaTypeText, Content: "second legacy"},
		{URI: "file:///single.txt", MediaType: MediaTypeText, Content: "only legacy"},
	}
	parsed := make([]parsedSource, len(sources))
	for index, source := range sources {
		parsed[index], err = parseSource(source,
			time.Date(2020+index, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		writeLegacyDocument(t, corpus, persistedFromParsed(parsed[index]))
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	for index, source := range sources[:2] {
		result, err := corpus.Ingest(ctx, source)
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("re-ingest of ambiguous legacy revision %d = %+v, %v, want unavailable",
				index, result, err)
		}
	}
	revisions := corpus.documents[parsed[0].document.ID]
	if len(revisions) != 2 {
		t.Fatalf("ambiguous legacy document has %d revisions, want 2", len(revisions))
	}
	for id, record := range revisions {
		if record.PublicationSequence != 0 {
			t.Fatalf("re-ingest published legacy revision %s at %d", id, record.PublicationSequence)
		}
	}
	single, err := corpus.Ingest(ctx, sources[2])
	if err != nil || single.Disposition != IngestUnchanged ||
		single.Revision.ID != parsed[2].revision.ID {
		t.Fatalf("re-ingest of a single legacy revision = %+v, %v, want unchanged", single, err)
	}
}

func TestSingleLegacyRevisionRemainsCurrent(t *testing.T) {
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseSource(Source{
		URI: "file:///single-legacy.txt", MediaType: MediaTypeText,
		Content: "single legacy revision",
	}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyDocument(t, corpus, persistedFromParsed(parsed))
	legacyEdge := graph.Edge{
		ID: "legacy-edge", From: parsed.document.ID, To: parsed.document.ID,
		Type: "legacy_link", Weight: 1,
	}
	writeLegacyEdge(t, corpus, legacyEdge)
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	assertCurrentRevision(t, corpus, parsed.revision.ID, "single")
	neighborhood, err := corpus.Neighborhood(context.Background(), NeighborhoodRequest{
		NodeIDs: []shoal.ID{parsed.document.ID}, EdgeTypes: []string{"legacy_link"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) != 1 || neighborhood.Edges[0].ID != legacyEdge.ID {
		t.Fatalf("legacy edge = %+v", neighborhood.Edges)
	}
}

func TestPublishedRevisionSupersedesSingleLegacyRevision(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := parseSource(Source{
		URI: "file:///legacy-upgrade.txt", MediaType: MediaTypeText,
		Content: "legacy content",
	}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyDocument(t, corpus, persistedFromParsed(legacy))
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	published, err := corpus.Ingest(ctx, Source{
		URI: "file:///legacy-upgrade.txt", MediaType: MediaTypeText,
		Content: "published content",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentRevision(t, corpus, published.Revision.ID, "published")
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	assertCurrentRevision(t, corpus, published.Revision.ID, "published")
}

func TestOpaqueEmbeddedValuesRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata := shoal.Metadata{
		string([]byte{'k', 0xff}): string([]byte{0, 0xfe, 'v'}),
	}
	first, err := corpus.Ingest(ctx, Source{
		URI: "file:///opaque-a.txt", MediaType: MediaTypeText,
		Content: "opaque source metadata", Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, Source{
		URI: "file:///opaque-b.txt", MediaType: MediaTypeText,
		Content: "opaque edge metadata",
	})
	if err != nil {
		t.Fatal(err)
	}
	properties := shoal.Metadata{
		string([]byte{'p', 0xff}): string([]byte{0xfd, 0, 'v'}),
	}
	edge := graph.Edge{
		ID:   shoal.ID(string([]byte{'e', '/', 0, 0xff})),
		From: first.Document.ID, To: second.Document.ID,
		Type: "opaque_link", Weight: 1, Properties: properties,
	}
	if err := corpus.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	err = corpus.Connect(ctx, graph.Edge{
		ID: "missing-endpoint", From: shoal.ID(string([]byte{0xff})),
		To: second.Document.ID, Type: "opaque_link", Weight: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("opaque missing endpoint error = %v", err)
	}
	invalidType := edge
	invalidType.ID = "invalid-type"
	invalidType.Type = string([]byte{0xff})
	if err := corpus.Connect(ctx, invalidType); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("invalid edge type error = %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	view, err := corpus.Document(ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view.Document.Metadata, metadata) ||
		!reflect.DeepEqual(view.Revision.Metadata, metadata) {
		t.Fatalf(
			"opaque document metadata = %#v, revision metadata = %#v",
			view.Document.Metadata, view.Revision.Metadata,
		)
	}
	if !reflect.DeepEqual(
		corpus.documents[first.Document.ID][first.Revision.ID].Source.Metadata,
		metadata,
	) {
		t.Fatalf(
			"opaque source metadata = %#v",
			corpus.documents[first.Document.ID][first.Revision.ID].Source.Metadata,
		)
	}
	neighborhood, err := corpus.Neighborhood(ctx, NeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Document.ID}, EdgeTypes: []string{"opaque_link"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) != 1 ||
		neighborhood.Edges[0].ID != edge.ID ||
		!reflect.DeepEqual(neighborhood.Edges[0].Properties, properties) {
		t.Fatalf("opaque edge round trip = %#v", neighborhood.Edges)
	}
}

func TestCorruptEmbeddedEnvelopeIsRejected(t *testing.T) {
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeEmbeddedRecord(embeddedRecordEdge, persistedEdge{Edge: graph.Edge{
		ID: "corrupt", From: "from", To: "to", Type: "links", Weight: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	writeRawExplorerRecord(t, corpus, edgeRecordRow("corrupt"), recordCQV2, encoded)
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataDir); !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("corrupt envelope error = %v", err)
	}
}

func TestCorruptLegacyJSONStringIsRejectedBeforeUnmarshal(t *testing.T) {
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	encoded := append([]byte(`{"Edge":{"ID":"`), 0xff)
	encoded = append(encoded, []byte(`","From":"from","To":"to","Type":"links"}}`)...)
	writeRawExplorerRecord(t, corpus, edgeRecordRow("legacy"), recordCQV1, encoded)
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dataDir); !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("corrupt legacy JSON error = %v", err)
	}
}

func assertCurrentRevision(
	t *testing.T, corpus *Explorer, revisionID shoal.ID, query string,
) {
	t.Helper()
	ctx := context.Background()
	documents, err := corpus.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Revision.ID != revisionID {
		t.Fatalf("current documents = %+v", documents)
	}
	view, err := corpus.Document(ctx, documents[0].Document.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision.ID != revisionID {
		t.Fatalf("current document revision = %q", view.Revision.ID)
	}
	response, err := corpus.Retrieve(ctx, retrieval.Request{Text: query, TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 ||
		response.Results[0].Evidence[0].Citation.RevisionID != revisionID {
		t.Fatalf("current retrieval = %+v", response.Results)
	}
}

func persistedFromParsed(parsed parsedSource) *persistedDocument {
	return &persistedDocument{
		Document: parsed.document,
		Revision: parsed.revision,
		Source:   parsed.source,
		Sections: parsed.sections,
		Spans:    parsed.spans,
		Nodes:    parsed.nodes,
		Edges:    parsed.edges,
	}
}

func writeLegacyDocument(
	t *testing.T, corpus *Explorer, record *persistedDocument,
) {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	writeRawExplorerRecord(
		t,
		corpus,
		documentRecordRow(record.Document.ID, record.Revision.ID),
		recordCQV1,
		encoded,
	)
}

func writeLegacyEdge(t *testing.T, corpus *Explorer, edge graph.Edge) {
	t.Helper()
	encoded, err := json.Marshal(persistedEdge{Edge: edge})
	if err != nil {
		t.Fatal(err)
	}
	writeRawExplorerRecord(
		t, corpus, edgeRecordRow(edge.ID), recordCQV1, encoded,
	)
}

func writeRawExplorerRecord(
	t *testing.T, corpus *Explorer, row []byte, qualifier string, value []byte,
) {
	t.Helper()
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		t.Fatal(err)
	}
	mutation.PutLatest([]byte(recordCF), []byte(qualifier), nil, value)
	if err := corpus.engine.Write(
		explorerTable, []*cclient.Mutation{mutation},
	); err != nil {
		t.Fatal(err)
	}
}
