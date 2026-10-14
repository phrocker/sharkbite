// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionadjudication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	bases "github.com/phrocker/shoal-oss/internal/decisionbasisstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Test authority stands in for an explicitly trusted cross-principal registry.
// It is deliberately not exported or usable as a production authority.
type serviceAuthority struct {
	binding     Binding
	config      decision.AdjudicationBasisConfig
	accessible  atomic.Bool
	captures    atomic.Int32
	roles       map[shoal.ID]bool
	captureFail bool
	verifyHook  func([]Material, *Candidate)
}

func (a *serviceAuthority) Resolve(_ context.Context, _ auth.Decision, request, prediction shoal.ID, _ auth.Operation) (Binding, error) {
	if !a.accessible.Load() || request != a.binding.Prediction.Request().ID() || prediction != a.binding.Prediction.ID() {
		return Binding{}, hidden()
	}
	return a.binding, nil
}
func (a *serviceAuthority) AuthorizeTarget(_ context.Context, _ auth.Decision, _ shoal.ID, _ auth.Operation) error {
	if !a.accessible.Load() {
		return hidden()
	}
	return nil
}
func (a *serviceAuthority) Capture(_ context.Context, _ auth.Decision, p decision.AdjudicationProposal, _ []Material) (decision.AdjudicationBasis, error) {
	a.captures.Add(1)
	if a.captureFail {
		return decision.AdjudicationBasis{}, errors.New("collector unavailable")
	}
	return decision.NewAdjudicationBasis(p, a.config)
}
func (a *serviceAuthority) Verify(_ context.Context, _ auth.Decision, _ auth.Operation, _ shoal.ID, m []Material, c *Candidate) error {
	if a.verifyHook != nil {
		a.verifyHook(m, c)
	}
	if !a.accessible.Load() {
		return hidden()
	}
	if c != nil {
		for _, r := range c.RequiredRoles {
			if !a.roles[r] {
				return hidden()
			}
		}
		// A valid structural witness reference does not attest the proposed label.
		if c.Proposal.Config().Disposition == decision.AdjudicationVerified && c.Proposal.Config().Label != "inspect" {
			return hidden()
		}
		if c.Basis.Config().AuthorityRevisionID != "revision" {
			return hidden()
		}
	}
	return nil
}

type serviceCAS struct {
	mu            sync.Mutex
	cells         map[string]allocator.Cell
	reads, writes atomic.Int32
	afterRead     func(int32)
	afterWrite    func()
	failReads     atomic.Bool
	unknown       bool
}

