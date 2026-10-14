// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type adversarialCAS struct {
	mu       sync.Mutex
	cells    map[string]allocator.Cell
	before   func()
	after    func()
	unknown  bool
	failRead bool
}

func (b *adversarialCAS) ReadExact(_ context.Context, cs []allocator.Coordinate) ([]allocator.Cell, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failRead {
		return nil, errors.New("lost readback")
	}
	var out []allocator.Cell
	for _, c := range cs {
		if cell, ok := b.cells[string(c.Row)]; ok {
			cell.Value = bytes.Clone(cell.Value)
			out = append(out, cell)
		}
	}
	return out, nil
}
func (b *adversarialCAS) CompareAndMutate(_ context.Context, m allocator.Mutation) (allocator.Status, error) {
	if b.before != nil {
		b.before()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	old, exists := b.cells[string(m.Row)]
	for _, c := range m.Conditions {
		if c.Absent && exists || !c.Absent && (!exists || !bytes.Equal(c.Value, old.Value) || c.TimestampSet && c.Timestamp != old.Timestamp) {
			return allocator.StatusRejected, nil
		}
	}
	if b.cells == nil {
		b.cells = map[string]allocator.Cell{}
	}
	u := m.Updates[0]
	b.cells[string(m.Row)] = allocator.Cell{Coordinate: u.Coordinate, Timestamp: u.Timestamp, Value: bytes.Clone(u.Value)}
	if b.after != nil {
		b.after()
	}
	if b.unknown {
		return allocator.StatusUnknown, errors.New("lost ack")
	}
	return allocator.StatusAccepted, nil
}
func adversarialFixture(t *testing.T) (*Store, *adversarialCAS, Scope, Binding, Intent, outcomes.Receipt) {
	t.Helper()
	backend := &adversarialCAS{}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	store, err := New(Config{Backend: backend, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	target, err := decision.AdjudicationTargetID("task", "picture", "subject", "question")
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{CoverageID: "trusted-closed-namespace:1", TargetID: target, TaskID: "task", PictureID: "picture", SubjectID: "subject", QuestionID: "question"}
	a := Attribution{SubjectID: shoal.ID(string([]byte{255, 0})), ActorID: shoal.ID(string([]byte{254, 1})), ClientID: shoal.ID(string([]byte{253})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{252, 0}))}}
	intent := Intent{ReceiptID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: "outcome-observation:1", RequestID: "request", PredictionID: "prediction", Reporter: a}
	receipt := outcomes.Receipt{ID: intent.ReceiptID, ObservationID: intent.ObservationID, ObservationConfig: decision.OutcomeObservationConfig{RequestID: intent.RequestID, PredictionID: intent.PredictionID, SubjectID: binding.SubjectID, QuestionID: binding.QuestionID, Kind: decision.OutcomeCorrectness, Label: "positive", EvidenceIDs: []shoal.ID{"evidence"}, ObservedAt: now.Add(-time.Hour), AssertedProvenance: decision.OutcomeProvenance{ReporterID: "claimed-reporter"}}, SubmitterID: a.SubjectID, ActorID: a.ActorID, ClientID: a.ClientID, OnBehalfOf: a.OnBehalfOf, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("b", 64), ReceivedAt: now.Add(-time.Minute), State: "proposed"}
	return store, backend, Scope{Domain: []byte{0, 255}}, binding, intent, receipt
}
func TestAdversarialCoveragePendingAndOriginalReceipt(t *testing.T) {
	s, b, scope, binding, intent, receipt := adversarialFixture(t)
	ctx := context.Background()
	if _, err := s.Load(ctx, scope, binding); err == nil {
		t.Fatal("absence certified complete")
	}
	if _, err := s.Begin(ctx, scope, binding, intent); err == nil {
		t.Fatal("Begin auto-registered coverage")
	}
	registered, err := s.Register(ctx, scope, binding)
	if err != nil || !registered.Complete() {
		t.Fatalf("registration: %v", err)
	}
	pending, err := s.Begin(ctx, scope, binding, intent)
	if err != nil || pending.Complete() || pending.Version <= registered.Version {
		t.Fatalf("pending: %v", err)
	}
	// Simulated process restart retains the exact pending journal without a TTL.
	restarted, err := New(Config{Backend: b, Clock: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	after, err := restarted.Load(ctx, scope, binding)
	if err != nil || after.Complete() || after.ID != pending.ID {
		t.Fatalf("pending expired on restart: %v", err)
	}
	replay, err := s.Begin(ctx, scope, binding, intent)
	if err != nil || replay.ID != pending.ID {
		t.Fatal("exact Begin changed generation")
	}
	changed := intent
	changed.ObservationID = "other"
	if _, err = s.Begin(ctx, scope, binding, changed); err == nil {
		t.Fatal("intent silently rebound")
	}
	// Original receipt may predate pending registration during authorized recovery.
	published, err := s.Publish(ctx, scope, binding, intent, receipt)
	if err != nil || !published.Complete() || published.Version <= pending.Version {
		t.Fatalf("publication of original receipt: %v", err)
	}
	again, err := s.Publish(ctx, scope, binding, intent, receipt)
	if err != nil || again.ID != published.ID {
		t.Fatal("exact publication changed generation")
	}
	for _, change := range []func(*outcomes.Receipt){func(r *outcomes.Receipt) { r.AuthorizationFingerprint = "auth-sha256:" + strings.Repeat("c", 64) }, func(r *outcomes.Receipt) { r.ReceivedAt = r.ReceivedAt.Add(time.Second) }, func(r *outcomes.Receipt) { r.ObservationConfig.Label = "negative" }, func(r *outcomes.Receipt) { r.ActorID = "substitute" }} {
		altered := receipt
		change(&altered)
		if _, err = s.Publish(ctx, scope, binding, intent, altered); err == nil {
			t.Fatal("published receipt rewritten")
		}
	}
	final, err := s.Load(ctx, scope, binding)
	if err != nil || final.ID != published.ID {
		t.Fatal("failed publication mutated snapshot")
	}
	other := binding
	other.CoverageID = "new-coverage"
	if _, err = s.Register(ctx, scope, other); err == nil {
		t.Fatal("coverage silently rebound")
	}
	if _, err = s.Load(ctx, Scope{Domain: []byte("other")}, binding); err == nil {
		t.Fatal("cross-domain inventory disclosed")
	}
}
func TestAdversarialConcurrentReportersCannotLoseIntent(t *testing.T) {
	s, b, scope, binding, a, _ := adversarialFixture(t)
	ctx := context.Background()
	if _, err := s.Register(ctx, scope, binding); err != nil {
		t.Fatal(err)
	}
	other := a
	other.ReceiptID = shoal.ID("outcome-receipt:" + strings.Repeat("c", 64))
	other.ObservationID = "observation:other"
	other.RequestID = "request:other"
	other.PredictionID = "prediction:other"
	other.Reporter = Attribution{SubjectID: "other", ActorID: "other-actor"}
	var barrier sync.WaitGroup
	barrier.Add(2)
	b.before = func() { barrier.Done(); barrier.Wait() }
	var workers sync.WaitGroup
	workers.Add(2)
	for _, intent := range []Intent{a, other} {
		go func(i Intent) { defer workers.Done(); _, _ = s.Begin(ctx, scope, binding, i) }(intent)
	}
	workers.Wait()
	b.before = nil
	for _, intent := range []Intent{a, other} {
		if _, err := s.Begin(ctx, scope, binding, intent); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load(ctx, scope, binding)
	if err != nil || len(got.Entries) != 2 || got.Complete() {
		t.Fatalf("lost cross-principal intent: %+v %v", got, err)
	}
}
func TestAdversarialUnknownAcknowledgementReconcilesOnlyExactBytes(t *testing.T) {
	s, b, scope, binding, intent, receipt := adversarialFixture(t)
	ctx := context.Background()
	if _, err := s.Register(ctx, scope, binding); err != nil {
		t.Fatal(err)
	}
	b.unknown = true
	pending, err := s.Begin(ctx, scope, binding, intent)
	if err != nil || pending.Complete() {
		t.Fatalf("exact pending readback not reconciled: %v", err)
	}
	published, err := s.Publish(ctx, scope, binding, intent, receipt)
	if err != nil || !published.Complete() {
		t.Fatalf("exact receipt readback not reconciled: %v", err)
	}
	b.mu.Lock()
	for key, cell := range b.cells {
		cell.Value = []byte(`{}`)
		b.cells[key] = cell
	}
	b.mu.Unlock()
	if _, err = s.Load(ctx, scope, binding); err == nil {
		t.Fatal("corruption became empty complete inventory")
	}
}

func TestAdversarialMissingAcknowledgementNeverCertifiesRollback(t *testing.T) {
	s, b, scope, binding, intent, receipt := adversarialFixture(t)
	ctx := context.Background()
	if _, err := s.Register(ctx, scope, binding); err != nil {
		t.Fatal(err)
	}
	b.after = func() { b.failRead = true }
	if got, err := s.Begin(ctx, scope, binding, intent); !errors.Is(err, ErrIndeterminate) || got.Complete() {
		t.Fatalf("lost pending acknowledgement: %+v %v", got, err)
	}
	b.after = nil
	b.failRead = false
	pending, err := s.Load(ctx, scope, binding)
	if err != nil || pending.Complete() || len(pending.Entries) != 1 {
		t.Fatalf("durable pending missing: %v", err)
	}
	b.after = func() { b.failRead = true }
	if got, err := s.Publish(ctx, scope, binding, intent, receipt); !errors.Is(err, ErrIndeterminate) || got.Complete() {
		t.Fatalf("lost publication acknowledgement: %+v %v", got, err)
	}
	b.after = nil
	b.failRead = false
	published, err := s.Publish(ctx, scope, binding, intent, receipt)
	if err != nil || !published.Complete() || published.Version != pending.Version+1 {
		t.Fatalf("publication not exactly reconciled: %v", err)
	}
}
