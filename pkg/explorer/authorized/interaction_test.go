/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized_test

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type generationChangingInteractionBase struct {
	*explorer.Explorer
	after func()
}

func (b *generationChangingInteractionBase) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	recorded, err := b.Explorer.RecordInteractionResult(ctx, session)
	if err == nil && b.after != nil {
		b.after()
	}
	return recorded, err
}

type forgedResultInteractionBase struct {
	*explorer.Explorer
}

type voidOnlyInteractionWriter struct {
	writer explorer.InteractionWriter
}

func (w voidOnlyInteractionWriter) EnsureInteractionSink(ctx context.Context) error {
	return w.writer.EnsureInteractionSink(ctx)
}

func (w voidOnlyInteractionWriter) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	return w.writer.RecordInteraction(ctx, session)
}

func (b *forgedResultInteractionBase) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	recorded, err := b.Explorer.RecordInteractionResult(ctx, session)
	if err == nil {
		recorded.Actor.SubjectID = "forged-return"
	}
	return recorded, err
}

type countingInteractionStore struct {
	authorized.PolicyStore
	nodesCalls int
}

type edgeHidingInteractionStore struct {
	authorized.PolicyStore
	hidden shoal.ID
}

func exactAuthorizedGraphEvidence(
	t testing.TB, corpus *explorer.Explorer, edge graph.Edge,
) interaction.EvidenceReference {
	t.Helper()
	neighborhood, err := corpus.Neighborhood(
		context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{edge.From}, Depth: 1,
		})
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[shoal.ID]graph.Node, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		nodes[node.ID] = node
	}
	var exact graph.Edge
	for _, candidate := range neighborhood.Edges {
		if candidate.ID == edge.ID {
			exact = candidate
			break
		}
	}
	if exact.ID == "" || nodes[edge.From].ID == "" || nodes[edge.To].ID == "" {
		t.Fatal("exact graph evidence is unavailable")
	}
	anchor, err := inference.NewGraphAnchor(graph.Path{
		Nodes: []graph.Node{nodes[edge.From], nodes[edge.To]},
		Edges: []graph.Edge{exact},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func (s edgeHidingInteractionStore) Edges(
	ctx context.Context,
	ids []shoal.ID,
) (map[shoal.ID]authorized.EdgeRegistration, error) {
	result, err := s.PolicyStore.Edges(ctx, ids)
	if err != nil {
		return nil, err
	}
	delete(result, s.hidden)
	return result, nil
}

func (s *countingInteractionStore) Nodes(
	ctx context.Context, ids []shoal.ID,
) (map[shoal.ID]authorized.NodeRegistration, error) {
	s.nodesCalls++
	return s.PolicyStore.Nodes(ctx, ids)
}

type countingInteractionBase struct {
	*explorer.Explorer
	recordCalls  int
	recordsCalls int
}

type hiddenFirstInteractionRecordBase struct {
	*explorer.Explorer
	hidden bool
}

func (b *hiddenFirstInteractionRecordBase) InteractionRecord(
	ctx context.Context, id shoal.ID,
) (explorer.InteractionRecord, error) {
	if !b.hidden {
		b.hidden = true
		return explorer.InteractionRecord{}, shoal.NewError(
			shoal.ErrorNotFound, "simulated concurrent retry winner")
	}
	return b.Explorer.InteractionRecord(ctx, id)
}

type mutatingInteractionSnapshotBase struct {
	*explorer.Explorer
	mutate func() error
}

func (b *mutatingInteractionSnapshotBase) ValidateSnapshot(
	ctx context.Context,
	id shoal.ID,
	asOf time.Time,
	nodeIDs []shoal.ID,
) error {
	if err := b.Explorer.ValidateSnapshot(
		ctx, id, asOf, nodeIDs); err != nil {
		return err
	}
	if b.mutate == nil {
		return nil
	}
	mutate := b.mutate
	b.mutate = nil
	return mutate()
}

type rejectingSnapshotValidator struct {
	calls int
}

func (v *rejectingSnapshotValidator) ValidateSnapshot(
	context.Context, shoal.ID, time.Time, []shoal.ID,
) error {
	v.calls++
	return shoal.NewError(
		shoal.ErrorConflict, "historical snapshot registry unavailable")
}

func (b *countingInteractionBase) InteractionRecord(
	ctx context.Context, id shoal.ID,
) (explorer.InteractionRecord, error) {
	b.recordCalls++
	return b.Explorer.InteractionRecord(ctx, id)
}

func (b *countingInteractionBase) InteractionRecords(
	ctx context.Context,
) ([]explorer.InteractionRecord, error) {
	b.recordsCalls++
	return b.Explorer.InteractionRecords(ctx)
}

func TestAuthorizedResultPathRequiresResultSink(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "result-sink-required",
		nil, nil, []auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: f.base, VectorScorer: f.base,
		InteractionWriter: voidOnlyInteractionWriter{writer: f.base},
		InteractionReader: f.base, SnapshotValidator: f.base,
		Resolver: f.authority.Resolver(), PolicySelector: selector,
		PolicyStore: f.store, GenerationReader: f.reader, Clock: f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := client.AnalyticsInteractionSink()
	if sink != nil {
		t.Fatal("analytics exposed a result sink backed by a void-only writer")
	}
	if _, err := interaction.NewRecorder(
		context.Background(), sink,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("recorder setup with void-only sink = %v", err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "result-sink-required"),
		Operation:  interaction.OperationRetrieval,
		SnapshotID: shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	ctx := f.context(t, decision)
	if _, err := interaction.NewRecorder(
		ctx, client,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("direct recorder setup with void-only writer = %v", err)
	}
	if _, err := client.RecordInteractionResult(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("result path without ResultSink = %v", err)
	}
	if err := client.RecordInteraction(ctx, session); err != nil {
		t.Fatalf("legacy void record path = %v", err)
	}
}

func TestAuthorizedInteractionRecorderAndViews(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///authorized-interaction.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "authorized interaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"recorder",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	ctx := f.context(t, decision)
	view, err := f.clientA.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_authorized",
		RecordedAt:               f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if err := f.clientA.EnsureInteractionSink(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.clientA.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	hydrated, err := f.clientA.Interaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.AuthorizationFingerprint != session.AuthorizationFingerprint ||
		hydrated.SnapshotID != session.SnapshotID ||
		hydrated.Actor.SubjectID != decision.Subject() ||
		hydrated.Actor.ActorID != decision.Actor() {
		t.Fatalf("hydrated interaction = %+v", hydrated)
	}
	summaries, err := f.clientA.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].SessionID != session.ID {
		t.Fatalf("authorized interactions = %+v", summaries)
	}

	bobDecision := f.decision(
		t,
		"other-reader",
		[][]byte{f.sourceB},
		[][]byte{f.policyB},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	bob := f.context(t, bobDecision)
	if _, err := f.clientB.Interaction(
		bob, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized interaction read = %v", err)
	}
	summaries, err = f.clientB.Interactions(bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("unauthorized list leaked interactions: %+v", summaries)
	}

	if _, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///authorized-interaction.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "replacement evidence under a different source policy",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.Interaction(
		ctx, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked source left interaction readable: %v", err)
	}
}

func TestAuthorizedInteractionReauthorizesExactSourceEdge(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///authorized-edge-interaction.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "authorized edge interaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.clientA.Document(
		f.admin(t), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID:     "application-evidence-edge",
		From:   receipt.Document.ID,
		To:     firstSpanID(t, view),
		Type:   "supports",
		Weight: 1,
	}
	if err := f.clientA.Connect(f.admin(t), edge); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "edge-recorder", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationConnect,
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	ctx := f.context(t, decision)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	evidence := exactAuthorizedGraphEvidence(t, f.base, edge)
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "authorized-edge"),
		RecordedAt:               f.clock.Now(),
		Operation:                interaction.OperationToolCall,
		AuthorizationOperation:   string(auth.OperationConnect),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{edge.From, edge.To},
		SeedEvidence:             []interaction.EvidenceReference{evidence},
	}
	if err := f.clientA.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	record, err := f.clientA.InteractionRecord(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.TouchedEdgeIDs) != 1 ||
		record.TouchedEdgeIDs[0] != edge.ID {
		t.Fatalf("touched edges = %v", record.TouchedEdgeIDs)
	}
	if record.Summary.AuthorizationOperation !=
		string(auth.OperationConnect) {
		t.Fatalf("authorization operation = %q",
			record.Summary.AuthorizationOperation)
	}

	retrieveOnly := f.decision(
		t, "edge-retrieve-only",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	retrieveFingerprint, err := auth.AuthorizationFingerprint(retrieveOnly)
	if err != nil {
		t.Fatal(err)
	}
	denied := session
	denied.ID = interaction.DerivedID(
		"session", "unauthorized-edge-operation")
	denied.AuthorizationFingerprint =
		shoal.ID(retrieveFingerprint.String())
	denied.AuthorizationExpiresAt =
		retrieveOnly.AuthenticationExpires()
	if err := f.clientA.RecordInteraction(
		f.context(t, retrieveOnly), denied,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("unauthorized exact operation error = %v", err)
	}
	connectOnly := f.decision(
		t, "edge-connect-only",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationConnect},
	)
	connectFingerprint, err := auth.AuthorizationFingerprint(connectOnly)
	if err != nil {
		t.Fatal(err)
	}
	deniedEvidence := session
	deniedEvidence.ID = interaction.DerivedID(
		"session", "unauthorized-edge-evidence")
	deniedEvidence.AuthorizationFingerprint =
		shoal.ID(connectFingerprint.String())
	deniedEvidence.AuthorizationExpiresAt =
		connectOnly.AuthenticationExpires()
	if err := f.clientA.RecordInteraction(
		f.context(t, connectOnly), deniedEvidence,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("missing retrieve authorization error = %v", err)
	}

	revoked := f.newClient(
		t, f.base,
		edgeHidingInteractionStore{
			PolicyStore: f.store,
			hidden:      edge.ID,
		},
		f.sourceA, f.policyA, nil,
	)
	if _, err := revoked.Interaction(
		ctx, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked edge left interaction readable: %v", err)
	}
	rejected := session
	rejected.ID = interaction.DerivedID("session", "revoked-edge-write")
	if err := revoked.RecordInteraction(ctx, rejected); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("revoked edge recording error = %v", err)
	}
	if _, err := f.base.InteractionRecord(
		context.Background(), rejected.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked edge recording persisted a record: %v", err)
	}
}