func (b *serviceCAS) ReadExact(_ context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	n := b.reads.Add(1)
	if b.afterRead != nil {
		b.afterRead(n)
	}
	if b.failReads.Load() {
		return nil, errors.New("read failed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []allocator.Cell
	for _, c := range coords {
		if cell, ok := b.cells[string(c.Row)]; ok {
			cell.Value = append([]byte(nil), cell.Value...)
			out = append(out, cell)
		}
	}
	return out, nil
}
func (b *serviceCAS) CompareAndMutate(_ context.Context, m allocator.Mutation) (allocator.Status, error) {
	b.mu.Lock()
	if b.cells == nil {
		b.cells = map[string]allocator.Cell{}
	}
	for _, c := range m.Conditions {
		old, exists := b.cells[string(c.Coordinate.Row)]
		if (c.Absent && exists) || (!c.Absent && (!exists || string(old.Value) != string(c.Value) || (c.TimestampSet && c.Timestamp != old.Timestamp))) {
			b.mu.Unlock()
			return allocator.StatusRejected, nil
		}
	}
	for _, u := range m.Updates {
		b.cells[string(u.Coordinate.Row)] = allocator.Cell{Coordinate: u.Coordinate, Timestamp: u.Timestamp, Value: append([]byte(nil), u.Value...)}
	}
	b.writes.Add(1)
	b.mu.Unlock()
	if b.afterWrite != nil {
		b.afterWrite()
	}
	if b.unknown {
		return allocator.StatusUnknown, errors.New("ack lost")
	}
	return allocator.StatusAccepted, nil
}

type serviceFixture struct {
	service        *Service
	authority      *serviceAuthority
	journal, bases *serviceCAS
	ctx            context.Context
	cfg            decision.AdjudicationProposalConfig
	clock          func() time.Time
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	policy, pred, pc, bc, judge := admissionFixture(t, 2)
	now := bc.CapturedAt
	clock := func() time.Time { return now }
	a := &serviceAuthority{binding: Binding{policy, pred}, config: bc, roles: map[shoal.ID]bool{policy.Config().AdjudicatorRoleID: true, policy.Config().DisputeResolverRoleID: true}}
	a.accessible.Store(true)
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(judge.SubjectID), Actor: shoal.ID(judge.ActorID), ClientID: shoal.ID(judge.ClientID), AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead, auth.OperationIngest}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "request", CorrelationID: "correlation", AuditPurpose: "adjudicate"})
	if e != nil {
		t.Fatal(e)
	}
	authn, e := auth.NewAuthorityWithClock(clock)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := authn.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	j, b := &serviceCAS{}, &serviceCAS{}
	js, e := journal.New(journal.Config{Backend: j, Clock: clock})
	if e != nil {
		t.Fatal(e)
	}
	bs, e := bases.New(bases.Config{Backend: b})
	if e != nil {
		t.Fatal(e)
	}
	svc, e := New(Config{Resolver: authn.Resolver(), Authority: a, Journal: js, Bases: bs, Clock: clock})
	if e != nil {
		t.Fatal(e)
	}
	return &serviceFixture{svc, a, j, b, ctx, pc, clock}
}
func TestServiceRetryPreservesBasisAndHistory(t *testing.T) {
	f := newServiceFixture(t)
	first, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	f.authority.captureFail = true
	again, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg)
	if e != nil || !reflect.DeepEqual(first, again) || f.authority.captures.Load() != 1 {
		t.Fatalf("retry recaptured: %v", e)
	}
	f.authority.captureFail = false
	second := f.cfg
	second.ExpectedHeadID = first.ID
	second.ExpectedVersion = 1
	second.Disposition = decision.AdjudicationDisputed
	second.Label = ""
	second.Reason = "conflicting evidence"
	r2, e := f.service.Adjudicate(f.ctx, []byte("two"), second)
	if e != nil {
		t.Fatal(e)
	}
	again, e = f.service.Adjudicate(f.ctx, []byte("one"), f.cfg)
	if e != nil || again.ID != first.ID || again.BasisID != first.BasisID {
		t.Fatalf("historical retry: %v", e)
	}
	h, e := f.service.History(f.ctx, first.TargetID)
	if e != nil || len(h) != 2 || h[1].ID != r2.ID {
		t.Fatalf("history: %v", e)
	}
	conflict := f.cfg
	conflict.Label = "ordinary"
	if _, e = f.service.Adjudicate(f.ctx, []byte("one"), conflict); !errors.Is(e, ErrConflict) {
		t.Fatalf("key conflict: %v", e)
	}
}
func TestServiceRequiresRolesAndWitnessSupport(t *testing.T) {
	for _, name := range []string{"adjudicator role", "witness label", "authority revision", "reporter conflict"} {
		t.Run(name, func(t *testing.T) {
			f := newServiceFixture(t)
			switch name {
			case "adjudicator role":
				f.authority.roles = map[shoal.ID]bool{}
			case "witness label":
				f.cfg.Label = "ordinary"
			case "authority revision":
				f.authority.config.AuthorityRevisionID = "forged"
			case "reporter conflict":
				f.authority.config.Outcomes[0].Reporter = admissionPerson("judge")
			}
			if _, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
				t.Fatalf("accepted: %v", e)
			}
			if f.journal.writes.Load() != 0 {
				t.Fatal("unauthorized append")
			}
		})
	}
	f := newServiceFixture(t)
	first, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	next := f.cfg
	next.ExpectedHeadID = first.ID
	next.ExpectedVersion = 1
	delete(f.authority.roles, f.authority.binding.Policy.Config().DisputeResolverRoleID)
	if _, e = f.service.Adjudicate(f.ctx, []byte("two"), next); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("missing resolver: %v", e)
	}
	if f.journal.writes.Load() != 1 {
		t.Fatal("missing role wrote")
	}
}
func TestServiceRechecksAfterJournalAndBasisIO(t *testing.T) {
	t.Run("journal pre-CAS read", func(t *testing.T) {
		f := newServiceFixture(t)
		f.journal.afterRead = func(n int32) {
			if n == 2 {
				f.authority.accessible.Store(false)
			}
		}
		if _, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
			t.Fatalf("revocation: %v", e)
		}
		if f.journal.writes.Load() != 0 {
			t.Fatal("revoked journal write")
		}
	})
	t.Run("basis history read", func(t *testing.T) {
		f := newServiceFixture(t)
		first, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg)
		if e != nil {
			t.Fatal(e)
		}
		f.bases.afterRead = func(int32) { f.authority.accessible.Store(false) }
		if h, e := f.service.History(f.ctx, first.TargetID); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || len(h) != 0 {
			t.Fatalf("history leaked: %v", e)
		}
	})
	t.Run("commit then source revocation", func(t *testing.T) {
		f := newServiceFixture(t)
		f.journal.afterWrite = func() { f.authority.accessible.Store(false) }
		if r, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg); !errors.Is(e, ErrIndeterminate) || r.ID != "" {
			t.Fatalf("commit became rollback: %v", e)
		}
		if f.journal.writes.Load() != 1 {
			t.Fatal("expected committed record")
		}
		f.authority.accessible.Store(true)
		f.journal.afterWrite = nil
		if _, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg); e != nil {
			t.Fatal(e)
		}
		if f.authority.captures.Load() != 1 || f.journal.writes.Load() != 1 {
			t.Fatal("reconciliation recomputed")
		}
	})
}
func TestServiceUncertainAcknowledgementAndConcurrentRetry(t *testing.T) {
	f := newServiceFixture(t)
	f.journal.unknown = true
	f.journal.afterWrite = func() { f.journal.failReads.Store(true) }
	if _, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg); !errors.Is(e, ErrIndeterminate) {
		t.Fatalf("lost ack: %v", e)
	}
	f.journal.afterWrite = nil
	f.journal.failReads.Store(false)
	if _, e := f.service.Adjudicate(f.ctx, []byte("one"), f.cfg); e != nil {
		t.Fatal(e)
	}
	if f.journal.writes.Load() != 1 || f.authority.captures.Load() != 1 {
		t.Fatal("duplicate replay")
	}
	g := newServiceFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := g.service.Adjudicate(g.ctx, []byte("same"), g.cfg); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if g.journal.writes.Load() != 1 {
		t.Fatal("multiple commits")
	}
}
func TestServiceMissingAuthorityAndUnboundCallerFailClosed(t *testing.T) {
	f := newServiceFixture(t)
	bad := f.service.config
	var typedNil *serviceAuthority
	bad.Authority = typedNil
	if _, e := New(bad); e == nil {
		t.Fatal("nil authority accepted")
	}
	if _, e := f.service.Adjudicate(context.Background(), []byte("one"), f.cfg); e == nil {
		t.Fatal("unbound caller accepted")
	}
	if f.journal.reads.Load() != 0 || f.bases.reads.Load() != 0 {
		t.Fatal("unbound caller reached storage")
	}
}

