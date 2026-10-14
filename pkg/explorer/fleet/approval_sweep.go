// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The approval sweep (#655).
//
// A held request is moved on by the calls of the two parties to it: the
// requester's re-request, an approver's decision, and either party's Status.
// A request neither party ever touches again stays in its stored state, and
// before #655 nothing could move a request whose target or policy had moved,
// or whose action could never be written, at all. The sweep writes down what
// such a request has become: expired once its window has closed, stranded
// for one of the permanent causes approvalStrandable names. It never decides,
// never re-enables and never materializes anything.
//
// The sweep is neither party, so it acts as neither. It acts as the host's own
// system observer, ApprovalSweeperSubject, in a decision the host mints for
// one approval's scope at a time and binds with its own binder: subject and
// actor ApprovalSweeperSubject, OperationApprovalSweep and nothing else, on
// exactly the request's source and policy. No authenticator mints that
// operation and no service role allows it, so nothing outside the host
// process can present the observer. Each transition it writes is audited and
// published as that observer, through #653's per-transition outbox, and the
// event row says so (ApprovalObservedBySweeper). Nobody is impersonated: the
// observer is recorded as what it is.
//
// Sweeps on several replicas may run at once. Every write is a
// compare-and-set from the version the sweep read, so one writer wins, and a
// loser counts the race and moves on; the next pass reads the record final
// and leaves it.

// ApprovalSweeperSubject is the system observer's identity: the subject and
// actor of every decision a sweep acts under, and of every transition it
// writes. No authenticator names a principal this way.
const ApprovalSweeperSubject shoal.ID = "shoal-system:approval-sweeper"

// ApprovalSweeperClientID is the client the system observer is minted under,
// bound like its subject and actor.
const ApprovalSweeperClientID shoal.ID = "shoal-system:approval-sweeper"

// MaxApprovalSweepDecisionLifetime bounds how far ahead a bound sweep
// decision may expire. The host mints one per approval, for one write and
// its event; a longer one is not the observer's decision.
const MaxApprovalSweepDecisionLifetime = 2 * time.Minute

// ApprovalSweepScope is the one approval a sweep decision is minted for.
type ApprovalSweepScope struct {
	SourceID []byte
	PolicyID []byte
}

// ApprovalSweepBinder returns ctx carrying the host's system observer
// decision for one approval's scope: subject and actor
// ApprovalSweeperSubject, OperationApprovalSweep alone, on exactly that
// source and policy, under the policy generation in force, briefly. The
// service checks all of that before acting under it.
type ApprovalSweepBinder func(context.Context, ApprovalSweepScope) (context.Context, error)

// ApprovalSweepRequest is one page of the approval key space.
type ApprovalSweepRequest struct {
	After []byte
	Limit int
}

// ApprovalSweepResult counts what one page of a sweep did. Next continues
// the scan; empty means the key space has been covered.
type ApprovalSweepResult struct {
	Examined int
	Expired  int
	Stranded int
	// Unpublished counts transitions that committed and whose event did
	// not publish; the next pass retries them (sweepOne).
	Unpublished int
	// Raced counts records another writer moved first.
	Raced int
	// Failed counts records the sweep could not judge or write. They are
	// left as they were.
	Failed int
	// Undecodable counts stored records the scan skipped because they do
	// not decode: corrupt, or written by a newer build. They are left as
	// they are, and no longer end the page they are on.
	Undecodable int
	// Settled counts enqueued records skipped because an earlier pass
	// found their materialization and nothing can change them again.
	Settled int
	Next    []byte
}

// ApprovalTolerantScanner is the optional scan the sweep prefers: a page of
// approvals that skips, and counts, any stored record that does not decode,
// instead of failing the page. Pending keeps the strict ScanApprovals, which
// fails closed. The durable store implements both.
type ApprovalTolerantScanner interface {
	ScanApprovalsTolerant(context.Context, []byte, int) (ApprovalPage, error)
}

