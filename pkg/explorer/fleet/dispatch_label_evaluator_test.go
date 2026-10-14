// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The dispatch read paths under the real reader label evaluator (#564),
// replacing the stub verdicts #369 and #575 were tested with: evidence
// carries the structured terms of a (source, label) policy (#570), and
// whether a reader may see it is decided from that reader's real decision.

func labelPolicyOn(t *testing.T, source, label string) []byte {
	t.Helper()
	id, err := authorized.LabelPolicyID([]byte(source), label)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func secretLabelPolicyID(t *testing.T) []byte {
	t.Helper()
	id, err := authorized.LabelPolicyID([]byte("source"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// secretTerms is the visibility a reference to a document on "source"
// labelled secret is recorded with: its label policy's grant labels.
func secretTerms(t *testing.T) []string {
	t.Helper()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: []byte("source"),
		GrantPolicyID: secretLabelPolicyID(t), Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	terms, err := policy.VisibilityTerms()
	if err != nil {
		t.Fatal(err)
	}
	labels, err := interaction.Conjoin(terms)
	if err != nil {
		t.Fatal(err)
	}
	return labels
}

func structuredEvidence(t *testing.T) (labelled, open EvidenceRef) {
	t.Helper()
	return EvidenceRef{
			AnchorID: "anchor-secret", Kind: interaction.EvidenceDocument,
			NodeIDs: []shoal.ID{"node-secret"}, Visibility: secretTerms(t),
		}, EvidenceRef{
			AnchorID: "anchor-open", Kind: interaction.EvidenceDocument,
			NodeIDs: []shoal.ID{"node-open"},
		}
}

// labelHolder is dispatchDecision plus the label policy grant: the same
// principal, holding the label.
func labelHolder(t *testing.T, operations ...auth.Operation) auth.Decision {
	t.Helper()
	return ownerUntil(t, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), true, operations...)
}

// ownerUntil is the "owner" principal expiring at expires, holding the label
// or not.
func ownerUntil(
	t *testing.T, expires time.Time, holds bool, operations ...auth.Operation,
) auth.Decision {
	t.Helper()
	policies := [][]byte{[]byte("policy")}
	if holds {
		policies = append(policies, secretLabelPolicyID(t))
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:     operations,
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      1,
		AuthenticationExpires: expires,
		RequestID:             "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// catalogGate is a NodeGate over a fixed catalog of node rules: each node's
// rule is the conjunction of the structured terms listed for it, and the
// reader is decided by the real evaluator. The authorized client's NodeGate
// reads the same thing from the policy catalog (dispatch_node_gate_test.go
// in internal/explorerfleetevents drives that one); this lets the fleet's own
// fixtures, whose node IDs exist in no corpus, keep a current rule per node.
type catalogGate struct {
	evaluator *authorized.LabelVisibility
	mu        sync.Mutex
	rules     map[shoal.ID][]string
	err       error
}

func (*catalogGate) GraphEvidenceValid(
	context.Context, shoal.ID, time.Time, []interaction.EvidenceReference,
) (bool, error) {
	return true, nil
}

func (g *catalogGate) GraphsVisibleToReader(
	ctx context.Context, graphs []evidencelabels.Graph,
) ([]bool, error) {
	verdicts := make([]bool, len(graphs))
	for index, graph := range graphs {
		visible, err := g.graphVisible(ctx, graph.NodeIDs, graph.EdgeIDs)
		if err != nil {
			return nil, err
		}
		verdicts[index] = visible
	}
	return verdicts, nil
}

func (g *catalogGate) graphVisible(
	ctx context.Context, nodeIDs, edgeIDs []shoal.ID,
) (bool, error) {
	g.mu.Lock()
	if g.err != nil {
		g.mu.Unlock()
		return false, g.err
	}
	var terms []string
	for _, id := range append(append([]shoal.ID(nil), nodeIDs...), edgeIDs...) {
		rule, ok := g.rules[id]
		if !ok {
			g.mu.Unlock()
			return false, nil
		}
		terms = append(terms, rule...)
	}
	g.mu.Unlock()
	return g.evaluator.VisibleToReader(ctx, terms)
}

// policyTerms is the structured terms of one policy on "domain".
func policyTerms(t *testing.T, source, grant []byte) []string {
	t.Helper()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: source,
		GrantPolicyID: grant, Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	terms, err := policy.VisibilityTerms()
	if err != nil {
		t.Fatal(err)
	}
	return terms
}

// fixtureCatalog is the current rules of structuredEvidence's two nodes:
// node-open is governed by the action's own source policy, node-secret by
// that and its secret label policy.
func fixtureCatalog(t *testing.T, resolver auth.Resolver, now time.Time) *catalogGate {
	t.Helper()
	source := policyTerms(t, []byte("source"), []byte("policy"))
	return &catalogGate{
		evaluator: realEvaluator(t, resolver, now),
		rules: map[shoal.ID][]string{
			"node-open": source,
			"node-secret": append(append([]string(nil), source...),
				policyTerms(t, []byte("source"), secretLabelPolicyID(t))...),
		},
	}
}

func realEvaluator(
	t *testing.T, resolver auth.Resolver, now time.Time,
	ceilings ...auth.ServiceCeilingConfig,
) *authorized.LabelVisibility {
	t.Helper()
	resolved, err := authorized.NewStaticCeilingResolver(ceilings...)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := authorized.NewLabelVisibility(authorized.LabelVisibilityConfig{
		Resolver: resolver, Ceilings: resolved,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluator
}

// labelledFixture is the executor-claim fixture with evidence stored on its
// action and the real evaluator wired.
func labelledFixture(t *testing.T, evidence ...EvidenceRef) *executorClaimFixture {
	t.Helper()
	fixture := newExecutorClaimFixture(t)
	fixture.service.evidenceVisibility = realEvaluator(
		t, fixture.authority.Resolver(), fixture.now)
	fixture.service.evidenceNodes = fixtureCatalog(
		t, fixture.authority.Resolver(), fixture.now)
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.Evidence = evidence
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored
	return fixture
}

type dispatchReads struct {
	status ActionRecord
	pull   ActionRecord
	team   ActionRecord
}

func readDispatch(
	t *testing.T, fixture *executorClaimFixture, reader, overseer context.Context,
) dispatchReads {
	t.Helper()
	status, err := fixture.service.Status(reader, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatalf("Status = %v", err)
	}
	pulled, err := fixture.service.Pull(reader, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil || len(pulled.Actions) != 1 {
		t.Fatalf("Pull = %d actions, %v", len(pulled.Actions), err)
	}
	team, err := fixture.service.TeamActions(overseer, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context:   dispatchContext(fixture.now, "request"),
	})
	if err != nil || len(team.Actions) != 1 {
		t.Fatalf("TeamActions = %d actions, %v", len(team.Actions), err)
	}
	return dispatchReads{status: status, pull: pulled.Actions[0], team: team.Actions[0]}
}

func assertReadsEqual(t *testing.T, who string, got, want dispatchReads) {
	t.Helper()
	for _, path := range []struct {
		name      string
		got, want ActionRecord
	}{
		{"Status", got.status, want.status},
		{"Pull", got.pull, want.pull},
		{"TeamActions", got.team, want.team},
	} {
		if !reflect.DeepEqual(path.got, path.want) {
			t.Fatalf("%s: %s returned\n%#v\nwant\n%#v", who, path.name, path.got, path.want)
		}
	}
}

// TestADispatchHolderSeesTheStoredEvidence: a reader holding the label
// receives every reference exactly as stored, on Status, Pull and
// TeamActions. A reader without it receives what it would have had the
// labelled reference never been recorded.
func TestADispatchHolderSeesTheStoredEvidence(t *testing.T) {
	labelled, open := structuredEvidence(t)
	fixture := labelledFixture(t, labelled, open)
	holder := bindDecision(t, fixture.authority, labelHolder(t,
		auth.OperationDispatch, auth.OperationInvoke))
	overseer := bindDecision(t, fixture.authority, labelHolder(t,
		auth.OperationTeamOverviewRead))
	stored := cloneActionRecord(fixture.dispatchStore.records[string(fixture.queued.ID)])
	held := readDispatch(t, fixture, holder, overseer)
	for name, record := range map[string]ActionRecord{
		"Status": held.status, "Pull": held.pull, "TeamActions": held.team,
	} {
		if !reflect.DeepEqual(record.Evidence, stored.Evidence) {
			t.Fatalf("%s gave a holder %#v, want the stored %#v",
				name, record.Evidence, stored.Evidence)
		}
	}

	outsiderOverseer := bindDecision(t, fixture.authority, dispatchDecision(
		t, "owner", "actor", "request", auth.OperationTeamOverviewRead))
	outside := readDispatch(t, fixture, fixture.enqueuer, outsiderOverseer)
	never := labelledFixture(t, open)
	neverOverseer := bindDecision(t, never.authority, dispatchDecision(
		t, "owner", "actor", "request", auth.OperationTeamOverviewRead))
	assertReadsEqual(t, "an outsider", outside,
		readDispatch(t, never, never.enqueuer, neverOverseer))
}

// TestADispatchReplayShowsAHolderTheStoredEvidence covers #575's three replay
// paths under the real evaluator: the enqueuer holding the label gets the
// stored record back, and one without it gets what a record without the
// labelled reference would have given.
func TestADispatchReplayShowsAHolderTheStoredEvidence(t *testing.T) {
	replays := []struct {
		name   string
		replay func(*testing.T, *executorClaimFixture, context.Context) ActionRecord
	}{
		{"enqueue replay", func(t *testing.T, f *executorClaimFixture, ctx context.Context) ActionRecord {
			record, err := f.service.Enqueue(ctx, enqueueOf(f))
			if err != nil {
				t.Fatalf("enqueue replay = %v", err)
			}
			return record
		}},
		{"invoke terminal replay", func(t *testing.T, f *executorClaimFixture, ctx context.Context) ActionRecord {
			record, err := f.service.Invoke(ctx, InvokeRequest{
				Enqueue: enqueueOf(f),
				ClaimID: []byte("invoke-claim"), Lease: time.Minute,
			})
			if err != nil {
				t.Fatalf("invoke replay = %v", err)
			}
			return record
		}},
	}
	for _, probe := range replays {
		t.Run(probe.name, func(t *testing.T) {
			labelled, open := structuredEvidence(t)
			// The holder enqueued its own action: a replay is answered only
			// to the authorization that enqueued.
			fixture := labelledFixture(t)
			holder := bindDecision(t, fixture.authority, labelHolder(t,
				auth.OperationDispatch, auth.OperationInvoke))
			queued, err := fixture.service.Enqueue(holder, holderEnqueue(fixture.now))
			if err != nil {
				t.Fatal(err)
			}
			fixture.queued, fixture.enqueuer = queued, holder
			completedByAnotherPrincipal(t, fixture)
			setEvidence(fixture, labelled, open)
			stored := cloneActionRecord(fixture.dispatchStore.records[string(fixture.queued.ID)])
			got := probe.replay(t, fixture, holder)
			if !reflect.DeepEqual(got.Evidence, stored.Evidence) {
				t.Fatalf("a holder's %s returned %#v, want the stored %#v",
					probe.name, got.Evidence, stored.Evidence)
			}

			outsiderFixture := labelledFixture(t)
			completedByAnotherPrincipal(t, outsiderFixture)
			setEvidence(outsiderFixture, labelled, open)
			outside := probe.replay(t, outsiderFixture, outsiderFixture.enqueuer)
			never := labelledFixture(t)
			completedByAnotherPrincipal(t, never)
			setEvidence(never, open)
			if want := probe.replay(t, never, never.enqueuer); !reflect.DeepEqual(outside, want) {
				t.Fatalf("an outsider's %s returned\n%#v\nwant, as if never "+
					"recorded,\n%#v", probe.name, outside, want)
			}
		})
	}

	t.Run("approval re-request", func(t *testing.T) {
		labelled, open := structuredEvidence(t)
		replay := func(t *testing.T, decision auth.Decision, evidence ...EvidenceRef) (ActionRecord, []EvidenceRef) {
			t.Helper()
			approval := decided(
				validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
			now := approval.DecidedAt.Add(2 * time.Second)
			authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			dispatchStore := newMemoryDispatchStore()
			service := &ApprovalService{
				dispatch: &DispatchService{
					store: dispatchStore, outbox: dispatchStore,
					recorder: &dispatchRecorder{}, events: dispatchEvents{},
					clock:              func() time.Time { return now },
					evidenceVisibility: realEvaluator(t, authority.Resolver(), now),
					evidenceNodes:      fixtureCatalog(t, authority.Resolver(), now),
				},
				store: &memoryApprovalStore{records: map[string]ApprovalRecord{
					string(approval.ID): CloneApprovalRecord(approval),
				}},
				recorder:    unguardedApprovalRecorder{},
				narrowed:    func(context.Context) bool { return false },
				generations: fixedGenerations{generation: approval.PolicyGeneration},
				window:      DefaultApprovalWindow,
			}
			action := cloneActionRecord(approval.Request)
			action.ApprovalRequestDigest = approval.RequestDigest
			action.ApproverSubject = approval.ApproverSubject
			action.ApproverActor = approval.ApproverActor
			action.ApproverClientID = approval.ApproverClientID
			action.ApprovedAt = approval.DecidedAt
			action.State = DispatchSucceeded
			action.ClaimID = []byte("worker-claim")
			action.ClaimFence = 1
			action.ClaimLease = time.Minute
			action.ClaimLeaseUntil = now.Add(time.Minute).UTC()
			action.ClaimantSubject = "worker-subject"
			action.ClaimantActor = "worker-actor"
			action.ExecutionPolicyGeneration = 1
			action.ExecutionExpiresAt = action.Deadline
			action.Evidence = evidence
			ctx := bindDecision(t, authority, decision)
			replayed, err := service.replayMaterialized(ctx, action, approval)
			if err != nil {
				t.Fatalf("the materialized replay was refused: %v", err)
			}
			return replayed, action.Evidence
		}
		expires := time.Unix(1_900_000_000, 0).UTC()
		held, stored := replay(t, ownerUntil(t, expires, true, auth.OperationInvoke), labelled, open)
		if !reflect.DeepEqual(held.Evidence, stored) {
			t.Fatalf("a holder's approval replay returned %#v, want %#v",
				held.Evidence, stored)
		}
		outsider := ownerUntil(t, expires, false, auth.OperationInvoke)
		outside, _ := replay(t, outsider, labelled, open)
		want, _ := replay(t, outsider, open)
		if !reflect.DeepEqual(outside, want) {
			t.Fatalf("an outsider's approval replay returned\n%#v\nwant\n%#v", outside, want)
		}
	})
}

// enqueueOf is the enqueue request that created the fixture's action, which
// equivalentEnqueue answers as a replay.
func enqueueOf(f *executorClaimFixture) EnqueueRequest {
	if string(f.queued.ID) == "holder-action" {
		return holderEnqueue(f.now)
	}
	return dispatchEnqueue(f.now, "request")
}

func holderEnqueue(now time.Time) EnqueueRequest {
	request := dispatchEnqueue(now, "request")
	request.ID = []byte("holder-action")
	request.IdempotencyKey = []byte("holder-idempotency")
	return request
}

func setEvidence(fixture *executorClaimFixture, evidence ...EvidenceRef) {
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.Evidence = evidence
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored
}

// TestAnExecutorWhoseCeilingLacksTheLabelIsRefused: a bound worker is a
// trusted service, so even holding the label policy grant it sees a
// reference only when every term of the nodes' current rules is inside its
// configured ceiling and, as at the tablet, the rule names its own role. No
// document rule names svc:action_execution, so a worker sees no reference
// that names a node whatever its ceiling holds; the ceiling-bounded positive
// case is pinned on the interaction reads (authorized
// TestATrustedServiceIsBoundedByItsCeiling).
func TestAnExecutorWhoseCeilingLacksTheLabelIsRefused(t *testing.T) {
	role := auth.ServiceRoleActionExecution
	labelled, open := structuredEvidence(t)
	labelled.Visibility = append(append([]string(nil), labelled.Visibility...),
		"svc:"+string(role))
	ceiling := func(identity shoal.ID, withGrant bool) auth.ServiceCeilingConfig {
		labels := [][]byte{[]byte("svc:" + string(role))}
		for _, term := range secretTerms(t) {
			if withGrant || !strings.HasPrefix(term, "g:") {
				labels = append(labels, []byte(term))
			}
		}
		return auth.ServiceCeilingConfig{
			Identity: identity, Role: role,
			Authorizations: accumulo.NewAuthorizations(labels...),
		}
	}
	for _, probe := range []struct {
		ceiling shoal.ID
		want    []shoal.ID
	}{
		{"full-ceiling", []shoal.ID{}},
		{"lacking-ceiling", []shoal.ID{}},
		{"unconfigured-ceiling", []shoal.ID{}},
	} {
		t.Run(string(probe.ceiling), func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			fixture.service.evidenceVisibility = realEvaluator(
				t, fixture.authority.Resolver(), fixture.now,
				ceiling("full-ceiling", true), ceiling("lacking-ceiling", false))
			gate := fixtureCatalog(t, fixture.authority.Resolver(), fixture.now)
			gate.evaluator = fixture.service.evidenceVisibility.(*authorized.LabelVisibility)
			fixture.service.evidenceNodes = gate
			setEvidence(fixture, labelled, open)
			decision, err := auth.NewDecision(auth.DecisionConfig{
				Subject: "worker-subject", Actor: "worker-actor",
				AuthorizationDomain: []byte("domain"),
				AllowedOperations:   []auth.Operation{auth.OperationExecute},
				PermittedSourceIDs:  [][]byte{[]byte("source")},
				PermittedPolicyIDs: [][]byte{
					[]byte("policy"), secretLabelPolicyID(t),
				},
				PolicyGeneration:       1,
				AuthenticationExpires:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
				RequestID:              "worker-request",
				CorrelationID:          "correlation",
				ServiceRole:            role,
				ServiceCeilingIdentity: probe.ceiling,
				ExecutorBinding:        "exec",
			})
			if err != nil {
				t.Fatal(err)
			}
			pulled := fixture.pullAs(t, bindDecision(t, fixture.authority, decision), "worker")
			if len(pulled) != 1 {
				t.Fatalf("pulled %d actions, want 1", len(pulled))
			}
			assertAnchors(t, "Pull (execute route)", pulled[0].Evidence, probe.want)
		})
	}
}

// tightenSecret adds the pii label policy to node-secret's current rule;
// loosenSecret leaves it governed by the source policy alone.
func tightenSecret(t *testing.T, gate *catalogGate) {
	t.Helper()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.rules["node-secret"] = append(append([]string(nil), gate.rules["node-secret"]...),
		policyTerms(t, []byte("source"), labelPolicyOn(t, "source", "pii"))...)
}

func loosenSecret(t *testing.T, gate *catalogGate) {
	t.Helper()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.rules["node-secret"] = policyTerms(t, []byte("source"), []byte("policy"))
}

// approvalReplay is the approval re-request replay of an action completed by
// another principal with evidence, under the catalog gate, after relabel.
func approvalReplay(
	t *testing.T, holds bool, relabel func(*testing.T, *catalogGate),
	evidence ...EvidenceRef,
) ActionRecord {
	t.Helper()
	approval := decided(validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	now := approval.DecidedAt.Add(2 * time.Second)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	gate := fixtureCatalog(t, authority.Resolver(), now)
	relabel(t, gate)
	dispatchStore := newMemoryDispatchStore()
	service := &ApprovalService{
		dispatch: &DispatchService{
			store: dispatchStore, outbox: dispatchStore,
			recorder: &dispatchRecorder{}, events: dispatchEvents{},
			clock: func() time.Time { return now }, evidenceNodes: gate,
		},
		store: &memoryApprovalStore{records: map[string]ApprovalRecord{
			string(approval.ID): CloneApprovalRecord(approval),
		}},
		recorder:    unguardedApprovalRecorder{},
		narrowed:    func(context.Context) bool { return false },
		generations: fixedGenerations{generation: approval.PolicyGeneration},
		window:      DefaultApprovalWindow,
	}
	action := cloneActionRecord(approval.Request)
	action.ApprovalRequestDigest = approval.RequestDigest
	action.ApproverSubject = approval.ApproverSubject
	action.ApproverActor = approval.ApproverActor
	action.ApproverClientID = approval.ApproverClientID
	action.ApprovedAt = approval.DecidedAt
	action.State = DispatchSucceeded
	action.ClaimID = []byte("worker-claim")
	action.ClaimFence = 1
	action.ClaimLease = time.Minute
	action.ClaimLeaseUntil = now.Add(time.Minute).UTC()
	action.ClaimantSubject = "worker-subject"
	action.ClaimantActor = "worker-actor"
	action.ExecutionPolicyGeneration = 1
	action.ExecutionExpiresAt = action.Deadline
	action.Evidence = evidence
	if len(evidence) > 0 {
		action.EvidenceSnapshotID = "evidence-snapshot"
		action.EvidenceSnapshotAsOf = now
	}
	ctx := bindDecision(t, authority, ownerUntil(t, now.Add(time.Hour), holds, auth.OperationInvoke))
	replayed, err := service.replayMaterialized(ctx, action, approval)
	if err != nil {
		t.Fatalf("the materialized replay was refused: %v", err)
	}
	return replayed
}

func hasAnchor(evidence []EvidenceRef, anchor shoal.ID) bool {
	for _, reference := range evidence {
		if reference.AnchorID == anchor {
			return true
		}
	}
	return false
}

// TestARelabelGovernsEveryDispatchRead: a reference that names a node is
// decided by the node's current rule on Status, Pull, TeamActions and all
// three replays, not by the labels stored with it (#564). Tightened, a
// holder of the old label loses it; loosened, a reader without the removed
// label gains it.
func TestARelabelGovernsEveryDispatchRead(t *testing.T) {
	noop := func(*testing.T, *catalogGate) {}
	t.Run("Status, Pull and TeamActions", func(t *testing.T) {
		labelled, open := structuredEvidence(t)
		fixture := labelledFixture(t, labelled, open)
		gate := fixture.service.evidenceNodes.(*catalogGate)
		holder := bindDecision(t, fixture.authority, labelHolder(t,
			auth.OperationDispatch, auth.OperationInvoke))
		overseer := bindDecision(t, fixture.authority, labelHolder(t,
			auth.OperationTeamOverviewRead))
		outsiderOverseer := bindDecision(t, fixture.authority, dispatchDecision(
			t, "owner", "actor", "request", auth.OperationTeamOverviewRead))
		check := func(stage string, reader, team context.Context, want bool) {
			t.Helper()
			reads := readDispatch(t, fixture, reader, team)
			for name, record := range map[string]ActionRecord{
				"Status": reads.status, "Pull": reads.pull, "TeamActions": reads.team,
			} {
				if got := hasAnchor(record.Evidence, "anchor-secret"); got != want {
					t.Fatalf("%s: %s visible = %v, want %v", stage, name, got, want)
				}
			}
		}
		check("a holder before", holder, overseer, true)
		tightenSecret(t, gate)
		check("a holder of the old label after tightening", holder, overseer, false)
		loosenSecret(t, gate)
		check("a reader without the label after loosening",
			fixture.enqueuer, outsiderOverseer, true)
	})
	for _, probe := range []struct {
		name   string
		replay func(*testing.T, *executorClaimFixture, context.Context) ActionRecord
	}{
		{"enqueue replay", func(t *testing.T, f *executorClaimFixture, ctx context.Context) ActionRecord {
			record, err := f.service.Enqueue(ctx, enqueueOf(f))
			if err != nil {
				t.Fatal(err)
			}
			return record
		}},
		{"invoke terminal replay", func(t *testing.T, f *executorClaimFixture, ctx context.Context) ActionRecord {
			record, err := f.service.Invoke(ctx, InvokeRequest{
				Enqueue: enqueueOf(f), ClaimID: []byte("invoke-claim"), Lease: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			return record
		}},
	} {
		t.Run(probe.name+" after tightening", func(t *testing.T) {
			labelled, open := structuredEvidence(t)
			fixture := labelledFixture(t)
			holder := bindDecision(t, fixture.authority, labelHolder(t,
				auth.OperationDispatch, auth.OperationInvoke))
			queued, err := fixture.service.Enqueue(holder, holderEnqueue(fixture.now))
			if err != nil {
				t.Fatal(err)
			}
			fixture.queued, fixture.enqueuer = queued, holder
			completedByAnotherPrincipal(t, fixture)
			setEvidence(fixture, labelled, open)
			if !hasAnchor(probe.replay(t, fixture, holder).Evidence, "anchor-secret") {
				t.Fatal("the holder lost the reference before the relabel")
			}
			tightenSecret(t, fixture.service.evidenceNodes.(*catalogGate))
			if hasAnchor(probe.replay(t, fixture, holder).Evidence, "anchor-secret") {
				t.Fatal("a holder of the old label kept the reference after tightening")
			}
		})
		t.Run(probe.name+" after loosening", func(t *testing.T) {
			labelled, open := structuredEvidence(t)
			fixture := labelledFixture(t)
			completedByAnotherPrincipal(t, fixture)
			setEvidence(fixture, labelled, open)
			if hasAnchor(probe.replay(t, fixture, fixture.enqueuer).Evidence, "anchor-secret") {
				t.Fatal("a reader without the label saw the reference before loosening")
			}
			loosenSecret(t, fixture.service.evidenceNodes.(*catalogGate))
			if !hasAnchor(probe.replay(t, fixture, fixture.enqueuer).Evidence, "anchor-secret") {
				t.Fatal("a reader without the removed label did not see the reference after loosening")
			}
		})
	}
	t.Run("approval re-request", func(t *testing.T) {
		labelled, open := structuredEvidence(t)
		if !hasAnchor(approvalReplay(t, true, noop, labelled, open).Evidence, "anchor-secret") {
			t.Fatal("the holder lost the reference before the relabel")
		}
		if hasAnchor(approvalReplay(t, true, tightenSecret, labelled, open).Evidence, "anchor-secret") {
			t.Fatal("a holder of the old label kept the reference after tightening")
		}
		if !hasAnchor(approvalReplay(t, false, loosenSecret, labelled, open).Evidence, "anchor-secret") {
			t.Fatal("a reader without the removed label did not see the reference after loosening")
		}
	})
}

// TestEvidenceAssertionsMustRideOnListedEdges pins what the read gate relies
// on: a graph reference's assertion must be on one of its path edges
// (interaction.EvidenceReference.Validate), so deciding the reference by its
// edges decides the assertion too (#564).
func TestEvidenceAssertionsMustRideOnListedEdges(t *testing.T) {
	reference := EvidenceRef{
		AnchorID: "anchor-graph", Kind: interaction.EvidenceGraph,
		NodeIDs: []shoal.ID{"node-a", "node-b"}, EdgeIDs: []shoal.ID{"edge-ab"},
		Assertions: []interaction.AssertionReference{{
			AssertionID: "assertion", EdgeID: "edge-ab",
			Origin: ontology.AssertionExplicit,
		}},
	}
	if err := validateEvidence([]EvidenceRef{reference}); err != nil {
		t.Fatalf("an assertion on a listed edge was refused: %v", err)
	}
	reference.Assertions[0].EdgeID = "edge-elsewhere"
	if err := validateEvidence([]EvidenceRef{reference}); err == nil {
		t.Fatal("an assertion on an unlisted edge was accepted")
	}
}

// countingGate counts how often the dispatch plane asks its node gate.
type countingGate struct {
	*catalogGate
	calls int
}

func (g *countingGate) GraphsVisibleToReader(
	ctx context.Context, graphs []evidencelabels.Graph,
) ([]bool, error) {
	g.calls++
	return g.catalogGate.GraphsVisibleToReader(ctx, graphs)
}

// TestADispatchPageAsksTheGateOnce: a page of many records, each with several
// references, is decided in one gate call, so its catalog reads are bounded
// by the gate's batching rather than multiplied by references (#564).
func TestADispatchPageAsksTheGateOnce(t *testing.T) {
	labelled, open := structuredEvidence(t)
	fixture := labelledFixture(t, labelled, open)
	gate := &countingGate{catalogGate: fixture.service.evidenceNodes.(*catalogGate)}
	fixture.service.evidenceNodes = gate
	base := fixture.dispatchStore.records[string(fixture.queued.ID)]
	for index := range 9 {
		record := cloneActionRecord(base)
		record.ID = []byte("page-action-" + string(rune('a'+index)))
		record.IdempotencyKey = []byte("page-idempotency-" + string(rune('a'+index)))
		fixture.dispatchStore.records[string(record.ID)] = record
	}
	overseer := bindDecision(t, fixture.authority, labelHolder(t,
		auth.OperationTeamOverviewRead))
	page, err := fixture.service.TeamActions(overseer, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context:   dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 10 {
		t.Fatalf("TeamActions returned %d actions, want 10", len(page.Actions))
	}
	for _, action := range page.Actions {
		if !hasAnchor(action.Evidence, "anchor-secret") {
			t.Fatal("the holder lost a reference")
		}
	}
	if gate.calls != 1 {
		t.Fatalf("a page of 10 records and 20 references asked the gate %d times, want 1", gate.calls)
	}
}

// withSnapshot pins evidence to a snapshot, as a completion with evidence
// does, or clears the pin with the evidence, as a record without any has.
func withSnapshot(fixture *executorClaimFixture, evidence ...EvidenceRef) {
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.Evidence = evidence
	if len(evidence) > 0 {
		stored.EvidenceSnapshotID = "evidence-snapshot"
		stored.EvidenceSnapshotAsOf = fixture.now
	} else {
		stored.EvidenceSnapshotID = ""
		stored.EvidenceSnapshotAsOf = time.Time{}
	}
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored
}

// TestAnOutsiderOfOnlyLabelledEvidenceSeesARecordThatNeverHadAny: when every
// reference is withheld, the record must be exactly one that never had
// evidence, snapshot pin included; a pin left behind says evidence was
// withheld (#398). Status, Pull, TeamActions and the three replays.
func TestAnOutsiderOfOnlyLabelledEvidenceSeesARecordThatNeverHadAny(t *testing.T) {
	labelled, _ := structuredEvidence(t)
	reads := func(evidence ...EvidenceRef) dispatchReads {
		fixture := labelledFixture(t)
		withSnapshot(fixture, evidence...)
		overseer := bindDecision(t, fixture.authority, dispatchDecision(
			t, "owner", "actor", "request", auth.OperationTeamOverviewRead))
		return readDispatch(t, fixture, fixture.enqueuer, overseer)
	}
	got, want := reads(labelled), reads()
	if want.status.EvidenceSnapshotID != "" {
		t.Fatal("the never-had-evidence record carries a snapshot pin")
	}
	assertReadsEqual(t, "an outsider", got, want)

	replay := func(name string, evidence ...EvidenceRef) ActionRecord {
		fixture := labelledFixture(t)
		completedByAnotherPrincipal(t, fixture)
		withSnapshot(fixture, evidence...)
		switch name {
		case "enqueue":
			record, err := fixture.service.Enqueue(fixture.enqueuer, enqueueOf(fixture))
			if err != nil {
				t.Fatal(err)
			}
			return record
		default:
			record, err := fixture.service.Invoke(fixture.enqueuer, InvokeRequest{
				Enqueue: enqueueOf(fixture), ClaimID: []byte("invoke-claim"), Lease: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			return record
		}
	}
	for _, name := range []string{"enqueue", "invoke"} {
		if got, want := replay(name, labelled), replay(name); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s replay to an outsider:\n%#v\nwant, as if it never had evidence,\n%#v",
				name, got, want)
		}
	}

	approval := func(evidence ...EvidenceRef) ActionRecord {
		noop := func(*testing.T, *catalogGate) {}
		record := approvalReplay(t, false, noop, evidence...)
		return record
	}
	if got, want := approval(labelled), approval(); !reflect.DeepEqual(got, want) {
		t.Fatalf("approval replay to an outsider:\n%#v\nwant\n%#v", got, want)
	}
}

// TestRedactionNeverChangesTheRequestDigest pins that redaction touches only
// completion-time fields (evidence and its snapshot pin): the approval
// request digest of what a reader is shown equals the stored record's. No
// digest covers evidence, so redaction never has to recompute one per viewer.
func TestRedactionNeverChangesTheRequestDigest(t *testing.T) {
	labelled, _ := structuredEvidence(t)
	fixture := labelledFixture(t)
	completedByAnotherPrincipal(t, fixture)
	withSnapshot(fixture, labelled)
	stored := cloneActionRecord(fixture.dispatchStore.records[string(fixture.queued.ID)])
	redacted, err := fixture.service.readableRecord(fixture.enqueuer, stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(redacted.Evidence) != 0 || redacted.EvidenceSnapshotID != "" {
		t.Fatalf("the outsider was not redacted; the probe is vacuous: %#v", redacted.Evidence)
	}
	if string(ApprovalRequestDigest(redacted)) != string(ApprovalRequestDigest(stored)) {
		t.Fatal("redaction changed the approval request digest")
	}
}
