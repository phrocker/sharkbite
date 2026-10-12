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
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/sdk"
)

// CorrelationIDHeader mirrors the explorer's Shoal-Correlation-ID (#527),
// restated rather than imported from the command that defines it.
const CorrelationIDHeader = dispatchapi.CorrelationIDHeader

// pollCorrelationPrefix marks a correlation the gateway minted for one poll.
const pollCorrelationPrefix = "gw-poll-"

// CheckCorrelationID applies exactly the explorer's rules for a
// Shoal-Correlation-ID it is handed: present, at most shoal.MaxIDBytes, valid
// UTF-8, printable, no spaces (dispatchapi.CheckCorrelationID, which a parity
// test in pkg/explorer/webapi holds to the explorer's validators). It must
// accept every correlation the explorer accepts — the record's correlation
// came through those same rules, and a stricter check here would make that
// action unclaimable by any gateway. A parity test in cmd/shoal-explore-web
// holds it to the explorer's header validator.
func CheckCorrelationID(id []byte) error {
	return dispatchapi.CheckCorrelationID(id)
}

// newPollCorrelation mints a fresh correlation for one poll.
func newPollCorrelation(random io.Reader) ([]byte, error) {
	if random == nil {
		random = rand.Reader
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, errors.New("poll correlation: random source failed")
	}
	return []byte(pollCorrelationPrefix + hex.EncodeToString(nonce)), nil
}

// ExtendRequest renews the lease of a claim this worker holds.
type ExtendRequest struct {
	// Context's correlation is the action record's (Action.Correlate).
	Context RequestContext
	ClaimID []byte
	// ClaimFence is the fence from the claim response, and it is what the
	// renewal binds on (#629): the explorer compares it with the fence of the
	// claim the record carries now and does not compare the version. A write
	// that leaves the claim alone — a displaced former holder's ambiguity
	// report is the case this exists for — moves the version, and the holder
	// cannot learn the new one by any route it is authorized for, so a
	// version-bound renewal would strand a live claim mid-effect. There is no
	// version field: the client never sends one on this route, and a fence
	// mismatch is ErrClaimLost (409), which is exactly fence_lost.
	ClaimFence uint64
	// Lease is the silence budget requested from the explorer's now, at most
	// dispatchapi.MaxActionClaimTTL. The explorer clamps the new end to the
	// action's deadline, so the end granted is read from the response.
	Lease time.Duration
}

// Extend renews a live claim (#430), bound on its fence (#629), and returns
// the record as renewed.
//
// The lease end to act on is the returned Action.ClaimLeaseUntil — the
// explorer's clamped end, at most the action's deadline — never now plus the
// requested lease. The fence is unchanged, so Complete and ReportAmbiguity
// keep using the claim's.
//
// A definite refusal is DispatchFenceLost. Nothing was written, and the
// claim, if still held, runs to the lease end it already had and no further:
//
//   - 404: the caller no longer holds this claim on this ref — it lapsed and
//     was re-claimed, the descriptor was rebound to another executor ref
//     (#391: an extension needs the current and the claimed ref), or the
//     action is gone. Completion stays possible after a rebind.
//   - 409: the record's claim is under another fence, or the lease or the
//     deadline has passed (ErrClaimLost), or the attestation gate refused the
//     renewal (ErrAttestationRequired). The explorer answers these with the
//     one code and the client never reads message text, so the status is
//     kept on the error: a worker whose action requires attestation
//     re-attests and extends once more before treating it as lost.
//
// A lost answer is handled as Complete handles one: a transport error, any
// 503 (until #505 a bare 503 may hide ErrActionCommitted), a 502 or 504, or a
// 2xx that does not describe this renewal, may have committed, and the
// identical body is resent once. Bound on the fence, the resend of a renewal
// that did commit is a second renewal of the same claim, not a conflict. A
// record from the resend is returned. Any other answer is
// DispatchIndeterminate — the lease may or may not have moved; the worker
// then acts on the lease end it had before, and completes with the fence.
func (c *DispatchClient) Extend(ctx context.Context, actionID []byte, request ExtendRequest) (Action, error) {
	record, err := c.api.Extend(ctx, actionID, dispatchapi.Extension{
		Context: request.Context.api(), ClaimID: request.ClaimID,
		ClaimFence: request.ClaimFence, Lease: request.Lease,
	})
	if err != nil {
		return Action{}, dispatchError(err)
	}
	return actionFrom(record), nil
}

