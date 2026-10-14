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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type wireDocument struct {
	ID            string       `json:"id"`
	RevisionID    string       `json:"revision_id"`
	Title         string       `json:"title"`
	RootSectionID string       `json:"root_section_id"`
	Metadata      wireMetadata `json:"metadata,omitempty"`
}

type wireRevision struct {
	ID            string       `json:"id"`
	DocumentID    string       `json:"document_id"`
	CreatedAt     time.Time    `json:"created_at"`
	SourceVersion string       `json:"source_version"`
	Metadata      wireMetadata `json:"metadata,omitempty"`
}

type wireSection struct {
	ID         string       `json:"id"`
	DocumentID string       `json:"document_id"`
	RevisionID string       `json:"revision_id"`
	ParentID   string       `json:"parent_id,omitempty"`
	Order      uint32       `json:"order"`
	Heading    string       `json:"heading"`
	Range      wireRange    `json:"range"`
	Metadata   wireMetadata `json:"metadata,omitempty"`
}

type wireSpan struct {
	ID         string       `json:"id"`
	DocumentID string       `json:"document_id"`
	RevisionID string       `json:"revision_id"`
	SectionID  string       `json:"section_id"`
	Order      uint32       `json:"order"`
	Range      wireRange    `json:"range"`
	Text       string       `json:"text"`
	Metadata   wireMetadata `json:"metadata,omitempty"`
}

type wireSectionView struct {
	Section  wireSection       `json:"section"`
	Spans    []wireSpan        `json:"spans"`
	Children []wireSectionView `json:"children"`
}

type wireNode struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind,omitempty"`
	Labels     []string     `json:"labels,omitempty"`
	Properties wireMetadata `json:"properties,omitempty"`
}

type wireEdge struct {
	ID         string       `json:"id"`
	From       string       `json:"from"`
	To         string       `json:"to"`
	Type       string       `json:"type"`
	Weight     shoal.Score  `json:"weight"`
	Properties wireMetadata `json:"properties,omitempty"`
}

type wirePath struct {
	Nodes []wireNode `json:"nodes"`
	Edges []wireEdge `json:"edges"`
}

type wirePosition struct {
	Offset int64 `json:"offset"`
	Page   int32 `json:"page,omitempty"`
}

type wireRange struct {
	Start wirePosition `json:"start"`
	End   wirePosition `json:"end"`
}

type wireExplanation struct {
	Modes   []retrieval.Mode       `json:"modes"`
	Summary string                 `json:"summary"`
	Scores  map[string]shoal.Score `json:"scores"`
}

type wireCitation struct {
	DocumentID string    `json:"document_id"`
	RevisionID string    `json:"revision_id"`
	SectionID  string    `json:"section_id,omitempty"`
	SpanID     string    `json:"span_id,omitempty"`
	Range      wireRange `json:"range"`
}

type wireNeighborhood struct {
	Nodes      []wireNode      `json:"nodes"`
	Edges      []wireEdge      `json:"edges"`
	Assertions []wireAssertion `json:"assertions,omitempty"`
}

type wireAssertion struct {
	ID          string                   `json:"id"`
	Subject     string                   `json:"subject"`
	SubjectType string                   `json:"subject_type,omitempty"`
	Predicate   string                   `json:"predicate"`
	Object      wireOntologyValue        `json:"object"`
	ObjectType  string                   `json:"object_type,omitempty"`
	Origin      ontology.AssertionOrigin `json:"origin"`
	Confidence  shoal.Score              `json:"confidence"`
	Evidence    []wireEvidenceRef        `json:"evidence"`
	Provenance  wireExtractionProvenance `json:"provenance"`
	Ontology    *wireOntologyIdentity    `json:"ontology,omitempty"`
	Metadata    wireMetadata             `json:"metadata,omitempty"`
}

type wireOntologyValue struct {
	Type      ontology.ValueType `json:"type"`
	Text      string             `json:"text,omitempty"`
	Integer   int64              `json:"integer,omitempty"`
	Number    float64            `json:"number,omitempty"`
	Boolean   *bool              `json:"boolean,omitempty"`
	Timestamp time.Time          `json:"timestamp,omitempty"`
	Reference string             `json:"reference,omitempty"`
}

type wireEvidenceRef struct {
	ID         string                   `json:"id"`
	Citation   *wireCitation            `json:"citation,omitempty"`
	Quote      string                   `json:"quote,omitempty"`
	Path       *wirePath                `json:"path,omitempty"`
	Derivation *wireAssertionDerivation `json:"derivation,omitempty"`
	Metadata   wireMetadata             `json:"metadata,omitempty"`
}

type wireAssertionDerivation struct {
	ID                    string       `json:"id"`
	EmbeddingModel        string       `json:"embedding_model"`
	EmbeddingModelVersion string       `json:"embedding_model_version"`
	SimilarityMetric      string       `json:"similarity_metric"`
	Threshold             shoal.Score  `json:"threshold"`
	TessellationCell      string       `json:"tessellation_cell"`
	Score                 shoal.Score  `json:"score"`
	SourceEndpoint        string       `json:"source_endpoint"`
	TargetEndpoint        string       `json:"target_endpoint"`
	IteratorName          string       `json:"iterator_name"`
	IteratorOptions       wireMetadata `json:"iterator_options,omitempty"`
}

type wireDerivationDetail struct {
	AssertionID           string       `json:"assertion_id"`
	DerivationID          string       `json:"derivation_id"`
	Origin                string       `json:"origin"`
	Score                 float64      `json:"score"`
	EmbeddingModel        string       `json:"embedding_model"`
	EmbeddingModelVersion string       `json:"embedding_model_version"`
	SimilarityMetric      string       `json:"similarity_metric"`
	Threshold             float64      `json:"threshold"`
	TessellationCell      string       `json:"tessellation_cell"`
	IteratorName          string       `json:"iterator_name"`
	IteratorOptions       wireMetadata `json:"iterator_options,omitempty"`
	Provider              string       `json:"provider"`
	Model                 string       `json:"model"`
	ModelVersion          string       `json:"model_version"`
}

type wireExtractionProvenance struct {
	Provider         string       `json:"provider"`
	Model            string       `json:"model"`
	ModelVersion     string       `json:"model_version"`
	Prompt           string       `json:"prompt"`
	PromptVersion    string       `json:"prompt_version"`
	Extractor        string       `json:"extractor"`
	ExtractorVersion string       `json:"extractor_version"`
	Metadata         wireMetadata `json:"metadata,omitempty"`
}

type wireOntologyIdentity struct {
	SchemaID  string `json:"schema_id"`
	VersionID string `json:"version_id"`
}

type wireMetadataEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type wireMetadata []wireMetadataEntry

// MarshalJSON keeps the uint64 frontier exact in JavaScript clients.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID       string    `json:"id"`
		AsOf     time.Time `json:"as_of"`
		Frontier string    `json:"frontier"`
	}{s.ID, s.AsOf, strconv.FormatUint(s.Frontier, 10)})
}

