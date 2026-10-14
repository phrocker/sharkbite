// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package api is the public wire contract of Shoal's pre-call admission seam
// (/api/v1/admission/*, docs/admission-seam.md) and a Go client for it.
//
// An out-of-process caller about to perform a side effect — the LLM gateway
// about to post a prompt to a hosted model, an extension about to act on
// another system — asks first (Request), is told denied, allowed, or allowed
// with obligations, performs the effect if it may, and then reports what
// happened (Report). Outstanding lists admissions granted and never reported.
//
// The types here are the exact wire shapes the explorer serves: field order,
// JSON tags and Go types are identical to what pkg/explorer/webapi encodes and
// decodes, and golden fixtures under testdata/wire pin the bytes. Encodings:
//
//   - IDs, keys, tokens, disclosures, withhold entries and cursors are
//     unpadded base64url strings (EncodeID).
//   - SourceID and PolicyID are []byte, so they are standard padded base64,
//     and null when nil.
//   - Lease is an integer count of nanoseconds (time.Duration).
//   - Input and Outcome are raw JSON.
//   - Times are RFC 3339 with nanoseconds in the sender's own zone; nothing is
//     normalised to UTC.
//
// This package imports only the standard library and pkg/shoal, so an
// extension may depend on it (internal/importboundary enforces that).
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Routes. RoutePrefix is the authenticated mount; every route is POST.
const (
	RoutePrefix      = "/api/v1/admission/"
	RequestRoute     = RoutePrefix + "request"
	ReportRoute      = RoutePrefix + "report"
	OutstandingRoute = RoutePrefix + "outstanding"
)

// CommitOutcomeHeader is set to CommitOutcomeIndeterminate on a failed
// response whose durable outcome is unknown: the request or report may have
// committed. A caller must inspect state or retry with the same identity
// rather than assume nothing happened.
const (
	CommitOutcomeHeader        = "Shoal-Commit-Outcome"
	CommitOutcomeIndeterminate = "indeterminate"
)

// Limits the plane applies. They are mirrored here rather than imported, and
// a parity test in pkg/explorer/webapi fails if they drift from the fleet
// package that enforces them.
const (
	// MaxIDBytes bounds every opaque identity (admission ID, idempotency key,
	// token and action IDs, cursors) after base64url decoding.
	MaxIDBytes = 256
	// MaxDisclosures bounds the corpus references one admission may declare.
	MaxDisclosures = 256
	// MaxNameBytes bounds capability and action names.
	MaxNameBytes = 128
	// MaxLease bounds Request.Lease.
	MaxLease = 5 * time.Minute
)

// Outcome is the plane's answer to a Request.
type Outcome string

const (
	// OutcomeDenied means the call must not happen. A denial carries no token
	// and no reason.
	OutcomeDenied Outcome = "denied"
	// OutcomeAllowed means the call may happen as declared.
	OutcomeAllowed Outcome = "allowed"
	// OutcomeObligated means the call may happen only with Grant.Withhold
	// satisfied.
	OutcomeObligated Outcome = "allowed_with_obligations"
)

// Effect classes a Request may declare. The set is open on the wire: the plane
// refuses a class it does not recognise rather than reading it as nothing.
const (
	// EffectReadsCorpus is work that reads Shoal's own evidence record.
	EffectReadsCorpus = "reads-corpus"
	// EffectEgressesContent is work that transmits corpus content off the
	// host running Shoal. Derive it from the provider actually configured: the
	// same caller is egress-free against a loopback model.
	EffectEgressesContent = "egresses-content"
	// EffectMutatesExternal is work that changes something outside Shoal's
	// evidence record.
	EffectMutatesExternal = "external"
)

// DispatchState is the state of the action record a report closed.
type DispatchState string

const (
	DispatchQueued    DispatchState = "queued"
	DispatchClaimed   DispatchState = "claimed"
	DispatchSucceeded DispatchState = "succeeded"
	DispatchFailed    DispatchState = "failed"
	DispatchCanceled  DispatchState = "canceled"
)