func TestFleetActionInteractionSinkUsesLifecycleOperationForEvidence(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///dispatch-evidence.txt", MediaType: explorer.MediaTypeText,
		Content: "dispatch evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.clientA.Document(
		f.admin(t), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID: "dispatch-evidence-edge", From: receipt.Document.ID,
		To: firstSpanID(t, view), Type: "supports", Weight: 1,
	}
	if err := f.clientA.Connect(f.admin(t), edge); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "dispatch-only", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationDispatch},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "dispatch-evidence"),
		Operation:  interaction.OperationToolCall,
		SnapshotID: shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{edge.From, edge.To},
		SeedEvidence: []interaction.EvidenceReference{
			exactAuthorizedGraphEvidence(t, f.base, edge),
		},
	}
	sink := f.clientA.FleetActionInteractionSink(auth.OperationDispatch)
	if sink == nil {
		t.Fatal("dispatch interaction sink is unavailable")
	}
	stored, err := sink.RecordInteractionResult(f.context(t, decision), session)
	if err != nil {
		t.Fatalf("dispatch-only lifecycle evidence = %v", err)
	}
	if stored.AuthorizationOperation != string(auth.OperationDispatch) {
		t.Fatalf("authorization operation = %q", stored.AuthorizationOperation)
	}
	invokeDecision := f.decision(
		t, "invoke-only", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationInvoke},
	)
	invokeFingerprint, err := auth.AuthorizationFingerprint(invokeDecision)
	if err != nil {
		t.Fatal(err)
	}
	invokeSession := session
	invokeSession.ID = interaction.DerivedID("session", "invoke-evidence")
	invokeSession.AuthorizationFingerprint =
		shoal.ID(invokeFingerprint.String())
	invokeSession.AuthorizationExpiresAt =
		invokeDecision.AuthenticationExpires()
	invokeSink := f.clientA.FleetActionInteractionSink(auth.OperationInvoke)
	if invokeSink == nil {
		t.Fatal("invoke interaction sink is unavailable")
	}
	if _, err := invokeSink.RecordInteractionResult(
		f.context(t, invokeDecision), invokeSession,
	); err != nil {
		t.Fatalf("invoke-only lifecycle evidence = %v", err)
	}

	// Execute is the third fleet action operation (#437), and it was missing
	// from this set — which is #480 item 1. main.go falls back to the bare
	// client when this returns nil, the bare client has no reconciliation
	// sink, and the auditor's "trusted interaction reconciliation sink is
	// unavailable" becomes ErrAuditOutcomeUnknown joined with
	// ErrActionCommitted. So every claim taken under execute committed and
	// then answered 503 "fleet action outcome requires reconciliation": the
	// write landed and the worker was told to reconcile it by hand.
	executeDecision := f.decision(
		t, "execute-only", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationExecute},
	)
	executeFingerprint, err := auth.AuthorizationFingerprint(executeDecision)
	if err != nil {
		t.Fatal(err)
	}
	executeSession := session
	executeSession.ID = interaction.DerivedID("session", "execute-evidence")
	executeSession.AuthorizationFingerprint =
		shoal.ID(executeFingerprint.String())
	executeSession.AuthorizationExpiresAt =
		executeDecision.AuthenticationExpires()
	executeSink := f.clientA.FleetActionInteractionSink(auth.OperationExecute)
	if executeSink == nil {
		t.Fatal("execute interaction sink is unavailable, so a worker that " +
			"claimed under execute commits its write and is then answered " +
			"503 requires-reconciliation")
	}
	executeStored, err := executeSink.RecordInteractionResult(
		f.context(t, executeDecision), executeSession)
	if err != nil {
		t.Fatalf("execute-only lifecycle evidence = %v", err)
	}
	if executeStored.AuthorizationOperation != string(auth.OperationExecute) {
		t.Fatalf("authorization operation = %q, want %q",
			executeStored.AuthorizationOperation, auth.OperationExecute)
	}

	// The second half, and the reason the predicate is shared: the
	// reconciliation method compared the same set separately, so admitting
	// execute at the sink while refusing it here would be the identical 503
	// with one more step before it.
	executeReconciler, ok := executeSink.(interface {
		RecordReconciledInteractionResult(
			context.Context, explorerfleetcap.Capability, interaction.Session,
		) (interaction.Session, error)
	})
	if !ok {
		t.Fatal("the execute sink does not support durable reconciliation, " +
			"which is the sink the auditor actually needs")
	}
	// Asserted as parity against an operation already in the set rather than
	// as a successful write. The durable receipt this fixture's session
	// carries makes either call fail later in the same way, which is a
	// property of the fixture and not of the gate — so the question that
	// matters is whether execute is treated like dispatch, and the gate's own
	// refusal (ErrorUnavailable, "authorized fleet reconciliation sink is
	// unavailable") is distinguishable from anything past it.
	dispatchReconciler, ok := f.clientA.FleetActionInteractionSink(
		auth.OperationDispatch).(interface {
		RecordReconciledInteractionResult(
			context.Context, explorerfleetcap.Capability, interaction.Session,
		) (interaction.Session, error)
	})
	if !ok {
		t.Fatal("the dispatch sink does not support reconciliation, so there " +
			"is nothing to compare execute against")
	}
	_, dispatchErr := dispatchReconciler.RecordReconciledInteractionResult(
		f.context(t, decision), explorerfleetcap.New(), executeStored)
	if shoal.IsErrorCode(dispatchErr, shoal.ErrorUnavailable) {
		t.Fatalf("the control is not a control: dispatch reconciliation was "+
			"itself refused by the gate (%v), so execute matching it proves "+
			"nothing", dispatchErr)
	}
	_, executeErr := executeReconciler.RecordReconciledInteractionResult(
		f.context(t, executeDecision), explorerfleetcap.New(), executeStored)
	if shoal.IsErrorCode(executeErr, shoal.ErrorUnavailable) {
		t.Fatalf("execute reconciliation was refused by the operation gate "+
			"(%v) while dispatch was not (%v): a surface that admits an "+
			"operation it cannot then reconcile is the same 503 with an "+
			"extra step", executeErr, dispatchErr)
	}

	// Approve is the fourth (#653): an approver's decision is published as an
	// approval event under the approver's own decision, whose publication
	// audit needs this sink and its reconciliation. Asserted the way execute
	// is, as parity with dispatch at the gate.
	approveDecision := f.decision(
		t, "approve-only", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationActionApprove},
	)
	approveSink := f.clientA.FleetActionInteractionSink(auth.OperationActionApprove)
	approveReconciler, ok := approveSink.(interface {
		RecordReconciledInteractionResult(
			context.Context, explorerfleetcap.Capability, interaction.Session,
		) (interaction.Session, error)
	})
	if approveSink == nil || !ok {
		t.Fatal("approve has no reconciling fleet lifecycle sink, so an " +
			"approver's committed decision answers its event unpublished")
	}
	if _, approveErr := approveReconciler.RecordReconciledInteractionResult(
		f.context(t, approveDecision), explorerfleetcap.New(), executeStored,
	); shoal.IsErrorCode(approveErr, shoal.ErrorUnavailable) {
		t.Fatalf("approve reconciliation was refused by the operation gate "+
			"(%v) while dispatch was not (%v)", approveErr, dispatchErr)
	}

	// Approval sweep is the fifth (#655): the host's system observer
	// publishes the expiries and strandings it records under its own
	// decision. Asserted the same way.
	sweepDecision := f.decision(
		t, "approval-sweep", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationApprovalSweep},
	)
	sweepSink := f.clientA.FleetActionInteractionSink(auth.OperationApprovalSweep)
	sweepReconciler, ok := sweepSink.(interface {
		RecordReconciledInteractionResult(
			context.Context, explorerfleetcap.Capability, interaction.Session,
		) (interaction.Session, error)
	})
	if sweepSink == nil || !ok {
		t.Fatal("approval sweep has no reconciling fleet lifecycle sink, so " +
			"a recorded expiry answers its event unpublished")
	}
	if _, sweepErr := sweepReconciler.RecordReconciledInteractionResult(
		f.context(t, sweepDecision), explorerfleetcap.New(), executeStored,
	); shoal.IsErrorCode(sweepErr, shoal.ErrorUnavailable) {
		t.Fatalf("approval sweep reconciliation was refused by the operation "+
			"gate (%v) while dispatch was not (%v)", sweepErr, dispatchErr)
	}

	// And the set is still a set: an unrelated operation gets no sink, so
	// this is not "any operation will do".
	if stray := f.clientA.FleetActionInteractionSink(
		auth.OperationRetrieve); stray != nil {
		t.Fatal("an unrelated operation was handed a fleet lifecycle sink")
	}

	hidden := f.newClient(
		t, f.base,
		edgeHidingInteractionStore{
			PolicyStore: f.store,
			hidden:      edge.ID,
		},
		f.sourceA, f.policyA, nil,
	)
	hiddenSink := hidden.FleetActionInteractionSink(auth.OperationDispatch)
	session.ID = interaction.DerivedID("session", "hidden-dispatch-evidence")
	if _, err := hiddenSink.RecordInteractionResult(
		f.context(t, decision), session,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("hidden exact evidence = %v", err)
	}
}

