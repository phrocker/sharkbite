// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleetevents

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestEvidenceVisibilityMustAlignAndBeCanonical pins the publish-time shape
// the delivery check relies on (#562): an expression per reference, or none
// at all, each canonical. A misaligned group could hand a reference another
// reference's label, so it is refused rather than guessed at.
func TestEvidenceVisibilityMustAlignAndBeCanonical(t *testing.T) {
	two := []interaction.EvidenceReference{{AnchorID: "a"}, {AnchorID: "b"}}
	for _, probe := range []struct {
		name       string
		visibility [][]string
		ok         bool
	}{
		{name: "no labels at all", ok: true},
		{name: "one labelled, one not",
			visibility: [][]string{nil, {"project-x", "secret"}}, ok: true},
		{name: "fewer expressions than references",
			visibility: [][]string{{"secret"}}},
		{name: "a non-canonical expression",
			visibility: [][]string{nil, {"secret", "project-x"}}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			err := validateEvidenceVisibility(two, probe.visibility)
			if (err == nil) != probe.ok {
				t.Fatalf("validateEvidenceVisibility = %v, want ok=%v",
					err, probe.ok)
			}
		})
	}
}

// TestAJoinEntryCoveringLabelledEvidenceMustNameIt is the publish-time half
// of #562's drop rule. Delivery withholds a labelled reference with the join
// entries that name it; an entry with no Reference that covers one of its
// identifiers by ObjectID alone names nothing, so it would be delivered with
// the hidden identifier in it. Such an event is refused at publish.
func TestAJoinEntryCoveringLabelledEvidenceMustNameIt(t *testing.T) {
	hidden := interaction.EvidenceReference{
		AnchorID: "anchor-hidden", Kind: interaction.EvidenceDocument,
		Citation: document.Citation{
			DocumentID: "doc-hidden", RevisionID: "rev-hidden",
			SectionID: "section-hidden",
			Range: document.SourceRange{
				End: document.SourcePosition{Offset: 1},
			},
		},
		NodeIDs: []shoal.ID{"doc-hidden", "section-hidden"},
	}
	base := Evidence{
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
	}
	event := func(visibility [][]string, join ...Evidence) Event {
		return Event{
			EventID: []byte("event"), Kind: "action.completed",
			ProducerID: []byte("producer"), ProducerGeneration: 1,
			ActionID: []byte("action"), TransitionID: []byte("transition"),
			Reason:                     interaction.Reason{Code: "completed"},
			Evidence:                   append([]Evidence{base}, join...),
			ConsumedEvidence:           []interaction.EvidenceReference{hidden},
			ConsumedEvidenceVisibility: visibility,
			OccurredAt:                 time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
		}
	}
	byObjectID := func(id shoal.ID) Evidence {
		entry := base
		entry.ObjectID = id
		return entry
	}
	named := base
	named.ObjectID = "anchor-hidden"
	named.Reference = &hidden

	labelled := [][]string{{"secret"}}
	if _, err := normalizeEvent(event(labelled, named), false); err != nil {
		t.Fatalf("an entry naming its labelled reference was refused: %v", err)
	}
	coverage := []Evidence{
		byObjectID("anchor-hidden"), byObjectID("doc-hidden"),
		byObjectID("rev-hidden"), byObjectID("section-hidden"),
	}
	if _, err := normalizeEvent(event(labelled, coverage...), false); err == nil {
		t.Fatal("a join covering labelled evidence by object ID alone was " +
			"accepted; delivery would hand its identifiers to a subscriber " +
			"lacking the labels")
	}
	// Unlabelled evidence carries nothing to withhold, so covering it by
	// object ID stays legal.
	if _, err := normalizeEvent(event(nil, coverage...), false); err != nil {
		t.Fatalf("unlabelled coverage by object ID was refused: %v", err)
	}
}

// TestAnAllUnlabelledVisibilityGroupIsStoredAsNil keeps an event's shape
// independent of how its author spelled "no labels".
func TestAnAllUnlabelledVisibilityGroupIsStoredAsNil(t *testing.T) {
	if got := canonicalVisibilityGroup([][]string{nil, {}}); got != nil {
		t.Fatalf("canonicalVisibilityGroup = %#v, want nil", got)
	}
	labelled := [][]string{nil, {"secret"}}
	if got := canonicalVisibilityGroup(labelled); len(got) != 2 {
		t.Fatalf("canonicalVisibilityGroup dropped a label: %#v", got)
	}
}
