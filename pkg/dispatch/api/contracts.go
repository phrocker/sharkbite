// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package api is the public wire contract of the worker-facing fleet dispatch
// routes (/api/v1/fleet/actions/*) and a Go client for them.
//
// An out-of-process worker — the effects gateway, or an extension that
// performs effects on another system — pulls work it may claim (Pull), takes
// one action under a fence (Claim), renews the claim while the effect runs
// (Extend), and reports the outcome under the claim (Complete). A worker that
// lost its fence mid-effect records what it attempted (ReportAmbiguity).
// Resolve reads the worker's own descriptor, and Status re-reads one record.
//
// The wire types here are the exact shapes the explorer serves: field order,
// JSON tags and Go types match what pkg/explorer/webapi encodes and decodes,
// and golden fixtures under testdata/wire pin the bytes. Encodings:
//
//   - IDs, claim IDs, cursors and the executor key are unpadded base64url
//     strings (EncodeID).
//   - Lease is an integer count of nanoseconds (time.Duration).
//   - Input and Output are raw JSON.
//   - Times are RFC 3339 with nanoseconds. The explorer refuses a request
//     deadline that is not in UTC, so the client converts it; response times
//     are in whatever zone the explorer recorded them.
//
// This package imports only the standard library and pkg/shoal (the import
// boundary also admits pkg/executorref), so an extension may depend on it
// (internal/importboundary enforces that).
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Routes. Every route is POST and sits under the authenticated fleet mount.
const (
	RoutePrefix  = "/api/v1/fleet/"
	ActionsRoute = RoutePrefix + "actions"
	PullRoute    = ActionsRoute + "/pull"
	AgentsRoute  = RoutePrefix + "agents"
)

// The per-action verbs: ActionRoute(id, VerbClaim) is
// /api/v1/fleet/actions/{id}/claim.
const (
	VerbClaim     = "claim"
	VerbExtend    = "extend"
	VerbComplete  = "complete"
	VerbAmbiguity = "ambiguity"
	VerbStatus    = "status"
)

// ActionRoute is the route of one verb on one action.
func ActionRoute(actionID []byte, verb string) string {
	return ActionsRoute + "/" + EncodeID(actionID) + "/" + verb
}

// ResolveRoute is the route that resolves one descriptor.
func ResolveRoute(agentID []byte) string {
	return AgentsRoute + "/" + EncodeID(agentID) + "/resolve"
}

// Headers.
const (
	// CommitOutcomeHeader is set to CommitOutcomeIndeterminate on a failed
	// response whose durable outcome is unknown: the mutation may have
	// committed.
	CommitOutcomeHeader        = "Shoal-Commit-Outcome"
	CommitOutcomeIndeterminate = "indeterminate"
	// CorrelationIDHeader carries the trace a request belongs to (#527). On
	// claim, extend, complete, ambiguity and status it is the action record's
	// correlation_id.
	CorrelationIDHeader = "Shoal-Correlation-ID"
)

// Limits the explorer applies. They are mirrored here rather than imported,
// and a parity test in pkg/explorer/webapi fails if they drift from the fleet
// package that enforces them.
const (
	// MaxActionIDBytes bounds action IDs, claim IDs, request IDs and cursors
	// after base64url decoding.
	MaxActionIDBytes = 256
	// MaxReasonCodeBytes bounds a request context's reason code.
	MaxReasonCodeBytes = 64
	// MaxNameBytes bounds capability and action names.
	MaxNameBytes = 128
	// MaxActionPayloadBytes bounds an action's input.
	MaxActionPayloadBytes = 1 << 20
	// MaxActionOutputBytes bounds a completion's output.
	MaxActionOutputBytes = 1 << 20
	// MaxActionClaimTTL bounds a claim's or an extension's lease. The
	// explorer refuses a longer one rather than shortening it.
	MaxActionClaimTTL = 5 * time.Minute
	// MaxDispatchListResults bounds a pull page.
	MaxDispatchListResults = 256
	// MaxAmbiguityTargetBytes and MaxAmbiguityReferenceBytes bound an
	// ambiguity report's optional text.
	MaxAmbiguityTargetBytes    = 256
	MaxAmbiguityReferenceBytes = 256
	// MaxEffectedBytes and MaxEffectedChunks bound a reported volume.
	MaxEffectedBytes  int64 = 1 << 40
	MaxEffectedChunks int64 = 1 << 24
	// MaxCorrelationIDBytes bounds a Shoal-Correlation-ID, which the explorer
	// holds to shoal.MaxIDBytes rather than to the action-ID bound.
	MaxCorrelationIDBytes = shoal.MaxIDBytes
	// ExecutorKeyBytes is the width of an action's executor key.
	ExecutorKeyBytes = 32
)

// DispatchState is an action record's state.
type DispatchState string

