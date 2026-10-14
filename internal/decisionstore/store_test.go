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

package decisionstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func taskConfig() decision.TaskConfig {
	return decision.TaskConfig{OwnerID: "owner", Name: "source-priority", Version: "v1", InputSchemaID: "schema:1", EvidencePolicyID: "evidence:1", LabelPolicyID: "labels:1", EvaluationPolicyID: "evaluation:1", PredictionUnit: "subject", LabelUnit: "finding", ActionUnit: "inspection", AggregationID: "rank:1", Questions: []decision.Question{
		{ID: "priority", Kind: decision.Ordinal, RubricID: "priority:1", Labels: []string{"low", "high"}},
		{ID: "relevance", Kind: decision.Choice, RubricID: "relevance:1", Labels: []string{"yes", "no"}},
	}}
}
func fixture(t *testing.T) (inference.ContextPack, decision.PictureConfig) {
	t.Helper()
	a, err := inference.NewDocumentAnchor(document.Citation{DocumentID: "source", RevisionID: "revision", SectionID: "section", SpanID: "span", Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: 4}}}, "code")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := inference.NewSnapshotPin("snapshot", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin("auth", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("inspect", []inference.EvidenceAnchor{a}, nil, snap, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	return pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "run1", EnumerationID: "inventory:1", ScopeID: "scope:1", BuilderID: "builder:1", OntologyProjectionID: "no-ontology:1", InputDigest: strings.Repeat("a", 64), TokenizerID: "tokenizer:1", InputTokens: 1, TokenBudget: 100, Cutoff: now, Sources: []decision.Source{{ID: "source", ArtifactID: "source", RevisionID: "revision", Digest: strings.Repeat("b", 64), OriginID: "origin:1", AuthorityPolicyID: "authority:1", Role: decision.Observation, Control: decision.CandidateControlled, ObservedAt: now.Add(-time.Minute), ReceivedAt: now}}, Subjects: []decision.Subject{{ID: "subject1", SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{a.ID()}}}, Measurements: []decision.Measurement{{ID: "coverage", Unit: "subject", MethodID: "count:1", Numerator: 1, Denominator: &one}}}
}
func predictorConfig() decision.PredictorConfig {
	return decision.PredictorConfig{Provider: "local", RuntimeID: "runtime:1", WeightsDigest: strings.Repeat("a", 64), TokenizerDigest: strings.Repeat("b", 64), FormattingID: "format:1", PreprocessingID: "preprocessing:1", CalibrationID: "uncalibrated:1", EnvironmentDigest: strings.Repeat("c", 64), Device: "cpu", Precision: "float64", BatchPolicyID: "batch:1", DistributionTolerance: 0.001, ReplayTolerance: 0.000001}
}
func requestFixture(t *testing.T) (decision.DecisionRequest, decision.ResultConfig) {
	t.Helper()
	pack, p := fixture(t)
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	picture, err := decision.NewPictureManifest(pack, p)
	if err != nil {
		t.Fatal(err)
	}
	predictor, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	r, err := decision.NewDecisionRequest(task, picture, predictor, decision.RequestConfig{PrincipalID: "principal", ReleaseID: "release:1", CorrelationID: "call:1", RequestedAt: now, Deadline: now.Add(time.Minute), SubjectIDs: []shoal.ID{"subject1"}})
	if err != nil {
		t.Fatal(err)
	}
	return r, decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: now.Add(time.Second), Answers: []decision.Answer{{SubjectID: "subject1", QuestionID: "priority", Status: decision.Answered, Label: "high", Distribution: []decision.LabelProbability{{Label: "low", Probability: 0.25}, {Label: "high", Probability: 0.75}}}, {SubjectID: "subject1", QuestionID: "relevance", Status: decision.Answered, Label: "yes"}}}
}

var testScope = Scope{Domain: []byte("domain"), Principal: "principal"}