func TestServiceRFileRestartReplaysOriginalAdmission(t *testing.T) {
	f := newServiceFixture(t)
	dir := t.TempDir()
	var original journal.Receipt
	for run := 0; run < 2; run++ {
		eng, e := engine.Open(dir, engine.Options{})
		if e != nil {
			t.Fatal(e)
		}
		for _, table := range []string{journal.Table, bases.Table} {
			if run == 0 {
				if e = eng.CreateTable(table, engine.TableOptions{}); e != nil {
					t.Fatal(e)
				}
			}
		}
		jb, e := explorercoord.NewEngineStore(eng, journal.Table)
		if e != nil {
			t.Fatal(e)
		}
		bb, e := explorercoord.NewEngineStore(eng, bases.Table)
		if e != nil {
			t.Fatal(e)
		}
		js, e := journal.New(journal.Config{Backend: jb, Clock: f.clock})
		if e != nil {
			t.Fatal(e)
		}
		bs, e := bases.New(bases.Config{Backend: bb})
		if e != nil {
			t.Fatal(e)
		}
		cfg := f.service.config
		cfg.Journal = js
		cfg.Bases = bs
		svc, e := New(cfg)
		if e != nil {
			t.Fatal(e)
		}
		r, e := svc.Adjudicate(f.ctx, []byte("durable"), f.cfg)
		if e != nil {
			t.Fatal(e)
		}
		if run == 0 {
			original = r
		} else if !reflect.DeepEqual(original, r) {
			t.Fatal("restart changed admission")
		}
		h, e := svc.History(f.ctx, r.TargetID)
		if e != nil || len(h) != 1 {
			t.Fatalf("restart history: %v", e)
		}
		for _, table := range []string{journal.Table, bases.Table} {
			if e = eng.Flush(table); e != nil {
				t.Fatal(e)
			}
		}
		count := 0
		if e = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !entry.IsDir() && filepath.Ext(path) == ".rf" {
				count++
			}
			return nil
		}); e != nil || count < 2 {
			t.Fatalf("basis+journal RFiles: %d %v", count, e)
		}
		if e = eng.Close(); e != nil {
			t.Fatal(e)
		}
		f.authority.captureFail = true
	}
	if f.authority.captures.Load() != 1 {
		t.Fatal("restart recaptured")
	}
}