// UnmarshalJSON parses the decimal frontier string without float conversion.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID       string    `json:"id"`
		AsOf     time.Time `json:"as_of"`
		Frontier string    `json:"frontier"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	var frontier uint64
	var err error
	if wire.Frontier != "" {
		frontier, err = strconv.ParseUint(wire.Frontier, 10, 64)
		if err != nil {
			return fmt.Errorf("frontier must be a decimal uint64 string")
		}
	}
	*s = Snapshot{ID: wire.ID, AsOf: wire.AsOf, Frontier: frontier}
	return nil
}

func (r DocumentRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot   Snapshot `json:"snapshot"`
		DocumentID string   `json:"document_id"`
		RevisionID string   `json:"revision_id,omitempty"`
	}{
		Snapshot: r.Snapshot, DocumentID: encodeID(r.DocumentID),
		RevisionID: encodeOptionalID(r.RevisionID),
	})
}

func (r RetrievalRequest) MarshalJSON() ([]byte, error) {
	documentIDs := make([]string, 0, len(r.Query.Scope.DocumentIDs))
	for _, id := range r.Query.Scope.DocumentIDs {
		documentIDs = append(documentIDs, encodeID(id))
	}
	nodeIDs := make([]string, 0, len(r.Query.Scope.NodeIDs))
	for _, id := range r.Query.Scope.NodeIDs {
		nodeIDs = append(nodeIDs, encodeID(id))
	}
	return json.Marshal(struct {
		Snapshot Snapshot `json:"snapshot"`
		Query    struct {
			Text  string           `json:"text"`
			TopK  uint32           `json:"top_k,omitempty"`
			Modes []retrieval.Mode `json:"modes,omitempty"`
			Scope struct {
				DocumentIDs []string `json:"document_ids,omitempty"`
				NodeIDs     []string `json:"node_ids,omitempty"`
			} `json:"scope,omitempty"`
			AsOf    time.Time `json:"as_of,omitempty"`
			Explain bool      `json:"explain,omitempty"`
		} `json:"query"`
	}{
		Snapshot: r.Snapshot,
		Query: struct {
			Text  string           `json:"text"`
			TopK  uint32           `json:"top_k,omitempty"`
			Modes []retrieval.Mode `json:"modes,omitempty"`
			Scope struct {
				DocumentIDs []string `json:"document_ids,omitempty"`
				NodeIDs     []string `json:"node_ids,omitempty"`
			} `json:"scope,omitempty"`
			AsOf    time.Time `json:"as_of,omitempty"`
			Explain bool      `json:"explain,omitempty"`
		}{
			Text: r.Query.Text, TopK: r.Query.TopK, Modes: r.Query.Modes,
			Scope: struct {
				DocumentIDs []string `json:"document_ids,omitempty"`
				NodeIDs     []string `json:"node_ids,omitempty"`
			}{DocumentIDs: documentIDs, NodeIDs: nodeIDs},
			AsOf: r.Query.AsOf, Explain: r.Query.Explain,
		},
	})
}

func (r NeighborhoodRequest) MarshalJSON() ([]byte, error) {
	nodeIDs := make([]string, 0, len(r.NodeIDs))
	for _, id := range r.NodeIDs {
		nodeIDs = append(nodeIDs, encodeID(id))
	}
	return json.Marshal(struct {
		Snapshot  Snapshot `json:"snapshot"`
		NodeIDs   []string `json:"node_ids"`
		Depth     uint32   `json:"depth,omitempty"`
		Fanout    uint32   `json:"fanout,omitempty"`
		MaxNodes  uint32   `json:"max_nodes,omitempty"`
		EdgeTypes []string `json:"edge_types,omitempty"`
		Cursor    string   `json:"cursor,omitempty"`
	}{
		Snapshot: r.Snapshot, NodeIDs: nodeIDs, Depth: r.Depth,
		Fanout: r.Fanout, MaxNodes: r.MaxNodes, EdgeTypes: r.EdgeTypes,
		Cursor: r.Cursor,
	})
}

func (r PathRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot  Snapshot `json:"snapshot"`
		From      string   `json:"from"`
		To        string   `json:"to"`
		MaxDepth  uint32   `json:"max_depth,omitempty"`
		Fanout    uint32   `json:"fanout,omitempty"`
		EdgeTypes []string `json:"edge_types,omitempty"`
	}{
		Snapshot: r.Snapshot, From: encodeID(r.From), To: encodeID(r.To),
		MaxDepth: r.MaxDepth, Fanout: r.Fanout, EdgeTypes: r.EdgeTypes,
	})
}

func (r DocumentsResponse) MarshalJSON() ([]byte, error) {
	documents := make([]any, 0, len(r.Documents))
	for _, summary := range r.Documents {
		documents = append(documents, struct {
			Document        wireDocument `json:"document"`
			Revision        wireRevision `json:"revision"`
			SourceURI       string       `json:"source_uri"`
			SourceMediaType string       `json:"source_media_type,omitempty"`
		}{
			Document: wireDocumentValue(summary.Document),
			Revision: wireRevisionValue(summary.Revision), SourceURI: summary.SourceURI,
			SourceMediaType: summary.SourceMediaType,
		})
	}
	return json.Marshal(struct {
		Snapshot   Snapshot `json:"snapshot"`
		Documents  []any    `json:"documents"`
		NextCursor string   `json:"next_cursor,omitempty"`
		Suppressed uint32   `json:"suppressed,omitempty"`
		Restricted uint32   `json:"restricted,omitempty"`
	}{r.Snapshot, documents, r.NextCursor, r.Suppressed, r.Restricted})
}

func (r *DocumentsResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot  Snapshot `json:"snapshot"`
		Documents []struct {
			Document        wireDocument `json:"document"`
			Revision        wireRevision `json:"revision"`
			SourceURI       string       `json:"source_uri"`
			SourceMediaType string       `json:"source_media_type,omitempty"`
		} `json:"documents"`
		NextCursor string `json:"next_cursor,omitempty"`
		Suppressed uint32 `json:"suppressed,omitempty"`
		Restricted uint32 `json:"restricted,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	documents := make([]explorer.DocumentSummary, 0, len(wire.Documents))
	for _, item := range wire.Documents {
		documentValue, err := documentValue(item.Document)
		if err != nil {
			return fmt.Errorf("documents.document: %w", err)
		}
		revisionValue, err := revisionValue(item.Revision)
		if err != nil {
			return fmt.Errorf("documents.revision: %w", err)
		}
		documents = append(documents, explorer.DocumentSummary{
			Document: documentValue, Revision: revisionValue, SourceURI: item.SourceURI,
			SourceMediaType: item.SourceMediaType,
		})
	}
	*r = DocumentsResponse{
		Snapshot: wire.Snapshot, Documents: documents, NextCursor: wire.NextCursor,
		Suppressed: wire.Suppressed, Restricted: wire.Restricted,
	}
	return nil
}

func (r ChangesResponse) MarshalJSON() ([]byte, error) {
	changes := make([]any, 0, len(r.Changes))
	for _, change := range r.Changes {
		changes = append(changes, struct {
			Kind            string       `json:"kind"`
			Document        wireDocument `json:"document"`
			Revision        wireRevision `json:"revision"`
			SourceURI       string       `json:"source_uri"`
			SourceMediaType string       `json:"source_media_type,omitempty"`
		}{
			Kind:            change.Kind,
			Document:        wireDocumentValue(change.Document.Document),
			Revision:        wireRevisionValue(change.Document.Revision),
			SourceURI:       change.Document.SourceURI,
			SourceMediaType: change.Document.SourceMediaType,
		})
	}
	return json.Marshal(struct {
		Changes    []any  `json:"changes"`
		NextCursor string `json:"next_cursor"`
		More       bool   `json:"more"`
	}{changes, r.NextCursor, r.More})
}

func (r *ChangesResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Changes []struct {
			Kind            string       `json:"kind"`
			Document        wireDocument `json:"document"`
			Revision        wireRevision `json:"revision"`
			SourceURI       string       `json:"source_uri"`
			SourceMediaType string       `json:"source_media_type,omitempty"`
		} `json:"changes"`
		NextCursor string `json:"next_cursor"`
		More       bool   `json:"more"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	changes := make([]WorkspaceChange, 0, len(wire.Changes))
	for _, item := range wire.Changes {
		documentValue, err := documentValue(item.Document)
		if err != nil {
			return fmt.Errorf("changes.document: %w", err)
		}
		revisionValue, err := revisionValue(item.Revision)
		if err != nil {
			return fmt.Errorf("changes.revision: %w", err)
		}
		changes = append(changes, WorkspaceChange{
			Kind: item.Kind,
			Document: explorer.DocumentSummary{
				Document: documentValue, Revision: revisionValue,
				SourceURI: item.SourceURI, SourceMediaType: item.SourceMediaType,
			},
		})
	}
	*r = ChangesResponse{
		Changes: changes, NextCursor: wire.NextCursor, More: wire.More,
	}
	return nil
}

func (r DocumentResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot Snapshot `json:"snapshot"`
		Document any      `json:"document"`
	}{r.Snapshot, struct {
		Document        wireDocument    `json:"document"`
		Revision        wireRevision    `json:"revision"`
		SourceURI       string          `json:"source_uri"`
		SourceMediaType string          `json:"source_media_type,omitempty"`
		Root            wireSectionView `json:"root"`
	}{
		Document:  wireDocumentValue(r.Document.Document),
		Revision:  wireRevisionValue(r.Document.Revision),
		SourceURI: r.Document.SourceURI, SourceMediaType: r.Document.SourceMediaType,
		Root: wireSectionViewValue(r.Document.Root),
	}})
}

func (r *DocumentResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot Snapshot `json:"snapshot"`
		Document struct {
			Document        wireDocument    `json:"document"`
			Revision        wireRevision    `json:"revision"`
			SourceURI       string          `json:"source_uri"`
			SourceMediaType string          `json:"source_media_type,omitempty"`
			Root            wireSectionView `json:"root"`
		} `json:"document"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	documentValue, err := documentValue(wire.Document.Document)
	if err != nil {
		return fmt.Errorf("document.document: %w", err)
	}
	revisionValue, err := revisionValue(wire.Document.Revision)
	if err != nil {
		return fmt.Errorf("document.revision: %w", err)
	}
	root, err := sectionViewValue(wire.Document.Root)
	if err != nil {
		return fmt.Errorf("document.root: %w", err)
	}
	*r = DocumentResponse{
		Snapshot: wire.Snapshot,
		Document: explorer.DocumentView{
			Document: documentValue, Revision: revisionValue,
			SourceURI: wire.Document.SourceURI, SourceMediaType: wire.Document.SourceMediaType,
			Root: root,
		},
	}
	return nil
}

