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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The ErrorCode vocabulary is closed. Each code answers one question an
// operator reconciling the record has to ask — did anything leave, and if it
// did, what does the gateway know about what happened — and two different
// answers must never share a code. Without request_not_sent and
// outcome_unknown as separate codes, "the request never left" and "it left and
// nobody knows" are the same failed record.
const (
	// ErrorRequestNotSent: no byte of the request reached a connection.
	ErrorRequestNotSent = "request_not_sent"
	// ErrorOutcomeUnknown: the request may have been written and nothing
	// establishes whether the effect happened.
	ErrorOutcomeUnknown = "outcome_unknown"
	// ErrorRetryExhausted: every permitted attempt was retryable and at least
	// one may have been written.
	ErrorRetryExhausted = "retry_exhausted"
	// ErrorInputInvalid: the input could not be bound to its route, so nothing
	// was sent.
	ErrorInputInvalid = "input_invalid"
	// errorTargetRejectedPrefix is followed by the three-digit status.
	errorTargetRejectedPrefix = "target_rejected_"
)

// TargetRejected is the error code for a final 4xx or 5xx from the target.
func TargetRejected(status int) string {
	return fmt.Sprintf("%s%03d", errorTargetRejectedPrefix, status)
}

// ValidErrorCode reports whether code belongs to the closed vocabulary. The
// dispatch client refuses to send any other, so a code cannot be composed
// from target-controlled text by a later change that forgets the rule.
func ValidErrorCode(code string) bool {
	switch code {
	case ErrorRequestNotSent, ErrorOutcomeUnknown, ErrorRetryExhausted, ErrorInputInvalid:
		return true
	}
	digits, ok := strings.CutPrefix(code, errorTargetRejectedPrefix)
	if !ok || len(digits) != 3 {
		return false
	}
	status, err := strconv.Atoi(digits)
	return err == nil && status >= 400 && status <= 599 && digits[0] != '0'
}

// Outcome is what the worker does next.
type Outcome int

const (
	// OutcomeRetry: send the identical bound request again (same key, same
	// bytes), within the worker's bounds.
	OutcomeRetry Outcome = iota + 1
	// OutcomeSucceeded: complete with Output.
	OutcomeSucceeded
	// OutcomeFailed: complete failed with ErrorCode.
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeRetry:
		return "retry"
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomeFailed:
		return "failed"
	}
	return "invalid"
}

// Kind names which row of the classification table applied. It is a closed
// set because it is what the logging policy records in place of anything the
// target said.
type Kind string

const (
	KindNotSent            Kind = "not_sent"
	KindSuccess            Kind = "success"
	KindReplayed           Kind = "replayed"
	KindConflict           Kind = "conflict"
	KindConflictUnverified Kind = "conflict_unverified"
	KindConflictUnmatched  Kind = "conflict_unmatched"
	KindRetryableStatus    Kind = "retryable_status"
	KindWrittenNoResponse  Kind = "written_no_response"
	KindRedirect           Kind = "redirect"
	KindRejected           Kind = "rejected"
	KindInvalid            Kind = "invalid_observation"
)

// Response is what was observed of a target's answer. Body holds at most the
// configured limit; Oversize says the body exceeded it and was not read
// further. BodyErr is a failure reading the body after the status line
// arrived. None of these fields is ever logged.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	Oversize bool
	BodyErr  error
}

// Observation is one attempt, reduced to what classification may depend on.
//
// Written must be true from the first moment a request byte may have been
// handed to a connection — the worker sets it from httptrace (GotConn onward),
// which errs towards written. The direction matters: a write misreported as
// never-sent is retried, and on an unprotected route that is a second effect,
// while a never-sent request misreported as written is only an honest
// outcome_unknown.
//
// Exactly one of Err and Response is set.
type Observation struct {
	Written  bool
	Err      error
	Response *Response
}

// Classification is the pure result of Classify.
type Classification struct {
	Outcome   Outcome
	Kind      Kind
	Status    int
	ErrorCode string
	// Output is the closed {status, idempotency, reference?} document,
	// set only when Outcome is OutcomeSucceeded.
	Output json.RawMessage
	// RetryAfter is the target's Retry-After in delta-seconds, when it sent
	// one on a retryable status. The worker caps it.
	RetryAfter    time.Duration
	HasRetryAfter bool
}

// output is the closed success shape. Evidence is never recorded by the
// gateway; see docs/gateway-proxy-design.md "Evidence anchors".
type output struct {
	Status      int    `json:"status"`
	Idempotency string `json:"idempotency"`
	Reference   string `json:"reference,omitempty"`
}

