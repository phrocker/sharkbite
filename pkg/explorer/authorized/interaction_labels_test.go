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
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized/authorizedtest"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Reader label evaluation on interaction and fold reads (#564, #567, #568),
// through real decisions, the real corpus and the real authorized client.

const (
	labelledSession shoal.ID = "interaction.session_labelled"
	plainSession    shoal.ID = "interaction.session_plain"
	documentSession shoal.ID = "interaction.session_document"
	neverSession    shoal.ID = "interaction.session_never_written"
	neverFold       shoal.ID = "interaction.fold_never_written"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// assertSameBytes is deep equality and byte equality both: nil-versus-empty
// differences survive one comparison but not the other.
func assertSameBytes(t *testing.T, name string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got=%#v\nwant=%#v", name, got, want)
	}
	if gotBytes, wantBytes := mustJSON(t, got), mustJSON(t, want); string(gotBytes) != string(wantBytes) {
		t.Fatalf("%s bytes:\n got=%s\nwant=%s", name, gotBytes, wantBytes)
	}
}

func assertNoLeak(t *testing.T, name string, value any) {
	t.Helper()
	if leaked := authorizedtest.Leaks(mustJSON(t, value)); len(leaked) != 0 {
		t.Fatalf("%s leaked %v to a reader without the labels", name, leaked)
	}
}

// assertSameRefusal requires err to be exactly the refusal an identifier that
// was never written produces.
func assertSameRefusal(t *testing.T, name string, err, never error) {
	t.Helper()
	if err == nil || never == nil {
		t.Fatalf("%s: err = %v, never-written = %v; want both refused", name, err, never)
	}
	if err.Error() != never.Error() ||
		shoal.IsErrorCode(err, shoal.ErrorNotFound) != shoal.IsErrorCode(never, shoal.ErrorNotFound) {
		t.Fatalf("%s: %v, but a never-written identifier is %v", name, err, never)
	}
}

// labelledWorld records three sessions as the holder: one under the
// structured output labels, one unlabelled, and one on the document labelled
// "secret" whose producer stamped the free-form label (as the chat path's
// effective output visibility does). Each is folded on its own.
type labelledWorld struct {
	f                                     *authorizedtest.Fixture
	labelledFold, plainFold, documentFold explorer.FoldResult
}

func newLabelledWorld(t *testing.T, ceilings ...auth.ServiceCeilingConfig) labelledWorld {
	t.Helper()
	f := authorizedtest.New(t, ceilings...)
	f.Record(t, authorizedtest.HolderSubject, labelledSession,
		authorizedtest.SecretLabels)
	f.Record(t, authorizedtest.HolderSubject, plainSession, nil)
	f.RecordOn(t, authorizedtest.HolderSubject, documentSession,
		f.SecretNodeID, []string{authorizedtest.DocumentLabel})
	return labelledWorld{
		f:            f,
		labelledFold: f.Fold(t, authorizedtest.HolderSubject, labelledSession),
		plainFold:    f.Fold(t, authorizedtest.HolderSubject, plainSession),
		documentFold: f.Fold(t, authorizedtest.HolderSubject, documentSession),
	}
}

// TestLabelledSessionsStoreStructuredTerms is the record-time half: the
// stored output restriction holds the label policy's grant labels, never the
// free-form label the producer derived from the node, which no reader holds.
func TestLabelledSessionsStoreStructuredTerms(t *testing.T) {
	w := newLabelledWorld(t)
	stored, err := w.f.Corpus.InteractionRecord(context.Background(), documentSession)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Session.RequiredVisibility,
		authorizedtest.DocumentLabelTerms) {
		t.Fatalf("stored RequiredVisibility = %v, want the label policy's "+
			"terms %v", stored.Session.RequiredVisibility,
			authorizedtest.DocumentLabelTerms)
	}
	for _, term := range stored.Session.RequiredVisibility {
		if !auth.IsStructuredVisibilityTerm(term) {
			t.Fatalf("a free-form term %q was recorded", term)
		}
	}
	labelled, err := w.f.Corpus.InteractionRecord(context.Background(), labelledSession)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(labelled.Session.RequiredVisibility, authorizedtest.SecretLabels) {
		t.Fatalf("structured output labels were rewritten: %v",
			labelled.Session.RequiredVisibility)
	}
}