func TestFleetActionReconciliationPreservesChangedDurablePin(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	original := f.decision(
		t, "fleet-reconcile", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationDispatch},
	)
	originalFingerprint, err := auth.AuthorizationFingerprint(original)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "fleet-reconcile"),
		RecordedAt: f.clock.Now(), Operation: interaction.OperationToolCall,
		AuthorizationOperation: string(auth.OperationDispatch),
		SnapshotID:             shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(originalFingerprint.String()),
		AuthorizationExpiresAt:   original.AuthenticationExpires(),
		RequestID:                original.RequestID(),
	}
	f.clock.Set(original.AuthenticationExpires().Add(time.Second))
	refreshed := f.decision(
		t, "fleet-reconcile", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationDispatch, auth.OperationInvoke},
	)
	refreshedFingerprint, err := auth.AuthorizationFingerprint(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if refreshedFingerprint == originalFingerprint {
		t.Fatal("refreshed authorization fingerprint did not change")
	}
	sink := f.clientA.FleetActionInteractionSink(auth.OperationDispatch)
	if _, exposed := sink.(interface {
		RecordReconciledInteractionResult(
			context.Context, interaction.Session,
		) (interaction.Session, error)
	}); exposed {
		t.Fatal("ordinary fleet action sink exposes reconciliation without capability")
	}
	reconciler, ok := sink.(interface {
		RecordReconciledInteractionResult(
			context.Context, explorerfleetcap.Capability, interaction.Session,
		) (interaction.Session, error)
	})
	if !ok {
		t.Fatal("fleet sink does not support durable reconciliation")
	}
	if _, err := reconciler.RecordReconciledInteractionResult(
		f.context(t, refreshed), explorerfleetcap.Capability{}, session,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("zero reconciliation capability = %v", err)
	}
	stored, err := reconciler.RecordReconciledInteractionResult(
		f.context(t, refreshed), explorerfleetcap.New(), session)
	if err != nil {
		t.Fatalf("first-write reconciliation = %v", err)
	}
	if stored.AuthorizationFingerprint !=
		shoal.ID(originalFingerprint.String()) ||
		!stored.AuthorizationExpiresAt.Equal(original.AuthenticationExpires()) ||
		stored.RequestID != original.RequestID() {
		t.Fatalf("durable authorization receipt changed: %#v", stored)
	}

	if _, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///fleet-reconciliation-retry.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "advance the interaction snapshot after receipt commit",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if shoal.ID(current.ID) == session.SnapshotID {
		t.Fatal("interaction snapshot did not advance")
	}
	retry := session
	retry.SnapshotID = shoal.ID(current.ID)
	retry.SnapshotAsOf = current.AsOf
	retried, err := reconciler.RecordReconciledInteractionResult(
		f.context(t, refreshed), explorerfleetcap.New(), retry)
	if err != nil {
		t.Fatalf("authoritative receipt retry = %v", err)
	}
	if !reflect.DeepEqual(retried, stored) {
		t.Fatalf("retry receipt changed: got %#v, want %#v", retried, stored)
	}
}

