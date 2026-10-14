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

// A recording may cite only what its recorder may read (#714): every cited
// document revision and every cited assertion, with its own evidence, is
// checked against the recorder, and a refusal does not depend on whether
// the hidden revision or assertion exists or what it says.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const citationURI = "file:///citation/relabelled.txt"

// citationWorld: the holder of "secret" ingests a labelled revision R1 and
// relabels the document to an unlabelled R2 that keeps R1's first line. That
// line's span ID depends only on the document, section, text and
// occurrence, so it is the same node in both revisions, and the reader, who
// lacks the label, may read it through R2 but may not read R1.
type citationWorld struct {
	*bindingWorld
	hidden, current explorer.IngestResult
	// shared is R1's span with the same ID as one of R2's, at R1's range.
	shared document.Span
	// currentShared is the same span in R2.
	currentShared document.Span
}

func newCitationWorld(t *testing.T, store authorized.PolicyStore) *citationWorld {
	t.Helper()
	w := &citationWorld{bindingWorld: newBindingWorld(t, newFixture(t), store)}
	holder := w.as(w.holder)
	var err error
	w.hidden, err = w.client.Ingest(holder, labelledSource(citationURI,
		"the shared opening line\n\nthe hidden plan is to sell\n", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authorized.WithDeclassificationReason(holder, "ticket:SEC-714")
	if err != nil {
		t.Fatal(err)
	}
	w.current, err = w.client.Ingest(ctx, labelledSource(citationURI,
		"preface\n\nthe shared opening line\n\nnothing to see here\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if w.current.Document.ID != w.hidden.Document.ID ||
		w.current.Revision.ID == w.hidden.Revision.ID {
		t.Fatalf("relabel did not make a new revision: %+v", w.current)
	}
	w.resnapshot()
	hiddenView, err := w.client.Document(w.as(w.holder),
		w.hidden.Document.ID, w.hidden.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentView, err := w.client.Document(w.as(w.holder),
		w.current.Document.ID, w.current.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range hiddenView.Root.Spans {
		for _, current := range currentView.Root.Spans {
			if span.ID == current.ID {
				w.shared, w.currentShared = span, current
			}
		}
	}
	if w.shared.ID == "" || w.shared.Range == w.currentShared.Range {
		t.Fatalf("fixture shares no span at a different range: %+v / %+v",
			hiddenView.Root.Spans, currentView.Root.Spans)
	}
	// The premise: the reader may read R2 and not R1.
	reader := w.as(w.reader)
	if _, err := w.client.Document(reader, w.hidden.Document.ID,
		w.hidden.Revision.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("the reader's Document(R1) = %v, want not_found", err)
	}
	if _, err := w.client.Document(reader, w.current.Document.ID, ""); err != nil {
		t.Fatalf("the reader's Document(R2) = %v", err)
	}
	return w
}

// citation is a document evidence reference to span's text in revision, at
// span's range in that revision (or shifted by shift bytes).
func citationEvidence(
	t *testing.T, documentID, revision shoal.ID, span document.Span, shift int64,
) interaction.EvidenceReference {
	t.Helper()
	citation := document.Citation{
		DocumentID: documentID, RevisionID: revision,
		SectionID: span.SectionID, SpanID: span.ID, Range: span.Range,
	}
	citation.Range.Start.Offset += shift
	citation.Range.End.Offset += shift
	anchor, err := inference.NewDocumentAnchor(citation, span.Text)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

// citationSession is a session as decision, seeded with evidence.
func (w *citationWorld) citationSession(
	t *testing.T, decision auth.Decision, id shoal.ID,
	evidence interaction.EvidenceReference,
) interaction.Session {
	t.Helper()
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	return interaction.Session{
		ID:                       id,
		RecordedAt:               w.f.clock.Now().UTC(),
		Operation:                interaction.OperationRetrieval,
		RequestID:                shoal.ID("request-" + string(id)),
		SnapshotID:               shoal.ID(w.snapshot.ID),
		SnapshotAsOf:             w.snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              evidence.NodeIDs,
		SeedEvidence:             []interaction.EvidenceReference{evidence},
	}
}

func requireNotPersisted(t *testing.T, base *explorer.Explorer, id shoal.ID) {
	t.Helper()
	if _, err := base.InteractionRecord(
		context.Background(), id); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("refused recording %s persisted a record: %v", id, err)
	}
}

// recordingPath records a session through one of the authorized recording
// entry points.
type recordingPath struct {
	name       string
	operations []auth.Operation
	record     func(*authorized.Client, context.Context, interaction.Session) error
}

type reconcilingSink interface {
	RecordReconciledInteractionResult(
		context.Context, explorerfleetcap.Capability, interaction.Session,
	) (interaction.Session, error)
}

var recordingPaths = []recordingPath{
	{
		name: "RecordInteractionResult", operations: labelReaderOperations,
		record: func(c *authorized.Client, ctx context.Context, s interaction.Session) error {
			_, err := c.RecordInteractionResult(ctx, s)
			return err
		},
	},
	{
		name: "RecordInteraction", operations: labelReaderOperations,
		record: func(c *authorized.Client, ctx context.Context, s interaction.Session) error {
			return c.RecordInteraction(ctx, s)
		},
	},
	{
		name:       "AnalyticsInteractionSink",
		operations: labelReaderOperations,
		record: func(c *authorized.Client, ctx context.Context, s interaction.Session) error {
			_, err := c.AnalyticsInteractionSink().RecordInteractionResult(ctx, s)
			return err
		},
	},
	{
		name:       "FleetActionInteractionSink",
		operations: []auth.Operation{auth.OperationDispatch},
		record: func(c *authorized.Client, ctx context.Context, s interaction.Session) error {
			_, err := c.FleetActionInteractionSink(auth.OperationDispatch).
				RecordInteractionResult(ctx, s)
			return err
		},
	},
	{
		// The durable fleet reconcile: the session's own receipt stands.
		name:       "RecordReconciledInteractionResult",
		operations: []auth.Operation{auth.OperationDispatch},
		record: func(c *authorized.Client, ctx context.Context, s interaction.Session) error {
			sink, ok := c.FleetActionInteractionSink(auth.OperationDispatch).(reconcilingSink)
			if !ok {
				return fmt.Errorf("the dispatch sink does not reconcile")
			}
			s.AuthorizationOperation = string(auth.OperationDispatch)
			_, err := sink.RecordReconciledInteractionResult(ctx, explorerfleetcap.New(), s)
			return err
		},
	},
}

func (w *citationWorld) pathDecision(
	t *testing.T, path recordingPath, subject string, labels ...string,
) auth.Decision {
	t.Helper()
	policies := [][]byte{w.f.policyA, w.f.policyB}
	for _, label := range labels {
		policies = append(policies, labelPolicy(t, w.f.sourceA, label))
	}
	return w.f.decision(t, subject,
		[][]byte{w.f.sourceA, w.f.sourceB}, policies, path.operations)
}

// TestRecordingRefusesAHiddenRevisionCitedThroughCurrentSpans is the issue's
// repro (b): the reader cites hidden R1 through the span ID it reads in R2.
// Whether R1 has that text at that range must not show: a true probe, a
// false probe and a revision that never existed are refused identically,
// through every recording path, and nothing is written. The holder, who may
// read R1, records the very same citation, so the trusted validator accepts
// it and the refusal is the recorder check's.
func TestRecordingRefusesAHiddenRevisionCitedThroughCurrentSpans(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newCitationWorld(t, store)
		docID := w.hidden.Document.ID
		for index, path := range recordingPaths {
			reader := w.pathDecision(t, path, "reader")
			holder := w.pathDecision(t, path, "holder", "secret")
			id := func(kind string) shoal.ID {
				return shoal.ID(fmt.Sprintf("interaction.session_citation_%s_%d", kind, index))
			}
			try := func(decision auth.Decision, id shoal.ID,
				evidence interaction.EvidenceReference) error {
				return path.record(w.client, w.as(decision),
					w.citationSession(t, decision, id, evidence))
			}
			never := try(reader, id("never"),
				citationEvidence(t, docID, "rev_never_existed", w.shared, 0))
			truth := try(reader, id("true"),
				citationEvidence(t, docID, w.hidden.Revision.ID, w.shared, 0))
			// The false probe: the same text one byte later, which R1 does
			// not have.
			falsehood := try(reader, id("false"),
				citationEvidence(t, docID, w.hidden.Revision.ID, w.shared, 1))
			assertSameRefusal(t, path.name+": true probe of R1", truth, never)
			assertSameRefusal(t, path.name+": false probe of R1", falsehood, never)
			for _, kind := range []string{"never", "true", "false"} {
				requireNotPersisted(t, w.f.base, id(kind))
			}
			// Legitimate recordings: the reader over R2, the holder over R1.
			if err := try(reader, id("current"), citationEvidence(t, docID,
				w.current.Revision.ID, w.currentShared, 0)); err != nil {
				t.Fatalf("%s: the reader's citation of R2: %v", path.name, err)
			}
			if err := try(holder, id("holder"), citationEvidence(t, docID,
				w.hidden.Revision.ID, w.shared, 0)); err != nil {
				t.Fatalf("%s: the holder's citation of R1: %v", path.name, err)
			}
			// An exact retry by the same recorder still succeeds.
			retry := w.citationSession(t, holder, id("holder"), citationEvidence(
				t, docID, w.hidden.Revision.ID, w.shared, 0))
			if err := path.record(w.client, w.as(holder), retry); err != nil {
				t.Fatalf("%s: the holder's exact retry: %v", path.name, err)
			}
		}
	})
}

// citedRelationWorld: a seed mints the entities, then the attacker's
// document and a document later relabelled secret both assert the same
// relation on them. The attacker's document asserts it first, so a recorder
// who reads both sees the relation through the attacker's document (#700),
// and nothing but a cited assertion reaches the hidden document. The
// relation's assertions are split into the attacker's own and the hidden
// document's.
type citedRelationWorld struct {
	w           *extractedEdgeWorld
	edge        graph.Edge
	own, theirs []interaction.AssertionReference
	hiddenDoc   shoal.ID
	hiddenRev   shoal.ID
}

func newCitedRelationWorld(t *testing.T, store authorized.PolicyStore) *citedRelationWorld {
	t.Helper()
	w := newExtractedEdgeWorld(t, store)
	body := skillBody("zeta", "zeta-cli")
	hiddenURI := sharedHiddenURIs[0]
	hiddenBody := body + "\n" + sharedEvidenceMarker + " lives here.\n"
	w.extract(w.ingest("file:///shared/seed/SKILL.md", body, nil))
	attacker := w.f.labelIngester(t, "attacker")
	own, err := w.extractAs(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil))
	if err != nil {
		t.Fatal(err)
	}
	hiddenExtraction := w.extract(w.ingest(hiddenURI, hiddenBody, nil))
	relation, from := relationOf(own)
	if again, _ := relationOf(hiddenExtraction); relation == "" || again != relation {
		t.Fatalf("fixture no longer shares the relation: %s vs %s", relation, again)
	}
	w.ingest(hiddenURI, hiddenBody+"\nNow secret.\n",
		shoal.Metadata{"shoal.visibility": "secret"})
	hidden := hiddenExtraction.DocumentID
	result := &citedRelationWorld{w: w, hiddenDoc: hidden}
	alternatives, err := w.f.base.ExtractionAssertions(context.Background(), []shoal.ID{relation})
	if err != nil {
		t.Fatal(err)
	}
	for _, assertion := range alternatives[relation] {
		reference := []interaction.AssertionReference{{
			AssertionID: assertion.ID(), EdgeID: relation, Origin: assertion.Origin(),
		}}
		citation := assertion.Evidence()[0].Citation()
		if citation.DocumentID == hidden {
			result.theirs = reference
			result.hiddenRev = citation.RevisionID
		} else if result.own == nil {
			result.own = reference
		}
	}
	if result.own == nil || result.theirs == nil {
		t.Fatalf("alternatives = %d", len(alternatives[relation]))
	}
	raw, err := w.f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{from}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range raw.Edges {
		if edge.ID == relation {
			result.edge = graph.Edge{ID: relation, From: from, To: edge.To}
		}
	}
	return result
}

// record records a session whose graph evidence is the relation cited with
// assertions, as subject holding labels, through path.
func (r *citedRelationWorld) record(
	t *testing.T, path recordingPath, id shoal.ID,
	assertions []interaction.AssertionReference, subject string, labels ...string,
) error {
	t.Helper()
	f := r.w.f
	snapshot, err := r.w.client.Snapshot(f.labelIngester(t, subject, labels...))
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	policies := [][]byte{f.policyA, f.policyB}
	for _, label := range labels {
		policies = append(policies, labelPolicy(t, f.sourceA, label))
	}
	decision := f.decision(t, subject, [][]byte{f.sourceA, f.sourceB},
		policies, path.operations)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	evidence := exactAuthorizedGraphEvidenceWith(t, f.base, r.edge, assertions)
	return path.record(r.w.client, f.context(t, decision), interaction.Session{
		ID:                       id,
		RecordedAt:               f.clock.Now().UTC(),
		Operation:                interaction.OperationRetrieval,
		RequestID:                shoal.ID("request-" + string(id)),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              evidence.NodeIDs,
		SeedEvidence:             []interaction.EvidenceReference{evidence},
	})
}

// TestRecordingRefusesAnUnreadableCitedAssertion is the issue's (a) at
// record time: the attacker sees a shared relation through its own
// document, and cites the hidden document's assertion of it, which the
// trusted validator accepts (any contributing extraction's). It is refused
// exactly as an assertion nobody made, through every recording path, while
// citing its own assertion succeeds.
func TestRecordingRefusesAnUnreadableCitedAssertion(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		r := newCitedRelationWorld(t, store)
		for index, path := range recordingPaths {
			id := func(kind string) shoal.ID {
				return shoal.ID(fmt.Sprintf("interaction.session_cited_%s_%d", kind, index))
			}
			never := r.record(t, path, id("never"), []interaction.AssertionReference{{
				AssertionID: "assertion:never-made", EdgeID: r.edge.ID,
				Origin: r.theirs[0].Origin,
			}}, "attacker")
			theirs := r.record(t, path, id("theirs"), r.theirs, "attacker")
			assertSameRefusal(t, path.name+": the hidden document's assertion", theirs, never)
			requireNotPersisted(t, r.w.f.base, id("never"))
			requireNotPersisted(t, r.w.f.base, id("theirs"))
			if err := r.record(t, path, id("own"), r.own, "attacker"); err != nil {
				t.Fatalf("%s: citing the attacker's own assertion: %v", path.name, err)
			}
			// The label holder may read the hidden document's evidence, so
			// the trusted validator accepts this very citation.
			if err := r.record(t, path, id("holder"), r.theirs, "holder", "secret"); err != nil {
				t.Fatalf("%s: the holder citing the hidden assertion: %v", path.name, err)
			}
		}
	})
}

// TestSessionBindsItsCitedAssertionsEvidence is the issue's (a) at read
// time: the holder records a session citing the hidden document's assertion
// of a relation the attacker reads through its own document. The session is
// bound to the revision that assertion's evidence cites, so the attacker,
// who may read every touched node and edge, is refused it exactly as a
// session never written, while a session citing the attacker's own
// assertion stays readable to it.
func TestSessionBindsItsCitedAssertionsEvidence(t *testing.T) {
	const (
		hiddenSession shoal.ID = "interaction.session_cited_hidden_bound"
		ownSession    shoal.ID = "interaction.session_cited_own_bound"
		neverSession  shoal.ID = "interaction.session_cited_never_written"
	)
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		r := newCitedRelationWorld(t, store)
		path := recordingPaths[0]
		if err := r.record(t, path, hiddenSession, r.theirs, "holder", "secret"); err != nil {
			t.Fatal(err)
		}
		if err := r.record(t, path, ownSession, r.own, "holder", "secret"); err != nil {
			t.Fatal(err)
		}
		bound, err := store.InteractionRevisions(context.Background(),
			[]shoal.ID{hiddenSession})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, reference := range bound[hiddenSession] {
			found = found || (reference.DocumentID == r.hiddenDoc &&
				reference.RevisionID == r.hiddenRev)
		}
		if !found {
			t.Fatalf("binding %v omits the cited assertion's revision %s",
				bound[hiddenSession], r.hiddenRev)
		}
		f := r.w.f
		attacker := f.labelIngester(t, "attacker")
		holder := f.labelIngester(t, "holder", "secret")
		_, never := r.w.client.InteractionRecord(attacker, neverSession)
		_, err = r.w.client.InteractionRecord(attacker, hiddenSession)
		assertSameRefusal(t, "the attacker reading the bound session", err, never)
		_, neverSubgraph := r.w.client.InteractionSubgraph(attacker, neverSession)
		_, err = r.w.client.InteractionSubgraph(attacker, hiddenSession)
		assertSameRefusal(t, "the attacker reading the bound subgraph", err, neverSubgraph)
		records, err := r.w.client.InteractionRecords(attacker)
		if err != nil {
			t.Fatal(err)
		}
		if listsSession(records, hiddenSession) {
			t.Fatal("the attacker lists the bound session")
		}
		// The control: the same session over its own assertion reads.
		if _, err := r.w.client.InteractionRecord(attacker, ownSession); err != nil {
			t.Fatalf("the attacker cannot read the session citing its own assertion: %v", err)
		}
		if !listsSession(records, ownSession) {
			t.Fatal("the attacker does not list the session citing its own assertion")
		}
		if _, err := r.w.client.InteractionRecord(holder, hiddenSession); err != nil {
			t.Fatalf("the holder cannot read its own session: %v", err)
		}
	})
}