// TestAHolderReadsEveryInteractionExactlyAsStored: a reader holding the labels
// receives each record, session and derived graph deep-equal to the stored
// one, label expression included, on every interaction read.
func TestAHolderReadsEveryInteractionExactlyAsStored(t *testing.T) {
	w := newLabelledWorld(t)
	holder := w.f.Context(t, authorizedtest.HolderSubject)
	ctx := context.Background()
	storedList, err := w.f.Corpus.InteractionRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := w.f.Client.InteractionRecords(holder)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "holder list", listed, storedList)
	page, err := w.f.Client.InteractionRecordsPage(holder, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "holder page", page.Records, storedList)
	for _, id := range []shoal.ID{labelledSession, plainSession, documentSession} {
		stored, err := w.f.Corpus.InteractionRecord(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if id != plainSession && stored.Summary.Visibility == "" {
			t.Fatalf("%s was stored without labels; the probe is vacuous", id)
		}
		got, err := w.f.Client.InteractionRecord(holder, id)
		if err != nil {
			t.Fatalf("holder point read of %s: %v", id, err)
		}
		assertSameBytes(t, "holder record "+string(id), got, stored)
		session, err := w.f.Client.Interaction(holder, id)
		if err != nil {
			t.Fatal(err)
		}
		assertSameBytes(t, "holder session "+string(id), session, stored.Session)
		storedGraph, err := w.f.Corpus.InteractionSubgraph(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := w.f.Client.InteractionSubgraph(holder, id)
		if err != nil {
			t.Fatal(err)
		}
		assertSameBytes(t, "holder subgraph "+string(id), graph, storedGraph)
	}
}

// TestAHolderReadsEveryFoldExactlyAsStored is the same for folds: list, page,
// unfold and the fold result.
func TestAHolderReadsEveryFoldExactlyAsStored(t *testing.T) {
	w := newLabelledWorld(t)
	holder := w.f.Context(t, authorizedtest.HolderSubject)
	ctx := context.Background()
	storedFolds, err := w.f.Corpus.Folds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folds, err := w.f.Client.Folds(holder)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "holder folds", folds, storedFolds)
	page, err := w.f.Client.FoldsPage(holder, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "holder fold page", page.Folds, storedFolds)
	for _, fold := range []explorer.FoldResult{w.labelledFold, w.plainFold, w.documentFold} {
		stored, err := w.f.Corpus.RehydrateFold(ctx, fold.FoldID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := w.f.Client.RehydrateFold(holder, fold.FoldID)
		if err != nil {
			t.Fatalf("holder unfold: %v", err)
		}
		assertSameBytes(t, "holder unfold", got, stored)
	}
	if w.labelledFold.Visibility != interaction.Expression(authorizedtest.SecretLabels) {
		t.Fatalf("the holder's own fold result lost its labels: %+v", w.labelledFold)
	}
}

// TestAnOutsiderCannotTellALabelledRecordWasWritten: a reader authorized on
// every touched source but holding none of the labels receives, on every
// read, exactly what it would receive had the labelled records never been
// written — the same refusal as an identifier that does not exist, and lists
// without them and without any marker that something was left out.
func TestAnOutsiderCannotTellALabelledRecordWasWritten(t *testing.T) {
	w := newLabelledWorld(t)
	outsider := w.f.Context(t, authorizedtest.OutsiderSubject)
	holder := w.f.Context(t, authorizedtest.HolderSubject)

	for _, id := range []shoal.ID{labelledSession, documentSession} {
		_, err := w.f.Client.InteractionRecord(outsider, id)
		_, never := w.f.Client.InteractionRecord(outsider, neverSession)
		assertSameRefusal(t, "record "+string(id), err, never)
		_, err = w.f.Client.Interaction(outsider, id)
		_, never = w.f.Client.Interaction(outsider, neverSession)
		assertSameRefusal(t, "session "+string(id), err, never)
		_, err = w.f.Client.InteractionSubgraph(outsider, id)
		_, never = w.f.Client.InteractionSubgraph(outsider, neverSession)
		assertSameRefusal(t, "subgraph "+string(id), err, never)
		_, err = w.f.Client.FoldInteractions(outsider, explorer.FoldRequest{
			SessionIDs: []shoal.ID{id}})
		_, never = w.f.Client.FoldInteractions(outsider, explorer.FoldRequest{
			SessionIDs: []shoal.ID{neverSession}})
		assertSameRefusal(t, "fold of "+string(id), err, never)
	}
	for _, fold := range []shoal.ID{w.labelledFold.FoldID, w.documentFold.FoldID} {
		_, err := w.f.Client.RehydrateFold(outsider, fold)
		_, never := w.f.Client.RehydrateFold(outsider, neverFold)
		assertSameRefusal(t, "unfold", err, never)
	}

	// Lists: exactly the unlabelled record, as the holder reads it.
	plain, err := w.f.Client.InteractionRecord(holder, plainSession)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := w.f.Client.InteractionRecords(outsider)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "outsider list", listed, []explorer.InteractionRecord{plain})
	page, err := w.f.Client.InteractionRecordsPage(outsider, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "outsider page", page,
		explorer.InteractionRecordPage{Records: []explorer.InteractionRecord{plain}})
	summaries, err := w.f.Client.Interactions(outsider)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "outsider summaries", summaries,
		[]explorer.InteractionSummary{plain.Summary})
	plainFold, err := w.f.Client.Folds(holder)
	if err != nil {
		t.Fatal(err)
	}
	var wantFolds []explorer.FoldSummary
	for _, fold := range plainFold {
		if fold.FoldID == w.plainFold.FoldID {
			wantFolds = append(wantFolds, fold)
		}
	}
	folds, err := w.f.Client.Folds(outsider)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "outsider folds", folds, wantFolds)
	foldPage, err := w.f.Client.FoldsPage(outsider, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "outsider fold page", foldPage,
		explorer.FoldSummaryPage{Folds: wantFolds})
	for name, value := range map[string]any{
		"list": listed, "page": page, "summaries": summaries,
		"folds": folds, "fold page": foldPage,
	} {
		assertNoLeak(t, name, value)
	}
}

