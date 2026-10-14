// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package webapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	exploreranalytics "github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Service is independent of HTTP and storage topology so another transport can
// implement the same browser contract against distributed Shoal.
type Service interface {
	Documents(context.Context, DocumentsRequest) (DocumentsResponse, error)
	Document(context.Context, DocumentRequest) (DocumentResponse, error)
	Retrieve(context.Context, RetrievalRequest) (RetrievalResponse, error)
	Neighborhood(context.Context, NeighborhoodRequest) (NeighborhoodResponse, error)
	Path(context.Context, PathRequest) (PathResponse, error)
}

// IngestProvider is an optional service extension for mutable browser uploads.
type IngestProvider interface {
	Ingest(context.Context, IngestRequest) (IngestResponse, error)
}

// IngestAvailabilityProvider reports whether an IngestProvider can currently
// accept calls without additional capability negotiation.
type IngestAvailabilityProvider interface {
	IngestAvailable() bool
}

type ExtractionProvider interface {
	Extract(context.Context, ExtractRequest) (ExtractResponse, error)
}

// ExtractionAvailabilityProvider reports whether an ExtractionProvider has
// the runtime configuration required to accept calls.
type ExtractionAvailabilityProvider interface {
	ExtractionAvailable() bool
}

// RecomputeProvider is an optional service extension that re-runs the
// deterministic derivation behind a latent similarity assertion. Services that
// cannot re-derive do not implement it, and the transport fails closed with an
// unavailable error rather than fabricating a result.
type RecomputeProvider interface {
	Recompute(context.Context, RecomputeDerivationRequest) (RecomputeDerivationResponse, error)
}

// ChangeProvider is an optional service extension for the resumable document
// change feed. Services that cannot serve an ordered feed do not implement it,
// and the transport fails closed with an unavailable error.
type ChangeProvider interface {
	Changes(context.Context, ChangesRequest) (ChangesResponse, error)
}

// ChangeAvailabilityProvider reports whether a ChangeProvider has an ordered,
// authorized change-feed backend.
type ChangeAvailabilityProvider interface {
	ChangesAvailable() bool
}

// CapabilityProvider is an optional service extension for dynamic feature
// negotiation. Services that do not implement it are treated as capable for
// stable non-vector operations, while vector fails closed.
type CapabilityProvider interface {
	Capabilities(context.Context) (Capabilities, error)
}

// MetadataProvider is an optional service extension for backends that can
// negotiate both feature availability and public bounds.
type MetadataProvider interface {
	Metadata(context.Context) (MetadataResponse, error)
}

type vectorAvailabilityProvider interface {
	VectorAvailable(context.Context) (bool, error)
}

// suppressionCountingRetriever is implemented by an authorized backing client
// that can report, alongside a retrieval, how many current documents
// authorization withheld from the caller. It is optional: a client that does
// not implement it is treated as withholding nothing, so the signal fails
// closed to "0 withheld" rather than fabricating a count.
type suppressionCountingRetriever interface {
	RetrieveWithSuppressed(
		context.Context, retrieval.Request,
	) (retrieval.Response, uint32, error)
}

// suppressionCountingLister is the Documents-listing counterpart to
// suppressionCountingRetriever and is optional under the same fail-closed rule.
type suppressionCountingLister interface {
	DocumentsWithSuppressed(
		context.Context,
	) ([]explorer.DocumentSummary, uint32, error)
}

// disclosureCountingRetriever is implemented by an authorized backing client
// that can report, alongside a retrieval, both withholding reason classes: plain
// authorization denials and mosaic co-occurrence restrictions. It is preferred
// over suppressionCountingRetriever when available so the two counts stay
// distinguishable end to end; a client that implements only the older interface
// still reports the denial count.
type disclosureCountingRetriever interface {
	RetrieveWithDisclosure(
		context.Context, retrieval.Request,
	) (retrieval.Response, authorized.Disclosure, error)
}

// reportingRetriever is the authorized retrieval extension that preserves
// request-local embedding-space observability, including on provider failure.
type reportingRetriever interface {
	RetrieveWithReport(
		context.Context, retrieval.Request,
	) (retrieval.Response, authorized.RetrievalReport, error)
}

// disclosureCountingLister is the Documents-listing counterpart to
// disclosureCountingRetriever under the same preference rule.
type disclosureCountingLister interface {
	DocumentsWithDisclosure(
		context.Context,
	) ([]explorer.DocumentSummary, authorized.Disclosure, error)
}

// changeFeedBackend is implemented by an authorized backing client that can
// serve the caller's resumable document change feed. It is optional: a client
// that does not implement it makes the change capability unavailable rather
// than serving an unfiltered or unordered feed.
type changeFeedBackend interface {
	Changes(
		context.Context, authorized.ChangeFeedRequest,
	) (authorized.ChangeFeedPage, error)
}

