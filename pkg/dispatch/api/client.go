// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes bounds every response body the client reads. A claim
// response carries the input (at most MaxActionPayloadBytes) and a pull page
// carries no inputs.
const MaxResponseBytes = 8 << 20

// Config configures a Client.
//
// BaseURL is an absolute http or https URL and may carry a path prefix
// ("https://gw.example/shoal/"); the routes are joined onto it. User info, a
// query and a fragment are refused.
//
// Token supplies the bearer token per request, so a rotating credential works.
// HTTPClient is copied: its redirect policy is replaced so no redirect is ever
// followed (a redirect answer is an unexpected_status error), and its cookie
// jar is dropped so no ambient credential is added.
//
// UserAgent, when set, is sent as User-Agent. Clock supplies now for the
// deadline check and for PullResult.ReceivedAt; time.Now when nil.
//
// ErrorCodes, when set, is the closed vocabulary of error codes Complete may
// send: a failed completion whose code it rejects is refused before sending.
// Nil admits any non-empty code.
type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
	UserAgent  string
	Clock      func() time.Time
	ErrorCodes func(string) bool
}

// Client speaks the worker-facing dispatch routes.
type Client struct {
	base       *url.URL
	http       *http.Client
	token      func(context.Context) (string, error)
	userAgent  string
	clock      func() time.Time
	errorCodes func(string) bool
}

// NewClient validates c and returns a client.
func NewClient(c Config) (*Client, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil || !base.IsAbs() || base.Host == "" || base.Hostname() == "" ||
		(base.Scheme != "http" && base.Scheme != "https") ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery ||
		base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("invalid dispatch client configuration: " +
			"base URL must be an absolute http(s) URL with a host and no " +
			"credentials, query or fragment")
	}
	if c.Token == nil {
		return nil, errors.New(
			"invalid dispatch client configuration: token source is required")
	}
	if strings.ContainsAny(c.UserAgent, "\r\n") {
		return nil, errors.New(
			"invalid dispatch client configuration: user agent contains a line break")
	}
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Jar = nil
	copied := *base
	copied.Path = strings.TrimSuffix(copied.Path, "/")
	copied.RawPath = ""
	clock := c.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Client{
		base: &copied, http: &client, token: c.Token, userAgent: c.UserAgent,
		clock: clock, errorCodes: c.ErrorCodes,
	}, nil
}

// ErrorKind is a failure reduced to what a worker does about it. Closed.
type ErrorKind string

const (
	// KindNotFound (404): on claim, re-pull — the loser of a claim race is
	// told not-found deliberately, to avoid an existence oracle.
	KindNotFound ErrorKind = "not_found"
	// KindConflict (409): the version or claim moved.
	KindConflict ErrorKind = "conflict"
	// KindIndeterminate: the mutation may have committed (503 with
	// Shoal-Commit-Outcome: indeterminate, or a resend that could not confirm
	// a lost answer).
	KindIndeterminate ErrorKind = "indeterminate"
	// KindUnavailable (503 without the header). Claim and Complete never
	// return it for a 503 of their own (see KindRepull and Complete).
	KindUnavailable ErrorKind = "unavailable"
	// KindRepull (Claim only: a transport error, any 503, a 502 or a 504):
	// the claim may or may not have been taken. The caller holds no claim — it
	// must not execute — and re-pulls. The original error is the cause.
	KindRepull       ErrorKind = "repull"
	KindInvalid      ErrorKind = "invalid_argument"
	KindUnauthorized ErrorKind = "unauthorized"
	KindDeadline     ErrorKind = "deadline"
	KindStatus       ErrorKind = "unexpected_status"
	KindTransport    ErrorKind = "transport"
	KindProtocol     ErrorKind = "protocol"
	KindRefused      ErrorKind = "refused_locally"
	KindNoCredential ErrorKind = "no_credential"
	// KindRecordedOtherwise: the record is terminal under this claim in a
	// different state from the one reported. Final; do not report again.
	KindRecordedOtherwise ErrorKind = "recorded_otherwise"
	// KindFenceLost (Extend only, a definite 404 or 409): the lease was not
	// renewed and nothing was written.
	KindFenceLost ErrorKind = "fence_lost"
	// KindAmbiguityUnrecorded (ReportAmbiguity only): the explorer definitely
	// refused the report, so nothing about this attempt is on the record. It
	// is NOT "nothing to report" (#514): keep the report for operator
	// reconciliation. errors.Is(err, ErrAmbiguityUnrecorded) holds for it.
	KindAmbiguityUnrecorded ErrorKind = "ambiguity_unrecorded"
)

