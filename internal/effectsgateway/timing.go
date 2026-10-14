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
	"errors"
	"net/http"
	"time"
)

// ReportWindow is the fixed margin for one authenticated POST to the explorer
// after the work is done. It is not a setting: making it configurable would
// invite tuning away the margin that keeps a completed effect reportable. It
// is the same value, for the same act, as the LLM gateway's
// minimumReportWindow.
const ReportWindow = 5 * time.Second

// DateResolution is the slack added to a server Date header. HTTP dates have
// one-second resolution and are truncated, so the server's real "now" when it
// wrote the header was up to one second later than the header says.
const DateResolution = time.Second

// Clock is the local monotonic clock. Production uses time.Now, whose values
// carry a monotonic reading so Sub between two of them is immune to wall-clock
// steps; tests substitute a fake that advances explicitly.
//
// Every margin in this file is a server-issued difference applied to a local
// monotonic instant. No local wall-clock reading is ever compared with a
// server timestamp, which is what makes the arithmetic indifferent to skew: a
// worker whose wall clock is thirty seconds fast computes the same margins as
// one that is exact.
type Clock func() time.Time

// ClaimTimes are the server timestamps the claim response carries.
type ClaimTimes struct {
	UpdatedAt       time.Time
	ClaimLeaseUntil time.Time
	Deadline        time.Time
}

// Anchored is a claim's lease and deadline translated onto the local
// monotonic clock.
type Anchored struct {
	LeaseLocal    time.Time
	DeadlineLocal time.Time
	// DeadlineLocalLatest is DeadlineLocal's late-side counterpart: no
	// earlier than the true local instant of the action's deadline, and so no
	// earlier than the end of any lease the explorer can ever grant on this
	// claim, which it clamps to the deadline. Set by AnchorAnswer; zero from
	// Anchor alone.
	DeadlineLocalLatest time.Time
}

// Anchor places a claim's bounds on the local clock.
//
// sent is the local monotonic instant at which this claim ID was first sent.
// The server applied the claim at UpdatedAt, which is at or after the moment
// the request was sent, so
//
//	leaseLocal    = sent + (claim_lease_until − updated_at)
//	deadlineLocal = sent + (deadline − updated_at)
//
// both land no later than the true local instants and are therefore
// conservative: the worker believes its lease ends earlier than it does, never
// later. Using the first send rather than the send that got the answer keeps
// that true across transport retries of the same claim, where the server may
// have applied the first copy and replayed it to a later one.
//
// Both differences are server minus server, so the server's own clock offset
// cancels; only its rate matters, over at most MaxActionClaimTTL.
func Anchor(sent time.Time, claim ClaimTimes) (Anchored, error) {
	if sent.IsZero() {
		return Anchored{}, errors.New("anchor requires the instant the claim was first sent")
	}
	if claim.UpdatedAt.IsZero() || claim.ClaimLeaseUntil.IsZero() || claim.Deadline.IsZero() {
		return Anchored{}, errors.New("claim response is missing updated_at, " +
			"claim_lease_until or deadline")
	}
	lease := claim.ClaimLeaseUntil.Sub(claim.UpdatedAt)
	deadline := claim.Deadline.Sub(claim.UpdatedAt)
	if lease <= 0 {
		return Anchored{}, errors.New("claim lease ends at or before the claim " +
			"was applied")
	}
	if deadline < lease {
		// The fleet clamps the lease to the deadline, so this cannot come from
		// a well-behaved server, and a worker that believed it would hold a
		// lease past the action's deadline would mis-gate every send.
		return Anchored{}, errors.New("claim lease extends past the action deadline")
	}
	return Anchored{
		LeaseLocal:    sent.Add(lease),
		DeadlineLocal: sent.Add(deadline),
	}, nil
}