// EmbeddedService adapts the public Explorer client to the workspace service.
type EmbeddedService struct {
	client            explorer.BoundedClient
	analytics         *exploreranalytics.Service
	ontologyMu        sync.RWMutex
	ontologyPublishMu sync.Mutex
	ontologyVersion   *ontology.OntologyVersion
	clock             func() time.Time
	// concealWithholding suppresses the withholding counts in responses. The
	// default is false, preserving the deliberate disclosure documented on
	// retrieveReporting: a caller should not silently mistake a short answer
	// for "nothing exists".
	//
	// What the counts disclose is volume, not content and not per-query
	// existence. Both are computed over the whole corpus before retrieval, so
	// the same identity sees the same numbers for every query; they move when
	// the corpus or the identity's grants change. A compartmented deployment
	// may still decline to tell a caller how much it cannot see, or to hand it
	// a change signal for content it has no grant for. Neither mode changes
	// what is recorded for audit.
	concealWithholding bool
}

// ConcealWithholding suppresses the withholding counts in every response that
// carries them. Both counts are concealed together: a response that omits one
// and emits the other would itself distinguish the two reason classes.
//
// It must be set during composition, before the service serves a request.
func (s *EmbeddedService) ConcealWithholding(conceal bool) {
	s.concealWithholding = conceal
}

// disclose returns the counts a response may carry for this service.
func (s *EmbeddedService) disclose(
	disclosure authorized.Disclosure,
) authorized.Disclosure {
	if s.concealWithholding {
		return authorized.Disclosure{}
	}
	return disclosure
}

// discloseEmbedding clears the withholding booleans an embedding report
// derives from the same counts. Concealing the counts while leaving these set
// would keep the signal in a narrower form rather than removing it. Every
// other field of the report is request-local observability and is preserved.
func (s *EmbeddedService) discloseEmbedding(
	report *authorized.EmbeddingQueryReport,
) *authorized.EmbeddingQueryReport {
	if report == nil || !s.concealWithholding {
		return report
	}
	concealed := *report
	concealed.Spaces = append(
		[]authorized.EmbeddingSpaceReport(nil), report.Spaces...)
	concealed.Suppressed = false
	concealed.Restricted = false
	return &concealed
}

// NewEmbeddedService creates a local service without exposing the embedded
// engine through the service contract.
func NewEmbeddedService(client explorer.BoundedClient) (*EmbeddedService, error) {
	if client == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "explorer client is required")
	}
	service := &EmbeddedService{client: client, clock: time.Now}
	if materializer, ok := client.(exploreranalytics.Materializer); ok {
		sinkProvider, ok := client.(interface {
			AnalyticsInteractionSink() interaction.ResultSink
		})
		if !ok {
			return service, nil
		}
		sink := sinkProvider.AnalyticsInteractionSink()
		if sink == nil {
			return service, nil
		}
		sharedRecorder, err := interaction.NewRecorder(
			context.Background(), sink)
		if err != nil {
			return nil, err
		}
		recorder, err := exploreranalytics.NewInteractionRecorder(
			sharedRecorder, service.clock)
		if err != nil {
			return nil, err
		}
		analyticsService, err := exploreranalytics.NewService(
			exploreranalytics.Config{
				Source: materializer, Limits: exploreranalytics.DefaultLimits(),
				Recorder: recorder, RequireRecording: true,
			},
		)
		if err != nil {
			return nil, err
		}
		service.analytics = analyticsService
	}
	return service, nil
}

func (s *EmbeddedService) Capabilities(ctx context.Context) (Capabilities, error) {
	capabilities := AllCapabilities()
	capabilities.Vector = false
	if !s.ExtractionAvailable() {
		capabilities.Extraction = false
	}
	if !s.ChangesAvailable() {
		capabilities.Changes = false
	}
	capabilities.Analytics = s.AnalyticsAvailable()
	provider, ok := s.client.(vectorAvailabilityProvider)
	if !ok {
		return capabilities, nil
	}
	available, err := provider.VectorAvailable(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	capabilities.Vector = available
	return capabilities, nil
}

// IngestAvailable reports whether the embedded client can accept ingestion.
func (s *EmbeddedService) IngestAvailable() bool {
	return s != nil && s.client != nil
}

// ExtractionAvailable reports whether the service has both an extraction
// backend and an active ontology.
func (s *EmbeddedService) ExtractionAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.client.(interface {
		ExtractDocument(
			context.Context,
			explorer.ExtractionRequest,
		) (explorer.ExtractionResult, error)
	}); !ok {
		return false
	}
	s.ontologyMu.RLock()
	defer s.ontologyMu.RUnlock()
	return s.ontologyVersion != nil
}

// ChangesAvailable reports whether the embedded client can serve the ordered,
// authorized change feed.
func (s *EmbeddedService) ChangesAvailable() bool {
	if s == nil {
		return false
	}
	_, ok := s.client.(changeFeedBackend)
	return ok
}