// TestALegacyFreeFormRecordIsTranslatedAtRead: a record written before
// record-time translation carries the free-form label its producer derived.
// Its touched node is known, so the label translates exactly to that node's
// label policy, the one its own read gate enforces: the holder sees the record
// as stored and the outsider does not. A free-form label no touched node's
// rule enforces cannot be translated and stays unsatisfiable for everyone.
func TestALegacyFreeFormRecordIsTranslatedAtRead(t *testing.T) {
	f := authorizedtest.New(t)
	const legacy shoal.ID = "interaction.session_legacy"
	const orphan shoal.ID = "interaction.session_orphan_label"
	writeLegacy := func(id, node shoal.ID) explorer.InteractionRecord {
		t.Helper()
		decision := f.Decision(t, authorizedtest.HolderSubject)
		fingerprint, err := auth.AuthorizationFingerprint(decision)
		if err != nil {
			t.Fatal(err)
		}
		// Straight to the corpus, as a build without translation wrote it.
		if err := f.Corpus.RecordInteraction(context.Background(), interaction.Session{
			ID: id, RecordedAt: f.Snapshot.AsOf.Add(time.Second).UTC(),
			Operation:  interaction.OperationRetrieval,
			SnapshotID: shoal.ID(f.Snapshot.ID), SnapshotAsOf: f.Snapshot.AsOf,
			AuthorizationFingerprint: shoal.ID(fingerprint.String()),
			AuthorizationExpiresAt:   decision.AuthenticationExpires(),
			SeedNodeIDs:              []shoal.ID{node},
			RequiredVisibility:       []string{authorizedtest.DocumentLabel},
		}); err != nil {
			t.Fatal(err)
		}
		stored, err := f.Corpus.InteractionRecord(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored.Session.RequiredVisibility,
			[]string{authorizedtest.DocumentLabel}) {
			t.Fatalf("legacy record stored %v", stored.Session.RequiredVisibility)
		}
		return stored
	}
	stored := writeLegacy(legacy, f.SecretNodeID)
	writeLegacy(orphan, f.NodeID)

	holder := f.Context(t, authorizedtest.HolderSubject)
	got, err := f.Client.InteractionRecord(holder, legacy)
	if err != nil {
		t.Fatalf("the holder lost a legacy record: %v", err)
	}
	assertSameBytes(t, "legacy record", got, stored)
	_, err = f.Client.InteractionRecord(
		f.Context(t, authorizedtest.OutsiderSubject), legacy)
	_, never := f.Client.InteractionRecord(
		f.Context(t, authorizedtest.OutsiderSubject), neverSession)
	assertSameRefusal(t, "legacy record to an outsider", err, never)
	_, err = f.Client.InteractionRecord(holder, orphan)
	_, never = f.Client.InteractionRecord(holder, neverSession)
	assertSameRefusal(t, "an untranslatable label", err, never)
}