func openStore(t *testing.T, dir string, create bool, clock func() time.Time) (*engine.Engine, *Store, CAS) {
	t.Helper()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if create {
		if err := eng.CreateTable(Table, engine.TableOptions{}); err != nil {
			eng.Close()
			t.Fatal(err)
		}
	}
	backend, err := explorercoord.NewEngineStore(eng, Table)
	if err != nil {
		eng.Close()
		t.Fatal(err)
	}
	store, err := New(backend, nil, clock)
	if err != nil {
		eng.Close()
		t.Fatal(err)
	}
	return eng, store, backend
}
func TestReceiptSurvivesRestartAndReplay(t *testing.T) {
	ctx := context.Background()
	clock := now
	dir := t.TempDir()
	eng, s, _ := openStore(t, dir, true, func() time.Time { return clock })
	request, result := requestFixture(t)
	key := []byte("request-key")
	first, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || first.Claim == nil {
		t.Fatalf("reserve: %#v %v", first, err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, s, _ = openStore(t, dir, false, func() time.Time { return clock })
	replay, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || replay.Claim != nil || replay.Receipt.ID != first.Receipt.ID {
		t.Fatalf("pending replay %#v %v", replay, err)
	}
	clock = now.Add(2 * time.Second)
	receipt, err := s.Commit(ctx, testScope, key, request, *first.Claim, result)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != Committed || receipt.PredictionID == "" {
		t.Fatal("missing committed prediction")
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Hour)
	eng, s, _ = openStore(t, dir, false, func() time.Time { return clock })
	defer eng.Close()
	replay, err = s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || replay.Claim != nil || replay.Receipt.PredictionID != receipt.PredictionID {
		t.Fatalf("committed replay %#v %v", replay, err)
	}
	if _, err := s.Commit(ctx, testScope, key, request, *first.Claim, result); err != nil {
		t.Fatal("exact commit retry failed:", err)
	}
	result.Answers[0].Label = "low"
	if _, err := s.Commit(ctx, testScope, key, request, *first.Claim, result); !errors.Is(err, ErrConflict) {
		t.Fatal("changed result replaced winner:", err)
	}
	replay.Receipt.Result.Answers[0].Distribution[0].Probability = 0
	reread, err := s.Get(ctx, testScope, key, request)
	if err != nil {
		t.Fatal(err)
	}
	if reread.Result.Answers[0].Distribution[0].Probability == 0 {
		t.Fatal("caller mutation altered stored result")
	}
}
func TestExpiredClaimIsFenced(t *testing.T) {
	ctx := context.Background()
	clock := now
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, result := requestFixture(t)
	key := []byte("claim")
	first, err := s.Reserve(ctx, testScope, key, request, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Second)
	if _, err := s.Commit(ctx, testScope, key, request, *first.Claim, result); !errors.Is(err, ErrExpired) {
		t.Fatal("expired claim committed:", err)
	}
	second, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || second.Claim == nil {
		t.Fatal("reclaim failed:", err)
	}
	if second.Claim.Token == first.Claim.Token || second.Claim.Version <= first.Claim.Version {
		t.Fatal("recovery did not fence owner")
	}
	if _, err := s.Commit(ctx, testScope, key, request, *first.Claim, result); !errors.Is(err, ErrConflict) {
		t.Fatal("old worker committed:", err)
	}
	clock = now.Add(2 * time.Second)
	if _, err := s.Commit(ctx, testScope, key, request, *second.Claim, result); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentReservationHasOneOwner(t *testing.T) {
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return now })
	defer eng.Close()
	request, _ := requestFixture(t)
	var wg sync.WaitGroup
	owners := make(chan bool, 16)
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Reserve(context.Background(), testScope, []byte("concurrent"), request, time.Minute)
			if err != nil && !errors.Is(err, ErrConflict) {
				failures <- err
			}
			owners <- r.Claim != nil
		}()
	}
	wg.Wait()
	close(owners)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	count := 0
	for owned := range owners {
		if owned {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("owners=%d", count)
	}
}
func TestRequestAndDomainIsolation(t *testing.T) {
	ctx := context.Background()
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return now })
	defer eng.Close()
	request, _ := requestFixture(t)
	key := []byte("same-key")
	first, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foreign := Scope{Domain: []byte("other-domain"), Principal: testScope.Principal}
	if _, err := s.Get(ctx, foreign, key, request); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign domain found receipt:", err)
	}
	other, err := s.Reserve(ctx, foreign, key, request, time.Minute)
	if err != nil || other.Receipt.ID == first.Receipt.ID {
		t.Fatal("domain collision:", err)
	}
	pack, pc := fixture(t)
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	picture, err := decision.NewPictureManifest(pack, pc)
	if err != nil {
		t.Fatal(err)
	}
	predictor, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := request.Config()
	cfg.CorrelationID = "different-request"
	changed, err := decision.NewDecisionRequest(task, picture, predictor, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, testScope, key, changed, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatal("different request under same key accepted:", err)
	}
}

