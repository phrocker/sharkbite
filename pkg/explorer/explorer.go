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
	"crypto/rand"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/dirlock"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Explorer is a durable embedded implementation of the Explorer and Retriever
// contracts. Its API contains no engine, cell, or storage-format types.
type Explorer struct {
	mu                             sync.RWMutex
	engine                         *engine.Engine
	lock                           *dirlock.Lock
	documents                      map[shoal.ID]map[shoal.ID]*persistedDocument
	edges                          map[shoal.ID]persistedEdge
	interactions                   map[shoal.ID]*persistedInteraction
	interactionLiveRecords         map[shoal.ID]*persistedInteraction
	folds                          map[shoal.ID]*persistedFold
	foldLiveRecords                map[shoal.ID]*persistedFold
	interactionNodeIDs             map[shoal.ID]struct{}
	interactionEdgeIDs             map[shoal.ID]struct{}
	extractions                    map[shoal.ID]*persistedExtraction
	graphMaterializations          map[shoal.ID]*persistedGraphMaterialization
	ontologyProposals              map[shoal.ID]*persistedOntologyProposal
	ontologyMutationIndeterminate  bool
	graphNodes                     map[shoal.ID]graph.Node
	graphEdges                     map[shoal.ID]graph.Edge
	graphAssertions                map[shoal.ID]ontology.Assertion
	outgoing                       map[shoal.ID][]shoal.ID
	incoming                       map[shoal.ID][]shoal.ID
	graphErr                       error
	graphInitialized               bool
	embedder                       model.Embedder
	embedders                      map[string]model.Embedder
	maxEmbeddingSpaceFanout        int
	queryEmbeddingMu               sync.Mutex
	queryEmbeddingCache            map[embeddingQueryCacheKey]cachedQueryEmbedding
	queryEmbeddingOrder            []embeddingQueryCacheKey
	maxQueryEmbeddingCache         int
	embeddingQueryObserver         EmbeddingQueryObserver
	recallEvidence                 map[string]string
	embeddingSpace                 embeddingSpaceCache
	latentLinkProjection           LatentLinkAssertionProjection
	maxLatentAssertions            uint32
	vectorProbeMu                  sync.Mutex
	vectorAvailability             vectorAvailabilityCache
	snapshot                       Snapshot
	snapshotHistory                map[string]persistedSnapshot
	latestSnapshotID               shoal.ID
	latestSnapshotNodeDigests      map[shoal.ID]string
	latestSnapshotEdgeDigests      map[shoal.ID]string
	latestSnapshotAssertionDigests map[shoal.ID]string
	snapshotAnchor                 time.Time
	lastPublicationSequence        uint64
	changeHistoryFloor             uint64
	changeCursorKey                []byte
	interactionRecordWriter        func([]byte, byte, any) error
	indeterminateInteractionErr    error
	readOnly                       bool
	publication                    RecordPublicationAdapter
	ownsEngine                     bool
	closed                         bool
}

type persistedDocument struct {
	Document            document.Document
	Revision            document.Revision
	Source              Source
	Sections            []document.Section
	Spans               []document.Span
	Nodes               []graph.Node
	Edges               []graph.Edge
	Embeddings          *persistedEmbeddingSet
	PublicationSequence uint64 `json:"publication_sequence,omitempty"`
	PublishedAt         time.Time
}

type persistedEmbeddingSet struct {
	Provenance persistedEmbeddingProvenance
	Spans      []persistedSpanEmbedding
}

type persistedEmbeddingProvenance struct {
	Provider   string
	Model      string
	Identity   string
	Dimensions int
}

type persistedSpanEmbedding struct {
	SpanID     shoal.ID
	TextDigest string
	Range      document.SourceRange
	Vector     []float32
}

type persistedEdge struct {
	Edge        graph.Edge
	PublishedAt time.Time
}

// EmbeddingQueryEvent is one operator-visible multi-space query outcome.
// It contains stable space identities and counters only, never query text,
// vector bytes, provider credentials, or source content.
type EmbeddingQueryEvent struct {
	SpaceIdentities []string
	Attempted       []string
	Completed       []string
	// Participating contains the stable spaces represented by successful
	// scored outputs. It is empty when vector scoring produced no result.
	Participating  []string
	FanoutLimit    int
	CacheHits      int
	ProviderCalls  int
	Unavailable    []string
	FanoutExceeded bool
}

// EmbeddingQueryObserver receives one query event. Events contain no query
// text, vector bytes, provider credentials, or source content.
type EmbeddingQueryObserver func(EmbeddingQueryEvent)

type embeddingQueryObservation struct {
	observer EmbeddingQueryObserver
}

type embeddingQueryObservationKey struct{}

// WithEmbeddingQueryObserver binds one observer to ctx. The binding replaces
// any earlier request observer and is inherited only by calls using the
// returned context. Authorization wrappers use this seam after projecting
// source scope so raw space identities never cross the authorization boundary.
func WithEmbeddingQueryObserver(
	ctx context.Context,
	observer EmbeddingQueryObserver,
) context.Context {
	return context.WithValue(
		ctx,
		embeddingQueryObservationKey{},
		embeddingQueryObservation{observer: observer},
	)
}

// ReportEmbeddingQueryEvent delivers an event to the observer bound to ctx.
// Trusted vector implementations may use this to participate in request-local
// reporting without installing process-global state.
func ReportEmbeddingQueryEvent(ctx context.Context, event EmbeddingQueryEvent) {
	observation, ok := ctx.Value(embeddingQueryObservationKey{}).(embeddingQueryObservation)
	if !ok || observation.observer == nil {
		return
	}
	observation.observer(cloneEmbeddingQueryEvent(event))
}

