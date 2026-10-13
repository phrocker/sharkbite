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

// Package webapi defines the transport-neutral Explorer workspace service and
// its standard-library HTTP transport.
package webapi

import (
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	DefaultPageSize      uint32 = 25
	MaxPageSize          uint32 = 100
	MaxTopK              uint32 = 50
	DefaultDepth         uint32 = 1
	MaxDepth             uint32 = 4
	DefaultFanout        uint32 = 12
	MaxFanout            uint32 = 50
	DefaultMaxNodes      uint32 = 100
	MaxNodes             uint32 = 250
	MaxEdgeTypes         uint32 = 64
	MaxEvidencePerResult uint32 = 32
	MaxResponseBytes     uint64 = 64 << 20
	MaxUploadFiles       uint32 = 8
	MaxUploadFileBytes   uint64 = 1 << 20
	MaxUploadTotalBytes  uint64 = 9 << 20
)

const (
	DefaultChangePageSize uint32 = 25
	MaxChangePageSize     uint32 = 100
)

// Capability identifies one browser-visible feature without revealing where or
// how the backing Explorer service executes it.
type Capability string

const (
	CapabilityDocuments     Capability = "documents"
	CapabilityDocument      Capability = "document"
	CapabilityRetrieve      Capability = "retrieve"
	CapabilityVector        Capability = "vector_retrieval"
	CapabilityNeighborhood  Capability = "neighborhood"
	CapabilityPath          Capability = "path"
	CapabilityIngest        Capability = "ingest"
	CapabilityExtraction    Capability = "extraction"
	CapabilityChanges       Capability = "changes"
	CapabilityAnalytics     Capability = "analytics"
	CapabilityDecisions     Capability = "decisions"
	CapabilityRegistrations Capability = "decision_registrations"
	CapabilityDatasets      Capability = "decision_datasets"
	CapabilityOutcomes      Capability = "decision_outcomes"
	CapabilityAdjudications Capability = "decision_adjudications"
)

// Capabilities advertises only stable logical features supported by a backend.
type Capabilities struct {
	Documents     bool `json:"documents"`
	Document      bool `json:"document"`
	Retrieve      bool `json:"retrieve"`
	Vector        bool `json:"vector_retrieval"`
	Neighborhood  bool `json:"neighborhood"`
	Path          bool `json:"path"`
	Ingest        bool `json:"ingest"`
	Extraction    bool `json:"extraction"`
	Changes       bool `json:"changes"`
	Analytics     bool `json:"analytics"`
	Decisions     bool `json:"decisions"`
	Registrations bool `json:"decision_registrations"`
	Datasets      bool `json:"decision_datasets"`
	Outcomes      bool `json:"decision_outcomes"`
	Adjudications bool `json:"decision_adjudications"`
}

// AllCapabilities returns the complete feature set implemented by the embedded
// service. Remote services must advertise their supported features explicitly.
func AllCapabilities() Capabilities {
	return Capabilities{
		Documents: true, Document: true, Retrieve: true,
		Vector: false, Neighborhood: true, Path: true, Ingest: true,
		Extraction: true, Changes: true, Analytics: false,
	}
}

// Supports reports whether the feature is currently available.
func (c Capabilities) Supports(capability Capability) bool {
	switch capability {
	case CapabilityDocuments:
		return c.Documents
	case CapabilityDocument:
		return c.Document
	case CapabilityRetrieve:
		return c.Retrieve
	case CapabilityVector:
		return c.Vector
	case CapabilityNeighborhood:
		return c.Neighborhood
	case CapabilityPath:
		return c.Path
	case CapabilityIngest:
		return c.Ingest
	case CapabilityExtraction:
		return c.Extraction
	case CapabilityChanges:
		return c.Changes
	case CapabilityAnalytics:
		return c.Analytics
	case CapabilityDecisions:
		return c.Decisions
	case CapabilityRegistrations:
		return c.Registrations
	case CapabilityDatasets:
		return c.Datasets
	case CapabilityOutcomes:
		return c.Outcomes
	case CapabilityAdjudications:
		return c.Adjudications
	default:
		return false
	}
}

// Snapshot pins every workspace request to one logical corpus view.
type Snapshot struct {
	ID       string    `json:"id"`
	AsOf     time.Time `json:"as_of"`
	Frontier uint64    `json:"frontier"`
}