func (r RetrievalResponse) MarshalJSON() ([]byte, error) {
	results := make([]any, 0, len(r.Retrieval.Results))
	embeddingSpaceIDs := make(
		[]string, len(r.Retrieval.EmbeddingSpaceIDs))
	for index, id := range r.Retrieval.EmbeddingSpaceIDs {
		embeddingSpaceIDs[index] = encodeID(id)
	}
	for _, result := range r.Retrieval.Results {
		evidence := make([]any, 0, len(result.Evidence))
		for _, item := range result.Evidence {
			evidence = append(evidence, struct {
				Citation any         `json:"citation"`
				Quote    string      `json:"quote"`
				Path     wirePath    `json:"path"`
				Score    shoal.Score `json:"score"`
			}{
				Citation: wireCitationValue(item.Citation), Quote: item.Quote,
				Path: wirePathValue(item.Path), Score: item.Score,
			})
		}
		results = append(results, struct {
			ID          string           `json:"id"`
			Score       shoal.Score      `json:"score"`
			Evidence    []any            `json:"evidence"`
			Explanation *wireExplanation `json:"explanation,omitempty"`
		}{
			encodeID(result.ID), result.Score, evidence,
			wireExplanationValue(result.Explanation),
		})
	}
	return json.Marshal(struct {
		Snapshot   Snapshot                  `json:"snapshot"`
		Retrieval  any                       `json:"retrieval"`
		Suppressed uint32                    `json:"suppressed,omitempty"`
		Restricted uint32                    `json:"restricted,omitempty"`
		Embedding  *wireEmbeddingQueryReport `json:"embedding,omitempty"`
	}{r.Snapshot, struct {
		RequestID         string   `json:"request_id,omitempty"`
		EmbeddingSpaceID  string   `json:"embedding_space_id,omitempty"`
		EmbeddingSpaceIDs []string `json:"embedding_space_ids,omitempty"`
		Results           []any    `json:"results"`
	}{
		encodeOptionalID(r.Retrieval.RequestID),
		encodeOptionalID(r.Retrieval.EmbeddingSpaceID),
		embeddingSpaceIDs,
		results,
	}, r.Suppressed, r.Restricted, wireEmbeddingQueryReportValue(r.Embedding)})
}

func (r *RetrievalResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot  Snapshot `json:"snapshot"`
		Retrieval struct {
			RequestID         string   `json:"request_id,omitempty"`
			EmbeddingSpaceID  string   `json:"embedding_space_id,omitempty"`
			EmbeddingSpaceIDs []string `json:"embedding_space_ids,omitempty"`
			Results           []struct {
				ID       string      `json:"id"`
				Score    shoal.Score `json:"score"`
				Evidence []struct {
					Citation wireCitation `json:"citation"`
					Quote    string       `json:"quote"`
					Path     wirePath     `json:"path"`
					Score    shoal.Score  `json:"score"`
				} `json:"evidence"`
				Explanation *wireExplanation `json:"explanation,omitempty"`
			} `json:"results"`
		} `json:"retrieval"`
		Suppressed uint32                    `json:"suppressed,omitempty"`
		Restricted uint32                    `json:"restricted,omitempty"`
		Embedding  *wireEmbeddingQueryReport `json:"embedding,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	requestID, err := decodeOptionalID(wire.Retrieval.RequestID)
	if err != nil {
		return fmt.Errorf("retrieval.request_id: %w", err)
	}
	embeddingSpaceID, err := decodeOptionalID(
		wire.Retrieval.EmbeddingSpaceID)
	if err != nil {
		return fmt.Errorf("retrieval.embedding_space_id: %w", err)
	}
	embeddingSpaceIDs := make(
		[]shoal.ID, len(wire.Retrieval.EmbeddingSpaceIDs))
	for index, encoded := range wire.Retrieval.EmbeddingSpaceIDs {
		embeddingSpaceIDs[index], err = decodeID(encoded)
		if err != nil {
			return fmt.Errorf(
				"retrieval.embedding_space_ids: %w", err)
		}
	}
	results := make([]retrieval.Result, 0, len(wire.Retrieval.Results))
	for _, item := range wire.Retrieval.Results {
		id, err := decodeID(item.ID)
		if err != nil {
			return fmt.Errorf("retrieval.results.id: %w", err)
		}
		evidence := make([]retrieval.Evidence, 0, len(item.Evidence))
		for _, evidenceItem := range item.Evidence {
			citation, err := citationValue(evidenceItem.Citation)
			if err != nil {
				return fmt.Errorf("retrieval.results.evidence.citation: %w", err)
			}
			path, err := pathValue(evidenceItem.Path)
			if err != nil {
				return fmt.Errorf("retrieval.results.evidence.path: %w", err)
			}
			evidence = append(evidence, retrieval.Evidence{
				Citation: citation, Quote: evidenceItem.Quote,
				Path: path, Score: evidenceItem.Score,
			})
		}
		results = append(results, retrieval.Result{
			ID: id, Score: item.Score, Evidence: evidence,
			Explanation: explanationValue(item.Explanation),
		})
	}
	embedding, err := embeddingQueryReportValue(wire.Embedding)
	if err != nil {
		return fmt.Errorf("embedding: %w", err)
	}
	if embedding != nil {
		if embedding.Suppressed != (wire.Suppressed > 0) {
			return fmt.Errorf(
				"embedding: suppressed flag does not match suppressed count")
		}
		if embedding.Restricted != (wire.Restricted > 0) {
			return fmt.Errorf(
				"embedding: restricted flag does not match restricted count")
		}
	}
	*r = RetrievalResponse{
		Snapshot: wire.Snapshot,
		Retrieval: retrieval.Response{
			RequestID:         requestID,
			EmbeddingSpaceID:  embeddingSpaceID,
			EmbeddingSpaceIDs: embeddingSpaceIDs,
			Results:           results,
		},
		Suppressed: wire.Suppressed,
		Restricted: wire.Restricted,
		Embedding:  embedding,
	}
	return nil
}

type wireEmbeddingQueryReport struct {
	Spaces         []wireEmbeddingSpace `json:"spaces"`
	FanoutLimit    uint32               `json:"fanout_limit,omitempty"`
	CacheHits      uint32               `json:"cache_hits,omitempty"`
	ProviderCalls  uint32               `json:"provider_calls,omitempty"`
	Observed       bool                 `json:"observed"`
	Suppressed     bool                 `json:"suppressed,omitempty"`
	Restricted     bool                 `json:"restricted,omitempty"`
	Degraded       bool                 `json:"degraded,omitempty"`
	FanoutExceeded bool                 `json:"fanout_exceeded,omitempty"`
}

type wireEmbeddingSpace struct {
	ID     string                          `json:"id"`
	Status authorized.EmbeddingSpaceStatus `json:"status"`
}

func wireEmbeddingQueryReportValue(
	report *authorized.EmbeddingQueryReport,
) *wireEmbeddingQueryReport {
	if report == nil {
		return nil
	}
	spaces := make([]wireEmbeddingSpace, 0, len(report.Spaces))
	for _, space := range report.Spaces {
		spaces = append(spaces, wireEmbeddingSpace{
			ID: encodeID(space.ID), Status: space.Status,
		})
	}
	return &wireEmbeddingQueryReport{
		Spaces: spaces, FanoutLimit: report.FanoutLimit,
		Observed: report.Observed, Suppressed: report.Suppressed,
		Restricted: report.Restricted, Degraded: report.Degraded,
		FanoutExceeded: report.FanoutExceeded,
	}
}

func embeddingQueryReportValue(
	wire *wireEmbeddingQueryReport,
) (*authorized.EmbeddingQueryReport, error) {
	if wire == nil {
		return nil, nil
	}
	if wire.CacheHits != 0 || wire.ProviderCalls != 0 {
		return nil, fmt.Errorf(
			"embedding cache/provider counters are operator-only")
	}
	report := &authorized.EmbeddingQueryReport{
		FanoutLimit: wire.FanoutLimit,
		Observed:    wire.Observed, Suppressed: wire.Suppressed,
		Restricted: wire.Restricted, Degraded: wire.Degraded,
		FanoutExceeded: wire.FanoutExceeded,
	}
	seen := make(map[shoal.ID]struct{}, len(wire.Spaces))
	for _, item := range wire.Spaces {
		id, err := decodeID(item.ID)
		if err != nil {
			return nil, fmt.Errorf("spaces.id: %w", err)
		}
		if err := shoal.ValidateRequiredID("spaces.id", id); err != nil {
			return nil, err
		}
		switch item.Status {
		case authorized.EmbeddingSpaceAvailable,
			authorized.EmbeddingSpaceUnavailable,
			authorized.EmbeddingSpaceNotAttempted,
			authorized.EmbeddingSpaceNotCompleted:
		default:
			return nil, fmt.Errorf("spaces.status: unknown status")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("spaces.id: duplicate identifier")
		}
		seen[id] = struct{}{}
		report.Spaces = append(report.Spaces, authorized.EmbeddingSpaceReport{
			ID: id, Status: item.Status,
		})
	}
	if !report.Observed &&
		(len(report.Spaces) > 0 ||
			report.CacheHits > 0 ||
			report.ProviderCalls > 0 ||
			report.FanoutExceeded) {
		return nil, fmt.Errorf(
			"embedding activity requires observed embedding activity")
	}
	if report.FanoutExceeded && !report.Degraded {
		return nil, fmt.Errorf("fanout_exceeded requires degraded")
	}
	for _, space := range report.Spaces {
		if space.Status != authorized.EmbeddingSpaceAvailable &&
			!report.Degraded {
			return nil, fmt.Errorf("non-available space requires degraded")
		}
	}
	return report, nil
}

func (r NeighborhoodResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot                Snapshot                       `json:"snapshot"`
		Neighborhood            any                            `json:"neighborhood"`
		OntologyInterpretations []OntologyInterpretationReport `json:"ontology_interpretations,omitempty"`
		Truncated               bool                           `json:"truncated"`
		NextCursor              string                         `json:"next_cursor,omitempty"`
	}{r.Snapshot, wireNeighborhoodValue(r.Neighborhood), r.OntologyInterpretations,
		r.Truncated, r.NextCursor})
}

func (r *NeighborhoodResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot                Snapshot                       `json:"snapshot"`
		Neighborhood            wireNeighborhood               `json:"neighborhood"`
		OntologyInterpretations []OntologyInterpretationReport `json:"ontology_interpretations,omitempty"`
		Truncated               bool                           `json:"truncated"`
		NextCursor              string                         `json:"next_cursor,omitempty"`
		ScannedEdges            *uint32                        `json:"scanned_edges,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	neighborhood, err := neighborhoodValue(wire.Neighborhood)
	if err != nil {
		return err
	}
	*r = NeighborhoodResponse{
		Snapshot: wire.Snapshot, Neighborhood: neighborhood,
		OntologyInterpretations: wire.OntologyInterpretations,
		Truncated:               wire.Truncated, NextCursor: wire.NextCursor,
		ScannedEdges: cloneUint32(wire.ScannedEdges),
	}
	return nil
}