func cloneEmbeddingQueryEvent(event EmbeddingQueryEvent) EmbeddingQueryEvent {
	event.SpaceIdentities = append([]string(nil), event.SpaceIdentities...)
	event.Attempted = append([]string(nil), event.Attempted...)
	event.Completed = append([]string(nil), event.Completed...)
	event.Participating = append([]string(nil), event.Participating...)
	event.Unavailable = append([]string(nil), event.Unavailable...)
	return event
}

// Options configures optional embedded Explorer features.
type Options struct {
	// Embedder enables vector indexing and retrieval. It must also implement
	// model.EmbeddingSpaceIdentityProvider so persisted provenance can detect
	// incompatible vector spaces without storing credentials. A nil Embedder
	// keeps ingestion and non-vector retrieval unchanged and advertises vector
	// as unavailable.
	Embedder model.Embedder

	// EmbeddingProviders are additional embedders that can serve historical
	// embedding spaces during a mixed-space migration.
	EmbeddingProviders []model.Embedder

	// MaxEmbeddingSpaceFanout bounds provider calls for one vector query. Zero
	// defaults to eight distinct spaces.
	MaxEmbeddingSpaceFanout int

	// MaxEmbeddingQueryCacheEntries bounds cached query vectors across every
	// embedding space. Zero defaults to 256 entries.
	MaxEmbeddingQueryCacheEntries int

	// EmbeddingQueryObserver receives one bounded fan-out summary per vector
	// retrieval or vector-score request. It must be safe for concurrent use.
	EmbeddingQueryObserver EmbeddingQueryObserver

	// RecallEvidence records benchmark evidence per embedding-space identity.
	RecallEvidence map[string]string

	// LatentLinkProjection configures how latent-edge iterator link cells are
	// presented as derived ontology assertions. A zero value uses Shoal's
	// default latent-link ontology and projection provenance.
	LatentLinkProjection LatentLinkAssertionProjection

	// MaxLatentDerivedAssertionsPerGraphRead bounds the latent assertions one
	// graph read may project. Zero uses DefaultMaxLatentDerivedAssertionsPerGraphRead.
	MaxLatentDerivedAssertionsPerGraphRead uint32

	// ReadOnly opens the corpus for reading only. A read-only corpus refuses
	// every mutation, including interaction capture, so it cannot serve an
	// inference: capture is part of serving one. Callers that need to serve
	// inference must open the corpus writable.
	ReadOnly bool
}

// Open opens or creates a local Explorer corpus rooted at dir.
func Open(dir string) (*Explorer, error) {
	return OpenWithOptions(dir, Options{})
}

// OpenWithOptions opens or creates a local Explorer corpus rooted at dir with
// explicitly configured optional features.
func OpenWithOptions(dir string, options Options) (*Explorer, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "data directory is required")
	}
	lock, err := dirlock.Acquire(dir, ".shoal-explorer-runtime.lock")
	if err != nil {
		return nil, shoal.WrapError(shoal.ErrorUnavailable, "acquire explorer storage", err)
	}
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		_ = lock.Close()
		return nil, shoal.WrapError(shoal.ErrorUnavailable, "open explorer storage", err)
	}
	explorer, err := openWithEngine(eng, options, nil)
	if err != nil {
		_ = eng.Close()
		_ = lock.Close()
		return nil, err
	}
	explorer.ownsEngine = true
	explorer.lock = lock
	return explorer, nil
}

// OpenWithEmbeddedEngine opens Explorer on an engine whose lifetime is owned
// by the caller. It is intended for the in-process transaction runtime, which
// must share one engine handle with both coordination and physical writes.
func OpenWithEmbeddedEngine(
	eng *engine.Engine,
	options Options,
	publication RecordPublicationAdapter,
) (*Explorer, error) {
	if eng == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "embedded engine is required")
	}
	return openWithEngine(eng, options, publication)
}