// Classify maps one attempt to what the worker does next. It is a pure
// function: no clock, no I/O, no state.
//
// The table, in the order its rows are tested:
//
//	observed                                   key / natural          unprotected
//	not written (DNS, dial, TLS)               retry                  retry
//	2xx                                        success                success
//	conflict status, body marker matches       success ("conflict")   (refused at config)
//	conflict status, no marker, retryable      retry                  (refused at config)
//	conflict status, no marker, not retryable  failed/outcome_unknown (refused at config)
//	configured retryable status                retry                  failed/outcome_unknown
//	written, then error/timeout/reset          retry                  failed/outcome_unknown
//	3xx (redirects are never followed)         failed/outcome_unknown failed/outcome_unknown
//	other 4xx/5xx                              failed/target_rejected_NNN
//
// Two rows are where a wrong answer costs most. A key conflict means the
// effect already happened under this key: providers answer a reused key with a
// different payload with something like 400 idempotency_error, and reading
// that as failure records "failed, effect possible" for an effect that
// succeeded exactly once — and invites a retry under a new action, which is a
// new key and a second effect. And a write followed by silence on an
// unprotected route is unknown, never "did not happen": re-sending it is the
// double effect this gateway exists to prevent.
func Classify(route *Route, observation Observation) Classification {
	if route == nil || (observation.Err == nil) == (observation.Response == nil) {
		// A malformed observation is a bug in the caller. Unknown is the only
		// answer that cannot be wrong in a harmful direction.
		return Classification{
			Outcome: OutcomeFailed, Kind: KindInvalid, ErrorCode: ErrorOutcomeUnknown,
		}
	}
	unprotected := route.idempotency == IdempotencyUnprotected

	if observation.Err != nil {
		if !observation.Written {
			return Classification{Outcome: OutcomeRetry, Kind: KindNotSent}
		}
		if unprotected {
			return Classification{
				Outcome: OutcomeFailed, Kind: KindWrittenNoResponse,
				ErrorCode: ErrorOutcomeUnknown,
			}
		}
		return Classification{Outcome: OutcomeRetry, Kind: KindWrittenNoResponse}
	}

	response := observation.Response
	status := response.Status
	switch {
	case status >= 200 && status <= 299:
		mode, kind := string(route.idempotency), KindSuccess
		// A replay marker counts only on a 2xx. Providers replay the original
		// response whatever it was, so a replayed 402 is a declined payment
		// being repeated back, and reading the header alone as success would
		// record a decline as a completed effect.
		if route.idempotency == IdempotencyKey && replayed(response.Header) {
			mode, kind = "replayed", KindReplayed
		}
		return succeeded(route, response, mode, kind)

	case route.conflict != nil && route.conflict.matchesStatus(status):
		matched, verifiable := route.conflict.matchesBody(response)
		if matched {
			return succeeded(route, response, "conflict", KindConflict)
		}
		// The status is one the target uses for a key conflict and the body
		// does not say "already done". On a same-key retry the likeliest
		// meaning is "the original is still in flight", whose outcome is not
		// yet known. Where the operator lists the status as retryable, the
		// same key is sent again until the target replays the original
		// outcome or the done marker; otherwise the record says unknown.
		// target_rejected would be a false failure for an original that may
		// still succeed. Conflict rules exist only on key and natural routes.
		if _, retryable := route.retryable[status]; retryable {
			result := Classification{Outcome: OutcomeRetry, Kind: KindRetryableStatus, Status: status}
			result.RetryAfter, result.HasRetryAfter = retryAfter(response.Header)
			return result
		}
		kind := KindConflictUnverified
		if verifiable {
			kind = KindConflictUnmatched
		}
		return Classification{
			Outcome: OutcomeFailed, Kind: kind,
			Status: status, ErrorCode: ErrorOutcomeUnknown,
		}
	}

	if _, retryable := route.retryable[status]; retryable {
		if unprotected {
			return Classification{
				Outcome: OutcomeFailed, Kind: KindRetryableStatus,
				Status: status, ErrorCode: ErrorOutcomeUnknown,
			}
		}
		result := Classification{Outcome: OutcomeRetry, Kind: KindRetryableStatus, Status: status}
		result.RetryAfter, result.HasRetryAfter = retryAfter(response.Header)
		return result
	}
	switch {
	case status >= 300 && status <= 399:
		// The client never follows a redirect, so a 3xx is the target's last
		// word. A 303 after a POST commonly means "created, look over there"
		// and a 307 means "not here"; nothing distinguishes them reliably.
		return Classification{
			Outcome: OutcomeFailed, Kind: KindRedirect,
			Status: status, ErrorCode: ErrorOutcomeUnknown,
		}
	case status >= 400 && status <= 599:
		return Classification{
			Outcome: OutcomeFailed, Kind: KindRejected,
			Status: status, ErrorCode: TargetRejected(status),
		}
	}
	// 1xx reaching here, or a status outside HTTP's range.
	return Classification{
		Outcome: OutcomeFailed, Kind: KindInvalid,
		Status: status, ErrorCode: ErrorOutcomeUnknown,
	}
}