// ErrAmbiguityUnrecorded matches a KindAmbiguityUnrecorded error.
var ErrAmbiguityUnrecorded = errors.New("ambiguity report was not recorded")

// Error is every error the client returns. Its text is composed from closed
// values; it never includes a transport error's text or a response body, so
// it is safe to log. Err is the cause, reachable through errors.Is and
// errors.As only.
type Error struct {
	Op     string
	Kind   ErrorKind
	Status int
	// Code is the explorer's error code, when it sent a well-formed one.
	Code string
	// Reason is a fixed explanation, for refusals and protocol errors.
	Reason string
	Err    error
}

func (e *Error) Error() string {
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
	if e.Reason != "" {
		b.WriteString(": ")
		b.WriteString(e.Reason)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is makes ErrAmbiguityUnrecorded match by kind.
func (e *Error) Is(target error) bool {
	return target == ErrAmbiguityUnrecorded && e.Kind == KindAmbiguityUnrecorded
}

// Kind returns the kind of a client error, or "" if err is not one.
func Kind(err error) ErrorKind {
	var dispatchErr *Error
	if errors.As(err, &dispatchErr) {
		return dispatchErr.Kind
	}
	return ""
}

func refused(op, reason string) error {
	return &Error{Op: op, Kind: KindRefused, Reason: reason}
}

func protocolError(op, reason string) error {
	return &Error{Op: op, Kind: KindProtocol, Reason: reason}
}

// Context is the context every call carries, as bytes. CorrelationID is also
// sent as the Shoal-Correlation-ID header; on every call but Resolve it is
// required, and on an action call it must be the record's correlation_id
// (Record.Correlate).
type Context struct {
	RequestID     []byte
	CorrelationID []byte
	ReasonCode    string
	ReasonDetail  string
	Deadline      time.Time
}

func (c Context) wire(op string, now time.Time) (RequestContext, error) {
	if len(c.RequestID) == 0 || len(c.RequestID) > MaxActionIDBytes {
		return RequestContext{}, refused(op, "request ID is outside its bound")
	}
	if len(c.CorrelationID) > MaxCorrelationIDBytes {
		return RequestContext{}, refused(op, "correlation ID is outside its bound")
	}
	if c.ReasonCode == "" || len(c.ReasonCode) > MaxReasonCodeBytes ||
		strings.TrimSpace(c.ReasonCode) != c.ReasonCode {
		return RequestContext{}, refused(op, "reason code is required, at most "+
			strconv.Itoa(MaxReasonCodeBytes)+" bytes, with no surrounding whitespace")
	}
	if c.Deadline.IsZero() || !now.Before(c.Deadline) {
		return RequestContext{}, refused(op, "request deadline must be in the future")
	}
	wire := RequestContext{
		RequestID:    EncodeID(c.RequestID),
		ReasonCode:   c.ReasonCode,
		ReasonDetail: c.ReasonDetail,
		// The explorer refuses a deadline whose location is not UTC.
		Deadline: c.Deadline.UTC(),
	}
	if len(c.CorrelationID) > 0 {
		wire.CorrelationID = EncodeID(c.CorrelationID)
	}
	return wire, nil
}

// correlationHeader checks a correlation the way the explorer will, before
// anything is sent, and returns the header value.
func correlationHeader(op string, id []byte) (string, error) {
	if len(id) == 0 {
		return "", refused(op, "correlation ID is required: take it from the action record")
	}
	if CheckCorrelationID(id) != nil {
		return "", refused(op, "correlation ID must be at most "+
			strconv.Itoa(MaxCorrelationIDBytes)+
			" bytes of valid UTF-8, printable and without spaces")
	}
	return string(id), nil
}

func checkActionID(op string, id []byte) error {
	if len(id) == 0 || len(id) > MaxActionIDBytes {
		return refused(op, "action ID is outside its bound")
	}
	return nil
}

// PullResult is one page of pullable actions plus what a worker needs to
// estimate the explorer's clock.
type PullResult struct {
	Records []Record
	Next    string
	// Date is the response's Date header; ReceivedAt is the client's clock
	// when the response arrived.
	Date       string
	ReceivedAt time.Time
}

// Pull lists actions the caller may claim. The page has no capability or
// action filter on the server, so the caller filters before claiming.
func (c *Client) Pull(ctx context.Context, request Context, after string, limit int) (PullResult, error) {
	const op = "pull"
	if limit <= 0 || limit > MaxDispatchListResults {
		return PullResult{}, refused(op, fmt.Sprintf("limit must be 1 to %d", MaxDispatchListResults))
	}
	if after != "" {
		if _, err := DecodeID(after); err != nil {
			return PullResult{}, refused(op, "cursor is not unpadded base64url")
		}
	}
	correlation, err := correlationHeader(op, request.CorrelationID)
	if err != nil {
		return PullResult{}, err
	}
	contextValue, err := request.wire(op, c.clock())
	if err != nil {
		return PullResult{}, err
	}
	var page pageWire
	header, receivedAt, err := c.post(ctx, op, PullRoute, correlation,
		PullRequest{Context: contextValue, After: after, Limit: limit}, &page)
	if err != nil {
		return PullResult{}, err
	}
	result := PullResult{
		Records: make([]Record, 0, len(page.Actions)), Next: page.Next,
		Date: header.Get("Date"), ReceivedAt: receivedAt,
	}
	for _, wire := range page.Actions {
		record, err := decodeRecord(wire)
		if err != nil {
			return PullResult{}, protocolError(op, err.Error())
		}
		result.Records = append(result.Records, record)
	}
	return result, nil
}

// Claim names the claim being taken.
type Claim struct {
	Context         Context
	ExpectedVersion uint64
	// ClaimID is fresh per claim attempt and reused verbatim for transport
	// retries of that attempt, so the claim's replay branch answers them.
	ClaimID []byte
	Lease   time.Duration
}

// Claim takes an action under a fence. The response is the only one that
// carries the input and the executor key, and its ClaimFence is what every
// later Extend, Complete and ReportAmbiguity for this claim sends.
//
// Every answer after which the claim may have committed — a transport error,
// any 503, a 502 or 504 — is KindRepull, never a definite failure: the caller
// holds no claim and executes nothing. If the claim did commit, it lapses at
// its lease and reappears on the pull page, and a completion against a claim
// the caller does not hold is refused. The original error is the cause.
//
// The bare-503 case is interim until #505: today a durable claim whose
// publication failed reaches the wire as a 503 without the indeterminate
// header.
func (c *Client) Claim(ctx context.Context, actionID []byte, request Claim) (Record, error) {
	const op = "claim"
	if err := checkActionID(op, actionID); err != nil {
		return Record{}, err
	}
	correlation, err := correlationHeader(op, request.Context.CorrelationID)
	if err != nil {
		return Record{}, err
	}
	if request.ExpectedVersion == 0 {
		return Record{}, refused(op, "expected version is required")
	}
	if len(request.ClaimID) == 0 || len(request.ClaimID) > MaxActionIDBytes {
		return Record{}, refused(op, "claim ID is outside its bound")
	}
	if request.Lease <= 0 || request.Lease > MaxActionClaimTTL {
		return Record{}, refused(op, "lease must be positive and at most "+
			MaxActionClaimTTL.String()+"; the explorer refuses a longer one "+
			"rather than shortening it")
	}
	contextValue, err := request.Context.wire(op, c.clock())
	if err != nil {
		return Record{}, err
	}
	body := ClaimRequest{
		Context: contextValue, ExpectedVersion: request.ExpectedVersion,
		ClaimID: EncodeID(request.ClaimID), Lease: request.Lease,
	}
	var response recordWire
	if _, _, err := c.post(ctx, op, ActionRoute(actionID, VerbClaim), correlation, body, &response); err != nil {
		if answerLost(err) {
			var dispatchErr *Error
			errors.As(err, &dispatchErr)
			return Record{}, &Error{Op: op, Kind: KindRepull,
				Status: dispatchErr.Status, Code: dispatchErr.Code, Err: err}
		}
		return Record{}, err
	}
	record, err := decodeRecord(response)
	if err != nil {
		return Record{}, protocolError(op, err.Error())
	}
	if !bytes.Equal(record.ID, actionID) || !bytes.Equal(record.ClaimID, request.ClaimID) ||
		record.State != DispatchClaimed || record.ClaimFence == 0 {
		return Record{}, protocolError(op, "claim response does not describe the requested claim")
	}
	return record, nil
}

// Extension renews the lease of a claim the caller holds.
type Extension struct {
	// Context's correlation is the record's (Record.Correlate).
	Context Context
	ClaimID []byte
	// ClaimFence is the fence from the claim response, and it is what the
	// renewal binds on (#629): the explorer compares it with the fence of the
	// claim the record carries now and does not compare the version. No
	// version is sent.
	ClaimFence uint64
	// Lease is requested from the explorer's now, at most MaxActionClaimTTL.
	// The explorer clamps the end to the action's deadline, so the end granted
	// is read from the response.
	Lease time.Duration
}

// Extend renews a live claim (#430), bound on its fence (#629), and returns
// the record as renewed. The lease end to act on is the returned
// ClaimLeaseUntil, never now plus the requested lease.
//
// A definite refusal — 404 (the caller no longer holds this claim on this
// ref) or 409 (another fence, a passed lease or deadline, or the attestation
// gate) — is KindFenceLost with the status kept: nothing was written, and the
// claim runs to the lease end it already had and no further.
//
// A lost answer — a transport error, any 503, a 502 or 504, or a 2xx that
// does not describe this renewal — may have committed, and the identical body
// is resent once. Bound on the fence, the resend of a renewal that did commit
// is a second renewal, not a conflict. Any answer to the resend but a record
// is KindIndeterminate.
func (c *Client) Extend(ctx context.Context, actionID []byte, request Extension) (Record, error) {
	const op = "extend"
	if err := checkActionID(op, actionID); err != nil {
		return Record{}, err
	}
	if len(request.ClaimID) == 0 || len(request.ClaimID) > MaxActionIDBytes {
		return Record{}, refused(op, "claim ID is outside its bound")
	}
	if request.ClaimFence == 0 {
		return Record{}, refused(op, "claim fence is required: take it from the claim response")
	}
	if request.Lease <= 0 || request.Lease > MaxActionClaimTTL {
		return Record{}, refused(op, "lease must be positive and at most "+MaxActionClaimTTL.String())
	}
	correlation, err := correlationHeader(op, request.Context.CorrelationID)
	if err != nil {
		return Record{}, err
	}
	contextValue, err := request.Context.wire(op, c.clock())
	if err != nil {
		return Record{}, err
	}
	body := ExtendRequest{
		Context: contextValue, ClaimFence: request.ClaimFence,
		ClaimID: EncodeID(request.ClaimID), Lease: request.Lease,
	}
	path := ActionRoute(actionID, VerbExtend)
	attempt := func() (Record, error) {
		var response recordWire
		if _, _, err := c.post(ctx, op, path, correlation, body, &response); err != nil {
			return Record{}, err
		}
		record, err := decodeRecord(response)
		if err != nil {
			return Record{}, protocolError(op, err.Error())
		}
		// An extension keeps the claim, its fence and its state, and grants an
		// end never past the action's deadline. Its version is whatever the
		// record reached.
		if !bytes.Equal(record.ID, actionID) || !bytes.Equal(record.ClaimID, request.ClaimID) ||
			record.ClaimFence != request.ClaimFence || record.Version == 0 ||
			record.State != DispatchClaimed || record.ClaimLeaseUntil.IsZero() ||
			(!record.Deadline.IsZero() && record.ClaimLeaseUntil.After(record.Deadline)) {
			return Record{}, protocolError(op, "extend response does not describe this claim's renewal")
		}
		return record, nil
	}
	record, err := attempt()
	if mayHaveCommitted(err) {
		resent, resendErr := attempt()
		if resendErr == nil {
			return resent, nil
		}
		return Record{}, &Error{Op: op, Kind: KindIndeterminate,
			Reason: "the renewal may have committed and the resend could not confirm it",
			Err:    resendErr}
	}
	if err != nil {
		var dispatchErr *Error
		if errors.As(err, &dispatchErr) &&
			(dispatchErr.Kind == KindNotFound || dispatchErr.Kind == KindConflict) {
			return Record{}, &Error{Op: op, Kind: KindFenceLost,
				Status: dispatchErr.Status, Code: dispatchErr.Code, Err: err}
		}
		return Record{}, err
	}
	return record, nil
}

// Completion is a terminal report: exactly one of Output, or Failed with
// ErrorCode.
type Completion struct {
	Context         Context
	ExpectedVersion uint64
	ClaimID         []byte
	// ClaimFence is the fence from the claim response, and is required: with
	// it the explorer binds the completion to the claim generation and does
	// not compare the version (#484), so a version moved by an extension or an
	// ambiguity report cannot strand the report of an effect that happened.
	ClaimFence uint64
	Output     json.RawMessage
	Failed     bool
	ErrorCode  string
	// Effected is how much of an irreversible egress happened before a
	// failure (#427). Zero when nothing left, and then omitted. Fix it once:
	// the record is write-once and the replay branch compares it, so a resend
	// carries the identical value.
	Effected Effected
}

// Complete reports the outcome under the claim. Evidence is never sent.
//
// A lost answer is recovered by resending the identical body: on a transport
// error, any 503, a 502 or 504, a 2xx that does not describe this report's
// terminal record, or a 400 or 500 (which a server before #547 answered for a
// record it had written and discarded), the report may have committed, and
// the replay branch answers a resend with the committed record.
//
// After such a first answer Complete returns a record or KindIndeterminate
// and nothing else: a resend answered by a transport error, any 503, a 502 or
// 504 is indeterminate; a resend answered by anything else that is not a
// record is followed by one third read, and anything but a record is
// indeterminate. With no possibly-committed answer before it, a first-attempt
// 404 or 409 is definite and returned as it is.
//
// When the committed record is terminal under this claim but differs from
// what was reported — state, error code, effected volume or output (compared
// as JSON values) — Complete returns it together with KindRecordedOtherwise:
// the record is final, and the caller must not report again.
func (c *Client) Complete(ctx context.Context, actionID []byte, completion Completion) (Record, error) {
	const op = "complete"
	if err := checkActionID(op, actionID); err != nil {
		return Record{}, err
	}
	if completion.ExpectedVersion == 0 {
		return Record{}, refused(op, "expected version is required")
	}
	if len(completion.ClaimID) == 0 || len(completion.ClaimID) > MaxActionIDBytes {
		return Record{}, refused(op, "claim ID is outside its bound")
	}
	if completion.ClaimFence == 0 {
		return Record{}, refused(op, "claim fence is required: take it from the claim response")
	}
	correlation, err := correlationHeader(op, completion.Context.CorrelationID)
	if err != nil {
		return Record{}, err
	}
	validCode := c.errorCodes
	if validCode == nil {
		validCode = func(code string) bool { return code != "" }
	}
	switch {
	case completion.Failed && len(completion.Output) != 0:
		return Record{}, refused(op, "a failed completion carries no output")
	case completion.Failed && !validCode(completion.ErrorCode):
		return Record{}, refused(op, "error code is not in the closed vocabulary")
	case !completion.Failed && completion.ErrorCode != "":
		return Record{}, refused(op, "a successful completion carries no error code")
	case !completion.Failed && len(completion.Output) == 0:
		return Record{}, refused(op, "a successful completion requires output")
	case !completion.Failed && !completion.Effected.Zero():
		return Record{}, refused(op, "a successful completion carries no effected volume")
	case completion.Effected.Bytes < 0 || completion.Effected.Chunks < 0 ||
		completion.Effected.Bytes > MaxEffectedBytes ||
		completion.Effected.Chunks > MaxEffectedChunks:
		return Record{}, refused(op, "effected volume is outside its bound")
	case completion.Effected.Chunks > 0 && completion.Effected.Bytes == 0:
		// The explorer would adjudicate this rather than refuse it, and the
		// record would then say the volume is unknown when the worker meant
		// "nothing": a chunk that left carried something.
		return Record{}, refused(op, "effected volume counts chunks but no bytes")
	}
	contextValue, err := completion.Context.wire(op, c.clock())
	if err != nil {
		return Record{}, err
	}
	body := CompleteRequest{
		Context: contextValue, ExpectedVersion: completion.ExpectedVersion,
		ClaimID:    EncodeID(completion.ClaimID),
		ClaimFence: completion.ClaimFence,
		Output:     completion.Output, ErrorCode: completion.ErrorCode,
		Failed: completion.Failed,
	}
	if !completion.Effected.Zero() {
		// Built once with the body, so every attempt below sends this value.
		body.Effected = &Effected{Bytes: completion.Effected.Bytes, Chunks: completion.Effected.Chunks}
	}
	path := ActionRoute(actionID, VerbComplete)
	unconfirmed := func(cause error) error {
		return &Error{Op: op, Kind: KindIndeterminate,
			Reason: "the report may have committed and the resend could not confirm it",
			Err:    cause}
	}
	// attempt sends the body and returns this report's terminal record. A 2xx
	// that does not decode or does not describe it is KindProtocol, and only a
	// 2xx produces that kind here.
	attempt := func() (Record, error) {
		var response recordWire
		if _, _, err := c.post(ctx, op, path, correlation, body, &response); err != nil {
			return Record{}, err
		}
		record, err := decodeRecord(response)
		if err != nil {
			return Record{}, protocolError(op, err.Error())
		}
		// Bound on the fence, the route answers 200 for a fresh terminal write
		// or a replay of one under this fence and claim ID, at a version past
		// the one this worker last saw.
		if !bytes.Equal(record.ID, actionID) || !bytes.Equal(record.ClaimID, completion.ClaimID) ||
			record.ClaimFence != completion.ClaimFence ||
			record.Version <= completion.ExpectedVersion ||
			(record.State != DispatchSucceeded && record.State != DispatchFailed) {
			return Record{}, protocolError(op,
				"completion response does not describe this claim's terminal record")
		}
		return record, nil
	}
	record, err := attempt()
	if mayHaveCommitted(err) || hidesCommittedRecord(err) {
		record, err = attempt()
		switch {
		case err == nil:
		case answerLost(err):
			return Record{}, unconfirmed(err)
		default:
			// Not a record, and not definite after a possibly-committed first
			// answer: the first attempt's write may still be in flight
			// (ErrExecutionAmbiguous), or a pre-#547 server may have discarded
			// the record this resend wrote. Read once more through the replay
			// branch; only a record settles it.
			if record, err = attempt(); err != nil {
				return Record{}, unconfirmed(err)
			}
		}
	}
	if err != nil {
		return Record{}, err
	}
	if !recordedAsReported(record, completion) {
		return record, &Error{Op: op, Kind: KindRecordedOtherwise}
	}
	return record, nil
}

// hidesCommittedRecord is the #492 shape: from a server before #547, a 400 or
// 500 from /complete may be a terminal record that was written and then
// discarded by the handler.
func hidesCommittedRecord(err error) bool {
	var dispatchErr *Error
	return errors.As(err, &dispatchErr) &&
		(dispatchErr.Status == http.StatusInternalServerError ||
			dispatchErr.Status == http.StatusBadRequest)
}

// mayHaveCommitted is a lost answer, or a 2xx whose body does not decode or
// does not describe the request's record: the route answered success, so a
// write or a replay happened.
func mayHaveCommitted(err error) bool {
	return answerLost(err) || Kind(err) == KindProtocol
}

// answerLost is a request that may have been applied whose answer did not
// arrive, or arrived without saying what happened: a transport failure or an
// indeterminate 503; a 502 or 504, which a proxy can answer after the
// explorer processed the request; or a bare 503 — interim until #505.
func answerLost(err error) bool {
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) {
		return false
	}
	switch dispatchErr.Kind {
	case KindTransport, KindIndeterminate, KindUnavailable:
		return true
	}
	return dispatchErr.Status == http.StatusBadGateway ||
		dispatchErr.Status == http.StatusGatewayTimeout
}

// recordedAsReported compares the committed record with the report: state,
// error code, for a failure the effected volume, and for a success the output
// as a JSON value, because the explorer canonicalizes what it stores.
func recordedAsReported(record Record, completion Completion) bool {
	if completion.Failed {
		return record.State == DispatchFailed &&
			record.ErrorCode == completion.ErrorCode &&
			record.Effected == completion.Effected
	}
	if record.State != DispatchSucceeded || record.ErrorCode != "" {
		return false
	}
	var recorded, reported any
	if json.Unmarshal(record.Output, &recorded) != nil ||
		json.Unmarshal(completion.Output, &reported) != nil {
		return false
	}
	return reflect.DeepEqual(recorded, reported)
}

// Ambiguity is what a worker attempted when it can no longer report the
// outcome through Complete (#438).
type Ambiguity struct {
	// Context's correlation is the record's (Record.Correlate).
	Context Context
	// ClaimFence is the fence of the attempt being reported: the one this
	// worker held, whether or not it still does.
	ClaimFence uint64
	Outcome    AmbiguityOutcome
	// Target and Reference are optional, at most MaxAmbiguityTargetBytes and
	// MaxAmbiguityReferenceBytes of PrintableText. Reference is the handle the
	// target returned, the thing an operator takes to the other system.
	Target    string
	Reference string
}

func (r Ambiguity) check(op string) error {
	if r.ClaimFence == 0 {
		return refused(op, "claim fence is required")
	}
	if !ValidAmbiguityOutcome(r.Outcome) {
		return refused(op, "outcome is not in the closed vocabulary")
	}
	if len(r.Target) > MaxAmbiguityTargetBytes || !PrintableText(r.Target) {
		return refused(op, "target must be at most "+
			strconv.Itoa(MaxAmbiguityTargetBytes)+" bytes of printable UTF-8")
	}
	if len(r.Reference) > MaxAmbiguityReferenceBytes || !PrintableText(r.Reference) {
		return refused(op, "reference must be at most "+
			strconv.Itoa(MaxAmbiguityReferenceBytes)+" bytes of printable UTF-8")
	}
	return nil
}

// ReportAmbiguity records a lost-fence report and returns the record that
// holds it. It always appends (expected_version 0): the caller this route
// exists for cannot learn the current version.
//
// Accepted, or an identical replay accepted (#542), returns the record. A
// definite refusal — 400 (the report budget is spent, #514), 404, 401 or 403
// — is KindAmbiguityUnrecorded. A 409 means a concurrent write moved the
// record and nothing was written; the identical report is resent once, and a
// definite refusal or conflict of the resend is unrecorded. Any other answer
// may have been recorded: it is resent once, and anything but a record
// carrying this report is KindIndeterminate.
func (c *Client) ReportAmbiguity(ctx context.Context, actionID []byte, report Ambiguity) (Record, error) {
	const op = "ambiguity"
	if err := checkActionID(op, actionID); err != nil {
		return Record{}, err
	}
	if err := report.check(op); err != nil {
		return Record{}, err
	}
	correlation, err := correlationHeader(op, report.Context.CorrelationID)
	if err != nil {
		return Record{}, err
	}
	contextValue, err := report.Context.wire(op, c.clock())
	if err != nil {
		return Record{}, err
	}
	body := AmbiguityRequest{
		Context: contextValue, ExpectedVersion: 0, ClaimFence: report.ClaimFence,
		Outcome: string(report.Outcome), Target: report.Target, Reference: report.Reference,
	}
	path := ActionRoute(actionID, VerbAmbiguity)
	attempt := func() (Record, error) {
		var response recordWire
		if _, _, err := c.post(ctx, op, path, correlation, body, &response); err != nil {
			return Record{}, err
		}
		record, err := decodeRecord(response)
		if err != nil {
			return Record{}, protocolError(op, err.Error())
		}
		if !bytes.Equal(record.ID, actionID) || !carriesReport(record, report) {
			return Record{}, protocolError(op, "ambiguity response is not a record carrying this report")
		}
		return record, nil
	}
	unrecorded := func(cause error) error {
		var dispatchErr *Error
		errors.As(cause, &dispatchErr)
		return &Error{Op: op, Kind: KindAmbiguityUnrecorded,
			Status: dispatchErr.Status, Code: dispatchErr.Code, Err: cause}
	}
	record, err := attempt()
	switch {
	case err == nil:
		return record, nil
	case ambiguityRefused(err):
		return Record{}, unrecorded(err)
	case Kind(err) == KindConflict:
		// Nothing was written; a definite answer to the resend stands.
		record, err = attempt()
		switch {
		case err == nil:
			return record, nil
		case ambiguityRefused(err) || Kind(err) == KindConflict:
			return Record{}, unrecorded(err)
		}
	default:
		// Possibly committed. Only a record from the resend settles it.
		if record, err = attempt(); err == nil {
			return record, nil
		}
	}
	return Record{}, &Error{Op: op, Kind: KindIndeterminate,
		Reason: "the report may have been recorded and the resend could not confirm it",
		Err:    err}
}

// ambiguityRefused is a definite answer from the explorer that wrote nothing.
func ambiguityRefused(err error) bool {
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) || dispatchErr.Status == 0 {
		return false
	}
	switch dispatchErr.Status {
	case http.StatusBadRequest, http.StatusNotFound,
		http.StatusUnauthorized, http.StatusForbidden:
		return dispatchErr.Kind != KindProtocol
	}
	return false
}