// TestTheExpressionIsShownOnlyToAReaderPermittedEveryTerm replaces #574's
// recorder-only rule. A producer that stamps an output label on the derived
// graph but not on the session's own restriction leaves a record any
// source-authorized reader may see; its expression names labels such a
// reader does not hold, so it is cleared exactly as an unlabelled record
// stores it. A holder sees it, recorder or not.
func TestTheExpressionIsShownOnlyToAReaderPermittedEveryTerm(t *testing.T) {
	f := authorizedtest.New(t)
	const wide shoal.ID = "interaction.session_wide_expression"
	f.RecordWith(t, authorizedtest.HolderSubject, wide, f.NodeID,
		authorizedtest.SecretLabels, nil)
	f.Record(t, authorizedtest.HolderSubject, plainSession, nil)
	ctx := context.Background()
	stored, err := f.Corpus.InteractionRecord(ctx, wide)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary.Visibility == "" || len(stored.Session.RequiredVisibility) != 0 {
		t.Fatalf("the probe needs an expression wider than the restriction: %+v",
			stored.Summary)
	}
	outsider := f.Context(t, authorizedtest.OutsiderSubject)
	holder := f.Context(t, authorizedtest.HolderSubject)
	plain, err := f.Client.InteractionRecord(holder, plainSession)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Client.InteractionRecord(outsider, wide)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "record", got)
	renamed := got
	renamed.Summary.SessionID, renamed.Session.ID = plain.Summary.SessionID, plain.Session.ID
	renamed.Summary.RecordedAt, renamed.Session.RecordedAt =
		plain.Summary.RecordedAt, plain.Session.RecordedAt
	assertSameBytes(t, "expression withheld as unlabelled", renamed, plain)
	graph, err := f.Client.InteractionSubgraph(outsider, wide)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "subgraph", graph)
	plainGraph, err := f.Client.InteractionSubgraph(holder, plainSession)
	if err != nil {
		t.Fatal(err)
	}
	assertSamePropertyKeys(t, graph, plainGraph)

	// The holder sees the stored record, label expression included.
	own, err := f.Client.InteractionRecord(holder, wide)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "holder", own, stored)

	// A fold conjoins its members' expressions and cannot be partly
	// unfolded, so the fold of this session is the holder's alone even
	// though its member session is not (#568). The outsider may not create
	// it either.
	_, err = f.Client.FoldInteractions(outsider, explorer.FoldRequest{
		SessionIDs: []shoal.ID{wide}})
	_, never := f.Client.FoldInteractions(outsider, explorer.FoldRequest{
		SessionIDs: []shoal.ID{neverSession}})
	assertSameRefusal(t, "an outsider's fold of the session", err, never)
	fold := f.Fold(t, authorizedtest.HolderSubject, wide)
	storedFold, err := f.Corpus.RehydrateFold(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	gotFold, err := f.Client.RehydrateFold(holder, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBytes(t, "holder unfold", gotFold, storedFold)
	_, err = f.Client.RehydrateFold(outsider, fold.FoldID)
	_, never = f.Client.RehydrateFold(outsider, neverFold)
	assertSameRefusal(t, "an outsider's unfold", err, never)
	folds, err := f.Client.Folds(outsider)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) != 0 {
		t.Fatalf("an outsider listed %+v", folds)
	}
}

