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

package harness

import (
	"context"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
)

// InteractionSink is the durable corpus boundary a graph-backed recorder
// writes through. It aliases the product-level interaction sink so inference,
// retrieval, chat, and MCP adapters share one persistence contract.
type InteractionSink = interaction.Sink

// GraphRecorder writes execution records into the corpus graph under the
// reserved interaction.* namespace.
//
// It is fail-closed by construction: NewGraphRecorder refuses to build unless
// the corpus already accepts an interaction write, and Record propagates every
// write failure to its caller, which fails the inference.
type GraphRecorder struct {
	sink InteractionSink
	now  func() time.Time
}

// NewGraphRecorder verifies the sink at setup time and returns a recorder.
// A read-only or otherwise unwritable corpus fails here, with a clear
// diagnostic, before any inference work is started.
func NewGraphRecorder(ctx context.Context, sink InteractionSink) (*GraphRecorder, error) {
	if ctx == nil {
		return nil, invalid("context is required")
	}
	if isNilInteractionSink(sink) {
		return nil, invalid("interaction sink is required")
	}
	if err := sink.EnsureInteractionSink(ctx); err != nil {
		return nil, err
	}
	return &GraphRecorder{sink: sink, now: time.Now}, nil
}

func isNilInteractionSink(sink InteractionSink) bool {
	if sink == nil {
		return true
	}
	value := reflect.ValueOf(sink)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// SetClock configures the recorder clock so fixture evaluation can be
// reproducible.
func (r *GraphRecorder) SetClock(now func() time.Time) error {
	if r == nil {
		return invalid("recorder is required")
	}
	if now == nil {
		return invalid("clock is required")
	}
	r.now = now
	return nil
}

// Record durably captures one execution record. Any failure fails the request.
func (r *GraphRecorder) Record(ctx context.Context, record EvaluationRecord) error {
	if r == nil || r.sink == nil {
		return invalid("recorder is required")
	}
	session, err := InteractionSession(record, r.now().UTC())
	if err != nil {
		return err
	}
	if sink, ok := r.sink.(interaction.ResultSink); ok {
		persisted, err := sink.RecordInteractionResult(ctx, session)
		if err != nil {
			return err
		}
		if err := interaction.ValidateRecordedSession(session, persisted); err != nil {
			return interaction.MarkCommittedRecord(invalid(
				"interaction sink returned an invalid or mismatched persisted session"))
		}
		return nil
	}
	return r.sink.RecordInteraction(ctx, session)
}

// InteractionSession projects a redacted evaluation record onto the public
// interaction contract. It carries identities, digests, counts, and the source
// node IDs the session was shown or cited — never the question, the prompt,
// the answer, evidence text, credentials, or model-chosen correlation strings.
func InteractionSession(
	record EvaluationRecord, recordedAt time.Time,
) (interaction.Session, error) {
	if recordedAt.IsZero() {
		return interaction.Session{}, invalid("interaction record time is required")
	}
	if err := validateLogicalID(
		"evaluation transcript ID", record.TranscriptID,
	); err != nil {
		return interaction.Session{}, err
	}
	if err := validateLogicalID(
		"evaluation snapshot ID", record.SnapshotID,
	); err != nil {
		return interaction.Session{}, err
	}
	if record.SnapshotAsOf.IsZero() {
		return interaction.Session{}, invalid("evaluation snapshot time is required")
	}
	if err := validateLogicalID(
		"evaluation authorization fingerprint",
		record.AuthorizationFingerprint,
	); err != nil {
		return interaction.Session{}, err
	}
	if record.AuthorizationExpiresAt.IsZero() {
		return interaction.Session{}, invalid(
			"evaluation authorization expiry is required")
	}
	session := interaction.Session{
		ID:                       interaction.SessionID(record.TranscriptID, recordedAt),
		RecordedAt:               recordedAt.UTC(),
		Operation:                interaction.OperationInference,
		SnapshotID:               record.SnapshotID,
		SnapshotAsOf:             record.SnapshotAsOf,
		AuthorizationFingerprint: record.AuthorizationFingerprint,
		AuthorizationExpiresAt:   record.AuthorizationExpiresAt,
		AuthorizationOperation:   record.AuthorizationOperation,
		EmbeddingSpaces:          record.EmbeddingSpaces,
		Provenance: interaction.Provenance{
			Harness:      interactionIdentifier(record.Provenance.Harness()),
			Provider:     interactionIdentifier(record.Provenance.Provider()),
			Model:        interactionIdentifier(record.Provenance.Model().Model()),
			ModelVersion: interactionIdentifier(record.Provenance.Model().Version()),
			PromptID:     interactionIdentifier(record.Provenance.Prompt().TemplateID()),
			PromptVer:    interactionIdentifier(record.Provenance.Prompt().Version()),
			PromptHash:   record.Provenance.Prompt().Hash(),
			ToolPolicy:   interactionIdentifier(record.Provenance.ToolPolicy()),
		},
		QueryDigest:   record.QueryDigest,
		RequestID:     record.RequestID,
		ContextPackID: record.ContextPackID,
		ResultID:      record.ResultID,
		StopReason:    string(record.StopReason),
		SeedNodeIDs:   record.SeedNodeIDs,
		SeedEvidence:  record.SeedEvidence,
		CitedNodeIDs:  record.CitedNodeIDs,
		CitedEvidence: record.CitedEvidence,
	}
	for _, turn := range record.Turns {
		mapped := interaction.Turn{
			Index:        turn.Index,
			Decision:     string(turn.Decision),
			InputTokens:  turn.Usage.InputTokens,
			OutputTokens: turn.Usage.OutputTokens,
			Failed:       turn.Failed,
		}
		if turn.ToolKind != "" {
			mapped.ToolCall = &interaction.ToolCall{
				Kind:              string(turn.ToolKind),
				RetrievedNodeIDs:  turn.RetrievedNodeIDs,
				RetrievedEvidence: turn.RetrievedEvidence,
			}
		}
		session.Turns = append(session.Turns, mapped)
	}
	if err := session.Validate(); err != nil {
		return interaction.Session{}, err
	}
	return session, nil
}

func interactionIdentifier(value string) string {
	if len(value) > interaction.MaxIdentifierBytes {
		return "sha256:" + interaction.Digest(value)
	}
	for index := 0; index < len(value); index++ {
		c := value[index]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == ':', c == '/', c == '@',
			c == '+':
		default:
			return "sha256:" + interaction.Digest(value)
		}
	}
	return value
}
