// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A relabel after an action recorded evidence governs that evidence as it
// governs the document (#564): every reference that names nodes is decided
// by those nodes' current rules, on every dispatch read and on delivery, and
// the stored labels are provenance only.

var readerOperations = []auth.Operation{
	auth.OperationDispatch, auth.OperationInvoke, auth.OperationRead,
	auth.OperationTeamOverviewRead,
}

// relabel re-ingests the recorded document with labels, which rewrites the
// current rule of each of its nodes.
func (p labelPlane) relabel(t *testing.T, labels string) {
	t.Helper()
	if _, err := p.client.Ingest(p.ingester, explorer.Source{
		URI: "file:///b/closed.md", MediaType: explorer.MediaTypeMarkdown,
		Content:  "# Closed\n\nA paragraph the action retrieved.\n",
		Metadata: shoal.Metadata{interaction.PropertyVisibility: labels},
	}); err != nil {
		t.Fatalf("relabel to %q: %v", labels, err)
	}
}

func (p labelPlane) reader(t *testing.T, labels ...string) context.Context {
	t.Helper()
	var held [][]byte
	for _, label := range labels {
		held = append(held, sourceBLabelPolicy(label))
	}
	return bindPlaneSubjectWith(t, p.authority, p.now, "owner", held, readerOperations...)
}

func carries(evidence []fleet.EvidenceRef, anchor shoal.ID) bool {
	for _, reference := range evidence {
		if reference.AnchorID == anchor {
			return true
		}
	}
	return false
}

// dispatchVerdicts reports, per read path, whether reader received the
// labelled reference. Pull offers only claimable work, so a completed action
// is not on it; the Pull page is pinned on the same funnel by the fleet
// package's relabel test (dispatch_label_evaluator_test.go).
func (p labelPlane) dispatchVerdicts(t *testing.T, reader context.Context) map[string]bool {
	t.Helper()
	request := integrationRequestContext(p.now)
	status, err := p.dispatch.Status(reader, fleet.StatusRequest{ID: p.completed.ID, Context: request})
	if err != nil {
		t.Fatal(err)
	}
	team, err := p.dispatch.TeamActions(reader, fleet.TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")}, Context: request,
	})
	if err != nil || len(team.Actions) != 1 {
		t.Fatalf("TeamActions = %d actions, %v", len(team.Actions), err)
	}
	anchor := p.reference.AnchorID
	return map[string]bool{
		"Status":      carries(status.Evidence, anchor),
		"TeamActions": carries(team.Actions[0].Evidence, anchor),
	}
}