func openWithEngine(
	eng *engine.Engine,
	options Options,
	publication RecordPublicationAdapter,
) (*Explorer, error) {
	embedders, err := embeddingProviderMap(options)
	if err != nil {
		return nil, err
	}
	maxFanout := options.MaxEmbeddingSpaceFanout
	if maxFanout <= 0 {
		maxFanout = 8
	}
	maxQueryCache := options.MaxEmbeddingQueryCacheEntries
	if maxQueryCache <= 0 {
		maxQueryCache = 256
	}
	latentProjection, err := latentLinkProjectionForOptions(
		options.LatentLinkProjection)
	if err != nil {
		return nil, err
	}
	maxLatentAssertions := options.MaxLatentDerivedAssertionsPerGraphRead
	if maxLatentAssertions == 0 {
		maxLatentAssertions = DefaultMaxLatentDerivedAssertionsPerGraphRead
	}
	found := false
	for _, table := range eng.TableNames() {
		if table == explorerTable {
			found = true
			break
		}
	}
	if !found {
		if err := eng.CreateTable(explorerTable, engine.TableOptions{}); err != nil {
			return nil, shoal.WrapError(shoal.ErrorInternal, "create explorer table", err)
		}
	}
	explorer := &Explorer{
		engine:                         eng,
		documents:                      make(map[shoal.ID]map[shoal.ID]*persistedDocument),
		edges:                          make(map[shoal.ID]persistedEdge),
		interactions:                   make(map[shoal.ID]*persistedInteraction),
		interactionLiveRecords:         make(map[shoal.ID]*persistedInteraction),
		folds:                          make(map[shoal.ID]*persistedFold),
		foldLiveRecords:                make(map[shoal.ID]*persistedFold),
		interactionNodeIDs:             make(map[shoal.ID]struct{}),
		interactionEdgeIDs:             make(map[shoal.ID]struct{}),
		extractions:                    make(map[shoal.ID]*persistedExtraction),
		graphMaterializations:          make(map[shoal.ID]*persistedGraphMaterialization),
		ontologyProposals:              make(map[shoal.ID]*persistedOntologyProposal),
		embedder:                       options.Embedder,
		embedders:                      embedders,
		maxEmbeddingSpaceFanout:        maxFanout,
		queryEmbeddingCache:            make(map[embeddingQueryCacheKey]cachedQueryEmbedding),
		maxQueryEmbeddingCache:         maxQueryCache,
		embeddingQueryObserver:         options.EmbeddingQueryObserver,
		recallEvidence:                 cloneStringMap(options.RecallEvidence),
		latentLinkProjection:           latentProjection,
		maxLatentAssertions:            maxLatentAssertions,
		snapshotHistory:                make(map[string]persistedSnapshot),
		latestSnapshotNodeDigests:      make(map[shoal.ID]string),
		latestSnapshotEdgeDigests:      make(map[shoal.ID]string),
		latestSnapshotAssertionDigests: make(map[shoal.ID]string),
		readOnly:                       options.ReadOnly,
		publication:                    publication,
	}
	if err := explorer.load(); err != nil {
		return nil, err
	}
	explorer.interactionLiveRecords = nil
	explorer.foldLiveRecords = nil
	if explorer.snapshotAnchor.IsZero() {
		explorer.snapshotAnchor = time.Now().UTC()
		if !explorer.readOnly {
			if err := explorer.writeRecord(
				snapshotAnchorRow, embeddedRecordSnapshotAnchor,
				persistedSnapshotAnchor{CreatedAt: explorer.snapshotAnchor},
			); err != nil {
				return nil, err
			}
		}
	}
	if len(explorer.changeCursorKey) == 0 {
		// The change-feed cursor seal key is generated once, on first open, and
		// persisted as corpus state so cursors stay valid across restart. Key
		// rotation is deliberately unimplemented: a single durable key is
		// sufficient for a single-instance workspace, and rotating would require
		// versioned cursors (a key version tag in the sealed payload so an old
		// cursor can still be opened with the retired key). A wiped or recreated
		// corpus mints a fresh key, which is the intended behaviour -- stale
		// cursors then fail authentication and the client is told to resynchronise.
		key := make([]byte, changeCursorKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, shoal.WrapError(
				shoal.ErrorInternal, "generate change cursor key", err)
		}
		explorer.changeCursorKey = key
		if !explorer.readOnly {
			if err := explorer.writeRecord(
				cursorKeyRow, embeddedRecordCursorKey,
				persistedCursorKey{Key: key},
			); err != nil {
				return nil, err
			}
		}
	}
	return explorer, nil
}

// Close flushes and closes the local corpus.
func (e *Explorer) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	var engineErr error
	if e.ownsEngine {
		engineErr = e.engine.Close()
	}
	lockErr := e.lock.Close()
	if err := errors.Join(engineErr, lockErr); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "close explorer storage", err)
	}
	return nil
}

// Ingest parses and durably stores one immutable Markdown, plain-text, or
// source-code-as-text revision.
func (e *Explorer) Ingest(ctx context.Context, source Source) (IngestResult, error) {
	return e.ingest(ctx, source, time.Time{})
}

// ValidateSource checks whether a source can be parsed without publishing it.
func ValidateSource(source Source) error {
	_, err := parseSource(source, time.Now())
	return err
}

// AnalyzeSource checks whether a source can be parsed and returns its stable
// document identity and content counts without publishing it.
func AnalyzeSource(source Source) (IngestResult, error) {
	parsed, err := parseSource(source, time.Now())
	if err != nil {
		return IngestResult{}, err
	}
	return IngestResult{
		Document:     cloneDocument(parsed.document),
		Revision:     cloneRevision(parsed.revision),
		SectionCount: len(parsed.sections),
		SpanCount:    len(parsed.spans),
	}, nil
}

// IngestWithOptions parses and durably stores one immutable revision with
// additive descriptive options. CreatedAt never determines the current
// revision; durable publication order does.
func (e *Explorer) IngestWithOptions(
	ctx context.Context, source Source, options IngestOptions,
) (IngestResult, error) {
	return e.ingest(ctx, source, options.CreatedAt)
}