const (
	DispatchQueued    DispatchState = "queued"
	DispatchClaimed   DispatchState = "claimed"
	DispatchSucceeded DispatchState = "succeeded"
	DispatchFailed    DispatchState = "failed"
	DispatchCanceled  DispatchState = "canceled"
)

// AmbiguityOutcome is what a worker that lost its fence says it attempted.
type AmbiguityOutcome string

const (
	// AmbiguityRequestNotSent: nothing was transmitted; the effect did not
	// happen.
	AmbiguityRequestNotSent AmbiguityOutcome = "request_not_sent"
	// AmbiguityOutcomeUnknown: the request left and the worker never learned
	// what happened to it.
	AmbiguityOutcomeUnknown AmbiguityOutcome = "outcome_unknown"
	// AmbiguityEffectObserved: the worker saw the effect succeed but could no
	// longer report it through Complete.
	AmbiguityEffectObserved AmbiguityOutcome = "effect_observed"
)

// ValidAmbiguityOutcome reports whether outcome is in the closed vocabulary.
func ValidAmbiguityOutcome(outcome AmbiguityOutcome) bool {
	switch outcome {
	case AmbiguityRequestNotSent, AmbiguityOutcomeUnknown, AmbiguityEffectObserved:
		return true
	}
	return false
}

// Effect is one class of consequence a descriptor's action declares.
type Effect string

const (
	EffectReadsCorpus     Effect = "reads-corpus"
	EffectEgressesContent Effect = "egresses-content"
	EffectMutatesExternal Effect = "external"
)

// Effects is a declared set of effect classes.
type Effects []Effect

// legacyEffect is the superseded scalar spelling the explorer emits beside
// the set: "external" when the set mutates or transmits, otherwise empty.
func (e Effects) legacyEffect() Effect {
	for _, effect := range e {
		if effect == EffectMutatesExternal || effect == EffectEgressesContent {
			return EffectMutatesExternal
		}
	}
	return ""
}

// ErrorCodeOrigin says who decided a record's error code.
const (
	ErrorCodeOriginService  = "service"
	ErrorCodeOriginExecutor = "executor"
)

// RequestContext is the context every request carries. Deadline must be in
// UTC and in the future.
type RequestContext struct {
	RequestID     string    `json:"request_id"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	ReasonCode    string    `json:"reason_code"`
	ReasonDetail  string    `json:"reason_detail,omitempty"`
	Deadline      time.Time `json:"deadline"`
}

// PullRequest is the body of PullRoute.
type PullRequest struct {
	Context RequestContext `json:"context"`
	After   string         `json:"after,omitempty"`
	Limit   int            `json:"limit"`
}

// ClaimRequest is the body of VerbClaim.
type ClaimRequest struct {
	Context         RequestContext `json:"context"`
	ExpectedVersion uint64         `json:"expected_version"`
	ClaimID         string         `json:"claim_id"`
	Lease           time.Duration  `json:"lease"`
}

// ExtendRequest is the body of VerbExtend. With ClaimFence the renewal binds
// on the fence and ExpectedVersion is not compared (#629), so a client
// leaves it zero and it is omitted.
type ExtendRequest struct {
	Context         RequestContext `json:"context"`
	ExpectedVersion uint64         `json:"expected_version,omitempty"`
	ClaimFence      uint64         `json:"claim_fence,omitempty"`
	ClaimID         string         `json:"claim_id"`
	Lease           time.Duration  `json:"lease"`
}

// CompleteRequest is the body of VerbComplete. With ClaimFence the
// completion binds on the claim generation, not the version (#484). Evidence
// fields are for trusted in-plane executors; a worker that has not read the
// corpus sends none.
type CompleteRequest struct {
	Context              RequestContext  `json:"context"`
	ExpectedVersion      uint64          `json:"expected_version"`
	ClaimID              string          `json:"claim_id"`
	ClaimFence           uint64          `json:"claim_fence,omitempty"`
	Output               json.RawMessage `json:"output,omitempty"`
	ErrorCode            string          `json:"error_code,omitempty"`
	Failed               bool            `json:"failed,omitempty"`
	Effected             *Effected       `json:"effected,omitempty"`
	EvidenceSnapshotID   string          `json:"evidence_snapshot_id,omitempty"`
	EvidenceSnapshotAsOf time.Time       `json:"evidence_snapshot_as_of,omitzero"`
	Evidence             []Evidence      `json:"evidence,omitempty"`
}

// AmbiguityRequest is the body of VerbAmbiguity. Outcome is a plain string
// on the request because that is the type the explorer decodes it into.
type AmbiguityRequest struct {
	Context         RequestContext `json:"context"`
	ExpectedVersion uint64         `json:"expected_version"`
	ClaimFence      uint64         `json:"claim_fence"`
	Outcome         string         `json:"outcome"`
	Target          string         `json:"target,omitempty"`
	Reference       string         `json:"reference,omitempty"`
}

// Effected is how much of an irreversible egress happened before a failure
// (#427): an upper bound on bytes handed to the transport, never a receipt.
type Effected struct {
	Bytes  int64 `json:"bytes"`
	Chunks int64 `json:"chunks,omitempty"`
}

// Zero reports whether nothing left.
func (e Effected) Zero() bool { return e.Bytes == 0 && e.Chunks == 0 }

// Evidence is one evidence anchor on a completion or a record.
type Evidence struct {
	AnchorID   string      `json:"anchor_id"`
	Kind       string      `json:"kind"`
	Citation   *Citation   `json:"citation,omitempty"`
	NodeIDs    []string    `json:"node_ids"`
	EdgeIDs    []string    `json:"edge_ids,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Visibility []string    `json:"visibility"`
}

