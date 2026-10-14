// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorer

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestDurableRetryUnderAnotherCorrelationKeepsTheFirst: at the durable
// writer, as at the trusted sink above it, a session recorded again under
// another correlation is the same session. It neither conflicts nor
// overwrites, and that holds across a restart (#532).
func TestDurableRetryUnderAnotherCorrelationKeepsTheFirst(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:            interaction.DerivedID("session", "correlated-retry"),
		RecordedAt:    time.Unix(1700000000, 0).UTC(),
		Operation:     interaction.OperationRetrieval,
		RequestID:     "request",
		CorrelationID: "trace-one",
	}
	first, err := corpus.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	before, err := corpus.InteractionSubgraph(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	retry := session
	retry.CorrelationID = "trace-two"
	again, err := corpus.RecordInteractionResult(ctx, retry)
	if err != nil {
		t.Fatalf("retry under another correlation = %v", err)
	}
	if !reflect.DeepEqual(again, first) || again.CorrelationID != "trace-one" {
		t.Fatalf("retry = %+v, want the first %+v", again, first)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.RecordInteractionResult(ctx, retry); err != nil {
		t.Fatalf("retry after restart = %v", err)
	}
	stored, err := corpus.InteractionRecord(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Session.CorrelationID != "trace-one" ||
		stored.Summary.CorrelationID != "trace-one" {
		t.Fatalf("stored correlation = %q / %q",
			stored.Session.CorrelationID, stored.Summary.CorrelationID)
	}
	after, err := corpus.InteractionSubgraph(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the retry changed the materialized session")
	}
	// Anything other than correlation still conflicts.
	different := session
	different.StopReason = "different"
	if _, err := corpus.RecordInteractionResult(
		ctx, different); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("divergent retry = %v", err)
	}
}

// The session shape before #532, as far as gob is concerned: gob matches
// struct fields by name, so a type without CorrelationID is exactly what the
// previous build encodes and decodes.
type pre532Session struct {
	ID         shoal.ID
	RecordedAt time.Time
	Operation  interaction.Operation
	Actor      interaction.ActorContext
	RequestID  shoal.ID
	StopReason string
}

type pre532PersistedInteraction struct {
	SessionID  shoal.ID
	Session    pre532Session
	Operation  interaction.Operation
	Actor      interaction.ActorContext
	RecordedAt time.Time
}

// TestInteractionEncodingIsCompatibleBothWays: a record the previous build
// wrote decodes here with no correlation, and a record this build writes
// decodes in the previous build's shape, which ignores the field.
func TestInteractionEncodingIsCompatibleBothWays(t *testing.T) {
	recordedAt := time.Unix(1700000000, 0).UTC()
	actor := interaction.ActorContext{SubjectID: "subject", ActorID: "actor"}
	old := pre532PersistedInteraction{
		SessionID: "interaction.session_old",
		Session: pre532Session{
			ID: "interaction.session_old", RecordedAt: recordedAt,
			Operation: interaction.OperationToolCall, Actor: actor,
			RequestID: "request", StopReason: "approval_request",
		},
		Operation: interaction.OperationToolCall, Actor: actor,
		RecordedAt: recordedAt,
	}
	encoded, err := encodeEmbeddedRecord(embeddedRecordInteraction, old)
	if err != nil {
		t.Fatal(err)
	}
	var current persistedInteraction
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordInteraction, &current); err != nil {
		t.Fatalf("this build cannot read a pre-#532 record: %v", err)
	}
	if current.Session.CorrelationID != "" ||
		current.Session.RequestID != "request" ||
		interactionSummary(current).CorrelationID != "" {
		t.Fatalf("pre-#532 record decoded as %+v", current.Session)
	}

	current.Session.CorrelationID = "trace-532"
	encoded, err = encodeEmbeddedRecord(embeddedRecordInteraction, current)
	if err != nil {
		t.Fatal(err)
	}
	var previous pre532PersistedInteraction
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordInteraction, &previous); err != nil {
		t.Fatalf("the previous shape cannot read a #532 record: %v", err)
	}
	if previous.Session.ID != "interaction.session_old" ||
		previous.Session.RequestID != "request" ||
		!reflect.DeepEqual(previous.Session.Actor, actor) {
		t.Fatalf("#532 record decoded in the previous shape as %+v",
			previous.Session)
	}
}
