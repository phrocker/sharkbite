// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Unit tests for #655's lifecycle rules: the transition table, the shapes
// Validate admits for withdrawn and stranded records, the evidence
// approvalStrandable requires, and the sweep's refusal of any decision but
// the system observer's. The behaviour end to end is tested against the real
// composition in cmd/shoal-explore-web.

func TestApprovalTransitionTableIsTheLifecycle(t *testing.T) {
	states := []ApprovalState{
		ApprovalPending, ApprovalApproved, ApprovalRefused, ApprovalExpired,
		ApprovalEnqueued, ApprovalWithdrawn, ApprovalStranded,
	}
	allowed := map[ApprovalState][]ApprovalState{
		ApprovalPending: {ApprovalApproved, ApprovalRefused, ApprovalExpired,
			ApprovalWithdrawn, ApprovalStranded},
		ApprovalApproved: {ApprovalEnqueued, ApprovalExpired, ApprovalStranded},
		ApprovalEnqueued: {ApprovalStranded},
	}
	for _, from := range states {
		for _, to := range states {
			want := false
			for _, candidate := range allowed[from] {
				want = want || candidate == to
			}
			if got := ApprovalTransitionAllowed(from, to); got != want {
				t.Errorf("%s -> %s allowed = %v, want %v", from, to, got, want)
			}
		}
		if from.terminal() != (len(allowed[from]) == 0) {
			t.Errorf("%s terminal = %v with %d ways out",
				from, from.terminal(), len(allowed[from]))
		}
	}
}

func withdrawnRecord(t *testing.T) ApprovalRecord {
	t.Helper()
	record := validApprovalRecord(t)
	record.Version, record.State = 2, ApprovalWithdrawn
	record.WithdrawnAt = record.RequestedAt.Add(time.Minute)
	record.UpdatedAt = record.WithdrawnAt
	reason, err := interaction.NewReason("changed", "detail")
	if err != nil {
		t.Fatal(err)
	}
	record.WithdrawalReason = reason
	return record
}

func strandedRecord(
	record ApprovalRecord, cause ApprovalCondition, at time.Time,
) ApprovalRecord {
	record = CloneApprovalRecord(record)
	record.Version++
	record.State = ApprovalStranded
	record.StrandedCause = cause
	record.UpdatedAt = at
	return record
}

func TestApprovalRecordValidationOfWithdrawalAndStranding(t *testing.T) {
	pending := validApprovalRecord(t)
	approved := decided(pending, ApprovalApproved, ApprovalVerdictApprove)
	enqueued := approved
	enqueued.Version++
	enqueued.State = ApprovalEnqueued
	enqueued.MaterializedAt = approved.DecidedAt.Add(time.Minute)
	enqueued.UpdatedAt = enqueued.MaterializedAt
	inWindow := pending.RequestedAt.Add(2 * time.Minute)
	pastGrace := pending.Request.Deadline.Add(ApprovalStrandGrace)
	for _, probe := range []struct {
		name   string
		record ApprovalRecord
		ok     bool
	}{
		{"withdrawn", withdrawnRecord(t), true},
		{"withdrawn with a verdict", func() ApprovalRecord {
			r := withdrawnRecord(t)
			d := decided(r, ApprovalWithdrawn, ApprovalVerdictApprove)
			d.Version, d.UpdatedAt = r.Version, r.UpdatedAt
			return d
		}(), false},
		{"withdrawn without a reason", func() ApprovalRecord {
			r := withdrawnRecord(t)
			r.WithdrawalReason = interaction.Reason{}
			return r
		}(), false},
		{"withdrawn after the window", func() ApprovalRecord {
			r := withdrawnRecord(t)
			r.WithdrawnAt = r.ExpiresAt
			r.UpdatedAt = r.WithdrawnAt
			return r
		}(), false},
		{"withdrawn at another time than its update", func() ApprovalRecord {
			r := withdrawnRecord(t)
			r.UpdatedAt = r.WithdrawnAt.Add(time.Second)
			return r
		}(), false},
		{"a withdrawal on a pending record", func() ApprovalRecord {
			r := CloneApprovalRecord(pending)
			r.WithdrawnAt = r.RequestedAt
			return r
		}(), false},
		{"a withdrawal reason on a pending record", func() ApprovalRecord {
			r := CloneApprovalRecord(pending)
			r.WithdrawalReason = interaction.Reason{Code: "x"}
			return r
		}(), false},
		{"stranded pending, target moved", strandedRecord(
			pending, ApprovalConditionTargetMoved, inWindow), true},
		{"stranded approved, policy moved", strandedRecord(
			approved, ApprovalConditionPolicyMoved, approved.DecidedAt), true},
		{"stranded on a moved target after the window", strandedRecord(
			pending, ApprovalConditionTargetMoved, pending.ExpiresAt), false},
		{"stranded enqueued, identity taken", strandedRecord(
			enqueued, ApprovalConditionIdentityTaken, enqueued.UpdatedAt), true},
		{"stranded enqueued, deadline passed", strandedRecord(
			enqueued, ApprovalConditionDeadlinePassed, pastGrace), true},
		{"stranded on the deadline inside the grace", strandedRecord(
			enqueued, ApprovalConditionDeadlinePassed,
			pastGrace.Add(-time.Nanosecond)), false},
		{"stranded enqueued on a moved target", strandedRecord(
			enqueued, ApprovalConditionTargetMoved, enqueued.UpdatedAt), false},
		{"stranded pending on a taken identity", strandedRecord(
			pending, ApprovalConditionIdentityTaken, inWindow), false},
		{"stranded after materializing, never decided", func() ApprovalRecord {
			r := strandedRecord(pending, ApprovalConditionIdentityTaken, inWindow)
			r.MaterializedAt = pending.RequestedAt.Add(time.Minute)
			return r
		}(), false},
		{"stranded on a rollout-sensitive cause", strandedRecord(
			approved, ApprovalConditionApproverMappingMoved, approved.DecidedAt), false},
		{"stranded on a closed window", strandedRecord(
			pending, ApprovalConditionWindowClosed, inWindow), false},
		{"stranded with no cause", strandedRecord(pending, "", inWindow), false},
		{"refused, then stranded", strandedRecord(
			decided(pending, ApprovalRefused, ApprovalVerdictRefuse),
			ApprovalConditionTargetMoved, approved.DecidedAt), false},
		{"a cause on a pending record", func() ApprovalRecord {
			r := CloneApprovalRecord(pending)
			r.StrandedCause = ApprovalConditionTargetMoved
			return r
		}(), false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if err := probe.record.Validate(); (err == nil) != probe.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, probe.ok)
			}
		})
	}
}

