// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// labelPlane is the production composition of the label evaluator (#564): a
// real corpus behind a real authorized client holding a document on source B
// labelled "secret", and dispatch and fleet events composed through
// ComposeDispatchWithAttestations and composeWithPublisher with that client's
// evaluator and translator, exactly as shoal-explore-web wires them.
type labelPlane struct {
	authority *auth.Authority
	dispatch  *fleet.DispatchService
	events    *fleetevents.Service
	backend   *Adapter
	completed fleet.ActionRecord
	reference fleet.EvidenceRef
	open      fleet.EvidenceRef
	client    *authorized.Client
	ingester  context.Context
	now       time.Time
}

// fullLabels is the production wiring: the client's evaluator, translator
// and current-rule node gate.
func fullLabels(client *authorized.Client) explorerfleet.DispatchLabels {
	return explorerfleet.DispatchLabels{
		Visibility: client.LabelVisibility(), Translator: client.LabelTranslator(),
		Nodes: client.NodeGate(),
	}
}

// newLabelPlane composes the plane with the dispatch labels choose picks from
// the real client, and the client's evaluator on fleet events when
// wireEvents.
func newLabelPlane(
	t *testing.T,
	choose func(*authorized.Client) explorerfleet.DispatchLabels,
	wireEvents bool,
) labelPlane {
	t.Helper()
	return newLabelPlaneWith(t, planeOptions{
		choose: choose, wireEvents: wireEvents, label: "secret",
	})
}

// planeOptions shapes a plane: the dispatch labels, whether events get the
// evaluator and gate, the free-form label set on the recorded document, and
// the label policies the action's owner (its enqueuer and claimant) holds.
type planeOptions struct {
	choose      func(*authorized.Client) explorerfleet.DispatchLabels
	wireEvents  bool
	label       string
	ownerLabels [][]byte
	// graphReference replaces the labelled document reference with the
	// review's graph reference: the open document's own nodes, carrying
	// the labelled document's contains edge.
	graphReference bool
	// wrapGate, when set, wraps the dispatch node gate.
	wrapGate func(evidencelabels.NodeGate) evidencelabels.NodeGate
	// refused is set when the completion is expected to be refused.
	refused bool
	// now fixes the plane's clock; zero uses a minute from now.
	now time.Time
	// evidence selects what the executor reports: "" both references,
	// "labelled" only the labelled one, "none" nothing at all.
	evidence string
}