// AnchorAnswer is Anchor plus the late side of the deadline, from the local
// monotonic instant the answer that was read arrived:
//
//	deadlineLocalLatest = received + (deadline − updated_at)
//
// The explorer applied the claim or renewal at updated_at, before its answer
// arrived, so the true local deadline is no later than this. As with Anchor
// the difference is server minus server and the instant is local monotonic:
// only the server clock's rate matters, never its offset, and no local wall
// reading is compared with a server timestamp. A later received only makes
// the bound later, so the instant read after a call that resent its request
// still bounds the answer that was used.
func AnchorAnswer(sent, received time.Time, claim ClaimTimes) (Anchored, error) {
	anchored, err := Anchor(sent, claim)
	if err != nil {
		return Anchored{}, err
	}
	if received.Before(sent) {
		return Anchored{}, errors.New("an answer cannot arrive before its request was sent")
	}
	anchored.DeadlineLocalLatest = received.Add(claim.Deadline.Sub(claim.UpdatedAt))
	return anchored, nil
}

// RetentionCovers is the PRECHECK retention rule for a key route:
//
//	deadline − created_at ≤ retention
//
// No attempt of an action can precede its created_at or follow its deadline,
// so the interval a target must deduplicate across is contained in this one.
// Both ends are server timestamps from the pull page, so the check needs no
// clock at all and is the same for every worker that evaluates it, including
// a re-claimer that never saw the first attempt.
func RetentionCovers(createdAt, deadline time.Time, retention time.Duration) bool {
	if createdAt.IsZero() || deadline.IsZero() || retention <= 0 {
		return false
	}
	return deadline.Sub(createdAt) <= retention
}