// replayVerdicts reports whether the enqueuer, replaying its own request,
// received the labelled reference. owner must be the enqueuing authorization.
func (p labelPlane) replayVerdicts(t *testing.T, owner context.Context) map[string]bool {
	t.Helper()
	request := integrationEnqueueRequest(p.now, "labelled", "enqueue-labelled")
	enqueued, err := p.dispatch.Enqueue(owner, request)
	if err != nil {
		t.Fatalf("enqueue replay: %v", err)
	}
	invoked, err := p.dispatch.Invoke(owner, fleet.InvokeRequest{
		Enqueue: request, ClaimID: []byte("claim-labelled"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("invoke replay: %v", err)
	}
	anchor := p.reference.AnchorID
	return map[string]bool{
		"enqueue replay": carries(enqueued.Evidence, anchor),
		"invoke replay":  carries(invoked.Evidence, anchor),
	}
}

// deliveredTo reports whether a subscriber holding labels received the
// labelled reference.
func (p labelPlane) deliveredTo(t *testing.T, name string, labels ...string) bool {
	t.Helper()
	var held [][]byte
	for _, label := range labels {
		held = append(held, sourceBLabelPolicy(label))
	}
	ctx := bindPlaneSubjectWith(t, p.authority, p.now, shoal.ID(name), held,
		auth.OperationSubscriptionCreate)
	subscription, err := p.events.Create(ctx, fleetevents.CreateRequest{
		Token: []byte("subscribe-" + name), RetryUntil: p.now.Add(time.Hour),
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
		if event.Kind != "action.completed" {
			continue
		}
		for _, reference := range event.ConsumedEvidence {
			if reference.AnchorID == p.reference.AnchorID {
				return true
			}
		}
		return false
	}
	t.Fatal("the completion was not delivered")
	return false
}

func assertAll(t *testing.T, who string, verdicts map[string]bool, want bool) {
	t.Helper()
	for path, got := range verdicts {
		if got != want {
			t.Fatalf("%s on %s: visible = %v, want %v", who, path, got, want)
		}
	}
}

// TestATightenedLabelWithholdsRecordedEvidence: recorded while the document
// was labelled secret, then tightened to secret&pii. A reader holding only
// secret, the enqueuer included, loses the reference on every dispatch read,
// every replay and delivery; one holding both keeps it.
func TestATightenedLabelWithholdsRecordedEvidence(t *testing.T) {
	plane := newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret",
		ownerLabels: [][]byte{sourceBLabelPolicy("secret")},
	})
	owner := bindPlaneSubjectWith(t, plane.authority, plane.now, "owner",
		[][]byte{sourceBLabelPolicy("secret")},
		auth.OperationDispatch, auth.OperationInvoke)
	assertAll(t, "a secret holder before", plane.dispatchVerdicts(t, plane.reader(t, "secret")), true)
	assertAll(t, "the enqueuer before", plane.replayVerdicts(t, owner), true)
	if !plane.deliveredTo(t, "before-secret", "secret") {
		t.Fatal("a secret holder was not delivered the reference before the relabel")
	}

	plane.relabel(t, "secret&pii")
	assertAll(t, "a secret holder after", plane.dispatchVerdicts(t, plane.reader(t, "secret")), false)
	assertAll(t, "the enqueuer after", plane.replayVerdicts(t, owner), false)
	if plane.deliveredTo(t, "after-secret", "secret") {
		t.Fatal("a secret holder was delivered the reference after it was tightened")
	}
	assertAll(t, "a holder of both", plane.dispatchVerdicts(t, plane.reader(t, "secret", "pii")), true)
	if !plane.deliveredTo(t, "after-both", "secret", "pii") {
		t.Fatal("a holder of both labels was not delivered the reference")
	}
}

// TestALoosenedLabelDoesNotReleaseTheCitedRevision: recorded while the
// document was labelled secret&pii, then loosened to secret. The stored
// terms are provenance only and are not what keeps the reference closed. The
// reference cites the revision that was labelled secret&pii, and a document
// reference is decided by its cited revision's own rule as well as the
// current one (#564 round 3; #585 for historical reads). Section and span
// identities do not change with the revision, so without that rule an
// unlabelled current revision would open a labelled historical one. A reader
// holding secret therefore stays refused; a holder of both still sees it.
// A reference naming only nodes and edges is released by loosening, which
// the fleet package's relabel test pins.
func TestALoosenedLabelDoesNotReleaseTheCitedRevision(t *testing.T) {
	plane := newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret&pii",
		ownerLabels: [][]byte{sourceBLabelPolicy("secret")},
	})
	owner := bindPlaneSubjectWith(t, plane.authority, plane.now, "owner",
		[][]byte{sourceBLabelPolicy("secret")},
		auth.OperationDispatch, auth.OperationInvoke)
	assertAll(t, "a secret holder before", plane.dispatchVerdicts(t, plane.reader(t, "secret")), false)
	assertAll(t, "the enqueuer before", plane.replayVerdicts(t, owner), false)
	if plane.deliveredTo(t, "before-secret", "secret") {
		t.Fatal("a secret holder was delivered the reference before the relabel")
	}

	plane.relabel(t, "secret")
	assertAll(t, "a secret holder after", plane.dispatchVerdicts(t, plane.reader(t, "secret")), false)
	assertAll(t, "the enqueuer after", plane.replayVerdicts(t, owner), false)
	if plane.deliveredTo(t, "after-secret", "secret") {
		t.Fatal("a secret holder was delivered a reference citing a revision labelled secret&pii")
	}
	assertAll(t, "a holder of both after", plane.dispatchVerdicts(t, plane.reader(t, "secret", "pii")), true)
	if !plane.deliveredTo(t, "after-both", "secret", "pii") {
		t.Fatal("a holder of both labels lost the reference")
	}
	assertAll(t, "a holder of neither", plane.dispatchVerdicts(t, plane.reader(t)), false)
}