func (r PathResponse) MarshalJSON() ([]byte, error) {
	assertions := make([]wireAssertion, 0, len(r.Assertions))
	for _, assertion := range r.Assertions {
		assertions = append(assertions, wireAssertionValue(assertion))
	}
	return json.Marshal(struct {
		Snapshot                Snapshot                       `json:"snapshot"`
		Path                    wirePath                       `json:"path"`
		Assertions              []wireAssertion                `json:"assertions,omitempty"`
		OntologyInterpretations []OntologyInterpretationReport `json:"ontology_interpretations,omitempty"`
	}{r.Snapshot, wirePathValue(r.Path), assertions, r.OntologyInterpretations})
}

func (r *PathResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot                Snapshot                       `json:"snapshot"`
		Path                    wirePath                       `json:"path"`
		Assertions              []wireAssertion                `json:"assertions,omitempty"`
		OntologyInterpretations []OntologyInterpretationReport `json:"ontology_interpretations,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	path, err := pathValue(wire.Path)
	if err != nil {
		return err
	}
	assertions := make([]ontology.Assertion, 0, len(wire.Assertions))
	for _, item := range wire.Assertions {
		assertion, err := assertionValue(item)
		if err != nil {
			return err
		}
		assertions = append(assertions, assertion)
	}
	*r = PathResponse{
		Snapshot: wire.Snapshot, Path: path, Assertions: assertions,
		OntologyInterpretations: wire.OntologyInterpretations,
	}
	return nil
}

func (r IngestResponse) MarshalJSON() ([]byte, error) {
	files := make([]any, 0, len(r.Files))
	for _, file := range r.Files {
		files = append(files, struct {
			Name         string                     `json:"name"`
			MediaType    string                     `json:"media_type"`
			Disposition  explorer.IngestDisposition `json:"disposition"`
			Document     wireDocument               `json:"document"`
			Revision     wireRevision               `json:"revision"`
			SectionCount int                        `json:"section_count"`
			SpanCount    int                        `json:"span_count"`
			SkillFile    *SkillFileResult           `json:"skill_file,omitempty"`
			NameNotice   *IngestNameNotice          `json:"name_notice,omitempty"`
		}{
			Name: file.Name, MediaType: file.MediaType, Disposition: file.Disposition,
			Document:     wireDocumentValue(file.Document),
			Revision:     wireRevisionValue(file.Revision),
			SectionCount: file.SectionCount, SpanCount: file.SpanCount,
			SkillFile: file.SkillFile, NameNotice: file.NameNotice,
		})
	}
	return json.Marshal(struct {
		Snapshot Snapshot `json:"snapshot"`
		Files    []any    `json:"files"`
	}{r.Snapshot, files})
}

func (r *IngestResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot Snapshot `json:"snapshot"`
		Files    []struct {
			Name         string                     `json:"name"`
			MediaType    string                     `json:"media_type"`
			Disposition  explorer.IngestDisposition `json:"disposition"`
			Document     wireDocument               `json:"document"`
			Revision     wireRevision               `json:"revision"`
			SectionCount int                        `json:"section_count"`
			SpanCount    int                        `json:"span_count"`
			SkillFile    *SkillFileResult           `json:"skill_file,omitempty"`
			NameNotice   *IngestNameNotice          `json:"name_notice,omitempty"`
		} `json:"files"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	files := make([]IngestFileResult, 0, len(wire.Files))
	for _, item := range wire.Files {
		documentValue, err := documentValue(item.Document)
		if err != nil {
			return fmt.Errorf("ingest.files.document: %w", err)
		}
		revisionValue, err := revisionValue(item.Revision)
		if err != nil {
			return fmt.Errorf("ingest.files.revision: %w", err)
		}
		files = append(files, IngestFileResult{
			Name: item.Name, MediaType: item.MediaType, Disposition: item.Disposition,
			Document: documentValue, Revision: revisionValue,
			SectionCount: item.SectionCount, SpanCount: item.SpanCount,
			SkillFile: item.SkillFile, NameNotice: item.NameNotice,
		})
	}
	*r = IngestResponse{Snapshot: wire.Snapshot, Files: files}
	return nil
}