// PageRequest carries bounded cursor pagination.
type PageRequest struct {
	Limit  uint32 `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// DocumentsRequest lists a page from a pinned corpus snapshot.
type DocumentsRequest struct {
	Snapshot Snapshot    `json:"snapshot,omitempty"`
	Page     PageRequest `json:"page,omitempty"`
}

// DocumentsResponse returns document summaries without exposing storage
// topology or implementation details.
type DocumentsResponse struct {
	Snapshot   Snapshot                   `json:"snapshot"`
	Documents  []explorer.DocumentSummary `json:"documents"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	// Suppressed reports how many current documents authorization withheld from
	// this identity. It is a count only and never names what was withheld.
	Suppressed uint32 `json:"suppressed,omitempty"`
	// Restricted reports how many current documents the mosaic co-occurrence
	// budget withheld from this identity. Unlike Suppressed, these are documents
	// the identity is individually authorized to read but that were withheld to
	// keep it within its distinct sensitivity-domain budget. It is a count only
	// and never names what was withheld, nor which domains were involved.
	Restricted uint32 `json:"restricted,omitempty"`
}

// DocumentRequest fetches one immutable hierarchy.
type DocumentRequest struct {
	Snapshot   Snapshot `json:"snapshot"`
	DocumentID shoal.ID `json:"document_id"`
	RevisionID shoal.ID `json:"revision_id,omitempty"`
}

// DocumentResponse contains the hierarchy and the snapshot that produced it.
type DocumentResponse struct {
	Snapshot Snapshot              `json:"snapshot"`
	Document explorer.DocumentView `json:"document"`
}

// RetrievalRequest runs a bounded retrieval plan.
type RetrievalRequest struct {
	Snapshot Snapshot          `json:"snapshot"`
	Query    retrieval.Request `json:"query"`
}

// RetrievalResponse preserves exact citations and explanations.
type RetrievalResponse struct {
	Snapshot  Snapshot           `json:"snapshot"`
	Retrieval retrieval.Response `json:"retrieval"`
	// Suppressed reports how many current documents authorization withheld from
	// this identity and therefore never searched. It is a count only and never
	// names what was withheld.
	Suppressed uint32 `json:"suppressed,omitempty"`
	// Restricted reports how many current documents the mosaic co-occurrence
	// budget withheld from this identity and therefore never searched. These are
	// documents the identity is individually authorized to read but that were
	// withheld to keep it within its distinct sensitivity-domain budget. It is a
	// count only and never names what was withheld, nor which domains.
	Restricted uint32 `json:"restricted,omitempty"`
	// Embedding reports only spaces in the caller-authorized candidate
	// projection. Space IDs are process-keyed opaque pseudonyms; provider/model
	// metadata and identities of suppressed spaces are never exposed.
	Embedding *authorized.EmbeddingQueryReport `json:"embedding,omitempty"`
}

// EmbeddingQueryError preserves an authorized embedding report on a failed
// retrieval. HTTP serializes the report beside the stable error code, and the
// remote service reconstructs this error so programmatic callers can inspect
// the same non-disclosing degradation signal.
type EmbeddingQueryError struct {
	err    error
	report authorized.EmbeddingQueryReport
}

func newEmbeddingQueryError(
	err error,
	report authorized.EmbeddingQueryReport,
) *EmbeddingQueryError {
	return &EmbeddingQueryError{err: err, report: report}
}

