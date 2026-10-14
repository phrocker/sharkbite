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

package effectsgateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/executorref"
)

// The dispatch client speaks the routes that exist on main and no others:
// pull, claim, extend (#430), complete, the lost-fence ambiguity route (#484),
// the gateway's own descriptor resolve, and attestation presentation (#528).
//
// It is a thin adapter over pkg/dispatch/api (#446), which holds the wire
// types and the rules for a lost answer: a claim that may have committed is a
// re-pull, an extension binds on the fence alone, a completion binds on the
// claim fence and resends until a record or an honest "indeterminate" settles
// it, and an ambiguity report that was refused is unrecorded. What stays here
// is the gateway's own: its types and error identities (DispatchError and its
// kinds, with the failure class of a transport error), the closed vocabulary
// of error codes it may report (ValidErrorCode), the poll correlation it
// mints, the executor-ref binding of Resolve and PresentAttestation, and
// attestation presentation itself. Golden fixtures in
// pkg/dispatch/api/testdata/wire pin both the bytes it sends and what it makes
// of every answer, as captured before the move.
//
// It has no heartbeat, and that is deliberate (#391): a worker cannot
// truthfully assert a descriptor's liveness, so the gateway never heartbeats
// and carries no registrar credential. It acts as itself, under the
// action-execution credential bound to one executor ref, and resolves only
// the descriptor that ref names (see BindExecutor and Resolve).
//
// Correlation (#527): every claim, extend, complete and ambiguity request
// carries Shoal-Correlation-ID taken from the action record's correlation_id,
// so the worker's transitions thread the trace the enqueuer started. Pull,
// which is about no one action, carries a correlation the client mints per
// poll. Every value is checked with the explorer's rule (CheckCorrelationID)
// before it is sent.
//
// Wire rules, taken from pkg/explorer/webapi/fleet_dispatch.go and checked
// against the real handler in this package's tests:
//
//   - every byte field is unpadded base64url;
//   - a lease is an integer number of nanoseconds, positive and at most
//     dispatchapi.MaxActionClaimTTL — the service refuses a longer one rather
//     than clamping it;
//   - every request carries a context with a request ID, a non-empty reason
//     code and a deadline in the future, in UTC;
//   - 404 and 409 are refusals, and 503 with Shoal-Commit-Outcome:
//     indeterminate means the mutation may have committed;
//   - until #505, a 503 *without* that header may also hide a committed
//     write, so the client does not read a bare 503 from /complete or /claim
//     as a clean refusal either (see Complete and Claim).

const (
	// ClaimNonceBytes is the CSPRNG part of a claim ID.
	ClaimNonceBytes = 16
	// claimIDPrefix versions the claim ID layout.
	claimIDPrefix = "gw1|"
)

// DispatchErrorKind is a dispatch failure reduced to what the worker does
// about it. Closed, and loggable. Every kind the public client returns has
// the same spelling here (dispatchapi.ErrorKind).
type DispatchErrorKind string

