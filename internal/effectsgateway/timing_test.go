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
	"net/http"
	"testing"
	"time"
)

// fakeClock is a monotonic clock that moves only when told. Its zero is an
// arbitrary local instant with no relation to the server's wall clock, which
// is the point: nothing in the arithmetic may compare the two.
type fakeClock struct{ now time.Time }

func newFakeClock(origin time.Time) *fakeClock { return &fakeClock{now: origin} }
func (c *fakeClock) Now() time.Time            { return c.now }
func (c *fakeClock) Advance(d time.Duration)   { c.now = c.now.Add(d) }

// server is the explorer's wall clock in these tests.
var serverEpoch = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func TestAnchorIsConservativeAndSkewFree(t *testing.T) {
	// Three workers: one whose local clock agrees with the server, one an
	// hour fast, one a day slow. A server that applied the claim 300ms after
	// the worker sent it returns the same record to all three.
	claim := ClaimTimes{
		UpdatedAt:       serverEpoch.Add(300 * time.Millisecond),
		ClaimLeaseUntil: serverEpoch.Add(300*time.Millisecond + 4*time.Minute),
		Deadline:        serverEpoch.Add(time.Hour),
	}
	for _, origin := range []time.Time{
		serverEpoch, serverEpoch.Add(time.Hour), serverEpoch.Add(-24 * time.Hour),
	} {
		clock := newFakeClock(origin)
		sent := clock.Now()
		clock.Advance(450 * time.Millisecond) // round trip
		anchored, err := Anchor(sent, claim)
		if err != nil {
			t.Fatal(err)
		}
		if got := anchored.LeaseLocal.Sub(sent); got != 4*time.Minute {
			t.Fatalf("origin %s: lease anchored %s after send, want 4m", origin, got)
		}
		if got := anchored.DeadlineLocal.Sub(sent); got != time.Hour-300*time.Millisecond {
			t.Fatalf("origin %s: deadline anchored %s after send", origin, got)
		}
		// Conservative: the true local lease end is the local instant of the
		// server apply (sent+300ms) plus 4m. The anchor is never later.
		trueEnd := sent.Add(300*time.Millisecond + 4*time.Minute)
		if anchored.LeaseLocal.After(trueEnd) {
			t.Fatalf("origin %s: anchored lease end is later than the real one", origin)
		}
	}
}

func TestAnchorRefusesImpossibleClaims(t *testing.T) {
	sent := newFakeClock(serverEpoch).Now()
	for name, claim := range map[string]ClaimTimes{
		"missing updated_at": {ClaimLeaseUntil: serverEpoch, Deadline: serverEpoch},
		"missing lease":      {UpdatedAt: serverEpoch, Deadline: serverEpoch},
		"missing deadline":   {UpdatedAt: serverEpoch, ClaimLeaseUntil: serverEpoch},
		"lease before apply": {UpdatedAt: serverEpoch, ClaimLeaseUntil: serverEpoch.Add(-time.Second), Deadline: serverEpoch.Add(time.Hour)},
		"lease at apply":     {UpdatedAt: serverEpoch, ClaimLeaseUntil: serverEpoch, Deadline: serverEpoch.Add(time.Hour)},
		"lease past deadline": {
			UpdatedAt: serverEpoch, ClaimLeaseUntil: serverEpoch.Add(2 * time.Minute),
			Deadline: serverEpoch.Add(time.Minute),
		},
	} {
		if _, err := Anchor(sent, claim); err == nil {
			t.Errorf("%s: anchored", name)
		}
	}
	if _, err := Anchor(time.Time{}, ClaimTimes{
		UpdatedAt: serverEpoch, ClaimLeaseUntil: serverEpoch.Add(time.Minute),
		Deadline: serverEpoch.Add(time.Hour),
	}); err == nil {
		t.Error("anchored without a send instant")
	}
}