func (e *Explorer) ingest(
	ctx context.Context, source Source, createdAt time.Time,
) (IngestResult, error) {
	if err := contextError(ctx); err != nil {
		return IngestResult{}, err
	}
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	parsed, err := parseSource(source, createdAt)
	if err != nil {
		return IngestResult{}, err
	}
	e.mu.RLock()
	if err := e.requireOpen(); err != nil {
		e.mu.RUnlock()
		return IngestResult{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		e.mu.RUnlock()
		return IngestResult{}, err
	}
	if revisions := e.documents[parsed.document.ID]; revisions != nil {
		if existing := revisions[parsed.revision.ID]; existing != nil {
			result, err := existingRevisionResult(revisions, existing)
			e.mu.RUnlock()
			return result, err
		}
	}
	e.mu.RUnlock()
	row := documentRecordRow(parsed.document.ID, parsed.revision.ID)
	record, publicationHead, attemptedValue, attempted, err :=
		e.documentRecordAttempt(ctx, row)
	if err != nil {
		return IngestResult{}, err
	}
	if attempted {
		if record.Document.ID != parsed.document.ID ||
			record.Revision.ID != parsed.revision.ID ||
			record.Source.URI != parsed.source.URI ||
			record.Source.MediaType != parsed.source.MediaType ||
			record.Source.Content != parsed.source.Content {
			return IngestResult{}, shoal.NewError(
				shoal.ErrorInternal,
				"transactional document attempt disagrees with the source",
			)
		}
	} else {
		embeddings, err := e.embedParsedSpans(ctx, parsed.spans)
		if err != nil {
			return IngestResult{}, err
		}
		record = &persistedDocument{
			Document:   parsed.document,
			Revision:   parsed.revision,
			Source:     parsed.source,
			Sections:   parsed.sections,
			Spans:      parsed.spans,
			Nodes:      parsed.nodes,
			Edges:      parsed.edges,
			Embeddings: embeddings,
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return IngestResult{}, err
	}
	if revisions := e.documents[record.Document.ID]; revisions != nil {
		if existing := revisions[record.Revision.ID]; existing != nil {
			return existingRevisionResult(revisions, existing)
		}
	}
	if !attempted {
		pending, err := e.publicationPending(ctx)
		if err != nil {
			return IngestResult{}, err
		}
		if pending {
			return IngestResult{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"a previous transactional publication requires recovery",
			)
		}
		publicationHead, err = e.documentRecordHead(ctx, []byte(parsed.document.ID))
		if err != nil {
			return IngestResult{}, err
		}
	}
	if record.Embeddings != nil {
		if err := e.ensureEmbeddingSpaceCompatibleLocked(
			record.Embeddings.Provenance,
		); err != nil {
			return IngestResult{}, err
		}
		e.embeddingSpace = embeddingSpaceCache{
			provenance: record.Embeddings.Provenance,
			found:      true,
		}
	}
	if attempted {
		if record.PublicationSequence == 0 || record.PublishedAt.IsZero() {
			return IngestResult{}, shoal.NewError(
				shoal.ErrorInternal,
				"transactional document attempt has no publication coordinate",
			)
		}
		if record.PublicationSequence > e.lastPublicationSequence {
			e.lastPublicationSequence = record.PublicationSequence
		}
	} else {
		if e.lastPublicationSequence == math.MaxUint64 {
			return IngestResult{}, shoal.NewError(
				shoal.ErrorUnavailable, "embedded publication sequence is exhausted")
		}
		record.PublishedAt = time.Now().UTC()
		// A write error can occur after the WAL append committed, so attempted
		// publication sequences must never be reused.
		e.lastPublicationSequence++
		record.PublicationSequence = e.lastPublicationSequence
	}
	if err := e.writeDocumentRecord(
		ctx, row, record, publicationHead, attemptedValue,
	); err != nil {
		return IngestResult{}, err
	}
	if e.documents[record.Document.ID] == nil {
		e.documents[record.Document.ID] = make(map[shoal.ID]*persistedDocument)
	}
	e.documents[record.Document.ID][record.Revision.ID] = record
	e.invalidateVectorAvailabilityLocked()
	if e.graphInitialized {
		if err := e.rebuildCurrentGraphLocked(); err != nil {
			return IngestResult{}, err
		}
	}
	return ingestResult(record, IngestApplied), nil
}

// Documents lists the newest revision of every document.
func (e *Explorer) Documents(ctx context.Context) ([]DocumentSummary, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	summaries := make([]DocumentSummary, 0, len(e.documents))
	for _, revisions := range e.documents {
		record, err := latestRevision(revisions)
		if err != nil {
			return nil, err
		}
		if record == nil {
			continue
		}
		summaries = append(summaries, DocumentSummary{
			Document:        cloneDocument(record.Document),
			Revision:        cloneRevision(record.Revision),
			SourceURI:       record.Source.URI,
			SourceMediaType: record.Source.MediaType,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Document.Title == summaries[j].Document.Title {
			return shoal.CompareID(
				summaries[i].Document.ID, summaries[j].Document.ID) < 0
		}
		return summaries[i].Document.Title < summaries[j].Document.Title
	})
	return summaries, nil
}

// Document returns a navigable document revision. An empty revisionID selects
// the newest revision.
func (e *Explorer) Document(
	ctx context.Context, documentID, revisionID shoal.ID,
) (DocumentView, error) {
	if err := contextError(ctx); err != nil {
		return DocumentView{}, err
	}
	if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
		return DocumentView{}, err
	}
	if err := shoal.ValidateOptionalID("revision ID", revisionID); err != nil {
		return DocumentView{}, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return DocumentView{}, err
	}
	revisions := e.documents[documentID]
	var (
		record *persistedDocument
		err    error
	)
	if revisionID == "" {
		if len(revisions) == 0 {
			return DocumentView{}, shoal.NewError(
				shoal.ErrorNotFound, "document revision not found")
		}
		record, err = latestRevision(revisions)
		if err != nil {
			return DocumentView{}, err
		}
	} else {
		record = revisions[revisionID]
	}
	if record == nil {
		return DocumentView{}, shoal.NewError(shoal.ErrorNotFound, "document revision not found")
	}
	root, err := buildSectionView(record)
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{
		Document:        cloneDocument(record.Document),
		Revision:        cloneRevision(record.Revision),
		SourceURI:       record.Source.URI,
		SourceMediaType: record.Source.MediaType,
		Root:            root,
	}, nil
}

// Connect adds an application-defined edge between existing document,
// section, or span nodes.
func (e *Explorer) Connect(ctx context.Context, edge graph.Edge) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validatePersistedEdge(edge); err != nil {
		return err
	}
	if interaction.IsInteractionID(edge.ID) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot use the reserved interaction edge ID namespace",
		)
	}
	if interaction.IsInteractionEdgeType(edge.Type) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot create edges in the reserved interaction namespace",
		)
	}
	if graph.IsProvenanceEdgeType(edge.Type) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot create edges in the reserved provenance namespace",
		)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return err
	}
	if err := e.requireWritableLocked(); err != nil {
		return err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return err
	}
	if node, ok := e.graphNodes[edge.From]; !ok {
		return shoal.NewError(shoal.ErrorNotFound, "edge source node not found")
	} else if interaction.IsInteractionKind(node.Kind) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot attach edges to interaction nodes",
		)
	} else if graph.IsProvenanceKind(node.Kind) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot attach edges to provenance nodes",
		)
	}
	if node, ok := e.graphNodes[edge.To]; !ok {
		return shoal.NewError(shoal.ErrorNotFound, "edge target node not found")
	} else if interaction.IsInteractionKind(node.Kind) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot attach edges to interaction nodes",
		)
	} else if graph.IsProvenanceKind(node.Kind) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"applications cannot attach edges to provenance nodes",
		)
	}
	if existing, ok := e.graphEdges[edge.ID]; ok {
		if edgesEqual(existing, edge) {
			return nil
		}
		return shoal.NewError(shoal.ErrorConflict, "edge ID already has different content")
	}
	record := persistedEdge{Edge: cloneEdge(edge), PublishedAt: time.Now().UTC()}
	if err := e.writeRecord(edgeRecordRow(edge.ID), embeddedRecordEdge, record); err != nil {
		return err
	}
	e.edges[edge.ID] = record
	e.graphEdges[edge.ID] = cloneEdge(edge)
	e.outgoing[edge.From] = append(e.outgoing[edge.From], edge.ID)
	sort.Slice(e.outgoing[edge.From], func(i, j int) bool {
		return shoal.CompareID(e.outgoing[edge.From][i], e.outgoing[edge.From][j]) < 0
	})
	e.incoming[edge.To] = append(e.incoming[edge.To], edge.ID)
	sort.Slice(e.incoming[edge.To], func(i, j int) bool {
		return shoal.CompareID(e.incoming[edge.To][i], e.incoming[edge.To][j]) < 0
	})
	e.refreshSnapshotLocked()
	return nil
}