func (e *EmbeddingQueryError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *EmbeddingQueryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// EmbeddingQueryReport returns an independent report copy.
func (e *EmbeddingQueryError) EmbeddingQueryReport() authorized.EmbeddingQueryReport {
	if e == nil {
		return authorized.EmbeddingQueryReport{}
	}
	report := e.report
	report.Spaces = append([]authorized.EmbeddingSpaceReport(nil), report.Spaces...)
	return report
}

// NeighborhoodRequest asks for a bounded graph expansion.
type NeighborhoodRequest struct {
	Snapshot  Snapshot   `json:"snapshot"`
	NodeIDs   []shoal.ID `json:"node_ids"`
	Depth     uint32     `json:"depth,omitempty"`
	Fanout    uint32     `json:"fanout,omitempty"`
	MaxNodes  uint32     `json:"max_nodes,omitempty"`
	EdgeTypes []string   `json:"edge_types,omitempty"`
	Cursor    string     `json:"cursor,omitempty"`
}

// NeighborhoodResponse returns only the bounded subgraph requested.
type NeighborhoodResponse struct {
	Snapshot                Snapshot                       `json:"snapshot"`
	Neighborhood            explorer.Neighborhood          `json:"neighborhood"`
	OntologyInterpretations []OntologyInterpretationReport `json:"ontology_interpretations,omitempty"`
	Truncated               bool                           `json:"truncated"`
	NextCursor              string                         `json:"next_cursor,omitempty"`
	// ScannedEdges is decode-only for compatible peers. Public serialization
	// omits internal scan counts because filtered edges may be sensitive.
	ScannedEdges *uint32 `json:"scanned_edges,omitempty"`
}

type OntologyInterpretationReport struct {
	AssertionID         string                        `json:"assertion_id"`
	SchemaID            string                        `json:"schema_id"`
	VersionID           string                        `json:"version_id"`
	Reading             ontology.OntologyReading      `json:"reading"`
	Status              ontology.InterpretationStatus `json:"status"`
	OriginalSubjectType string                        `json:"original_subject_type,omitempty"`
	SubjectType         string                        `json:"subject_type,omitempty"`
	OriginalPredicate   string                        `json:"original_predicate"`
	Predicate           string                        `json:"predicate"`
	OriginalObjectType  string                        `json:"original_object_type,omitempty"`
	ObjectType          string                        `json:"object_type,omitempty"`
	AppliedMorphisms    []string                      `json:"applied_morphisms,omitempty"`
	Reason              string                        `json:"reason,omitempty"`
}

// PathRequest asks for one bounded directed path.
type PathRequest struct {
	Snapshot  Snapshot `json:"snapshot"`
	From      shoal.ID `json:"from"`
	To        shoal.ID `json:"to"`
	MaxDepth  uint32   `json:"max_depth,omitempty"`
	Fanout    uint32   `json:"fanout,omitempty"`
	EdgeTypes []string `json:"edge_types,omitempty"`
}

// PathResponse contains the selected explanation path.
type PathResponse struct {
	Snapshot                Snapshot                       `json:"snapshot"`
	Path                    graph.Path                     `json:"path"`
	Assertions              []ontology.Assertion           `json:"assertions,omitempty"`
	OntologyInterpretations []OntologyInterpretationReport `json:"ontology_interpretations,omitempty"`
}

// UploadFile carries one bounded, untrusted browser file after HTTP parsing.
type UploadFile struct {
	Name    string
	Content []byte
}

// IngestRequest carries one bounded browser upload batch.
type IngestRequest struct {
	Files []UploadFile
	// DeclassificationReason is an optional reason code for a relabel that
	// removes visibility labels (#630, #664). It is recorded on every
	// declassification audit the batch writes and satisfies an operator's
	// -require-declassification-reason. Empty means none; anything else must
	// pass authorized.ValidateDeclassificationReason, or the whole batch is
	// refused with ErrorInvalidArgument before any file is ingested.
	DeclassificationReason string `json:"declassification_reason,omitempty"`
}

// DeclassificationReasonField is the multipart form field, and the MCP ingest
// argument, that carries IngestRequest.DeclassificationReason.
const DeclassificationReasonField = "declassification_reason"

// IngestFileResult reports the durable outcome for one uploaded file.
type IngestFileResult struct {
	Name         string                     `json:"name"`
	MediaType    string                     `json:"media_type"`
	Disposition  explorer.IngestDisposition `json:"disposition"`
	Document     document.Document          `json:"document"`
	Revision     document.Revision          `json:"revision"`
	SectionCount int                        `json:"section_count"`
	SpanCount    int                        `json:"span_count"`
	SkillFile    *SkillFileResult           `json:"skill_file,omitempty"`
}

// SkillFileResult describes whether a markdown upload matches the agent skill
// file convention. It is upload metadata only; extraction remains separate.
type SkillFileResult struct {
	Expected    bool   `json:"expected"`
	Recognized  bool   `json:"recognized"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Message     string `json:"message"`
}

// IngestResponse returns the fresh snapshot after a successful ingest batch.
type IngestResponse struct {
	Snapshot Snapshot           `json:"snapshot"`
	Files    []IngestFileResult `json:"files"`
}

type ExtractRequest struct {
	Snapshot     Snapshot `json:"snapshot"`
	DocumentID   shoal.ID `json:"document_id"`
	RevisionID   shoal.ID `json:"revision_id,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
}

type ExtractResponse struct {
	Snapshot            Snapshot   `json:"snapshot"`
	DocumentID          shoal.ID   `json:"document_id"`
	RevisionID          shoal.ID   `json:"revision_id"`
	ExtractionID        shoal.ID   `json:"extraction_id"`
	EntityCount         int        `json:"entity_count"`
	RelationCount       int        `json:"relation_count"`
	GraphNodeCount      int        `json:"graph_node_count"`
	GraphEdgeCount      int        `json:"graph_edge_count"`
	CreatedEntities     int        `json:"created_entities"`
	ReusedEntities      int        `json:"reused_entities"`
	EntityNodeIDs       []shoal.ID `json:"entity_node_ids"`
	RelationshipEdgeIDs []shoal.ID `json:"relationship_edge_ids"`
}

// RecomputeDerivationRequest asks the workspace to re-run the deterministic
// derivation that produced a latent, similarity-derived ontology assertion. The
// caller passes AssertionID (the derived edge's ontology.assertion.id) and the
// Digest it currently displays. An empty Digest means the caller holds no prior
// digest yet, so the response captures a baseline instead of reporting drift.
type RecomputeDerivationRequest struct {
	Snapshot    Snapshot `json:"snapshot"`
	AssertionID shoal.ID `json:"assertion_id"`
	Digest      string   `json:"digest,omitempty"`
}

// RecomputeDerivationResponse returns the freshly re-derived derivation detail,
// a deterministic Digest over that detail, and whether re-running the
// derivation reproduced the caller's prior digest byte-for-byte. Unchanged is
// true when the caller's Digest matches the fresh Digest (or the caller held no
// prior digest); it is false when the inputs changed, and Detail then carries
// the new derivation so the caller can see exactly what changed.
type RecomputeDerivationResponse struct {
	Snapshot  Snapshot         `json:"snapshot"`
	Unchanged bool             `json:"unchanged"`
	Digest    string           `json:"digest"`
	Detail    DerivationDetail `json:"detail"`
}

// DerivationDetail describes how a latent similarity assertion was derived: the
// producer identity (embedding model and version, similarity metric, threshold,
// tessellation cell, iterator name and options), the similarity score, and the
// derivation and assertion identifiers.
type DerivationDetail struct {
	AssertionID           shoal.ID       `json:"assertion_id"`
	DerivationID          shoal.ID       `json:"derivation_id"`
	Origin                string         `json:"origin"`
	Score                 float64        `json:"score"`
	EmbeddingModel        string         `json:"embedding_model"`
	EmbeddingModelVersion string         `json:"embedding_model_version"`
	SimilarityMetric      string         `json:"similarity_metric"`
	Threshold             float64        `json:"threshold"`
	TessellationCell      string         `json:"tessellation_cell"`
	IteratorName          string         `json:"iterator_name"`
	IteratorOptions       shoal.Metadata `json:"iterator_options,omitempty"`
	Provider              string         `json:"provider"`
	Model                 string         `json:"model"`
	ModelVersion          string         `json:"model_version"`
}

// ChangesRequest asks for the caller's document change feed. An empty Cursor
// starts from the beginning of retained history. The cursor is opaque: it
// carries the caller's resume position and the corpus identity it was minted
// against, and never exposes a raw sequence number to the client.
type ChangesRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  uint32 `json:"limit,omitempty"`
}