func (r *DocumentRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot   Snapshot `json:"snapshot"`
		DocumentID string   `json:"document_id"`
		RevisionID string   `json:"revision_id,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	documentID, err := decodeID(wire.DocumentID)
	if err != nil {
		return fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeOptionalID(wire.RevisionID)
	if err != nil {
		return fmt.Errorf("revision_id: %w", err)
	}
	*r = DocumentRequest{
		Snapshot: wire.Snapshot, DocumentID: documentID, RevisionID: revisionID,
	}
	return nil
}

func (r ExtractRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot     Snapshot `json:"snapshot"`
		DocumentID   string   `json:"document_id"`
		RevisionID   string   `json:"revision_id,omitempty"`
		Instructions string   `json:"instructions,omitempty"`
	}{
		Snapshot: r.Snapshot, DocumentID: encodeID(r.DocumentID),
		RevisionID: encodeOptionalID(r.RevisionID), Instructions: r.Instructions,
	})
}

func (r *ExtractRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot     Snapshot `json:"snapshot"`
		DocumentID   string   `json:"document_id"`
		RevisionID   string   `json:"revision_id,omitempty"`
		Instructions string   `json:"instructions,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	documentID, err := decodeID(wire.DocumentID)
	if err != nil {
		return fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeOptionalID(wire.RevisionID)
	if err != nil {
		return fmt.Errorf("revision_id: %w", err)
	}
	*r = ExtractRequest{
		Snapshot: wire.Snapshot, DocumentID: documentID, RevisionID: revisionID,
		Instructions: wire.Instructions,
	}
	return nil
}

func (r ExtractResponse) MarshalJSON() ([]byte, error) {
	entityNodeIDs := make([]string, 0, len(r.EntityNodeIDs))
	for _, id := range r.EntityNodeIDs {
		entityNodeIDs = append(entityNodeIDs, encodeID(id))
	}
	relationshipEdgeIDs := make([]string, 0, len(r.RelationshipEdgeIDs))
	for _, id := range r.RelationshipEdgeIDs {
		relationshipEdgeIDs = append(relationshipEdgeIDs, encodeID(id))
	}
	return json.Marshal(struct {
		Snapshot            Snapshot `json:"snapshot"`
		DocumentID          string   `json:"document_id"`
		RevisionID          string   `json:"revision_id"`
		ExtractionID        string   `json:"extraction_id"`
		EntityCount         int      `json:"entity_count"`
		RelationCount       int      `json:"relation_count"`
		GraphNodeCount      int      `json:"graph_node_count"`
		GraphEdgeCount      int      `json:"graph_edge_count"`
		CreatedEntities     int      `json:"created_entities"`
		ReusedEntities      int      `json:"reused_entities"`
		EntityNodeIDs       []string `json:"entity_node_ids"`
		RelationshipEdgeIDs []string `json:"relationship_edge_ids"`
	}{
		Snapshot:            r.Snapshot,
		DocumentID:          encodeID(r.DocumentID),
		RevisionID:          encodeOptionalID(r.RevisionID),
		ExtractionID:        encodeOptionalID(r.ExtractionID),
		EntityCount:         r.EntityCount,
		RelationCount:       r.RelationCount,
		GraphNodeCount:      r.GraphNodeCount,
		GraphEdgeCount:      r.GraphEdgeCount,
		CreatedEntities:     r.CreatedEntities,
		ReusedEntities:      r.ReusedEntities,
		EntityNodeIDs:       entityNodeIDs,
		RelationshipEdgeIDs: relationshipEdgeIDs,
	})
}

func (r *ExtractResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot            Snapshot `json:"snapshot"`
		DocumentID          string   `json:"document_id"`
		RevisionID          string   `json:"revision_id"`
		ExtractionID        string   `json:"extraction_id"`
		EntityCount         int      `json:"entity_count"`
		RelationCount       int      `json:"relation_count"`
		GraphNodeCount      int      `json:"graph_node_count"`
		GraphEdgeCount      int      `json:"graph_edge_count"`
		CreatedEntities     int      `json:"created_entities"`
		ReusedEntities      int      `json:"reused_entities"`
		EntityNodeIDs       []string `json:"entity_node_ids"`
		RelationshipEdgeIDs []string `json:"relationship_edge_ids"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	documentID, err := decodeID(wire.DocumentID)
	if err != nil {
		return fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeOptionalID(wire.RevisionID)
	if err != nil {
		return fmt.Errorf("revision_id: %w", err)
	}
	extractionID, err := decodeOptionalID(wire.ExtractionID)
	if err != nil {
		return fmt.Errorf("extraction_id: %w", err)
	}
	entityNodeIDs, err := decodeIDs(wire.EntityNodeIDs)
	if err != nil {
		return fmt.Errorf("entity_node_ids: %w", err)
	}
	relationshipEdgeIDs, err := decodeIDs(wire.RelationshipEdgeIDs)
	if err != nil {
		return fmt.Errorf("relationship_edge_ids: %w", err)
	}
	*r = ExtractResponse{
		Snapshot: wire.Snapshot, DocumentID: documentID, RevisionID: revisionID,
		ExtractionID: extractionID, EntityCount: wire.EntityCount,
		RelationCount: wire.RelationCount, GraphNodeCount: wire.GraphNodeCount,
		GraphEdgeCount: wire.GraphEdgeCount, CreatedEntities: wire.CreatedEntities,
		ReusedEntities: wire.ReusedEntities, EntityNodeIDs: entityNodeIDs,
		RelationshipEdgeIDs: relationshipEdgeIDs,
	}
	return nil
}

func (r RecomputeDerivationRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot    Snapshot `json:"snapshot"`
		AssertionID string   `json:"assertion_id"`
		Digest      string   `json:"digest,omitempty"`
	}{
		Snapshot: r.Snapshot, AssertionID: encodeID(r.AssertionID), Digest: r.Digest,
	})
}

func (r *RecomputeDerivationRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot    Snapshot `json:"snapshot"`
		AssertionID string   `json:"assertion_id"`
		Digest      string   `json:"digest,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	assertionID, err := decodeID(wire.AssertionID)
	if err != nil {
		return fmt.Errorf("assertion_id: %w", err)
	}
	*r = RecomputeDerivationRequest{
		Snapshot: wire.Snapshot, AssertionID: assertionID, Digest: wire.Digest,
	}
	return nil
}

func (r RecomputeDerivationResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Snapshot  Snapshot             `json:"snapshot"`
		Unchanged bool                 `json:"unchanged"`
		Digest    string               `json:"digest"`
		Detail    wireDerivationDetail `json:"detail"`
	}{
		Snapshot: r.Snapshot, Unchanged: r.Unchanged, Digest: r.Digest,
		Detail: wireDerivationDetailValue(r.Detail),
	})
}

func (r *RecomputeDerivationResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot  Snapshot             `json:"snapshot"`
		Unchanged bool                 `json:"unchanged"`
		Digest    string               `json:"digest"`
		Detail    wireDerivationDetail `json:"detail"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	detail, err := derivationDetailValue(wire.Detail)
	if err != nil {
		return fmt.Errorf("detail: %w", err)
	}
	*r = RecomputeDerivationResponse{
		Snapshot: wire.Snapshot, Unchanged: wire.Unchanged, Digest: wire.Digest,
		Detail: detail,
	}
	return nil
}

func (r *NeighborhoodRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot  Snapshot `json:"snapshot"`
		NodeIDs   []string `json:"node_ids"`
		Depth     uint32   `json:"depth,omitempty"`
		Fanout    uint32   `json:"fanout,omitempty"`
		MaxNodes  uint32   `json:"max_nodes,omitempty"`
		EdgeTypes []string `json:"edge_types,omitempty"`
		Cursor    string   `json:"cursor,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	nodeIDs, err := decodeIDs(wire.NodeIDs)
	if err != nil {
		return fmt.Errorf("node_ids: %w", err)
	}
	*r = NeighborhoodRequest{
		Snapshot: wire.Snapshot, NodeIDs: nodeIDs, Depth: wire.Depth,
		Fanout: wire.Fanout, MaxNodes: wire.MaxNodes, EdgeTypes: wire.EdgeTypes,
		Cursor: wire.Cursor,
	}
	return nil
}

func (r *PathRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot  Snapshot `json:"snapshot"`
		From      string   `json:"from"`
		To        string   `json:"to"`
		MaxDepth  uint32   `json:"max_depth,omitempty"`
		Fanout    uint32   `json:"fanout,omitempty"`
		EdgeTypes []string `json:"edge_types,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	from, err := decodeID(wire.From)
	if err != nil {
		return fmt.Errorf("from: %w", err)
	}
	to, err := decodeID(wire.To)
	if err != nil {
		return fmt.Errorf("to: %w", err)
	}
	*r = PathRequest{
		Snapshot: wire.Snapshot, From: from, To: to, MaxDepth: wire.MaxDepth,
		Fanout: wire.Fanout, EdgeTypes: wire.EdgeTypes,
	}
	return nil
}

func (r *RetrievalRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot Snapshot `json:"snapshot"`
		Query    struct {
			Text  string           `json:"text"`
			TopK  uint32           `json:"top_k,omitempty"`
			Modes []retrieval.Mode `json:"modes,omitempty"`
			Scope struct {
				DocumentIDs []string `json:"document_ids,omitempty"`
				NodeIDs     []string `json:"node_ids,omitempty"`
			} `json:"scope,omitempty"`
			AsOf    time.Time `json:"as_of,omitempty"`
			Explain bool      `json:"explain,omitempty"`
		} `json:"query"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	documentIDs, err := decodeIDs(wire.Query.Scope.DocumentIDs)
	if err != nil {
		return fmt.Errorf("query.scope.document_ids: %w", err)
	}
	nodeIDs, err := decodeIDs(wire.Query.Scope.NodeIDs)
	if err != nil {
		return fmt.Errorf("query.scope.node_ids: %w", err)
	}
	*r = RetrievalRequest{
		Snapshot: wire.Snapshot,
		Query: retrieval.Request{
			Text: wire.Query.Text, TopK: wire.Query.TopK, Modes: wire.Query.Modes,
			Scope: retrieval.Scope{DocumentIDs: documentIDs, NodeIDs: nodeIDs},
			AsOf:  wire.Query.AsOf, Explain: wire.Query.Explain,
		},
	}
	return nil
}

func wireDocumentValue(value document.Document) wireDocument {
	return wireDocument{
		ID: encodeID(value.ID), RevisionID: encodeID(value.RevisionID),
		Title: value.Title, RootSectionID: encodeID(value.RootSectionID),
		Metadata: wireMetadataValue(value.Metadata),
	}
}

func wireRevisionValue(value document.Revision) wireRevision {
	return wireRevision{
		ID: encodeID(value.ID), DocumentID: encodeID(value.DocumentID),
		CreatedAt: value.CreatedAt, SourceVersion: value.SourceVersion,
		Metadata: wireMetadataValue(value.Metadata),
	}
}

func wireSectionViewValue(value explorer.SectionView) wireSectionView {
	view := wireSectionView{
		Section:  wireSectionValue(value.Section),
		Spans:    make([]wireSpan, 0, len(value.Spans)),
		Children: make([]wireSectionView, 0, len(value.Children)),
	}
	for _, span := range value.Spans {
		view.Spans = append(view.Spans, wireSpanValue(span))
	}
	for _, child := range value.Children {
		view.Children = append(view.Children, wireSectionViewValue(child))
	}
	return view
}