// Neighborhood expands both incoming and outgoing graph relationships.
func (e *Explorer) Neighborhood(
	ctx context.Context, request NeighborhoodRequest,
) (Neighborhood, error) {
	if err := contextError(ctx); err != nil {
		return Neighborhood{}, err
	}
	normalized, err := request.Normalize()
	if err != nil {
		return Neighborhood{}, err
	}
	request = normalized
	typeFilter := make(map[string]struct{}, len(request.EdgeTypes))
	for _, edgeType := range request.EdgeTypes {
		typeFilter[edgeType] = struct{}{}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return Neighborhood{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return Neighborhood{}, err
	}
	if e.graphErr != nil {
		return Neighborhood{}, e.graphErr
	}
	nodes, edges := e.graphNodes, e.graphEdges
	seen := make(map[shoal.ID]struct{})
	frontier := make(map[shoal.ID]struct{})
	for _, id := range request.NodeIDs {
		if _, ok := nodes[id]; !ok {
			return Neighborhood{}, shoal.NewError(shoal.ErrorNotFound, "graph node not found")
		}
		seen[id] = struct{}{}
		frontier[id] = struct{}{}
	}
	selectedEdges := make(map[shoal.ID]graph.Edge)
	explicit := idSet(request.NodeIDs)
	for depth := uint32(0); depth < request.Depth && len(frontier) > 0; depth++ {
		next := make(map[shoal.ID]struct{})
		for _, edge := range edges {
			if len(typeFilter) > 0 {
				if _, ok := typeFilter[edge.Type]; !ok {
					continue
				}
			}
			_, from := frontier[edge.From]
			_, to := frontier[edge.To]
			if !from && !to {
				continue
			}
			if excludedInteractionEdge(nodes, explicit, edge) {
				continue
			}
			selectedEdges[edge.ID] = edge
			for _, id := range []shoal.ID{edge.From, edge.To} {
				if _, exists := seen[id]; !exists {
					seen[id] = struct{}{}
					next[id] = struct{}{}
				}
			}
		}
		frontier = next
	}
	result := Neighborhood{
		Nodes:      make([]graph.Node, 0, len(seen)),
		Edges:      make([]graph.Edge, 0, len(selectedEdges)),
		Assertions: make([]ontology.Assertion, 0, len(selectedEdges)),
	}
	for id := range seen {
		result.Nodes = append(result.Nodes, cloneNode(nodes[id]))
	}
	for _, edge := range selectedEdges {
		result.Edges = append(result.Edges, cloneEdge(edge))
	}
	result.Assertions = e.assertionsForEdgesLocked(selectedEdges)
	sort.Slice(result.Nodes, func(i, j int) bool {
		return shoal.CompareID(result.Nodes[i].ID, result.Nodes[j].ID) < 0
	})
	sort.Slice(result.Edges, func(i, j int) bool {
		return shoal.CompareID(result.Edges[i].ID, result.Edges[j].ID) < 0
	})
	return result, nil
}

func (e *Explorer) requireOpen() error {
	if e.closed {
		return shoal.NewError(shoal.ErrorUnavailable, "explorer is closed")
	}
	if e.indeterminateInteractionErr != nil {
		return e.indeterminateInteractionErr
	}
	return nil
}

// excludedInteractionEdge reports whether graph expansion must not cross an
// edge because doing so would surface an interaction node that the caller did
// not explicitly seed. This is the default-exclusion property: a model that
// expands the neighborhood of a source span must never discover, and so must
// never be able to cite, an earlier session's own output.
func excludedInteractionEdge(
	nodes map[shoal.ID]graph.Node,
	explicit map[shoal.ID]struct{},
	edge graph.Edge,
) bool {
	for _, id := range []shoal.ID{edge.From, edge.To} {
		node, ok := nodes[id]
		if !ok {
			continue
		}
		if !interaction.IsInteractionKind(node.Kind) {
			continue
		}
		if _, seeded := explicit[id]; !seeded {
			return true
		}
	}
	return false
}

func (e *Explorer) computeCurrentGraph() (
	map[shoal.ID]graph.Node,
	map[shoal.ID]graph.Edge,
	map[shoal.ID]ontology.Assertion,
	error,
) {
	nodes := make(map[shoal.ID]graph.Node)
	edges := make(map[shoal.ID]graph.Edge)
	assertions := make(map[shoal.ID]ontology.Assertion)
	for _, revisions := range e.documents {
		record, err := latestRevision(revisions)
		if err != nil {
			return nil, nil, nil, err
		}
		if record == nil {
			continue
		}
		for _, node := range record.Nodes {
			nodes[node.ID] = node
		}
		for _, edge := range record.Edges {
			edges[edge.ID] = edge
		}
	}
	for id, record := range e.edges {
		edge := record.Edge
		if _, from := nodes[edge.From]; !from {
			continue
		}
		if _, to := nodes[edge.To]; !to {
			continue
		}
		edges[id] = edge
	}
	extractionIDs := make([]shoal.ID, 0, len(e.extractions))
	for id := range e.extractions {
		extractionIDs = append(extractionIDs, id)
	}
	sort.Slice(extractionIDs, func(i, j int) bool {
		return shoal.CompareID(extractionIDs[i], extractionIDs[j]) < 0
	})
	for _, id := range extractionIDs {
		record := e.extractions[id]
		if record == nil {
			continue
		}
		if _, ok := nodes[record.DocumentID]; !ok {
			continue
		}
		for _, node := range record.Nodes {
			if existing, exists := nodes[node.ID]; exists && !nodesEqual(existing, node) {
				continue
			}
			nodes[node.ID] = node
		}
		for _, edge := range record.Edges {
			if _, from := nodes[edge.From]; !from {
				continue
			}
			if _, to := nodes[edge.To]; !to {
				continue
			}
			if existing, exists := edges[edge.ID]; exists && !edgesEqual(existing, edge) {
				return nil, nil, nil, shoal.NewError(
					shoal.ErrorConflict,
					"extraction edge ID already has different content",
				)
			}
			edges[edge.ID] = edge
		}
		for _, persisted := range record.Assertions {
			assertion, err := restoreAssertion(persisted)
			if err != nil {
				return nil, nil, nil, err
			}
			if _, ok := edges[persisted.EdgeID]; ok {
				assertions[persisted.EdgeID] = assertion
			}
		}
	}
	materializationIDs := make(
		[]shoal.ID, 0, len(e.graphMaterializations))
	for id := range e.graphMaterializations {
		materializationIDs = append(materializationIDs, id)
	}
	sort.Slice(materializationIDs, func(i, j int) bool {
		return shoal.CompareID(
			materializationIDs[i], materializationIDs[j]) < 0
	})
	for _, id := range materializationIDs {
		record := e.graphMaterializations[id]
		if record == nil {
			continue
		}
		for _, node := range record.Nodes {
			if existing, exists := nodes[node.ID]; exists &&
				!nodesEqual(existing, node) {
				return nil, nil, nil, shoal.NewError(
					shoal.ErrorConflict,
					"materialized graph node ID already has different content",
				)
			}
			nodes[node.ID] = cloneNode(node)
		}
		for _, edge := range record.Edges {
			if _, from := nodes[edge.From]; !from {
				return nil, nil, nil, shoal.NewError(
					shoal.ErrorInternal,
					"materialized graph edge source is missing",
				)
			}
			if _, to := nodes[edge.To]; !to {
				return nil, nil, nil, shoal.NewError(
					shoal.ErrorInternal,
					"materialized graph edge target is missing",
				)
			}
			if existing, exists := edges[edge.ID]; exists &&
				!edgesEqual(existing, edge) {
				return nil, nil, nil, shoal.NewError(
					shoal.ErrorConflict,
					"materialized graph edge ID already has different content",
				)
			}
			edges[edge.ID] = cloneEdge(edge)
		}
	}
	// Interaction nodes share the corpus graph but are excluded from
	// retrieval and from expansion that did not explicitly seed them. An
	// interaction edge whose source endpoint disappeared (a superseded
	// revision) is dropped so the graph stays connected and valid.
	for _, record := range e.interactions {
		for _, node := range record.Nodes {
			nodes[node.ID] = node
		}
	}
	// Fold summaries are derived content in the same reserved namespace, so
	// they inherit every default-exclusion rule sessions already have.
	for _, record := range e.folds {
		for _, node := range record.Nodes {
			nodes[node.ID] = node
		}
	}
	latentAssertions, err := e.latentLinkAssertionsLocked()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, assertion := range latentAssertions {
		edge, ok, err := latentAssertionGraphEdge(assertion)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ok {
			continue
		}
		if _, from := nodes[edge.From]; !from {
			continue
		}
		if _, to := nodes[edge.To]; !to {
			continue
		}
		if existing, exists := edges[edge.ID]; exists && !edgesEqual(existing, edge) {
			return nil, nil, nil, shoal.NewError(
				shoal.ErrorConflict,
				"derived assertion edge ID already has different content",
			)
		}
		edges[edge.ID] = edge
		assertions[edge.ID] = assertion
		producer, assertionNode, derivationEdge, ok, err :=
			ProducerGraphElementsForAssertion(assertion)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ok {
			continue
		}
		// Load-bearing: TestLatentLinkProducerProvenanceReachableFromBoundedGraph
		// pins that live latent reads materialize producer nodes, assertion
		// nodes, and derivation edges in the cached graph.
		if err := putDerivedGraphNode(nodes, producer); err != nil {
			return nil, nil, nil, err
		}
		if err := putDerivedGraphNode(nodes, assertionNode); err != nil {
			return nil, nil, nil, err
		}
		if existing, exists := edges[derivationEdge.ID]; exists &&
			!edgesEqual(existing, derivationEdge) {
			return nil, nil, nil, shoal.NewError(
				shoal.ErrorConflict,
				"producer derivation edge ID already has different content",
			)
		}
		edges[derivationEdge.ID] = derivationEdge
	}
	for _, record := range e.interactions {
		for _, edge := range record.Edges {
			if _, from := nodes[edge.From]; !from {
				continue
			}
			if _, to := nodes[edge.To]; !to {
				continue
			}
			edges[edge.ID] = edge
		}
	}
	for _, record := range e.folds {
		for _, edge := range record.Edges {
			if _, from := nodes[edge.From]; !from {
				continue
			}
			if _, to := nodes[edge.To]; !to {
				continue
			}
			edges[edge.ID] = edge
		}
	}
	return nodes, edges, assertions, nil
}

func (e *Explorer) ensureGraphLocked() error {
	if e.graphInitialized {
		return e.graphErr
	}
	return e.rebuildCurrentGraphLocked()
}

func (e *Explorer) rebuildCurrentGraphLocked() error {
	nodes, edges, assertions, err := e.computeCurrentGraph()
	if err != nil {
		e.graphNodes = make(map[shoal.ID]graph.Node)
		e.graphEdges = make(map[shoal.ID]graph.Edge)
		e.graphAssertions = make(map[shoal.ID]ontology.Assertion)
		e.outgoing = make(map[shoal.ID][]shoal.ID)
		e.incoming = make(map[shoal.ID][]shoal.ID)
		e.graphErr = err
		e.graphInitialized = true
		e.refreshSnapshotLocked()
		return err
	}
	outgoing := make(map[shoal.ID][]shoal.ID, len(nodes))
	incoming := make(map[shoal.ID][]shoal.ID, len(nodes))
	for id, edge := range edges {
		outgoing[edge.From] = append(outgoing[edge.From], id)
		incoming[edge.To] = append(incoming[edge.To], id)
	}
	for id := range outgoing {
		sort.Slice(outgoing[id], func(i, j int) bool {
			return shoal.CompareID(outgoing[id][i], outgoing[id][j]) < 0
		})
	}
	for id := range incoming {
		sort.Slice(incoming[id], func(i, j int) bool {
			return shoal.CompareID(incoming[id][i], incoming[id][j]) < 0
		})
	}
	e.graphNodes, e.graphEdges, e.graphAssertions = nodes, edges, assertions
	e.outgoing, e.incoming = outgoing, incoming
	e.graphErr = nil
	e.graphInitialized = true
	e.refreshSnapshotLocked()
	return nil
}

func latestRevision(
	revisions map[shoal.ID]*persistedDocument,
) (*persistedDocument, error) {
	if len(revisions) == 1 {
		for _, record := range revisions {
			return record, nil
		}
	}
	var latest *persistedDocument
	sequences := make(map[uint64]struct{}, len(revisions))
	for _, record := range revisions {
		if record.PublicationSequence == 0 {
			continue
		}
		if _, duplicate := sequences[record.PublicationSequence]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInternal, "stored publication sequences are not unique")
		}
		sequences[record.PublicationSequence] = struct{}{}
		if latest == nil ||
			record.PublicationSequence > latest.PublicationSequence {
			latest = record
		}
	}
	if latest == nil {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"embedded publication order is unavailable for legacy revisions",
		)
	}
	return latest, nil
}