// WorkspaceChange is one visible document publication reported by the feed. It
// carries the same summary shape the Documents listing returns, so the feed's
// disclosure surface is exactly that of the listing it mirrors.
type WorkspaceChange struct {
	Kind     string                   `json:"kind"`
	Document explorer.DocumentSummary `json:"document"`
}

// ChangesResponse returns an ordered, resumable window of visible changes.
// NextCursor is always present so a client always has a token to resume from,
// even when the window is empty. More is a bare liveness hint that a further
// visible change exists; it is never a count of withheld changes.
type ChangesResponse struct {
	Changes    []WorkspaceChange `json:"changes"`
	NextCursor string            `json:"next_cursor"`
	More       bool              `json:"more"`
}

// MetadataResponse advertises server-enforced public bounds.
type MetadataResponse struct {
	MaxPageSize                uint32            `json:"max_page_size"`
	MaxTopK                    uint32            `json:"max_top_k"`
	MaxDepth                   uint32            `json:"max_depth"`
	MaxFanout                  uint32            `json:"max_fanout"`
	MaxNodes                   uint32            `json:"max_nodes"`
	MaxEdgeTypes               uint32            `json:"max_edge_types"`
	MaxResponseBytes           uint64            `json:"max_response_bytes"`
	MaxUploadFiles             uint32            `json:"max_upload_files"`
	MaxUploadFileBytes         uint64            `json:"max_upload_file_bytes"`
	MaxUploadTotalBytes        uint64            `json:"max_upload_total_bytes"`
	AnalyticsLimits            *analytics.Limits `json:"analytics_limits,omitempty"`
	AnalyticsRecordingRequired bool              `json:"analytics_recording_required,omitempty"`
	Capabilities               Capabilities      `json:"capabilities"`
}