const (
	// DispatchNotFound (404): on claim, re-pull — the loser of a claim race
	// is told not-found deliberately, to avoid an existence oracle, so it
	// never means "this action is gone".
	DispatchNotFound = DispatchErrorKind(dispatchapi.KindNotFound)
	// DispatchConflict (409): the version or claim moved.
	DispatchConflict = DispatchErrorKind(dispatchapi.KindConflict)
	// DispatchIndeterminate (503 + Shoal-Commit-Outcome: indeterminate): the
	// mutation may have committed. Claim reports it as DispatchRepull.
	DispatchIndeterminate = DispatchErrorKind(dispatchapi.KindIndeterminate)
	// DispatchUnavailable (503 without the header). Claim and Complete never
	// return it for a 503 of their own: until #505 a bare 503 from either may
	// hide a committed write (see DispatchRepull and Complete).
	DispatchUnavailable = DispatchErrorKind(dispatchapi.KindUnavailable)
	// DispatchRepull (Claim only: a transport error, any 503, a 502 or a
	// 504): the claim may or may not have been taken. The caller holds no
	// claim — it must not execute — and re-pulls. If the claim did commit,
	// the record reappears on the pull page when its lease lapses, and a
	// completion against it is refused by the claim check, so acting as
	// though nothing is held is safe. The original error is its cause. The
	// bare-503 case is interim until #505; see Claim.
	DispatchRepull       = DispatchErrorKind(dispatchapi.KindRepull)
	DispatchInvalid      = DispatchErrorKind(dispatchapi.KindInvalid)
	DispatchUnauthorized = DispatchErrorKind(dispatchapi.KindUnauthorized)
	DispatchDeadline     = DispatchErrorKind(dispatchapi.KindDeadline)
	DispatchStatus       = DispatchErrorKind(dispatchapi.KindStatus)
	DispatchTransport    = DispatchErrorKind(dispatchapi.KindTransport)
	DispatchProtocol     = DispatchErrorKind(dispatchapi.KindProtocol)
	DispatchRefusedLocal = DispatchErrorKind(dispatchapi.KindRefused)
	DispatchNoCredential = DispatchErrorKind(dispatchapi.KindNoCredential)
	// DispatchRecordedOtherwise: the record is terminal under this claim, in
	// a different state from the one reported — for example a success whose
	// output the explorer refused and recorded as failed. Final; do not
	// report again.
	DispatchRecordedOtherwise = DispatchErrorKind(dispatchapi.KindRecordedOtherwise)
	// DispatchFenceLost (Extend only, a definite 404 or 409): the lease was
	// not renewed and nothing was written. The claim, if this worker still
	// holds it, runs to the lease end it already had and no further. See
	// Extend for what each status covers.
	DispatchFenceLost = DispatchErrorKind(dispatchapi.KindFenceLost)
	// DispatchAmbiguityUnrecorded (ReportAmbiguity only): the explorer
	// definitely refused the report, or answered not-found, so nothing about
	// this attempt is on the record. It is NOT "nothing to report" (#514):
	// the caller keeps the report in its local unrecorded log for operator
	// reconciliation. errors.Is(err, ErrAmbiguityUnrecorded) holds for it.
	DispatchAmbiguityUnrecorded = DispatchErrorKind(dispatchapi.KindAmbiguityUnrecorded)
	// DispatchAttestationRefused (PresentAttestation only): the explorer's
	// one opaque verification refusal.
	DispatchAttestationRefused DispatchErrorKind = "attestation_refused"
)

// ErrAmbiguityUnrecorded matches a DispatchAmbiguityUnrecorded error.
var ErrAmbiguityUnrecorded = errors.New("ambiguity report was not recorded")

var validDispatchErrors = map[DispatchErrorKind]bool{
	DispatchNotFound: true, DispatchConflict: true, DispatchIndeterminate: true,
	DispatchUnavailable: true, DispatchInvalid: true, DispatchUnauthorized: true,
	DispatchDeadline: true, DispatchStatus: true, DispatchTransport: true,
	DispatchProtocol: true, DispatchRefusedLocal: true, DispatchNoCredential: true,
	DispatchRecordedOtherwise: true, DispatchRepull: true,
	DispatchFenceLost: true, DispatchAmbiguityUnrecorded: true,
	DispatchAttestationRefused: true,
}

// DispatchError is every error the client returns after validation. Its text
// is composed here from closed values; it never includes a transport error's
// text or a response body.
type DispatchError struct {
	Op     string
	Kind   DispatchErrorKind
	Status int
	// Code is the explorer's error code, when it sent a well-formed one.
	Code string
	// Failure categorizes a transport failure.
	Failure FailureKind
	// reason is a fixed local explanation for refused_locally.
	reason string
	cause  error
}