func newLabelPlaneWith(t *testing.T, options planeOptions) labelPlane {
	t.Helper()
	choose, wireEvents := options.choose, options.wireEvents
	// A minute ahead, so corpus snapshots taken in real time precede it.
	now := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if !options.now.IsZero() {
		now = options.now
	}
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	selector, err := authorized.NewStaticPolicySelector(sourceB, []byte("policy-b"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, Resolver: authority.Resolver(), PolicySelector: selector,
		PolicyStore:       authorized.NewMemoryPolicyStore(),
		GenerationReader:  integrationGeneration{},
		InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Clock:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := choose(client)

	// The labelled document, ingested by a principal holding its label.
	ingester := bindPlaneSubjectWith(t, authority, now, "ingester",
		allLabels(), auth.OperationIngest, auth.OperationRead,
		auth.OperationRetrieve, auth.OperationNeighborhood)
	// What an executor reports: the node's shoal.visibility, free-form.
	reported := ingestReference(t, client, ingester, "file:///b/closed.md",
		"anchor-b-closed", options.label)
	open := ingestReference(t, client, ingester, "file:///b/open.md",
		"anchor-b-open", "")
	if options.graphReference {
		reported = graphReferenceAcross(t, client, ingester, open, reported)
	}
	if options.wrapGate != nil && labels.Nodes != nil {
		labels.Nodes = options.wrapGate(labels.Nodes)
	}

	config := runtimeConfig(t.TempDir())
	config = explorerfleet.ConfigureRuntime(config)
	ConfigureRuntime(&config)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	result := fleet.ExecutionResult{
		Output:             json.RawMessage(`{"ok":true}`),
		EvidenceSnapshotID: "snapshot", EvidenceSnapshotAsOf: now,
		Evidence: []fleet.EvidenceRef{open, reported},
	}
	switch options.evidence {
	case "labelled":
		result.Evidence = []fleet.EvidenceRef{reported}
	case "none":
		result.Evidence = nil
		result.EvidenceSnapshotID, result.EvidenceSnapshotAsOf = "", time.Time{}
	}
	executor := &integrationExecutor{result: result}
	registry, _ := newIntegrationRegistry(t, authority.Resolver(), now, executor)
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	var visibility evidencelabels.Visibility
	var nodes evidencelabels.NodeGate
	if wireEvents {
		visibility, nodes = client.LabelVisibility(), client.NodeGate()
	}
	events, publisher, err := composeWithPublisher(
		backend, authority.Resolver(), integrationGeneration{},
		integrationAuditor{}, integrationLease{}, bytes.Repeat([]byte{7}, 32),
		func() time.Time { return now }, visibility, nodes)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := explorerfleet.ComposeDispatchWithAttestations(
		runtime, registry, authority.Resolver(), integrationActionRecorder{},
		publisher, nil, func() time.Time { return now }, nil, labels)
	if err != nil {
		t.Fatal(err)
	}
	owner := bindPlaneSubjectWith(t, authority, now, "owner",
		options.ownerLabels, auth.OperationDispatch, auth.OperationInvoke)
	queued, err := dispatch.Enqueue(owner,
		integrationEnqueueRequest(now, "labelled", "enqueue-labelled"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := dispatch.Claim(owner, fleet.ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("claim-labelled"), Lease: time.Minute,
		Context: integrationRequestContext(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := dispatch.ExecuteClaim(owner, claimed)
	if options.refused {
		if completed.State != fleet.DispatchFailed ||
			completed.ErrorCode != "invalid_executor_evidence" || len(completed.Evidence) != 0 {
			t.Fatalf("complete = %q / %q with %d references (%v), want "+
				"refused as invalid_executor_evidence", completed.State,
				completed.ErrorCode, len(completed.Evidence), err)
		}
	} else if err != nil || completed.State != fleet.DispatchSucceeded {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	return labelPlane{
		authority: authority, dispatch: dispatch, events: events,
		backend: backend, completed: completed, reference: reported, now: now,
		open: open, client: client, ingester: ingester,
	}
}

// allLabels is every label policy on B the tests use.
func allLabels() [][]byte {
	return [][]byte{sourceBLabelPolicy("secret"), sourceBLabelPolicy("pii")}
}

// graphReferenceAcross is the review's reference: graph evidence naming the
// open document's own document and span nodes, whose one edge is the closed
// document's contains edge, reported with the closed document's label. No
// forgery is needed to build it: an executor reports whatever it saw.
func graphReferenceAcross(
	t *testing.T, client *authorized.Client, ctx context.Context,
	open, closed fleet.EvidenceRef,
) fleet.EvidenceRef {
	t.Helper()
	around, err := client.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{closed.Citation.DocumentID}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var edge shoal.ID
	for _, candidate := range around.Edges {
		if candidate.From == closed.Citation.DocumentID {
			edge = candidate.ID
		}
	}
	if edge == "" {
		t.Fatal("the closed document has no outgoing edge")
	}
	return fleet.EvidenceRef{
		AnchorID: "anchor-b-graph", Kind: interaction.EvidenceGraph,
		NodeIDs:    []shoal.ID{open.Citation.DocumentID, open.Citation.SpanID},
		EdgeIDs:    []shoal.ID{edge},
		Visibility: closed.Visibility,
	}
}

// joinAnything is a dispatch gate that decides reads with the real gate but
// accepts any path at record time, standing for a reference recorded before
// join validation existed.
type joinAnything struct{ evidencelabels.NodeGate }

func (joinAnything) GraphEvidenceValid(
	context.Context, shoal.ID, time.Time, []interaction.EvidenceReference,
) (bool, error) {
	return true, nil
}

// ingestReference ingests one document on B, labelled when label is set,
// and returns the reference an executor would report for its first span.
func ingestReference(
	t *testing.T, client *authorized.Client, ctx context.Context,
	uri string, anchor shoal.ID, label string,
) fleet.EvidenceRef {
	t.Helper()
	metadata := shoal.Metadata{}
	if label != "" {
		metadata[interaction.PropertyVisibility] = label
	}
	receipt, err := client.Ingest(ctx, explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeMarkdown,
		Content:  "# Closed\n\nA paragraph the action retrieved.\n",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := client.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	section := view.Root
	for len(section.Spans) == 0 {
		if len(section.Children) == 0 {
			t.Fatal("the ingested document has no span")
		}
		section = section.Children[0]
	}
	span := section.Spans[0]
	reference := fleet.EvidenceRef{
		AnchorID: anchor, Kind: interaction.EvidenceDocument,
		Citation: document.Citation{
			DocumentID: receipt.Document.ID, RevisionID: receipt.Revision.ID,
			SectionID: span.SectionID, SpanID: span.ID, Range: span.Range,
		},
		NodeIDs: []shoal.ID{receipt.Document.ID, span.SectionID, span.ID},
	}
	if label != "" {
		labels, err := interaction.ParseVisibility(label)
		if err != nil {
			t.Fatal(err)
		}
		reference.Visibility = labels
	}
	return reference
}

// bindPlaneSubject binds a decision on the action's source and on B; holds
// adds B's secret label policy.
func bindPlaneSubject(
	t *testing.T, authority *auth.Authority, now time.Time,
	subject shoal.ID, holds bool, operations ...auth.Operation,
) context.Context {
	t.Helper()
	var labels [][]byte
	if holds {
		labels = append(labels, sourceBLabelPolicy("secret"))
	}
	return bindPlaneSubjectWith(t, authority, now, subject, labels, operations...)
}

// bindPlaneSubjectWith binds a decision on the action's source and on B,
// holding exactly the label policies given.
func bindPlaneSubjectWith(
	t *testing.T, authority *auth.Authority, now time.Time,
	subject shoal.ID, labels [][]byte, operations ...auth.Operation,
) context.Context {
	t.Helper()
	policies := append([][]byte{[]byte("policy"), []byte("policy-b")}, labels...)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: subject, Actor: "actor", ClientID: "client",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     operations,
		PermittedSourceIDs:    [][]byte{[]byte("source"), sourceB},
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(2 * time.Hour),
		RequestID:             "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// secretBTerms is the grant labels of B's secret label policy.
func secretBTerms(t *testing.T) []string {
	t.Helper()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: sourceB,
		GrantPolicyID: sourceBLabelPolicy("secret"), Epoch: 1,
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

func (p labelPlane) status(t *testing.T, ctx context.Context) fleet.ActionRecord {
	t.Helper()
	record, err := p.dispatch.Status(ctx, fleet.StatusRequest{
		ID: p.completed.ID, Context: integrationRequestContext(p.now),
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (p labelPlane) delivered(t *testing.T, subject shoal.ID, holds bool) fleetevents.Event {
	t.Helper()
	ctx := bindPlaneSubject(t, p.authority, p.now, subject, holds,
		auth.OperationSubscriptionCreate)
	subscription, err := p.events.Create(ctx, fleetevents.CreateRequest{
		Token: []byte("subscribe-" + subject), RetryUntil: p.now.Add(time.Hour),
		AgentID: "agent", AgentGeneration: 1, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.events.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription.ID, Limit: fleetevents.MaxPageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.Kind == "action.completed" {
			return event
		}
	}
	t.Fatal("the completion was not delivered")
	return fleetevents.Event{}
}

func anchorsOf(references []fleet.EvidenceRef) []shoal.ID {
	result := make([]shoal.ID, 0, len(references))
	for _, reference := range references {
		result = append(result, reference.AnchorID)
	}
	return result
}

// TestTheProductionCompositionRecordsAndDecidesStructuredLabels is #564 end
// to end. The executor reports the retrieved node's free-form label; dispatch
// records the structured terms of the label policy enforcing it on that node;
// a reader holding that policy sees the evidence exactly as stored on Status
// and on delivery, and one who does not sees neither it nor any trace of it.
func TestTheProductionCompositionRecordsAndDecidesStructuredLabels(t *testing.T) {
	plane := newLabelPlane(t, fullLabels, true)
	var recorded fleet.EvidenceRef
	for _, reference := range plane.completed.Evidence {
		if reference.AnchorID == plane.reference.AnchorID {
			recorded = reference
		}
	}
	if !reflect.DeepEqual(recorded.Visibility, secretBTerms(t)) {
		t.Fatalf("the evidence was recorded with %v, want the label policy's "+
			"terms %v: a free-form label is held by no reader",
			recorded.Visibility, secretBTerms(t))
	}

	holder := bindPlaneSubject(t, plane.authority, plane.now, "owner", true,
		auth.OperationDispatch, auth.OperationInvoke)
	if got := plane.status(t, holder); !reflect.DeepEqual(got.Evidence, plane.completed.Evidence) {
		t.Fatalf("a holder's Status returned %#v, want the stored %#v",
			got.Evidence, plane.completed.Evidence)
	}
	outsider := bindPlaneSubject(t, plane.authority, plane.now, "owner", false,
		auth.OperationDispatch, auth.OperationInvoke)
	if got := anchorsOf(plane.status(t, outsider).Evidence); !reflect.DeepEqual(
		got, []shoal.ID{plane.open.AnchorID}) {
		t.Fatalf("an outsider's Status returned anchors %v", got)
	}

	stored := storedCompletion(t, labelledDelivery{backend: plane.backend})
	if got := plane.delivered(t, "holder", true); !reflect.DeepEqual(got, stored) {
		t.Fatalf("a holding subscriber received\n%#v\nwant the stored\n%#v", got, stored)
	}
	outside := plane.delivered(t, "outsider", false)
	encoded, err := json.Marshal(outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range append(secretBTerms(t), string(plane.reference.Citation.DocumentID),
		string(plane.reference.AnchorID)) {
		if bytes.Contains(encoded, []byte(marker)) {
			t.Fatalf("an outsider received %q: %s", marker, encoded)
		}
	}
}

// TestEachConsumerFailsClosedWithoutTheEvaluator is the wiring check: with
// the node gate left out of one consumer, a holder no
// longer sees the labelled evidence there. Every probe here must fail, which
// is what makes the passing composition above evidence of its wiring.
func TestEachConsumerFailsClosedWithoutTheEvaluator(t *testing.T) {
	holderSees := func(t *testing.T, plane labelPlane) []shoal.ID {
		t.Helper()
		holder := bindPlaneSubject(t, plane.authority, plane.now, "owner", true,
			auth.OperationDispatch, auth.OperationInvoke)
		return anchorsOf(plane.status(t, holder).Evidence)
	}
	// Every reference here names nodes, so the current-rule node gate
	// decides it and the stored labels are provenance only (#564).
	t.Run("dispatch without the node gate", func(t *testing.T) {
		plane := newLabelPlane(t, func(client *authorized.Client) explorerfleet.DispatchLabels {
			return explorerfleet.DispatchLabels{
				Visibility: client.LabelVisibility(), Translator: client.LabelTranslator(),
			}
		}, true)
		if got := holderSees(t, plane); len(got) != 0 {
			t.Fatalf("without the node gate a holder still saw %v", got)
		}
	})
	t.Run("events without the node gate", func(t *testing.T) {
		plane := newLabelPlane(t, fullLabels, false)
		got := plane.delivered(t, "holder", true)
		for _, reference := range got.ConsumedEvidence {
			if reference.AnchorID == plane.reference.AnchorID {
				t.Fatal("without the node gate a holding subscriber still " +
					"received the labelled reference")
			}
		}
	})
}
