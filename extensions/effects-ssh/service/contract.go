// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/sshclient"
)

// The wire contract. The effects gateway classifies these answers with the
// route in testdata/contract/route.json, and testdata/contract/answers.json
// pins every answer's exact bytes; the gateway's own test
// (internal/effectsgateway/effectsssh_contract_test.go) classifies the same
// fixtures, so each side pins the other.
//
//	answer             status  gateway classification
//	completed          200     success, reference = exit status
//	  (replayed)       200     success, idempotency "replayed"
//	outcome_unknown    409     failed/outcome_unknown (conflict status, the
//	                           body is not the done marker, not retryable)
//	in_progress, busy  503     retry under the same key (retryable)
//	key_conflict       409     failed/outcome_unknown (as outcome_unknown)
//	not_run            502     failed/target_rejected_502
//	refused            4xx/5xx failed/target_rejected_NNN; nothing ran
//
// 409 and 503 are the only statuses that need the route's rules. Without
// them the gateway still never re-runs anything (every repeat goes through
// the journal), so they fail safe only in that sense: the record would be
// wrong. A 409 would read as target_rejected_409, a definite failure for a
// command that may have run, and a 503 as target_rejected_503. That is why
// the route must carry both rules, and the gateway test proves the shipped
// route does (TestEffectsSSHRouteWithoutItsRulesIsUnsafe).

// States are the "state" values of an answer body.
const (
	StateCompleted      = "completed"
	StateOutcomeUnknown = "outcome_unknown"
	StateInProgress     = "in_progress"
	StateBusy           = "busy"
	StateNotRun         = "not_run"
	StateKeyConflict    = "key_conflict"
	StateRefused        = "refused"
)

// Refusal reasons. Each is answered before anything is journaled or dialed,
// for a request whose key the journal does not hold, so nothing ran for it.
const (
	RefusedUnauthorized       = "unauthorized"
	RefusedMethod             = "method_not_allowed"
	RefusedPath               = "not_found"
	RefusedQuery              = "query_not_allowed"
	RefusedBody               = "body_not_allowed"
	RefusedKey                = "idempotency_key_invalid"
	RefusedUnknownCommand     = "unknown_command"
	RefusedJournalFull        = "journal_full"
	RefusedJournalUnavailable = "journal_unavailable"
)

var refusalStatus = map[string]int{
	RefusedUnauthorized:       http.StatusUnauthorized,
	RefusedMethod:             http.StatusMethodNotAllowed,
	RefusedPath:               http.StatusNotFound,
	RefusedQuery:              http.StatusBadRequest,
	RefusedBody:               http.StatusBadRequest,
	RefusedKey:                http.StatusBadRequest,
	RefusedUnknownCommand:     http.StatusNotFound,
	RefusedJournalFull:        http.StatusInsufficientStorage,
	RefusedJournalUnavailable: http.StatusInternalServerError,
}

// ReplayHeader is what the gateway reads, on a 2xx only, as "this answer
// replays an earlier request's result".
const ReplayHeader = "Idempotent-Replayed"

// RetryAfterSeconds are the Retry-After values of the retryable answers. An
// in-progress answer is given only after the request has already waited
// (-attach-wait) for the run, so its retry can be prompt.
const (
	retryAfterInProgress = "1"
	retryAfterBusy       = "5"
)

// Answer is one response, fully determined: status, the headers this
// service sets, and the body bytes.
type Answer struct {
	Status int
	Header map[string]string
	Body   string
}

func jsonHeader() map[string]string {
	return map[string]string{"Content-Type": "application/json"}
}

// Completed is the answer for a recorded completed run.
func Completed(reference string, replayed bool) Answer {
	answer := Answer{Status: http.StatusOK, Header: jsonHeader(),
		Body: `{"state":"completed","reference":"` + reference + `"}`}
	if replayed {
		answer.Header[ReplayHeader] = "true"
	}
	return answer
}

// OutcomeUnknownAnswer is the answer for a key whose command may have run and
// whose result is not known. It is the same bytes whether the run timed out,
// lost its connection, or the process restarted after starting it.
func OutcomeUnknownAnswer(replayed bool) Answer {
	answer := Answer{Status: http.StatusConflict, Header: jsonHeader(),
		Body: `{"state":"outcome_unknown"}`}
	if replayed {
		answer.Header[ReplayHeader] = "true"
	}
	return answer
}

