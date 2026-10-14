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

// Package interaction defines the reserved knowledge-graph namespace that
// records how an inference was served: which session ran, which turns it took,
// which tool calls it made, what those calls retrieved, and what the answer
// actually cited.
//
// Interaction nodes live in the same corpus as content but under a reserved
// kind namespace. Retrieval excludes them by default; they are reachable only
// by explicit traversal from an interaction seed or by an explicit
// kind-scoped query. A model must never be able to cite its own prior output
// as though it were source evidence.
//
// An interaction node carries the conjunction of every visibility label of
// every source node it touched. Visibility is never derived from the asker's
// grant set, because that would let a highly cleared user's session become a
// covert channel. A reviewed declassification path is deliberately absent from
// this package; it is a later, explicit, authority-bearing action.
package interaction

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// KindPrefix reserves the interaction node-kind namespace. Content ingestion
// must never mint a node kind under this prefix.
const KindPrefix = "interaction."

// Reserved interaction node kinds.
const (
	KindSession   = KindPrefix + "session"
	KindInference = KindPrefix + "inference"
	KindTurn      = KindPrefix + "turn"
	KindToolCall  = KindPrefix + "tool_call"
	KindTombstone = KindPrefix + "tombstone"
	KindFold      = KindPrefix + "fold"
)

// EdgeTypePrefix reserves the interaction edge-type namespace.
const EdgeTypePrefix = "interaction."

// Reserved interaction edge types. Retrieved and cited are deliberately
// distinct: what the model was shown is a larger set than what it cited, and
// visibility must be derived from everything it was shown.
const (
	EdgeHasTurn      = EdgeTypePrefix + "has_turn"
	EdgeHasInference = EdgeTypePrefix + "has_inference"
	EdgeHasToolCall  = EdgeTypePrefix + "has_tool_call"
	EdgeRetrieved    = EdgeTypePrefix + "retrieved"
	EdgeCited        = EdgeTypePrefix + "cited"
	// EdgeFolds points from a fold summary to a session it folds. It is what
	// makes a fold rehydratable and what makes cross-session traversal
	// possible without widening anything.
	EdgeFolds = EdgeTypePrefix + "folds"
)

// Node and edge property keys. Values are identities, digests, counts, and
// visibility labels only. Prompts, questions, quotes, credentials, and
// model-chosen correlation strings are never persisted here.
const (
	// PropertyVisibility carries the canonical conjunction expression. It is
	// also the key content ingestion uses to declare a source's visibility.
	PropertyVisibility = "shoal.visibility"
	// Derived nodes whose complete conjunction exceeds one graph metadata
	// value carry its digest and term count in the graph. The full uncapped
	// conjunction remains in the durable interaction envelope and explicit
	// typed view.
	PropertyVisibilityDigest = "interaction.visibility_digest"
	PropertyVisibilityCount  = "interaction.visibility_count"

	PropertySessionID       = "interaction.session_id"
	PropertyInferenceID     = "interaction.inference_id"
	PropertyTurnID          = "interaction.turn_id"
	PropertyIndex           = "interaction.index"
	PropertyDecision        = "interaction.decision"
	PropertyToolKind        = "interaction.tool_kind"
	PropertyRetrieved       = "interaction.retrieved_count"
	PropertyCited           = "interaction.cited_count"
	PropertyTurnCount       = "interaction.turn_count"
	PropertyStopReason      = "interaction.stop_reason"
	PropertyRecordedAt      = "interaction.recorded_at"
	PropertyDeletedAt       = "interaction.deleted_at"
	PropertyNodeCount       = "interaction.node_count"
	PropertyEdgeCount       = "interaction.edge_count"
	PropertyQueryDigest     = "interaction.query_digest"
	PropertyRequestID       = "interaction.request_id"
	PropertyContextPackID   = "interaction.context_pack_id"
	PropertyResultID        = "interaction.result_id"
	PropertyInputTokens     = "interaction.input_tokens"
	PropertyOutputTokens    = "interaction.output_tokens"
	PropertyFailed          = "interaction.failed"
	PropertyHarness         = "interaction.harness"
	PropertyProvider        = "interaction.provider"
	PropertyModel           = "interaction.model"
	PropertyModelVersion    = "interaction.model_version"
	PropertyPromptID        = "interaction.prompt_template_id"
	PropertyPromptVersion   = "interaction.prompt_version"
	PropertyPromptHash      = "interaction.prompt_hash"
	PropertyToolPolicy      = "interaction.tool_policy"
	PropertySnapshotID      = "interaction.snapshot_id"
	PropertySnapshotAsOf    = "interaction.snapshot_as_of"
	PropertyAuthFingerprint = "interaction.authorization_fingerprint"
	PropertyAuthExpiresAt   = "interaction.authorization_expires_at"
	PropertyAuthOperation   = "interaction.authorization_operation"
	PropertyOntologySchema  = "interaction.ontology_schema_id"
	PropertyOntologyVersion = "interaction.ontology_version_id"
	PropertyEmbeddingSpace  = "interaction.embedding_space"
	PropertyEmbeddingSpaces = "interaction.embedding_space_set_digest"
	PropertyEmbeddingCount  = "interaction.embedding_space_count"
	PropertyOperation       = "interaction.operation"
	PropertySubjectID       = "interaction.subject_id"
	PropertyActorID         = "interaction.actor_id"
	PropertyClientID        = "interaction.client_id"
	PropertyDelegationCount = "interaction.delegation_count"
	PropertyDelegationID    = "interaction.delegation_id"
	PropertyReasonCode      = "interaction.reason_code"
	PropertyReasonDigest    = "interaction.reason_digest"

	// PropertySummaryDigest carries the SHA-256 digest of a fold's
	// out-of-band summary text. The digest is stored so a fold can be
	// correlated with the summary it describes; the summary text itself is
	// never persisted, because it is derived from evidence the record is not
	// allowed to carry.
	PropertySummaryDigest = "interaction.summary_digest"
	PropertyFoldedAt      = "interaction.folded_at"
	PropertyFoldedCount   = "interaction.folded_session_count"
)

// LabelInteraction marks every interaction node so label-based consumers can
// exclude them without parsing kinds.
const LabelInteraction = "interaction"

// Public static bounds. They exist so a single recorded session can never
// become an unbounded write.
const (
	MaxTurns = 4096
	// MaxTouchedNodes and MaxVisibilityLabels are retained as compatibility
	// sizing constants only. They are not semantic truncation or rejection
	// limits: provenance and visibility unions must include every source.
	MaxTouchedNodes      = 65536
	MaxVisibilityLabels  = 64
	MaxVisibilityLabelSz = 256
	MaxIdentifierBytes   = 256
	MaxFoldMembers       = 4096
	MaxDelegationEntries = 64
)