// TestAnchorAcrossClaimRetry: the first copy of a claim is applied, its reply
// is lost, and a retry of the same claim ID gets the replayed record. The
// anchor must use the first send, or it would place the lease end later than
// it is by the length of the lost round trip.
func TestAnchorAcrossClaimRetry(t *testing.T) {
	clock := newFakeClock(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	firstSend := clock.Now()
	claim := ClaimTimes{
		UpdatedAt:       serverEpoch,
		ClaimLeaseUntil: serverEpoch.Add(time.Minute),
		Deadline:        serverEpoch.Add(time.Hour),
	}
	clock.Advance(10 * time.Second) // first reply lost, plane timeout, resend
	retrySend := clock.Now()
	fromFirst, _ := Anchor(firstSend, claim)
	fromRetry, _ := Anchor(retrySend, claim)
	if !fromFirst.LeaseLocal.Before(fromRetry.LeaseLocal) {
		t.Fatal("first-send anchoring is not the earlier, conservative one")
	}
	if got := fromFirst.LeaseLocal.Sub(firstSend); got != time.Minute {
		t.Fatalf("lease = %s", got)
	}
}

func TestRetentionCovers(t *testing.T) {
	created := serverEpoch
	for _, row := range []struct {
		name      string
		deadline  time.Time
		retention time.Duration
		want      bool
	}{
		{"well inside", created.Add(time.Hour), 24 * time.Hour, true},
		{"exactly equal", created.Add(24 * time.Hour), 24 * time.Hour, true},
		{"one nanosecond over", created.Add(24*time.Hour + 1), 24 * time.Hour, false},
		{"far over", created.Add(24 * time.Hour), time.Hour, false},
		{"no retention", created.Add(time.Minute), 0, false},
		{"zero deadline", time.Time{}, time.Hour, false},
	} {
		if got := RetentionCovers(created, row.deadline, row.retention); got != row.want {
			t.Errorf("%s: %v, want %v", row.name, got, row.want)
		}
	}
	if RetentionCovers(time.Time{}, created, time.Hour) {
		t.Error("covered with no created_at")
	}
}

func TestServerNowUsesOnlyTheServerClockAndElapsedMonotonic(t *testing.T) {
	header := http.Header{"Date": {serverEpoch.Format(http.TimeFormat)}}
	// The local clock is wildly wrong; only the elapsed time matters.
	clock := newFakeClock(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC))
	received := clock.Now()
	clock.Advance(3 * time.Second)
	got, err := ServerNow(header, received, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := serverEpoch.Add(DateResolution + 3*time.Second); !got.Equal(want) {
		t.Fatalf("server now = %s, want %s", got, want)
	}
	// A clock that appears to run backwards does not make the estimate earlier.
	got, err = ServerNow(header, received, received.Add(-time.Minute))
	if err != nil || !got.Equal(serverEpoch.Add(DateResolution)) {
		t.Fatalf("backwards elapsed = %s, %v", got, err)
	}
	for name, h := range map[string]http.Header{
		"absent":    {},
		"malformed": {"Date": {"yesterday"}},
	} {
		if _, err := ServerNow(h, received, received); err == nil {
			t.Errorf("%s Date header produced an estimate", name)
		}
	}
}

func TestDeadlineAdmits(t *testing.T) {
	const operation = 3 * time.Minute
	now := serverEpoch
	for _, row := range []struct {
		name     string
		deadline time.Time
		want     bool
	}{
		{"room to spare", now.Add(time.Hour), true},
		{"exactly T + report window", now.Add(operation + ReportWindow), false},
		{"one nanosecond more", now.Add(operation + ReportWindow + 1), true},
		{"a conventional 30-second caller deadline", now.Add(30 * time.Second), false},
		{"already past", now.Add(-time.Second), false},
	} {
		if got := DeadlineAdmits(now, row.deadline, operation); got != row.want {
			t.Errorf("%s: %v, want %v", row.name, got, row.want)
		}
	}
	if DeadlineAdmits(now, now.Add(time.Hour), 0) {
		t.Error("admitted with no operation timeout")
	}
}

func TestSendGate(t *testing.T) {
	const operation = time.Minute
	clock := newFakeClock(time.Date(1990, 6, 1, 0, 0, 0, 0, time.UTC))
	sent := clock.Now()
	anchored, err := Anchor(sent, ClaimTimes{
		UpdatedAt:       serverEpoch,
		ClaimLeaseUntil: serverEpoch.Add(4 * time.Minute),
		Deadline:        serverEpoch.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := SendGate{OperationTimeout: operation}
	renewing := SendGate{OperationTimeout: operation, Renew: true, RenewAfter: 2 * time.Minute}
	for _, row := range []struct {
		name     string
		gate     SendGate
		at       time.Duration
		draining bool
		want     GateRefusal
	}{
		{"fresh claim", plain, 0, false, GateOpen},
		{"draining", plain, 0, true, GateDraining},
		{"without renewal, lease still holds T+5s", plain, 4*time.Minute - operation - ReportWindow, false, GateOpen},
		{"without renewal, one nanosecond too late", plain, 4*time.Minute - operation - ReportWindow + 1, false, GateLease},
		{"renewing, before the renewal point", renewing, 2 * time.Minute, false, GateOpen},
		{"renewing, renewal overdue", renewing, 2*time.Minute + 1, false, GateLease},
		{"deadline too close even when renewing", renewing, 10*time.Minute - operation - ReportWindow + 1, false, GateDeadline},
		{"misconfigured", SendGate{}, 0, false, GateMisconfigured},
		{"renewing without an interval", SendGate{OperationTimeout: operation, Renew: true}, 0, false, GateMisconfigured},
	} {
		got := row.gate.Check(anchored, sent.Add(row.at), row.draining)
		if got != row.want {
			t.Errorf("%s: %q, want %q", row.name, got, row.want)
		}
	}
	if got := plain.Check(Anchored{}, sent, false); got != GateMisconfigured {
		t.Errorf("unanchored claim = %q", got)
	}
}

// TestSkewedWorkersAgreeOnTheGate is the duplicate the design warns about: two
// workers skewed in opposite directions must not hold overlapping beliefs
// about one server-side boundary. With anchoring, both compute the same
// local margins from the same server deltas, so their wall clocks never enter.
func TestSkewedWorkersAgreeOnTheGate(t *testing.T) {
	claim := ClaimTimes{
		UpdatedAt:       serverEpoch,
		ClaimLeaseUntil: serverEpoch.Add(time.Minute),
		Deadline:        serverEpoch.Add(time.Hour),
	}
	gate := SendGate{OperationTimeout: 30 * time.Second}
	fast := newFakeClock(serverEpoch.Add(30 * time.Second))
	slow := newFakeClock(serverEpoch.Add(-30 * time.Second))
	fastAnchor, _ := Anchor(fast.Now(), claim)
	slowAnchor, _ := Anchor(slow.Now(), claim)
	for elapsed := time.Duration(0); elapsed <= time.Minute; elapsed += time.Second {
		a := gate.Check(fastAnchor, fast.Now().Add(elapsed), false)
		b := gate.Check(slowAnchor, slow.Now().Add(elapsed), false)
		if a != b {
			t.Fatalf("after %s the skewed workers disagree: %q vs %q", elapsed, a, b)
		}
	}
}

func TestGracePeriod(t *testing.T) {
	for _, row := range []struct {
		operation, plane time.Duration
		seconds          int64
	}{
		// The completion budget is the report window until three plane
		// timeouts exceed it.
		{10 * time.Minute, time.Second, 620},
		{10 * time.Minute, 15 * time.Second, 660},
		{3 * time.Minute, 10 * time.Second, 225},
		{90 * time.Second, time.Second, 110},
		{1500 * time.Millisecond, time.Second, 22},
	} {
		if got := GracePeriodSeconds(row.operation, row.plane); got != row.seconds {
			t.Errorf("T=%s P=%s: grace %ds, want %ds", row.operation, row.plane, got, row.seconds)
		}
	}
}

// TestDrainBoundLeavesTheExitMargin: the drain gives up before the grace
// period ends, by the margin the abandonment and the exit need.
func TestDrainBoundLeavesTheExitMargin(t *testing.T) {
	if DrainBound(time.Minute, time.Second) != GracePeriod(time.Minute, time.Second)-exitMargin {
		t.Fatal("the drain bound does not leave the exit margin")
	}
	if CompletionBudget(time.Second) != ReportWindow || CompletionBudget(10*time.Second) != 30*time.Second {
		t.Fatal("completion budget is not max(ReportWindow, 3×planeTimeout)")
	}
}
