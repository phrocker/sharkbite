// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The rollout switch (#655), against the composition openService builds.

// newDisabledHarness is newEventHarness without -approval-terminal-states,
// as the shipped default runs.
func newDisabledHarness(t *testing.T) (*approvalHarness, *eventProbe) {
	t.Helper()
	probe := newEventProbe()
	h := &approvalHarness{t: t, root: t.TempDir(), withoutTerminalStates: true}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	h.wrapEvents = func(inner fleet.ApprovalEventPublisher) fleet.ApprovalEventPublisher {
		probe.inner = inner
		return probedApprovalEvents{
			ApprovalEventPublisher: inner, probe: probe, resolver: authority.Resolver(),
		}
	}
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")
	return h, probe
}

// TestNothingWritesATerminalStateWhileDisabled: with the switch off, which a
// replica must be while any replica runs a build before #655, no path writes
// a withdrawn or stranded record, and nothing writes an outbox row such a
// build cannot read:
//   - Withdraw refuses deliberately, before reading anything, the same for an
//     ID that exists and one that does not;
//   - Status and Decide report a moved target, a moved policy and a closed
//     window exactly as before #655, and write nothing new;
//   - the sweep refuses.
//
// Every request is then checked: never withdrawn or stranded, and every
// outbox row a build before #655 reads (requester or approver observer).
func TestNothingWritesATerminalStateWhileDisabled(t *testing.T) {
	t.Parallel()
	h, probe := newDisabledHarness(t)
	if _, err := h.register("relay", "relay-registration", 0, true, ""); err != nil {
		t.Fatal(err)
	}
	moved := h.held("disabled-moved")
	movedApproved := h.held("disabled-moved-approved")
	policy := h.held("disabled-policy")
	lapsed := h.held("disabled-lapsed")
	withdrawn := h.held("disabled-withdraw")
	movedReceipt := h.requestFor("relay", moved)
	approvedReceipt := h.requestFor("relay", movedApproved)
	h.mustRequest(requester, policy, fleet.ApprovalPending)
	h.mustRequest(requester, lapsed, fleet.ApprovalPending)
	h.mustRequest(requester, withdrawn, fleet.ApprovalPending)
	if _, err := h.decide(approver, approvedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}

	// Withdraw: a deliberate 409, the same for an ID that exists and one
	// that does not, and nothing written.
	_, existing := h.withdraw(requester, withdrawn.id)
	_, missing := h.withdraw(requester, []byte("disabled-missing"))
	if !errors.Is(existing, fleet.ErrApprovalTerminalStatesDisabled) ||
		!shoal.IsErrorCode(existing, shoal.ErrorConflict) ||
		existing.Error() != missing.Error() {
		t.Fatalf("withdraw while disabled = %v, missing = %v", existing, missing)
	}

	// The relay's generation moves, and the policy too.
	if _, err := h.register("relay", "relay-regenerate", 1, true, ""); err != nil {
		t.Fatal(err)
	}
	for _, request := range []heldRequest{moved, movedApproved} {
		for _, who := range []principal{requester, approver} {
			status := h.statusAs(who, request.id)
			if status.State != fleet.ApprovalUnresolvable ||
				status.Condition != fleet.ApprovalConditionTargetMoved {
				t.Fatalf("%s status as %s = %q/%q stored %q", request.id, who.subject,
					status.State, status.Condition, status.Approval.State)
			}
		}
	}
	// Decide on the moved target decides, as before #655: approval of a
	// request whose target moved is a statement the requester can never use.
	if _, err := h.decide(approver, movedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatalf("decide on a moved target while disabled = %v", err)
	}
	h.reader.value.Store(workspacePolicyGeneration + 1)
	newer := approver
	newer.generation = workspacePolicyGeneration + 1
	policyStatus := h.statusAs(newer, policy.id)
	if policyStatus.State != fleet.ApprovalUnresolvable ||
		policyStatus.Condition != fleet.ApprovalConditionPolicyMoved {
		t.Fatalf("policy-moved status = %q/%q", policyStatus.State, policyStatus.Condition)
	}
	if _, err := h.decide(newer, fleet.ApprovalReceipt{
		ID: policy.id, RequestDigest: policyStatus.Approval.RequestDigest,
		PolicyGeneration: policyStatus.Approval.PolicyGeneration,
	}, fleet.ApprovalVerdictApprove); !errors.Is(err, fleet.ErrApprovalSuperseded) ||
		errors.Is(err, fleet.ErrApprovalStranded) {
		t.Fatalf("decide under a moved policy while disabled = %v", err)
	}
	h.reader.value.Store(workspacePolicyGeneration)

	// The window closes: Status reports it and writes nothing.
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	if status := h.status(lapsed.id); status.State != fleet.ApprovalExpired ||
		status.Condition != fleet.ApprovalConditionWindowClosed ||
		status.Approval.State != fleet.ApprovalPending {
		t.Fatalf("lapsed status = %q/%q stored %q", status.State,
			status.Condition, status.Approval.State)
	}
	if _, err := sweepApprovals(context.Background(), h.opened.approvalService); !errors.Is(
		err, fleet.ErrApprovalTerminalStatesDisabled) {
		t.Fatalf("sweep while disabled = %v", err)
	}

	for _, request := range []heldRequest{moved, movedApproved, policy, lapsed, withdrawn} {
		rows, err := h.opened.approvalOutbox.ApprovalTransitions(context.Background(), request.id)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			state := row.Transition.Record.State
			observer := row.Transition.Publisher.Observer
			if state == fleet.ApprovalWithdrawn || state == fleet.ApprovalStranded ||
				observer == fleet.ApprovalObservedBySweeper {
				t.Fatalf("%s: a disabled host wrote a %s row observed by %q",
					request.id, state, observer)
			}
		}
	}
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventWithdrawn ||
			published.kind == fleet.ApprovalEventStranded {
			t.Fatalf("a disabled host published %s", published.kind)
		}
	}
}