// IsInteractionKind reports whether a node kind is in the reserved namespace.
func IsInteractionKind(kind string) bool {
	return strings.HasPrefix(kind, KindPrefix)
}

// IsInteractionEdgeType reports whether an edge type is in the reserved
// namespace.
func IsInteractionEdgeType(edgeType string) bool {
	return strings.HasPrefix(edgeType, EdgeTypePrefix)
}

// ToolCall is one tool invocation made inside a turn. RetrievedNodeIDs are the
// source graph nodes the call put in front of the model, not only the ones it
// went on to cite.
type ToolCall struct {
	Kind              string
	RetrievedNodeIDs  []shoal.ID
	RetrievedEvidence []EvidenceReference
}

// Operation identifies the product interaction represented by a session.
// Inference and chat operations materialize an addressable inference node;
// retrieval and tool-call operations remain session/turn/tool-call records.
type Operation string

const (
	OperationInference Operation = "inference"
	OperationRetrieval Operation = "retrieval"
	OperationToolCall  Operation = "tool_call"
	OperationChat      Operation = "chat"
)

// Validate rejects unknown interaction operations.
func (o Operation) Validate() error {
	switch o {
	case OperationInference, OperationRetrieval, OperationToolCall, OperationChat:
		return nil
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown interaction operation")
	}
}

// HasInference reports whether this operation produces an addressable
// inference node in addition to its session, turns, and tool calls.
func (o Operation) HasInference() bool {
	return o == OperationInference || o == OperationChat
}

// ActorContext is the trusted, non-credential identity context under which an
// interaction ran. The complete delegation chain is retained in the typed
// durable record; graph metadata carries only its count and stable opaque ID.
type ActorContext struct {
	SubjectID  shoal.ID
	ActorID    shoal.ID
	ClientID   shoal.ID
	OnBehalfOf []shoal.ID
}

// Validate checks actor and delegation identities without interpreting them.
func (a ActorContext) Validate() error {
	present := a.SubjectID != "" || a.ActorID != "" || a.ClientID != "" ||
		len(a.OnBehalfOf) > 0
	if !present {
		return nil
	}
	if err := shoal.ValidateRequiredID(
		"interaction subject ID", a.SubjectID,
	); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"interaction actor ID", a.ActorID,
	); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"interaction client ID", a.ClientID,
	); err != nil {
		return err
	}
	if len(a.OnBehalfOf) > MaxDelegationEntries {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction delegation chain exceeds the public bound",
		)
	}
	for _, id := range a.OnBehalfOf {
		if err := shoal.ValidateRequiredID(
			"interaction delegation identity", id,
		); err != nil {
			return err
		}
	}
	return nil
}

// Reason is redacted action rationale. Code is a bounded machine-readable
// category; Digest is SHA-256 of any free-form explanation. Raw reason text is
// never persisted.
type Reason struct {
	Code   string
	Digest string
}

// NewReason creates a safe reason from a category and optional free-form
// detail. The detail is hashed immediately and is never retained.
func NewReason(code, detail string) (Reason, error) {
	reason := Reason{Code: code}
	if detail != "" {
		reason.Digest = Digest(detail)
	}
	if err := reason.Validate(); err != nil {
		return Reason{}, err
	}
	return reason, nil
}

// Validate checks the redacted reason shape.
func (r Reason) Validate() error {
	if r.Code == "" && r.Digest == "" {
		return nil
	}
	if r.Code == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction reason code is required")
	}
	if len(r.Code) > MaxVisibilityLabelSz {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction reason code exceeds the public byte bound",
		)
	}
	for index := 0; index < len(r.Code); index++ {
		value := r.Code[index]
		switch {
		case value >= 'a' && value <= 'z',
			value >= 'A' && value <= 'Z',
			value >= '0' && value <= '9':
		case value == '_', value == '-', value == '.', value == ':':
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction reason code contains an unsupported character",
			)
		}
	}
	if r.Digest == "" {
		return nil
	}
	return validateDigest("interaction reason digest", r.Digest, false)
}

// CallerAssertedReason is rationale the authenticated caller asserted for an
// interaction. It is deliberately a different type and field from Reason:
// Reason is derived by the authorization boundary from the trusted decision,
// while a caller-asserted reason is only what the caller said. Shoal bounds
// its shape, stores it in the durable record attributed to the session's
// Actor, and lets producers bind it into their retry identity so a divergent
// retry conflicts. It never verifies the assertion and never authorizes
// anything on it.
//
// Raw free-form text is never persisted: free-form detail is retained only as
// DetailDigest. Source is the one verbatim value, and only for a producer that
// constrains it to a fixed content-address shape before recording, such as an
// ATPL policy digest ("atpl:policy:v1:<sha256 hex>").
type CallerAssertedReason struct {
	// Code is the caller's bounded machine-readable reason category.
	Code string
	// DetailDigest is SHA-256 of free-form detail the caller supplied.
	DetailDigest string
	// Source is a content address the caller asserted as the source of the
	// interaction, retained verbatim. It is mutually exclusive with
	// DetailDigest.
	Source string
}

// IsZero reports whether no caller-asserted reason is present.
func (r CallerAssertedReason) IsZero() bool {
	return r == CallerAssertedReason{}
}

// Validate checks the bounded caller-asserted reason shape.
func (r CallerAssertedReason) Validate() error {
	if r.IsZero() {
		return nil
	}
	if r.Code == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction caller-asserted reason code is required",
		)
	}
	if err := validateReasonToken(
		"interaction caller-asserted reason code", r.Code,
	); err != nil {
		return err
	}
	if r.DetailDigest != "" && r.Source != "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction caller-asserted reason cannot carry both a detail "+
				"digest and a source",
		)
	}
	if r.Source != "" {
		return validateReasonToken(
			"interaction caller-asserted reason source", r.Source)
	}
	return validateDigest(
		"interaction caller-asserted reason detail digest",
		r.DetailDigest, true,
	)
}

// validateReasonToken applies Reason.Code's bound and charset to a retained
// caller-asserted value, so no caller-controlled byte outside it is persisted.
func validateReasonToken(name, value string) error {
	if len(value) > MaxVisibilityLabelSz {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" exceeds the public byte bound")
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
		case character == '_', character == '-', character == '.',
			character == ':':
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				name+" contains an unsupported character",
			)
		}
	}
	return nil
}