func (s *EmbeddedService) Extract(
	ctx context.Context,
	request ExtractRequest,
) (ExtractResponse, error) {
	if _, err := s.pin(ctx, request.Snapshot); err != nil {
		return ExtractResponse{}, err
	}
	version, configured, err := s.activeOntologyForMutation(ctx)
	if err != nil {
		return ExtractResponse{}, err
	}
	if !configured {
		return ExtractResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "an active ontology is required for extraction")
	}
	backend, ok := s.client.(interface {
		ExtractDocument(
			context.Context,
			explorer.ExtractionRequest,
		) (explorer.ExtractionResult, error)
	})
	if !ok {
		return ExtractResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"extraction\" is unavailable")
	}
	result, err := backend.ExtractDocument(ctx, explorer.ExtractionRequest{
		DocumentID: request.DocumentID, RevisionID: request.RevisionID,
		Version: version, Instructions: request.Instructions,
	})
	if err != nil {
		return ExtractResponse{}, err
	}
	responseSnapshot := fromExplorerSnapshot(result.Snapshot)
	if err := s.confirmSnapshot(ctx, responseSnapshot); err != nil {
		return ExtractResponse{}, err
	}
	return ExtractResponse{
		Snapshot:   responseSnapshot,
		DocumentID: result.DocumentID, RevisionID: result.RevisionID,
		ExtractionID: result.ExtractionID, EntityCount: result.EntityCount,
		RelationCount: result.RelationCount, GraphNodeCount: result.GraphNodeCount,
		GraphEdgeCount: result.GraphEdgeCount, CreatedEntities: result.CreatedEntities,
		ReusedEntities:      result.ReusedEntities,
		EntityNodeIDs:       append([]shoal.ID(nil), result.EntityNodeIDs...),
		RelationshipEdgeIDs: append([]shoal.ID(nil), result.RelationshipEdgeIDs...),
	}, nil
}

// Recompute re-runs the deterministic derivation behind a latent similarity
// assertion against the current corpus frontier and reports whether it
// reproduced the caller's prior digest byte-for-byte. It reads through the same
// bounded, authorized graph path the browser reads, so a caller can only
// recompute a derivation it is authorized to see.
func (s *EmbeddedService) Recompute(
	ctx context.Context,
	request RecomputeDerivationRequest,
) (RecomputeDerivationResponse, error) {
	if err := shoal.ValidateRequiredID(
		"derived assertion ID", request.AssertionID); err != nil {
		return RecomputeDerivationResponse{}, err
	}
	before, err := s.client.Snapshot(ctx)
	if err != nil {
		return RecomputeDerivationResponse{}, err
	}
	snapshot := fromExplorerSnapshot(before)
	assertion, err := s.rederiveAssertion(ctx, request.AssertionID)
	if err != nil {
		return RecomputeDerivationResponse{}, err
	}
	if err := s.confirmSnapshot(ctx, snapshot); err != nil {
		return RecomputeDerivationResponse{}, err
	}
	detail, err := derivationDetail(assertion)
	if err != nil {
		return RecomputeDerivationResponse{}, err
	}
	digest := derivationDigest(detail)
	// An empty caller digest is a baseline capture with nothing to compare, so
	// it reports unchanged. A non-empty digest reports unchanged only when the
	// fresh re-derivation is byte-identical to what the caller already holds.
	unchanged := request.Digest == "" || request.Digest == digest
	return RecomputeDerivationResponse{
		Snapshot: snapshot, Unchanged: unchanged, Digest: digest, Detail: detail,
	}, nil
}

// rederiveAssertion re-reads the derived assertion for id through the bounded
// authorized graph, following the producer→assertion provenance edge so the
// read exercises the same materialization the browser graph does.
func (s *EmbeddedService) rederiveAssertion(
	ctx context.Context, id shoal.ID,
) (ontology.Assertion, error) {
	result, err := s.client.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{id}, Depth: 1, Fanout: MaxFanout, MaxNodes: MaxNodes,
		EdgeTypes: []string{graph.EdgeTypeProduced},
		Direction: explorer.GraphDirectionIncoming,
	})
	if err != nil {
		return ontology.Assertion{}, err
	}
	for _, assertion := range result.Neighborhood.Assertions {
		if assertion.ID() == id {
			return assertion, nil
		}
	}
	return ontology.Assertion{}, shoal.NewError(
		shoal.ErrorNotFound,
		"derived assertion is not present at the current snapshot",
	)
}

func derivationDetail(assertion ontology.Assertion) (DerivationDetail, error) {
	if assertion.Origin() != ontology.AssertionDerived {
		return DerivationDetail{}, shoal.NewError(
			shoal.ErrorNotFound, "assertion is not derived")
	}
	evidence := assertion.Evidence()
	if len(evidence) != 1 {
		return DerivationDetail{}, shoal.NewError(
			shoal.ErrorNotFound, "derived assertion evidence is missing")
	}
	derivation, ok := evidence[0].Derivation()
	if !ok {
		return DerivationDetail{}, shoal.NewError(
			shoal.ErrorNotFound, "derived assertion derivation is missing")
	}
	provenance := assertion.Provenance()
	return DerivationDetail{
		AssertionID:           assertion.ID(),
		DerivationID:          derivation.ID(),
		Origin:                string(assertion.Origin()),
		Score:                 float64(derivation.Score()),
		EmbeddingModel:        derivation.EmbeddingModel(),
		EmbeddingModelVersion: derivation.EmbeddingModelVersion(),
		SimilarityMetric:      derivation.SimilarityMetric(),
		Threshold:             float64(derivation.Threshold()),
		TessellationCell:      derivation.TessellationCell(),
		IteratorName:          derivation.IteratorName(),
		IteratorOptions:       derivation.IteratorOptions(),
		Provider:              provenance.Provider(),
		Model:                 provenance.Model(),
		ModelVersion:          provenance.ModelVersion(),
	}, nil
}