func TestAuthorizedRecorderSetupRequiresLiveCredential(t *testing.T) {
	f := newFixture(t)
	if err := f.clientA.EnsureInteractionSink(
		context.Background(),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("credential-less recorder setup = %v", err)
	}
	expired := f.decision(
		t, "expired-recorder",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationConnect},
	)
	ctx := f.context(t, expired)
	f.clock.Set(expired.AuthenticationExpires().Add(time.Second))
	if err := f.clientA.EnsureInteractionSink(
		ctx,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("expired recorder setup = %v", err)
	}
}

func TestAuthorizedRecorderSetupSupportsEvidenceEmptyActionOnlyGrant(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "action-only-recorder",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationConnect},
	)
	ctx := f.context(t, decision)
	recorder, err := interaction.NewRecorder(ctx, f.clientA)
	if err != nil {
		t.Fatalf("action-only recorder setup = %v", err)
	}
	if err := recorder.SetClock(f.clock.Now); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	request := interaction.Session{
		ID: interaction.DerivedID(
			"session", "action-only-recorder"),
		Operation:                interaction.OperationToolCall,
		AuthorizationOperation:   string(auth.OperationConnect),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		Turns: []interaction.Turn{{
			Index: 0, Decision: string(auth.OperationConnect),
			ToolCall: &interaction.ToolCall{
				Kind: string(auth.OperationConnect),
			},
		}},
	}
	persisted, err := recorder.Record(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AuthorizationOperation !=
		string(auth.OperationConnect) ||
		persisted.Actor.SubjectID != decision.Subject() ||
		persisted.Actor.ActorID != decision.Actor() ||
		len(persisted.TouchedNodeIDs()) != 0 {
		t.Fatalf("action-only persisted session = %+v", persisted)
	}
}