// Turn is one model decision. A turn that stopped rather than calling a tool
// carries no ToolCall.
type Turn struct {
	Index        int
	Decision     string
	InputTokens  int
	OutputTokens int
	Failed       bool
	ToolCall     *ToolCall
}

// Provenance is the redacted identity of the harness, model, prompt template,
// and tool policy that served an inference.
type Provenance struct {
	Harness      string
	Provider     string
	Model        string
	ModelVersion string
	PromptID     string
	PromptVer    string
	PromptHash   string
	ToolPolicy   string
}

// Session is one recorded inference. It carries identities, digests, counts,
// and the source node IDs it touched. It never carries the question, the
// prompt, the answer text, evidence quotes, authorization grants, or
// model-chosen correlation strings. Its one correlation string, CorrelationID,
// is the authenticated decision's, never a model's.
type Session struct {
	ID         shoal.ID
	RecordedAt time.Time
	Operation  Operation
	Actor      ActorContext
	Reason     Reason
	// CorrelationID is the correlation of the authenticated decision the
	// session was recorded under: the Shoal-Correlation-ID a caller supplied,
	// or the one its authenticator generated (#527, #532). It threads one
	// operation across hops — an approval and its dispatch, a gateway acting
	// for a user — which a request ID, naming one request, cannot.
	//
	// It is metadata only. It is caller-supplyable, so it is excluded from
	// everything that decides what a session is: it is not an input to any
	// session ID, it is not materialized into the interaction subgraph, and
	// an exact retry that differs only in correlation is the same session
	// (the first recorded correlation is kept). It is excluded from
	// AuthorizationFingerprint, as it is on the decision. It is never
	// authority and never attribution: Actor is who acted.
	//
	// A trusted sink stamps it from the decision, as it does Actor and
	// Reason; a producer's own value is not trusted. Empty means the session
	// was recorded before this field existed, without a decision correlation,
	// or with one that was not recordable (see RecordableCorrelationID).
	CorrelationID shoal.ID
	// CallerAssertedReason is what the authenticated Actor asserted, recorded
	// beside the trusted Reason and never in its place. It is not verified
	// and never participates in authorization.
	CallerAssertedReason     CallerAssertedReason
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
	AuthorizationFingerprint shoal.ID
	AuthorizationExpiresAt   time.Time
	// AuthorizationOperation is the exact trusted operation authorized for
	// this interaction. Empty retains the legacy retrieval authorization.
	AuthorizationOperation string
	// OntologySchemaID and OntologyVersionID pin the exact read-time lens.
	OntologySchemaID  shoal.ID
	OntologyVersionID shoal.ID
	// EmbeddingSpaceID is retained for legacy single/set-ID records.
	EmbeddingSpaceID shoal.ID
	// EmbeddingSpaces carries canonical stable full identities from the core
	// request-local embedding query observer.
	EmbeddingSpaces EmbeddingSpaceSet
	Provenance      Provenance
	QueryDigest     string
	RequestID       shoal.ID
	ContextPackID   shoal.ID
	ResultID        shoal.ID
	StopReason      string
	// RequiredVisibility is a producer-supplied output restriction conjoined
	// with every touched source label. It can narrow but never replace
	// source-derived visibility.
	RequiredVisibility []string

	// SeedNodeIDs are source nodes the session was shown before its first
	// turn. They count as retrieved.
	SeedNodeIDs  []shoal.ID
	SeedEvidence []EvidenceReference
	Turns        []Turn
	// CitedNodeIDs are source nodes the final answer actually cited.
	CitedNodeIDs  []shoal.ID
	CitedEvidence []EvidenceReference
}

// Subgraph is the materialized interaction record: its own nodes, its edges to
// itself and to the source nodes it touched, and the conjoined visibility the
// whole record requires.
type Subgraph struct {
	Nodes      []graph.Node
	Edges      []graph.Edge
	Visibility []string
	// TouchedNodeIDs is the sorted union of every source node the session was
	// shown or cited. Visibility is the conjunction over exactly this set.
	TouchedNodeIDs []shoal.ID
	// TouchedEdgeIDs is the sorted union of every exact source edge present in
	// retrieved or cited evidence. These edges are not copied into the derived
	// interaction graph, but their current existence and policy remain
	// required to serve it.
	TouchedEdgeIDs []shoal.ID
}

// VisibilityResolver reports the visibility labels a source node requires. It
// must return an error for a node it cannot resolve so recording fails closed
// rather than silently under-labeling an interaction record.
type VisibilityResolver func(shoal.ID) ([]string, error)

// NodeVisibility reads the declared visibility labels of a graph node. A node
// with no declared labels is public.
func NodeVisibility(node graph.Node) ([]string, error) {
	return metadataVisibility(node.Properties)
}

// EdgeVisibility reads the declared visibility labels of a graph edge. A
// source edge with no declared labels is public.
func EdgeVisibility(edge graph.Edge) ([]string, error) {
	return metadataVisibility(edge.Properties)
}

func metadataVisibility(properties shoal.Metadata) ([]string, error) {
	if properties[PropertyVisibility] == "" &&
		properties[PropertyVisibilityDigest] != "" {
		if err := validateDigest(
			"interaction visibility digest",
			properties[PropertyVisibilityDigest],
			false,
		); err != nil {
			return nil, err
		}
		count, err := strconv.Atoi(properties[PropertyVisibilityCount])
		if err != nil || count <= 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction visibility count is invalid",
			)
		}
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"interaction visibility is digest-only and requires the explicit "+
				"derived record view",
		)
	}
	return ParseVisibility(properties[PropertyVisibility])
}

// ParseVisibility parses a canonical conjunction expression into sorted unique
// labels. An empty expression is public.
func ParseVisibility(expression string) ([]string, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, nil
	}
	parts := strings.Split(expression, "&")
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		label := strings.TrimSpace(part)
		if err := validateLabel(label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return Conjoin(labels)
}