func buildSectionView(record *persistedDocument) (SectionView, error) {
	sections := make(map[shoal.ID]document.Section, len(record.Sections))
	children := make(map[shoal.ID][]shoal.ID)
	spans := make(map[shoal.ID][]document.Span)
	for _, section := range record.Sections {
		if _, duplicate := sections[section.ID]; duplicate {
			return SectionView{}, shoal.NewError(
				shoal.ErrorInternal, "stored document has duplicate sections")
		}
		sections[section.ID] = section
		if section.ParentID != "" {
			children[section.ParentID] = append(children[section.ParentID], section.ID)
		}
	}
	for _, span := range record.Spans {
		if _, ok := sections[span.SectionID]; !ok {
			return SectionView{}, shoal.NewError(
				shoal.ErrorInternal, "stored span has no section")
		}
		spans[span.SectionID] = append(spans[span.SectionID], span)
	}
	visiting := make(map[shoal.ID]bool)
	var build func(shoal.ID) (SectionView, error)
	build = func(id shoal.ID) (SectionView, error) {
		section, ok := sections[id]
		if !ok {
			return SectionView{}, shoal.NewError(
				shoal.ErrorInternal, "stored section hierarchy is incomplete")
		}
		if visiting[id] {
			return SectionView{}, shoal.NewError(
				shoal.ErrorInternal, "stored section hierarchy contains a cycle")
		}
		visiting[id] = true
		view := SectionView{
			Section: cloneSection(section),
			Spans:   append([]document.Span(nil), spans[id]...),
		}
		for i := range view.Spans {
			view.Spans[i] = cloneSpan(view.Spans[i])
		}
		sort.Slice(view.Spans, func(i, j int) bool {
			return view.Spans[i].Order < view.Spans[j].Order
		})
		childIDs := append([]shoal.ID(nil), children[id]...)
		sort.Slice(childIDs, func(i, j int) bool {
			return sections[childIDs[i]].Order < sections[childIDs[j]].Order
		})
		for _, childID := range childIDs {
			child, err := build(childID)
			if err != nil {
				return SectionView{}, err
			}
			view.Children = append(view.Children, child)
		}
		delete(visiting, id)
		return view, nil
	}
	if _, ok := sections[record.Document.RootSectionID]; !ok {
		return SectionView{}, shoal.NewError(
			shoal.ErrorInternal, "stored document root is missing")
	}
	return build(record.Document.RootSectionID)
}

