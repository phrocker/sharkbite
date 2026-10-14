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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type failingRecorder struct {
	calls int
	err   error
}

func (r *failingRecorder) Record(context.Context, EvaluationRecord) error {
	r.calls++
	return r.err
}

type stubSink struct {
	ensureErr  error
	recordErr  error
	ensured    int
	recorded   int
	lastRecord interaction.Session
	mutate     func(*interaction.Session)
}

func (s *stubSink) EnsureInteractionSink(context.Context) error {
	s.ensured++
	return s.ensureErr
}

func (s *stubSink) RecordInteraction(
	_ context.Context, session interaction.Session,
) error {
	s.recorded++
	s.lastRecord = session
	return s.recordErr
}

func (s *stubSink) RecordInteractionResult(
	_ context.Context, session interaction.Session,
) (interaction.Session, error) {
	s.recorded++
	s.lastRecord = session
	if s.recordErr != nil {
		return interaction.Session{}, s.recordErr
	}
	if s.mutate != nil {
		s.mutate(&session)
	}
	return session, nil
}

func TestGraphRecorderRejectsMismatchedPersistedResult(t *testing.T) {
	sink := &stubSink{mutate: func(session *interaction.Session) {
		session.ResultID = "different-result"
	}}
	recorder, err := NewGraphRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return fixedTime }); err != nil {
		t.Fatal(err)
	}
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance(
		"fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	record := EvaluationRecord{
		Provenance: provenance, TranscriptID: "mismatched-result",
		SnapshotID: "snapshot", SnapshotAsOf: fixedTime.Add(-time.Minute),
		AuthorizationFingerprint: "auth-sha256:mismatched-result",
		AuthorizationExpiresAt:   fixedTime.Add(time.Hour),
	}
	if err := recorder.Record(context.Background(), record); !errors.Is(err, ErrInvalid) ||
		!interaction.IsCommittedRecord(err) {
		t.Fatalf("mismatched persisted result error = %v", err)
	}
}

// TestGeneratorRequiresRecorder pins binding decision 4 structurally: there is
// no configuration in which an inference is served without a recorder.
func TestGeneratorRequiresRecorder(t *testing.T) {
	pack, _, _ := fixture(t)
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGenerator(
		NewFakeRunner(), &fakeTools{pack: pack}, budgets(), provenance, nil,
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil recorder error = %v", err)
	}
	if _, err := NewCachedGenerator(
		NewFakeRunner(), &fakeTools{pack: pack}, budgets(), provenance, nil,
		&recordingCache{},
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil recorder error for cached generator = %v", err)
	}
}

// TestCaptureFailureFailsTheRequest pins that recording is part of serving an
// inference: if the interaction cannot be recorded, the request fails.
func TestCaptureFailureFailsTheRequest(t *testing.T) {
	pack, initial, _ := fixture(t)
	stop, err := NewStopAction("stop", resultFor(t, pack, initial), Usage{})
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	captureErr := errors.New("interaction sink is unavailable")
	recorder := &failingRecorder{err: captureErr}
	g, err := NewGenerator(
		NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack},
		budgets(), provenance, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	if _, err := g.Run(context.Background(), pack); !errors.Is(err, captureErr) {
		t.Fatalf("run error = %v, want the capture failure", err)
	}
	if recorder.calls != 1 {
		t.Fatalf("recorder calls = %d", recorder.calls)
	}

	second, err := NewGenerator(
		NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack},
		budgets(), provenance, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	second.now = func() time.Time { return fixedTime }
	if _, err := second.Generate(context.Background(), pack); !errors.Is(
		err, captureErr,
	) {
		t.Fatalf("generate error = %v, want the capture failure", err)
	}
}

// TestNewGraphRecorderChecksSinkAtSetup pins that an unwritable corpus is
// rejected when the recorder is built, not at first write.
func TestNewGraphRecorderChecksSinkAtSetup(t *testing.T) {
	unavailable := shoal.NewError(
		shoal.ErrorUnavailable, "corpus is open read-only")
	sink := &stubSink{ensureErr: unavailable}
	if _, err := NewGraphRecorder(context.Background(), sink); err == nil {
		t.Fatal("recorder accepted an unwritable sink")
	}

	if sink.ensured != 1 {
		t.Fatalf("sink checks = %d", sink.ensured)
	}
	if sink.recorded != 0 {
		t.Fatal("recorder wrote to an unwritable sink")
	}
	if _, err := NewGraphRecorder(context.Background(), nil); !errors.Is(
		err, ErrInvalid,
	) {
		t.Fatalf("nil sink error = %v", err)
	}
	var typedNil *stubSink
	if _, err := NewGraphRecorder(
		context.Background(), typedNil,
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("typed-nil sink error = %v", err)
	}

	writable := &stubSink{}
	recorder, err := NewGraphRecorder(context.Background(), writable)
	if err != nil {
		t.Fatal(err)
	}
	if writable.ensured != 1 {
		t.Fatalf("writable sink checks = %d", writable.ensured)
	}
	if err := recorder.SetClock(func() time.Time { return fixedTime }); err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil clock error = %v", err)
	}
}