// TestTheSweepRequiresTheTerminalStates: the host refuses to start a sweep
// that the switch does not allow, before it opens anything.
func TestTheSweepRequiresTheTerminalStates(t *testing.T) {
	err := run(context.Background(), []string{
		"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
		"-approval-sweep-interval", "1m",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires -approval-terminal-states") {
		t.Fatalf("a sweep without the terminal states = %v", err)
	}
	err = run(context.Background(), []string{
		"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
		"-approval-terminal-states", "-approval-sweep-interval", "1s",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("a sweep interval below the minimum = %v", err)
	}
}

// TestTheSweepCoversAStoreLargerThanOnePass: a pass whose budget covers part
// of the key space resumes where the last stopped, so every record is
// reached over several passes, and the cycle then wraps to the beginning.
func TestTheSweepCoversAStoreLargerThanOnePass(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	const requests = 5
	for i := 0; i < requests; i++ {
		h.mustRequest(requester, h.held(fmt.Sprintf("sweep-large-%d", i)),
			fleet.ApprovalPending)
	}
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	sweeper := newApprovalSweeper(h.opened.approvalService)
	sweeper.pageLimit, sweeper.maxPages = 2, 1
	expired, passes := 0, 0
	for ; passes < 10 && expired < requests; passes++ {
		total, err := sweeper.pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if total.Examined > 2 {
			t.Fatalf("a pass exceeded its budget: %+v", total)
		}
		expired += total.Expired
	}
	if expired != requests || passes != 3 || len(sweeper.cursor) != 0 {
		t.Fatalf("expired %d in %d passes, cursor %q", expired, passes, sweeper.cursor)
	}
	for i := 0; i < requests; i++ {
		id := []byte(fmt.Sprintf("sweep-large-%d", i))
		if stored := h.status(id).Approval; stored.State != fleet.ApprovalExpired {
			t.Fatalf("%s = %q", id, stored.State)
		}
	}
	// One pass with budget to spare covers the key space exactly once: it
	// stops at the end rather than running on from the beginning.
	whole := newApprovalSweeper(h.opened.approvalService)
	whole.pageLimit = 2
	if total, err := whole.pass(context.Background()); err != nil ||
		total.Examined != requests || len(whole.cursor) != 0 {
		t.Fatalf("an unbounded pass = %+v, %v", total, err)
	}
	// The cycle wrapped: the next pass starts at the beginning again.
	total, err := sweeper.pass(context.Background())
	if err != nil || total.Examined != 2 || len(sweeper.cursor) == 0 {
		t.Fatalf("pass after the wrap = %+v, %v", total, err)
	}
}

// TestTheSweepServesErrorCountersOnly: the health surface gains the sweep's
// two error counters and nothing that tracks tenant workload.
func TestTheSweepServesErrorCountersOnly(t *testing.T) {
	t.Parallel()
	if approvalSweepMetrics(nil) != nil || combineMetrics(nil, nil) != nil {
		t.Fatal("a metrics writer with nothing to serve")
	}
	h := newApprovalHarness(t)
	var b strings.Builder
	combineMetrics(nil, approvalSweepMetrics(h.opened.approvalService))(&b)
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.Fields(line)[0]
		if name != "shoal_fleet_approval_sweep_undecodable_total" &&
			name != "shoal_fleet_approval_sweep_failed_total" &&
			name != "shoal_fleet_approval_sweep_page_failed_total" {
			t.Fatalf("unexpected series %q", name)
		}
	}
	if !strings.Contains(b.String(), "shoal_fleet_approval_sweep_undecodable_total 0") {
		t.Fatalf("metrics = %s", b.String())
	}
}

// TestTheSwitchIsWiredFromTheFlag: the shipped binary, started as an
// operator starts it, refuses a withdrawal until -approval-terminal-states
// is given, and with it answers the missing ID as such.
func TestTheSwitchIsWiredFromTheFlag(t *testing.T) {
	for _, probe := range []struct {
		flags []string
		code  int
		want  string
	}{
		{nil, http.StatusConflict, "not enabled on this host"},
		{[]string{"-approval-terminal-states"}, http.StatusNotFound, "not found"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		output := &lockedBuffer{}
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, append([]string{
				"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
			}, probe.flags...), output)
		}()
		baseURL := waitForListeningURL(t, output)
		body := fmt.Sprintf(`{"request_id":"d2l0aGRyYXc","reason_code":"test","deadline":%q}`,
			time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
		response, err := http.Post(baseURL+"/api/v1/fleet/approvals/"+
			encodeWireID([]byte("no-such-approval"))+"/withdraw",
			"application/json", strings.NewReader(body))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		<-done
		if response.StatusCode != probe.code || !strings.Contains(string(data), probe.want) {
			t.Fatalf("withdraw with %v = %d %s", probe.flags, response.StatusCode, data)
		}
	}
}

// failingScanStore fails every scan that starts after one cursor.
type failingScanStore struct {
	fleet.ApprovalStore
	after string
}

func (s failingScanStore) ScanApprovals(
	ctx context.Context, after []byte, limit int,
) (fleet.ApprovalPage, error) {
	if string(after) == s.after {
		return fleet.ApprovalPage{}, shoal.NewError(shoal.ErrorUnavailable, "injected scan failure")
	}
	return s.ApprovalStore.ScanApprovals(ctx, after, limit)
}

// TestAPageThatKeepsFailingDoesNotHoldTheSweep: a scan that fails at one
// cursor, pass after pass, is counted, and after a few passes the sweep wraps
// past it, so the rest of the key space keeps being swept.
func TestAPageThatKeepsFailingDoesNotHoldTheSweep(t *testing.T) {
	t.Parallel()
	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	h.wrap = func(store fleet.ApprovalStore) fleet.ApprovalStore {
		return failingScanStore{ApprovalStore: store, after: "sweep-stuck-1"}
	}
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")
	for i := 0; i < 4; i++ {
		h.mustRequest(requester, h.held(fmt.Sprintf("sweep-stuck-%d", i)), fleet.ApprovalPending)
	}
	sweeper := newApprovalSweeper(h.opened.approvalService)
	sweeper.pageLimit, sweeper.maxPages = 2, 1
	if total, err := sweeper.pass(context.Background()); err != nil || total.Examined != 2 {
		t.Fatalf("first pass = %+v, %v", total, err)
	}
	for i := 0; i < maxApprovalSweepPageFailures; i++ {
		if _, err := sweeper.pass(context.Background()); err == nil {
			t.Fatalf("pass %d at the failing page succeeded", i)
		}
	}
	if len(sweeper.cursor) != 0 {
		t.Fatalf("the sweep is still held at %q", sweeper.cursor)
	}
	if total, err := sweeper.pass(context.Background()); err != nil || total.Examined != 2 {
		t.Fatalf("pass after wrapping = %+v, %v", total, err)
	}
	if _, _, pageFailed := h.opened.approvalService.ApprovalSweepCounters(); pageFailed !=
		maxApprovalSweepPageFailures {
		t.Fatalf("page failures counted %d", pageFailed)
	}
}

// TestStatusTouchesNoOutboxWhileDisabled: with the switch off Status is the
// read it was before #655. An expiry the requester's Request wrote, whose
// event did not publish, is left for that Request's replay; Status does not
// publish it.
func TestStatusTouchesNoOutboxWhileDisabled(t *testing.T) {
	t.Parallel()
	h, probe := newDisabledHarness(t)
	request := h.held("disabled-status-drain")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	probe.loseBefore(fleet.ApprovalEventExpired, 1)
	if _, err := h.request(requester, request); !errors.Is(err, fleet.ErrApprovalEventUnpublished) {
		t.Fatalf("expiry with its event lost = %v", err)
	}
	if status := h.status(request.id); status.Approval.State != fleet.ApprovalExpired {
		t.Fatalf("status = %q", status.Approval.State)
	}
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventExpired {
			t.Fatal("a disabled Status published the requester's expiry")
		}
	}
	h.mustRequest(requester, request, fleet.ApprovalExpired)
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after the replay", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventExpired)
}