type faultCAS struct {
	CAS
	unknown  bool
	failRead bool
	apply    bool
}

func (f *faultCAS) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	if !f.unknown {
		return f.CAS.CompareAndMutate(ctx, m)
	}
	if f.apply {
		status, err := f.CAS.CompareAndMutate(ctx, m)
		if err != nil || status != allocator.StatusAccepted {
			return status, err
		}
	}
	f.failRead = true
	return allocator.StatusUnknown, allocator.ErrConditionalUnknown
}
func (f *faultCAS) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	if f.failRead {
		return nil, errors.New("read unavailable")
	}
	return f.CAS.ReadExact(ctx, c)
}
func TestUnknownCommitIsReconciledAfterRestart(t *testing.T) {
	ctx := context.Background()
	clock := now
	eng, s, backend := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, result := requestFixture(t)
	key := []byte("uncertain")
	reservation, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Second)
	fault := &faultCAS{CAS: backend, unknown: true, apply: true}
	broken, err := New(fault, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Commit(ctx, testScope, key, request, *reservation.Claim, result); !errors.Is(err, ErrIndeterminate) {
		t.Fatal("unknown commit reported definitively:", err)
	}
	// A fresh store instance discovers the committed result without another model call.
	recovered, err := New(backend, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	replay, err := recovered.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || replay.Claim != nil || replay.Receipt.State != Committed {
		t.Fatalf("recovery %#v %v", replay, err)
	}
}
func TestMalformedResultDoesNotSpendClaim(t *testing.T) {
	ctx := context.Background()
	clock := now
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, result := requestFixture(t)
	key := []byte("malformed")
	reservation, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Second)
	bad := result
	bad.PredictorID = "substituted-model"
	if _, err := s.Commit(ctx, testScope, key, request, *reservation.Claim, bad); err == nil {
		t.Fatal("substituted model committed")
	}
	before, err := s.Get(ctx, testScope, key, request)
	if err != nil || before.State != Pending || before.Version != reservation.Receipt.Version {
		t.Fatal("invalid result mutated reservation")
	}
	if _, err := s.Commit(ctx, testScope, key, request, *reservation.Claim, result); err != nil {
		t.Fatal(err)
	}
}
func TestCorruptRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	eng, s, backend := openStore(t, t.TempDir(), true, func() time.Time { return now })
	defer eng.Close()
	request, _ := requestFixture(t)
	key := []byte("corrupt")
	if _, err := s.Reserve(ctx, testScope, key, request, time.Minute); err != nil {
		t.Fatal(err)
	}
	coord, err := s.coordinate(testScope, key, request)
	if err != nil {
		t.Fatal(err)
	}
	cells, err := backend.ReadExact(ctx, []allocator.Coordinate{coord})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), cells[0].Value...)
	corrupt[len(corrupt)/2] ^= 1
	status, err := backend.CompareAndMutate(ctx, allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{{Coordinate: coord, Value: cells[0].Value}}, Updates: []allocator.Update{{Coordinate: coord, Value: corrupt, Timestamp: 2}}})
	if err != nil || status != allocator.StatusAccepted {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, testScope, key, request); !errors.Is(err, ErrCorrupt) {
		t.Fatal("corrupt receipt accepted:", err)
	}
	if _, err := s.Reserve(ctx, testScope, key, request, time.Minute); !errors.Is(err, ErrCorrupt) {
		t.Fatal("corrupt row treated as absent:", err)
	}
}