// NotRunAnswer is the answer for a run that opened no session.
func NotRunAnswer(replayed bool) Answer {
	answer := Answer{Status: http.StatusBadGateway, Header: jsonHeader(),
		Body: `{"state":"not_run"}`}
	if replayed {
		answer.Header[ReplayHeader] = "true"
	}
	return answer
}

// KeyConflictAnswer is the answer for a known key under a different request.
// The gateway's binder is pure, so the same action sends the same request
// unless its route changed between attempts (a restart with a new -routes);
// the key's command may then have run, so the answer is classified exactly as
// outcome_unknown is, never as a rejection.
func KeyConflictAnswer() Answer {
	return Answer{Status: http.StatusConflict, Header: jsonHeader(),
		Body: `{"state":"key_conflict"}`}
}

// InProgressAnswer is the answer for a key this process is still running.
func InProgressAnswer() Answer {
	header := jsonHeader()
	header["Retry-After"] = retryAfterInProgress
	return Answer{Status: http.StatusServiceUnavailable, Header: header,
		Body: `{"state":"in_progress"}`}
}

// BusyAnswer is the answer for a new key refused by the concurrency bound;
// nothing was journaled, so the retry is a first attempt.
func BusyAnswer() Answer {
	header := jsonHeader()
	header["Retry-After"] = retryAfterBusy
	return Answer{Status: http.StatusServiceUnavailable, Header: header,
		Body: `{"state":"busy"}`}
}

// Refused is the answer for a refusal.
func Refused(reason string) Answer {
	status, ok := refusalStatus[reason]
	if !ok {
		reason, status = RefusedJournalUnavailable, http.StatusInternalServerError
	}
	header := jsonHeader()
	if reason == RefusedMethod {
		header["Allow"] = http.MethodPost
	}
	return Answer{Status: status, Header: header,
		Body: `{"state":"refused","reason":"` + reason + `"}`}
}

// FinishedAnswer is the answer a durable finished record replays.
func FinishedAnswer(finished *Finished, replayed bool) Answer {
	switch finished.Outcome {
	case OutcomeCompleted:
		return Completed(finished.Reference, replayed)
	case OutcomeNotRun:
		return NotRunAnswer(replayed)
	}
	return OutcomeUnknownAnswer(replayed)
}

func (a Answer) write(w http.ResponseWriter) {
	for name, value := range a.Header {
		w.Header().Set(name, value)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Body)))
	w.WriteHeader(a.Status)
	_, _ = w.Write([]byte(a.Body))
}

// ReferencePattern is the route's reference pattern: the gateway copies the
// reference into ActionRecord.Output only if it matches.
const ReferencePattern = `(exit-[0-9]{1,10}|signal-[A-Z0-9]{1,16})(-truncated)?`

var referencePattern = regexp.MustCompile(`^(?:` + ReferencePattern + `)$`)

var signalName = regexp.MustCompile(`^[A-Z0-9]{1,16}$`)

// SignalLabel is the recorded form of a host-reported signal name.
func SignalLabel(raw string) string {
	if signalName.MatchString(raw) {
		return raw
	}
	return "OTHER"
}

func validReference(reference string) bool { return referencePattern.MatchString(reference) }

// Reference renders a completed run's outcome: "exit-<status>" or
// "signal-<NAME>", with "-truncated" when either stream exceeded its bound. A
// signal name outside [A-Z0-9]{1,16} is target-controlled text that is not a
// signal name, and is recorded as OTHER.
func Reference(result sshclient.Result) string {
	var reference string
	if result.Signal != "" {
		reference = "signal-" + SignalLabel(result.Signal)
	} else {
		status := result.ExitStatus
		if status < 0 {
			status = 0
		}
		reference = "exit-" + strconv.Itoa(status)
	}
	if result.Stdout.Truncated || result.Stderr.Truncated {
		reference += "-truncated"
	}
	return reference
}

// Fingerprint identifies the request a key was first used for. Two requests
// under one key are the same request only if their fingerprints are equal.
// v1 accepts no parameters, so the fingerprint is the command name; the
// parameters' canonical form is included so a later version that accepts
// some cannot collide with this one.
func Fingerprint(command string) string {
	sum := sha256.Sum256([]byte("effects-ssh/v1\x00" + command + "\x00{}"))
	return hex.EncodeToString(sum[:])
}

// validJournalKey: the canonical key spelling, unpadded base64url of 32 bytes.
func validJournalKey(key string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(key)
	return err == nil && len(decoded) == 32
}

// KeyDigest is the short form of a key the log carries. The gateway does not
// log executor keys, and neither does this service.
func KeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}