// derivationDigest is the deterministic fingerprint that makes "recompute"
// meaningful: identical derivation inputs always fold to the same digest, and
// any changed input changes it. Every field is length-prefixed so no value can
// impersonate a field boundary, and iterator options are canonicalized so map
// iteration order cannot perturb the digest.
func derivationDigest(detail DerivationDetail) string {
	var builder strings.Builder
	writeField := func(name, value string) {
		builder.WriteString(strconv.Itoa(len(name)))
		builder.WriteByte(':')
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
		builder.WriteByte('|')
	}
	writeField("assertion_id", string(detail.AssertionID))
	writeField("derivation_id", string(detail.DerivationID))
	writeField("origin", detail.Origin)
	writeField("score", strconv.FormatFloat(detail.Score, 'g', -1, 64))
	writeField("embedding_model", detail.EmbeddingModel)
	writeField("embedding_model_version", detail.EmbeddingModelVersion)
	writeField("similarity_metric", detail.SimilarityMetric)
	writeField("threshold", strconv.FormatFloat(detail.Threshold, 'g', -1, 64))
	writeField("tessellation_cell", detail.TessellationCell)
	writeField("iterator_name", detail.IteratorName)
	writeField("provider", detail.Provider)
	writeField("model", detail.Model)
	writeField("model_version", detail.ModelVersion)
	writeField("iterator_options", canonicalDerivationMetadata(detail.IteratorOptions))
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func canonicalDerivationMetadata(metadata shoal.Metadata) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	// Load-bearing: TestRecomputeDigestIsStableAcrossCalls pins that map
	// iteration order cannot change the digest, so these keys must be sorted.
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString(strconv.Itoa(len(keys)))
	for _, key := range keys {
		value := metadata[key]
		builder.WriteByte('|')
		// Load-bearing: TestRecomputeDigestSeparatesMetadataBoundaries pins that
		// each dynamic key and value is length-prefixed, so a delimiter inside a
		// value cannot impersonate the boundary to the next option.
		builder.WriteString(strconv.Itoa(len(key)))
		builder.WriteByte(':')
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
	}
	return builder.String()
}

func (s *EmbeddedService) Documents(
	ctx context.Context, request DocumentsRequest,
) (DocumentsResponse, error) {
	snapshot, err := s.pin(ctx, request.Snapshot)
	if err != nil {
		return DocumentsResponse{}, err
	}
	limit, err := normalizeLimit(request.Page.Limit)
	if err != nil {
		return DocumentsResponse{}, err
	}
	offset, err := decodeCursor(request.Page.Cursor, snapshot.ID)
	if err != nil {
		return DocumentsResponse{}, err
	}
	documents, disclosure, err := s.documentsCountingDisclosure(ctx)
	if err != nil {
		return DocumentsResponse{}, err
	}
	if err := s.confirmSnapshot(ctx, snapshot); err != nil {
		return DocumentsResponse{}, err
	}
	if offset > len(documents) {
		return DocumentsResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "document cursor is outside the snapshot")
	}
	end := offset + int(limit)
	if end > len(documents) {
		end = len(documents)
	}
	disclosed := s.disclose(disclosure)
	response := DocumentsResponse{
		Snapshot:   snapshot,
		Documents:  append([]explorer.DocumentSummary(nil), documents[offset:end]...),
		Suppressed: disclosed.Suppressed,
		Restricted: disclosed.Restricted,
	}
	if end < len(documents) {
		response.NextCursor = encodeCursor(snapshot.ID, end)
	}
	return response, nil
}

// documentsCountingDisclosure lists the authorized documents and, when the
// backing client can report it, the corpus-wide counts of current documents
// withheld from this identity, split by reason class: plain authorization
// denials and mosaic co-occurrence restrictions. See retrieveReporting
// for the amplification risk of disclosing these counts; the same caveat applies
// to the document listing. A client that reports only the older suppression
// count still contributes the denial count with a zero restriction count, and a
// client that reports neither withholds nothing.
func (s *EmbeddedService) documentsCountingDisclosure(
	ctx context.Context,
) ([]explorer.DocumentSummary, authorized.Disclosure, error) {
	if counter, ok := s.client.(disclosureCountingLister); ok {
		return counter.DocumentsWithDisclosure(ctx)
	}
	if counter, ok := s.client.(suppressionCountingLister); ok {
		documents, suppressed, err := counter.DocumentsWithSuppressed(ctx)
		return documents, authorized.Disclosure{Suppressed: suppressed}, err
	}
	documents, err := s.client.Documents(ctx)
	return documents, authorized.Disclosure{}, err
}

// Changes serves the caller's resumable document change feed. The request
// cursor is an opaque sealed token minted by the authorized layer; this service
// never reads or constructs it, it only forwards it and returns the sealed
// cursor the backend produces. The cursor's confidentiality and integrity --
// and the resynchronise handling for a stale or foreign cursor -- live in
// authorized.Client.Changes, which holds the per-corpus seal key. No
// withheld-change count is emitted: see authorized.Client.Changes for why the
// feed discloses less than the Documents and Retrieve listings.
func (s *EmbeddedService) Changes(
	ctx context.Context, request ChangesRequest,
) (ChangesResponse, error) {
	backend, ok := s.client.(changeFeedBackend)
	if !ok {
		return ChangesResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"changes\" is unavailable")
	}
	limit, err := normalizeChangeLimit(request.Limit)
	if err != nil {
		return ChangesResponse{}, err
	}
	page, err := backend.Changes(ctx, authorized.ChangeFeedRequest{
		Cursor: request.Cursor,
		Limit:  int(limit),
	})
	if err != nil {
		return ChangesResponse{}, err
	}
	changes := make([]WorkspaceChange, 0, len(page.Changes))
	for _, change := range page.Changes {
		changes = append(changes, WorkspaceChange{
			Kind: string(change.Kind),
			Document: explorer.DocumentSummary{
				Document:        change.Document,
				Revision:        change.Revision,
				SourceURI:       change.SourceURI,
				SourceMediaType: change.SourceMediaType,
			},
		})
	}
	return ChangesResponse{
		Changes:    changes,
		NextCursor: page.Cursor,
		More:       page.More,
	}, nil
}