// assertSamePropertyKeys compares derived nodes and edges by property key set;
// their values carry session-derived IDs.
func assertSamePropertyKeys(t *testing.T, withheld, plain explorer.Neighborhood) {
	t.Helper()
	keys := func(metadata shoal.Metadata) string {
		var names []string
		for key := range metadata {
			names = append(names, key)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	if len(withheld.Nodes) != len(plain.Nodes) || len(withheld.Edges) != len(plain.Edges) {
		t.Fatalf("subgraph shapes differ")
	}
	plainNodes := map[string]string{}
	for _, node := range plain.Nodes {
		plainNodes[node.Kind] = keys(node.Properties)
	}
	for _, node := range withheld.Nodes {
		if keys(node.Properties) != plainNodes[node.Kind] {
			t.Fatalf("%s node keys = %s, unlabelled = %s",
				node.Kind, keys(node.Properties), plainNodes[node.Kind])
		}
	}
	plainEdges := map[string]string{}
	for _, edge := range plain.Edges {
		plainEdges[edge.Type] = keys(edge.Properties)
	}
	for _, edge := range withheld.Edges {
		if keys(edge.Properties) != plainEdges[edge.Type] {
			t.Fatalf("%s edge keys = %s, unlabelled = %s",
				edge.Type, keys(edge.Properties), plainEdges[edge.Type])
		}
	}
}

// TestATrustedServiceIsBoundedByItsCeiling: a trusted service holding every
// grant still sees a labelled record only when every term is inside its
// configured ceiling. A ceiling lacking the label refuses it, and a service
// with no ceiling configured is refused every labelled record.
func TestATrustedServiceIsBoundedByItsCeiling(t *testing.T) {
	ceilingWith := func(identity shoal.ID, terms ...string) auth.ServiceCeilingConfig {
		labels := [][]byte{[]byte("svc:" + string(auth.ServiceRoleDataRead))}
		for _, term := range terms {
			labels = append(labels, []byte(term))
		}
		return auth.ServiceCeilingConfig{
			Identity: identity, Role: auth.ServiceRoleDataRead,
			Authorizations: accumulo.NewAuthorizations(labels...),
		}
	}
	lacking := append([]string(nil), authorizedtest.SecretLabels...)
	for index, term := range lacking {
		if strings.HasPrefix(term, "g:") {
			lacking = append(lacking[:index], lacking[index+1:]...)
			break
		}
	}
	w := newLabelledWorld(t,
		ceilingWith("full-ceiling", authorizedtest.SecretLabels...),
		ceilingWith("lacking-ceiling", lacking...),
	)
	stored, err := w.f.Corpus.InteractionRecord(context.Background(), labelledSession)
	if err != nil {
		t.Fatal(err)
	}
	full := w.f.ServiceContext(t, "full-ceiling")
	got, err := w.f.Client.InteractionRecord(full, labelledSession)
	if err != nil {
		t.Fatalf("a service whose ceiling holds every term was refused: %v", err)
	}
	assertSameBytes(t, "service within its ceiling", got, stored)
	for _, identity := range []shoal.ID{"lacking-ceiling", "unconfigured-ceiling"} {
		ctx := w.f.ServiceContext(t, identity)
		_, err := w.f.Client.InteractionRecord(ctx, labelledSession)
		_, never := w.f.Client.InteractionRecord(ctx, neverSession)
		assertSameRefusal(t, string(identity), err, never)
		_, err = w.f.Client.RehydrateFold(ctx, w.labelledFold.FoldID)
		_, never = w.f.Client.RehydrateFold(ctx, neverFold)
		assertSameRefusal(t, string(identity)+" unfold", err, never)
		// Unlabelled data is unaffected by the ceiling.
		if _, err := w.f.Client.InteractionRecord(ctx, plainSession); err != nil {
			t.Fatalf("%s lost an unlabelled record: %v", identity, err)
		}
	}
}