func (e *DispatchError) Error() string {
	var b strings.Builder
	b.WriteString("dispatch ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d", e.Status)
		if e.Code != "" {
			b.WriteString(", ")
			b.WriteString(e.Code)
		}
		b.WriteString(")")
	}
	if e.Failure != FailureNone {
		b.WriteString(" (")
		b.WriteString(string(e.Failure))
		b.WriteString(")")
	}
	if e.reason != "" {
		b.WriteString(": ")
		b.WriteString(e.reason)
	}
	return b.String()
}

// Unwrap exposes the cause to errors.Is and errors.As only. It is never
// formatted: Error does not include it.
func (e *DispatchError) Unwrap() error { return e.cause }

// Is makes ErrAmbiguityUnrecorded match by kind, so a caller tests for the
// unrecorded case without inspecting statuses.
func (e *DispatchError) Is(target error) bool {
	return target == ErrAmbiguityUnrecorded && e.Kind == DispatchAmbiguityUnrecorded
}

// DispatchKind returns the kind of a client error, or "" if err is not one.
func DispatchKind(err error) DispatchErrorKind {
	var dispatchErr *DispatchError
	if errors.As(err, &dispatchErr) {
		return dispatchErr.Kind
	}
	return ""
}

func refusedLocally(op, reason string) error {
	return &DispatchError{Op: op, Kind: DispatchRefusedLocal, reason: reason}
}

// dispatchError maps a public client error onto this package's identity:
// the same kind, status, code and reason, the failure class of a transport
// error, and the cause chain the gateway always had. Only the kinds that
// carried a cause before the move carry one now: a transport error its
// transport failure, and a re-pull, a lost fence, an unrecorded report or an
// unconfirmed resend the answer that led to it. Protocol and credential
// errors carry none, as before.
func dispatchError(err error) error {
	var apiErr *dispatchapi.Error
	if err == nil || !errors.As(err, &apiErr) {
		return err
	}
	return fromAPIError(apiErr)
}

func fromAPIError(apiErr *dispatchapi.Error) *DispatchError {
	result := &DispatchError{
		Op: apiErr.Op, Kind: DispatchErrorKind(apiErr.Kind),
		Status: apiErr.Status, Code: apiErr.Code, reason: apiErr.Reason,
	}
	switch apiErr.Kind {
	case dispatchapi.KindTransport:
		result.cause = apiErr.Err
		result.Failure = ClassifyFailure(apiErr.Err)
	case dispatchapi.KindRepull, dispatchapi.KindFenceLost,
		dispatchapi.KindAmbiguityUnrecorded, dispatchapi.KindIndeterminate:
		var inner *dispatchapi.Error
		if errors.As(apiErr.Err, &inner) {
			converted := fromAPIError(inner)
			result.cause = converted
			if apiErr.Kind == dispatchapi.KindRepull {
				result.Failure = converted.Failure
			}
		}
	}
	return result
}

// RequestContext is the context every dispatch call carries.
//
// CorrelationID is also sent as the Shoal-Correlation-ID header. On claim,
// extend, complete and ambiguity it is required and must be the action
// record's correlation_id (Action.Correlate sets it); on pull, an empty one is
// replaced by a correlation minted for that poll.
type RequestContext struct {
	RequestID     []byte
	CorrelationID []byte
	ReasonCode    string
	ReasonDetail  string
	Deadline      time.Time
}

func (r RequestContext) api() dispatchapi.Context {
	return dispatchapi.Context{
		RequestID: r.RequestID, CorrelationID: r.CorrelationID,
		ReasonCode: r.ReasonCode, ReasonDetail: r.ReasonDetail, Deadline: r.Deadline,
	}
}

// NewRequestID returns 16 fresh CSPRNG bytes.
func NewRequestID(random io.Reader) ([]byte, error) {
	if random == nil {
		random = rand.Reader
	}
	id := make([]byte, 16)
	if _, err := io.ReadFull(random, id); err != nil {
		return nil, errors.New("request ID: random source failed")
	}
	return id, nil
}