func (s *EmbeddedService) Document(
	ctx context.Context, request DocumentRequest,
) (DocumentResponse, error) {
	snapshot, err := s.pin(ctx, request.Snapshot)
	if err != nil {
		return DocumentResponse{}, err
	}
	view, err := s.client.Document(ctx, request.DocumentID, request.RevisionID)
	if err != nil {
		return DocumentResponse{}, err
	}
	if err := s.confirmSnapshot(ctx, snapshot); err != nil {
		return DocumentResponse{}, err
	}
	return DocumentResponse{Snapshot: snapshot, Document: view}, nil
}

func (s *EmbeddedService) Retrieve(
	ctx context.Context, request RetrievalRequest,
) (RetrievalResponse, error) {
	snapshot, err := s.pin(ctx, request.Snapshot)
	if err != nil {
		return RetrievalResponse{}, err
	}
	query, err := request.Query.Normalize()
	if err != nil {
		return RetrievalResponse{}, err
	}
	if query.TopK > MaxTopK {
		return RetrievalResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace top_k exceeds the server bound")
	}
	if !query.AsOf.IsZero() && !query.AsOf.Equal(snapshot.AsOf) {
		return RetrievalResponse{}, shoal.NewError(
			shoal.ErrorConflict, "retrieval as_of does not match the pinned snapshot")
	}
	// The embedded backend is already pinned by the snapshot identity and does
	// not implement historical publication-frontier reads.
	query.AsOf = time.Time{}
	response, report, err := s.retrieveReporting(ctx, query)
	if err != nil {
		if concealed := s.discloseEmbedding(report.Embedding); concealed != nil {
			return RetrievalResponse{}, newEmbeddingQueryError(err, *concealed)
		}
		return RetrievalResponse{}, err
	}
	if err := validateRetrievalResponse(response, query); err != nil {
		return RetrievalResponse{}, shoal.WrapError(
			shoal.ErrorInternal, "invalid retrieval response", err)
	}
	if err := s.confirmSnapshot(ctx, snapshot); err != nil {
		return RetrievalResponse{}, err
	}
	disclosed := s.disclose(report.Disclosure)
	return RetrievalResponse{
		Snapshot: snapshot, Retrieval: response,
		Suppressed: disclosed.Suppressed,
		Restricted: disclosed.Restricted,
		Embedding:  s.discloseEmbedding(report.Embedding),
	}, nil
}

// retrieveReporting runs the authorized retrieval and, when the backing client
// can report it, preserves both document withholding counts and request-local
// authorized embedding-space outcomes.
//
// Amplification risk. The Suppressed count is a real disclosure. Because a
// caller can re-run retrieval as the corpus changes, and vary the request, and
// watch the number move, it is a coarse oracle for the existence and rough
// volume of content the caller is not allowed to read. The count is the entire
// leak — no identifiers, labels, or snippets accompany it — but it is still a
// leak. It is emitted deliberately so a short or empty answer can never be
// silently mistaken for "nothing exists".
//
// The Restricted count is deliberately narrower: it only ever counts documents
// the caller is individually authorized to read, withheld to prevent
// aggregation. It therefore discloses nothing about content the caller is not
// cleared to read, and cannot become an existence oracle for unauthorized
// content the way a caller-distinguishable denial signal could. It names no
// domain and no document. See docs and app.js for how the two counts are
// surfaced as distinct reason classes.
func (s *EmbeddedService) retrieveReporting(
	ctx context.Context, query retrieval.Request,
) (retrieval.Response, authorized.RetrievalReport, error) {
	if reporter, ok := s.client.(reportingRetriever); ok {
		return reporter.RetrieveWithReport(ctx, query)
	}
	if counter, ok := s.client.(disclosureCountingRetriever); ok {
		response, disclosure, err := counter.RetrieveWithDisclosure(ctx, query)
		return response, authorized.RetrievalReport{Disclosure: disclosure}, err
	}
	if counter, ok := s.client.(suppressionCountingRetriever); ok {
		response, suppressed, err := counter.RetrieveWithSuppressed(ctx, query)
		return response, authorized.RetrievalReport{
			Disclosure: authorized.Disclosure{Suppressed: suppressed},
		}, err
	}
	response, err := s.client.Retrieve(ctx, query)
	return response, authorized.RetrievalReport{}, err
}

