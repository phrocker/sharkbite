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
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// The logging policy is enforced by the shape of the one function that writes
// a log line, not by convention at its call sites.
//
// Permitted: action ID, claim fence, claim nonce, route action, method, path
// template, status, classification, byte counts, durations.
//
// Never: credentials, the action's input, the filled path or query, bodies,
// headers, or anything the target said in words. In particular never a target
// client's err.Error(): Go's *url.Error embeds the full request URL, and the
// URL carries input-derived path parameters — an account number, a ticket
// reference, a name. So a failure is logged as its FailureKind, which is a
// closed set computed from the error's type and never from its text.
//
// Every field of LogRecord is a type whose values are a closed set or a number,
// except the action ID, which is opaque bytes encoded by the logger itself. A
// route contributes its action, method and template through *Route, which
// only ParseRoutes can construct. There is deliberately no string field, no
// error field and no free-form message.

// Event names what happened. Closed.
type Event string

const (
	EventSkipped   Event = "skipped"
	EventClaimed   Event = "claimed"
	EventSent      Event = "sent"
	EventClassify  Event = "classified"
	EventCompleted Event = "completed"
	EventRefused   Event = "refused"
	EventFenceLost Event = "fence_lost"
	EventDispatch  Event = "dispatch_error"
	EventStartup   Event = "startup"
	// EventExtended: the lease was renewed to the explorer's (clamped) end.
	EventExtended Event = "extended"
	// EventAttested: an attestation was presented ahead of a claim or an
	// extension.
	EventAttested Event = "attested"
	// EventAmbiguity: a lost-fence report is on the record.
	EventAmbiguity Event = "ambiguity_reported"
	// EventUnrecorded: a report could not be recorded and is held in the
	// local unrecorded log for operator reconciliation (#514). The line
	// carries the log's entry count, which is the gauge.
	EventUnrecorded Event = "unrecorded"
	// EventUnrecordedCleared: a held report was recorded on a retry, or
	// acknowledged by the operator, and left the log.
	EventUnrecordedCleared Event = "unrecorded_cleared"
	// EventReadiness: readiness changed; NotReady says why, empty when ready.
	EventReadiness Event = "readiness"
	// EventAbandoned: the drain's grace period ended with runs unfinished;
	// Abandoned counts them.
	EventAbandoned Event = "abandoned"
)

var validEvents = map[Event]bool{
	EventSkipped: true, EventClaimed: true, EventSent: true, EventClassify: true,
	EventCompleted: true, EventRefused: true, EventFenceLost: true,
	EventDispatch: true, EventStartup: true, EventExtended: true,
	EventAttested: true, EventAmbiguity: true, EventUnrecorded: true,
	EventUnrecordedCleared: true, EventReadiness: true, EventAbandoned: true,
}

// FailureKind is a transport failure reduced to its category. Closed.
type FailureKind string

const (
	FailureNone          FailureKind = ""
	FailureEgressRefused FailureKind = "egress_refused"
	FailureDNS           FailureKind = "dns"
	FailureDial          FailureKind = "dial"
	FailureRefused       FailureKind = "connection_refused"
	FailureTLS           FailureKind = "tls"
	FailureTimeout       FailureKind = "timeout"
	FailureCanceled      FailureKind = "canceled"
	FailureReset         FailureKind = "reset"
	FailureOther         FailureKind = "other"
)

var validFailures = map[FailureKind]bool{
	FailureNone: true, FailureEgressRefused: true, FailureDNS: true,
	FailureDial: true, FailureRefused: true, FailureTLS: true,
	FailureTimeout: true, FailureCanceled: true, FailureReset: true,
	FailureOther: true,
}

// ClassifyFailure reduces a transport error to a FailureKind using only its
// type chain. It never reads err.Error().
func ClassifyFailure(err error) FailureKind {
	if err == nil {
		return FailureNone
	}
	var (
		dnsErr    *net.DNSError
		netErr    net.Error
		opErr     *net.OpError
		verifyErr *tls.CertificateVerificationError
		recordErr tls.RecordHeaderError
		alertErr  tls.AlertError
		unknownCA x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, ErrEgressRefused):
		return FailureEgressRefused
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.As(err, &dnsErr):
		return FailureDNS
	case errors.As(err, &verifyErr), errors.As(err, &recordErr),
		errors.As(err, &alertErr), errors.As(err, &unknownCA),
		errors.As(err, &hostErr), errors.As(err, &invalid):
		return FailureTLS
	case errors.Is(err, syscall.ECONNREFUSED):
		return FailureRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return FailureReset
	case errors.As(err, &netErr) && netErr.Timeout():
		return FailureTimeout
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return FailureDial
	}
	return FailureOther
}

// ClaimNonce is the CSPRNG part of a claim ID. It identifies one claim attempt
// in the logs without repeating the pod name, and is not an identity.
type ClaimNonce [ClaimNonceBytes]byte

