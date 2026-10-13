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

// Interactions, folds and application edges are bound to the revisions they
// were derived from (#695): an authorized relabel that removes a label must
// not open what was derived from the labelled revision.

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	bindingClosedURI = "file:///binding/closed.txt"
	bindingOpenURI   = "file:///binding/open.txt"

	bindingSession       shoal.ID = "interaction.session_binding_labelled"
	bindingOpenSession   shoal.ID = "interaction.session_binding_open"
	bindingLaterSession  shoal.ID = "interaction.session_binding_later"
	bindingNeverSession  shoal.ID = "interaction.session_binding_never"
	bindingEdge          shoal.ID = "edge-binding-labelled"
	bindingLaterEdge     shoal.ID = "edge-binding-later"
	bindingUnrelatedEdge shoal.ID = "edge-binding-unrelated"
	bindingNeverFold     shoal.ID = "interaction.fold_binding_never"
)

// bindingWorld is the issue's reproduction: a holder of label "secret"
// ingests a labelled document, records a session over it, folds it, and
// connects an edge to it; then relabels the document with new, unlabelled
// content. reader holds only the source policy.
type bindingWorld struct {
	t        *testing.T
	f        *fixture
	store    authorized.PolicyStore
	client   *authorized.Client
	holder   auth.Decision
	reader   auth.Decision
	closed   explorer.IngestResult
	open     explorer.IngestResult
	snapshot explorer.Snapshot
	fold     explorer.FoldResult
}

func (f *fixture) bindingDecision(
	t testing.TB, subject string, labels ...string,
) auth.Decision {
	t.Helper()
	policies := [][]byte{f.policyA, f.policyB}
	for _, label := range labels {
		policies = append(policies, labelPolicy(t, f.sourceA, label))
	}
	return f.decision(t, subject,
		[][]byte{f.sourceA, f.sourceB}, policies, labelReaderOperations)
}