// Conjoin returns the conjunction of one or more visibility label sets: the
// sorted unique union of every label, because a reader must hold all of them.
func Conjoin(sets ...[]string) ([]string, error) {
	seen := make(map[string]struct{})
	for _, set := range sets {
		for _, label := range set {
			if err := validateLabel(label); err != nil {
				return nil, err
			}
			seen[label] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	labels := make([]string, 0, len(seen))
	for label := range seen {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels, nil
}

// Expression renders sorted labels as a canonical conjunction. Public
// visibility renders as the empty string.
func Expression(labels []string) string {
	return strings.Join(labels, "&")
}

func validatePrefixedDigest(name, digest string, optional bool) error {
	if strings.HasPrefix(digest, "sha256:") {
		digest = strings.TrimPrefix(digest, "sha256:")
	}
	return validateDigest(name, digest, optional)
}

func validateIdentifier(name, value string, optional bool) error {
	if value == "" {
		if optional {
			return nil
		}
		return shoal.NewError(shoal.ErrorInvalidArgument, name+" is required")
	}
	if len(value) > MaxIdentifierBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" exceeds the public byte bound")
	}
	for index := 0; index < len(value); index++ {
		c := value[index]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == ':', c == '/', c == '@',
			c == '+':
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				name+" contains an unsupported character",
			)
		}
	}
	return nil
}

// ValidateLabel checks one free-form visibility label against the public
// label charset [A-Za-z0-9_.:-] and the MaxVisibilityLabelSz bound. It never
// folds, trims or otherwise rewrites the label: a label that is not already
// exactly valid is refused.
func ValidateLabel(label string) error { return validateLabel(label) }

func validateLabel(label string) error {
	if label == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "visibility label cannot be empty")
	}
	if len(label) > MaxVisibilityLabelSz {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"visibility label exceeds the public byte bound",
		)
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == ':':
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"visibility label contains an unsupported character",
			)
		}
	}
	return nil
}

// Validate checks the static shape of a session before it is materialized.
func (s Session) Validate() error {
	if err := shoal.ValidateRequiredID("interaction session ID", s.ID); err != nil {
		return err
	}
	// Session identities must live in the session-owned part of the reserved
	// interaction namespace, which every source publication path rejects. That
	// disjointness is what makes the graph-ID reservation safe across writers:
	// two processes with independent local state can never durably commit a
	// source node and a session (or a session and another kind of derived
	// interaction node) under one graph identity.
	if !IsSessionID(s.ID) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction session ID must be minted by SessionID or "+
				"OperationSessionID in the reserved session namespace",
		)
	}
	if s.RecordedAt.IsZero() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction session time is required")
	}
	if s.Operation != "" {
		if err := s.Operation.Validate(); err != nil {
			return err
		}
	}
	if err := s.Actor.Validate(); err != nil {
		return err
	}
	if err := s.Reason.Validate(); err != nil {
		return err
	}
	if err := s.CallerAssertedReason.Validate(); err != nil {
		return err
	}
	if err := ValidateCorrelationID(s.CorrelationID); err != nil {
		return err
	}
	if err := validateDigest(
		"interaction query digest", s.QueryDigest, true,
	); err != nil {
		return err
	}
	if err := validatePrefixedDigest(
		"interaction prompt hash", s.Provenance.PromptHash, true,
	); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"interaction harness", s.Provenance.Harness},
		{"interaction provider", s.Provenance.Provider},
		{"interaction model", s.Provenance.Model},
		{"interaction model version", s.Provenance.ModelVersion},
		{"interaction prompt ID", s.Provenance.PromptID},
		{"interaction prompt version", s.Provenance.PromptVer},
		{"interaction tool policy", s.Provenance.ToolPolicy},
		{"interaction stop reason", s.StopReason},
	} {
		if err := validateIdentifier(field.name, field.value, true); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name string
		id   shoal.ID
	}{
		{"interaction request ID", s.RequestID},
		{"interaction context pack ID", s.ContextPackID},
		{"interaction result ID", s.ResultID},
	} {
		if err := shoal.ValidateOptionalID(field.name, field.id); err != nil {
			return err
		}
	}
	hasExecutionPin := s.SnapshotID != "" || !s.SnapshotAsOf.IsZero() ||
		s.AuthorizationFingerprint != "" || !s.AuthorizationExpiresAt.IsZero()
	if hasExecutionPin {
		if err := shoal.ValidateRequiredID(
			"interaction snapshot ID", s.SnapshotID,
		); err != nil {
			return err
		}
		if s.SnapshotAsOf.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "interaction snapshot time is required")
		}
		if err := shoal.ValidateRequiredID(
			"interaction authorization fingerprint",
			s.AuthorizationFingerprint,
		); err != nil {
			return err
		}
		if s.AuthorizationExpiresAt.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction authorization expiry is required",
			)
		}
		if s.AuthorizationExpiresAt.Before(s.SnapshotAsOf) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction authorization expires before its observed snapshot",
			)
		}
		if s.SnapshotAsOf.After(s.RecordedAt) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction snapshot time is after the recording time",
			)
		}
		if !s.RecordedAt.Before(s.AuthorizationExpiresAt) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction was recorded after authorization expired",
			)
		}
	}
	if len(s.Turns) > MaxTurns {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction session exceeds the public turn bound")
	}
	if err := shoal.ValidateOptionalID(
		"interaction embedding space ID", s.EmbeddingSpaceID,
	); err != nil {
		return err
	}
	if err := validateIdentifier(
		"interaction authorization operation",
		s.AuthorizationOperation,
		true,
	); err != nil {
		return err
	}
	if s.OntologySchemaID != "" || s.OntologyVersionID != "" {
		if _, err := ontology.NewOntologyIdentityFromIDs(
			s.OntologySchemaID, s.OntologyVersionID,
		); err != nil {
			return err
		}
	}
	if err := s.EmbeddingSpaces.Validate(); err != nil {
		return err
	}
	if s.EmbeddingSpaceID != "" &&
		len(s.EmbeddingSpaces.Identities) > 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction cannot mix legacy and canonical embedding space pins",
		)
	}
	if _, err := Conjoin(s.RequiredVisibility); err != nil {
		return err
	}
	for _, id := range s.SeedNodeIDs {
		if err := shoal.ValidateRequiredID("interaction seed node ID", id); err != nil {
			return err
		}
	}
	for _, evidence := range s.SeedEvidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	if len(s.SeedEvidence) > 0 &&
		!equalIDs(dedupeIDs(s.SeedNodeIDs), evidenceNodeIDs(s.SeedEvidence)) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction seed nodes do not match seed evidence")
	}
	for _, id := range s.CitedNodeIDs {
		if err := shoal.ValidateRequiredID("interaction cited node ID", id); err != nil {
			return err
		}
	}
	for _, evidence := range s.CitedEvidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	if len(s.CitedEvidence) > 0 &&
		!equalIDs(dedupeIDs(s.CitedNodeIDs), evidenceNodeIDs(s.CitedEvidence)) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction cited nodes do not match cited evidence")
	}
	turnIndexes := make(map[int]struct{}, len(s.Turns))
	for _, turn := range s.Turns {
		if turn.Index < 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction turn index cannot be negative",
			)
		}
		if _, duplicate := turnIndexes[turn.Index]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction turn indexes must be unique",
			)
		}
		turnIndexes[turn.Index] = struct{}{}
		if turn.InputTokens < 0 || turn.OutputTokens < 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction turn token counts cannot be negative",
			)
		}
		if err := validateIdentifier(
			"interaction turn decision", turn.Decision, true,
		); err != nil {
			return err
		}
		if turn.ToolCall == nil {
			continue
		}
		if err := validateIdentifier(
			"interaction tool kind", turn.ToolCall.Kind, false,
		); err != nil {
			return err
		}
		for _, id := range turn.ToolCall.RetrievedNodeIDs {
			if err := shoal.ValidateRequiredID(
				"interaction retrieved node ID", id,
			); err != nil {
				return err
			}
		}
		for _, evidence := range turn.ToolCall.RetrievedEvidence {
			if err := evidence.Validate(); err != nil {
				return err
			}
		}
		if len(turn.ToolCall.RetrievedEvidence) > 0 &&
			!equalIDs(
				dedupeIDs(turn.ToolCall.RetrievedNodeIDs),
				evidenceNodeIDs(turn.ToolCall.RetrievedEvidence),
			) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction tool nodes do not match retrieved evidence")
		}
	}
	// One anchor identity must describe one evidence reference across seed,
	// cited, and tool-call evidence, or the session-wide union is ambiguous.
	if _, err := canonicalEvidenceReferences(
		s.evidenceReferenceValues(),
	); err != nil {
		return err
	}
	return nil
}

