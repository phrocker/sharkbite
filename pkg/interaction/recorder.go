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

package interaction

import (
	"context"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Sink is the durable interaction boundary implemented by Explorer and its
// authorization-enforcing client.
type Sink interface {
	EnsureInteractionSink(context.Context) error
	RecordInteraction(context.Context, Session) error
}

// ResultSink extends Sink for product recorders that return the exact session
// accepted for persistence. Authorization-enforcing sinks use this result to
// expose trusted actor/reason enrichment rather than caller-supplied values.
type ResultSink interface {
	Sink
	RecordInteractionResult(context.Context, Session) (Session, error)
}

// ValidateRecordedSession checks a successful trusted sink receipt. The sink
// may supply trusted admission time, actor, reason and correlation metadata,
// a default authorization operation, and the structured form of the output
// restriction, but cannot replace the recorded work or its pins.
//
// The output restriction may differ only by translation (#564): the
// authorized sink replaces a free-form ingest label ("secret"), which no
// reader holds, with the structured grant labels of the policy enforcing it.
// So the persisted restriction must keep every structured term requested,
// add no free-form term, and replace a free-form term only by adding a
// structured one; anything else is a sink weakening or rewriting it.
// Callers must mark a validation failure as committed, since writing succeeded.
func ValidateRecordedSession(requested, persisted Session) error {
	expected, err := requested.Canonical()
	if err != nil {
		return err
	}
	actual, err := persisted.Canonical()
	if err != nil {
		return err
	}
	expected.RecordedAt = actual.RecordedAt
	expected.Actor = actual.Actor
	expected.Reason = actual.Reason
	expected.CorrelationID = actual.CorrelationID
	if expected.AuthorizationOperation == "" {
		expected.AuthorizationOperation = actual.AuthorizationOperation
	}
	if translatedRestriction(expected.RequiredVisibility, actual.RequiredVisibility) {
		expected.RequiredVisibility = actual.RequiredVisibility
	}
	if !reflect.DeepEqual(actual, expected) {
		return shoal.NewError(
			shoal.ErrorInternal,
			"durable interaction sink returned a mismatched session",
		)
	}
	return nil
}

// translatedRestriction reports whether persisted is requested with free-form
// terms replaced by structured ones and nothing structured dropped.
func translatedRestriction(requested, persisted []string) bool {
	present := make(map[string]struct{}, len(persisted))
	for _, label := range persisted {
		present[label] = struct{}{}
	}
	asked := make(map[string]struct{}, len(requested))
	replaced := false
	for _, label := range requested {
		asked[label] = struct{}{}
		if _, kept := present[label]; kept {
			continue
		}
		if structuredGrantLabel(label) {
			return false
		}
		replaced = true
	}
	added := false
	for _, label := range persisted {
		if _, wasAsked := asked[label]; wasAsked {
			continue
		}
		if !structuredGrantLabel(label) {
			return false
		}
		added = true
	}
	return !replaced || added
}

// structuredGrantLabel recognizes the structured grant label prefixes (d:,
// s:, g:, svc:). It is a classification, not validation: a free-form label
// that happens to share a prefix is treated as structured, which only makes
// the check above stricter.
func structuredGrantLabel(label string) bool {
	for _, prefix := range []string{"d:", "s:", "g:", "svc:"} {
		if len(label) > len(prefix) && label[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// Recorder is the product-level fail-closed recorder for retrieval, chat, MCP,
// and other non-harness adapters. It canonicalizes a typed Session, supplies a
// UTC timestamp when one is absent, and returns only after the sink accepts the
// durable record.
type Recorder struct {
	sink ResultSink
	now  func() time.Time
}

// NewRecorder verifies the sink during setup so a product surface cannot begin
// serving interactions without durable capture.
func NewRecorder(ctx context.Context, sink ResultSink) (*Recorder, error) {
	if ctx == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "context is required")
	}
	if isNilSink(sink) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction sink is required")
	}
	if err := sink.EnsureInteractionSink(ctx); err != nil {
		return nil, err
	}
	return &Recorder{sink: sink, now: time.Now}, nil
}

// SetClock configures the recorder clock for deterministic tests.
func (r *Recorder) SetClock(now func() time.Time) error {
	if r == nil {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction recorder is required")
	}
	if now == nil {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction recorder clock is required")
	}
	r.now = now
	return nil
}

// Record durably stores a typed interaction. RecordedAt is always overwritten
// from the recorder's trusted clock; a caller cannot backdate or future-date
// authorization chronology. On an exact retry, the ResultSink returns the
// original accepted session and timestamp.
func (r *Recorder) Record(
	ctx context.Context, session Session,
) (Session, error) {
	if r == nil || isNilSink(r.sink) {
		return Session{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction recorder is required")
	}
	recordedAt := r.now().UTC()
	if !session.AuthorizationExpiresAt.IsZero() &&
		!recordedAt.Before(session.AuthorizationExpiresAt) {
		return Session{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"interaction authorization expired before recording",
		)
	}
	session.RecordedAt = recordedAt
	canonical, err := session.Canonical()
	if err != nil {
		return Session{}, err
	}
	persisted, err := r.sink.RecordInteractionResult(ctx, canonical)
	if err != nil {
		return Session{}, err
	}
	persisted, err = persisted.Canonical()
	if err != nil {
		return Session{}, MarkCommittedRecord(
			shoal.WrapError(
				shoal.ErrorInternal,
				"durable interaction sink returned an invalid session",
				err,
			),
		)
	}
	if err := ValidateRecordedSession(canonical, persisted); err != nil {
		return Session{}, MarkCommittedRecord(err)
	}
	deliveredAt := r.now().UTC()
	if deliveredAt.IsZero() {
		return persisted, MarkCommittedRecord(
			shoal.NewError(
				shoal.ErrorInternal,
				"trusted interaction recorder clock is unavailable after persistence",
			),
		)
	}
	if !persisted.AuthorizationExpiresAt.IsZero() &&
		!deliveredAt.Before(persisted.AuthorizationExpiresAt) {
		return persisted, MarkCommittedRecord(
			shoal.NewError(
				shoal.ErrorUnauthorized,
				"interaction authorization expired after durable recording",
			),
		)
	}
	if err := ctx.Err(); err != nil {
		return persisted, MarkCommittedRecord(
			shoal.WrapError(
				shoal.ErrorUnavailable,
				"interaction recording completed after request cancellation",
				err,
			),
		)
	}
	return persisted, nil
}

func isNilSink(sink ResultSink) bool {
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