type differingCaptureAuthority struct {
	*serviceAuthority
	gate chan struct{}
	n    atomic.Int32
}

func (a *differingCaptureAuthority) Capture(_ context.Context, _ auth.Decision, p decision.AdjudicationProposal, _ []Material) (decision.AdjudicationBasis, error) {
	n := a.n.Add(1)
	if n == 2 {
		close(a.gate)
	}
	<-a.gate
	c := a.config
	c.CapturedAt = c.CapturedAt.Add(time.Duration(n) * time.Nanosecond)
	return decision.NewAdjudicationBasis(p, c)
}
func TestServiceConcurrentServerBasisVariation(t *testing.T) {
	f := newServiceFixture(t)
	later := func() time.Time { return f.clock().Add(time.Minute) }
	js, e := journal.New(journal.Config{Backend: f.journal, Clock: later})
	if e != nil {
		t.Fatal(e)
	}
	cfg := f.service.config
	cfg.Journal = js
	cfg.Clock = later
	cfg.Authority = &differingCaptureAuthority{serviceAuthority: f.authority, gate: make(chan struct{})}
	svc, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, e := svc.Adjudicate(f.ctx, []byte("same"), f.cfg); results <- e }()
	}
	e1, e2 := <-results, <-results
	if e1 != nil || e2 != nil {
		t.Fatalf("identical caller requests conflict solely on server capture variance: %v; %v", e1, e2)
	}
}

type boundedHistoryJournal struct {
	rows                   []journal.Receipt
	earlier                bool
	calls, appends, writes int
}