// Canonical returns an independently owned session with UTC timestamps and
// sorted, deduplicated opaque identity sets. Provenance edges are materialized
// from this form so retries and durable hydration retain one stable shape.
func (s Session) Canonical() (Session, error) {
	if err := s.Validate(); err != nil {
		return Session{}, err
	}
	canonical := s
	var err error
	if canonical.Operation == "" {
		canonical.Operation = OperationInference
	}
	canonical.RecordedAt = s.RecordedAt.UTC()
	canonical.SnapshotAsOf = s.SnapshotAsOf.UTC()
	canonical.AuthorizationExpiresAt = s.AuthorizationExpiresAt.UTC()
	canonical.EmbeddingSpaces, err = s.EmbeddingSpaces.Canonical()
	if err != nil {
		return Session{}, err
	}
	canonical.Actor.OnBehalfOf = append(
		[]shoal.ID(nil), s.Actor.OnBehalfOf...)
	canonical.RequiredVisibility, err = Conjoin(s.RequiredVisibility)
	if err != nil {
		return Session{}, err
	}
	canonical.SeedNodeIDs = dedupeIDs(s.SeedNodeIDs)
	canonical.SeedEvidence, err = canonicalEvidenceReferences(s.SeedEvidence)
	if err != nil {
		return Session{}, err
	}
	canonical.CitedNodeIDs = dedupeIDs(s.CitedNodeIDs)
	canonical.CitedEvidence, err = canonicalEvidenceReferences(s.CitedEvidence)
	if err != nil {
		return Session{}, err
	}
	canonical.Turns = make([]Turn, len(s.Turns))
	for index, turn := range s.Turns {
		canonical.Turns[index] = turn
		if turn.ToolCall != nil {
			call := *turn.ToolCall
			call.RetrievedNodeIDs = dedupeIDs(turn.ToolCall.RetrievedNodeIDs)
			call.RetrievedEvidence, err = canonicalEvidenceReferences(
				turn.ToolCall.RetrievedEvidence)
			if err != nil {
				return Session{}, err
			}
			canonical.Turns[index].ToolCall = &call
		}
	}
	sort.Slice(canonical.Turns, func(i, j int) bool {
		return canonical.Turns[i].Index < canonical.Turns[j].Index
	})
	return canonical, nil
}

// TouchedNodeIDs returns the sorted, deduplicated union of every source node
// the session retrieved or cited. It has no policy cap: dropping a late source
// would understate exposure and therefore widen the derived record.
func (s Session) TouchedNodeIDs() []shoal.ID {
	ids := append([]shoal.ID(nil), s.SeedNodeIDs...)
	ids = append(ids, s.CitedNodeIDs...)
	for _, turn := range s.Turns {
		if turn.ToolCall != nil {
			ids = append(ids, turn.ToolCall.RetrievedNodeIDs...)
		}
	}
	return dedupeIDs(ids)
}

// TouchedEdgeIDs returns every exact source graph edge referenced by retrieved
// or cited evidence.
func (s Session) TouchedEdgeIDs() []shoal.ID {
	ids := evidenceEdgeIDs(s.SeedEvidence)
	ids = append(ids, evidenceEdgeIDs(s.CitedEvidence)...)
	for _, turn := range s.Turns {
		if turn.ToolCall != nil {
			ids = append(ids,
				evidenceEdgeIDs(turn.ToolCall.RetrievedEvidence)...)
		}
	}
	return dedupeIDs(ids)
}

// TouchedAssertions returns every authoritative assertion identity and origin
// carried by retrieved or cited evidence.
func (s Session) TouchedAssertions() []AssertionReference {
	references := evidenceAssertions(s.SeedEvidence)
	references = append(references, evidenceAssertions(s.CitedEvidence)...)
	for _, turn := range s.Turns {
		if turn.ToolCall != nil {
			references = append(
				references,
				evidenceAssertions(turn.ToolCall.RetrievedEvidence)...,
			)
		}
	}
	return evidenceAssertions([]EvidenceReference{{Assertions: references}})
}

// EvidenceReferences returns the canonical union of every retrieved and cited
// evidence anchor in the session. It fails rather than silently omitting
// evidence, because callers use the result to bind evidence to a pinned
// frontier and an empty result selects a weaker validation path.
func (s Session) EvidenceReferences() ([]EvidenceReference, error) {
	return canonicalEvidenceReferences(s.evidenceReferenceValues())
}

func (s Session) evidenceReferenceValues() []EvidenceReference {
	values := append([]EvidenceReference(nil), s.SeedEvidence...)
	values = append(values, s.CitedEvidence...)
	for _, turn := range s.Turns {
		if turn.ToolCall != nil {
			values = append(values, turn.ToolCall.RetrievedEvidence...)
		}
	}
	return values
}