func wireSectionValue(value document.Section) wireSection {
	return wireSection{
		ID: encodeID(value.ID), DocumentID: encodeID(value.DocumentID),
		RevisionID: encodeID(value.RevisionID), ParentID: encodeOptionalID(value.ParentID),
		Order: value.Order, Heading: value.Heading,
		Range: wireRangeValue(value.Range), Metadata: wireMetadataValue(value.Metadata),
	}
}

func wireSpanValue(value document.Span) wireSpan {
	return wireSpan{
		ID: encodeID(value.ID), DocumentID: encodeID(value.DocumentID),
		RevisionID: encodeID(value.RevisionID), SectionID: encodeID(value.SectionID),
		Order: value.Order, Range: wireRangeValue(value.Range),
		Text: value.Text, Metadata: wireMetadataValue(value.Metadata),
	}
}

func wireNeighborhoodValue(value explorer.Neighborhood) wireNeighborhood {
	nodes := make([]wireNode, 0, len(value.Nodes))
	edges := make([]wireEdge, 0, len(value.Edges))
	assertions := make([]wireAssertion, 0, len(value.Assertions))
	for _, node := range value.Nodes {
		nodes = append(nodes, wireNodeValue(node))
	}
	for _, edge := range value.Edges {
		edges = append(edges, wireEdgeValue(edge))
	}
	for _, assertion := range value.Assertions {
		assertions = append(assertions, wireAssertionValue(assertion))
	}
	return wireNeighborhood{Nodes: nodes, Edges: edges, Assertions: assertions}
}

func wirePathValue(value graph.Path) wirePath {
	nodes := make([]wireNode, 0, len(value.Nodes))
	edges := make([]wireEdge, 0, len(value.Edges))
	for _, node := range value.Nodes {
		nodes = append(nodes, wireNodeValue(node))
	}
	for _, edge := range value.Edges {
		edges = append(edges, wireEdgeValue(edge))
	}
	return wirePath{Nodes: nodes, Edges: edges}
}

func wireNodeValue(value graph.Node) wireNode {
	return wireNode{
		ID: encodeID(value.ID), Kind: value.Kind,
		Labels: value.Labels, Properties: wireMetadataValue(value.Properties),
	}
}

func wireEdgeValue(value graph.Edge) wireEdge {
	return wireEdge{
		ID: encodeID(value.ID), From: encodeID(value.From), To: encodeID(value.To),
		Type: value.Type, Weight: value.Weight,
		Properties: wireMetadataValue(value.Properties),
	}
}

func wireCitationValue(value document.Citation) wireCitation {
	return wireCitation{
		DocumentID: encodeID(value.DocumentID), RevisionID: encodeID(value.RevisionID),
		SectionID: encodeOptionalID(value.SectionID),
		SpanID:    encodeOptionalID(value.SpanID), Range: wireRangeValue(value.Range),
	}
}

func wireRangeValue(value document.SourceRange) wireRange {
	return wireRange{
		Start: wirePosition{Offset: value.Start.Offset, Page: value.Start.Page},
		End:   wirePosition{Offset: value.End.Offset, Page: value.End.Page},
	}
}

func wireExplanationValue(value *retrieval.Explanation) *wireExplanation {
	if value == nil {
		return nil
	}
	return &wireExplanation{
		Modes: value.Modes, Summary: value.Summary, Scores: value.Scores,
	}
}

func wireMetadataValue(value shoal.Metadata) wireMetadata {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	metadata := make(wireMetadata, 0, len(keys))
	for _, key := range keys {
		metadata = append(metadata, wireMetadataEntry{
			Key:   base64.RawURLEncoding.EncodeToString([]byte(key)),
			Value: base64.RawURLEncoding.EncodeToString([]byte(value[key])),
		})
	}
	return metadata
}

func wireAssertionValue(value ontology.Assertion) wireAssertion {
	subjectType, _ := value.SubjectType()
	objectType, _ := value.ObjectType()
	evidence := make([]wireEvidenceRef, 0, len(value.Evidence()))
	for _, item := range value.Evidence() {
		evidence = append(evidence, wireEvidenceRefValue(item))
	}
	var identity *wireOntologyIdentity
	if ontologyIdentity, ok := value.Ontology(); ok {
		identity = &wireOntologyIdentity{
			SchemaID:  encodeID(ontologyIdentity.SchemaID()),
			VersionID: encodeID(ontologyIdentity.VersionID()),
		}
	}
	return wireAssertion{
		ID:          encodeID(value.ID()),
		Subject:     encodeID(value.Subject()),
		SubjectType: encodeOptionalID(subjectType),
		Predicate:   encodeID(value.Predicate()),
		Object:      wireOntologyValueValue(value.Object()),
		ObjectType:  encodeOptionalID(objectType),
		Origin:      value.Origin(),
		Confidence:  value.Confidence(),
		Evidence:    evidence,
		Provenance:  wireExtractionProvenanceValue(value.Provenance()),
		Ontology:    identity,
		Metadata:    wireMetadataValue(value.Metadata()),
	}
}

func wireOntologyValueValue(value ontology.Value) wireOntologyValue {
	wire := wireOntologyValue{Type: value.Type()}
	switch value.Type() {
	case ontology.ValueString:
		wire.Text, _ = value.StringValue()
	case ontology.ValueInteger:
		wire.Integer, _ = value.IntegerValue()
	case ontology.ValueNumber:
		wire.Number, _ = value.NumberValue()
	case ontology.ValueBoolean:
		boolean, _ := value.BooleanValue()
		wire.Boolean = &boolean
	case ontology.ValueTimestamp:
		wire.Timestamp, _ = value.TimestampValue()
	case ontology.ValueReference:
		reference, _ := value.ReferenceValue()
		wire.Reference = encodeID(reference)
	}
	return wire
}

func wireEvidenceRefValue(value ontology.EvidenceRef) wireEvidenceRef {
	var derivation *wireAssertionDerivation
	if got, ok := value.Derivation(); ok {
		wire := wireAssertionDerivationValue(got)
		derivation = &wire
	}
	var citation *wireCitation
	var path *wirePath
	if derivation == nil {
		wire := wireCitationValue(value.Citation())
		citation = &wire
		if got, ok := value.Path(); ok {
			wirePath := wirePathValue(got)
			path = &wirePath
		}
	}
	return wireEvidenceRef{
		ID: encodeID(value.ID()), Citation: citation, Quote: value.Quote(), Path: path,
		Derivation: derivation,
		Metadata:   wireMetadataValue(value.Metadata()),
	}
}

func wireAssertionDerivationValue(
	value ontology.AssertionDerivation,
) wireAssertionDerivation {
	return wireAssertionDerivation{
		ID:                    encodeID(value.ID()),
		EmbeddingModel:        value.EmbeddingModel(),
		EmbeddingModelVersion: value.EmbeddingModelVersion(),
		SimilarityMetric:      value.SimilarityMetric(),
		Threshold:             value.Threshold(),
		TessellationCell:      value.TessellationCell(),
		Score:                 value.Score(),
		SourceEndpoint:        encodeID(value.SourceEndpoint()),
		TargetEndpoint:        encodeID(value.TargetEndpoint()),
		IteratorName:          value.IteratorName(),
		IteratorOptions:       wireMetadataValue(value.IteratorOptions()),
	}
}

func wireExtractionProvenanceValue(
	value ontology.ExtractionProvenance,
) wireExtractionProvenance {
	return wireExtractionProvenance{
		Provider:         value.Provider(),
		Model:            value.Model(),
		ModelVersion:     value.ModelVersion(),
		Prompt:           value.Prompt(),
		PromptVersion:    value.PromptVersion(),
		Extractor:        value.Extractor(),
		ExtractorVersion: value.ExtractorVersion(),
		Metadata:         wireMetadataValue(value.Metadata()),
	}
}

func wireDerivationDetailValue(value DerivationDetail) wireDerivationDetail {
	return wireDerivationDetail{
		AssertionID:           encodeID(value.AssertionID),
		DerivationID:          encodeOptionalID(value.DerivationID),
		Origin:                value.Origin,
		Score:                 value.Score,
		EmbeddingModel:        value.EmbeddingModel,
		EmbeddingModelVersion: value.EmbeddingModelVersion,
		SimilarityMetric:      value.SimilarityMetric,
		Threshold:             value.Threshold,
		TessellationCell:      value.TessellationCell,
		IteratorName:          value.IteratorName,
		IteratorOptions:       wireMetadataValue(value.IteratorOptions),
		Provider:              value.Provider,
		Model:                 value.Model,
		ModelVersion:          value.ModelVersion,
	}
}