// existingRevisionResult reports an ingest whose content resolved to a
// revision the document already has. Revisions are immutable and
// content-addressed, so nothing is written either way. The disposition says
// whether that revision is the document's current one (IngestUnchanged) or an
// earlier one that stays historical (IngestHistorical, #674). Ingest never
// makes an earlier revision current again: that would let a delayed stale
// retry silently roll the document back to old content.
//
// When the document's publication order is unavailable (all of its revisions
// are legacy ones), which revision is current is unknown, so that error is
// reported instead of either disposition.
func existingRevisionResult(
	revisions map[shoal.ID]*persistedDocument, existing *persistedDocument,
) (IngestResult, error) {
	current, err := latestRevision(revisions)
	if err != nil {
		return IngestResult{}, err
	}
	if current.Revision.ID != existing.Revision.ID {
		return ingestResult(existing, IngestHistorical), nil
	}
	return ingestResult(existing, IngestUnchanged), nil
}

func ingestResult(record *persistedDocument, disposition IngestDisposition) IngestResult {
	return IngestResult{
		Disposition:  disposition,
		Document:     cloneDocument(record.Document),
		Revision:     cloneRevision(record.Revision),
		SectionCount: len(record.Sections),
		SpanCount:    len(record.Spans),
	}
}

func edgesEqual(left, right graph.Edge) bool {
	if left.ID != right.ID || left.From != right.From || left.To != right.To ||
		left.Type != right.Type || left.Weight != right.Weight ||
		len(left.Properties) != len(right.Properties) {
		return false
	}
	for key, value := range left.Properties {
		if right.Properties[key] != value {
			return false
		}
	}
	return true
}