// LogRecord is everything a log line may carry. See the policy above.
type LogRecord struct {
	Event          Event
	ActionID       []byte
	Fence          uint64
	ClaimNonce     ClaimNonce
	Route          *Route
	Status         int
	Classification Kind
	Failure        FailureKind
	Gate           GateRefusal
	DispatchError  DispatchErrorKind
	// Ambiguity is the outcome of a lost-fence report, closed.
	Ambiguity dispatchapi.AmbiguityOutcome
	// Unrecorded is the unrecorded log's entry count after this event; set
	// on EventUnrecorded and EventUnrecordedCleared.
	Unrecorded int
	// NotReady is why the worker is not ready, on EventReadiness.
	NotReady NotReadyReason
	// Abandoned is how many runs a drain gave up on, on EventAbandoned.
	Abandoned     int
	RequestBytes  int64
	ResponseBytes int64
	Duration      time.Duration
}

// Logger writes LogRecords as JSON lines.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
	now Clock
}

// NewLogger writes to out. A nil clock uses time.Now.
func NewLogger(out io.Writer, now Clock) *Logger {
	if now == nil {
		now = time.Now
	}
	return &Logger{out: out, now: now}
}

type logLine struct {
	Time           string `json:"time"`
	Event          string `json:"event"`
	ActionID       string `json:"action_id,omitempty"`
	Fence          uint64 `json:"fence,omitempty"`
	ClaimNonce     string `json:"claim_nonce,omitempty"`
	Action         string `json:"route_action,omitempty"`
	Method         string `json:"method,omitempty"`
	PathTemplate   string `json:"path_template,omitempty"`
	Status         int    `json:"status,omitempty"`
	Classification string `json:"classification,omitempty"`
	Failure        string `json:"failure,omitempty"`
	Gate           string `json:"gate,omitempty"`
	DispatchError  string `json:"dispatch_error,omitempty"`
	Ambiguity      string `json:"ambiguity,omitempty"`
	Unrecorded     *int   `json:"unrecorded_entries,omitempty"`
	NotReady       string `json:"not_ready,omitempty"`
	Abandoned      int    `json:"abandoned_runs,omitempty"`
	RequestBytes   int64  `json:"request_bytes,omitempty"`
	ResponseBytes  int64  `json:"response_bytes,omitempty"`
	DurationMillis int64  `json:"duration_ms,omitempty"`
}

// Log writes one record. A value outside a closed set — reachable only by
// converting an arbitrary string to one of these types — is written as
// "invalid" rather than as the string, so a later caller cannot launder text
// through a type conversion.
func (l *Logger) Log(record LogRecord) {
	if l == nil || l.out == nil {
		return
	}
	line := logLine{
		Time:          l.now().UTC().Format(time.RFC3339Nano),
		Event:         closed(string(record.Event), validEvents[record.Event]),
		Fence:         record.Fence,
		Status:        record.Status,
		RequestBytes:  record.RequestBytes,
		ResponseBytes: record.ResponseBytes,
	}
	if len(record.ActionID) > 0 {
		id := record.ActionID
		if len(id) > 256 {
			id = id[:256]
		}
		line.ActionID = base64.RawURLEncoding.EncodeToString(id)
	}
	if record.ClaimNonce != (ClaimNonce{}) {
		line.ClaimNonce = hex.EncodeToString(record.ClaimNonce[:])
	}
	if record.Route != nil {
		line.Action = record.Route.action
		line.Method = record.Route.method
		line.PathTemplate = record.Route.template
	}
	if record.Classification != "" {
		line.Classification = closed(string(record.Classification),
			validKinds[record.Classification])
	}
	if record.Failure != FailureNone {
		line.Failure = closed(string(record.Failure), validFailures[record.Failure])
	}
	if record.Gate != GateOpen {
		line.Gate = closed(string(record.Gate), validGates[record.Gate])
	}
	if record.DispatchError != "" {
		line.DispatchError = closed(string(record.DispatchError),
			validDispatchErrors[record.DispatchError])
	}
	if record.Ambiguity != "" {
		line.Ambiguity = closed(string(record.Ambiguity), validAmbiguity[record.Ambiguity])
	}
	line.Abandoned = record.Abandoned
	if record.Event == EventUnrecorded || record.Event == EventUnrecordedCleared ||
		record.Event == EventAbandoned {
		count := record.Unrecorded
		line.Unrecorded = &count
	}
	if record.NotReady != "" {
		line.NotReady = closed(string(record.NotReady), validNotReady[record.NotReady])
	}
	if record.Duration > 0 {
		line.DurationMillis = record.Duration.Milliseconds()
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(encoded, '\n'))
}

func closed(value string, valid bool) string {
	if !valid {
		return "invalid"
	}
	return value
}

var validKinds = map[Kind]bool{
	KindNotSent: true, KindSuccess: true, KindReplayed: true, KindConflict: true,
	KindConflictUnverified: true, KindConflictUnmatched: true, KindRetryableStatus: true,
	KindWrittenNoResponse: true, KindRedirect: true, KindRejected: true,
	KindInvalid: true,
}

var validGates = map[GateRefusal]bool{
	GateDraining: true, GateDeadline: true, GateLease: true, GateMisconfigured: true,
	GateRetention: true,
}

var validAmbiguity = map[dispatchapi.AmbiguityOutcome]bool{
	dispatchapi.AmbiguityRequestNotSent: true, dispatchapi.AmbiguityEffectObserved: true,
	dispatchapi.AmbiguityOutcomeUnknown: true,
}

var validNotReady = map[NotReadyReason]bool{
	NotReadyStarting: true, NotReadyDraining: true, NotReadyUnrecordedFull: true,
	NotReadyStopped: true,
}