// Exhausted converts a retry the worker will not perform into a failure.
// anyWritten says whether any attempt of this action may have been written;
// if none was, nothing reached the target and the code says so.
func Exhausted(anyWritten bool) Classification {
	if !anyWritten {
		return Classification{
			Outcome: OutcomeFailed, Kind: KindNotSent, ErrorCode: ErrorRequestNotSent,
		}
	}
	return Classification{
		Outcome: OutcomeFailed, Kind: KindRetryableStatus, ErrorCode: ErrorRetryExhausted,
	}
}

func succeeded(route *Route, response *Response, mode string, kind Kind) Classification {
	document := output{
		Status: response.Status, Idempotency: mode,
		Reference: route.extractReference(response),
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		// Unreachable for this struct; recorded as unknown rather than as a
		// success with no output.
		return Classification{
			Outcome: OutcomeFailed, Kind: KindInvalid,
			Status: response.Status, ErrorCode: ErrorOutcomeUnknown,
		}
	}
	return Classification{
		Outcome: OutcomeSucceeded, Kind: kind,
		Status: response.Status, Output: encoded,
	}
}

func (c *conflictRule) matchesStatus(status int) bool {
	_, ok := c.status[status]
	return ok
}

// matchesBody reports whether the body confirms the conflict, and whether it
// could be examined at all.
func (c *conflictRule) matchesBody(response *Response) (matched, verifiable bool) {
	if c.pointer == "" {
		return true, true
	}
	if response.Oversize || response.BodyErr != nil {
		return false, false
	}
	document, ok := decodeBody(response.Body)
	if !ok {
		// A body that is not JSON was read in full and cannot carry the
		// configured marker, so it is a verified non-match.
		return false, true
	}
	value, found := resolvePointer(document, c.pointer)
	if !found {
		return false, true
	}
	text, isString := value.(string)
	if !isString {
		return false, true
	}
	_, matched = c.equals[text]
	return matched, true
}

// extractReference copies a target-side identifier into Output only if it is
// short and matches the operator's pattern exactly. Anything else — oversize
// body, unreadable body, missing field, non-string, pattern miss, more than
// one header value — is success without a reference, never a failure: the
// reference is a convenience for reconciliation and the effect is what
// happened.
func (r *Route) extractReference(response *Response) string {
	rule := r.reference
	if rule == nil {
		return ""
	}
	var candidate string
	if rule.header != "" {
		values := response.Header.Values(rule.header)
		if len(values) != 1 {
			return ""
		}
		candidate = values[0]
	} else {
		if response.Oversize || response.BodyErr != nil {
			return ""
		}
		document, ok := decodeBody(response.Body)
		if !ok {
			return ""
		}
		value, found := resolvePointer(document, rule.pointer)
		if !found {
			return ""
		}
		text, isString := value.(string)
		if !isString {
			return ""
		}
		candidate = text
	}
	if candidate == "" || len(candidate) > MaxReferenceBytes ||
		!rule.pattern.MatchString(candidate) {
		return ""
	}
	return candidate
}

func decodeBody(body []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return document, true
}

func replayed(header http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(header.Get("Idempotent-Replayed")), "true")
}

// maxRetryAfter keeps a hostile Retry-After from parking a claim. The worker
// applies its own, tighter cap against the lease.
const maxRetryAfter = time.Hour

// retryAfter reads the delta-seconds form only. The HTTP-date form needs the
// target's clock to mean anything, and a classifier that read the local clock
// would no longer be pure; the worker falls back to its own backoff.
func retryAfter(header http.Header) (time.Duration, bool) {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return maxRetryAfter, true
	}
	delay := time.Duration(seconds) * time.Second
	if seconds > int64(maxRetryAfter/time.Second) {
		delay = maxRetryAfter
	}
	return delay, true
}

// ReadBounded reads at most limit bytes of a response body, through an
// io.LimitReader, and reports whether there was more. It never reads past
// limit+1 bytes, so a target streaming an unbounded body cannot make the
// worker buffer it.
func ReadBounded(body io.Reader, limit int64) (data []byte, oversize bool, err error) {
	if limit <= 0 {
		return nil, false, errors.New("response limit must be positive")
	}
	data, err = io.ReadAll(io.LimitReader(body, limit+1))
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, err
}