// Citation locates document evidence.
type Citation struct {
	DocumentID string `json:"document_id"`
	RevisionID string `json:"revision_id"`
	SectionID  string `json:"section_id,omitempty"`
	SpanID     string `json:"span_id,omitempty"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	StartPage  int32  `json:"start_page,omitempty"`
	EndPage    int32  `json:"end_page,omitempty"`
}

// Assertion names one graph assertion an anchor rests on.
type Assertion struct {
	AssertionID string `json:"assertion_id"`
	EdgeID      string `json:"edge_id"`
	Origin      string `json:"origin"`
}

// AmbiguityReport is one lost-fence report as a record carries it.
type AmbiguityReport struct {
	ClaimFence uint64           `json:"claim_fence"`
	Outcome    AmbiguityOutcome `json:"outcome"`
	Target     string           `json:"target,omitempty"`
	Reference  string           `json:"reference,omitempty"`
	Subject    string           `json:"subject"`
	Actor      string           `json:"actor"`
	ReportedAt time.Time        `json:"reported_at"`
}

// Action is an action record as every dispatch route returns it. Input and
// ExecutorKey are populated on the claim response only.
type Action struct {
	ID               string            `json:"id"`
	Version          uint64            `json:"version"`
	State            DispatchState     `json:"state"`
	AgentID          string            `json:"agent_id"`
	AgentGeneration  int64             `json:"agent_generation"`
	Capability       string            `json:"capability"`
	Action           string            `json:"action"`
	Input            json.RawMessage   `json:"input,omitempty"`
	Output           json.RawMessage   `json:"output,omitempty"`
	ErrorCode        string            `json:"error_code,omitempty"`
	ErrorCodeOrigin  string            `json:"error_code_origin,omitempty"`
	Effected         *Effected         `json:"effected,omitempty"`
	RequestID        string            `json:"request_id"`
	CorrelationID    string            `json:"correlation_id,omitempty"`
	Deadline         time.Time         `json:"deadline"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	ExecutorKey      string            `json:"executor_key,omitempty"`
	ClaimID          string            `json:"claim_id,omitempty"`
	ClaimFence       uint64            `json:"claim_fence,omitempty"`
	ClaimLeaseUntil  time.Time         `json:"claim_lease_until,omitempty"`
	EffectPossible   bool              `json:"effect_possible"`
	AmbiguityReports []AmbiguityReport `json:"ambiguity_reports,omitempty"`
	// The evidence fields are what an in-plane executor reported; the
	// explorer returns them on every record.
	EvidenceSnapshotID   string     `json:"evidence_snapshot_id,omitempty"`
	EvidenceSnapshotAsOf time.Time  `json:"evidence_snapshot_as_of,omitempty"`
	Evidence             []Evidence `json:"evidence,omitempty"`
}

// Page is the response of PullRoute.
type Page struct {
	Actions []Action `json:"actions"`
	Next    string   `json:"next,omitempty"`
}

// Descriptor is the part of a resolved descriptor a worker reads. The
// explorer sends more; this client ignores what it does not name.
type Descriptor struct {
	ID           string       `json:"id"`
	Generation   int64        `json:"generation"`
	ExecutorRef  string       `json:"executor_ref"`
	Capabilities []Capability `json:"capabilities"`
}

// Capability is one declared capability of a descriptor.
type Capability struct {
	Name    string             `json:"name"`
	Actions []DescriptorAction `json:"actions"`
}