// maxSettledApprovals bounds the sweep's memory of enqueued approvals it has
// found materialized. Past it the memory is cleared and those records are
// checked once more, which is correct and only costs a read each.
const maxSettledApprovals = 1 << 16

// approvalSweepState is what the sweep remembers across pages and passes.
//
// settled holds enqueued approvals whose action has been read and is their
// materialization. Nothing can change such a record again: enqueued may only
// become stranded, and only when its identity is taken or its action never
// written, and an action is never deleted while isMaterializationOf compares
// only fields an action keeps for life. So once seen, it need never be read
// again. The memory is per process and bounded; losing it costs a read.
type approvalSweepState struct {
	mu          sync.Mutex
	settled     map[string]struct{}
	undecodable atomic.Uint64
	failed      atomic.Uint64
	pageFailed  atomic.Uint64
}

func (s *approvalSweepState) isSettled(id []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.settled[string(id)]
	return ok
}

func (s *approvalSweepState) settle(id []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled == nil || len(s.settled) >= maxSettledApprovals {
		s.settled = make(map[string]struct{})
	}
	s.settled[string(id)] = struct{}{}
}

// ApprovalSweepCounters reports the sweep's error counts since the process
// started: records skipped as undecodable, records it could not judge or
// write, and pages whose scan failed. They count this process's own
// failures, never tenant workload.
func (s *ApprovalService) ApprovalSweepCounters() (undecodable, failed, pageFailed uint64) {
	return s.sweep.undecodable.Load(), s.sweep.failed.Load(), s.sweep.pageFailed.Load()
}