// NewClaimID builds "gw1|" ‖ pod ‖ "|" ‖ 16 CSPRNG bytes.
//
// Fresh per claim attempt of a pulled record, and reused verbatim for
// transport retries of that attempt so the claim's replay branch answers
// them. The pod name makes it pod-identifying in the record; the random
// suffix is what makes it unique, because two replicas sharing a claim ID are
// both told they hold the claim and both told they succeeded (#391). It is
// not an identity: Status and lapsed-claim Pull expose it to co-principals,
// and the worker reports under the principal that claimed, not under this.
func NewClaimID(pod string, random io.Reader) ([]byte, ClaimNonce, error) {
	maxPod := dispatchapi.MaxActionIDBytes - len(claimIDPrefix) - 1 - ClaimNonceBytes
	if pod == "" || len(pod) > maxPod {
		return nil, ClaimNonce{}, fmt.Errorf("pod name must be 1 to %d bytes", maxPod)
	}
	for i := 0; i < len(pod); i++ {
		if pod[i] < 0x21 || pod[i] > 0x7e || pod[i] == '|' {
			return nil, ClaimNonce{}, errors.New("pod name must be printable " +
				"ASCII without spaces or |")
		}
	}
	if random == nil {
		random = rand.Reader
	}
	var nonce ClaimNonce
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return nil, ClaimNonce{}, errors.New("claim ID: random source failed")
	}
	id := make([]byte, 0, len(claimIDPrefix)+len(pod)+1+ClaimNonceBytes)
	id = append(id, claimIDPrefix...)
	id = append(id, pod...)
	id = append(id, '|')
	id = append(id, nonce[:]...)
	return id, nonce, nil
}

// Action is a dispatch record as the worker sees it. Input and ExecutorKey
// are populated on the claim response only; the pull page never carries them,
// and re-reading the action does not return them, so the worker keeps them
// from the claim.
type Action struct {
	ID              []byte
	Version         uint64
	State           dispatchapi.DispatchState
	AgentID         []byte
	AgentGeneration int64
	Capability      string
	Action          string
	Input           json.RawMessage
	Output          json.RawMessage
	ErrorCode       string
	Deadline        time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExecutorKey     ExecutorKey
	ClaimID         []byte
	ClaimFence      uint64
	ClaimLeaseUntil time.Time
	EffectPossible  bool
	// Effected is the volume a failed record says escaped before the failure
	// (#427), zero when none was reported. Read back on a resend, never
	// re-derived: the field is write-once and compared on replay.
	Effected dispatchapi.Effected
	// CorrelationID is the record's correlation_id: the trace the enqueuer
	// started, which every request the worker makes about this action
	// carries (#527).
	CorrelationID []byte
	// AmbiguityReports are the lost-fence reports on the record (#438), read
	// so ReportAmbiguity can confirm its own is among them.
	AmbiguityReports []RecordedAmbiguity
}

// RecordedAmbiguity is one ambiguity report as the record holds it. The
// reporter's identity and time are the explorer's and are not read.
type RecordedAmbiguity = dispatchapi.RecordedAmbiguity

// Correlate returns request carrying this action's correlation ID, which is
// what every claim, extend, complete and ambiguity request about it sends.
func (a Action) Correlate(request RequestContext) RequestContext {
	request.CorrelationID = append([]byte(nil), a.CorrelationID...)
	return request
}

// ClaimTimes returns the server timestamps Anchor needs.
func (a Action) ClaimTimes() ClaimTimes {
	return ClaimTimes{
		UpdatedAt: a.UpdatedAt, ClaimLeaseUntil: a.ClaimLeaseUntil, Deadline: a.Deadline,
	}
}

