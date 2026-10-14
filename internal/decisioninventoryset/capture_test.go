// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventoryset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

type setCAS struct {
	mu            sync.Mutex
	cells         map[string]allocator.Cell
	reads, writes int
	afterRead     func(int)
	readErr       bool
}

func (b *setCAS) ReadExact(_ context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	b.mu.Lock()
	b.reads++
	n := b.reads
	var rows []allocator.Cell
	for _, c := range coords {
		if cell, ok := b.cells[string(c.Row)]; ok {
			cell.Value = append([]byte(nil), cell.Value...)
			rows = append(rows, cell)
		}
	}
	hook, fail := b.afterRead, b.readErr
	b.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if fail {
		return nil, errors.New("unavailable")
	}
	return rows, nil
}
func (b *setCAS) CompareAndMutate(_ context.Context, m allocator.Mutation) (allocator.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cells == nil {
		b.cells = map[string]allocator.Cell{}
	}
	for _, c := range m.Conditions {
		old, ok := b.cells[string(c.Coordinate.Row)]
		if c.Absent && ok || !c.Absent && (!ok || !bytes.Equal(old.Value, c.Value) || c.TimestampSet && c.Timestamp != old.Timestamp) {
			return allocator.StatusRejected, nil
		}
	}
	for _, u := range m.Updates {
		b.cells[string(u.Coordinate.Row)] = allocator.Cell{Coordinate: u.Coordinate, Timestamp: u.Timestamp, Value: append([]byte(nil), u.Value...)}
	}
	b.writes++
	return allocator.StatusAccepted, nil
}
func fixture(t *testing.T, n int) (*Reader, *inventory.Store, *setCAS, inventory.Scope, []inventory.Binding, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	backend := &setCAS{}
	store, e := inventory.New(inventory.Config{Backend: backend, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	scope := inventory.Scope{Domain: []byte{0, 255}}
	bindings := []inventory.Binding{}
	for i := 0; i < n; i++ {
		subject := shoal.ID(fmt.Sprintf("subject%d", i))
		target, e := decision.AdjudicationTargetID("task", "picture", subject, "question")
		if e != nil {
			t.Fatal(e)
		}
		b := inventory.Binding{CoverageID: "coverage", TargetID: target, TaskID: "task", PictureID: "picture", SubjectID: subject, QuestionID: "question"}
		if _, e = store.Register(context.Background(), scope, b); e != nil {
			t.Fatal(e)
		}
		bindings = append(bindings, b)
	}
	reader, e := New(Config{Store: store, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	backend.reads = 0
	return reader, store, backend, scope, bindings, now
}
func report(b inventory.Binding, now time.Time) (inventory.Intent, outcomes.Receipt) {
	i := inventory.Intent{ReceiptID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: "observation", RequestID: "request", PredictionID: "prediction", Reporter: inventory.Attribution{SubjectID: shoal.ID(string([]byte{255})), ActorID: "actor"}}
	r := outcomes.Receipt{ID: i.ReceiptID, ObservationID: i.ObservationID, ObservationConfig: decision.OutcomeObservationConfig{RequestID: i.RequestID, PredictionID: i.PredictionID, SubjectID: b.SubjectID, Kind: decision.OutcomeCorrectness, QuestionID: b.QuestionID, Label: "yes", EvidenceIDs: []shoal.ID{"evidence"}, ObservedAt: now}, SubmitterID: i.Reporter.SubjectID, ActorID: i.Reporter.ActorID, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("b", 64), ReceivedAt: now, State: "proposed"}
	return i, r
}
func TestStableCaptureCanonicalOrderAndReadOnly(t *testing.T) {
	r, _, backend, scope, b, now := fixture(t, 3)
	writes := backend.writes
	first, e := r.CaptureSet(context.Background(), scope, b)
	if e != nil {
		t.Fatal(e)
	}
	if backend.reads != 6 || backend.writes != writes {
		t.Fatal("capture did not use exactly two read-only passes")
	}
	b[0], b[2] = b[2], b[0]
	r.config.Clock = func() time.Time { return now.Add(time.Minute) }
	second, e := r.CaptureSet(context.Background(), scope, b)
	if e != nil || second.VectorID != first.VectorID || !reflect.DeepEqual(first.Snapshots, second.Snapshots) || first.Window == second.Window {
		t.Fatal("input order or clock changed vector identity", e)
	}
	for i := 1; i < len(first.Snapshots); i++ {
		if first.Snapshots[i-1].Binding.TargetID >= first.Snapshots[i].Binding.TargetID {
			t.Fatal("not canonically sorted")
		}
	}
}
func TestPendingAndCompletedTransitionAreDetected(t *testing.T) {
	for _, publish := range []bool{false, true} {
		r, s, backend, scope, b, now := fixture(t, 2)
		backend.afterRead = func(n int) {
			if n != 2 {
				return
			}
			backend.afterRead = nil
			i, receipt := report(b[0], now)
			if _, e := s.Begin(context.Background(), scope, b[0], i); e != nil {
				t.Fatal(e)
			}
			if publish {
				if _, e := s.Publish(context.Background(), scope, b[0], i, receipt); e != nil {
					t.Fatal(e)
				}
			}
		}
		got, e := r.CaptureSet(context.Background(), scope, b)
		if publish && !errors.Is(e, ErrChanged) || !publish && !errors.Is(e, ErrIncomplete) || !reflect.DeepEqual(got, Capture{}) {
			t.Fatal("changed or pending set disclosed", publish, e)
		}
	}
}
func TestMutationAfterEstablishedCutDoesNotClaimDeliveryFreshness(t *testing.T) {
	r, s, backend, scope, b, now := fixture(t, 2)
	sorted := append([]inventory.Binding(nil), b...)
	if sorted[0].TargetID > sorted[1].TargetID {
		sorted[0], sorted[1] = sorted[1], sorted[0]
	}
	backend.afterRead = func(n int) {
		if n != 4 {
			return
		}
		backend.afterRead = nil
		i, _ := report(sorted[0], now)
		if _, e := s.Begin(context.Background(), scope, sorted[0], i); e != nil {
			t.Fatal(e)
		}
	}
	capture, e := r.CaptureSet(context.Background(), scope, b)
	if e != nil || len(capture.Snapshots) != 2 {
		t.Fatal("historical stable cut unavailable", e)
	}
	current, e := s.Load(context.Background(), scope, sorted[0])
	if e != nil || current.Complete() || capture.Snapshots[0].ID == current.ID {
		t.Fatal("test did not distinguish historical cut from current state", e)
	}
}
func TestBoundsStopFurtherReadsWithoutPartialOutput(t *testing.T) {
	r, _, backend, scope, b, _ := fixture(t, 3)
	base := len(b)*int(unsafe.Sizeof(inventory.Snapshot{})) + len(b)*int(unsafe.Sizeof(inventory.Binding{})) + len(scope.Domain)
	for _, v := range b {
		base += bindingBytes(v)
	}
	snapshot, e := r.config.Store.Load(context.Background(), scope, b[0])
	if e != nil {
		t.Fatal(e)
	}
	backend.reads = 0
	capture, e := r.captureSet(context.Background(), scope, b, base+snapshotBytes(snapshot)+1)
	if !errors.Is(e, ErrLimit) || backend.reads != 2 || !reflect.DeepEqual(capture, Capture{}) {
		t.Fatal("budget failure loaded remaining targets or returned partial output", backend.reads, e)
	}
	huge := inventory.Snapshot{Entries: make([]inventory.Entry, 1)}
	huge.Entries[0].Intent.Reporter.OnBehalfOf = []shoal.ID{shoal.ID(strings.Repeat("x", MaxRetainedBytes))}
	if snapshotBytes(huge) <= MaxRetainedBytes {
		t.Fatal("string payload omitted from byte accounting")
	}
	if _, e = r.CaptureSet(context.Background(), scope, append(b, b[0])); e == nil {
		t.Fatal("duplicate target admitted")
	}
	if _, e = r.CaptureSet(context.Background(), scope, nil); e == nil {
		t.Fatal("empty capture admitted")
	}
	if _, e = r.CaptureSet(context.Background(), scope, make([]inventory.Binding, MaxTargets+1)); e == nil {
		t.Fatal("target bound omitted")
	}
}
func TestCancellationFailureAndClockRollback(t *testing.T) {
	for _, mode := range []string{"cancel", "read", "clock"} {
		r, _, backend, scope, b, now := fixture(t, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		switch mode {
		case "cancel":
			backend.afterRead = func(n int) {
				if n == 2 {
					cancel()
				}
			}
		case "read":
			backend.readErr = true
		case "clock":
			calls := 0
			r.config.Clock = func() time.Time {
				calls++
				if calls >= 3 {
					return now.Add(-time.Second)
				}
				return now
			}
		}
		capture, e := r.CaptureSet(ctx, scope, b)
		if e == nil || !reflect.DeepEqual(capture, Capture{}) {
			t.Fatal("failed collection returned material", mode, e)
		}
	}
}
func TestPhysicalRFileRestartRetainsVectorAndPending(t *testing.T) {
	_, _, _, scope, b, now := fixture(t, 2)
	dir := filepath.Join(t.TempDir(), "engine")
	open := func(create bool) (*engine.Engine, *inventory.Store, *Reader) {
		eng, e := engine.Open(dir, engine.Options{})
		if e != nil {
			t.Fatal(e)
		}
		if create {
			if e = eng.CreateTable(inventory.Table, engine.TableOptions{}); e != nil {
				t.Fatal(e)
			}
		}
		backend, e := explorercoord.NewEngineStore(eng, inventory.Table)
		if e != nil {
			t.Fatal(e)
		}
		s, e := inventory.New(inventory.Config{Backend: backend, Clock: func() time.Time { return now }})
		if e != nil {
			t.Fatal(e)
		}
		r, e := New(Config{Store: s, Clock: func() time.Time { return now }})
		if e != nil {
			t.Fatal(e)
		}
		return eng, s, r
	}
	ctx := context.Background()
	eng, s, r := open(true)
	for _, binding := range b {
		if _, e := s.Register(ctx, scope, binding); e != nil {
			t.Fatal(e)
		}
	}
	before, e := r.CaptureSet(ctx, scope, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = eng.Flush(inventory.Table); e != nil {
		t.Fatal(e)
	}
	if e = eng.Close(); e != nil {
		t.Fatal(e)
	}
	eng, s, r = open(false)
	after, e := r.CaptureSet(ctx, scope, b)
	if e != nil || before.VectorID != after.VectorID {
		t.Fatal("physical restart changed vector", e)
	}
	i, _ := report(b[0], now)
	if _, e = s.Begin(ctx, scope, b[0], i); e != nil {
		t.Fatal(e)
	}
	if e = eng.Flush(inventory.Table); e != nil {
		t.Fatal(e)
	}
	if e = eng.Close(); e != nil {
		t.Fatal(e)
	}
	eng, _, r = open(false)
	defer eng.Close()
	r.config.Clock = func() time.Time { return now.Add(365 * 24 * time.Hour) }
	if capture, e := r.CaptureSet(ctx, scope, b); !errors.Is(e, ErrIncomplete) || !reflect.DeepEqual(capture, Capture{}) {
		t.Fatal("pending expired on restart", e)
	}
}

func TestMixedFirstPassAcrossTargetsCannotCertifyCut(t *testing.T) {
	r, s, backend, scope, b, now := fixture(t, 2)
	backend.afterRead = func(n int) {
		if n != 1 {
			return
		}
		backend.afterRead = nil
		for _, binding := range b {
			i, receipt := report(binding, now)
			if _, e := s.Begin(context.Background(), scope, binding, i); e != nil {
				t.Fatal(e)
			}
			if _, e := s.Publish(context.Background(), scope, binding, i, receipt); e != nil {
				t.Fatal(e)
			}
		}
	}
	capture, e := r.CaptureSet(context.Background(), scope, b)
	if !errors.Is(e, ErrChanged) || !reflect.DeepEqual(capture, Capture{}) {
		t.Fatal("mixed first pass certified", e)
	}
}