func TestInteractionSessionHashesOversizedIdentifiers(t *testing.T) {
	model, prompt := provenanceParts(t)
	for _, size := range []int{
		interaction.MaxIdentifierBytes,
		interaction.MaxIdentifierBytes + 1,
		shoal.MaxSemanticStringBytes,
	} {
		t.Run(fmt.Sprintf("bytes-%d", size), func(t *testing.T) {
			provenance, err := NewProvenance(
				strings.Repeat("a", size),
				model, prompt, "grounded-tools-v1",
			)
			if err != nil {
				t.Fatal(err)
			}
			sink := &stubSink{}
			recorder, err := NewGraphRecorder(context.Background(), sink)
			if err != nil {
				t.Fatal(err)
			}
			if err := recorder.SetClock(
				func() time.Time { return fixedTime }); err != nil {
				t.Fatal(err)
			}
			if err := recorder.Record(
				context.Background(),
				EvaluationRecord{
					Provenance: provenance,
					TranscriptID: shoal.ID(
						fmt.Sprintf("transcript-identifier-%d", size)),
					SnapshotID:               "snapshot-long-identifier",
					SnapshotAsOf:             fixedTime.Add(-time.Minute),
					AuthorizationFingerprint: "auth-sha256:long-identifier",
					AuthorizationExpiresAt:   fixedTime.Add(time.Hour),
				},
			); err != nil {
				t.Fatal(err)
			}
			got := sink.lastRecord.Provenance.Harness
			if size <= interaction.MaxIdentifierBytes {
				if got != strings.Repeat("a", size) {
					t.Fatalf("boundary identifier = %q", got)
				}
			} else if !strings.HasPrefix(got, "sha256:") {
				t.Fatalf("oversized harness identity was not hashed: %q", got)
			}
		})
	}
}