type lostAckCAS struct{ CAS }

func (f lostAckCAS) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	status, err := f.CAS.CompareAndMutate(ctx, m)
	if err != nil || status != allocator.StatusAccepted {
		return status, err
	}
	return allocator.StatusUnknown, allocator.ErrConditionalUnknown
}
func TestLostAcknowledgementReconcilesExactWrite(t *testing.T) {
	ctx := context.Background()
	clock := now
	eng, _, backend := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	s, err := New(lostAckCAS{backend}, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	request, result := requestFixture(t)
	key := []byte("lost-ack")
	reserved, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || reserved.Claim == nil {
		t.Fatal("reservation acknowledgement not reconciled:", err)
	}
	clock = now.Add(2 * time.Second)
	receipt, err := s.Commit(ctx, testScope, key, request, *reserved.Claim, result)
	if err != nil || receipt.State != Committed {
		t.Fatal("commit acknowledgement not reconciled:", err)
	}
}
func TestDeadlineAndLateFailure(t *testing.T) {
	ctx := context.Background()
	clock := now
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, _ := requestFixture(t)
	live, err := s.Reserve(ctx, testScope, []byte("live"), request, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, testScope, []byte("expired"), request, time.Second); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(61 * time.Second)
	if _, err := s.Reserve(ctx, testScope, []byte("expired"), request, time.Minute); !errors.Is(err, ErrExpired) {
		t.Fatal("request restarted beyond deadline:", err)
	}
	pending, err := s.Get(ctx, testScope, []byte("expired"), request)
	if err != nil || pending.State != Pending {
		t.Fatal("uncertain request silently terminalized")
	}
	failure := decision.ResultConfig{RequestID: request.ID(), PredictorID: request.PredictorID(), Status: decision.Failed, Reason: "timeout", CompletedAt: clock}
	if _, err := s.Commit(ctx, testScope, []byte("live"), request, *live.Claim, failure); err != nil {
		t.Fatal("late failure cannot close live claim:", err)
	}
}
func TestReceiptCrashHelper(t *testing.T) {
	dir := os.Getenv("SHOAL_DECISION_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	clock := now
	_, s, _ := openStore(t, dir, true, func() time.Time { return clock })
	request, result := requestFixture(t)
	reserved, err := s.Reserve(context.Background(), testScope, []byte("crash"), request, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SHOAL_DECISION_CRASH_PHASE") == "committed" {
		clock = now.Add(2 * time.Second)
		if _, err := s.Commit(context.Background(), testScope, []byte("crash"), request, *reserved.Claim, result); err != nil {
			t.Fatal(err)
		}
	}
	// Exit without Engine.Close: reopening must recover the synced WAL.
	os.Exit(0)
}
func TestAbruptProcessExitRecoversReceipts(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"pending", "committed"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(executable, "-test.run=^TestReceiptCrashHelper$")
			cmd.Env = append(os.Environ(), "SHOAL_DECISION_CRASH_DIR="+dir, "SHOAL_DECISION_CRASH_PHASE="+phase)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child: %v\n%s", err, output)
			}
			eng, s, _ := openStore(t, dir, false, func() time.Time { return now.Add(3 * time.Second) })
			defer eng.Close()
			request, _ := requestFixture(t)
			r, err := s.Reserve(context.Background(), testScope, []byte("crash"), request, time.Minute)
			if err != nil || r.Claim != nil || string(r.Receipt.State) != phase {
				t.Fatalf("WAL recovery %#v %v", r, err)
			}
		})
	}
}