// AmbiguityReport is what a worker attempted when it can no longer report
// the outcome through Complete (#438).
type AmbiguityReport struct {
	// Context's correlation is the action record's (Action.Correlate).
	Context RequestContext
	// ClaimFence is the fence of the attempt being reported: the one this
	// worker held, whether or not it still does.
	ClaimFence uint64
	// Outcome is dispatchapi.AmbiguityRequestNotSent, AmbiguityEffectObserved
	// or AmbiguityOutcomeUnknown.
	Outcome dispatchapi.AmbiguityOutcome
	// Target and Reference are optional, bounded at
	// dispatchapi.MaxAmbiguityTargetBytes and MaxAmbiguityReferenceBytes,
	// and must be valid UTF-8 made only of printable characters — the
	// explorer's rule, applied before sending so a report is refused before
	// it is sent rather than lost to a 400. Reference is the handle the
	// target returned, the thing an operator takes to the other system.
	Target    string
	Reference string
}

// printableAmbiguityText is the explorer's validateAmbiguityText
// (dispatchapi.PrintableText): valid UTF-8 and unicode.IsPrint for every
// codepoint, which admits the ASCII space and refuses every control, format
// (bidi overrides, zero-width) and unassigned codepoint. A parity test holds
// the two to the same verdicts.
func printableAmbiguityText(value string) bool { return dispatchapi.PrintableText(value) }

// ReportAmbiguity records a lost-fence report and returns the record that
// holds it. It always appends (expected_version 0): the caller this route
// exists for cannot learn the current version, and the fence is the
// invariant the explorer asserts.
//
// Four answers, and only the first two are success:
//
//   - accepted: the record now carries this report.
//   - an identical replay accepted (#542): the explorer answers with the
//     record that already carries it, which reads the same as accepted.
//   - refused or not found: DispatchAmbiguityUnrecorded, matching
//     ErrAmbiguityUnrecorded. The explorer answered definitely — a 400 (the
//     per-record report budget is spent, which a co-tenant can do, #514), a
//     404 (this principal's holder entry is gone or never existed, or the
//     action is), or a 401/403 — and nothing about this attempt is on the
//     record. Per #514 this is never "nothing to report": the caller writes
//     the report to its local unrecorded log.
//   - indeterminate: DispatchIndeterminate. The answer was lost (a transport
//     error, any 5xx, or a 2xx that is not a record carrying this report)
//     and an identical resend — safe, since an identical report replays —
//     did not confirm it either. The caller logs it as unrecorded too.
//
// A 409 means a concurrent write moved the record between the explorer's read
// and its compare-and-set, so nothing was written; the identical report is
// resent once, and a definite refusal of the resend is unrecorded.
func (c *DispatchClient) ReportAmbiguity(ctx context.Context, actionID []byte, report AmbiguityReport) (Action, error) {
	record, err := c.api.ReportAmbiguity(ctx, actionID, dispatchapi.Ambiguity{
		Context: report.Context.api(), ClaimFence: report.ClaimFence,
		Outcome: report.Outcome, Target: report.Target, Reference: report.Reference,
	})
	if err != nil {
		return Action{}, dispatchError(err)
	}
	return actionFrom(record), nil
}