// actionFrom is the gateway's view of a decoded record. The public client
// has already refused a malformed executor key, so the key here is either
// absent or exactly ExecutorKeyBytes.
func actionFrom(record dispatchapi.Record) Action {
	action := Action{
		ID: record.ID, Version: record.Version, State: record.State,
		AgentID: record.AgentID, AgentGeneration: record.AgentGeneration,
		Capability: record.Capability, Action: record.Action,
		Input: record.Input, Output: record.Output, ErrorCode: record.ErrorCode,
		Deadline: record.Deadline, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		ClaimID: record.ClaimID, ClaimFence: record.ClaimFence,
		ClaimLeaseUntil: record.ClaimLeaseUntil, EffectPossible: record.EffectPossible,
		Effected: record.Effected, CorrelationID: record.CorrelationID,
		AmbiguityReports: record.AmbiguityReports,
	}
	if len(record.ExecutorKey) == ExecutorKeyBytes {
		copy(action.ExecutorKey.bytes[:], record.ExecutorKey)
		action.ExecutorKey.set = true
	}
	return action
}

// Descriptor is the part of a resolved descriptor the startup check reads.
type Descriptor struct {
	ID           []byte
	Generation   int64
	ExecutorRef  string
	Capabilities []dispatchapi.Capability
}

// Actions returns the named capability's actions for VerifyDescriptor.
func (d Descriptor) Actions(capability string) ([]DescriptorAction, bool) {
	for _, declared := range d.Capabilities {
		if declared.Name != capability {
			continue
		}
		actions := make([]DescriptorAction, len(declared.Actions))
		for i, action := range declared.Actions {
			actions[i] = DescriptorAction{
				Name: action.Name, Effects: action.Effects,
				InputSchema:  action.InputSchema,
				OutputSchema: action.OutputSchema,
			}
		}
		return actions, true
	}
	return nil, false
}

// PullPage is one page of pullable actions plus what PRECHECK needs to
// estimate the explorer's clock.
type PullPage struct {
	Actions []Action
	Next    string
	// Header holds the response's Date header only.
	Header     http.Header
	ReceivedAt time.Time
}

// DispatchClient calls the explorer's fleet dispatch routes.
type DispatchClient struct {
	api        *dispatchapi.Client
	base       *url.URL
	http       *http.Client
	credential func() (string, error)
	clock      Clock
	// executorRef is the ref the gateway's credential is bound to, set by
	// BindExecutor. Resolve and PresentAttestation refuse without it.
	executorRef string
}