func sweeperPublisher(record ApprovalRecord) ApprovalPublisher {
	return ApprovalPublisher{
		Operation: auth.OperationApprovalSweep, Subject: ApprovalSweeperSubject,
		Actor: ApprovalSweeperSubject, ClientID: ApprovalSweeperClientID,
		RequestID: "sweep-request", CorrelationID: "sweep-request",
		AuthorizationFingerprint: auth.Fingerprint{4},
		AuthorizationExpiresAt:   record.RequestedAt.Add(time.Hour),
		Observer:                 ApprovalObservedBySweeper,
	}
}

// TestApprovalTransitionBindsTheNewKindsToTheirMakers: a withdrawal is the
// requester's alone; an expiry or a stranding is its observer's, and the
// sweeper observer is the system identity under the sweep operation and
// nothing else.
func TestApprovalTransitionBindsTheNewKindsToTheirMakers(t *testing.T) {
	pending := validApprovalRecord(t)
	withdrawn := withdrawnRecord(t)
	stranded := strandedRecord(
		pending, ApprovalConditionTargetMoved, pending.RequestedAt.Add(time.Minute))
	expired := pending
	expired.Version, expired.State = 2, ApprovalExpired
	expired.UpdatedAt = pending.ExpiresAt.Add(time.Minute)
	observed := func(p ApprovalPublisher, o ApprovalObserver) ApprovalPublisher {
		p.Observer = o
		return p
	}
	anApprover := ApprovalPublisher{
		Operation: auth.OperationActionApprove, Subject: "carol", Actor: "carol",
		RequestID: "carol-request", CorrelationID: "correlation",
		AuthorizationFingerprint: auth.Fingerprint{5},
		AuthorizationExpiresAt:   pending.RequestedAt.Add(time.Hour),
	}
	for _, probe := range []struct {
		name      string
		record    ApprovalRecord
		publisher ApprovalPublisher
		ok        bool
	}{
		{"withdrawn by the requester", withdrawn, requesterPublisher(withdrawn), true},
		{"withdrawn by an approver", withdrawn, anApprover, false},
		{"withdrawn by the sweeper", withdrawn, sweeperPublisher(withdrawn), false},
		{"withdrawn with an observer", withdrawn,
			observed(requesterPublisher(withdrawn), ApprovalObservedByRequester), false},
		{"stranding observed by the requester", stranded,
			observed(requesterPublisher(stranded), ApprovalObservedByRequester), true},
		{"stranding observed by an approver", stranded,
			observed(anApprover, ApprovalObservedByApprover), true},
		{"stranding observed by the sweeper", stranded, sweeperPublisher(stranded), true},
		{"expiry observed by the sweeper", expired, sweeperPublisher(expired), true},
		{"the sweeper under dispatch", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.Operation = auth.OperationDispatch
			return p
		}(), false},
		{"another principal under the sweep operation", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.Subject, p.Actor = "mallory", "mallory"
			return p
		}(), false},
		{"another subject behind the observer's actor", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.Subject = "mallory"
			return p
		}(), false},
		{"another actor behind the observer's subject", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.Actor = "mallory"
			return p
		}(), false},
		{"the sweeper under another client", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.ClientID = "host"
			return p
		}(), false},
		{"the sweeper on someone's behalf", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.OnBehalfOf = []shoal.ID{"dave"}
			return p
		}(), false},
		{"the sweeper posing as an approver", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.Operation, p.Observer = auth.OperationActionApprove, ApprovalObservedByApprover
			return p
		}(), false},
		{"the sweeper observing as the requester", stranded, func() ApprovalPublisher {
			p := sweeperPublisher(stranded)
			p.Observer = ApprovalObservedByRequester
			return p
		}(), false},
		{"stranding with no observer", stranded, requesterPublisher(stranded), false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, err := NewApprovalTransition(probe.record, probe.publisher)
			if (err == nil) != probe.ok {
				t.Fatalf("NewApprovalTransition = %v, want ok=%v", err, probe.ok)
			}
		})
	}
}