func putDerivedGraphNode(nodes map[shoal.ID]graph.Node, node graph.Node) error {
	if existing, exists := nodes[node.ID]; exists && !nodesEqual(existing, node) {
		return shoal.NewError(
			shoal.ErrorConflict,
			"derived graph node ID already has different content",
		)
	}
	nodes[node.ID] = node
	return nil
}

func nodesEqual(left, right graph.Node) bool {
	if left.ID != right.ID || left.Kind != right.Kind ||
		len(left.Labels) != len(right.Labels) ||
		len(left.Properties) != len(right.Properties) {
		return false
	}
	for index := range left.Labels {
		if left.Labels[index] != right.Labels[index] {
			return false
		}
	}
	for key, value := range left.Properties {
		if right.Properties[key] != value {
			return false
		}
	}
	return true
}

func validatePersistedEdge(edge graph.Edge) error {
	if err := edge.Validate(); err != nil {
		return err
	}
	if !utf8.ValidString(edge.Type) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"embedded edge type must be valid UTF-8",
		)
	}
	return nil
}

func validateLegacyPersistedEdge(edge graph.Edge) error {
	if edge.ID == "" || edge.From == "" || edge.To == "" ||
		strings.TrimSpace(edge.Type) == "" {
		return shoal.NewError(shoal.ErrorInvalidArgument, "edge is structurally incomplete")
	}
	for _, value := range []string{
		string(edge.ID), string(edge.From), string(edge.To), edge.Type,
	} {
		if !utf8.ValidString(value) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "edge values must be valid UTF-8")
		}
	}
	if math.IsNaN(float64(edge.Weight)) || math.IsInf(float64(edge.Weight), 0) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "edge weight must be finite")
	}
	for key, value := range edge.Properties {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "edge properties must be valid UTF-8")
		}
	}
	return nil
}

func cloneNode(node graph.Node) graph.Node {
	node.Labels = append([]string(nil), node.Labels...)
	node.Properties = cloneMetadata(node.Properties)
	return node
}

func cloneEdge(edge graph.Edge) graph.Edge {
	edge.Properties = cloneMetadata(edge.Properties)
	return edge
}

func cloneDocument(value document.Document) document.Document {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneRevision(value document.Revision) document.Revision {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneSection(value document.Section) document.Section {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneSpan(value document.Span) document.Span {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	switch err := ctx.Err(); {
	case errors.Is(err, context.Canceled):
		return shoal.WrapError(shoal.ErrorCanceled, "operation canceled", err)
	case errors.Is(err, context.DeadlineExceeded):
		return shoal.WrapError(shoal.ErrorDeadline, "operation deadline exceeded", err)
	default:
		return nil
	}
}

var _ Client = (*Explorer)(nil)
var _ BoundedClient = (*Explorer)(nil)
