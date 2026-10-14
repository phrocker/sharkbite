// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package interaction_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCorrelationIDShape(t *testing.T) {
	for _, valid := range []shoal.ID{
		"",
		"upstream-alice-451",
		"dev-correlation-0123456789abcdef",
		"trace/é/ü",
		shoal.ID(strings.Repeat("x", shoal.MaxIDBytes)),
	} {
		if err := interaction.ValidateCorrelationID(valid); err != nil {
			t.Fatalf("%q refused: %v", valid, err)
		}
		if interaction.RecordableCorrelationID(valid) != valid {
			t.Fatalf("%q was not recordable", valid)
		}
	}
	for name, invalid := range map[string]shoal.ID{
		"space":         "two words",
		"padded":        " padded",
		"newline":       "line\nbreak",
		"terminal":      "evil\x1b[2J",
		"nul":           "nul\x00",
		"bidi override": "abc‮def",
		"invalid utf-8": "bad\xffbyte",
		"over bound":    shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)),
	} {
		if err := interaction.ValidateCorrelationID(
			invalid); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: error = %v", name, err)
		}
		if got := interaction.RecordableCorrelationID(invalid); got != "" {
			t.Fatalf("%s: recordable as %q", name, got)
		}
	}
}

// TestSessionBoundaryRefusesUnprintableCorrelation is the defence in depth:
// the hosted authenticators already refuse these at mint, but a session is
// checked on its own, including when it is decoded from storage.
func TestSessionBoundaryRefusesUnprintableCorrelation(t *testing.T) {
	session := correlatedSession("trace-ok")
	if err := session.Validate(); err != nil {
		t.Fatal(err)
	}
	session.CorrelationID = "evil\x1b]0;title\x07"
	if err := session.Validate(); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("unprintable correlation error = %v", err)
	}
	if _, err := session.Canonical(); err == nil {
		t.Fatal("canonical accepted an unprintable correlation")
	}
}

// TestCorrelationIsNotPartOfTheMaterializedSession: two events that differ
// only in correlation materialize the same session — same identity, same
// nodes, same edges, same visibility — and the correlation string appears in
// none of it. The subgraph is what a session's identity and retrieval surface
// are made of; correlation is caller-supplyable and must not split or merge
// it.
func TestCorrelationIsNotPartOfTheMaterializedSession(t *testing.T) {
	resolve := func(shoal.ID) ([]string, error) { return []string{"ops"}, nil }
	one, err := correlatedSession("trace-one").Subgraph(resolve)
	if err != nil {
		t.Fatal(err)
	}
	two, err := correlatedSession("trace-two").Subgraph(resolve)
	if err != nil {
		t.Fatal(err)
	}
	none, err := correlatedSession("").Subgraph(resolve)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, two) || !reflect.DeepEqual(one, none) {
		t.Fatal("correlation changed the materialized session")
	}
	for _, node := range one.Nodes {
		for key, value := range node.Properties {
			if strings.Contains(value, "trace-one") {
				t.Fatalf("node %s property %s carries the correlation",
					node.ID, key)
			}
		}
	}
}

// TestRecordedSessionAcceptsSinkCorrelation: the trusted sink supplies
// correlation as it supplies actor and reason, so a recorder accepts it.
func TestRecordedSessionAcceptsSinkCorrelation(t *testing.T) {
	requested := correlatedSession("")
	persisted := correlatedSession("stamped-by-sink")
	if err := interaction.ValidateRecordedSession(
		requested, persisted); err != nil {
		t.Fatal(err)
	}
}

func correlatedSession(correlation shoal.ID) interaction.Session {
	return interaction.Session{
		ID:            interaction.DerivedID("session", "correlated"),
		RecordedAt:    time.Unix(1700000000, 0).UTC(),
		Operation:     interaction.OperationToolCall,
		RequestID:     "request-1",
		CorrelationID: correlation,
		Actor: interaction.ActorContext{
			SubjectID: "subject", ActorID: "actor",
		},
		Turns: []interaction.Turn{{
			Index: 0, Decision: "fleet.dispatch.enqueue_admission",
			ToolCall: &interaction.ToolCall{
				Kind:             "fleet.dispatch.enqueue_admission",
				RetrievedNodeIDs: []shoal.ID{"span-a"},
			},
		}},
	}
}
