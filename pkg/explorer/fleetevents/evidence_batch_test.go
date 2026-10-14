// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleetevents

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// countingNodeGate admits every reference naming "node-open" and counts how
// often it is asked.
type countingNodeGate struct{ calls, graphs int }

func (g *countingNodeGate) GraphsVisibleToReader(
	_ context.Context, graphs []evidencelabels.Graph,
) ([]bool, error) {
	g.calls++
	g.graphs += len(graphs)
	verdicts := make([]bool, len(graphs))
	for index, graph := range graphs {
		verdicts[index] = len(graph.NodeIDs) > 0 && graph.NodeIDs[0] == "node-open"
	}
	return verdicts, nil
}

func (*countingNodeGate) GraphEvidenceValid(
	context.Context, shoal.ID, time.Time, []interaction.EvidenceReference,
) (bool, error) {
	return true, nil
}

// TestAPageOfEventsAsksTheGateOnce: every reference of every event on a
// delivered page is decided in one gate call (#564), so delivery's catalog
// reads are bounded by the gate's batching rather than multiplied by events
// and references.
func TestAPageOfEventsAsksTheGateOnce(t *testing.T) {
	reference := func(anchor, node shoal.ID) interaction.EvidenceReference {
		return interaction.EvidenceReference{
			AnchorID: anchor, Kind: interaction.EvidenceDocument,
			Citation: document.Citation{
				DocumentID: node, RevisionID: "rev", SectionID: "section",
				Range: document.SourceRange{End: document.SourcePosition{Offset: 1}},
			},
			NodeIDs: []shoal.ID{node, "section"},
		}
	}
	var events []Event
	for index := range 8 {
		suffix := shoal.ID(string(rune('a' + index)))
		events = append(events, Event{
			EventID: []byte("event-" + string(suffix)), Kind: "action.completed",
			ConsumedEvidence: []interaction.EvidenceReference{
				reference("open-"+suffix, "node-open"),
				reference("closed-"+suffix, "node-closed"),
			},
			CitedEvidence: []interaction.EvidenceReference{
				reference("cited-"+suffix, "node-closed"),
			},
		})
	}
	gate := &countingNodeGate{}
	service := &Service{evidenceNodes: gate}
	delivered, err := service.readableEvents(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if gate.calls != 1 || gate.graphs != 24 {
		t.Fatalf("8 events with 24 references asked the gate %d times about "+
			"%d references, want once about all 24", gate.calls, gate.graphs)
	}
	for _, event := range delivered {
		if len(event.ConsumedEvidence) != 1 || event.ConsumedEvidence[0].NodeIDs[0] != "node-open" ||
			len(event.CitedEvidence) != 0 {
			t.Fatalf("delivered %#v, want only the open reference", event)
		}
	}
}

// TestMisalignedVerdictsFailClosed: verdicts that do not align with their
// group are the question failing. The group is never delivered unfiltered.
func TestMisalignedVerdictsFailClosed(t *testing.T) {
	references := []interaction.EvidenceReference{{AnchorID: "a"}, {AnchorID: "b"}}
	labelled := []labelledReference{{reference: references[0]}, {reference: references[1]}}
	kept, visibility, withheld, err := readableEvidenceGroup(
		references, nil, labelled, []bool{true})
	if err == nil || kept != nil || visibility != nil || withheld {
		t.Fatalf("misaligned verdicts returned %v, %v, %v, %v; want an error and nothing",
			kept, visibility, withheld, err)
	}
}