func (s *EmbeddedService) Neighborhood(
	ctx context.Context, request NeighborhoodRequest,
) (NeighborhoodResponse, error) {
	snapshot, err := s.pin(ctx, request.Snapshot)
	if err != nil {
		return NeighborhoodResponse{}, err
	}
	depth, fanout, maxNodes, err := normalizeGraphBounds(
		request.Depth, request.Fanout, request.MaxNodes)
	if err != nil {
		return NeighborhoodResponse{}, err
	}
	normalizedBase, err := (explorer.NeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return NeighborhoodResponse{}, err
	}
	request.NodeIDs = normalizedBase.NodeIDs
	request.EdgeTypes = normalizedBase.EdgeTypes
	if err := validateEdgeTypes(request.EdgeTypes); err != nil {
		return NeighborhoodResponse{}, err
	}
	if len(request.NodeIDs) == 0 {
		return NeighborhoodResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "at least one graph node ID is required")
	}
	if uint32(len(request.NodeIDs)) > maxNodes {
		return NeighborhoodResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph seeds exceed max_nodes")
	}
	normalizedRequest := request
	normalizedRequest.Depth = depth
	normalizedRequest.Fanout = fanout
	normalizedRequest.MaxNodes = maxNodes
	afterEdgeID, err := decodeGraphCursor(
		request.Cursor, snapshot.ID, normalizedRequest)
	if err != nil {
		return NeighborhoodResponse{}, err
	}

	result, err := s.client.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: depth, Fanout: fanout,
		MaxNodes: maxNodes, EdgeTypes: request.EdgeTypes,
		AfterEdgeID: afterEdgeID,
	})
	if err != nil {
		return NeighborhoodResponse{}, err
	}
	if err := s.confirmSnapshot(ctx, snapshot); err != nil {
		return NeighborhoodResponse{}, err
	}
	response := NeighborhoodResponse{
		Snapshot: snapshot, Neighborhood: result.Neighborhood,
		OntologyInterpretations: ontologyInterpretationReports(
			result.Neighborhood.Interpretations),
		Truncated: result.Truncated,
	}
	if result.Continuation {
		response.NextCursor = encodeGraphCursor(
			snapshot.ID, normalizedRequest, result.NextAfterEdgeID)
	}
	return response, nil
}

func ontologyInterpretationReports(
	values []ontology.AssertionInterpretation,
) []OntologyInterpretationReport {
	if len(values) == 0 {
		return nil
	}
	out := make([]OntologyInterpretationReport, 0, len(values))
	for _, value := range values {
		original := value.Original()
		originalSubject, _ := original.SubjectType()
		originalObject, _ := original.ObjectType()
		subject, _ := value.SubjectType()
		object, _ := value.ObjectType()
		applied := value.AppliedMorphisms()
		appliedStrings := make([]string, len(applied))
		for i, id := range applied {
			appliedStrings[i] = encodeID(id)
		}
		reader := value.Reader()
		out = append(out, OntologyInterpretationReport{
			AssertionID: encodeID(original.ID()),
			SchemaID:    encodeID(reader.SchemaID()), VersionID: encodeID(reader.VersionID()),
			Reading: value.Reading(), Status: value.Status(),
			OriginalSubjectType: encodeOptionalID(originalSubject),
			SubjectType:         encodeOptionalID(subject),
			OriginalPredicate:   encodeID(original.Predicate()),
			Predicate:           encodeID(value.Predicate()),
			OriginalObjectType:  encodeOptionalID(originalObject),
			ObjectType:          encodeOptionalID(object),
			AppliedMorphisms:    appliedStrings, Reason: value.Reason(),
		})
	}
	return out
}

func (s *EmbeddedService) Path(
	ctx context.Context, request PathRequest,
) (PathResponse, error) {
	snapshot, err := s.pin(ctx, request.Snapshot)
	if err != nil {
		return PathResponse{}, err
	}
	if err := shoal.ValidateRequiredID("path source node ID", request.From); err != nil {
		return PathResponse{}, err
	}
	if err := shoal.ValidateRequiredID("path target node ID", request.To); err != nil {
		return PathResponse{}, err
	}
	depth, fanout, maxNodes, err := normalizeGraphBounds(
		request.MaxDepth, request.Fanout,
		effectiveGraphNodeLimit(ctx, MaxNodes))
	if err != nil {
		return PathResponse{}, err
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{request.From}, Depth: depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return PathResponse{}, err
	}
	request.EdgeTypes = normalized.EdgeTypes
	if err := validateEdgeTypes(request.EdgeTypes); err != nil {
		return PathResponse{}, err
	}
	bounded, err := s.client.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{request.From}, Depth: depth, Fanout: fanout,
		MaxNodes: maxNodes, EdgeTypes: request.EdgeTypes,
		Direction: explorer.GraphDirectionOutgoing,
	})
	if err != nil {
		return PathResponse{}, err
	}
	path, err := directedPath(
		bounded.Neighborhood, request.From, request.To, depth)
	if err != nil {
		return PathResponse{}, err
	}
	if err := path.Validate(); err != nil {
		return PathResponse{}, shoal.WrapError(shoal.ErrorInternal, "invalid path", err)
	}
	if err := s.confirmSnapshot(ctx, snapshot); err != nil {
		return PathResponse{}, err
	}
	assertions := assertionsForPath(path, bounded.Neighborhood.Assertions)
	return PathResponse{
		Snapshot: snapshot, Path: path,
		Assertions: assertions,
		OntologyInterpretations: ontologyInterpretationReports(
			interpretationsForAssertions(
				assertions, bounded.Neighborhood.Interpretations)),
	}, nil
}