// NewDispatchClient binds a client to an explorer base URL (which must already
// have passed the dispatch URL rule) and a bearer-token source read per call.
func NewDispatchClient(
	base *url.URL, client *http.Client, credential func() (string, error), clock Clock,
) (*DispatchClient, error) {
	if base == nil || !base.IsAbs() || base.Host == "" {
		return nil, errors.New("dispatch client requires an absolute explorer URL")
	}
	if client == nil || credential == nil {
		return nil, errors.New("dispatch client requires an HTTP client and a credential source")
	}
	if clock == nil {
		clock = time.Now
	}
	copied := *base
	copied.Path = strings.TrimSuffix(copied.Path, "/")
	copied.RawPath = ""
	apiClient, err := dispatchapi.NewClient(dispatchapi.Config{
		BaseURL: copied.String(), HTTPClient: client,
		Token:     func(context.Context) (string, error) { return credential() },
		UserAgent: UserAgent, Clock: clock, ErrorCodes: ValidErrorCode,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch client: %w", err)
	}
	return &DispatchClient{api: apiClient, base: &copied, http: client,
		credential: credential, clock: clock}, nil
}

// BindExecutor returns a copy of the client bound to the executor ref its
// credential is minted for (#391). The ref must pass the explorer's one rule,
// executorref.ValidExecutorRef. A bound client resolves only the descriptor
// that names this ref and presents attestations only for it.
func (c *DispatchClient) BindExecutor(ref string) (*DispatchClient, error) {
	if err := executorref.ValidExecutorRef(ref); err != nil {
		return nil, fmt.Errorf("executor ref: %w", err)
	}
	bound := *c
	bound.executorRef = ref
	return &bound, nil
}

// ExecutorRef is the ref the client is bound to, or "" before BindExecutor.
func (c *DispatchClient) ExecutorRef() string { return c.executorRef }

// Pull lists actions the worker's principal may claim. The page has no
// capability or action filter on the server, so the caller filters before
// claiming: a claim is what sets EffectPossible.
func (c *DispatchClient) Pull(
	ctx context.Context, request RequestContext, after string, limit int,
) (PullPage, error) {
	const op = "pull"
	// A poll is about no one action, so it starts its own trace: minted here,
	// fresh per poll, unless the caller threads one.
	if len(request.CorrelationID) == 0 {
		minted, err := newPollCorrelation(nil)
		if err != nil {
			return PullPage{}, refusedLocally(op, err.Error())
		}
		request.CorrelationID = minted
	}
	result, err := c.api.Pull(ctx, request.api(), after, limit)
	if err != nil {
		return PullPage{}, dispatchError(err)
	}
	page := PullPage{
		Actions: make([]Action, 0, len(result.Records)), Next: result.Next,
		Header: http.Header{}, ReceivedAt: result.ReceivedAt,
	}
	if result.Date != "" {
		page.Header.Set("Date", result.Date)
	}
	for _, record := range result.Records {
		page.Actions = append(page.Actions, actionFrom(record))
	}
	return page, nil
}

// ClaimRequest names the claim being taken.
type ClaimRequest struct {
	Context         RequestContext
	ExpectedVersion uint64
	ClaimID         []byte
	Lease           time.Duration
}

// Claim takes an action under a fence. The response is the only one that
// carries the input and the executor key. Its ClaimFence is what every later
// Extend, Complete and ReportAmbiguity for this claim sends. The request's
// correlation is the pulled record's (Action.Correlate).
//
// Every answer after which the claim may have committed — a transport error,
// any 503, a 502 or 504 — is DispatchRepull, never a definite failure, and
// the caller contract is unchanged: Claim returns no Action, so the caller
// holds no claim and executes nothing. If the claim did commit, it lapses at
// its lease and reappears on the pull page; a completion against a claim the
// worker does not hold is refused. The original error, with its kind, is kept
// as the cause.
//
// The bare-503 case is interim until #505: today ErrActionCommitted (the
// claim was durably written and only its publication failed) reaches the wire
// as a bare 503, indistinguishable from a clean refusal. After #505 a
// header-less 503 is a clean pre-write refusal and narrows back to
// DispatchUnavailable; the other cases stay.
func (c *DispatchClient) Claim(ctx context.Context, actionID []byte, request ClaimRequest) (Action, error) {
	record, err := c.api.Claim(ctx, actionID, dispatchapi.Claim{
		Context: request.Context.api(), ExpectedVersion: request.ExpectedVersion,
		ClaimID: request.ClaimID, Lease: request.Lease,
	})
	if err != nil {
		return Action{}, dispatchError(err)
	}
	return actionFrom(record), nil
}

// Completion is a terminal report. Exactly one of Output and Failed+ErrorCode.
type Completion struct {
	Context         RequestContext
	ExpectedVersion uint64
	ClaimID         []byte
	// ClaimFence is the fence from the claim response, and is required. With
	// it the explorer binds the completion to the claim generation and does
	// not compare the version (#484), so a version moved by an extension or
	// by an ambiguity report cannot strand the report of an effect that
	// happened. Without it the explorer falls back to the exact-version
	// comparison such a report can strand; the client never sends that shape.
	ClaimFence uint64
	Output     json.RawMessage
	Failed     bool
	// ErrorCode is in the closed vocabulary (ValidErrorCode); the client
	// refuses any other before sending.
	ErrorCode string
	// Effected is how much of an irreversible egress happened before a
	// failure (#427): an upper bound on bytes handed to the transport, "at
	// most N bytes may have reached the target", never a receipt. Zero when
	// nothing left, and then omitted from the wire. Valid only on a failure,
	// and only for an action declaring egresses-content; on any other action
	// the explorer adjudicates it as invalid_executor_effected, which
	// Complete returns as DispatchRecordedOtherwise.
	//
	// The caller fixes it once, when its send has completed or failed, and
	// the resend below carries the identical value: the record is write-once
	// and the replay branch compares it, so a different number is a different
	// report.
	Effected dispatchapi.Effected
}

// Complete reports the outcome under the claim. Evidence is never sent: the
// gateway is the first executor that is not trusted, and an evidence anchor is
// a claim about the corpus it never read.
//
// The resend rules are dispatchapi.Client.Complete's, and they are the ones
// this gateway always applied:
//
//   - a transport error, any 503 (interim until #505), a 502 or 504, a 2xx
//     that does not describe this report's terminal record, or a 400 or 500
//     (the #492 shape of a server before #547) may have committed, and the
//     identical body is resent;
//   - after such an answer Complete returns a record or DispatchIndeterminate
//     and nothing else: a resend answered by a lost answer is indeterminate,
//     and a resend answered by anything else that is not a record — 409, 404
//     (ErrExecutionAmbiguous may still be writing), a 400/500 or another
//     status — is followed by one third read, and anything but a record is
//     DispatchIndeterminate;
//   - with no possibly-committed answer before it, a first-attempt 409 or 404
//     is definite and returned as it is.
//
// When the committed record is terminal under this claim but differs from
// what was reported — state, error code, effected volume, or output compared
// as JSON values — Complete returns it together with
// DispatchRecordedOtherwise: the record is final, and the caller must not
// report again.
func (c *DispatchClient) Complete(ctx context.Context, actionID []byte, completion Completion) (Action, error) {
	record, err := c.api.Complete(ctx, actionID, dispatchapi.Completion{
		Context: completion.Context.api(), ExpectedVersion: completion.ExpectedVersion,
		ClaimID: completion.ClaimID, ClaimFence: completion.ClaimFence,
		Output: completion.Output, Failed: completion.Failed,
		ErrorCode: completion.ErrorCode, Effected: completion.Effected,
	})
	if err != nil && dispatchapi.Kind(err) != dispatchapi.KindRecordedOtherwise {
		return Action{}, dispatchError(err)
	}
	return actionFrom(record), dispatchError(err)
}

// Resolve reads the gateway's own descriptor, once at startup.
//
// Only the descriptor bound to this client's executor ref (BindExecutor): an
// unbound client refuses before sending, and a descriptor naming another ref
// is refused rather than returned. The explorer confines an action-execution
// credential the same way (#391); the check here means a gateway handed some
// other credential still never adopts a descriptor that is not its own.
func (c *DispatchClient) Resolve(ctx context.Context, agentID []byte, request RequestContext) (Descriptor, error) {
	const op = "resolve"
	if c.executorRef == "" {
		return Descriptor{}, refusedLocally(op, "resolve needs the executor ref this gateway is bound to")
	}
	resolved, err := c.api.Resolve(ctx, agentID, request.api())
	if err != nil {
		return Descriptor{}, dispatchError(err)
	}
	if resolved.ExecutorRef != c.executorRef {
		return Descriptor{}, refusedLocally(op,
			"the descriptor is bound to another executor ref; a gateway serves only its own")
	}
	return Descriptor{
		ID: append([]byte(nil), agentID...), Generation: resolved.Generation,
		ExecutorRef: resolved.ExecutorRef, Capabilities: resolved.Capabilities,
	}, nil
}

// wellFormedCode admits the explorer's short snake-case error codes and
// nothing else, so a proxy in front of the explorer cannot put text into an
// error message through the code field. The public client applies the same
// rule to dispatch answers; this one is for attestation answers.
func wellFormedCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !(c >= 'a' && c <= 'z' || c == '_') {
			return false
		}
	}
	return true
}