// RequestContext accompanies every call. RequestID is required and
// CorrelationID optional, both unpadded base64url.
type RequestContext struct {
	RequestID     string    `json:"request_id"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	ReasonCode    string    `json:"reason_code"`
	ReasonDetail  string    `json:"reason_detail,omitempty"`
	Deadline      time.Time `json:"deadline"`
}

// Request is a caller stating what it is about to do. Input is the
// declaration the action's registered input schema requires, never the
// payload itself.
type Request struct {
	Context         RequestContext  `json:"context"`
	ID              string          `json:"id"`
	IdempotencyKey  string          `json:"idempotency_key"`
	TokenID         string          `json:"token_id"`
	AgentID         string          `json:"agent_id"`
	AgentGeneration int64           `json:"agent_generation"`
	Capability      string          `json:"capability"`
	Action          string          `json:"action"`
	SourceID        []byte          `json:"source_id"`
	PolicyID        []byte          `json:"policy_id"`
	ObjectID        string          `json:"object_id"`
	Effects         []string        `json:"effects"`
	Input           json.RawMessage `json:"input"`
	Disclosures     []string        `json:"disclosures,omitempty"`
	Lease           time.Duration   `json:"lease"`
}

// Token is what an admitted caller returns on the matching Report. It carries
// no authority of its own; the plane re-checks every field.
type Token struct {
	ActionID  string    `json:"action_id"`
	TokenID   string    `json:"token_id"`
	Version   uint64    `json:"version"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validate reports whether the token could close the loop at all: both IDs
// decode to 1..MaxIDBytes bytes (the bound the report route applies), the
// version is set, and an expiry is present. A missing expiry is refused rather
// than read as "no deadline", because no report window can be shown to exist.
func (t Token) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"action ID", t.ActionID}, {"token ID", t.TokenID},
	} {
		decoded, err := DecodeID(field.value)
		if err != nil {
			return fmt.Errorf("token %s: %w", field.name, err)
		}
		if len(decoded) > MaxIDBytes {
			return fmt.Errorf(
				"token %s exceeds the %d-byte bound the report endpoint applies",
				field.name, MaxIDBytes)
		}
	}
	if t.Version == 0 {
		return errors.New("token version is invalid")
	}
	if t.ExpiresAt.IsZero() {
		return errors.New(
			"token carries no expiry, so no report window can be established")
	}
	return nil
}

// Grant is the answer to a Request. Withhold is always present, [] when there
// is nothing to withhold, and is encoded exactly as Request.Disclosures are so
// the two match by equality. A denial carries no Token.
type Grant struct {
	Outcome  Outcome  `json:"outcome"`
	Token    *Token   `json:"token,omitempty"`
	Withhold []string `json:"withhold"`
}

// Report closes an admission. Outcome and Failed are exclusive; ErrorCode is
// required when Failed is set.
type Report struct {
	Context   RequestContext  `json:"context"`
	Token     Token           `json:"token"`
	Outcome   json.RawMessage `json:"outcome,omitempty"`
	Failed    bool            `json:"failed,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	// Effected is how much of an irreversible egress happened before the
	// failure (#427). Valid only with Failed, and only for an action that
	// declares it may egress content.
	//
	// Omitted when nothing left, so a caller that has nothing to report
	// sends the same bytes it sent before this field existed.
	Effected *Effected `json:"effected,omitempty"`
}

// Effected is the volume of a partial egress, in units the plane already
// understands.
//
// An UPPER BOUND on what may have left, never a receipt. It counts bytes
// handed to the transport, which is the most any sender can know: kernel
// buffers and intermediate proxies sit between the write and the reader, so
// "N bytes were received" is not a statement a gateway can make. Read it as
// "at most N bytes may have reached the caller".
//
// That is also why it is never refined. Both gateways know the final count
// at the moment they report — the LLM gateway after its response handler has
// returned, when the bytes passed to the ResponseWriter can no longer grow;
// the effects gateway when its send completes or fails (its client carries
// the field on /complete; the worker that will perform the send, and count a
// partial write, is not in this tree yet) — so the first report is the whole
// report, which is what lets the field be write-once.

// Fixed integers, not a unit/value pair: a unit label would be
// caller-controlled text on a durable record. A gateway with a tokenizer
// derives tokens from bytes itself; that is its own observation and not
// something the plane carries.
type Effected struct {
	Bytes  int64 `json:"bytes"`
	Chunks int64 `json:"chunks,omitempty"`
}

// Receipt acknowledges a Report.
type Receipt struct {
	ActionID   string        `json:"action_id"`
	Version    uint64        `json:"version"`
	State      DispatchState `json:"state"`
	ReportedAt time.Time     `json:"reported_at"`
}

// OutstandingRequest pages through the caller's unreported admissions. After
// is the previous page's Next, empty for the first page.
type OutstandingRequest struct {
	Context RequestContext `json:"context"`
	After   string         `json:"after,omitempty"`
	Limit   int            `json:"limit"`
}

// OutstandingAdmission is an admission granted and never reported. Expired
// means it can no longer be reported: whether the effect happened is unknown
// and stays unknown.
type OutstandingAdmission struct {
	ActionID   string    `json:"action_id"`
	TokenID    string    `json:"token_id"`
	Version    uint64    `json:"version"`
	AdmittedAt time.Time `json:"admitted_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Expired    bool      `json:"expired"`
}

// OutstandingPage is one page. Admissions is always present; Next is omitted
// on the last page.
type OutstandingPage struct {
	Admissions []OutstandingAdmission `json:"admissions"`
	Next       string                 `json:"next,omitempty"`
}

// ErrorResponse is the body of every non-2xx answer. Indeterminate mirrors
// CommitOutcomeHeader.
type ErrorResponse struct {
	Code          shoal.ErrorCode `json:"code"`
	Message       string          `json:"message"`
	Indeterminate bool            `json:"indeterminate,omitempty"`
}

// EncodeID renders opaque identity bytes as the wire spells them.
func EncodeID(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// DecodeID parses a wire identity: non-empty, unpadded base64url.
func DecodeID(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("is required")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("must be unpadded base64url")
	}
	if len(decoded) == 0 {
		return nil, errors.New("is required")
	}
	return decoded, nil
}