func TestInteractionSessionPreservesExecutionPins(t *testing.T) {
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance(
		"fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	snapshotAt := fixedTime.Add(-time.Minute)
	expiresAt := fixedTime.Add(time.Hour)
	session, err := InteractionSession(EvaluationRecord{
		Provenance:               provenance,
		TranscriptID:             "transcript-pinned",
		SnapshotID:               "snapshot-pinned",
		SnapshotAsOf:             snapshotAt,
		AuthorizationFingerprint: "auth-sha256:pinned",
		AuthorizationExpiresAt:   expiresAt,
	}, fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if session.SnapshotID != "snapshot-pinned" ||
		!session.SnapshotAsOf.Equal(snapshotAt) ||
		session.AuthorizationFingerprint != "auth-sha256:pinned" ||
		!session.AuthorizationExpiresAt.Equal(expiresAt) {
		t.Fatalf("session pins = %+v", session)
	}
}

func TestInteractionSessionPreservesCanonicalEmbeddingSpaces(t *testing.T) {
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance(
		"fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	spaces, err := interaction.NewEmbeddingSpaceSet([]string{
		"18:embedding-space-v15:alpha",
		"18:embedding-space-v14:beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := InteractionSession(EvaluationRecord{
		Provenance:               provenance,
		TranscriptID:             "transcript-spaces",
		SnapshotID:               "snapshot-spaces",
		SnapshotAsOf:             fixedTime.Add(-time.Minute),
		AuthorizationFingerprint: "auth-sha256:spaces",
		AuthorizationExpiresAt:   fixedTime.Add(time.Hour),
		EmbeddingSpaces:          spaces,
	}, fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(session.EmbeddingSpaces, spaces) {
		t.Fatalf("session embedding spaces = %+v", session.EmbeddingSpaces)
	}
}

// TestGraphRecorderRecordsThroughTheSink pins the end-to-end projection and
// that sink failures propagate.
func TestGraphRecorderRecordsThroughTheSink(t *testing.T) {
	pack, initial, _ := fixture(t)
	stop, err := NewStopAction("stop", resultFor(t, pack, initial), Usage{})
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	sink := &stubSink{}
	recorder, err := NewGraphRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return fixedTime }); err != nil {
		t.Fatal(err)
	}
	g, err := NewGenerator(
		NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack},
		budgets(), provenance, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	if _, err := g.Run(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if sink.recorded != 1 {
		t.Fatalf("sink writes = %d", sink.recorded)
	}
	session := sink.lastRecord
	if err := session.Validate(); err != nil {
		t.Fatalf("recorded session is invalid: %v", err)
	}
	if !strings.HasPrefix(string(session.ID), interaction.KindPrefix) {
		t.Fatalf("session ID %q is outside the reserved namespace", session.ID)
	}
	if session.Provenance.Harness != "fake-harness" ||
		session.Provenance.Model != "fake-model" {
		t.Fatalf("session provenance = %+v", session.Provenance)
	}
	if session.SnapshotID != pack.Snapshot().ID() ||
		!session.SnapshotAsOf.Equal(pack.Snapshot().AsOf()) ||
		session.AuthorizationFingerprint != pack.Authorization().Fingerprint() ||
		!session.AuthorizationExpiresAt.Equal(pack.Authorization().ExpiresAt()) {
		t.Fatalf("session execution pins = %+v", session)
	}
	if len(session.Turns) == 0 {
		t.Fatal("session recorded no turns")
	}
	if len(session.SeedNodeIDs) == 0 {
		t.Fatal("session recorded no retrieved source node IDs")
	}
	if len(session.CitedNodeIDs) == 0 {
		t.Fatal("session recorded no cited source node IDs")
	}
	if len(session.SeedEvidence) != 1 ||
		session.SeedEvidence[0].AnchorID != initial.ID() {
		t.Fatalf("session seed evidence = %+v", session.SeedEvidence)
	}
	if len(session.CitedEvidence) != 1 ||
		session.CitedEvidence[0].AnchorID != initial.ID() {
		t.Fatalf("session cited evidence = %+v", session.CitedEvidence)
	}
	if session.SeedEvidence[0].Citation.Range !=
		session.CitedEvidence[0].Citation.Range {
		t.Fatal("recorded evidence lost its exact source range")
	}

	sink.recordErr = errors.New("write refused")
	if _, err := g.Run(context.Background(), pack); !errors.Is(
		err, sink.recordErr,
	) {
		t.Fatalf("run error = %v, want the sink failure", err)
	}
}

// TestRecordedSessionIsRedacted pins the redaction discipline: the durable
// interaction record must not carry the question, the answer, evidence text,
// or model-chosen correlation strings that could smuggle a credential.
func TestRecordedSessionIsRedacted(t *testing.T) {
	pack, initial, _ := fixture(t)
	secretCorrelation := shoal.ID("https://secret.example/token")
	stop, err := NewStopAction(
		secretCorrelation, resultFor(t, pack, initial), Usage{InputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := provenanceParts(t)
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	sink := &stubSink{}
	recorder, err := NewGraphRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return fixedTime }); err != nil {
		t.Fatal(err)
	}
	g, err := NewGenerator(
		NewFakeRunner(ScriptAction(stop)), &fakeTools{pack: pack},
		budgets(), provenance, recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	if _, err := g.Run(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	session := sink.lastRecord
	subgraph, err := session.Subgraph(func(shoal.ID) ([]string, error) {
		return []string{"ops"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := fmt.Sprintf("%+v %+v", session, subgraph)
	for _, secret := range []string{
		string(secretCorrelation), "secret.example", "/token",
	} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("interaction record retained %q", secret)
		}
	}
	if session.QueryDigest != "" && strings.Contains(
		encoded, pack.Query(),
	) {
		t.Fatal("interaction record retained the raw query")
	}
	for _, node := range subgraph.Nodes {
		if !interaction.IsInteractionKind(node.Kind) {
			t.Fatalf("subgraph contains a non-interaction node: %+v", node)
		}
		visibility := node.Properties[interaction.PropertyVisibility]
		// A tool call that retrieved nothing conjoins over the empty set and
		// is correctly unlabeled. Every node that touched a span must carry
		// exactly the resolved label.
		if visibility != "" && visibility != "ops" {
			t.Fatalf("node %q visibility = %q", node.ID, visibility)
		}
		if node.Kind == interaction.KindSession && visibility != "ops" {
			t.Fatalf("session node visibility = %q, want ops", visibility)
		}
	}
}
