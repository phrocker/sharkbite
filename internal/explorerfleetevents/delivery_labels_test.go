// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// realLabels is the production reader label evaluator (#564). It answers
// from the real auth.Decision bound to ctx, so it can only answer for whoever
// is asking at the moment it is asked — which is what makes publish-time
// evaluation (the publisher asking) distinguishable from delivery-time
// evaluation (each subscriber asking).
func realLabels(now time.Time) func(auth.Resolver) evidencelabels.Visibility {
	return func(resolver auth.Resolver) evidencelabels.Visibility {
		evaluator, err := authorized.NewLabelVisibility(authorized.LabelVisibilityConfig{
			Resolver: resolver, Clock: func() time.Time { return now },
		})
		if err != nil {
			panic(err)
		}
		return evaluator
	}
}

var deliveryNow = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// catalogGate is an evidencelabels.NodeGate over a fixed catalog of the
// current node rules: each node's rule is the conjunction of its listed
// structured terms, and the subscriber is decided by the real evaluator.
// The references below name nodes that exist in no corpus, so their rules
// are listed here; label_evaluator_e2e_test.go drives the authorized
// client's own gate over a real catalog.
type catalogGate struct {
	evaluator evidencelabels.Visibility
	rules     map[shoal.ID][]string
}

func (catalogGate) GraphEvidenceValid(
	context.Context, shoal.ID, time.Time, []interaction.EvidenceReference,
) (bool, error) {
	return true, nil
}

func (g catalogGate) GraphsVisibleToReader(
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

func (g catalogGate) graphVisible(
	ctx context.Context, nodeIDs, edgeIDs []shoal.ID,
) (bool, error) {
	var terms []string
	for _, id := range append(append([]shoal.ID(nil), nodeIDs...), edgeIDs...) {
		rule, ok := g.rules[id]
		if !ok {
			return false, nil
		}
		terms = append(terms, rule...)
	}
	return g.evaluator.VisibleToReader(ctx, terms)
}

func termsOf(source, grant []byte) []string {
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: source,
		GrantPolicyID: grant, Epoch: 1,
	})
	if err != nil {
		panic(err)
	}
	terms, err := policy.VisibilityTerms()
	if err != nil {
		panic(err)
	}
	return terms
}

// deliveryCatalog is the current rules of the two references' nodes: A's
// open document is governed by A's source policy; B's labelled document by
// B's source policy and its secret and project-x label policies.
func deliveryCatalog(evaluator evidencelabels.Visibility) catalogGate {
	open := termsOf([]byte("source"), []byte("policy"))
	closed := append(append(termsOf(sourceB, []byte("policy-b")),
		termsOf(sourceB, sourceBLabelPolicy("secret"))...),
		termsOf(sourceB, sourceBLabelPolicy("project-x"))...)
	rules := map[shoal.ID][]string{}
	for _, id := range openEvidence.NodeIDs {
		rules[id] = open
	}
	for _, id := range secretEvidence.NodeIDs {
		rules[id] = closed
	}
	return catalogGate{evaluator: evaluator, rules: rules}
}

// sourceB is the source of the labelled document, and its two free-form
// labels are enforced as (source, label) policies (#570).
var sourceB = []byte("source-b")

func sourceBLabelPolicy(label string) []byte {
	id, err := authorized.LabelPolicyID(sourceB, label)
	if err != nil {
		panic(err)
	}
	return id
}

// secretVisibility is what the reference to B is recorded with: the grant
// labels of B's secret and project-x label policies.
func secretVisibility() []string {
	var terms []string
	for _, label := range []string{"secret", "project-x"} {
		policy, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: []byte("domain"), SourceID: sourceB,
			GrantPolicyID: sourceBLabelPolicy(label), Epoch: 1,
		})
		if err != nil {
			panic(err)
		}
		policyTerms, err := policy.VisibilityTerms()
		if err != nil {
			panic(err)
		}
		terms = append(terms, policyTerms...)
	}
	labels, err := interaction.Conjoin(terms)
	if err != nil {
		panic(err)
	}
	return labels
}