// failingRegistryStore answers every read with one error.
type failingRegistryStore struct {
	*memoryStore
	err error
}

func (s failingRegistryStore) Get(context.Context, shoal.ID) (Stored, error) {
	return Stored{}, s.err
}

type failingActionStore struct {
	*memoryDispatchStore
	err error
}

func (s failingActionStore) GetAction(context.Context, []byte) (ActionRecord, error) {
	return ActionRecord{}, s.err
}

// TestApprovalStrandableNeedsConclusiveEvidence: a final state is written
// only on a durable fact. Every weaker answer — a failed or absent registry
// read, a read older than the request, a lapsed lease, a policy generation
// behind the request's, an action deadline inside the grace — leaves the
// request alone.
func TestApprovalStrandableNeedsConclusiveEvidence(t *testing.T) {
	pending := validApprovalRecord(t)
	approved := decided(pending, ApprovalApproved, ApprovalVerdictApprove)
	enqueued := approved
	enqueued.Version++
	enqueued.State = ApprovalEnqueued
	enqueued.MaterializedAt = approved.DecidedAt.Add(time.Minute)
	enqueued.UpdatedAt = enqueued.MaterializedAt
	inWindow := pending.RequestedAt.Add(2 * time.Minute)

	descriptor := func(generation int64, edit func(*Descriptor)) Store {
		memory := newMemoryStore()
		d := approvalBranchDescriptor(pending.RequestedAt)
		d.LeaseExpiresAt = pending.Request.Deadline.Add(time.Hour)
		d.Generation = generation
		if edit != nil {
			edit(&d)
		}
		memory.records["agent"] = Stored{Descriptor: d}
		return memory
	}
	service := func(store Store, inForce int64, actions DispatchStore) *ApprovalService {
		if actions == nil {
			actions = newMemoryDispatchStore()
		}
		return &ApprovalService{
			dispatch: &DispatchService{
				store: actions, registry: &Service{store: store},
			},
			generations: fixedGenerations{generation: inForce},
		}
	}
	other := approvalTestRecord(t)
	other.Action = "other"
	squatted := newMemoryDispatchStore()
	squatted.records[string(other.ID)] = other
	type answer struct {
		state ApprovalState
		cause ApprovalCondition
		err   bool
	}
	for _, probe := range []struct {
		name    string
		service *ApprovalService
		record  ApprovalRecord
		now     time.Time
		want    answer
	}{
		{"live", service(descriptor(1, nil), 3, nil), pending, inWindow, answer{}},
		{"window closed", service(descriptor(1, nil), 3, nil), pending,
			pending.ExpiresAt, answer{state: ApprovalExpired}},
		{"target moved", service(descriptor(2, nil), 3, nil), pending, inWindow,
			answer{ApprovalStranded, ApprovalConditionTargetMoved, false}},
		{"approved, target moved", service(descriptor(2, nil), 3, nil), approved,
			inWindow, answer{ApprovalStranded, ApprovalConditionTargetMoved, false}},
		{"target revoked", service(descriptor(1, func(d *Descriptor) {
			d.RevokedAt = pending.RequestedAt
		}), 3, nil), pending, inWindow,
			answer{ApprovalStranded, ApprovalConditionTargetMoved, false}},
		{"lease lapsed is not evidence", service(descriptor(1, func(d *Descriptor) {
			d.LeaseExpiresAt = pending.RequestedAt
		}), 3, nil), pending, inWindow, answer{}},
		{"a read older than the request is not evidence", service(
			descriptor(1, nil), 3, nil), func() ApprovalRecord {
			r := CloneApprovalRecord(pending)
			r.Request.AgentGeneration = 2
			r.RequestDigest = ApprovalRequestDigest(r.Request)
			return r
		}(), inWindow, answer{}},
		{"an absent agent is not evidence", service(newMemoryStore(), 3, nil),
			pending, inWindow, answer{}},
		{"a failed read is an error, not evidence", service(failingRegistryStore{
			memoryStore: newMemoryStore(),
			err:         shoal.NewError(shoal.ErrorUnavailable, "down"),
		}, 3, nil), pending, inWindow, answer{err: true}},
		{"policy moved forward", service(descriptor(1, nil), 4, nil), pending,
			inWindow, answer{ApprovalStranded, ApprovalConditionPolicyMoved, false}},
		{"policy behind the request is not evidence", service(
			descriptor(1, nil), 2, nil), pending, inWindow, answer{}},
		{"enqueued inside the grace", service(descriptor(2, nil), 4, nil), enqueued,
			pending.Request.Deadline.Add(ApprovalStrandGrace - time.Nanosecond), answer{}},
		{"enqueued past the grace", service(descriptor(1, nil), 3, nil), enqueued,
			pending.Request.Deadline.Add(ApprovalStrandGrace),
			answer{ApprovalStranded, ApprovalConditionDeadlinePassed, false}},
		{"enqueued, identity taken", service(descriptor(1, nil), 3, squatted),
			enqueued, enqueued.UpdatedAt,
			answer{ApprovalStranded, ApprovalConditionIdentityTaken, false}},
		{"enqueued, unreadable action", service(descriptor(1, nil), 3,
			failingActionStore{
				memoryDispatchStore: newMemoryDispatchStore(),
				err:                 shoal.NewError(shoal.ErrorNotFound, "lagging"),
			}), enqueued, pending.Request.Deadline.Add(2 * ApprovalStrandGrace),
			answer{err: true}},
		{"final", service(descriptor(2, nil), 4, nil), withdrawnRecord(t),
			inWindow, answer{}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			state, cause, err := probe.service.approvalStrandable(
				context.Background(), probe.record, probe.now)
			if (err != nil) != probe.want.err || state != probe.want.state ||
				cause != probe.want.cause {
				t.Fatalf("approvalStrandable = %q/%q, %v; want %+v",
					state, cause, err, probe.want)
			}
		})
	}
}