func interpretationsForAssertions(
	assertions []ontology.Assertion,
	interpretations []ontology.AssertionInterpretation,
) []ontology.AssertionInterpretation {
	if len(assertions) == 0 || len(interpretations) == 0 {
		return nil
	}
	selected := make(map[shoal.ID]struct{}, len(assertions))
	for _, assertion := range assertions {
		selected[assertion.ID()] = struct{}{}
	}
	out := make([]ontology.AssertionInterpretation, 0, len(assertions))
	for _, interpretation := range interpretations {
		if _, ok := selected[interpretation.Original().ID()]; ok {
			out = append(out, interpretation)
		}
	}
	return out
}

func (s *EmbeddedService) pin(
	ctx context.Context, requested Snapshot,
) (Snapshot, error) {
	before, err := s.client.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := fromExplorerSnapshot(before)
	if err := validateRequestedSnapshot(requested, snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *EmbeddedService) confirmSnapshot(ctx context.Context, expected Snapshot) error {
	current, err := s.client.Snapshot(ctx)
	if err != nil {
		return err
	}
	if fromExplorerSnapshot(current) != expected {
		return shoal.NewError(
			shoal.ErrorConflict, "corpus changed while serving the snapshot")
	}
	return nil
}

func validateRequestedSnapshot(requested, current Snapshot) error {
	if requested.ID != "" && requested.ID != current.ID {
		return shoal.NewError(
			shoal.ErrorConflict, "requested snapshot is no longer current")
	}
	if !requested.AsOf.IsZero() && !requested.AsOf.Equal(current.AsOf) {
		return shoal.NewError(
			shoal.ErrorConflict, "requested as_of does not match the snapshot")
	}
	if requested.Frontier != 0 && requested.Frontier != current.Frontier {
		return shoal.NewError(
			shoal.ErrorConflict, "requested frontier does not match the snapshot")
	}
	return nil
}

func fromExplorerSnapshot(snapshot explorer.Snapshot) Snapshot {
	return Snapshot{
		ID: snapshot.ID, AsOf: snapshot.AsOf, Frontier: snapshot.Frontier,
	}
}

func normalizeLimit(limit uint32) (uint32, error) {
	if limit == 0 {
		return DefaultPageSize, nil
	}
	if limit > MaxPageSize {
		return 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "page limit exceeds the server bound")
	}
	return limit, nil
}

func normalizeGraphBounds(depth, fanout, maxNodes uint32) (uint32, uint32, uint32, error) {
	if depth == 0 {
		depth = DefaultDepth
	}
	if fanout == 0 {
		fanout = DefaultFanout
	}
	if maxNodes == 0 {
		maxNodes = DefaultMaxNodes
	}
	if depth > MaxDepth {
		return 0, 0, 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph depth exceeds the server bound")
	}
	if fanout > MaxFanout {
		return 0, 0, 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph fanout exceeds the server bound")
	}
	if maxNodes > MaxNodes {
		return 0, 0, 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph max_nodes exceeds the server bound")
	}
	return depth, fanout, maxNodes, nil
}

func validateEdgeTypes(edgeTypes []string) error {
	if uint32(len(edgeTypes)) > MaxEdgeTypes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "edge type count exceeds the server bound")
	}
	return nil
}

func validateRetrievalResponse(response retrieval.Response, query retrieval.Request) error {
	if err := response.ValidateFor(query); err != nil {
		return err
	}
	for _, result := range response.Results {
		if uint32(len(result.Evidence)) > MaxEvidencePerResult {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"retrieval evidence count exceeds the server bound",
			)
		}
	}
	return nil
}

func encodeCursor(snapshot string, offset int) string {
	value := snapshot + ":" + strconv.Itoa(offset)
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func normalizeChangeLimit(limit uint32) (uint32, error) {
	if limit == 0 {
		return DefaultChangePageSize, nil
	}
	if limit > MaxChangePageSize {
		return 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "change page limit exceeds the server bound")
	}
	return limit, nil
}

func decodeCursor(cursor, snapshot string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	value, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, shoal.NewError(shoal.ErrorInvalidArgument, "invalid document cursor")
	}
	prefix := snapshot + ":"
	if !strings.HasPrefix(string(value), prefix) {
		return 0, shoal.NewError(
			shoal.ErrorConflict, "document cursor belongs to another snapshot")
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(string(value), prefix))
	if err != nil || offset < 0 {
		return 0, shoal.NewError(shoal.ErrorInvalidArgument, "invalid document cursor")
	}
	return offset, nil
}

type graphCursorPayload struct {
	Snapshot  string `json:"snapshot"`
	Signature string `json:"signature"`
	After     string `json:"after"`
}