// PresentAttestation presents the operator-signed statement in statementFile
// for this client's executor ref, under the idempotency key in keyFile, and
// returns the attestation's expiry (the receipt's expires_at) (#528).
//
// Both files are read on every call, so a statement the operator's signer
// rotates is picked up by the next presentation. The key file's bytes are the
// key exactly as the statement's nonce binds it: nothing is trimmed. A claim
// or extension needs an attestation whose expiry is at or after the lease end
// it would grant, so the worker re-presents before it extends past this.
//
// It is built on sdk.Attestation(), with this client's HTTP client and
// credential: the explorer requires the presenting principal to be the one
// that claims, and the ref to be the credential's binding. The opaque
// verification refusal is DispatchAttestationRefused. Presenting the same
// statement again is idempotent, so a lost or indeterminate answer is
// presented once more; if that is lost too, the result is
// DispatchIndeterminate.
func (c *DispatchClient) PresentAttestation(ctx context.Context, statementFile, keyFile string) (time.Time, error) {
	const op = "attest"
	if c.executorRef == "" {
		return time.Time{}, refusedLocally(op, "presentation needs the executor ref this gateway is bound to")
	}
	statement, err := readBoundedFile(statementFile, attestationapi.MaxReportBytes)
	if err != nil {
		return time.Time{}, refusedLocally(op, "statement file: "+err.Error())
	}
	key, err := readBoundedFile(keyFile, attestationapi.MaxIdempotencyKeyBytes)
	if err != nil {
		return time.Time{}, refusedLocally(op, "key file: "+err.Error())
	}
	if _, err := attestationapi.NewRequest(c.executorRef, key, statement); err != nil {
		return time.Time{}, refusedLocally(op, "the presentation is outside the route's bounds")
	}
	client, err := sdk.New(sdk.Config{
		BaseURL: c.base.String(), HTTPClient: c.http,
		Token: func(context.Context) (string, error) { return c.credential() },
	})
	if err != nil {
		return time.Time{}, refusedLocally(op, "the explorer URL cannot address the attestation route")
	}
	present := func() (attestationapi.Receipt, error) {
		return client.Attestation().Present(ctx, c.executorRef, key, statement)
	}
	receipt, err := present()
	if err != nil && attestationLost(err) {
		if receipt, err = present(); err != nil {
			if attestationLost(err) {
				return time.Time{}, &DispatchError{Op: op, Kind: DispatchIndeterminate,
					reason: "the presentation may have been recorded and the retry could not confirm it",
					cause:  err}
			}
		}
	}
	if err != nil {
		return time.Time{}, attestationError(op, err)
	}
	return receipt.ExpiresAt, nil
}

// attestationLost is an answer after which the presentation may have been
// recorded: a transport error, an indeterminate answer, any 5xx, or a 2xx the
// client could not read.
func attestationLost(err error) bool {
	if errors.Is(err, attestationapi.ErrRefused) {
		return false
	}
	var httpErr *attestationapi.HTTPError
	if !errors.As(err, &httpErr) {
		// The client's own validation errors are refused before sending and
		// were checked above; anything else here is the transport.
		return true
	}
	return httpErr.Indeterminate || httpErr.Status >= 500 ||
		(httpErr.Status >= 200 && httpErr.Status <= 299)
}

func attestationError(op string, err error) error {
	if errors.Is(err, attestationapi.ErrRefused) {
		return &DispatchError{Op: op, Kind: DispatchAttestationRefused,
			Status: http.StatusUnauthorized, cause: err}
	}
	var httpErr *attestationapi.HTTPError
	if !errors.As(err, &httpErr) {
		return &DispatchError{Op: op, Kind: DispatchTransport, Failure: ClassifyFailure(err), cause: err}
	}
	result := &DispatchError{Op: op, Status: httpErr.Status, cause: err}
	if wellFormedCode(httpErr.Code) {
		result.Code = httpErr.Code
	}
	switch httpErr.Status {
	case http.StatusNotFound:
		result.Kind = DispatchNotFound
	case http.StatusConflict:
		result.Kind = DispatchConflict
	case http.StatusBadRequest:
		result.Kind = DispatchInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		result.Kind = DispatchUnauthorized
	default:
		result.Kind = DispatchStatus
	}
	return result
}

// readBoundedFile reads at most limit bytes, refusing an empty or larger
// file. Its errors name neither the path nor the contents.
func readBoundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, errors.New("cannot be read")
	}
	if len(data) == 0 || len(data) > limit {
		return nil, errors.New("is empty or exceeds its bound")
	}
	return data, nil
}