// Subgraph materializes the session, turn, and tool-call nodes with their
// retrieved and cited edges. resolve supplies the visibility labels of every
// touched source node; if it fails for any node, the whole record fails rather
// than being written with an understated visibility.
func (s Session) Subgraph(resolve VisibilityResolver) (Subgraph, error) {
	return s.SubgraphWithEvidence(resolve, nil)
}

// SubgraphWithEvidence materializes the interaction while resolving both
// source-node and exact source-edge visibility. An edge resolver is mandatory
// whenever the session contains edge-backed evidence.
func (s Session) SubgraphWithEvidence(
	resolveNode VisibilityResolver,
	resolveEdge VisibilityResolver,
) (Subgraph, error) {
	canonical, err := s.Canonical()
	if err != nil {
		return Subgraph{}, err
	}
	s = canonical
	if resolveNode == nil {
		return Subgraph{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction visibility resolver is required")
	}
	nodeCache := make(map[shoal.ID][]string)
	labelsForNodes := func(ids []shoal.ID) ([]string, error) {
		sets := make([][]string, 0, len(ids))
		for _, id := range ids {
			if cached, ok := nodeCache[id]; ok {
				sets = append(sets, cached)
				continue
			}
			labels, err := resolveNode(id)
			if err != nil {
				return nil, err
			}
			normalized, err := Conjoin(labels)
			if err != nil {
				return nil, err
			}
			nodeCache[id] = normalized
			sets = append(sets, normalized)
		}
		return Conjoin(sets...)
	}
	edgeCache := make(map[shoal.ID][]string)
	labelsForEdges := func(ids []shoal.ID) ([]string, error) {
		if len(ids) > 0 && resolveEdge == nil {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction edge visibility resolver is required",
			)
		}
		sets := make([][]string, 0, len(ids))
		for _, id := range ids {
			if cached, ok := edgeCache[id]; ok {
				sets = append(sets, cached)
				continue
			}
			labels, err := resolveEdge(id)
			if err != nil {
				return nil, err
			}
			normalized, err := Conjoin(labels)
			if err != nil {
				return nil, err
			}
			edgeCache[id] = normalized
			sets = append(sets, normalized)
		}
		return Conjoin(sets...)
	}

	sessionNode := graph.Node{
		ID:     s.ID,
		Kind:   KindSession,
		Labels: []string{LabelInteraction},
		Properties: shoal.Metadata{
			PropertyRecordedAt: s.RecordedAt.UTC().Format(time.RFC3339Nano),
			PropertyTurnCount:  strconv.Itoa(len(s.Turns)),
			PropertyOperation:  string(s.Operation),
		},
	}
	setIfPresent(sessionNode.Properties, PropertySnapshotID, string(s.SnapshotID))
	if !s.SnapshotAsOf.IsZero() {
		setIfPresent(
			sessionNode.Properties,
			PropertySnapshotAsOf,
			s.SnapshotAsOf.UTC().Format(time.RFC3339Nano),
		)
	}
	setIfPresent(
		sessionNode.Properties, PropertyAuthFingerprint,
		string(s.AuthorizationFingerprint),
	)
	setIfPresent(
		sessionNode.Properties,
		PropertyAuthOperation,
		s.AuthorizationOperation,
	)
	setIfPresent(
		sessionNode.Properties, PropertyOntologySchema,
		string(s.OntologySchemaID),
	)
	setIfPresent(
		sessionNode.Properties, PropertyOntologyVersion,
		string(s.OntologyVersionID),
	)
	if !s.AuthorizationExpiresAt.IsZero() {
		setIfPresent(
			sessionNode.Properties,
			PropertyAuthExpiresAt,
			s.AuthorizationExpiresAt.UTC().Format(time.RFC3339Nano),
		)
	}
	setIfPresent(
		sessionNode.Properties, PropertyEmbeddingSpace,
		string(s.EmbeddingSpaceID),
	)
	setIfPresent(
		sessionNode.Properties, PropertyEmbeddingSpaces,
		s.EmbeddingSpaces.Digest,
	)
	if len(s.EmbeddingSpaces.Identities) > 0 {
		sessionNode.Properties[PropertyEmbeddingCount] =
			strconv.Itoa(len(s.EmbeddingSpaces.Identities))
	}
	setIfPresent(
		sessionNode.Properties, PropertySubjectID, string(s.Actor.SubjectID))
	setIfPresent(
		sessionNode.Properties, PropertyActorID, string(s.Actor.ActorID))
	setIfPresent(
		sessionNode.Properties, PropertyClientID, string(s.Actor.ClientID))
	if len(s.Actor.OnBehalfOf) > 0 {
		sessionNode.Properties[PropertyDelegationCount] =
			strconv.Itoa(len(s.Actor.OnBehalfOf))
		sessionNode.Properties[PropertyDelegationID] =
			string(delegationID(s.Actor.OnBehalfOf))
	}
	setIfPresent(sessionNode.Properties, PropertyReasonCode, s.Reason.Code)
	setIfPresent(sessionNode.Properties, PropertyReasonDigest, s.Reason.Digest)

	nodes := []graph.Node{sessionNode}
	var edges []graph.Edge
	rootID := s.ID
	var inferenceNode graph.Node
	if s.Operation.HasInference() {
		inferenceID := InferenceID(s.ID)
		inferenceNode = graph.Node{
			ID:     inferenceID,
			Kind:   KindInference,
			Labels: []string{LabelInteraction},
			Properties: shoal.Metadata{
				PropertySessionID:   string(s.ID),
				PropertyInferenceID: string(inferenceID),
				PropertyTurnCount:   strconv.Itoa(len(s.Turns)),
				PropertyCited:       strconv.Itoa(len(s.CitedNodeIDs)),
			},
		}
		setIfPresent(
			inferenceNode.Properties, PropertySnapshotID, string(s.SnapshotID))
		if !s.SnapshotAsOf.IsZero() {
			setIfPresent(
				inferenceNode.Properties,
				PropertySnapshotAsOf,
				s.SnapshotAsOf.UTC().Format(time.RFC3339Nano),
			)
		}
		setIfPresent(
			inferenceNode.Properties,
			PropertyAuthFingerprint,
			string(s.AuthorizationFingerprint),
		)
		setIfPresent(
			inferenceNode.Properties,
			PropertyAuthOperation,
			s.AuthorizationOperation,
		)
		if !s.AuthorizationExpiresAt.IsZero() {
			setIfPresent(
				inferenceNode.Properties,
				PropertyAuthExpiresAt,
				s.AuthorizationExpiresAt.UTC().Format(time.RFC3339Nano),
			)
		}
		setIfPresent(inferenceNode.Properties, PropertyStopReason, s.StopReason)
		setIfPresent(inferenceNode.Properties, PropertyQueryDigest, s.QueryDigest)
		setIfPresent(inferenceNode.Properties, PropertyRequestID, string(s.RequestID))
		setIfPresent(
			inferenceNode.Properties,
			PropertyContextPackID,
			string(s.ContextPackID),
		)
		setIfPresent(inferenceNode.Properties, PropertyResultID, string(s.ResultID))
		setIfPresent(inferenceNode.Properties, PropertyHarness, s.Provenance.Harness)
		setIfPresent(inferenceNode.Properties, PropertyProvider, s.Provenance.Provider)
		setIfPresent(inferenceNode.Properties, PropertyModel, s.Provenance.Model)
		setIfPresent(
			inferenceNode.Properties,
			PropertyModelVersion,
			s.Provenance.ModelVersion,
		)
		setIfPresent(inferenceNode.Properties, PropertyPromptID, s.Provenance.PromptID)
		setIfPresent(
			inferenceNode.Properties,
			PropertyPromptVersion,
			s.Provenance.PromptVer,
		)
		setIfPresent(
			inferenceNode.Properties,
			PropertyPromptHash,
			s.Provenance.PromptHash,
		)
		setIfPresent(
			inferenceNode.Properties,
			PropertyToolPolicy,
			s.Provenance.ToolPolicy,
		)
		setIfPresent(
			inferenceNode.Properties,
			PropertyEmbeddingSpace,
			string(s.EmbeddingSpaceID),
		)
		setIfPresent(
			inferenceNode.Properties,
			PropertyEmbeddingSpaces,
			s.EmbeddingSpaces.Digest,
		)
		if len(s.EmbeddingSpaces.Identities) > 0 {
			inferenceNode.Properties[PropertyEmbeddingCount] =
				strconv.Itoa(len(s.EmbeddingSpaces.Identities))
		}
		nodes = append(nodes, inferenceNode)
		edges = append(
			edges, provenanceEdge(EdgeHasInference, s.ID, inferenceID))
		rootID = inferenceID
	}
	touched := make(map[shoal.ID]struct{})
	addTouched := func(ids []shoal.ID) {
		for _, id := range ids {
			touched[id] = struct{}{}
		}
	}

	seed := dedupeIDs(s.SeedNodeIDs)
	addTouched(seed)
	for _, id := range seed {
		edges = append(edges, provenanceEdge(EdgeRetrieved, rootID, id))
	}
	cited := dedupeIDs(s.CitedNodeIDs)
	addTouched(cited)
	for _, id := range cited {
		edges = append(edges, provenanceEdge(EdgeCited, rootID, id))
	}

	for _, turn := range s.Turns {
		turnID := DerivedID("turn", string(rootID), strconv.Itoa(turn.Index))
		turnNode := graph.Node{
			ID:     turnID,
			Kind:   KindTurn,
			Labels: []string{LabelInteraction},
			Properties: shoal.Metadata{
				PropertySessionID:    string(s.ID),
				PropertyIndex:        strconv.Itoa(turn.Index),
				PropertyInputTokens:  strconv.Itoa(turn.InputTokens),
				PropertyOutputTokens: strconv.Itoa(turn.OutputTokens),
				PropertyFailed:       strconv.FormatBool(turn.Failed),
			},
		}
		if s.Operation.HasInference() {
			setIfPresent(
				turnNode.Properties,
				PropertyInferenceID,
				string(InferenceID(s.ID)),
			)
		}
		setIfPresent(turnNode.Properties, PropertyDecision, turn.Decision)
		edges = append(edges, provenanceEdge(EdgeHasTurn, rootID, turnID))

		turnVisibility := []string(nil)
		if turn.ToolCall != nil {
			retrieved := dedupeIDs(turn.ToolCall.RetrievedNodeIDs)
			addTouched(retrieved)
			retrievedEdges := evidenceEdgeIDs(
				turn.ToolCall.RetrievedEvidence)
			callID := DerivedID("tool_call", string(turnID))
			callNode := graph.Node{
				ID:     callID,
				Kind:   KindToolCall,
				Labels: []string{LabelInteraction},
				Properties: shoal.Metadata{
					PropertySessionID: string(s.ID),
					PropertyTurnID:    string(turnID),
					PropertyIndex:     strconv.Itoa(turn.Index),
					PropertyRetrieved: strconv.Itoa(len(retrieved)),
				},
			}
			setIfPresent(callNode.Properties, PropertyToolKind, turn.ToolCall.Kind)
			nodeVisibility, err := labelsForNodes(retrieved)
			if err != nil {
				return Subgraph{}, err
			}
			edgeVisibility, err := labelsForEdges(retrievedEdges)
			if err != nil {
				return Subgraph{}, err
			}
			callVisibility, err := Conjoin(
				nodeVisibility, edgeVisibility)
			if err != nil {
				return Subgraph{}, err
			}
			setVisibility(callNode.Properties, callVisibility)
			turnVisibility = callVisibility
			nodes = append(nodes, callNode)
			edges = append(edges, provenanceEdge(EdgeHasToolCall, turnID, callID))
			for _, id := range retrieved {
				edges = append(edges, provenanceEdge(EdgeRetrieved, callID, id))
			}
		}
		setVisibility(turnNode.Properties, turnVisibility)
		nodes = append(nodes, turnNode)
	}

	touchedIDs := make([]shoal.ID, 0, len(touched))
	for id := range touched {
		touchedIDs = append(touchedIDs, id)
	}
	sort.Slice(touchedIDs, func(i, j int) bool {
		return shoal.CompareID(touchedIDs[i], touchedIDs[j]) < 0
	})
	nodeVisibility, err := labelsForNodes(touchedIDs)
	if err != nil {
		return Subgraph{}, err
	}
	touchedEdgeIDs := s.TouchedEdgeIDs()
	edgeVisibility, err := labelsForEdges(touchedEdgeIDs)
	if err != nil {
		return Subgraph{}, err
	}
	visibility, err := Conjoin(
		nodeVisibility, edgeVisibility, s.RequiredVisibility)
	if err != nil {
		return Subgraph{}, err
	}
	setVisibility(sessionNode.Properties, visibility)
	if s.Operation.HasInference() {
		setVisibility(inferenceNode.Properties, visibility)
	}

	sortNodes(nodes)
	sortEdges(edges)
	for _, node := range nodes {
		if err := node.Validate(); err != nil {
			return Subgraph{}, err
		}
	}
	for _, edge := range edges {
		if err := edge.Validate(); err != nil {
			return Subgraph{}, err
		}
	}
	return Subgraph{
		Nodes:          nodes,
		Edges:          edges,
		Visibility:     visibility,
		TouchedNodeIDs: touchedIDs,
		TouchedEdgeIDs: touchedEdgeIDs,
	}, nil
}