func derivationDetailValue(value wireDerivationDetail) (DerivationDetail, error) {
	assertionID, err := decodeID(value.AssertionID)
	if err != nil {
		return DerivationDetail{}, fmt.Errorf("assertion_id: %w", err)
	}
	derivationID, err := decodeOptionalID(value.DerivationID)
	if err != nil {
		return DerivationDetail{}, fmt.Errorf("derivation_id: %w", err)
	}
	iteratorOptions, err := metadataValue(value.IteratorOptions)
	if err != nil {
		return DerivationDetail{}, fmt.Errorf("iterator_options: %w", err)
	}
	return DerivationDetail{
		AssertionID:           assertionID,
		DerivationID:          derivationID,
		Origin:                value.Origin,
		Score:                 value.Score,
		EmbeddingModel:        value.EmbeddingModel,
		EmbeddingModelVersion: value.EmbeddingModelVersion,
		SimilarityMetric:      value.SimilarityMetric,
		Threshold:             value.Threshold,
		TessellationCell:      value.TessellationCell,
		IteratorName:          value.IteratorName,
		IteratorOptions:       iteratorOptions,
		Provider:              value.Provider,
		Model:                 value.Model,
		ModelVersion:          value.ModelVersion,
	}, nil
}

func documentValue(value wireDocument) (document.Document, error) {
	id, err := decodeID(value.ID)
	if err != nil {
		return document.Document{}, fmt.Errorf("id: %w", err)
	}
	revisionID, err := decodeID(value.RevisionID)
	if err != nil {
		return document.Document{}, fmt.Errorf("revision_id: %w", err)
	}
	rootSectionID, err := decodeID(value.RootSectionID)
	if err != nil {
		return document.Document{}, fmt.Errorf("root_section_id: %w", err)
	}
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return document.Document{}, fmt.Errorf("metadata: %w", err)
	}
	return document.Document{
		ID: id, RevisionID: revisionID, Title: value.Title,
		RootSectionID: rootSectionID, Metadata: metadata,
	}, nil
}

func revisionValue(value wireRevision) (document.Revision, error) {
	id, err := decodeID(value.ID)
	if err != nil {
		return document.Revision{}, fmt.Errorf("id: %w", err)
	}
	documentID, err := decodeID(value.DocumentID)
	if err != nil {
		return document.Revision{}, fmt.Errorf("document_id: %w", err)
	}
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return document.Revision{}, fmt.Errorf("metadata: %w", err)
	}
	return document.Revision{
		ID: id, DocumentID: documentID, CreatedAt: value.CreatedAt,
		SourceVersion: value.SourceVersion, Metadata: metadata,
	}, nil
}

func sectionViewValue(value wireSectionView) (explorer.SectionView, error) {
	section, err := sectionValue(value.Section)
	if err != nil {
		return explorer.SectionView{}, fmt.Errorf("section: %w", err)
	}
	spans := make([]document.Span, 0, len(value.Spans))
	for _, item := range value.Spans {
		span, err := spanValue(item)
		if err != nil {
			return explorer.SectionView{}, fmt.Errorf("spans: %w", err)
		}
		spans = append(spans, span)
	}
	children := make([]explorer.SectionView, 0, len(value.Children))
	for _, item := range value.Children {
		child, err := sectionViewValue(item)
		if err != nil {
			return explorer.SectionView{}, fmt.Errorf("children: %w", err)
		}
		children = append(children, child)
	}
	return explorer.SectionView{Section: section, Spans: spans, Children: children}, nil
}

func sectionValue(value wireSection) (document.Section, error) {
	id, err := decodeID(value.ID)
	if err != nil {
		return document.Section{}, fmt.Errorf("id: %w", err)
	}
	documentID, err := decodeID(value.DocumentID)
	if err != nil {
		return document.Section{}, fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeID(value.RevisionID)
	if err != nil {
		return document.Section{}, fmt.Errorf("revision_id: %w", err)
	}
	parentID, err := decodeOptionalID(value.ParentID)
	if err != nil {
		return document.Section{}, fmt.Errorf("parent_id: %w", err)
	}
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return document.Section{}, fmt.Errorf("metadata: %w", err)
	}
	return document.Section{
		ID: id, DocumentID: documentID, RevisionID: revisionID,
		ParentID: parentID, Order: value.Order, Heading: value.Heading,
		Range: sourceRangeValue(value.Range), Metadata: metadata,
	}, nil
}

func spanValue(value wireSpan) (document.Span, error) {
	id, err := decodeID(value.ID)
	if err != nil {
		return document.Span{}, fmt.Errorf("id: %w", err)
	}
	documentID, err := decodeID(value.DocumentID)
	if err != nil {
		return document.Span{}, fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeID(value.RevisionID)
	if err != nil {
		return document.Span{}, fmt.Errorf("revision_id: %w", err)
	}
	sectionID, err := decodeID(value.SectionID)
	if err != nil {
		return document.Span{}, fmt.Errorf("section_id: %w", err)
	}
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return document.Span{}, fmt.Errorf("metadata: %w", err)
	}
	return document.Span{
		ID: id, DocumentID: documentID, RevisionID: revisionID,
		SectionID: sectionID, Order: value.Order, Range: sourceRangeValue(value.Range),
		Text: value.Text, Metadata: metadata,
	}, nil
}

func neighborhoodValue(value wireNeighborhood) (explorer.Neighborhood, error) {
	nodes := make([]graph.Node, 0, len(value.Nodes))
	for _, item := range value.Nodes {
		node, err := nodeValue(item)
		if err != nil {
			return explorer.Neighborhood{}, fmt.Errorf("nodes: %w", err)
		}
		nodes = append(nodes, node)
	}
	edges := make([]graph.Edge, 0, len(value.Edges))
	for _, item := range value.Edges {
		edge, err := edgeValue(item)
		if err != nil {
			return explorer.Neighborhood{}, fmt.Errorf("edges: %w", err)
		}
		edges = append(edges, edge)
	}
	assertions := make([]ontology.Assertion, 0, len(value.Assertions))
	for _, item := range value.Assertions {
		assertion, err := assertionValue(item)
		if err != nil {
			return explorer.Neighborhood{}, fmt.Errorf("assertions: %w", err)
		}
		assertions = append(assertions, assertion)
	}
	return explorer.Neighborhood{
		Nodes: nodes, Edges: edges, Assertions: assertions,
	}, nil
}

func pathValue(value wirePath) (graph.Path, error) {
	nodes := make([]graph.Node, 0, len(value.Nodes))
	for _, item := range value.Nodes {
		node, err := nodeValue(item)
		if err != nil {
			return graph.Path{}, fmt.Errorf("nodes: %w", err)
		}
		nodes = append(nodes, node)
	}
	edges := make([]graph.Edge, 0, len(value.Edges))
	for _, item := range value.Edges {
		edge, err := edgeValue(item)
		if err != nil {
			return graph.Path{}, fmt.Errorf("edges: %w", err)
		}
		edges = append(edges, edge)
	}
	return graph.Path{Nodes: nodes, Edges: edges}, nil
}

func nodeValue(value wireNode) (graph.Node, error) {
	id, err := decodeID(value.ID)
	if err != nil {
		return graph.Node{}, fmt.Errorf("id: %w", err)
	}
	metadata, err := metadataValue(value.Properties)
	if err != nil {
		return graph.Node{}, fmt.Errorf("properties: %w", err)
	}
	return graph.Node{
		ID: id, Kind: value.Kind, Labels: value.Labels, Properties: metadata,
	}, nil
}

func edgeValue(value wireEdge) (graph.Edge, error) {
	id, err := decodeID(value.ID)
	if err != nil {
		return graph.Edge{}, fmt.Errorf("id: %w", err)
	}
	from, err := decodeID(value.From)
	if err != nil {
		return graph.Edge{}, fmt.Errorf("from: %w", err)
	}
	to, err := decodeID(value.To)
	if err != nil {
		return graph.Edge{}, fmt.Errorf("to: %w", err)
	}
	metadata, err := metadataValue(value.Properties)
	if err != nil {
		return graph.Edge{}, fmt.Errorf("properties: %w", err)
	}
	return graph.Edge{
		ID: id, From: from, To: to, Type: value.Type,
		Weight: value.Weight, Properties: metadata,
	}, nil
}