func newBindingWorld(
	t *testing.T, f *fixture, store authorized.PolicyStore,
) *bindingWorld {
	t.Helper()
	w := &bindingWorld{
		t: t, f: f, store: store,
		holder: f.bindingDecision(t, "holder", "secret"),
		reader: f.bindingDecision(t, "reader"),
	}
	w.client = w.newClient(store)
	holder := w.as(w.holder)
	var err error
	w.closed, err = w.client.Ingest(holder,
		labelledSource(bindingClosedURI, "the secret plan", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	w.open, err = w.client.Ingest(holder,
		labelledSource(bindingOpenURI, "an open note", ""))
	if err != nil {
		t.Fatal(err)
	}
	w.resnapshot()
	return w
}

// resnapshot takes a fresh snapshot and moves the clock just past it, as a
// recorder does, minting both principals' decisions at that time.
func (w *bindingWorld) resnapshot() {
	w.t.Helper()
	var err error
	w.snapshot, err = w.client.Snapshot(w.as(w.holder))
	if err != nil {
		w.t.Fatal(err)
	}
	w.f.clock.Set(w.snapshot.AsOf.Add(time.Second))
	w.holder = w.f.bindingDecision(w.t, "holder", "secret")
	w.reader = w.f.bindingDecision(w.t, "reader")
}

func (w *bindingWorld) newClient(store authorized.PolicyStore) *authorized.Client {
	selector := w.f.staticSelector(w.t, w.f.sourceA, w.f.policyA)
	return w.f.labelClient(w.t, w.f.base, store, selector, selector)
}

func (w *bindingWorld) as(decision auth.Decision) context.Context {
	return w.f.context(w.t, decision)
}

// record writes a session as the holder that was shown seeds.
func (w *bindingWorld) record(
	client *authorized.Client, id shoal.ID, seeds ...shoal.ID,
) {
	w.t.Helper()
	if err := w.tryRecord(client, id, seeds...); err != nil {
		w.t.Fatalf("record %s: %v", id, err)
	}
}

func (w *bindingWorld) tryRecord(
	client *authorized.Client, id shoal.ID, seeds ...shoal.ID,
) error {
	w.t.Helper()
	fingerprint, err := auth.AuthorizationFingerprint(w.holder)
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = client.RecordInteractionResult(w.as(w.holder), interaction.Session{
		ID:                       id,
		RecordedAt:               w.f.clock.Now().UTC(),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(w.snapshot.ID),
		SnapshotAsOf:             w.snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   w.holder.AuthenticationExpires(),
		SeedNodeIDs:              seeds,
	})
	return err
}

func (w *bindingWorld) connect(
	client *authorized.Client, id, from, to shoal.ID,
) {
	w.t.Helper()
	if err := client.Connect(w.as(w.holder), graph.Edge{
		ID: id, From: from, To: to, Type: "references", Weight: 1,
	}); err != nil {
		w.t.Fatalf("connect %s: %v", id, err)
	}
}

// relabel re-ingests the labelled URI with entirely new, unlabelled content,
// with a reason: the relabel the declassification audit records (#630).
func (w *bindingWorld) relabel(client *authorized.Client) explorer.IngestResult {
	w.t.Helper()
	ctx, err := authorized.WithDeclassificationReason(
		w.as(w.holder), "ticket:SEC-695")
	if err != nil {
		w.t.Fatal(err)
	}
	relabelled, err := client.Ingest(ctx,
		labelledSource(bindingClosedURI, "nothing to see any more", ""))
	if err != nil {
		w.t.Fatal(err)
	}
	if relabelled.Document.ID != w.closed.Document.ID ||
		relabelled.Revision.ID == w.closed.Revision.ID {
		w.t.Fatalf("relabel did not make a new revision of the same document: %+v",
			relabelled)
	}
	return relabelled
}

// setup records the session and fold and connects the edge over the
// labelled revision, then relabels it.
func (w *bindingWorld) setup(client *authorized.Client) explorer.IngestResult {
	w.t.Helper()
	w.record(client, bindingSession, w.closed.Document.ID)
	w.record(client, bindingOpenSession, w.open.Document.ID)
	var err error
	w.fold, err = client.FoldInteractions(w.as(w.holder),
		explorer.FoldRequest{SessionIDs: []shoal.ID{bindingSession}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.connect(client, bindingEdge, w.open.Document.ID, w.closed.Document.ID)
	// The probes are not vacuous: before the relabel the reader is denied
	// and the holder reads.
	w.requireDerivedDenied(client, "before the relabel")
	w.requireHolderReads(client)
	if _, err := client.RehydrateFold(w.as(w.holder), w.fold.FoldID); err != nil {
		w.t.Fatalf("holder RehydrateFold: %v", err)
	}
	return w.relabel(client)
}

func (w *bindingWorld) neverRecord(client *authorized.Client) error {
	_, err := client.InteractionRecord(w.as(w.reader), bindingNeverSession)
	return err
}

// requireDerivedDenied: the reader cannot reach the session, its fold or the
// edge, each refused exactly as an object that was never written is.
func (w *bindingWorld) requireDerivedDenied(client *authorized.Client, when string) {
	w.t.Helper()
	reader := w.as(w.reader)
	never := w.neverRecord(client)
	_, err := client.InteractionRecord(reader, bindingSession)
	assertSameRefusal(w.t, when+": InteractionRecord", err, never)
	_, err = client.Interaction(reader, bindingSession)
	_, neverInteraction := client.Interaction(reader, bindingNeverSession)
	assertSameRefusal(w.t, when+": Interaction", err, neverInteraction)
	_, err = client.InteractionSubgraph(reader, bindingSession)
	_, neverSubgraph := client.InteractionSubgraph(reader, bindingNeverSession)
	assertSameRefusal(w.t, when+": InteractionSubgraph", err, neverSubgraph)
	records, err := client.InteractionRecords(reader)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, record := range records {
		if record.Summary.SessionID == bindingSession {
			w.t.Fatalf("%s: InteractionRecords lists the session", when)
		}
	}
	if w.fold.FoldID != "" {
		_, err = client.RehydrateFold(reader, w.fold.FoldID)
		_, neverFold := client.RehydrateFold(reader, bindingNeverFold)
		assertSameRefusal(w.t, when+": RehydrateFold", err, neverFold)
		folds, err := client.Folds(reader)
		if err != nil {
			w.t.Fatal(err)
		}
		for _, fold := range folds {
			if fold.FoldID == w.fold.FoldID {
				w.t.Fatalf("%s: Folds lists the fold", when)
			}
		}
	}
	if w.edgeVisible(client, w.reader, bindingEdge) {
		w.t.Fatalf("%s: the reader sees the edge", when)
	}
}

func (w *bindingWorld) edgeVisible(
	client *authorized.Client, decision auth.Decision, edgeID shoal.ID,
) bool {
	w.t.Helper()
	around, err := client.Neighborhood(w.as(decision), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{w.open.Document.ID}, Depth: 1,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, edge := range around.Edges {
		if edge.ID == edgeID {
			return true
		}
	}
	return false
}

// requireHolderReads: the label holder still reads everything.
func (w *bindingWorld) requireHolderReads(client *authorized.Client) {
	w.t.Helper()
	holder := w.as(w.holder)
	for _, id := range []shoal.ID{bindingSession, bindingOpenSession} {
		if _, err := client.InteractionRecord(holder, id); err != nil {
			w.t.Fatalf("holder InteractionRecord(%s): %v", id, err)
		}
		if _, err := client.InteractionSubgraph(holder, id); err != nil {
			w.t.Fatalf("holder InteractionSubgraph(%s): %v", id, err)
		}
	}
	records, err := client.InteractionRecords(holder)
	if err != nil {
		w.t.Fatal(err)
	}
	if !listsSession(records, bindingSession) {
		w.t.Fatal("holder InteractionRecords omits the session")
	}
	// The fold is not checked here: its stored expression carries the
	// free-form "secret" its member was stamped with, which no current
	// registration translates once the label is removed, so after the
	// relabel it is hidden from every reader by its own labels (#570),
	// before and independently of the revision binding. setup checks the
	// holder reads it while the label stands.
	if !w.edgeVisible(client, w.holder, bindingEdge) {
		w.t.Fatal("holder does not see the edge")
	}
}

func listsSession(records []explorer.InteractionRecord, id shoal.ID) bool {
	for _, record := range records {
		if record.Summary.SessionID == id {
			return true
		}
	}
	return false
}

// TestRelabelKeepsDerivedOutputsClosed is the issue's reproduction for the
// interaction, its fold and an application edge.
func TestRelabelKeepsDerivedOutputsClosed(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		w.setup(w.client)
		reader := w.as(w.reader)
		// The labelled revision itself stays closed, as intended.
		if _, err := w.client.Document(reader, w.closed.Document.ID,
			w.closed.Revision.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("Document(R1) = %v, want not_found", err)
		}
		// The current revision opened: the relabel did remove the label.
		if _, err := w.client.Document(reader, w.closed.Document.ID, ""); err != nil {
			t.Fatalf("Document(current) = %v, want readable", err)
		}
		w.requireDerivedDenied(w.client, "after the relabel")
		w.requireHolderReads(w.client)
		// The recorded binding alone closes the session: read through a
		// catalog that treats a missing binding as open, so an incomplete
		// or missing recording would show here.
		open := w.newClient(absentBindingIsOpen{PolicyStore: store})
		_, err := open.InteractionRecord(reader, bindingSession)
		assertSameRefusal(t, "recorded binding alone", err, w.neverRecord(open))
	})
}

// absentBindingIsOpen reports every session as bound, with no revisions when
// nothing was recorded, so a reader through it gets no conservative legacy
// rule: only what the recording bound.
type absentBindingIsOpen struct {
	authorized.PolicyStore
}

func (s absentBindingIsOpen) InteractionRevisions(
	ctx context.Context, sessionIDs []shoal.ID,
) (map[shoal.ID][]authorized.RevisionReference, error) {
	recorded, err := s.PolicyStore.InteractionRevisions(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	for _, sessionID := range sessionIDs {
		if _, ok := recorded[sessionID]; !ok {
			recorded[sessionID] = nil
		}
	}
	return recorded, nil
}

// TestRevisionBindingLeavesUnrelatedReadsAlone: a session and an edge over
// documents that were never labelled, and a session and an edge made over
// the relabelled document's new revision, read exactly as before.
func TestRevisionBindingLeavesUnrelatedReadsAlone(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		w.setup(w.client)
		reader := w.as(w.reader)
		if _, err := w.client.InteractionRecord(reader, bindingOpenSession); err != nil {
			t.Fatalf("an unrelated session is denied: %v", err)
		}
		other, err := w.client.Ingest(w.as(w.holder),
			labelledSource("file:///binding/other.txt", "another note", ""))
		if err != nil {
			t.Fatal(err)
		}
		w.connect(w.client, bindingUnrelatedEdge, w.open.Document.ID, other.Document.ID)
		if !w.edgeVisible(w.client, w.reader, bindingUnrelatedEdge) {
			t.Fatal("an unrelated edge is denied")
		}
		// Made over the new, unlabelled revision: bound to it, not to R1.
		w.resnapshot()
		reader = w.as(w.reader)
		w.record(w.client, bindingLaterSession, w.closed.Document.ID)
		if _, err := w.client.InteractionRecord(reader, bindingLaterSession); err != nil {
			t.Fatalf("a session over the unlabelled revision is denied: %v", err)
		}
		w.connect(w.client, bindingLaterEdge, w.open.Document.ID, w.closed.Document.ID)
		if !w.edgeVisible(w.client, w.reader, bindingLaterEdge) {
			t.Fatal("an edge connected to the unlabelled revision is denied")
		}
	})
}

// preBindingStore records what a binary from before #695 records: no
// interaction revisions, and edges without a revision binding.
type preBindingStore struct {
	authorized.PolicyStore
}

func (preBindingStore) PutInteractionRevisions(
	context.Context, shoal.ID, []authorized.RevisionReference,
) error {
	return nil
}

func unbound(registration authorized.EdgeRegistration) authorized.EdgeRegistration {
	registration.SourceRevisions = nil
	registration.SourceRevisionsBound = false
	return registration
}

func (s preBindingStore) ReserveEdge(
	ctx context.Context, registration authorized.EdgeRegistration,
) error {
	return s.PolicyStore.ReserveEdge(ctx, unbound(registration))
}

func (s preBindingStore) RollbackEdgeReservation(
	ctx context.Context, registration authorized.EdgeRegistration,
) error {
	return s.PolicyStore.RollbackEdgeReservation(ctx, unbound(registration))
}

func (s preBindingStore) PutEdge(
	ctx context.Context, registration authorized.EdgeRegistration,
) error {
	return s.PolicyStore.PutEdge(ctx, unbound(registration))
}

// TestLegacyDerivedOutputsAreHeldToEveryRevision: a session and an edge
// recorded before the binding existed carry no revisions, so they are held to
// every revision of the document they reach. The relabel's leak stays closed
// for them too, and the conservative rule also closes outputs made over the
// unlabelled revision, which a bound output does not.
func TestLegacyDerivedOutputsAreHeldToEveryRevision(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		legacy := w.newClient(preBindingStore{PolicyStore: store})
		w.setup(legacy)
		if got, err := store.InteractionRevisions(context.Background(),
			[]shoal.ID{bindingSession}); err != nil || len(got) != 0 {
			t.Fatalf("the legacy session recorded revisions: %v, %v", got, err)
		}
		w.requireDerivedDenied(w.client, "legacy, after the relabel")
		w.requireHolderReads(w.client)
		reader := w.as(w.reader)
		// Unlabelled throughout: the conservative rule changes nothing.
		if _, err := w.client.InteractionRecord(reader, bindingOpenSession); err != nil {
			t.Fatalf("a legacy session over an open document is denied: %v", err)
		}
		// Made over the unlabelled revision, but nothing records that.
		w.resnapshot()
		reader = w.as(w.reader)
		w.record(legacy, bindingLaterSession, w.closed.Document.ID)
		_, err := w.client.InteractionRecord(reader, bindingLaterSession)
		assertSameRefusal(t, "legacy session over R2", err, w.neverRecord(w.client))
		w.connect(legacy, bindingLaterEdge, w.open.Document.ID, w.closed.Document.ID)
		if w.edgeVisible(w.client, w.reader, bindingLaterEdge) {
			t.Fatal("a legacy edge to a once-labelled document is visible")
		}
		if !w.edgeVisible(w.client, w.holder, bindingLaterEdge) {
			t.Fatal("the holder does not see the legacy edge")
		}
	})
}