// ServerNow estimates the explorer's current time from a response's Date
// header: the header, plus its resolution, plus the local monotonic time that
// has passed since the response arrived. It errs late, which is the
// conservative direction for every comparison against a deadline below.
//
// An absent or unparseable Date header is refused rather than replaced with
// the local wall clock: substituting the local clock is exactly the skew this
// file exists to keep out.
func ServerNow(header http.Header, receivedAt, now time.Time) (time.Time, error) {
	raw := header.Get("Date")
	if raw == "" {
		return time.Time{}, errors.New("response carries no Date header, so " +
			"the explorer's time cannot be estimated")
	}
	date, err := http.ParseTime(raw)
	if err != nil {
		return time.Time{}, errors.New("response Date header is not an HTTP date")
	}
	elapsed := now.Sub(receivedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return date.Add(DateResolution + elapsed).UTC(), nil
}

// DeadlineAdmits is the PRECHECK deadline rule:
//
//	serverNow + T + ReportWindow < deadline
//
// An action whose remaining deadline cannot hold one operation and its report
// is skipped before it is claimed, which is free; discovering it after the
// claim has already set EffectPossible is not.
func DeadlineAdmits(serverNow, deadline time.Time, operationTimeout time.Duration) bool {
	if serverNow.IsZero() || deadline.IsZero() || operationTimeout <= 0 {
		return false
	}
	return serverNow.Add(operationTimeout + ReportWindow).Before(deadline)
}

// SendGate is the predicate evaluated immediately before a request leaves,
// on every attempt, not once at claim. A liveness check at claim says nothing
// about the instant the bytes go out.
type SendGate struct {
	OperationTimeout time.Duration
	// Renew is whether the lease is being renewed (requires #430). Without
	// renewal the lease must cover the whole operation and its report; with
	// it, the lease need only outlast the next renewal point.
	Renew bool
	// RenewAfter is the renewal interval, L/2, used only when Renew is set.
	RenewAfter time.Duration
}

// GateRefusal names why a send was refused. It is a closed set so it can be
// logged.
type GateRefusal string

const (
	GateOpen          GateRefusal = ""
	GateDraining      GateRefusal = "draining"
	GateDeadline      GateRefusal = "deadline"
	GateLease         GateRefusal = "lease"
	GateMisconfigured GateRefusal = "misconfigured"
	// GateRetention is the PRECHECK retention rule refusing a key route's
	// action whose deadline outlives the target's idempotency window.
	GateRetention GateRefusal = "retention"
)

// Check evaluates the gate at local instant now:
//
//	not draining
//	deadlineLocal − now ≥ T + ReportWindow
//	leaseLocal − now ≥ RenewAfter                 (renewing)
//	leaseLocal − now ≥ T + ReportWindow           (not renewing)
//
// The second line is the real bound and holds either way. The third is the
// only lease-relative thing a renewing worker can usefully assert — not that
// the operation fits in the lease, which can never be true past five minutes,
// but that the next renewal is not already overdue. The fourth is what the
// third becomes when nothing will renew the lease: the lease is then the bound
// on the whole call, as it is for the LLM gateway.
func (g SendGate) Check(anchored Anchored, now time.Time, draining bool) GateRefusal {
	if g.OperationTimeout <= 0 || (g.Renew && g.RenewAfter <= 0) ||
		anchored.LeaseLocal.IsZero() || anchored.DeadlineLocal.IsZero() {
		return GateMisconfigured
	}
	if draining {
		return GateDraining
	}
	needed := g.OperationTimeout + ReportWindow
	if anchored.DeadlineLocal.Sub(now) < needed {
		return GateDeadline
	}
	leaseNeeded := needed
	if g.Renew {
		leaseNeeded = g.RenewAfter
	}
	if anchored.LeaseLocal.Sub(now) < leaseNeeded {
		return GateLease
	}
	return GateOpen
}

// CompletionBudget is how long the worker gives one completion, resends
// included: the report window, or three plane timeouts when that is longer,
// since the client may send the body three times (Complete).
func CompletionBudget(planeTimeout time.Duration) time.Duration {
	if 3*planeTimeout > ReportWindow {
		return 3 * planeTimeout
	}
	return ReportWindow
}

// FallbackReportAttempts is how many ambiguity reports, each bounded by the
// report window, the worker makes before it writes the unrecorded log.
const FallbackReportAttempts = 2

// exitMargin is the process's own exit after the drain.
const exitMargin = 5 * time.Second

// GracePeriod is the minimum terminationGracePeriodSeconds for a pod running
// the gateway:
//
//	T + max(ReportWindow, 3×planeTimeout) + 2×ReportWindow + 5s
//
// It is the worst shutdown path, from the same constants the worker spends:
// a request in flight when SIGTERM arrives runs for up to T, then its
// completion (CompletionBudget), then the fallback ambiguity reports if the
// completion is refused (FallbackReportAttempts report windows), and five
// seconds of process exit. A shorter grace period has the kubelet SIGKILL a
// worker that has performed an effect and not yet reported it, which is the
// stranding this gateway exists to avoid.
func GracePeriod(operationTimeout, planeTimeout time.Duration) time.Duration {
	return operationTimeout + CompletionBudget(planeTimeout) +
		FallbackReportAttempts*ReportWindow + exitMargin
}

// DrainBound is how long a drain waits for work in hand before abandoning
// it: the grace period less the exit margin, which belongs to the
// abandonment's one durable write and the process's exit. The kubelet's clock
// starts before SIGTERM is delivered, so waiting the whole grace period would
// put the abandonment at or after SIGKILL.
func DrainBound(operationTimeout, planeTimeout time.Duration) time.Duration {
	return GracePeriod(operationTimeout, planeTimeout) - exitMargin
}

// GracePeriodSeconds rounds GracePeriod up to whole seconds, which is the unit
// Kubernetes takes. Rounding down would shave the margin the formula exists
// to provide.
func GracePeriodSeconds(operationTimeout, planeTimeout time.Duration) int64 {
	grace := GracePeriod(operationTimeout, planeTimeout)
	seconds := int64(grace / time.Second)
	if grace%time.Second != 0 {
		seconds++
	}
	return seconds
}
