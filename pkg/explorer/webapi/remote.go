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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	exploreranalytics "github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	maxWireMetadataBytes = int64(shoal.MaxMetadataBytes*2 + 4096)
	maxWireIDBytes       = int64(shoal.MaxIDBytes*2 + 128)
	maxWireTextBytes     = int64(shoal.MaxSemanticStringBytes*2 + 4096)
	maxWireGraphItem     = maxWireMetadataBytes + 4*maxWireIDBytes + maxWireTextBytes

	maxRemoteMetadataResponseBytes = int64(1 << 20)
	maxRemoteResponseBytes         = int64(MaxResponseBytes)
)

// RemoteService adapts the same logical Explorer web API over HTTP. It keeps
// validation, bounds, errors, and feature negotiation on the server side so the
// browser contract is identical to embedded mode.
type RemoteService struct {
	base   *url.URL
	client *http.Client
}

// NewRemoteService creates a remote-backed service for an upstream Explorer
// web API root URL.
func NewRemoteService(endpoint string, client *http.Client) (*RemoteService, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "remote endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "remote endpoint must be an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "remote endpoint must use http or https")
	}
	if parsed.User != nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "remote endpoint must not include userinfo")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &RemoteService{base: parsed, client: client}, nil
}

func (s *RemoteService) Capabilities(ctx context.Context) (Capabilities, error) {
	metadata, err := s.Metadata(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return metadata.Capabilities, nil
}

func (s *RemoteService) Metadata(ctx context.Context) (MetadataResponse, error) {
	return s.metadata(ctx)
}

// IngestAvailable returns false because remote capability negotiation requires
// a request context. Callers that snapshot a context-free tool surface must
// fail closed rather than advertise ingestion unconditionally.
func (*RemoteService) IngestAvailable() bool {
	return false
}

func (s *RemoteService) Ingest(
	ctx context.Context, request IngestRequest,
) (IngestResponse, error) {
	prepared, err := prepareUploads(request)
	if err != nil {
		return IngestResponse{}, err
	}
	// Checked here as well as upstream, so an invalid reason is refused
	// before anything is sent and with the same error as embedded mode.
	if _, err := declassificationReasonContext(
		ctx, request.DeclassificationReason); err != nil {
		return IngestResponse{}, err
	}
	if err := s.ensureCapability(ctx, CapabilityIngest); err != nil {
		return IngestResponse{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if request.DeclassificationReason != "" {
		if err := writer.WriteField(
			DeclassificationReasonField, request.DeclassificationReason); err != nil {
			_ = writer.Close()
			return IngestResponse{}, shoal.WrapError(
				shoal.ErrorInternal, "encode workspace upload", err)
		}
	}
	for _, item := range request.Files {
		name, err := sanitizeUploadFilename(item.Name)
		if err != nil {
			_ = writer.Close()
			return IngestResponse{}, err
		}
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			_ = writer.Close()
			return IngestResponse{}, shoal.WrapError(
				shoal.ErrorInternal, "encode workspace upload", err)
		}
		if _, err := part.Write(item.Content); err != nil {
			_ = writer.Close()
			return IngestResponse{}, shoal.WrapError(
				shoal.ErrorInternal, "encode workspace upload", err)
		}
	}
	if err := writer.Close(); err != nil {
		return IngestResponse{}, shoal.WrapError(
			shoal.ErrorInternal, "encode workspace upload", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.endpoint("ingest"), &body)
	if err != nil {
		return IngestResponse{}, shoal.WrapError(
			shoal.ErrorInvalidArgument, "build remote upload request", err)
	}
	httpRequest.Header.Set("Content-Type", writer.FormDataContentType())
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("X-Shoal-Workspace-Request", "1")
	httpResponse, err := s.client.Do(httpRequest)
	if err != nil {
		return IngestResponse{}, explorer.MarkIndeterminateCommit(
			shoal.WrapError(
				remoteTransportCode(err), "remote workspace unavailable", err))
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		remoteErr, verified := decodeRemoteErrorOutcome(httpResponse)
		if (len(request.Files) > 1 || !verified) &&
			!explorer.IsIndeterminateCommit(remoteErr) {
			remoteErr = explorer.MarkIndeterminateCommit(remoteErr)
		}
		return IngestResponse{}, remoteErr
	}
	var response IngestResponse
	if err := decodeOneJSON(httpResponse.Body, &response, maxRemoteResponseBytes); err != nil {
		return IngestResponse{}, explorer.MarkIndeterminateCommit(
			shoal.WrapError(
				remoteDecodeCode(err), "decode remote workspace response", err))
	}
	if err := validateRemoteIngestResponse(prepared, response); err != nil {
		return IngestResponse{}, explorer.MarkIndeterminateCommit(err)
	}
	return response, nil
}

func (s *RemoteService) Documents(
	ctx context.Context, request DocumentsRequest,
) (DocumentsResponse, error) {
	limit, err := normalizeLimit(request.Page.Limit)
	if err != nil {
		return DocumentsResponse{}, err
	}
	var response DocumentsResponse
	if err := s.post(
		ctx, CapabilityDocuments, "documents", request, &response,
		maxRemoteResponseBytes,
	); err != nil {
		return DocumentsResponse{}, err
	}
	if err := validateRemoteSnapshot(request.Snapshot, response.Snapshot); err != nil {
		return DocumentsResponse{}, err
	}
	if uint32(len(response.Documents)) > limit {
		return DocumentsResponse{}, remoteContractError(
			"remote document page exceeds the server bound", nil)
	}
	if response.NextCursor != "" {
		if len(response.Documents) == 0 {
			return DocumentsResponse{}, remoteContractError(
				"remote document cursor did not advance", nil)
		}
		if response.NextCursor == request.Page.Cursor {
			return DocumentsResponse{}, remoteContractError(
				"remote document cursor repeated the request cursor", nil)
		}
	}
	seenDocuments := make(map[shoal.ID]struct{}, len(response.Documents))
	for index, summary := range response.Documents {
		if err := validateRemoteDocumentSummary(summary); err != nil {
			return DocumentsResponse{}, err
		}
		if _, duplicate := seenDocuments[summary.Document.ID]; duplicate {
			return DocumentsResponse{}, remoteContractError(
				"remote document page has duplicate document IDs", nil)
		}
		seenDocuments[summary.Document.ID] = struct{}{}
		if index > 0 && compareDocumentSummary(
			response.Documents[index-1], summary,
		) > 0 {
			return DocumentsResponse{}, remoteContractError(
				"remote document page is not ordered", nil)
		}
	}
	return response, nil
}

func (s *RemoteService) Document(
	ctx context.Context, request DocumentRequest,
) (DocumentResponse, error) {
	if err := shoal.ValidateRequiredID("document ID", request.DocumentID); err != nil {
		return DocumentResponse{}, err
	}
	if err := shoal.ValidateOptionalID("revision ID", request.RevisionID); err != nil {
		return DocumentResponse{}, err
	}
	var response DocumentResponse
	if err := s.post(
		ctx, CapabilityDocument, "document", request, &response,
		maxRemoteResponseBytes,
	); err != nil {
		return DocumentResponse{}, err
	}
	if err := validateRemoteSnapshot(request.Snapshot, response.Snapshot); err != nil {
		return DocumentResponse{}, err
	}
	if err := validateRemoteDocumentView(response.Document); err != nil {
		return DocumentResponse{}, err
	}
	if response.Document.Document.ID != request.DocumentID {
		return DocumentResponse{}, remoteContractError(
			"remote document does not match the requested document ID", nil)
	}
	if request.RevisionID != "" && response.Document.Revision.ID != request.RevisionID {
		return DocumentResponse{}, remoteContractError(
			"remote document does not match the requested revision ID", nil)
	}
	return response, nil
}

func (s *RemoteService) Retrieve(
	ctx context.Context, request RetrievalRequest,
) (RetrievalResponse, error) {
	query, err := request.Query.Normalize()
	if err != nil {
		return RetrievalResponse{}, err
	}
	if query.TopK > MaxTopK {
		return RetrievalResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace top_k exceeds the server bound")
	}
	if !query.AsOf.IsZero() && !request.Snapshot.AsOf.IsZero() &&
		!query.AsOf.Equal(request.Snapshot.AsOf) {
		return RetrievalResponse{}, shoal.NewError(
			shoal.ErrorConflict, "retrieval as_of does not match the pinned snapshot")
	}
	request.Query = query
	if query.HasMode(retrieval.ModeVector) &&
		len(query.Scope.DocumentIDs) == 0 &&
		len(query.Scope.NodeIDs) == 0 {
		if err := s.ensureCapability(ctx, CapabilityVector); err != nil {
			return RetrievalResponse{}, err
		}
	}
	var response RetrievalResponse
	if err := s.post(
		ctx, CapabilityRetrieve, "retrieve", request, &response,
		maxRemoteResponseBytes,
	); err != nil {
		return RetrievalResponse{}, err
	}
	if err := validateRemoteSnapshot(request.Snapshot, response.Snapshot); err != nil {
		return RetrievalResponse{}, err
	}
	if !query.AsOf.IsZero() && !query.AsOf.Equal(response.Snapshot.AsOf) {
		return RetrievalResponse{}, shoal.NewError(
			shoal.ErrorConflict, "retrieval as_of does not match the pinned snapshot")
	}
	if err := validateRetrievalResponse(response.Retrieval, query); err != nil {
		return RetrievalResponse{}, shoal.WrapError(
			shoal.ErrorInternal, "invalid retrieval response", err)
	}
	return response, nil
}

func (s *RemoteService) Neighborhood(
	ctx context.Context, request NeighborhoodRequest,
) (NeighborhoodResponse, error) {
	depth, fanout, maxNodes, err := normalizeGraphBounds(
		request.Depth, request.Fanout, request.MaxNodes)
	if err != nil {
		return NeighborhoodResponse{}, err
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return NeighborhoodResponse{}, err
	}
	if len(normalized.NodeIDs) == 0 {
		return NeighborhoodResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "at least one graph node ID is required")
	}
	if uint32(len(normalized.NodeIDs)) > maxNodes {
		return NeighborhoodResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph seeds exceed max_nodes")
	}
	if err := validateEdgeTypes(normalized.EdgeTypes); err != nil {
		return NeighborhoodResponse{}, err
	}
	request.NodeIDs = normalized.NodeIDs
	request.EdgeTypes = normalized.EdgeTypes
	request.Depth = depth
	request.Fanout = fanout
	request.MaxNodes = maxNodes
	if request.Cursor != "" && (len(normalized.NodeIDs) != 1 || depth != 1) {
		return NeighborhoodResponse{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"graph cursors require one seed and depth 1",
		)
	}
	var response NeighborhoodResponse
	if err := s.post(
		ctx, CapabilityNeighborhood, "neighborhood", request, &response,
		remoteGraphResponseLimit(maxNodes, fanout),
	); err != nil {
		return NeighborhoodResponse{}, err
	}
	if err := validateRemoteSnapshot(request.Snapshot, response.Snapshot); err != nil {
		return NeighborhoodResponse{}, err
	}
	if err := validateRemoteNeighborhood(
		request, response.Neighborhood, maxNodes, fanout,
	); err != nil {
		return NeighborhoodResponse{}, err
	}
	if response.ScannedEdges != nil &&
		*response.ScannedEdges < uint32(len(response.Neighborhood.Edges)) {
		return NeighborhoodResponse{}, remoteContractError(
			"remote graph scan count is smaller than the returned edge count", nil)
	}
	if response.NextCursor != "" {
		if !response.Truncated {
			return NeighborhoodResponse{}, remoteContractError(
				"remote graph cursor returned for a complete response", nil)
		}
		if len(request.NodeIDs) != 1 || request.Depth != 1 {
			return NeighborhoodResponse{}, remoteContractError(
				"remote graph cursor returned for an ineligible request", nil)
		}
		if response.NextCursor == request.Cursor {
			return NeighborhoodResponse{}, remoteContractError(
				"remote graph cursor repeated the request cursor", nil)
		}
	}
	return response, nil
}

func (s *RemoteService) Path(ctx context.Context, request PathRequest) (PathResponse, error) {
	if err := shoal.ValidateRequiredID("path source node ID", request.From); err != nil {
		return PathResponse{}, err
	}
	if err := shoal.ValidateRequiredID("path target node ID", request.To); err != nil {
		return PathResponse{}, err
	}
	if effectiveGraphNodeLimit(ctx, MaxNodes) < MaxNodes {
		return PathResponse{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"remote path cannot enforce the workspace graph node budget",
		)
	}
	depth, fanout, _, err := normalizeGraphBounds(
		request.MaxDepth, request.Fanout, MaxNodes)
	if err != nil {
		return PathResponse{}, err
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{request.From}, Depth: depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return PathResponse{}, err
	}
	if err := validateEdgeTypes(normalized.EdgeTypes); err != nil {
		return PathResponse{}, err
	}
	request.EdgeTypes = normalized.EdgeTypes
	request.MaxDepth = depth
	request.Fanout = fanout
	var response PathResponse
	if err := s.post(
		ctx, CapabilityPath, "path", request, &response,
		remotePathResponseLimit(depth),
	); err != nil {
		return PathResponse{}, err
	}
	if err := validateRemoteSnapshot(request.Snapshot, response.Snapshot); err != nil {
		return PathResponse{}, err
	}
	if err := response.Path.Validate(); err != nil {
		return PathResponse{}, shoal.WrapError(shoal.ErrorInternal, "invalid path", err)
	}
	if len(response.Path.Nodes) == 0 ||
		response.Path.Nodes[0].ID != request.From ||
		response.Path.Nodes[len(response.Path.Nodes)-1].ID != request.To ||
		uint32(len(response.Path.Edges)) > depth {
		return PathResponse{}, remoteContractError(
			"remote path does not match the bounded request", nil)
	}
	if request.From == request.To &&
		(len(response.Path.Nodes) != 1 || len(response.Path.Edges) != 0) {
		return PathResponse{}, remoteContractError(
			"remote identity path must contain only the requested node", nil)
	}
	seenEdges := make(map[shoal.ID]struct{}, len(response.Path.Edges))
	for _, edge := range response.Path.Edges {
		if _, duplicate := seenEdges[edge.ID]; duplicate {
			return PathResponse{}, remoteContractError(
				"remote path contains duplicate edge IDs", nil)
		}
		seenEdges[edge.ID] = struct{}{}
		if !edgeTypeAllowed(edge.Type, request.EdgeTypes) {
			return PathResponse{}, remoteContractError(
				"remote path contains an excluded edge type", nil)
		}
	}
	return response, nil
}

// Analytics invokes the upstream authorized bounded analytics provider. The
// upstream must advertise both the capability and its exact runtime limits.
func (s *RemoteService) Analytics(
	ctx context.Context,
	request AnalyticsRequest,
) (AnalyticsResponse, error) {
	metadata, err := s.Metadata(ctx)
	if err != nil {
		return AnalyticsResponse{}, err
	}
	if !metadata.Capabilities.Analytics || metadata.AnalyticsLimits == nil ||
		!metadata.AnalyticsRecordingRequired {
		return AnalyticsResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"analytics\" is unavailable")
	}
	analyticsRequest := exploreranalytics.Request{
		SnapshotID: request.Snapshot.ID,
		Scope:      request.Scope, PageRank: request.PageRank,
	}
	if err := exploreranalytics.ValidateRequest(
		analyticsRequest, *metadata.AnalyticsLimits); err != nil {
		return AnalyticsResponse{}, err
	}
	var response AnalyticsResponse
	if err := s.postWithCommitOutcome(
		ctx, CapabilityAnalytics, "analytics", request, &response,
		maxRemoteResponseBytes, true,
	); err != nil {
		return AnalyticsResponse{}, err
	}
	if err := ValidateAnalyticsResponse(
		request, response, *metadata.AnalyticsLimits); err != nil {
		return AnalyticsResponse{}, explorer.MarkIndeterminateCommit(
			remoteContractError("remote analytics response is invalid", err))
	}
	return response, nil
}

func (s *RemoteService) post(
	ctx context.Context,
	capability Capability,
	path string,
	request any,
	response any,
	responseLimit int64,
) error {
	return s.postWithCommitOutcome(
		ctx, capability, path, request, response, responseLimit, false)
}

func (s *RemoteService) postWithCommitOutcome(
	ctx context.Context,
	capability Capability,
	path string,
	request any,
	response any,
	responseLimit int64,
	mayCommit bool,
) error {
	if err := s.ensureCapability(ctx, capability); err != nil {
		return err
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(request); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "encode workspace request", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.endpoint(path), &body)
	if err != nil {
		return shoal.WrapError(shoal.ErrorInvalidArgument, "build remote request", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpResponse, err := s.client.Do(httpRequest)
	if err != nil {
		remoteErr := shoal.WrapError(
			remoteTransportCode(err), "remote workspace unavailable", err)
		if mayCommit {
			return explorer.MarkIndeterminateCommit(remoteErr)
		}
		return remoteErr
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		remoteErr, verified := decodeRemoteErrorOutcome(httpResponse)
		if mayCommit && !verified &&
			!explorer.IsIndeterminateCommit(remoteErr) {
			remoteErr = explorer.MarkIndeterminateCommit(remoteErr)
		}
		return remoteErr
	}
	if err := decodeOneJSON(httpResponse.Body, response, responseLimit); err != nil {
		remoteErr := shoal.WrapError(
			remoteDecodeCode(err), "decode remote workspace response", err)
		if mayCommit {
			return explorer.MarkIndeterminateCommit(remoteErr)
		}
		return remoteErr
	}
	return nil
}

func (s *RemoteService) ensureCapability(
	ctx context.Context, capability Capability,
) error {
	capabilities, err := s.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !capabilities.Supports(capability) {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			fmt.Sprintf("workspace capability %q is unavailable", capability),
		)
	}
	return nil
}

func (s *RemoteService) metadata(ctx context.Context) (MetadataResponse, error) {
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodGet, s.endpoint("meta"), nil)
	if err != nil {
		return MetadataResponse{}, shoal.WrapError(
			shoal.ErrorInvalidArgument, "build remote metadata request", err)
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpResponse, err := s.client.Do(httpRequest)
	if err != nil {
		return MetadataResponse{}, shoal.WrapError(
			remoteTransportCode(err), "remote workspace unavailable", err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return MetadataResponse{}, decodeRemoteError(httpResponse)
	}
	metadata, err := decodeRemoteMetadata(httpResponse.Body)
	if err != nil {
		return MetadataResponse{}, shoal.WrapError(
			remoteDecodeCode(err), "decode remote workspace metadata", err)
	}
	return metadata, nil
}

func (s *RemoteService) endpoint(path string) string {
	target := *s.base
	basePath := strings.TrimRight(target.Path, "/")
	target.Path = basePath + "/api/v1/" + strings.TrimLeft(path, "/")
	target.RawQuery = ""
	target.Fragment = ""
	return target.String()
}

func remoteGraphResponseLimit(maxNodes, fanout uint32) int64 {
	return minInt64(
		maxRemoteResponseBytes,
		int64(maxNodes)*(maxWireGraphItem+int64(fanout)*maxWireGraphItem)+(1<<20),
	)
}

func remotePathResponseLimit(depth uint32) int64 {
	return minInt64(
		maxRemoteResponseBytes,
		int64(depth+1)*maxWireGraphItem+int64(depth)*maxWireGraphItem+(1<<20),
	)
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func decodeRemoteMetadata(reader io.Reader) (MetadataResponse, error) {
	var wire struct {
		MaxPageSize                uint32                    `json:"max_page_size"`
		MaxTopK                    uint32                    `json:"max_top_k"`
		MaxDepth                   uint32                    `json:"max_depth"`
		MaxFanout                  uint32                    `json:"max_fanout"`
		MaxNodes                   uint32                    `json:"max_nodes"`
		MaxEdgeTypes               uint32                    `json:"max_edge_types,omitempty"`
		MaxResponseBytes           uint64                    `json:"max_response_bytes,omitempty"`
		MaxUploadFiles             uint32                    `json:"max_upload_files,omitempty"`
		MaxUploadFileBytes         uint64                    `json:"max_upload_file_bytes,omitempty"`
		MaxUploadTotalBytes        uint64                    `json:"max_upload_total_bytes,omitempty"`
		AnalyticsLimits            *exploreranalytics.Limits `json:"analytics_limits,omitempty"`
		AnalyticsRecordingRequired bool                      `json:"analytics_recording_required,omitempty"`
		Capabilities               *Capabilities             `json:"capabilities,omitempty"`
	}
	if err := decodeOneJSON(reader, &wire, maxRemoteMetadataResponseBytes); err != nil {
		return MetadataResponse{}, err
	}
	if wire.MaxPageSize < MaxPageSize ||
		wire.MaxTopK < MaxTopK ||
		wire.MaxDepth < MaxDepth ||
		wire.MaxFanout < MaxFanout ||
		wire.MaxNodes < MaxNodes ||
		(wire.MaxEdgeTypes != 0 && wire.MaxEdgeTypes < MaxEdgeTypes) ||
		(wire.MaxResponseBytes != 0 && wire.MaxResponseBytes < MaxResponseBytes) ||
		(wire.MaxUploadFiles != 0 && wire.MaxUploadFiles < MaxUploadFiles) ||
		(wire.MaxUploadFileBytes != 0 && wire.MaxUploadFileBytes < MaxUploadFileBytes) ||
		(wire.MaxUploadTotalBytes != 0 && wire.MaxUploadTotalBytes < MaxUploadTotalBytes) {
		return MetadataResponse{}, errors.New("remote workspace bounds are below public bounds")
	}
	capabilities := Capabilities{}
	if wire.Capabilities != nil {
		capabilities = *wire.Capabilities
	}
	if capabilities.Ingest &&
		(wire.MaxUploadFiles == 0 ||
			wire.MaxUploadFileBytes == 0 ||
			wire.MaxUploadTotalBytes == 0) {
		return MetadataResponse{}, errors.New("remote workspace upload bounds are incomplete")
	}
	if wire.AnalyticsLimits != nil {
		if err := wire.AnalyticsLimits.Validate(); err != nil {
			return MetadataResponse{}, errors.New(
				"remote workspace analytics limits are invalid")
		}
	}
	if capabilities.Analytics &&
		(wire.AnalyticsLimits == nil || !wire.AnalyticsRecordingRequired) {
		return MetadataResponse{}, errors.New(
			"remote workspace analytics metadata is incomplete")
	}
	if !capabilities.Analytics &&
		(wire.AnalyticsLimits != nil || wire.AnalyticsRecordingRequired) {
		return MetadataResponse{}, errors.New(
			"remote workspace advertises analytics metadata without capability")
	}
	return MetadataResponse{
		MaxPageSize: MaxPageSize, MaxTopK: MaxTopK,
		MaxDepth: MaxDepth, MaxFanout: MaxFanout,
		MaxNodes: MaxNodes, MaxEdgeTypes: MaxEdgeTypes,
		MaxResponseBytes: MaxResponseBytes,
		MaxUploadFiles:   MaxUploadFiles, MaxUploadFileBytes: MaxUploadFileBytes,
		MaxUploadTotalBytes:        MaxUploadTotalBytes,
		AnalyticsLimits:            wire.AnalyticsLimits,
		AnalyticsRecordingRequired: wire.AnalyticsRecordingRequired,
		Capabilities:               capabilities,
	}, nil
}

func validateRemoteSnapshot(requested, actual Snapshot) error {
	if actual.ID == "" || actual.AsOf.IsZero() {
		return remoteContractError("remote response omitted snapshot identity", nil)
	}
	return validateRequestedSnapshot(requested, actual)
}

func validateRemoteDocumentSummary(summary explorer.DocumentSummary) error {
	if err := summary.Document.Validate(); err != nil {
		return remoteContractError("invalid remote document summary", err)
	}
	if err := summary.Revision.Validate(); err != nil {
		return remoteContractError("invalid remote revision summary", err)
	}
	if strings.TrimSpace(summary.SourceURI) == "" {
		return remoteContractError("remote document summary has a blank source URI", nil)
	}
	if summary.SourceMediaType != "" && !knownExplorerMediaType(summary.SourceMediaType) {
		return remoteContractError("remote document summary has an invalid media type", nil)
	}
	if summary.Document.ID != summary.Revision.DocumentID ||
		summary.Document.RevisionID != summary.Revision.ID {
		return remoteContractError("remote document summary has inconsistent ownership", nil)
	}
	return nil
}

func validateRemoteIngestResponse(
	request []preparedUpload, response IngestResponse,
) error {
	if err := validateRemoteSnapshot(Snapshot{}, response.Snapshot); err != nil {
		return err
	}
	if len(response.Files) != len(request) {
		return remoteContractError("remote ingest response count does not match the request", nil)
	}
	seenDocuments := make(map[shoal.ID]struct{}, len(response.Files))
	for index, file := range response.Files {
		expected, err := explorer.AnalyzeSource(request[index].source)
		if err != nil {
			return remoteContractError("remote ingest request is invalid", err)
		}
		if file.Name != request[index].name {
			return remoteContractError("remote ingest response name does not match the request", nil)
		}
		if file.MediaType != request[index].mediaType {
			return remoteContractError("remote ingest response media type does not match the request", nil)
		}
		if file.Disposition != explorer.IngestApplied &&
			file.Disposition != explorer.IngestUnchanged {
			return remoteContractError("remote ingest response disposition is invalid", nil)
		}
		if file.SectionCount < 0 || file.SectionCount > document.MaxSectionsPerRevision ||
			file.SpanCount < 0 || file.SpanCount > document.MaxSpansPerRevision {
			return remoteContractError("remote ingest response counts exceed bounds", nil)
		}
		if err := file.Document.Validate(); err != nil {
			return remoteContractError("invalid remote ingested document", err)
		}
		if err := file.Revision.Validate(); err != nil {
			return remoteContractError("invalid remote ingested revision", err)
		}
		if file.Document.ID != file.Revision.DocumentID ||
			file.Document.RevisionID != file.Revision.ID {
			return remoteContractError("remote ingested document has inconsistent ownership", nil)
		}
		if file.Document.ID != expected.Document.ID ||
			file.Revision.ID != expected.Revision.ID ||
			file.Document.RootSectionID != expected.Document.RootSectionID ||
			file.Document.Title != expected.Document.Title ||
			file.Revision.SourceVersion != expected.Revision.SourceVersion ||
			!maps.Equal(file.Document.Metadata, expected.Document.Metadata) ||
			!maps.Equal(file.Revision.Metadata, expected.Revision.Metadata) ||
			file.SectionCount != expected.SectionCount ||
			file.SpanCount != expected.SpanCount {
			return remoteContractError("remote ingest response does not match the uploaded source", nil)
		}
		if _, duplicate := seenDocuments[file.Document.ID]; duplicate {
			return remoteContractError("remote ingest response has duplicate document IDs", nil)
		}
		seenDocuments[file.Document.ID] = struct{}{}
	}
	return nil
}

func compareDocumentSummary(left, right explorer.DocumentSummary) int {
	if left.Document.Title < right.Document.Title {
		return -1
	}
	if left.Document.Title > right.Document.Title {
		return 1
	}
	return shoal.CompareID(left.Document.ID, right.Document.ID)
}

func validateRemoteDocumentView(view explorer.DocumentView) error {
	if err := validateRemoteDocumentSummary(explorer.DocumentSummary{
		Document: view.Document, Revision: view.Revision, SourceURI: view.SourceURI,
		SourceMediaType: view.SourceMediaType,
	}); err != nil {
		return err
	}
	if view.Root.Section.ID != view.Document.RootSectionID {
		return remoteContractError("remote document root does not match document", nil)
	}
	state := remoteDocumentValidation{
		seenSections: make(map[shoal.ID]struct{}),
		seenSpans:    make(map[shoal.ID]struct{}),
	}
	return validateRemoteSectionView(
		view.Root, view.Document.ID, view.Revision.ID, "",
		document.SourceRange{}, false, &state,
	)
}

type remoteDocumentValidation struct {
	sectionCount int
	spanCount    int
	seenSections map[shoal.ID]struct{}
	seenSpans    map[shoal.ID]struct{}
}

func validateRemoteSectionView(
	view explorer.SectionView,
	documentID shoal.ID,
	revisionID shoal.ID,
	parentID shoal.ID,
	parentRange document.SourceRange,
	hasParent bool,
	state *remoteDocumentValidation,
) error {
	if err := view.Section.Validate(); err != nil {
		return remoteContractError("invalid remote document section", err)
	}
	if !sourceRangeWithinRevisionBound(view.Section.Range) {
		return remoteContractError("remote section range exceeds revision bounds", nil)
	}
	state.sectionCount++
	if state.sectionCount > document.MaxSectionsPerRevision {
		return remoteContractError("remote document has too many sections", nil)
	}
	if _, duplicate := state.seenSections[view.Section.ID]; duplicate {
		return remoteContractError("remote document has duplicate section IDs", nil)
	}
	state.seenSections[view.Section.ID] = struct{}{}
	if view.Section.DocumentID != documentID ||
		view.Section.RevisionID != revisionID ||
		view.Section.ParentID != parentID {
		return remoteContractError("remote section has inconsistent ownership", nil)
	}
	if hasParent && !sourceRangeContains(parentRange, view.Section.Range) {
		return remoteContractError("remote section range is outside its parent", nil)
	}
	orders := make(map[uint32]struct{}, len(view.Spans)+len(view.Children))
	for index, span := range view.Spans {
		if err := span.Validate(); err != nil {
			return remoteContractError("invalid remote document span", err)
		}
		if index > 0 && view.Spans[index-1].Order > span.Order {
			return remoteContractError("remote spans are not ordered", nil)
		}
		if !sourceRangeWithinRevisionBound(span.Range) {
			return remoteContractError("remote span range exceeds revision bounds", nil)
		}
		if int64(len(span.Text)) != span.Range.End.Offset-span.Range.Start.Offset {
			return remoteContractError("remote span text does not match its source range", nil)
		}
		state.spanCount++
		if state.spanCount > document.MaxSpansPerRevision {
			return remoteContractError("remote document has too many spans", nil)
		}
		if _, duplicate := state.seenSpans[span.ID]; duplicate {
			return remoteContractError("remote document has duplicate span IDs", nil)
		}
		state.seenSpans[span.ID] = struct{}{}
		if _, duplicate := orders[span.Order]; duplicate {
			return remoteContractError("remote document has duplicate sibling order", nil)
		}
		orders[span.Order] = struct{}{}
		if span.DocumentID != documentID ||
			span.RevisionID != revisionID ||
			span.SectionID != view.Section.ID {
			return remoteContractError("remote span has inconsistent ownership", nil)
		}
		if !sourceRangeContains(view.Section.Range, span.Range) {
			return remoteContractError("remote span range is outside its section", nil)
		}
	}
	for index, child := range view.Children {
		if _, duplicate := orders[child.Section.Order]; duplicate {
			return remoteContractError("remote document has duplicate sibling order", nil)
		}
		orders[child.Section.Order] = struct{}{}
		if index > 0 && view.Children[index-1].Section.Order > child.Section.Order {
			return remoteContractError("remote child sections are not ordered", nil)
		}
		if err := validateRemoteSectionView(
			child, documentID, revisionID, view.Section.ID,
			view.Section.Range, true, state,
		); err != nil {
			return err
		}
	}
	return nil
}

func sourceRangeContains(outer, inner document.SourceRange) bool {
	if inner.Start.Offset < outer.Start.Offset || inner.End.Offset > outer.End.Offset {
		return false
	}
	if outer.Start.Page > 0 && inner.Start.Page > 0 && inner.Start.Page < outer.Start.Page {
		return false
	}
	if outer.End.Page > 0 && inner.End.Page > 0 && inner.End.Page > outer.End.Page {
		return false
	}
	return true
}

func sourceRangeWithinRevisionBound(value document.SourceRange) bool {
	return value.Start.Offset >= 0 &&
		value.End.Offset >= value.Start.Offset &&
		value.End.Offset <= document.MaxRevisionSourceBytes
}

func validateRemoteNeighborhood(
	request NeighborhoodRequest,
	neighborhood explorer.Neighborhood,
	maxNodes uint32,
	fanout uint32,
) error {
	if uint32(len(neighborhood.Nodes)) > maxNodes {
		return remoteContractError("remote neighborhood exceeds max_nodes", nil)
	}
	edgeLimit := uint64(maxNodes) * uint64(fanout)
	if uint64(len(neighborhood.Edges)) > edgeLimit {
		return remoteContractError("remote neighborhood exceeds fanout bounds", nil)
	}
	if request.Depth == 1 && len(request.NodeIDs) == 1 &&
		uint32(len(neighborhood.Edges)) > fanout {
		return remoteContractError("remote neighborhood exceeds fanout bounds", nil)
	}
	nodes := make(map[shoal.ID]struct{}, len(neighborhood.Nodes))
	for index, node := range neighborhood.Nodes {
		if index > 0 &&
			shoal.CompareID(neighborhood.Nodes[index-1].ID, node.ID) > 0 {
			return remoteContractError("remote graph nodes are not ordered", nil)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return remoteContractError("remote neighborhood has duplicate node IDs", nil)
		}
		if err := node.Validate(); err != nil {
			return remoteContractError("invalid remote graph node", err)
		}
		nodes[node.ID] = struct{}{}
	}
	for _, seed := range request.NodeIDs {
		if _, ok := nodes[seed]; !ok {
			return remoteContractError("remote neighborhood omitted a requested seed", nil)
		}
	}
	edges := make(map[shoal.ID]struct{}, len(neighborhood.Edges))
	adjacency := make(map[shoal.ID][]shoal.ID, len(neighborhood.Nodes))
	for index, edge := range neighborhood.Edges {
		if index > 0 &&
			shoal.CompareID(neighborhood.Edges[index-1].ID, edge.ID) > 0 {
			return remoteContractError("remote graph edges are not ordered", nil)
		}
		if _, duplicate := edges[edge.ID]; duplicate {
			return remoteContractError("remote neighborhood has duplicate edge IDs", nil)
		}
		edges[edge.ID] = struct{}{}
		if err := edge.Validate(); err != nil {
			return remoteContractError("invalid remote graph edge", err)
		}
		if _, ok := nodes[edge.From]; !ok {
			return remoteContractError("remote edge source is outside the neighborhood", nil)
		}
		if _, ok := nodes[edge.To]; !ok {
			return remoteContractError("remote edge target is outside the neighborhood", nil)
		}
		if !edgeTypeAllowed(edge.Type, request.EdgeTypes) {
			return remoteContractError(
				"remote neighborhood contains an excluded edge type", nil)
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		adjacency[edge.To] = append(adjacency[edge.To], edge.From)
	}
	if _, ok := remoteNeighborhoodDistances(
		nodes, adjacency, request.NodeIDs, request.Depth,
	); !ok {
		return remoteContractError(
			"remote neighborhood contains nodes outside the bounded expansion", nil)
	}
	if !remoteNeighborhoodExpansionFeasible(
		neighborhood.Edges, request.NodeIDs, request.Depth, fanout,
	) {
		return remoteContractError("remote neighborhood exceeds fanout bounds", nil)
	}
	return nil
}

func remoteNeighborhoodDistances(
	nodes map[shoal.ID]struct{},
	adjacency map[shoal.ID][]shoal.ID,
	seeds []shoal.ID,
	depth uint32,
) (map[shoal.ID]uint32, bool) {
	distances := make(map[shoal.ID]uint32, len(nodes))
	frontier := make([]shoal.ID, 0, len(seeds))
	for _, seed := range seeds {
		distances[seed] = 0
		frontier = append(frontier, seed)
	}
	for level := uint32(0); level < depth && len(frontier) > 0; level++ {
		next := make([]shoal.ID, 0)
		for _, id := range frontier {
			for _, other := range adjacency[id] {
				if _, ok := distances[other]; ok {
					continue
				}
				distances[other] = level + 1
				next = append(next, other)
			}
		}
		frontier = next
	}
	return distances, len(distances) == len(nodes)
}

func remoteNeighborhoodExpansionFeasible(
	edges []graph.Edge,
	seeds []shoal.ID,
	depth uint32,
	fanout uint32,
) bool {
	nodes := make(map[shoal.ID]struct{}, len(seeds)+2*len(edges))
	for _, seed := range seeds {
		nodes[seed] = struct{}{}
	}
	incident := make(map[shoal.ID][]int, len(nodes))
	for index, edge := range edges {
		nodes[edge.From] = struct{}{}
		nodes[edge.To] = struct{}{}
		incident[edge.From] = append(incident[edge.From], index)
		if edge.To != edge.From {
			incident[edge.To] = append(incident[edge.To], index)
		}
	}
	for node := range incident {
		sort.Slice(incident[node], func(i, j int) bool {
			return shoal.CompareID(edges[incident[node][i]].ID, edges[incident[node][j]].ID) < 0
		})
	}
	reached := make(map[shoal.ID]uint32, len(nodes))
	frontier := make([]shoal.ID, 0, len(seeds))
	for _, seed := range seeds {
		if _, ok := reached[seed]; ok {
			continue
		}
		reached[seed] = 0
		frontier = append(frontier, seed)
	}
	expandedEdges := make(map[shoal.ID]struct{}, len(edges))
	for level := uint32(0); level < depth && len(frontier) > 0; level++ {
		next := make([]shoal.ID, 0)
		for _, node := range frontier {
			edgeIndexes := incident[node]
			if uint32(len(edgeIndexes)) > fanout {
				edgeIndexes = edgeIndexes[:fanout]
			}
			for _, edgeIndex := range edgeIndexes {
				edge := edges[edgeIndex]
				expandedEdges[edge.ID] = struct{}{}
				other := edge.To
				if other == node {
					other = edge.From
				}
				if _, ok := reached[other]; ok {
					continue
				}
				reached[other] = level + 1
				next = append(next, other)
			}
		}
		frontier = next
	}
	if len(reached) != len(nodes) {
		return false
	}
	for _, edge := range edges {
		if _, ok := expandedEdges[edge.ID]; !ok {
			return false
		}
	}
	return true
}

func edgeTypeAllowed(edgeType string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if edgeType == candidate {
			return true
		}
	}
	return false
}

func remoteContractError(message string, err error) error {
	if err != nil {
		message += ": " + err.Error()
	}
	return shoal.NewError(shoal.ErrorInternal, message)
}

func decodeOneJSON(reader io.Reader, value any, limit int64) error {
	limited := &io.LimitedReader{R: responseReader{reader: reader}, N: limit + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("response body exceeds the remote workspace bound")
	}
	var extra any
	err := decoder.Decode(&extra)
	if limited.N <= 0 {
		return errors.New("response body exceeds the remote workspace bound")
	}
	if !errors.Is(err, io.EOF) {
		return errors.New("response body must contain one JSON object")
	}
	return nil
}

type responseReader struct {
	reader io.Reader
}

func (r responseReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, responseReadError{err: err}
	}
	return n, err
}

type responseReadError struct {
	err error
}

func (e responseReadError) Error() string {
	return e.err.Error()
}

func (e responseReadError) Unwrap() error {
	return e.err
}

func decodeRemoteError(response *http.Response) error {
	decoded, _ := decodeRemoteErrorOutcome(response)
	return decoded
}

func decodeRemoteErrorOutcome(response *http.Response) (error, bool) {
	var payload struct {
		Code          shoal.ErrorCode           `json:"code"`
		Message       string                    `json:"message"`
		Embedding     *wireEmbeddingQueryReport `json:"embedding,omitempty"`
		Indeterminate bool                      `json:"indeterminate,omitempty"`
	}
	indeterminate := response.Header.Get(CommitOutcomeHeader) ==
		CommitOutcomeIndeterminate
	err := decodeOneJSON(response.Body, &payload, maxRemoteMetadataResponseBytes)
	if err == nil && isKnownErrorCode(payload.Code) {
		if payload.Code != errorCodeFromHTTPStatus(response.StatusCode) {
			mismatch := errorFromHTTPStatus(
				response.StatusCode,
				"remote workspace error code does not match status",
			)
			explicit := indeterminate || payload.Indeterminate
			return markRemoteIndeterminate(mismatch, explicit), explicit
		}
		message := trimErrorCode(payload.Code, payload.Message)
		var decoded error
		switch payload.Code {
		case shoal.ErrorCanceled:
			decoded = shoal.WrapError(payload.Code, message, context.Canceled)
		case shoal.ErrorDeadline:
			decoded = shoal.WrapError(payload.Code, message, context.DeadlineExceeded)
		default:
			decoded = shoal.NewError(payload.Code, message)
		}
		report, reportErr := embeddingQueryReportValue(payload.Embedding)
		if reportErr != nil {
			return markRemoteIndeterminate(
				remoteContractError(
					"invalid remote embedding query report", reportErr),
				indeterminate || payload.Indeterminate,
			), indeterminate || payload.Indeterminate
		}
		if report != nil {
			if !report.Degraded {
				return markRemoteIndeterminate(
					remoteContractError(
						"remote error carried a non-degraded embedding query report",
						nil,
					),
					indeterminate || payload.Indeterminate,
				), indeterminate || payload.Indeterminate
			}
			decoded = newEmbeddingQueryError(decoded, *report)
		}
		return markRemoteIndeterminate(
			decoded, indeterminate || payload.Indeterminate), true
	}
	if err != nil && isRemoteTransportDecodeError(err) {
		return markRemoteIndeterminate(shoal.WrapError(
			remoteDecodeCode(err), "read remote workspace error response", err),
			indeterminate), indeterminate
	}
	return markRemoteIndeterminate(errorFromHTTPStatus(
		response.StatusCode, "remote workspace request failed"),
		indeterminate), indeterminate
}

func markRemoteIndeterminate(err error, indeterminate bool) error {
	if !indeterminate {
		return err
	}
	return explorer.MarkIndeterminateCommit(err)
}

func trimErrorCode(code shoal.ErrorCode, message string) string {
	return strings.TrimPrefix(message, string(code)+": ")
}

func remoteTransportCode(err error) shoal.ErrorCode {
	if errors.Is(err, context.Canceled) {
		return shoal.ErrorCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return shoal.ErrorDeadline
	}
	return shoal.ErrorUnavailable
}

func remoteDecodeCode(err error) shoal.ErrorCode {
	var readErr responseReadError
	if errors.As(err, &readErr) {
		return remoteTransportCode(err)
	}
	if errors.Is(err, context.Canceled) {
		return shoal.ErrorCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return shoal.ErrorDeadline
	}
	return shoal.ErrorInternal
}

func isRemoteTransportDecodeError(err error) bool {
	var readErr responseReadError
	return errors.As(err, &readErr) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func errorFromHTTPStatus(status int, message string) error {
	code := errorCodeFromHTTPStatus(status)
	switch code {
	case shoal.ErrorCanceled:
		return shoal.WrapError(code, message, context.Canceled)
	case shoal.ErrorDeadline:
		return shoal.WrapError(code, message, context.DeadlineExceeded)
	default:
		return shoal.NewError(code, message)
	}
}

func errorCodeFromHTTPStatus(status int) shoal.ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return shoal.ErrorInvalidArgument
	case http.StatusUnauthorized, http.StatusForbidden:
		return shoal.ErrorUnauthorized
	case http.StatusNotFound:
		return shoal.ErrorNotFound
	case http.StatusConflict:
		return shoal.ErrorConflict
	case http.StatusServiceUnavailable:
		return shoal.ErrorUnavailable
	case http.StatusGatewayTimeout:
		return shoal.ErrorDeadline
	case 499:
		return shoal.ErrorCanceled
	case http.StatusInternalServerError:
		return shoal.ErrorInternal
	default:
		return shoal.ErrorUnavailable
	}
}

func isKnownErrorCode(code shoal.ErrorCode) bool {
	switch code {
	case shoal.ErrorInvalidArgument, shoal.ErrorNotFound, shoal.ErrorConflict,
		shoal.ErrorUnauthorized, shoal.ErrorUnavailable, shoal.ErrorCanceled,
		shoal.ErrorDeadline, shoal.ErrorInternal:
		return true
	default:
		return false
	}
}

var _ Service = (*RemoteService)(nil)
var _ CapabilityProvider = (*RemoteService)(nil)
var _ MetadataProvider = (*RemoteService)(nil)