func TestConcurrentDifferentResultsCommitOneWinner(t *testing.T) {
	ctx := context.Background()
	clock := now
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, result := requestFixture(t)
	key := []byte("competing-results")
	reservation, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Second)
	results := []decision.ResultConfig{result, result}
	results[1].Answers = append([]decision.Answer(nil), result.Answers...)
	results[1].Answers[0].Label = "low"
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, r := range results {
		wg.Add(1)
		go func(r decision.ResultConfig) {
			defer wg.Done()
			_, err := s.Commit(ctx, testScope, key, request, *reservation.Claim, r)
			outcomes <- err
		}(r)
	}
	wg.Wait()
	close(outcomes)
	wins, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	stored, err := s.Get(ctx, testScope, key, request)
	if err != nil || stored.State != Committed {
		t.Fatal("winner missing:", err)
	}
}
func TestIndeterminateReservationNeverGrantsUnconfirmedOwnership(t *testing.T) {
	ctx := context.Background()
	eng, s, backend := openStore(t, t.TempDir(), true, func() time.Time { return now })
	defer eng.Close()
	broken, err := New(&faultCAS{CAS: backend, unknown: true, apply: true}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request, _ := requestFixture(t)
	key := []byte("reservation-ack-lost")
	result, err := broken.Reserve(ctx, testScope, key, request, time.Minute)
	if !errors.Is(err, ErrIndeterminate) || result.Claim != nil {
		t.Fatal("unconfirmed ownership granted:", err)
	}
	replay, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil || replay.Claim != nil || replay.Receipt.State != Pending {
		t.Fatal("ambiguous pending reservation replaced:", err)
	}
}

type delayedCAS struct {
	CAS
	afterRead  func()
	afterWrite func()
}

func (d delayedCAS) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	cells, err := d.CAS.ReadExact(ctx, c)
	if d.afterRead != nil {
		d.afterRead()
	}
	return cells, err
}
func (d delayedCAS) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	status, err := d.CAS.CompareAndMutate(ctx, m)
	if d.afterWrite != nil {
		d.afterWrite()
	}
	return status, err
}
func TestReservationLatencyCannotGrantExpiredOwnership(t *testing.T) {
	for _, mode := range []string{"read crosses deadline", "write crosses deadline", "write crosses lease"} {
		t.Run(mode, func(t *testing.T) {
			clock := now
			eng, reader, backend := openStore(t, t.TempDir(), true, func() time.Time { return clock })
			defer eng.Close()
			delayed := delayedCAS{CAS: backend}
			lease := 2 * time.Minute
			switch mode {
			case "read crosses deadline":
				delayed.afterRead = func() { clock = now.Add(time.Minute) }
			case "write crosses deadline":
				delayed.afterWrite = func() { clock = now.Add(time.Minute) }
			case "write crosses lease":
				lease = time.Second
				delayed.afterWrite = func() { clock = now.Add(time.Second) }
			}
			s, err := New(delayed, nil, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			request, _ := requestFixture(t)
			key := []byte(mode)
			reservation, err := s.Reserve(context.Background(), testScope, key, request, lease)
			if !errors.Is(err, ErrExpired) || reservation.Claim != nil {
				t.Fatalf("expired ownership: %#v %v", reservation, err)
			}
			stored, err := reader.Get(context.Background(), testScope, key, request)
			if mode == "read crosses deadline" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatal("expired request written:", err)
				}
			} else if err != nil || stored.State != Pending {
				t.Fatal("durable pending state lost:", err)
			}
		})
	}
}
func TestNewSuccessAfterDeadlineRejected(t *testing.T) {
	clock := now
	ctx := context.Background()
	eng, s, backend := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, result := requestFixture(t)
	key := []byte("late-success")
	reserved, err := s.Reserve(ctx, testScope, key, request, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	delayed, err := New(delayedCAS{CAS: backend, afterRead: func() { clock = now.Add(61 * time.Second) }}, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delayed.Commit(ctx, testScope, key, request, *reserved.Claim, result); !errors.Is(err, ErrExpired) {
		t.Fatal("late success admitted:", err)
	}
	stored, err := s.Get(ctx, testScope, key, request)
	if err != nil || stored.State != Pending {
		t.Fatal("rejection consumed claim")
	}
	result.Status = decision.Failed
	result.Answers = nil
	result.Reason = "timeout"
	result.CompletedAt = clock
	if _, err := s.Commit(ctx, testScope, key, request, *reserved.Claim, result); err != nil {
		t.Fatal("late failure rejected:", err)
	}
}
func TestReclaimedLeaseRejectsCompletionBeforeClaim(t *testing.T) {
	clock := now
	ctx := context.Background()
	eng, s, _ := openStore(t, t.TempDir(), true, func() time.Time { return clock })
	defer eng.Close()
	request, result := requestFixture(t)
	key := []byte("old-completion")
	if _, err := s.Reserve(ctx, testScope, key, request, time.Second); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Second)
	renewed, err := s.Reserve(ctx, testScope, key, request, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, testScope, key, request, *renewed.Claim, result); err == nil {
		t.Fatal("pre-claim result accepted")
	}
	result.CompletedAt = clock
	if _, err := s.Commit(ctx, testScope, key, request, *renewed.Claim, result); err != nil {
		t.Fatal("inclusive claim boundary rejected:", err)
	}
}
func TestReceiptTransitionsAcrossImmutableFormats(t *testing.T) {
	for _, format := range []engine.StorageFormat{engine.StorageFormatRFile, engine.StorageFormatParquet} {
		t.Run(string(format), func(t *testing.T) {
			ctx := context.Background()
			clock := now
			dir := t.TempDir()
			eng, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { eng.Close() }()
			if err := eng.CreateTable(Table, engine.TableOptions{FileFormat: format}); err != nil {
				t.Fatal(err)
			}
			bind := func() *Store {
				backend, err := explorercoord.NewEngineStore(eng, Table)
				if err != nil {
					t.Fatal(err)
				}
				s, err := New(backend, nil, func() time.Time { return clock })
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			reopen := func() {
				if err := eng.Close(); err != nil {
					t.Fatal(err)
				}
				eng, err = engine.Open(dir, engine.Options{})
				if err != nil {
					t.Fatal(err)
				}
			}
			s := bind()
			request, result := requestFixture(t)
			key := []byte("format")
			first, err := s.Reserve(ctx, testScope, key, request, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := eng.Flush(Table); err != nil {
				t.Fatal(err)
			}
			extension := ".rf"
			if format == engine.StorageFormatParquet {
				extension = ".parquet"
			}
			files := 0
			if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && filepath.Ext(path) == extension {
					files++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if files == 0 {
				t.Fatalf("flush produced no %s files", extension)
			}
			reopen()
			s = bind()
			clock = now.Add(2 * time.Second)
			renewed, err := s.Reserve(ctx, testScope, key, request, time.Minute)
			if err != nil || renewed.Claim == nil {
				t.Fatal("flushed reservation cannot be reclaimed:", err)
			}
			if _, err := s.Commit(ctx, testScope, key, request, *first.Claim, result); !errors.Is(err, ErrConflict) {
				t.Fatal("old owner survived flush/reopen:", err)
			}
			result.CompletedAt = clock
			receipt, err := s.Commit(ctx, testScope, key, request, *renewed.Claim, result)
			if err != nil {
				t.Fatal(err)
			}
			if err := eng.Flush(Table); err != nil {
				t.Fatal(err)
			}
			reopen()
			s = bind()
			replay, err := s.Reserve(ctx, testScope, key, request, time.Minute)
			if err != nil || replay.Claim != nil || replay.Receipt.PredictionID != receipt.PredictionID {
				t.Fatal("flushed committed replay failed:", err)
			}
		})
	}
}