func assertionValue(value wireAssertion) (ontology.Assertion, error) {
	subject, err := decodeID(value.Subject)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("subject: %w", err)
	}
	subjectType, err := decodeOptionalID(value.SubjectType)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("subject_type: %w", err)
	}
	predicate, err := decodeID(value.Predicate)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("predicate: %w", err)
	}
	object, err := ontologyValue(value.Object)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("object: %w", err)
	}
	objectType, err := decodeOptionalID(value.ObjectType)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("object_type: %w", err)
	}
	evidence := make([]ontology.EvidenceRef, 0, len(value.Evidence))
	for _, item := range value.Evidence {
		ref, err := evidenceRefValue(item)
		if err != nil {
			return ontology.Assertion{}, fmt.Errorf("evidence: %w", err)
		}
		evidence = append(evidence, ref)
	}
	provenance, err := extractionProvenanceValue(value.Provenance)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("provenance: %w", err)
	}
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("metadata: %w", err)
	}
	options := make([]ontology.AssertionOption, 0, 3)
	if subjectType != "" {
		options = append(options, ontology.WithAssertionSubjectType(subjectType))
	}
	if objectType != "" {
		options = append(options, ontology.WithAssertionObjectType(objectType))
	}
	if value.Ontology != nil {
		identity, err := ontologyIdentityValue(*value.Ontology)
		if err != nil {
			return ontology.Assertion{}, fmt.Errorf("ontology: %w", err)
		}
		options = append(options, ontology.WithAssertionOntology(identity))
	}
	assertion, err := ontology.NewAssertion(
		subject, predicate, object, value.Origin, value.Confidence,
		evidence, provenance, metadata, options...,
	)
	if err != nil {
		return ontology.Assertion{}, err
	}
	id, err := decodeID(value.ID)
	if err != nil {
		return ontology.Assertion{}, fmt.Errorf("id: %w", err)
	}
	if assertion.ID() != id {
		return ontology.Assertion{}, fmt.Errorf("id does not match assertion content")
	}
	return assertion, nil
}

func ontologyValue(value wireOntologyValue) (ontology.Value, error) {
	switch value.Type {
	case ontology.ValueString:
		return ontology.NewStringValue(value.Text)
	case ontology.ValueInteger:
		return ontology.NewIntegerValue(value.Integer), nil
	case ontology.ValueNumber:
		return ontology.NewNumberValue(value.Number)
	case ontology.ValueBoolean:
		if value.Boolean == nil {
			return ontology.Value{}, fmt.Errorf("boolean is required")
		}
		return ontology.NewBooleanValue(*value.Boolean), nil
	case ontology.ValueTimestamp:
		return ontology.NewTimestampValue(value.Timestamp)
	case ontology.ValueReference:
		reference, err := decodeID(value.Reference)
		if err != nil {
			return ontology.Value{}, fmt.Errorf("reference: %w", err)
		}
		return ontology.NewReferenceValue(reference)
	default:
		return ontology.Value{}, fmt.Errorf("unknown value type %q", value.Type)
	}
}

func evidenceRefValue(value wireEvidenceRef) (ontology.EvidenceRef, error) {
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return ontology.EvidenceRef{}, fmt.Errorf("metadata: %w", err)
	}
	var ref ontology.EvidenceRef
	if value.Derivation != nil {
		derivation, err := assertionDerivationValue(*value.Derivation)
		if err != nil {
			return ontology.EvidenceRef{}, fmt.Errorf("derivation: %w", err)
		}
		ref, err = ontology.NewDerivationEvidenceRef(derivation, metadata)
		if err != nil {
			return ontology.EvidenceRef{}, err
		}
	} else {
		if value.Citation == nil {
			return ontology.EvidenceRef{}, fmt.Errorf("citation is required")
		}
		citation, err := citationValue(*value.Citation)
		if err != nil {
			return ontology.EvidenceRef{}, fmt.Errorf("citation: %w", err)
		}
		var options []ontology.EvidenceOption
		if value.Path != nil {
			path, err := pathValue(*value.Path)
			if err != nil {
				return ontology.EvidenceRef{}, fmt.Errorf("path: %w", err)
			}
			options = append(options, ontology.WithEvidencePath(path))
		}
		ref, err = ontology.NewEvidenceRef(citation, value.Quote, metadata, options...)
		if err != nil {
			return ontology.EvidenceRef{}, err
		}
	}
	id, err := decodeID(value.ID)
	if err != nil {
		return ontology.EvidenceRef{}, fmt.Errorf("id: %w", err)
	}
	if ref.ID() != id {
		return ontology.EvidenceRef{}, fmt.Errorf("id does not match evidence content")
	}
	return ref, nil
}

func assertionDerivationValue(
	value wireAssertionDerivation,
) (ontology.AssertionDerivation, error) {
	source, err := decodeID(value.SourceEndpoint)
	if err != nil {
		return ontology.AssertionDerivation{}, fmt.Errorf("source_endpoint: %w", err)
	}
	target, err := decodeID(value.TargetEndpoint)
	if err != nil {
		return ontology.AssertionDerivation{}, fmt.Errorf("target_endpoint: %w", err)
	}
	options, err := metadataValue(value.IteratorOptions)
	if err != nil {
		return ontology.AssertionDerivation{}, fmt.Errorf("iterator_options: %w", err)
	}
	derivation, err := ontology.NewAssertionDerivation(
		value.EmbeddingModel,
		value.EmbeddingModelVersion,
		value.SimilarityMetric,
		value.Threshold,
		value.TessellationCell,
		value.Score,
		source,
		target,
		value.IteratorName,
		options,
	)
	if err != nil {
		return ontology.AssertionDerivation{}, err
	}
	id, err := decodeID(value.ID)
	if err != nil {
		return ontology.AssertionDerivation{}, fmt.Errorf("id: %w", err)
	}
	if derivation.ID() != id {
		return ontology.AssertionDerivation{}, fmt.Errorf("id does not match derivation content")
	}
	return derivation, nil
}

func extractionProvenanceValue(
	value wireExtractionProvenance,
) (ontology.ExtractionProvenance, error) {
	metadata, err := metadataValue(value.Metadata)
	if err != nil {
		return ontology.ExtractionProvenance{}, fmt.Errorf("metadata: %w", err)
	}
	return ontology.NewExtractionProvenance(
		value.Provider,
		value.Model,
		value.ModelVersion,
		value.Prompt,
		value.PromptVersion,
		value.Extractor,
		value.ExtractorVersion,
		metadata,
	)
}

func ontologyIdentityValue(
	value wireOntologyIdentity,
) (ontology.OntologyIdentity, error) {
	schemaID, err := decodeID(value.SchemaID)
	if err != nil {
		return ontology.OntologyIdentity{}, fmt.Errorf("schema_id: %w", err)
	}
	versionID, err := decodeID(value.VersionID)
	if err != nil {
		return ontology.OntologyIdentity{}, fmt.Errorf("version_id: %w", err)
	}
	return ontology.NewOntologyIdentityFromIDs(schemaID, versionID)
}

func citationValue(value wireCitation) (document.Citation, error) {
	documentID, err := decodeID(value.DocumentID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeID(value.RevisionID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("revision_id: %w", err)
	}
	sectionID, err := decodeOptionalID(value.SectionID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("section_id: %w", err)
	}
	spanID, err := decodeOptionalID(value.SpanID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("span_id: %w", err)
	}
	return document.Citation{
		DocumentID: documentID, RevisionID: revisionID,
		SectionID: sectionID, SpanID: spanID,
		Range: sourceRangeValue(value.Range),
	}, nil
}

func sourceRangeValue(value wireRange) document.SourceRange {
	return document.SourceRange{
		Start: document.SourcePosition{
			Offset: value.Start.Offset, Page: value.Start.Page,
		},
		End: document.SourcePosition{
			Offset: value.End.Offset, Page: value.End.Page,
		},
	}
}

func explanationValue(value *wireExplanation) *retrieval.Explanation {
	if value == nil {
		return nil
	}
	return &retrieval.Explanation{
		Modes: value.Modes, Summary: value.Summary, Scores: value.Scores,
	}
}

func metadataValue(value wireMetadata) (shoal.Metadata, error) {
	if len(value) == 0 {
		return nil, nil
	}
	metadata := make(shoal.Metadata, len(value))
	for _, item := range value {
		key, err := base64.RawURLEncoding.DecodeString(item.Key)
		if err != nil {
			return nil, fmt.Errorf("key must be unpadded base64url")
		}
		decodedValue, err := base64.RawURLEncoding.DecodeString(item.Value)
		if err != nil {
			return nil, fmt.Errorf("value must be unpadded base64url")
		}
		if _, duplicate := metadata[string(key)]; duplicate {
			return nil, fmt.Errorf("metadata contains duplicate keys")
		}
		metadata[string(key)] = string(decodedValue)
	}
	return metadata, nil
}

func encodeID(value shoal.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func encodeOptionalID(value shoal.ID) string {
	if value == "" {
		return ""
	}
	return encodeID(value)
}

func decodeID(value string) (shoal.ID, error) {
	if value == "" {
		return "", fmt.Errorf("is required")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("must be unpadded base64url")
	}
	return shoal.ID(decoded), nil
}

func decodeOptionalID(value string) (shoal.ID, error) {
	if value == "" {
		return "", nil
	}
	return decodeID(value)
}

func decodeIDs(values []string) ([]shoal.ID, error) {
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		id, err := decodeID(value)
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}

func cloneUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func strictUnmarshal(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