// scanningApprovalStore is memoryApprovalStore with a scan.
type scanningApprovalStore struct {
	*memoryApprovalStore
	writes int
	mu     sync.Mutex
}

func (s *scanningApprovalStore) ApplyApproval(
	ctx context.Context, mutation ApprovalMutation,
) (ApprovalRecord, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return s.memoryApprovalStore.ApplyApproval(ctx, mutation)
}

func (s *scanningApprovalStore) ScanApprovals(
	_ context.Context, _ []byte, limit int,
) (ApprovalPage, error) {
	s.memoryApprovalStore.mu.Lock()
	defer s.memoryApprovalStore.mu.Unlock()
	keys := make([]string, 0, len(s.records))
	for key := range s.records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	page := ApprovalPage{}
	for _, key := range keys {
		if len(page.Approvals) == limit {
			break
		}
		page.Approvals = append(page.Approvals, CloneApprovalRecord(s.records[key]))
	}
	return page, nil
}

// TestApprovalSweepActsOnlyAsTheSystemObserver: the sweep acts under
// whatever the host's binder binds only if it is exactly the system observer
// on the record's scope. A binder that bound a party, the observer with any
// other operation, on someone's behalf, or for another scope writes nothing.
func TestApprovalSweepActsOnlyAsTheSystemObserver(t *testing.T) {
	lapsed := validApprovalRecord(t)
	now := lapsed.ExpiresAt.Add(time.Minute)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mint := func(edit func(*auth.DecisionConfig)) ApprovalSweepBinder {
		return func(ctx context.Context, scope ApprovalSweepScope) (context.Context, error) {
			config := auth.DecisionConfig{
				Subject: ApprovalSweeperSubject, Actor: ApprovalSweeperSubject,
				ClientID:              ApprovalSweeperClientID,
				AuthorizationDomain:   []byte("domain"),
				AllowedOperations:     []auth.Operation{auth.OperationApprovalSweep},
				PermittedSourceIDs:    [][]byte{scope.SourceID},
				PermittedPolicyIDs:    [][]byte{scope.PolicyID},
				PolicyGeneration:      3,
				AuthenticationExpires: now.Add(time.Minute),
				RequestID:             "sweep-request", CorrelationID: "sweep-request",
			}
			if edit != nil {
				edit(&config)
			}
			decision, err := auth.NewDecision(config)
			if err != nil {
				return nil, err
			}
			return authority.Binder().Bind(ctx, decision)
		}
	}
	for _, probe := range []struct {
		name   string
		binder ApprovalSweepBinder
		writes bool
	}{
		{"the system observer", mint(nil), true},
		{"a party", mint(func(c *auth.DecisionConfig) {
			c.Subject, c.Actor = "alice", "alice-agent"
		}), false},
		{"another subject behind the observer's actor", mint(func(c *auth.DecisionConfig) {
			c.Subject = "alice"
		}), false},
		{"another actor behind the observer's subject", mint(func(c *auth.DecisionConfig) {
			c.Actor = "alice-agent"
		}), false},
		{"the observer holding dispatch too", mint(func(c *auth.DecisionConfig) {
			c.AllowedOperations = append(c.AllowedOperations, auth.OperationDispatch)
		}), false},
		{"the observer under another operation", mint(func(c *auth.DecisionConfig) {
			c.AllowedOperations = []auth.Operation{auth.OperationActionApprove}
		}), false},
		{"the observer on someone's behalf", mint(func(c *auth.DecisionConfig) {
			c.OnBehalfOf = []shoal.ID{"dave"}
		}), false},
		{"the observer for another scope", mint(func(c *auth.DecisionConfig) {
			c.PermittedPolicyIDs = [][]byte{[]byte("other")}
		}), false},
		{"the observer for a wider scope", mint(func(c *auth.DecisionConfig) {
			c.PermittedPolicyIDs = append(c.PermittedPolicyIDs, []byte("other"))
		}), false},
		{"the observer for more sources", mint(func(c *auth.DecisionConfig) {
			c.PermittedSourceIDs = append(c.PermittedSourceIDs, []byte("other"))
		}), false},
		{"the observer under another client", mint(func(c *auth.DecisionConfig) {
			c.ClientID = "host"
		}), false},
		{"a long-lived observer decision", mint(func(c *auth.DecisionConfig) {
			c.AuthenticationExpires = now.Add(24 * time.Hour)
		}), false},
		{"a binder that fails", func(context.Context, ApprovalSweepScope) (context.Context, error) {
			return nil, errors.New("no")
		}, false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			store := &scanningApprovalStore{memoryApprovalStore: &memoryApprovalStore{
				records: map[string]ApprovalRecord{
					string(lapsed.ID): CloneApprovalRecord(lapsed),
				},
			}}
			service := &ApprovalService{
				dispatch: &DispatchService{
					resolver: authority.Resolver(),
					clock:    func() time.Time { return now },
				},
				store: store, recorder: unguardedApprovalRecorder{},
				narrowed:    func(context.Context) bool { return false },
				generations: fixedGenerations{generation: 3},
				window:      DefaultApprovalWindow, sweeper: probe.binder,
				terminal: true,
			}
			result, err := service.Sweep(context.Background(), ApprovalSweepRequest{
				Limit: MaxApprovalListResults,
			})
			if err != nil {
				t.Fatal(err)
			}
			stored, _ := store.GetApproval(context.Background(), lapsed.ID)
			if probe.writes {
				if result.Expired != 1 || stored.State != ApprovalExpired ||
					store.writes != 1 {
					t.Fatalf("sweep = %+v, stored %q", result, stored.State)
				}
				return
			}
			if result.Failed != 1 || stored.State != ApprovalPending || store.writes != 0 {
				t.Fatalf("sweep under %s = %+v, stored %q, %d writes",
					probe.name, result, stored.State, store.writes)
			}
		})
	}
	unconfigured := &ApprovalService{dispatch: &DispatchService{}}
	if _, err := unconfigured.Sweep(context.Background(), ApprovalSweepRequest{
		Limit: 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("a sweep with no observer configured = %v", err)
	}
}