// The two references the executor records. The open one is from the action's
// own source and carries no label; the secret one is drawn from a document in
// source B labelled secret&project-x.
var (
	openEvidence = fleet.EvidenceRef{
		AnchorID: "anchor-a-open", Kind: interaction.EvidenceDocument,
		Citation: document.Citation{
			DocumentID: "doc-a-open", RevisionID: "rev-a-open",
			SectionID: "section-a-open",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0},
				End:   document.SourcePosition{Offset: 10},
			},
		},
		NodeIDs: []shoal.ID{"doc-a-open", "section-a-open"},
	}
	secretEvidence = fleet.EvidenceRef{
		AnchorID: "anchor-b-hidden", Kind: interaction.EvidenceDocument,
		Citation: document.Citation{
			DocumentID: "doc-b-hidden", RevisionID: "rev-b-hidden",
			SectionID: "section-b-hidden", SpanID: "span-b-hidden",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 4711},
				End:   document.SourcePosition{Offset: 4799},
			},
		},
		NodeIDs: []shoal.ID{
			"doc-b-hidden", "section-b-hidden", "span-b-hidden"},
		Visibility: secretVisibility(),
	}
)

// secretMarkers is every identifier, anchor and label the B reference
// carries. None may appear in an envelope delivered to a subscriber that does
// not hold its labels — raw, or in the encodings the HTTP wire uses.
func secretMarkers() []string {
	raw := []string{
		"anchor-b-hidden", "doc-b-hidden", "rev-b-hidden",
		"section-b-hidden", "span-b-hidden",
		"project-x", "secret", "4711", "4799",
	}
	markers := append(append([]string(nil), raw...), secretVisibility()...)
	for _, value := range raw[:5] {
		markers = append(markers,
			base64.RawURLEncoding.EncodeToString([]byte(value)),
			base64.StdEncoding.EncodeToString([]byte(value)))
	}
	return markers
}

type labelledDelivery struct {
	authority *auth.Authority
	events    *fleetevents.Service
	backend   *Adapter
	now       time.Time
}