// TestLegacyOutputsAreHeldOnlyToWhatExistedWhenMade: a session recorded
// before the binding is held to the revisions published by its snapshot (by
// publication sequence) and the current one, so a label added and removed
// after it was made does not close it. One made while the label stood stays
// closed. A legacy edge has no such bound and counts every revision, and
// without a trusted publication reader every revision counts for a session
// too.
func TestLegacyOutputsAreHeldOnlyToWhatExistedWhenMade(t *testing.T) {
	const laterURI = "file:///binding/later.txt"
	const (
		earlySession shoal.ID = "interaction.session_binding_early"
		labelSession shoal.ID = "interaction.session_binding_during_label"
		earlyEdge    shoal.ID = "edge-binding-early"
	)
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		legacy := w.newClient(preBindingStore{PolicyStore: store})
		holder := w.as(w.holder)
		later, err := w.client.Ingest(holder, labelledSource(laterURI, "open at first", ""))
		if err != nil {
			t.Fatal(err)
		}
		w.resnapshot()
		w.record(legacy, earlySession, later.Document.ID)
		w.connect(legacy, earlyEdge, w.open.Document.ID, later.Document.ID)
		holder = w.as(w.holder)
		if _, err := w.client.Ingest(holder,
			labelledSource(laterURI, "now secret", "secret")); err != nil {
			t.Fatal(err)
		}
		w.resnapshot()
		w.record(legacy, labelSession, later.Document.ID)
		ctx, err := authorized.WithDeclassificationReason(w.as(w.holder), "ticket:SEC-695")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.client.Ingest(ctx,
			labelledSource(laterURI, "open again", "")); err != nil {
			t.Fatal(err)
		}
		reader := w.as(w.reader)
		if _, err := w.client.InteractionRecord(reader, earlySession); err != nil {
			t.Fatalf("a legacy session made before the label is denied: %v", err)
		}
		// An edge's Connect time is not compared with revision
		// publication times (a stepped clock could drop the revision it was
		// connected over), so it is held to every revision.
		if w.edgeVisible(w.client, w.reader, earlyEdge) {
			t.Fatal("a legacy edge to a once-labelled document is visible")
		}
		if !w.edgeVisible(w.client, w.holder, earlyEdge) {
			t.Fatal("the holder does not see the legacy edge")
		}
		_, err = w.client.InteractionRecord(reader, labelSession)
		assertSameRefusal(t, "legacy session made under the label", err,
			w.neverRecord(w.client))

		// Without a publication reader nothing bounds the history.
		inner, ok := trustedSnapshotValidator(w.f.base).(authorized.EvidenceSnapshotValidator)
		if !ok {
			t.Fatal("the corpus does not validate evidence snapshots")
		}
		selector := w.f.staticSelector(t, w.f.sourceA, w.f.policyA)
		unbounded, err := authorized.NewClient(authorized.Config{
			Base:              w.f.base,
			InteractionWriter: trustedInteractionWriter(w.f.base),
			InteractionReader: trustedInteractionReader(w.f.base),
			SnapshotValidator: &relabelAfterValidation{inner: inner},
			FoldStore:         trustedFoldStore(w.f.base),
			Resolver:          w.f.authority.Resolver(),
			PolicySelector:    selector,
			PolicyStore:       store,
			GenerationReader:  w.f.reader,
			Clock:             w.f.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = unbounded.InteractionRecord(reader, earlySession)
		assertSameRefusal(t, "unbounded legacy session", err, w.neverRecord(unbounded))
		if w.edgeVisible(unbounded, w.reader, earlyEdge) {
			t.Fatal("an unbounded legacy edge is visible")
		}
	})
}