// carriesReport reports whether the record holds this report. The reporter's
// identity is the explorer's to record, and is not compared.
func carriesReport(record Record, report Ambiguity) bool {
	for _, recorded := range record.AmbiguityReports {
		if recorded.ClaimFence == report.ClaimFence && recorded.Outcome == report.Outcome &&
			recorded.Target == report.Target && recorded.Reference == report.Reference {
			return true
		}
	}
	return false
}

// Status re-reads one action record. It commits nothing.
func (c *Client) Status(ctx context.Context, actionID []byte, request Context) (Record, error) {
	const op = "status"
	if err := checkActionID(op, actionID); err != nil {
		return Record{}, err
	}
	correlation, err := correlationHeader(op, request.CorrelationID)
	if err != nil {
		return Record{}, err
	}
	contextValue, err := request.wire(op, c.clock())
	if err != nil {
		return Record{}, err
	}
	var response recordWire
	if _, _, err := c.post(ctx, op, ActionRoute(actionID, VerbStatus), correlation,
		contextValue, &response); err != nil {
		return Record{}, err
	}
	record, err := decodeRecord(response)
	if err != nil {
		return Record{}, protocolError(op, err.Error())
	}
	if !bytes.Equal(record.ID, actionID) {
		return Record{}, protocolError(op, "status response names a different action")
	}
	return record, nil
}

