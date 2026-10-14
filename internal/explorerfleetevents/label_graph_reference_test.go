// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
)

// The review's graph reference (#564 round 2): graph evidence naming the
// open document's own nodes, whose edge is the labelled document's, reported
// with the labelled document's label. The gate decides it by its edge's rule
// and both of that edge's endpoints, not only by the nodes it names.

// TestAGraphReferenceIsDecidedByItsEdges reads such a reference as if it had
// been recorded before join validation existed. An outsider never receives
// it, on Status, TeamActions, the enqueue and invoke replays, or delivery; a
// holder of the edge's labels does.
func TestAGraphReferenceIsDecidedByItsEdges(t *testing.T) {
	plane := newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret",
		graphReference: true,
		wrapGate: func(gate evidencelabels.NodeGate) evidencelabels.NodeGate {
			return joinAnything{gate}
		},
	})
	if !carries(plane.completed.Evidence, plane.reference.AnchorID) {
		t.Fatal("the graph reference was not recorded; the probe is vacuous")
	}
	owner := bindPlaneSubjectWith(t, plane.authority, plane.now, "owner", nil,
		auth.OperationDispatch, auth.OperationInvoke)
	assertAll(t, "an outsider", plane.dispatchVerdicts(t, plane.reader(t)), false)
	assertAll(t, "the outsider enqueuer", plane.replayVerdicts(t, owner), false)
	if plane.deliveredTo(t, "graph-outsider") {
		t.Fatal("an outsider was delivered a graph reference over a labelled edge")
	}
	assertAll(t, "a holder", plane.dispatchVerdicts(t, plane.reader(t, "secret")), true)
	if !plane.deliveredTo(t, "graph-holder", "secret") {
		t.Fatal("a holder of the edge's labels was not delivered the reference")
	}
}

// TestAGraphReferenceWhoseEdgesDoNotJoinIsRefused is the record-time half:
// with the production gate, a graph reference whose edge does not run between
// its named nodes is refused as invalid executor evidence and nothing of it
// is recorded.
func TestAGraphReferenceWhoseEdgesDoNotJoinIsRefused(t *testing.T) {
	newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret",
		graphReference: true, refused: true,
	})
}