// DescriptorAction is one declared action. LegacyEffect is the superseded
// scalar spelling the explorer still emits beside Effects; decoding folds it
// into Effects when only it is present and refuses the two when they
// disagree, as the explorer does.
//
// It is the one response object that refuses fields it does not know. New
// gates on an action arrive as new fields (requires_approval and
// requires_attestation did), and a worker that cannot see a gate must refuse
// the descriptor rather than serve the action without it: the explorer's own
// decoder for this object refuses unknown fields too.
type DescriptorAction struct {
	Name                string          `json:"name"`
	InputSchema         json.RawMessage `json:"input_schema"`
	OutputSchema        json.RawMessage `json:"output_schema"`
	Effects             Effects         `json:"effects,omitempty"`
	LegacyEffect        *Effect         `json:"effect,omitempty"`
	RequiresApproval    bool            `json:"requires_approval,omitempty"`
	RequiresAttestation bool            `json:"requires_attestation,omitempty"`
}

// UnmarshalJSON decodes an action strictly: unknown fields are refused, and
// the two effect spellings are reconciled.
func (a *DescriptorAction) UnmarshalJSON(data []byte) error {
	type fields DescriptorAction
	var decoded fields
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	action := DescriptorAction(decoded)
	if err := action.normalizeEffects(); err != nil {
		return err
	}
	*a = action
	return nil
}

// normalizeEffects applies the explorer's reading of the two spellings.
func (a *DescriptorAction) normalizeEffects() error {
	if a.LegacyEffect == nil {
		return nil
	}
	if a.Effects != nil {
		if *a.LegacyEffect != a.Effects.legacyEffect() {
			return errors.New("action declares effect and effects inconsistently")
		}
		return nil
	}
	if *a.LegacyEffect != "" {
		a.Effects = Effects{*a.LegacyEffect}
	}
	return nil
}

// ErrorResponse is the body of a failed response.
type ErrorResponse struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Indeterminate bool   `json:"indeterminate,omitempty"`
}

// EncodeID spells opaque bytes as the wire does: unpadded base64url.
func EncodeID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// DecodeID reads an unpadded base64url ID.
func DecodeID(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}

// CheckCorrelationID applies exactly the explorer's rules for a
// Shoal-Correlation-ID: present, at most MaxCorrelationIDBytes, valid UTF-8,
// and only printable characters other than the space. It must accept every
// correlation the explorer accepts — a record's correlation came through the
// same rules, and a stricter check would make that action unclaimable. A
// parity test in pkg/explorer/webapi holds it to the explorer's validators.
func CheckCorrelationID(id []byte) error {
	if len(id) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "correlation ID is required")
	}
	if len(id) > MaxCorrelationIDBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			fmt.Sprintf("correlation ID cannot exceed %d bytes", MaxCorrelationIDBytes))
	}
	if !utf8.Valid(id) {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"interaction correlation ID must be valid UTF-8")
	}
	for _, character := range string(id) {
		if !unicode.IsPrint(character) || character == ' ' {
			return shoal.NewError(shoal.ErrorInvalidArgument,
				"interaction correlation ID must be printable and contain no spaces")
		}
	}
	return nil
}

// PrintableText is the explorer's rule for an ambiguity report's target and
// reference: valid UTF-8 and unicode.IsPrint for every codepoint, which
// admits the ASCII space and refuses every control, format (bidi overrides,
// zero-width) and unassigned codepoint.
func PrintableText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, codepoint := range value {
		if !unicode.IsPrint(codepoint) {
			return false
		}
	}
	return true
}

// ErrExecutorKeyMissing says a claim response carried no executor key.
var ErrExecutorKeyMissing = errors.New("executor key is missing")

// ParseExecutorKey decodes an executor key from either spelling the platform
// emits for it — the HTTP wire's unpadded base64url, or the padded standard
// base64 the MCP tools emit — and refuses everything else, including any
// whitespace (encoding/base64 silently skips '\r' and '\n'). Compare keys as
// bytes (EqualExecutorKeys), never as spellings.
func ParseExecutorKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, ErrExecutorKeyMissing
	}
	if strings.ContainsAny(encoded, " \t\r\n") {
		return nil, errors.New("executor key must not contain whitespace")
	}
	var (
		decoded []byte
		err     error
	)
	switch {
	case strings.HasSuffix(encoded, "="):
		// Padded: only the standard alphabet is ever emitted padded.
		decoded, err = base64.StdEncoding.Strict().DecodeString(encoded)
	default:
		decoded, err = base64.RawURLEncoding.Strict().DecodeString(encoded)
	}
	if err != nil {
		return nil, errors.New(
			"executor key must be unpadded base64url or padded standard base64")
	}
	if len(decoded) != ExecutorKeyBytes {
		return nil, fmt.Errorf(
			"executor key must decode to exactly %d bytes, not %d",
			ExecutorKeyBytes, len(decoded))
	}
	return decoded, nil
}

// EqualExecutorKeys compares two decoded keys in constant time. An absent key
// equals nothing, including another absent key.
func EqualExecutorKeys(a, b []byte) bool {
	if len(a) != ExecutorKeyBytes || len(b) != ExecutorKeyBytes {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