func (j *boundedHistoryJournal) History(context.Context, journal.Scope, shoal.ID) ([]journal.Receipt, error) {
	j.calls++
	if j.earlier && j.calls == 1 {
		return j.rows[:len(j.rows)-1], nil
	}
	return j.rows, nil
}
func (j *boundedHistoryJournal) AppendChecked(ctx context.Context, _ journal.Scope, _ []byte, _ decision.AdjudicationProposal, _ journal.Attribution, _ shoal.ID, guard func(context.Context, []journal.Receipt) error) (journal.Receipt, error) {
	j.appends++
	if e := guard(ctx, j.rows); e != nil {
		return journal.Receipt{}, e
	}
	j.writes++
	return journal.Receipt{}, errors.New("unexpected admitted write")
}

type boundedHistoryBases struct {
	values  map[shoal.ID]decision.AdjudicationBasis
	retains int
}

func (b *boundedHistoryBases) Retain(_ context.Context, _ bases.Scope, v decision.AdjudicationBasis) error {
	b.retains++
	b.values[v.ID()] = v
	return nil
}
func (b *boundedHistoryBases) Load(_ context.Context, _ bases.Scope, id shoal.ID, _ decision.AdjudicationProposal) (decision.AdjudicationBasis, error) {
	v, ok := b.values[id]
	if !ok {
		return v, bases.ErrNotFound
	}
	return v, nil
}
func TestServiceProspectiveHistoryBoundBeforeAndDuringAppend(t *testing.T) {
	for _, duringGuard := range []bool{false, true} {
		t.Run(fmt.Sprint(duringGuard), func(t *testing.T) {
			f := newServiceFixture(t)
			c := f.authority.config
			c.RoleEvidenceIDs = nil
			for i := 0; i < 1000; i++ {
				c.RoleEvidenceIDs = append(c.RoleEvidenceIDs, shoal.ID(fmt.Sprintf("role:%04d:%s", i, strings.Repeat("r", 490))))
			}
			f.authority.config = c
			raw, e := json.Marshal(c)
			if e != nil {
				t.Fatal(e)
			}
			count := MaxHistoryBasisBytes / len(raw)
			if count < 2 || count >= journal.MaxEntries {
				t.Fatal("bad bound fixture")
			}
			j := &boundedHistoryJournal{earlier: duringGuard}
			b := &boundedHistoryBases{values: map[shoal.ID]decision.AdjudicationBasis{}}
			d, e := f.service.caller(f.ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			a := attribution(d)
			pc := f.cfg
			for i := 0; i < count; i++ {
				p := admissionProposed(t, f.authority.binding.Policy, f.authority.binding.Prediction, pc)
				basis, e := decision.NewAdjudicationBasis(p, c)
				if e != nil {
					t.Fatal(e)
				}
				id, e := journal.ReceiptID(journal.Scope{Domain: d.AuthorizationDomain()}, p.TargetID(), []byte(fmt.Sprint(i)), a)
				if e != nil {
					t.Fatal(e)
				}
				r := journal.Receipt{ID: id, Version: int64(i + 1), TargetID: p.TargetID(), TaskID: p.TaskID(), PictureID: p.PictureID(), PolicyID: p.PolicyID(), ProposalID: p.ID(), BasisID: basis.ID(), ProposalConfig: p.Config(), Adjudicator: a, ReceivedAt: f.clock()}
				j.rows = append(j.rows, r)
				b.values[basis.ID()] = basis
				pc.ExpectedVersion = r.Version
				pc.ExpectedHeadID = r.ID
			}
			cfg := f.service.config
			cfg.Journal = j
			cfg.Bases = b
			svc, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = svc.Adjudicate(f.ctx, []byte("would-exceed"), pc); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
				t.Fatalf("admitted unreadable history: %v", e)
			}
			if j.writes != 0 {
				t.Fatal("committed unreadable history")
			}
			if duringGuard {
				if j.appends != 1 || b.retains != 1 {
					t.Fatal("did not exercise pre-CAS growth")
				}
			} else if j.appends != 0 || b.retains != 0 {
				t.Fatal("exceeded before retention")
			}
			h, e := svc.History(f.ctx, j.rows[0].TargetID)
			if e != nil || len(h) != count {
				t.Fatalf("original history unreadable: %v", e)
			}
		})
	}
}