// recordSession records an interaction session, as the ingester, touching
// the same span the action's evidence names.
func (p labelPlane) recordSession(t *testing.T, id shoal.ID) {
	t.Helper()
	decision, err := p.authority.Resolver().Resolve(p.ingester)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.client.Snapshot(p.ingester)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	span := p.reference.NodeIDs[len(p.reference.NodeIDs)-1]
	if _, err := p.client.RecordInteractionResult(p.ingester, interaction.Session{
		ID: id, RecordedAt: p.now, Operation: interaction.OperationRetrieval,
		SnapshotID: shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{span},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDispatchAndInteractionReadsAgreeAcrossARelabel pins the two planes
// together: the same node, the same relabels (tighten, then loosen to
// unlabelled; re-ingesting an earlier revision's exact content and labels
// returns that revision and moves no rule), and the
// same readers, against an expected verdict table for each plane, so that
// neither can drift from the other silently.
//
// Both planes follow one rule over two kinds of object: a reference is no
// more visible than the object it cites is readable now. A document
// reference cites a revision; a graph reference cites nodes and edges. The
// table pins where that rule and the interaction plane's current behaviour
// meet differently:
//
//   - After a tightening the base explorer refuses an interaction record to
//     every reader, holders of the new labels included, until it is
//     re-recorded (staleDerivedVisibilityError). Dispatch decides by the
//     current rules, so a holder of the new labels sees the reference.
//   - After loosening to unlabelled, both planes agree again. The dispatch
//     reference cites the revision that was labelled secret, and that
//     revision is still readable only with secret, so the reference is too.
//     The interaction session is bound to the revision it was recorded over
//     (#695), so it is held to secret as well, even though its touched nodes
//     are now public.
//
// This test is what showed the cited revision mattered: a gate that checked
// only current node rules would have made the loosened dispatch row all
// true, and a labelled historical revision readable through an unlabelled
// current one.
func TestDispatchAndInteractionReadsAgreeAcrossARelabel(t *testing.T) {
	plane := newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret",
	})
	const session shoal.ID = "interaction.session_parity"
	plane.recordSession(t, session)
	readers := [][]string{nil, {"secret"}, {"pii"}, {"secret", "pii"}}
	for _, stage := range []struct {
		name, labels string
		// dispatch and interactions are the expected verdicts per reader,
		// in the order of readers.
		dispatch, interactions []bool
	}{
		{"recorded", "", []bool{false, true, false, true}, []bool{false, true, false, true}},
		{"tightened", "secret&pii", []bool{false, false, false, true}, []bool{false, false, false, false}},
		{"loosened", "-", []bool{false, true, false, true}, []bool{false, true, false, true}},
	} {
		switch stage.labels {
		case "":
		case "-":
			plane.relabel(t, "")
		default:
			plane.relabel(t, stage.labels)
		}
		for index, labels := range readers {
			reader := plane.reader(t, labels...)
			dispatch := plane.dispatchVerdicts(t, reader)["Status"]
			_, err := plane.client.InteractionRecord(reader, session)
			if err != nil && !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatal(err)
			}
			interactions := err == nil
			if dispatch != stage.dispatch[index] || interactions != stage.interactions[index] {
				t.Fatalf("%s, reader holding %v: dispatch visible = %v (want %v), "+
					"interaction visible = %v (want %v)", stage.name, labels,
					dispatch, stage.dispatch[index], interactions, stage.interactions[index])
			}
			if stage.name == "recorded" && dispatch != interactions {
				t.Fatalf("%s, reader holding %v: the planes disagree", stage.name, labels)
			}
		}
	}
}