// Tombstone is the durable, auditable residue of an explicitly deleted
// interaction record. Deletion is never a TTL and never silent.
type Tombstone struct {
	SessionID  shoal.ID
	DeletedAt  time.Time
	NodeCount  int
	EdgeCount  int
	Visibility []string
}

// Node materializes a tombstone. It keeps the visibility the deleted record
// required, so a deletion can never publish the fact that a restricted session
// existed to a reader who could not have seen the session itself.
func (t Tombstone) Node() (graph.Node, error) {
	if err := shoal.ValidateRequiredID("interaction session ID", t.SessionID); err != nil {
		return graph.Node{}, err
	}
	if t.DeletedAt.IsZero() {
		return graph.Node{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction deletion time is required")
	}
	visibility, err := Conjoin(t.Visibility)
	if err != nil {
		return graph.Node{}, err
	}
	node := graph.Node{
		ID:     TombstoneID(t.SessionID),
		Kind:   KindTombstone,
		Labels: []string{LabelInteraction},
		Properties: shoal.Metadata{
			PropertySessionID: string(t.SessionID),
			PropertyDeletedAt: t.DeletedAt.UTC().Format(time.RFC3339Nano),
			PropertyNodeCount: strconv.Itoa(t.NodeCount),
			PropertyEdgeCount: strconv.Itoa(t.EdgeCount),
		},
	}
	setVisibility(node.Properties, visibility)
	if err := node.Validate(); err != nil {
		return graph.Node{}, err
	}
	return node, nil
}

// TombstoneID is the stable identity of the tombstone for one session.
func TombstoneID(sessionID shoal.ID) shoal.ID {
	return DerivedID("tombstone", string(sessionID))
}

// SessionID derives a stable session identity from a transcript identity and
// the instant the record was captured. The instant is part of the identity so
// that serving the same deterministic inference twice produces two durable
// records rather than silently collapsing into one.
func SessionID(transcriptID shoal.ID, recordedAt time.Time) shoal.ID {
	return DerivedID(
		"session",
		string(transcriptID),
		recordedAt.UTC().Format(time.RFC3339Nano),
	)
}

// IsSessionID reports whether an opaque ID is in the session-owned part of
// the reserved interaction namespace.
func IsSessionID(id shoal.ID) bool {
	return strings.HasPrefix(string(id), KindSession+"_")
}

// OperationSessionID derives a stable opaque session identity for retrieval,
// tool-call, chat, and other product interaction adapters. The caller-provided
// correlation identity is hashed into the result and is never exposed.
func OperationSessionID(
	operation Operation, correlationID shoal.ID, recordedAt time.Time,
) (shoal.ID, error) {
	if err := operation.Validate(); err != nil {
		return "", err
	}
	if err := shoal.ValidateRequiredID(
		"interaction correlation ID", correlationID,
	); err != nil {
		return "", err
	}
	if recordedAt.IsZero() {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction session time is required")
	}
	return DerivedID(
		"session",
		string(operation),
		string(correlationID),
		recordedAt.UTC().Format(time.RFC3339Nano),
	), nil
}

// InferenceID is the stable address of the single inference represented by a
// session. It is intentionally derived from the session rather than the result
// so failed and empty-result inferences remain addressable.
func InferenceID(sessionID shoal.ID) shoal.ID {
	return DerivedID("inference", string(sessionID))
}

// DerivedID mints a collision-resistant identity inside the interaction
// namespace from already-public identity components.
func DerivedID(kind string, parts ...string) shoal.ID {
	hash := sha256.New()
	var length [8]byte
	write := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write(KindPrefix)
	write(kind)
	for _, part := range parts {
		write(part)
	}
	return shoal.ID(KindPrefix + kind + "_" + hex.EncodeToString(hash.Sum(nil)[:16]))
}

// Digest hashes free text into a non-reversible identity so a record can be
// correlated without persisting the text itself.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateDigest(name, digest string, optional bool) error {
	if digest == "" {
		if optional {
			return nil
		}
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" is required")
	}
	if len(digest) != sha256.Size*2 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			name+" must be a SHA-256 digest in lowercase hex",
		)
	}
	for index := 0; index < len(digest); index++ {
		value := digest[index]
		if (value >= '0' && value <= '9') ||
			(value >= 'a' && value <= 'f') {
			continue
		}
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			name+" must be a SHA-256 digest in lowercase hex",
		)
	}
	return nil
}