// newLabelledDelivery runs a real dispatch through the real fleetevents
// service and the real event backend: the action is enqueued, claimed and
// executed by "owner", whose executor records both references (or exactly
// evidence, when given). The owner — the publisher of every lifecycle event —
// holds neither label.
func newLabelledDelivery(
	t *testing.T, visibility func(auth.Resolver) evidencelabels.Visibility,
	evidence ...fleet.EvidenceRef,
) labelledDelivery {
	t.Helper()
	if len(evidence) == 0 {
		evidence = []fleet.EvidenceRef{openEvidence, secretEvidence}
	}
	now := deliveryNow
	config := runtimeConfig(t.TempDir())
	config = explorerfleet.ConfigureRuntime(config)
	ConfigureRuntime(&config)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	owner := bindSubject(t, authority, now, "owner",
		auth.OperationDispatch, auth.OperationInvoke)

	executor := &integrationExecutor{result: fleet.ExecutionResult{
		Output:             json.RawMessage(`{"ok":true}`),
		EvidenceSnapshotID: "snapshot", EvidenceSnapshotAsOf: now,
		Evidence: evidence,
	}}
	registry, _ := newIntegrationRegistry(t, authority.Resolver(), now, executor)
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	var evaluator evidencelabels.Visibility
	var nodes evidencelabels.NodeGate
	if visibility != nil {
		evaluator = visibility(authority.Resolver())
		nodes = deliveryCatalog(evaluator)
	}
	capability := explorerfleetcap.New()
	events, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: backend, Resolver: authority.Resolver(),
		GenerationReader: integrationGeneration{}, LeaseValidator: integrationLease{},
		Auditor: integrationAuditor{}, CursorKey: bytes.Repeat([]byte{7}, 32),
		Clock:              func() time.Time { return now },
		EvidenceVisibility: evaluator,
		EvidenceNodes:      nodes,
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewActionEventPublisher(
		events, authority.Resolver(), capability, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := explorerfleet.ComposeDispatch(
		runtime, registry, authority.Resolver(), integrationActionRecorder{},
		publisher, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil || completed.State != fleet.DispatchSucceeded {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	if len(completed.Evidence) != len(evidence) {
		t.Fatalf("the action recorded %d evidence references, want %d: "+
			"this probe cannot check what was never recorded",
			len(completed.Evidence), len(evidence))
	}
	return labelledDelivery{
		authority: authority, events: events, backend: backend, now: now,
	}
}

func bindSubject(
	t *testing.T, authority *auth.Authority, now time.Time,
	subject shoal.ID, operations ...auth.Operation,
) context.Context {
	t.Helper()
	// Authorized on the action's own source. The (domain, source, policy,
	// object) check does not ask about labels — which is the whole of #562.
	// Only "holder" is also granted B and its two label policies.
	sources := [][]byte{[]byte("source")}
	policies := [][]byte{[]byte("policy")}
	if subject == "holder" {
		sources = append(sources, sourceB)
		policies = append(policies, []byte("policy-b"),
			sourceBLabelPolicy("secret"), sourceBLabelPolicy("project-x"))
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: subject, Actor: "actor", ClientID: "client",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   operations,
		PermittedSourceIDs:  sources,
		PermittedPolicyIDs:  policies,
		PolicyGeneration:    1, AuthenticationExpires: now.Add(2 * time.Hour),
		RequestID: "request", CorrelationID: "correlation",
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

// subscribe binds subject as a subscriber on source A.
func (d labelledDelivery) subscribe(
	t *testing.T, subject shoal.ID,
) (context.Context, []byte) {
	t.Helper()
	ctx := bindSubject(t, d.authority, d.now, subject,
		auth.OperationSubscriptionCreate)
	subscription, err := d.events.Create(ctx, fleetevents.CreateRequest{
		Token: []byte("subscribe-" + subject), RetryUntil: d.now.Add(time.Hour),
		AgentID: "agent", AgentGeneration: 1, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, subscription.ID
}

// deliveryPaths are every subscriber-facing way to read an envelope. Each
// returns the completed event as delivered and the bytes it was delivered
// as. They all read through Service.Pull — there is no list API, webhook or
// separate replay path — and Pull delivers nothing authorizeDelivery has not
// returned, so that is the one point the label check lives.
var deliveryPaths = []struct {
	name string
	read func(
		*testing.T, labelledDelivery, context.Context, []byte,
	) (completedEnvelope, []byte)
}{
	{name: "backfill from the retained floor", read: readBackfill},
	{name: "resume from a cursor", read: readResumed},
	{name: "long-poll", read: readLongPoll},
	{name: "HTTP pull", read: readHTTP},
}

type completedEnvelope struct {
	event fleetevents.Event
	found bool
}

func completedFrom(t *testing.T, events []fleetevents.Event) completedEnvelope {
	t.Helper()
	for _, event := range events {
		if event.Kind == "action.completed" {
			return completedEnvelope{event: event, found: true}
		}
	}
	return completedEnvelope{}
}

func encodeEvents(t *testing.T, events []fleetevents.Event) []byte {
	t.Helper()
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	// %#v as well as JSON: a field the JSON encoding happened to skip must
	// not be a place for a B identifier to hide.
	return append(encoded, []byte(fmt.Sprintf("%#v", events))...)
}

func readBackfill(
	t *testing.T, d labelledDelivery, ctx context.Context, subscription []byte,
) (completedEnvelope, []byte) {
	page, err := d.events.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription, Limit: fleetevents.MaxPageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	return completedFrom(t, page.Events), encodeEvents(t, page.Events)
}

func readResumed(
	t *testing.T, d labelledDelivery, ctx context.Context, subscription []byte,
) (completedEnvelope, []byte) {
	first, err := d.events.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completedFrom(t, first.Events).found {
		t.Fatal("the first page already held the completion; the resume " +
			"probe would not exercise a cursor")
	}
	resumed, err := d.events.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription, Cursor: first.NextCursor,
		Limit: fleetevents.MaxPageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	return completedFrom(t, resumed.Events), encodeEvents(t, resumed.Events)
}

func readLongPoll(
	t *testing.T, d labelledDelivery, ctx context.Context, subscription []byte,
) (completedEnvelope, []byte) {
	page, err := d.events.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription, Limit: fleetevents.MaxPageSize,
		Wait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return completedFrom(t, page.Events), encodeEvents(t, page.Events)
}

func readHTTP(
	t *testing.T, d labelledDelivery, ctx context.Context, subscription []byte,
) (completedEnvelope, []byte) {
	handler, err := webapi.NewFleetEventsHandler(d.events)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		webapi.FleetEventsRoutePrefix+"subscriptions/"+
			base64.RawURLEncoding.EncodeToString(subscription)+"/pull",
		strings.NewReader(`{"limit":256}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP pull = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	var page struct {
		Events []struct {
			Kind             string `json:"kind"`
			ConsumedEvidence []struct {
				AnchorID string `json:"anchor_id"`
			} `json:"consumed_evidence"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	// The HTTP wire is a different shape; rebuild just enough of the domain
	// event to compare anchors. The byte check runs on the body itself.
	for _, event := range page.Events {
		if event.Kind != "action.completed" {
			continue
		}
		result := fleetevents.Event{Kind: event.Kind}
		for _, reference := range event.ConsumedEvidence {
			anchor, err := base64.RawURLEncoding.DecodeString(reference.AnchorID)
			if err != nil {
				t.Fatal(err)
			}
			result.ConsumedEvidence = append(result.ConsumedEvidence,
				interaction.EvidenceReference{AnchorID: shoal.ID(anchor)})
		}
		return completedEnvelope{event: result, found: true}, body
	}
	return completedEnvelope{}, body
}

func anchors(references []interaction.EvidenceReference) []shoal.ID {
	result := make([]shoal.ID, 0, len(references))
	for _, reference := range references {
		result = append(result, reference.AnchorID)
	}
	return result
}

func assertNoSecretBytes(t *testing.T, path string, delivered []byte) {
	t.Helper()
	for _, marker := range secretMarkers() {
		if bytes.Contains(delivered, []byte(marker)) {
			t.Fatalf("%s delivered %q, from evidence whose labels the "+
				"subscriber does not hold:\n%s", path, marker, delivered)
		}
	}
}

// TestASubscriberReceivesOnlyEvidenceItsLabelsCover is #562.
//
// Lifecycle delivery carried every evidence reference on the envelope, and
// authorizeDelivery's check is (domain, source, policy, object) per item —
// not a label check. A subscriber authorized on source A received citation
// identifiers, offsets, node IDs and anchors for a document in source B
// labelled secret&project-x.
func TestASubscriberReceivesOnlyEvidenceItsLabelsCover(t *testing.T) {
	// The owner publishes every lifecycle event and holds neither label, so
	// a filter applied at publish — under the publisher's identity — would
	// withhold the reference from "holder" too.
	delivery := newLabelledDelivery(t, realLabels(deliveryNow))

	stored := storedCompletion(t, delivery)

	for _, path := range deliveryPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Run("a subscriber lacking the labels", func(t *testing.T) {
				ctx, subscription := delivery.subscribe(t, "outsider-"+shoal.ID(
					strings.ReplaceAll(path.name, " ", "-")))
				completed, delivered := path.read(t, delivery, ctx, subscription)
				if !completed.found {
					t.Fatal("the completion was not delivered at all; the " +
						"label check withholds references, not events")
				}
				assertNoSecretBytes(t, path.name, delivered)
				if got := anchors(completed.event.ConsumedEvidence); !reflect.DeepEqual(
					got, []shoal.ID{"anchor-a-open"}) {
					t.Fatalf("%s delivered anchors %v, want only the open "+
						"one", path.name, got)
				}
				if path.name == "HTTP pull" {
					return
				}
				// Exactly the stored event without the B reference: nothing
				// added in its place — no count, no placeholder anchor.
				if want := withoutSecret(t, stored); !reflect.DeepEqual(
					completed.event, want) {
					t.Fatalf("%s delivered\n%#v\nwant exactly\n%#v",
						path.name, completed.event, want)
				}
			})
			t.Run("a subscriber holding the labels", func(t *testing.T) {
				ctx, subscription := delivery.subscribe(t, "holder")
				completed, delivered := path.read(t, delivery, ctx, subscription)
				if !completed.found {
					t.Fatal("the completion was not delivered")
				}
				if got := anchors(completed.event.ConsumedEvidence); !reflect.DeepEqual(
					got, []shoal.ID{"anchor-a-open", "anchor-b-hidden"}) {
					t.Fatalf("%s delivered anchors %v to a subscriber "+
						"holding the labels, want both", path.name, got)
				}
				if !bytes.Contains(delivered, []byte(
					base64.RawURLEncoding.EncodeToString([]byte("doc-b-hidden")))) &&
					!bytes.Contains(delivered, []byte("doc-b-hidden")) {
					t.Fatalf("%s withheld the B citation from a holder", path.name)
				}
				if path.name == "HTTP pull" {
					return
				}
				if !reflect.DeepEqual(completed.event, stored) {
					t.Fatalf("%s delivered\n%#v\nto a holder, want the "+
						"stored event unchanged\n%#v",
						path.name, completed.event, stored)
				}
			})
		})
	}

	// Redaction is per audience at delivery. The durable record of the
	// transition — what the backend holds — still carries both references
	// and the label expression after every delivery above.
	t.Run("the durable event is unchanged", func(t *testing.T) {
		after := storedCompletion(t, delivery)
		if !reflect.DeepEqual(after, stored) {
			t.Fatalf("delivery mutated the durable event:\n%#v\nwas\n%#v",
				after, stored)
		}
		if got := anchors(after.ConsumedEvidence); !reflect.DeepEqual(
			got, []shoal.ID{"anchor-a-open", "anchor-b-hidden"}) {
			t.Fatalf("the durable event carries %v, want both references", got)
		}
		if !reflect.DeepEqual(after.ConsumedEvidenceVisibility,
			[][]string{nil, secretVisibility()}) {
			t.Fatalf("the durable event's visibility = %#v",
				after.ConsumedEvidenceVisibility)
		}
	})
}

// TestWithNoEvaluatorLabelledEvidenceIsWithheldFromEverySubscriber is the
// fail-closed direction, the same as the dispatch read paths: with no node
// gate nothing can evaluate a referenced node's current rule, so no
// reference naming a node is delivered, to a holder or anyone else (#564).
// The event itself is still delivered.
func TestWithNoEvaluatorLabelledEvidenceIsWithheldFromEverySubscriber(t *testing.T) {
	delivery := newLabelledDelivery(t, nil)
	for _, path := range deliveryPaths {
		t.Run(path.name, func(t *testing.T) {
			ctx, subscription := delivery.subscribe(t, "holder")
			completed, delivered := path.read(t, delivery, ctx, subscription)
			if !completed.found {
				t.Fatal("the completion was not delivered at all")
			}
			assertNoSecretBytes(t, path.name, delivered)
			if got := anchors(completed.event.ConsumedEvidence); len(got) != 0 {
				t.Fatalf("%s delivered anchors %v with no node gate, want none",
					path.name, got)
			}
		})
	}
}

func storedCompletion(t *testing.T, d labelledDelivery) fleetevents.Event {
	t.Helper()
	events, _, err := d.backend.Scan(context.Background(), 1, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "action.completed" {
			// Delivery zeroes the internal stream position.
			event.Sequence = 0
			return event
		}
	}
	t.Fatal("no completion was stored")
	return fleetevents.Event{}
}

// withoutSecret is the expected delivery to a subscriber lacking B's labels,
// built independently of the code under test: the stored event with the B
// reference, its visibility entry and its authorization join entry removed,
// and nothing else changed.
func withoutSecret(t *testing.T, stored fleetevents.Event) fleetevents.Event {
	t.Helper()
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var want fleetevents.Event
	if err := json.Unmarshal(encoded, &want); err != nil {
		t.Fatal(err)
	}
	var consumed []interaction.EvidenceReference
	var visibility [][]string
	for i, reference := range want.ConsumedEvidence {
		if reference.AnchorID == secretEvidence.AnchorID {
			continue
		}
		consumed = append(consumed, reference)
		visibility = append(visibility, want.ConsumedEvidenceVisibility[i])
	}
	// With the only labelled reference gone, the group has no labelled
	// entry left, and an event that never had one carries nil. Anything else
	// — even [][]string{nil} — marks that something was withheld (#398).
	labelled := false
	for _, entry := range visibility {
		if len(entry) > 0 {
			labelled = true
		}
	}
	if !labelled {
		visibility = nil
	}
	want.ConsumedEvidence, want.ConsumedEvidenceVisibility = consumed, visibility
	var join []fleetevents.Evidence
	for _, item := range want.Evidence {
		if item.Reference != nil && item.Reference.AnchorID == secretEvidence.AnchorID {
			continue
		}
		join = append(join, item)
	}
	want.Evidence = join
	return want
}

// TestAnOutsiderCannotTellEvidenceWasWithheld is #398 applied to #562's
// envelope: what a subscriber lacking B's labels receives must be
// indistinguishable from the same transition published with only the
// unlabelled reference. Deep equality on the delivered events catches any
// marker of a withheld reference — a count, a placeholder, or the shape of
// a visibility group that once held a label — not only one this test names.
func TestAnOutsiderCannotTellEvidenceWasWithheld(t *testing.T) {
	evaluator := realLabels(deliveryNow)
	withheld := newLabelledDelivery(t, evaluator)
	neverLabelled := newLabelledDelivery(t, evaluator, openEvidence)
	for _, path := range deliveryPaths {
		t.Run(path.name, func(t *testing.T) {
			ctx, subscription := withheld.subscribe(t, "outsider")
			got, gotBytes := path.read(t, withheld, ctx, subscription)
			ctx, subscription = neverLabelled.subscribe(t, "outsider")
			want, wantBytes := path.read(t, neverLabelled, ctx, subscription)
			if !got.found || !want.found {
				t.Fatal("the completion was not delivered")
			}
			if !reflect.DeepEqual(got.event, want.event) {
				t.Fatalf("%s: an outsider can tell evidence was withheld:\n"+
					"%#v\nwant, as if it had never been recorded,\n%#v",
					path.name, got.event, want.event)
			}
			if path.name == "HTTP pull" {
				// The cursor is sealed with a fresh nonce, so compare the
				// body without it.
				if !bytes.Equal(withoutCursor(t, gotBytes), withoutCursor(t, wantBytes)) {
					t.Fatalf("HTTP bodies differ:\n%s\n%s", gotBytes, wantBytes)
				}
			}
		})
	}
}

func withoutCursor(t *testing.T, body []byte) []byte {
	t.Helper()
	var page map[string]json.RawMessage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	delete(page, "next_cursor")
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