// Sweep examines one page of approvals and records, as the system observer,
// every one that can no longer progress. It fails only for a request it
// cannot serve at all — no sweeper configured, a bad page bound, a scan that
// failed, or a cancelled context. A record it cannot judge or write is
// counted and left for the next pass.
func (s *ApprovalService) Sweep(
	ctx context.Context, request ApprovalSweepRequest,
) (ApprovalSweepResult, error) {
	if s.sweeper == nil {
		return ApprovalSweepResult{}, shoal.NewError(
			shoal.ErrorUnavailable, "the approval sweep is not configured")
	}
	// Every outbox row the sweep writes names it as observer, which a build
	// before #655 cannot read, so it is a terminal-states writer like the
	// rest (ApprovalConfig.TerminalStates).
	if !s.terminal {
		return ApprovalSweepResult{}, terminalStatesDisabled()
	}
	if request.Limit <= 0 || request.Limit > MaxApprovalListResults {
		return ApprovalSweepResult{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval sweep limit is outside its bound")
	}
	// A store wrapper that does not forward the tolerant scan falls back to
	// the strict one, where an undecodable record fails its page; the host's
	// pass then steps past a page that keeps failing (approvalSweeper).
	var page ApprovalPage
	var err error
	if tolerant, ok := s.store.(ApprovalTolerantScanner); ok {
		page, err = tolerant.ScanApprovalsTolerant(ctx, request.After, request.Limit)
	} else {
		page, err = s.store.ScanApprovals(ctx, request.After, request.Limit)
	}
	if err != nil {
		if ctx.Err() == nil {
			s.sweep.pageFailed.Add(1)
		}
		return ApprovalSweepResult{}, err
	}
	var lastID []byte
	result := ApprovalSweepResult{
		Next: append([]byte(nil), page.Next...), Undecodable: page.Undecodable,
	}
	s.sweep.undecodable.Add(uint64(page.Undecodable))
	defer func() { s.sweep.failed.Add(uint64(result.Failed)) }()
	for _, record := range page.Approvals {
		if err := ctx.Err(); err != nil {
			// Stopped mid-page: Next is the last record finished, so a
			// caller resumes after it rather than redoing the page, and a
			// page that never fits one budget still advances.
			if result.Examined > 0 {
				result.Next = append([]byte(nil), lastID...)
			}
			return result, err
		}
		result.Examined++
		s.sweepOne(ctx, record, &result)
		// Done, even if the budget ran out inside it: whatever it left
		// unwritten is still unwritten next cycle, and every write is a
		// compare-and-set, so the next cycle judges it again.
		lastID = record.ID
	}
	return result, nil
}

func (s *ApprovalService) sweepOne(
	ctx context.Context, record ApprovalRecord, result *ApprovalSweepResult,
) {
	now := s.dispatch.clock().UTC()
	if record.State.terminal() {
		// A transition this observer wrote whose event did not publish is
		// retried here, inside #653's window. Only rows the observer
		// published are touched; the drain skips every other row, and closes
		// a lapsed one, as any drain does.
		if s.events != nil &&
			(record.State == ApprovalExpired || record.State == ApprovalStranded) &&
			now.Before(record.UpdatedAt.Add(ApprovalEventRetryWindow)) {
			bound, _, err := s.sweepDecision(ctx, record, now)
			if err != nil {
				result.Failed++
				return
			}
			s.drainApprovalEventsQuietly(bound, record.ID)
		}
		return
	}
	if record.State == ApprovalEnqueued {
		if s.sweep.isSettled(record.ID) {
			result.Settled++
			return
		}
		action, err := s.dispatch.store.GetAction(ctx, record.ID)
		if err == nil && isMaterializationOf(action, record) {
			s.sweep.settle(record.ID)
			return
		}
	}
	state, cause, err := s.approvalStrandable(ctx, record, now)
	if err != nil {
		result.Failed++
		return
	}
	if state == "" {
		return
	}
	bound, decision, err := s.sweepDecision(ctx, record, now)
	if err != nil {
		result.Failed++
		return
	}
	_, err = s.settle(
		bound, auth.OperationApprovalSweep, decision, record, state, cause, now)
	switch {
	case err == nil, errors.Is(err, ErrApprovalEventUnpublished):
		if err != nil {
			result.Unpublished++
		}
		if state == ApprovalExpired {
			result.Expired++
		} else {
			result.Stranded++
		}
	case errors.Is(err, ErrApprovalConflict):
		result.Raced++
	default:
		result.Failed++
	}
}

// sweepDecision binds the system observer for one record and checks that what
// the host bound is exactly that observer: its identity and client, on
// nobody's behalf, holding the sweep and nothing else, on exactly this
// record's source and policy, expiring within
// MaxApprovalSweepDecisionLifetime. A host that bound anything broader is
// refused rather than trusted.
func (s *ApprovalService) sweepDecision(
	ctx context.Context, record ApprovalRecord, now time.Time,
) (context.Context, auth.Decision, error) {
	bound, err := s.sweeper(ctx, ApprovalSweepScope{
		SourceID: append([]byte(nil), record.Request.SourceID...),
		PolicyID: append([]byte(nil), record.Request.PolicyID...),
	})
	if err != nil {
		return nil, auth.Decision{}, err
	}
	decision, err := s.dispatch.resolver.Resolve(bound)
	if err != nil {
		return nil, auth.Decision{}, err
	}
	operations := decision.AllowedOperations()
	sources, policies := decision.PermittedSourceIDs(), decision.PermittedPolicyIDs()
	if decision.Subject() != ApprovalSweeperSubject ||
		decision.Actor() != ApprovalSweeperSubject ||
		decision.ClientID() != ApprovalSweeperClientID ||
		len(sources) != 1 || !bytes.Equal(sources[0], record.Request.SourceID) ||
		len(policies) != 1 || !bytes.Equal(policies[0], record.Request.PolicyID) ||
		decision.AuthenticationExpires().After(now.Add(MaxApprovalSweepDecisionLifetime)) ||
		len(decision.OnBehalfOf()) > 0 ||
		len(operations) != 1 || operations[0] != auth.OperationApprovalSweep {
		return nil, auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"the approval sweep is not bound to the system observer")
	}
	if err := decision.AuthorizeObject(
		auth.OperationApprovalSweep, auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            record.Request.SourceID,
			PolicyID:            record.Request.PolicyID,
			ObjectID:            shoal.ID(record.ID),
		}, now); err != nil {
		return nil, auth.Decision{}, err
	}
	return bound, decision, nil
}