func TestAuthorizedInteractionPreservesNarrowerExpiry(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "narrow-expiry",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	narrowExpiry := f.clock.Now().Add(5 * time.Minute)
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "narrow-expiry"),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   narrowExpiry,
	}
	persisted, err := f.clientA.RecordInteractionResult(
		f.context(t, decision), session)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.AuthorizationExpiresAt.Equal(narrowExpiry) {
		t.Fatalf("persisted expiry = %v, want %v",
			persisted.AuthorizationExpiresAt, narrowExpiry)
	}
	record, err := f.base.InteractionRecord(
		context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Session.AuthorizationExpiresAt.Equal(narrowExpiry) ||
		!record.Summary.AuthorizationExpiresAt.Equal(narrowExpiry) {
		t.Fatalf("durable expiry = session %v summary %v, want %v",
			record.Session.AuthorizationExpiresAt,
			record.Summary.AuthorizationExpiresAt,
			narrowExpiry,
		)
	}
}

func TestAuthorizedInteractionEnrichesTrustedActorDelegationAndReason(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///actor-context.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "actor context evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:             "human-subject",
		Actor:               "agent-actor",
		ClientID:            "mcp-client",
		OnBehalfOf:          []shoal.ID{"fleet", "delegating-agent"},
		AuthorizationDomain: f.domain,
		AllowedOperations: []auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
		},
		PermittedSourceIDs:    [][]byte{f.sourceA},
		PermittedPolicyIDs:    [][]byte{f.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: f.clock.Now().Add(time.Hour),
		RequestID:             "actor-context-request",
		AuditPurpose:          "fulfill grounded retrieval request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.context(t, decision)
	view, err := f.clientA.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	spoofedReason, err := interaction.NewReason(
		"spoofed_reason", "caller-controlled explanation")
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         "interaction.session_actor-context",
		RecordedAt: f.clock.Now(),
		Operation:  interaction.OperationRetrieval,
		Actor: interaction.ActorContext{
			SubjectID: "spoofed-subject",
			ActorID:   "spoofed-actor",
		},
		Reason:                   spoofedReason,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	recorder, err := interaction.NewRecorder(ctx, f.clientA)
	if err != nil {
		t.Fatal(err)
	}
	returned, err := recorder.Record(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if returned.Actor.SubjectID != decision.Subject() ||
		returned.Actor.ActorID != decision.Actor() ||
		returned.Actor.ClientID != decision.ClientID() ||
		len(returned.Actor.OnBehalfOf) != 2 ||
		returned.Reason.Code != "audit_purpose" ||
		returned.Reason.Digest !=
			interaction.Digest(decision.AuditPurpose()) {
		t.Fatalf("recorder returned untrusted metadata = %+v", returned)
	}
	hydrated, err := f.base.Interaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.Actor.SubjectID != decision.Subject() ||
		hydrated.Actor.ActorID != decision.Actor() ||
		hydrated.Actor.ClientID != decision.ClientID() ||
		len(hydrated.Actor.OnBehalfOf) != 2 ||
		hydrated.Actor.OnBehalfOf[0] != "fleet" ||
		hydrated.Actor.OnBehalfOf[1] != "delegating-agent" ||
		hydrated.Reason.Code != "audit_purpose" ||
		hydrated.Reason.Digest !=
			interaction.Digest(decision.AuditPurpose()) {
		t.Fatalf("trusted actor enrichment = %+v", hydrated)
	}
	subgraph, err := f.base.InteractionSubgraph(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var properties shoal.Metadata
	for _, node := range subgraph.Nodes {
		if node.Kind == interaction.KindSession {
			properties = node.Properties
			break
		}
	}
	if properties[interaction.PropertySubjectID] != "human-subject" ||
		properties[interaction.PropertyActorID] != "agent-actor" ||
		properties[interaction.PropertyClientID] != "mcp-client" ||
		properties[interaction.PropertyDelegationCount] != "2" ||
		properties[interaction.PropertyDelegationID] == "" ||
		properties[interaction.PropertyReasonCode] != "audit_purpose" ||
		properties[interaction.PropertyReasonDigest] == "" {
		t.Fatalf("actor graph properties = %+v", properties)
	}
	for _, value := range properties {
		if strings.Contains(value, decision.AuditPurpose()) {
			t.Fatal("raw audit purpose entered the interaction graph")
		}
	}
}

func TestAuthorizedInteractionRecorderRejectsWrongPin(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///authorized-interaction.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "authorized interaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"recorder",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	ctx := f.context(t, decision)
	view, err := f.clientA.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_wrong-pin",
		RecordedAt:               f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: "auth-sha256:wrong",
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if err := f.clientA.RecordInteraction(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("wrong authorization pin record = %v", err)
	}
	if _, err := f.base.Interaction(
		ctx, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("rejected interaction was persisted: %v", err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session.ID = "interaction.session_wrong-snapshot"
	session.AuthorizationFingerprint = shoal.ID(fingerprint.String())
	session.SnapshotID = "forged-snapshot"
	if err := f.clientA.RecordInteraction(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged snapshot record = %v", err)
	}
	session.ID = "interaction.session_expired-pin"
	session.SnapshotID = shoal.ID(snapshot.ID)
	session.SnapshotAsOf = snapshot.AsOf
	session.RecordedAt = snapshot.AsOf
	session.AuthorizationExpiresAt = snapshot.AsOf.Add(500 * time.Millisecond)
	if err := f.clientA.RecordInteraction(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("expired authorization pin record = %v", err)
	}
}

func TestAuthorizedInteractionAcceptsTrustedHistoricalSnapshot(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///historical-interaction.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "historical interaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.clientA.Document(
		f.alice(t), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///unrelated-later.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "unrelated later publication",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.ID == snapshot.ID {
		t.Fatal("unrelated publication did not advance the snapshot")
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "historical-recorder",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "historical-snapshot"),
		RecordedAt: f.clock.Now(), Operation: interaction.OperationRetrieval,
		SnapshotID: shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if err := f.clientA.RecordInteraction(
		f.context(t, decision), session,
	); err != nil {
		t.Fatalf("trusted historical snapshot was rejected: %v", err)
	}
}

func TestAuthorizedNewWriteRechecksSnapshotAfterConcurrentIngest(t *testing.T) {
	f := newFixture(t)
	source := explorer.Source{
		URI:       "file:///snapshot-race.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "original evidence",
	}
	receipt, err := f.clientA.Ingest(f.admin(t), source)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "snapshot-race",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead, auth.OperationRetrieve},
	)
	view, err := f.clientA.Document(
		f.context(t, decision), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	base := &mutatingInteractionSnapshotBase{Explorer: f.base}
	base.mutate = func() error {
		_, mutateErr := f.base.Ingest(context.Background(), explorer.Source{
			URI:       source.URI,
			MediaType: source.MediaType,
			Content:   source.Content,
			Metadata: shoal.Metadata{
				interaction.PropertyVisibility: "restricted",
			},
		})
		return mutateErr
	}
	client := f.newClient(
		t, base, f.store, f.sourceA, f.policyA, nil)
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "snapshot-race"),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if _, err := client.RecordInteractionResult(
		f.context(t, decision), session,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("stale snapshot write error = %v", err)
	}
	if _, err := f.base.InteractionRecord(
		context.Background(), session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("stale snapshot write persisted a record: %v", err)
	}
}

func TestAuthorizedExactRetryUsesTrustedDurableRecord(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "retry-recorder",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "authorized-retry"),
		RecordedAt: snapshot.AsOf.Add(-time.Hour),
		Operation:  interaction.OperationRetrieval,
		SnapshotID: shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	ctx := f.context(t, decision)
	first, err := f.clientA.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RecordedAt.Equal(f.clock.Now()) {
		t.Fatalf("accepted caller timestamp %v", first.RecordedAt)
	}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	validator := &rejectingSnapshotValidator{}
	retryClient, err := authorized.NewClient(authorized.Config{
		Base: f.base, VectorScorer: f.base,
		InteractionWriter: f.base, InteractionReader: f.base,
		SnapshotValidator: validator,
		Resolver:          f.authority.Resolver(), PolicySelector: selector,
		PolicyStore: f.store, GenerationReader: f.reader, Clock: f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(f.clock.Now().Add(time.Second))
	session.RecordedAt = snapshot.AsOf.Add(24 * time.Hour)
	retried, err := retryClient.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatalf("exact durable retry was rejected: %v", err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("retry result differs: got %+v want %+v", retried, first)
	}
	if validator.calls != 0 {
		t.Fatalf("exact retry consulted snapshot validator %d times",
			validator.calls)
	}
}

func TestAuthorizedExactRetryRejectsStricterRequiredVisibility(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "retry-visibility",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID: interaction.DerivedID(
			"session", "authorized-retry-visibility"),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	firstContext, err := interaction.WithRequiredVisibility(
		f.context(t, decision), []string{"restricted"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.clientA.RecordInteractionResult(firstContext, session)
	if err != nil {
		t.Fatal(err)
	}

	f.clock.Set(f.clock.Now().Add(time.Second))
	retryContext, err := interaction.WithRequiredVisibility(
		f.context(t, decision), []string{"restricted", "sensitive"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.RecordInteractionResult(
		retryContext, session,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("stricter visibility retry error = %v", err)
	}

	record, err := f.base.InteractionRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Summary.Visibility != "restricted" ||
		record.Session.ID != first.ID ||
		!record.Session.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("stricter retry changed durable record: %+v", record)
	}
}

func TestAuthorizedResultSinkExactRetryAfterReopen(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTime := snapshot.AsOf.Add(time.Second)
	f.clock.Set(firstTime)
	decision := f.decision(
		t, "restart-retry",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "authorized-restart-retry"),
		RecordedAt:               snapshot.AsOf.Add(-time.Hour),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	ctx := f.context(t, decision)
	recorder, err := interaction.NewRecorder(ctx, f.clientA)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(f.clock.Now); err != nil {
		t.Fatal(err)
	}
	first, err := recorder.Record(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RecordedAt.Equal(firstTime) {
		t.Fatalf("first accepted time = %v", first.RecordedAt)
	}
	if err := f.base.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	retryClient := f.newClient(
		t, reopened, f.store, f.sourceA, f.policyA, nil)
	retryRecorder, err := interaction.NewRecorder(ctx, retryClient)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(firstTime.Add(time.Minute))
	if err := retryRecorder.SetClock(f.clock.Now); err != nil {
		t.Fatal(err)
	}
	session.RecordedAt = firstTime.Add(24 * time.Hour)
	retried, err := retryRecorder.Record(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("reopened retry = %+v, want %+v", retried, first)
	}
	records, err := reopened.InteractionRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("durable record count = %d", len(records))
	}
}

func TestAuthorizedResultSinkAdoptsConcurrentRetryWinner(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTime := snapshot.AsOf.Add(time.Second)
	f.clock.Set(firstTime)
	decision := f.decision(
		t, "concurrent-retry",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "concurrent-retry"),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	ctx := f.context(t, decision)
	first, err := f.clientA.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(firstTime.Add(time.Minute))
	base := &hiddenFirstInteractionRecordBase{Explorer: f.base}
	retryClient := f.newClient(
		t, base, f.store, f.sourceA, f.policyA, nil)
	retried, err := retryClient.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatalf("concurrent durable winner was rejected: %v", err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("concurrent retry = %+v, want %+v", retried, first)
	}
}

func TestAuthorizedInteractionMarksPostSinkExpiryCommitted(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "post-sink-expiry",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	base := &generationChangingInteractionBase{Explorer: f.base}
	base.after = func() {
		f.clock.Set(decision.AuthenticationExpires())
	}
	client := f.newClient(
		t, base, f.store, f.sourceA, f.policyA, nil)
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "post-sink-expiry"),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	recorded, err := client.RecordInteractionResult(
		f.context(t, decision), session)
	if !explorer.IsCommittedInteraction(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("post-sink expiry error = %v", err)
	}
	if recorded.ID != session.ID {
		t.Fatalf("committed session = %+v", recorded)
	}
	if _, err := f.base.InteractionRecord(
		context.Background(), session.ID); err != nil {
		t.Fatalf("committed record unavailable: %v", err)
	}
}

func TestAuthorizedTombstoneSubgraphDoesNotLeakExistence(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///deleted-interaction.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "deleted interaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"deletion-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	ctx := f.context(t, decision)
	view, err := f.clientA.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_deleted-authorized",
		RecordedAt:               f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if err := f.clientA.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := f.base.DeleteInteraction(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	subgraph, err := f.clientA.InteractionSubgraph(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subgraph.Nodes) != 1 ||
		subgraph.Nodes[0].Kind != interaction.KindTombstone {
		t.Fatalf("authorized tombstone subgraph = %+v", subgraph)
	}
	renewed, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               decision.Subject(),
		Actor:                 decision.Actor(),
		ClientID:              decision.ClientID(),
		OnBehalfOf:            decision.OnBehalfOf(),
		AuthorizationDomain:   decision.AuthorizationDomain(),
		AllowedOperations:     decision.AllowedOperations(),
		PermittedSourceIDs:    decision.PermittedSourceIDs(),
		PermittedPolicyIDs:    decision.PermittedPolicyIDs(),
		PolicyGeneration:      decision.PolicyGeneration(),
		AuthenticationExpires: f.clock.Now().Add(30 * time.Minute),
		RequestID:             "renewed-deletion-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.InteractionSubgraph(
		f.context(t, renewed), session.ID,
	); err != nil {
		t.Fatalf("renewed shorter credential cannot read tombstone: %v", err)
	}
	if _, err := f.clientA.Interaction(
		ctx, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("typed deleted interaction read = %v", err)
	}

	bobDecision := f.decision(
		t,
		"other-reader",
		[][]byte{f.sourceB},
		[][]byte{f.policyB},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	if _, err := f.clientB.InteractionSubgraph(
		f.context(t, bobDecision), session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized tombstone read leaked existence: %v", err)
	}
}

func TestAuthorizedInteractionMarksPostCommitGenerationFailure(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///post-commit-generation.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "post commit generation evidence",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"generation-recorder",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	ctx := f.context(t, decision)
	view, err := f.clientA.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	base := &generationChangingInteractionBase{
		Explorer: f.base,
		after: func() {
			f.reader.Set(f.domain, 2)
		},
	}
	client := f.newClient(
		t, base, f.store, f.sourceA, f.policyA, nil)
	recorder, err := interaction.NewRecorder(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_post-commit-generation",
		RecordedAt:               f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if _, err := recorder.Record(
		ctx, session,
	); !explorer.IsCommittedInteraction(err) {
		t.Fatalf("post-commit generation error = %v", err)
	}
	if _, err := f.base.Interaction(
		context.Background(), session.ID,
	); err != nil {
		t.Fatalf("committed interaction was not durable: %v", err)
	}
}

func TestAuthorizedInteractionRejectsForgedSinkResult(t *testing.T) {
	f := newFixture(t)
	wrapped := &forgedResultInteractionBase{Explorer: f.base}
	client := f.newClient(
		t, wrapped, f.store, f.sourceA, f.policyA, nil)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"trusted-result",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationRetrieve},
	)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_forged-result",
		RecordedAt:               f.clock.Now(),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	ctx := f.context(t, decision)
	if _, err := client.RecordInteractionResult(
		ctx, session,
	); !explorer.IsCommittedInteraction(err) {
		t.Fatalf("forged sink result error = %v", err)
	}
	stored, err := f.base.Interaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Actor.SubjectID != decision.Subject() {
		t.Fatalf("stored actor was forged: %+v", stored.Actor)
	}
}

func TestAuthorizedInteractionReadsUseBulkAndPointPaths(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///bulk-interactions.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "bulk interaction evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"bulk-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	ctx := f.context(t, decision)
	view, err := f.clientA.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []shoal.ID{"interaction.session_bulk-a", "interaction.session_bulk-b"} {
		if err := f.clientA.RecordInteraction(ctx, interaction.Session{
			ID:                       id,
			RecordedAt:               f.clock.Now(),
			SnapshotID:               shoal.ID(snapshot.ID),
			SnapshotAsOf:             snapshot.AsOf,
			AuthorizationFingerprint: shoal.ID(fingerprint.String()),
			AuthorizationExpiresAt:   decision.AuthenticationExpires(),
			SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	store := &countingInteractionStore{PolicyStore: f.store}
	base := &countingInteractionBase{Explorer: f.base}
	client := f.newClient(t, base, store, f.sourceA, f.policyA, nil)
	records, err := client.InteractionRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || store.nodesCalls != 1 ||
		base.recordsCalls != 1 {
		t.Fatalf(
			"records=%d node_batches=%d bulk_reads=%d",
			len(records), store.nodesCalls, base.recordsCalls,
		)
	}
	if _, err := client.InteractionSubgraph(
		ctx, "interaction.session_bulk-a",
	); err != nil {
		t.Fatal(err)
	}
	if base.recordCalls != 1 || base.recordsCalls != 1 {
		t.Fatalf("point_reads=%d bulk_reads=%d",
			base.recordCalls, base.recordsCalls)
	}
}

func TestSourceLessInteractionRequiresOriginalAuthorizationProjection(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	ownerDecision := f.decision(
		t,
		"source-less-owner",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	owner := f.context(t, ownerDecision)
	fingerprint, err := auth.AuthorizationFingerprint(ownerDecision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_source-less",
		RecordedAt:               f.clock.Now(),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   ownerDecision.AuthenticationExpires(),
	}
	if err := f.clientA.RecordInteraction(owner, session); err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.Interaction(owner, session.ID); err != nil {
		t.Fatalf("owner cannot read source-less interaction: %v", err)
	}

	otherDecision := f.decision(
		t,
		"source-less-other",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationValidate,
		},
	)
	other := f.context(t, otherDecision)
	if _, err := f.clientA.Interaction(
		other, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("source-less point read leaked across projections: %v", err)
	}
	if _, err := f.clientA.InteractionSubgraph(
		other, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("source-less subgraph leaked across projections: %v", err)
	}
	summaries, err := f.clientA.Interactions(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("source-less list leaked across projections: %+v", summaries)
	}
}

func TestInteractionReadsRequireExplicitTrustedReader(t *testing.T) {
	f := newFixture(t)
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:             f.base,
		VectorScorer:     f.base,
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := f.decision(
		t,
		"interaction-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead},
	)
	if _, err := client.InteractionRecords(
		f.context(t, decision),
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("implicit base interaction reader error = %v", err)
	}
	if err := client.EnsureInteractionSink(
		f.context(t, decision),
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("implicit base interaction writer error = %v", err)
	}
}

type syntheticInteractionBase struct {
	*explorer.Explorer
	records []explorer.InteractionRecord
}

func (b *syntheticInteractionBase) InteractionRecords(
	context.Context,
) ([]explorer.InteractionRecord, error) {
	return b.records, nil
}

type batchBoundedStore struct {
	authorized.PolicyStore
	nodeCalls    int
	largestBatch int
}

func (s *batchBoundedStore) Nodes(
	ctx context.Context, ids []shoal.ID,
) (map[shoal.ID]authorized.NodeRegistration, error) {
	s.nodeCalls++
	if len(ids) > s.largestBatch {
		s.largestBatch = len(ids)
	}
	return s.PolicyStore.Nodes(ctx, ids)
}

// TestAuthorizedInteractionListAuthorizesBoundedBatches pins the read-path
// bound: interaction provenance is intentionally uncapped per record, so a
// list must never submit the union of the whole durable history in one
// policy-store lookup. Authorization is a conjunction, so batching cannot
// change the fail-closed outcome.
func TestAuthorizedInteractionListAuthorizesBoundedBatches(t *testing.T) {
	f := newFixture(t)
	const (
		records       = 8
		nodesPerBatch = 500
		bound         = 1024
	)
	synthetic := make([]explorer.InteractionRecord, 0, records)
	for record := 0; record < records; record++ {
		nodeIDs := make([]shoal.ID, 0, nodesPerBatch)
		for node := 0; node < nodesPerBatch; node++ {
			nodeIDs = append(nodeIDs, shoal.ID(
				"unregistered-"+strconv.Itoa(record)+"-"+strconv.Itoa(node)))
		}
		synthetic = append(synthetic, explorer.InteractionRecord{
			Summary: explorer.InteractionSummary{
				SessionID: shoal.ID(
					"interaction.session_bounded-" + strconv.Itoa(record)),
			},
			TouchedNodeIDs: nodeIDs,
		})
	}
	// One record whose own provenance exceeds the bound proves a single
	// uncapped record is chunked rather than submitted whole.
	oversized := make([]shoal.ID, 0, 2*bound)
	for node := 0; node < 2*bound; node++ {
		oversized = append(
			oversized, shoal.ID("unregistered-large-"+strconv.Itoa(node)))
	}
	synthetic = append(synthetic, explorer.InteractionRecord{
		Summary: explorer.InteractionSummary{
			SessionID: "interaction.session_bounded-large",
		},
		TouchedNodeIDs: oversized,
	})
	base := &syntheticInteractionBase{Explorer: f.base, records: synthetic}
	store := &batchBoundedStore{PolicyStore: f.store}
	client := f.newClient(t, base, store, f.sourceA, f.policyA, nil)
	decision := f.decision(
		t,
		"bounded-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead},
	)
	visible, err := client.InteractionRecords(f.context(t, decision))
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("unregistered provenance was authorized: %d", len(visible))
	}
	if store.largestBatch > bound {
		t.Fatalf("policy lookup batch = %d, want at most %d",
			store.largestBatch, bound)
	}
	if store.nodeCalls < records*nodesPerBatch/bound {
		t.Fatalf("node lookups = %d, want the list split into batches",
			store.nodeCalls)
	}
}
