// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
)

// TestProvenanceShowsCorrelationBesideTheActor: the provenance views already
// show the session's actor to a caller authorized to see the session, and
// that is the only place they show its correlation (#532). A session without
// one shows nothing, so a pre-#532 record's wire shape is unchanged.
func TestProvenanceShowsCorrelationBesideTheActor(t *testing.T) {
	actor := interaction.ActorContext{SubjectID: "alice", ActorID: "agent"}
	summary := provenanceSummary(explorer.InteractionSummary{
		SessionID: "interaction.session_x", RecordedAt: time.Unix(1, 0).UTC(),
		Actor: actor, CorrelationID: "gateway-trace-532",
	})
	session := provenanceSession(interaction.Session{
		ID: "interaction.session_x", RecordedAt: time.Unix(1, 0).UTC(),
		Actor: actor, CorrelationID: "gateway-trace-532",
	})
	want := encodeID("gateway-trace-532")
	for name, value := range map[string]any{"summary": summary, "session": session} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"correlation_id":"`+want+`"`) ||
			!strings.Contains(string(encoded), `"actor":{`) {
			t.Fatalf("%s wire = %s", name, encoded)
		}
	}
	legacy, err := json.Marshal(provenanceSummary(explorer.InteractionSummary{
		SessionID: "interaction.session_y", Actor: actor,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "correlation_id") {
		t.Fatalf("a session without correlation shows one: %s", legacy)
	}
}