// neighborhoodCountingBase counts the base neighborhoods read through it,
// and the edges they returned.
type neighborhoodCountingBase struct {
	explorer.BoundedClient
	calls, edges int
}

func (b *neighborhoodCountingBase) Neighborhood(
	ctx context.Context, request explorer.NeighborhoodRequest,
) (explorer.Neighborhood, error) {
	result, err := b.BoundedClient.Neighborhood(ctx, request)
	b.calls++
	b.edges += len(result.Edges)
	return result, err
}

// noEdgeAssertions is the corpus's validator without its edge-assertion
// reader, so NewClient promotes none.
type noEdgeAssertions struct {
	authorized.EvidenceSnapshotValidator
	authorized.ExtractionAssertionReader
}

// TestCitedAssertionLookupReadsOnlyTheCitedEdges: resolving a cited
// assertion reads the edge's attached assertion from the trusted corpus by
// edge ID, never the base neighborhood of its source, which for a hub is its
// whole adjacency read under the base's exclusive lock. The control, a
// client without the trusted reader, falls back to that scan and still
// records.
func TestCitedAssertionLookupReadsOnlyTheCitedEdges(t *testing.T) {
	const hubEdges = 24
	store := authorized.NewMemoryPolicyStore()
	r := newCitedRelationWorld(t, store)
	f := r.w.f
	// The relation's source becomes a hub: each further document extracts a
	// relation of the same type from it to a tool of its own, so even the
	// type-filtered fallback reads every hub edge.
	for index := 0; index < hubEdges; index++ {
		hub, _ := relationOf(r.w.extract(r.w.ingest(
			fmt.Sprintf("file:///citation/hub-%d/SKILL.md", index),
			skillBody("zeta", fmt.Sprintf("zeta-tool-%d", index)), nil)))
		if hub == "" || hub == r.edge.ID {
			t.Fatalf("hub document %d asserts no new relation", index)
		}
	}
	around, err := f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{r.edge.From}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(around.Edges) < hubEdges {
		t.Fatalf("the relation's source has %d edges; not a hub", len(around.Edges))
	}
	selector := f.staticSelector(t, f.sourceA, f.policyA)
	newClient := func(base explorer.Client, validator authorized.SnapshotValidator) *authorized.Client {
		client, err := authorized.NewClient(authorized.Config{
			Base:                   base,
			VectorScorer:           trustedVectorScorer(f.base),
			InteractionWriter:      trustedInteractionWriter(f.base),
			InteractionReader:      trustedInteractionReader(f.base),
			SnapshotValidator:      validator,
			DerivedAssertionReader: trustedDerivedAssertionReader(f.base),
			FoldStore:              trustedFoldStore(f.base),
			Resolver:               f.authority.Resolver(),
			PolicySelector:         selector,
			EdgePolicySelector:     selector,
			PolicyStore:            store,
			GenerationReader:       f.reader,
			Clock:                  f.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	validator := trustedSnapshotValidator(f.base)
	evidenceValidator, ok := validator.(authorized.EvidenceSnapshotValidator)
	extractions, extractionsOK := validator.(authorized.ExtractionAssertionReader)
	if !ok || !extractionsOK {
		t.Fatal("the corpus is not a trusted evidence validator")
	}
	world := r.w.client
	defer func() { r.w.client = world }()
	path := recordingPaths[0]

	trusted := &neighborhoodCountingBase{BoundedClient: f.base}
	r.w.client = newClient(trusted, validator)
	if err := r.record(t, path, "interaction.session_hub_trusted", r.own, "attacker"); err != nil {
		t.Fatalf("recording through the trusted reader: %v", err)
	}
	if trusted.calls != 0 {
		t.Fatalf("recording read %d base neighborhoods (%d edges) to resolve one cited assertion",
			trusted.calls, trusted.edges)
	}

	// The control: the fallback does scan the hub, so the count above would
	// see a scan.
	fallback := &neighborhoodCountingBase{BoundedClient: f.base}
	r.w.client = newClient(fallback, noEdgeAssertions{
		EvidenceSnapshotValidator: evidenceValidator,
		ExtractionAssertionReader: extractions,
	})
	if err := r.record(t, path, "interaction.session_hub_fallback", r.own, "attacker"); err != nil {
		t.Fatalf("recording through the fallback: %v", err)
	}
	if fallback.calls == 0 || fallback.edges < hubEdges {
		t.Fatalf("the fallback read %d neighborhoods with %d edges; the control is vacuous",
			fallback.calls, fallback.edges)
	}
}