func encodeGraphCursor(
	snapshot string, request NeighborhoodRequest, after shoal.ID,
) string {
	encoded, _ := json.Marshal(graphCursorPayload{
		Snapshot: snapshot, Signature: graphRequestSignature(request),
		After: encodeID(after),
	})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeGraphCursor(
	cursor, snapshot string, request NeighborhoodRequest,
) (shoal.ID, error) {
	if cursor == "" {
		return "", nil
	}
	if len(request.NodeIDs) != 1 || request.Depth != 1 {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"graph cursors require one seed and depth 1",
		)
	}
	encoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", shoal.NewError(shoal.ErrorInvalidArgument, "invalid graph cursor")
	}
	var payload graphCursorPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return "", shoal.NewError(shoal.ErrorInvalidArgument, "invalid graph cursor")
	}
	if payload.Snapshot != snapshot {
		return "", shoal.NewError(
			shoal.ErrorConflict, "graph cursor belongs to another snapshot")
	}
	if payload.Signature != graphRequestSignature(request) {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "graph cursor does not match the request")
	}
	after, err := decodeOptionalID(payload.After)
	if err != nil {
		return "", shoal.NewError(shoal.ErrorInvalidArgument, "invalid graph cursor")
	}
	return after, nil
}

func graphRequestSignature(request NeighborhoodRequest) string {
	value := struct {
		NodeIDs   []string `json:"node_ids"`
		Depth     uint32   `json:"depth"`
		Fanout    uint32   `json:"fanout"`
		MaxNodes  uint32   `json:"max_nodes"`
		EdgeTypes []string `json:"edge_types"`
	}{
		Depth: request.Depth, Fanout: request.Fanout,
		MaxNodes: request.MaxNodes, EdgeTypes: request.EdgeTypes,
	}
	for _, id := range request.NodeIDs {
		value.NodeIDs = append(value.NodeIDs, encodeID(id))
	}
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func directedPath(
	neighborhood explorer.Neighborhood, from, to shoal.ID, maxDepth uint32,
) (graph.Path, error) {
	nodes := make(map[shoal.ID]graph.Node, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		nodes[node.ID] = node
	}
	if _, ok := nodes[from]; !ok {
		return graph.Path{}, shoal.NewError(shoal.ErrorNotFound, "path source node not found")
	}
	if from == to {
		return graph.Path{Nodes: []graph.Node{nodes[from]}}, nil
	}
	outgoing := make(map[shoal.ID][]graph.Edge)
	for _, edge := range neighborhood.Edges {
		outgoing[edge.From] = append(outgoing[edge.From], edge)
	}
	for id := range outgoing {
		sort.Slice(outgoing[id], func(i, j int) bool {
			return shoal.CompareID(outgoing[id][i].ID, outgoing[id][j].ID) < 0
		})
	}
	type predecessor struct {
		from shoal.ID
		edge graph.Edge
	}
	previous := make(map[shoal.ID]predecessor)
	seen := map[shoal.ID]struct{}{from: {}}
	frontier := []shoal.ID{from}
	found := false
	for level := uint32(0); level < maxDepth && len(frontier) > 0 && !found; level++ {
		next := make([]shoal.ID, 0)
		for _, id := range frontier {
			for _, edge := range outgoing[id] {
				if _, ok := seen[edge.To]; ok {
					continue
				}
				seen[edge.To] = struct{}{}
				previous[edge.To] = predecessor{from: id, edge: edge}
				next = append(next, edge.To)
				if edge.To == to {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		frontier = next
	}
	if !found {
		return graph.Path{}, shoal.NewError(
			shoal.ErrorNotFound, "no directed path within the server bounds")
	}
	nodeIDs := []shoal.ID{to}
	pathEdges := make([]graph.Edge, 0)
	for current := to; current != from; {
		step := previous[current]
		pathEdges = append(pathEdges, step.edge)
		current = step.from
		nodeIDs = append(nodeIDs, current)
	}
	for left, right := 0, len(nodeIDs)-1; left < right; left, right = left+1, right-1 {
		nodeIDs[left], nodeIDs[right] = nodeIDs[right], nodeIDs[left]
	}
	for left, right := 0, len(pathEdges)-1; left < right; left, right = left+1, right-1 {
		pathEdges[left], pathEdges[right] = pathEdges[right], pathEdges[left]
	}
	path := graph.Path{Edges: pathEdges, Nodes: make([]graph.Node, 0, len(nodeIDs))}
	for _, id := range nodeIDs {
		path.Nodes = append(path.Nodes, nodes[id])
	}
	return path, nil
}

func assertionsForPath(
	path graph.Path,
	assertions []ontology.Assertion,
) []ontology.Assertion {
	if len(assertions) == 0 || len(path.Edges) == 0 {
		return nil
	}
	byEdge := make(map[shoal.ID]ontology.Assertion, len(assertions))
	for _, assertion := range assertions {
		edgeID := assertion.ID()
		// Extracted relationships retain their assertion identity separately
		// from the graph edge. Derived assertions use their own ID as the edge.
		if assertion.Origin() != ontology.AssertionDerived {
			if linked := assertion.Metadata()["shoal.graph.edge_id"]; linked != "" {
				edgeID = shoal.ID(linked)
			}
		}
		byEdge[edgeID] = assertion
	}
	selected := make([]ontology.Assertion, 0, len(path.Edges))
	for _, edge := range path.Edges {
		if assertion, ok := byEdge[edge.ID]; ok {
			selected = append(selected, assertion)
		}
	}
	return selected
}

var _ Service = (*EmbeddedService)(nil)
var _ ExtractionProvider = (*EmbeddedService)(nil)
var _ RecomputeProvider = (*EmbeddedService)(nil)