// Resolve reads one descriptor. A correlation in request is optional here and
// is not sent as a header. A descriptor naming another ID, an action carrying
// a field this client does not know (it may be a gate this client cannot
// honour), or an action whose two effect spellings disagree is a protocol
// error; which executor ref the descriptor may name is the caller's to check.
func (c *Client) Resolve(ctx context.Context, agentID []byte, request Context) (Descriptor, error) {
	const op = "resolve"
	if len(agentID) == 0 {
		return Descriptor{}, refused(op, "agent ID is required")
	}
	contextValue, err := request.wire(op, c.clock())
	if err != nil {
		return Descriptor{}, err
	}
	var response Descriptor
	if _, _, err := c.post(ctx, op, ResolveRoute(agentID), "", contextValue, &response); err != nil {
		return Descriptor{}, err
	}
	id, err := DecodeID(response.ID)
	if err != nil || !bytes.Equal(id, agentID) {
		return Descriptor{}, protocolError(op, "resolve response names a different descriptor")
	}
	return response, nil
}

// post sends one JSON request and decodes a 2xx answer into out. A non-empty
// correlation, already checked, is sent as Shoal-Correlation-ID.
func (c *Client) post(
	ctx context.Context, op, path, correlation string, body, out any,
) (http.Header, time.Time, error) {
	token, err := c.token(ctx)
	if err != nil {
		// The token source's error may name a file path or a variable; it is
		// kept as the cause and never formatted.
		return nil, time.Time{}, &Error{Op: op, Kind: KindNoCredential, Err: err}
	}
	if strings.ContainsAny(token, "\r\n") || token == "" {
		return nil, time.Time{}, &Error{Op: op, Kind: KindNoCredential}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, time.Time{}, refused(op, "request could not be encoded")
	}
	endpoint := *c.base
	endpoint.Path = c.base.Path + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(),
		bytes.NewReader(encoded))
	if err != nil {
		return nil, time.Time{}, refused(op, "request could not be built")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		request.Header.Set("User-Agent", c.userAgent)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if correlation != "" {
		request.Header.Set(CorrelationIDHeader, correlation)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, time.Time{}, &Error{Op: op, Kind: KindTransport, Err: err}
	}
	defer response.Body.Close()
	receivedAt := c.clock()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	oversize := len(data) > MaxResponseBytes
	if oversize {
		data, readErr = data[:MaxResponseBytes], nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, receivedAt, statusError(op, response, data)
	}
	if readErr != nil {
		return nil, receivedAt, &Error{Op: op, Kind: KindTransport,
			Status: response.StatusCode, Err: readErr}
	}
	if oversize {
		return nil, receivedAt, &Error{Op: op, Kind: KindProtocol,
			Status: response.StatusCode, Reason: "response exceeds the client's bound"}
	}
	if err := decodeResponse(data, out); err != nil {
		return nil, receivedAt, &Error{Op: op, Kind: KindProtocol,
			Status: response.StatusCode, Reason: "response is not the expected JSON", Err: err}
	}
	return response.Header, receivedAt, nil
}

func statusError(op string, response *http.Response, body []byte) error {
	result := &Error{Op: op, Status: response.StatusCode}
	// The code is read leniently, as the effects gateway always read it: it
	// decides nothing (the status does) and wellFormedCode bounds what is
	// kept, so a proxy's odd error body costs the code at most.
	var envelope struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &envelope) == nil && wellFormedCode(envelope.Code) {
		result.Code = envelope.Code
	}
	switch response.StatusCode {
	case http.StatusNotFound:
		result.Kind = KindNotFound
	case http.StatusConflict:
		result.Kind = KindConflict
	case http.StatusServiceUnavailable:
		result.Kind = KindUnavailable
		if strings.EqualFold(strings.TrimSpace(response.Header.Get(CommitOutcomeHeader)),
			CommitOutcomeIndeterminate) {
			result.Kind = KindIndeterminate
		}
	case http.StatusBadRequest:
		result.Kind = KindInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		result.Kind = KindUnauthorized
	case http.StatusGatewayTimeout:
		result.Kind = KindDeadline
	default:
		result.Kind = KindStatus
	}
	return result
}

// wellFormedCode admits the explorer's short snake-case error codes and
// nothing else, so a proxy in front of the explorer cannot put text into an
// error message through the code field.
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