func provenanceEdge(edgeType string, from, to shoal.ID) graph.Edge {
	return graph.Edge{
		ID:     DerivedID("edge", edgeType, string(from), string(to)),
		From:   from,
		To:     to,
		Type:   edgeType,
		Weight: 1,
	}
}

func setIfPresent(metadata shoal.Metadata, key, value string) {
	if value == "" {
		return
	}
	metadata[key] = value
}

func setVisibility(metadata shoal.Metadata, labels []string) {
	expression := Expression(labels)
	if expression == "" {
		return
	}
	if len(expression) <= shoal.MaxMetadataValueBytes {
		metadata[PropertyVisibility] = expression
		return
	}
	metadata[PropertyVisibilityDigest] = Digest(expression)
	metadata[PropertyVisibilityCount] = strconv.Itoa(len(labels))
}

func delegationID(chain []shoal.ID) shoal.ID {
	parts := make([]string, len(chain))
	for index, id := range chain {
		parts[index] = string(id)
	}
	return DerivedID("delegation", parts...)
}

func dedupeIDs(ids []shoal.ID) []shoal.ID {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[shoal.ID]struct{}, len(ids))
	result := make([]shoal.ID, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return result
}

func sortNodes(nodes []graph.Node) {
	sort.Slice(nodes, func(i, j int) bool {
		return shoal.CompareID(nodes[i].ID, nodes[j].ID) < 0
	})
}

func sortEdges(edges []graph.Edge) {
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
}