// TestRevisionBindingSurvivesReopen: the bindings are durable. A catalog
// reopened over the same corpus keeps every derived output closed.
func TestRevisionBindingSurvivesReopen(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	first, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := newBindingWorld(t, f, first)
	w.setup(w.client)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	client := w.newClient(reopened)
	w.requireDerivedDenied(client, "after reopen")
	w.requireHolderReads(client)
	if _, err := client.InteractionRecord(w.as(w.reader), bindingOpenSession); err != nil {
		t.Fatalf("an unrelated session is denied after reopen: %v", err)
	}
}

// TestBindingToAnUnregisteredRevisionDenies: a recorded revision the catalog
// does not know fails closed, for every reader, like a never-written record.
func TestBindingToAnUnregisteredRevisionDenies(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		w.record(w.client, bindingOpenSession, w.open.Document.ID)
		if _, err := w.client.InteractionRecord(w.as(w.reader), bindingOpenSession); err != nil {
			t.Fatalf("the session is not readable to begin with: %v", err)
		}
		if err := store.PutInteractionRevisions(context.Background(),
			bindingOpenSession, []authorized.RevisionReference{{
				DocumentID: w.open.Document.ID, RevisionID: "revision-never-registered",
			}}); err != nil {
			t.Fatal(err)
		}
		for _, decision := range []auth.Decision{w.reader, w.holder} {
			_, err := w.client.InteractionRecord(w.as(decision), bindingOpenSession)
			assertSameRefusal(t, "unregistered revision", err, w.neverRecord(w.client))
		}
	})
}
