// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistrationstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func fixture() (Scope, Frozen, []byte, time.Time) {
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	blob := []byte(`{"trusted":"host-validated canonical artifact envelope"}`)
	s := Scope{Domain: []byte{255, 0}, SubjectID: shoal.ID(string([]byte{254, 0})), ActorID: shoal.ID(string([]byte{253, 0})), ClientID: shoal.ID(string([]byte{252, 0})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{251, 0}))}}
	f := Frozen{SelectionSHA256: strings.Repeat("a", 64), ProfileID: "profile", ProfileRevisionID: "profile-v1", BuilderID: "builder", RequestID: "request", TaskID: "task", PictureID: "picture", PredictorID: "predictor", Sources: []SourcePin{{Mode: collector.Imported, CollectorID: "collector", ObservationID: "observation", ArtifactID: "artifact", EnrollmentID: "enrollment", AuthorityPolicyID: "policy", Generation: 1, ArtifactSHA256: strings.Repeat("b", 64), SourceSHA256: strings.Repeat("c", 64), ReceivedAt: now.Add(-time.Second)}}, AcceptedAt: now, AuthenticationExpiresAt: now.Add(time.Hour), AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("d", 64), RecordSHA256: hash(blob), RecordBytes: len(blob)}
	return s, f, blob, now
}

type memoryCAS struct {
	mu        sync.Mutex
	cells     map[string]allocator.Cell
	writes    int
	failAt    int
	lostAt    int
	readFault bool
}

func (m *memoryCAS) ReadExact(ctx context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readFault {
		return nil, errors.New("read failure")
	}
	var out []allocator.Cell
	for _, c := range coords {
		if v, ok := m.cells[string(c.Row)]; ok {
			v.Value = bytes.Clone(v.Value)
			out = append(out, v)
		}
	}
	return out, nil
}
func (m *memoryCAS) CompareAndMutate(ctx context.Context, v allocator.Mutation) (allocator.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writes == m.failAt {
		m.readFault = true
		return allocator.StatusUnknown, errors.New("write unknown")
	}
	if m.cells == nil {
		m.cells = map[string]allocator.Cell{}
	}
	for _, c := range v.Conditions {
		old, ok := m.cells[string(c.Coordinate.Row)]
		if (c.Absent && ok) || (!c.Absent && (!ok || !bytes.Equal(c.Value, old.Value) || (c.TimestampSet && c.Timestamp != old.Timestamp))) {
			return allocator.StatusRejected, nil
		}
	}
	for _, u := range v.Updates {
		m.cells[string(u.Coordinate.Row)] = allocator.Cell{Coordinate: u.Coordinate, Timestamp: u.Timestamp, Value: bytes.Clone(u.Value)}
	}
	if m.writes == m.lostAt {
		m.readFault = true
		return allocator.StatusUnknown, errors.New("ack lost")
	}
	return allocator.StatusAccepted, nil
}
func openMemory(t *testing.T, m *memoryCAS, clock *time.Time) *Store {
	t.Helper()
	s, e := New(Config{Backend: m, Clock: func() time.Time { return *clock }})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestFreezeReadyAndOpaqueRestart(t *testing.T) {
	scope, f, b, now := fixture()
	ctx := context.Background()
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	key := []byte{0, 255}
	r, e := s.Begin(ctx, scope, key, f, b)
	if e != nil {
		t.Fatal(e)
	}
	if r.State != Preparing || r.Version != 1 {
		t.Fatal(r)
	}
	got, e := s.LookupRequest(ctx, scope.Domain, scope.SubjectID, f.RequestID)
	if e != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("lookup %v", e)
	}
	n, prepared, e := s.LoadPrepared(ctx, scope, key)
	if e != nil || !bytes.Equal(prepared, b) || !reflect.DeepEqual(n, r) {
		t.Fatal(e)
	}
	prepared[0] = '!'
	if _, again, e := s.LoadPrepared(ctx, scope, key); e != nil || !bytes.Equal(again, b) {
		t.Fatal("aliased blob", e)
	}
	calls := 0
	guard := func(_ context.Context, v Registration, raw []byte) error {
		calls++
		if v.FrozenSHA256 != r.FrozenSHA256 || !bytes.Equal(raw, b) {
			return errors.New("bad retained proof")
		}
		return nil
	}
	now = now.Add(time.Minute)
	ready, e := s.MarkReady(ctx, scope, key, r.FrozenSHA256, guard)
	if e != nil || ready.State != Ready || ready.Version != 2 || calls != 2 {
		t.Fatalf("ready %v calls%d", e, calls)
	}
	s = openMemory(t, m, &now)
	same, e := s.MarkReady(ctx, scope, key, r.FrozenSHA256, guard)
	if e != nil || !reflect.DeepEqual(same, ready) || calls != 3 {
		t.Fatalf("ready replay %v", e)
	}
	old := f
	old.AcceptedAt = now
	if _, e = s.Begin(ctx, scope, key, old, b); !errors.Is(e, ErrConflict) {
		t.Fatalf("rebuilt original %v", e)
	}
	if _, e = s.MarkReady(ctx, scope, key, r.FrozenSHA256, func(context.Context, Registration, []byte) error { return errors.New("revoked") }); e == nil {
		t.Fatal("ready bypassed guard")
	}
}
func TestCrashEveryWriteRepairsExactPreparation(t *testing.T) {
	for stage := 1; stage <= 4; stage++ {
		for _, accepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%t", stage, accepted), func(t *testing.T) {
				scope, f, b, now := fixture()
				ctx := context.Background()
				m := &memoryCAS{}
				if accepted {
					m.lostAt = stage
				} else {
					m.failAt = stage
				}
				s := openMemory(t, m, &now)
				key := []byte("key")
				guard := func(context.Context, Registration, []byte) error { return nil }
				r, e := s.Begin(ctx, scope, key, f, b)
				if stage == 4 && e == nil {
					_, e = s.MarkReady(ctx, scope, key, r.FrozenSHA256, guard)
				}
				if !errors.Is(e, ErrIndeterminate) {
					t.Fatalf("lost uncertainty: %v", e)
				}
				m.readFault = false
				m.failAt = 0
				m.lostAt = 0
				r, e = s.Begin(ctx, scope, key, f, b)
				if e != nil {
					t.Fatal(e)
				}
				ready, e := s.MarkReady(ctx, scope, key, r.FrozenSHA256, guard)
				if e != nil || ready.State != Ready {
					t.Fatal(e)
				}
			})
		}
	}
}
func TestAliasScopeConflictMutationAndNoAbort(t *testing.T) {
	scope, f, b, now := fixture()
	ctx := context.Background()
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	r, e := s.Begin(ctx, scope, []byte("one"), f, b)
	if e != nil {
		t.Fatal(e)
	}
	other := scope
	other.ActorID = "different"
	if _, e = s.Begin(ctx, other, []byte("one"), f, b); !errors.Is(e, ErrConflict) || !errors.Is(e, ErrIndeterminate) {
		t.Fatalf("alias takeover %v", e)
	}
	winner, e := s.LookupRequest(ctx, scope.Domain, scope.SubjectID, f.RequestID)
	if e != nil || winner.ID != r.ID {
		t.Fatal("alias overwritten", e)
	}
	for _, guard := range []VerifyRetention{func(_ context.Context, r Registration, b []byte) error { r.Scope.Domain[0]++; return nil }, func(_ context.Context, r Registration, b []byte) error { r.Frozen.Sources[0].Generation++; return nil }, func(_ context.Context, r Registration, b []byte) error { b[0]++; return nil }} {
		if _, e = s.MarkReady(ctx, scope, []byte("one"), r.FrozenSHA256, guard); !errors.Is(e, ErrConflict) {
			t.Fatalf("callback mutation %v", e)
		}
	}
	now = now.Add(365 * 24 * time.Hour)
	got, e := s.ReadByKey(ctx, scope, []byte("one"))
	if e != nil || got.State != Preparing {
		t.Fatal("expired preparation", e)
	}
	other = scope
	other.Domain = []byte("other")
	if _, e = s.ReadByKey(ctx, other, []byte("one")); !errors.Is(e, ErrNotFound) {
		t.Fatal("domain alias", e)
	}
}
func TestInvalidBoundsCorruptionAndCancellation(t *testing.T) {
	scope, f, b, now := fixture()
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.Begin(ctx, scope, []byte("key"), f, b); e == nil || m.writes != 0 {
		t.Fatal("cancel wrote")
	}
	if _, e := s.Begin(context.Background(), scope, make([]byte, 1025), f, b); e == nil || m.writes != 0 {
		t.Fatal("oversize key")
	}
	bad := f
	bad.Sources = make([]SourcePin, 257)
	if _, e := s.Begin(context.Background(), scope, []byte("key"), bad, b); e == nil || m.writes != 0 {
		t.Fatal("oversize sources")
	}
	r, e := s.Begin(context.Background(), scope, []byte("key"), f, b)
	if e != nil {
		t.Fatal(e)
	}
	coord := s.primaryCoord(r.ID)
	cell := m.cells[string(coord.Row)]
	cell.Value = bytes.Replace(cell.Value, []byte(`"Version":1`), []byte(`"Version":2`), 1)
	m.cells[string(coord.Row)] = cell
	if _, e = s.ReadByKey(context.Background(), scope, []byte("key")); !errors.Is(e, ErrCorrupt) {
		t.Fatal("corrupt primary", e)
	}
}
func TestPhysicalRFileFrozenPreparationAndReady(t *testing.T) {
	scope, f, b, now := fixture()
	dir := filepath.Join(t.TempDir(), "engine")
	open := func() (*engine.Engine, *Store) {
		eng, e := engine.Open(dir, engine.Options{})
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, name := range eng.TableNames() {
			found = found || name == Table
		}
		if !found {
			if e = eng.CreateTable(Table, engine.TableOptions{}); e != nil {
				t.Fatal(e)
			}
		}
		backend, e := explorercoord.NewEngineStore(eng, Table)
		if e != nil {
			t.Fatal(e)
		}
		s, e := New(Config{Backend: backend, Clock: func() time.Time { return now }})
		if e != nil {
			t.Fatal(e)
		}
		return eng, s
	}
	flushClose := func(eng *engine.Engine) {
		if e := eng.Flush(Table); e != nil {
			t.Fatal(e)
		}
		if e := eng.Close(); e != nil {
			t.Fatal(e)
		}
	}
	eng, s := open()
	ctx := context.Background()
	r, e := s.Begin(ctx, scope, []byte("key"), f, b)
	if e != nil {
		t.Fatal(e)
	}
	flushClose(eng)
	eng, s = open()
	got, raw, e := s.LoadPrepared(ctx, scope, []byte("key"))
	if e != nil || !reflect.DeepEqual(r, got) || !bytes.Equal(raw, b) {
		t.Fatal("preparation lost", e)
	}
	ready, e := s.MarkReady(ctx, scope, []byte("key"), r.FrozenSHA256, func(context.Context, Registration, []byte) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	flushClose(eng)
	eng, s = open()
	defer eng.Close()
	got, e = s.LookupRequest(ctx, scope.Domain, scope.SubjectID, f.RequestID)
	if e != nil || !reflect.DeepEqual(got, ready) {
		t.Fatal("ready lost", e)
	}
}

var _ decisionstore.CAS = (*memoryCAS)(nil)

func TestConcurrentExactRetriesAndOptionalOpaqueActor(t *testing.T) {
	scope, f, b, now := fixture()
	scope.ActorID = ""
	scope.ClientID = ""
	scope.OnBehalfOf = []shoal.ID{}
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	ctx := context.Background()
	const count = 8
	results := make(chan Registration, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Begin(ctx, scope, []byte("shared"), f, b)
			if e == nil {
				r, e = s.MarkReady(ctx, scope, []byte("shared"), r.FrozenSHA256, func(context.Context, Registration, []byte) error { return nil })
			}
			results <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first Registration
	for r := range results {
		if first.ID == "" {
			first = r
		}
		if !reflect.DeepEqual(first, r) || r.State != Ready || r.Scope.ActorID != "" || r.Scope.ClientID != "" {
			t.Fatal("different exact winner")
		}
	}
}

func TestSourceAcquisitionModeIsMandatoryAndFrozen(t *testing.T) {
	scope, f, b, now := fixture()
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	ctx := context.Background()
	for _, mode := range []collector.Mode{"", "unknown"} {
		bad := f
		bad.Sources = append([]SourcePin(nil), f.Sources...)
		bad.Sources[0].Mode = mode
		if _, e := s.Begin(ctx, scope, []byte("key"), bad, b); e == nil || m.writes != 0 {
			t.Fatalf("invalid mode %q persisted: %v", mode, e)
		}
	}
	imported, e := s.Begin(ctx, scope, []byte("key"), f, b)
	if e != nil {
		t.Fatal(e)
	}
	observed := f
	observed.Sources = append([]SourcePin(nil), f.Sources...)
	observed.Sources[0].Mode = collector.ServerObserved
	if _, e = s.Begin(ctx, scope, []byte("key"), observed, b); !errors.Is(e, ErrConflict) {
		t.Fatalf("mode substitution accepted: %v", e)
	}
	got, _, e := s.LoadPrepared(ctx, scope, []byte("key"))
	if e != nil || got.Frozen.Sources[0].Mode != collector.Imported || got.FrozenSHA256 != imported.FrozenSHA256 {
		t.Fatalf("original acquisition mode lost: %v", e)
	}
	scope.SubjectID = "other-subject"
	separate, e := s.Begin(ctx, scope, []byte("key"), observed, b)
	if e != nil || separate.FrozenSHA256 == imported.FrozenSHA256 {
		t.Fatalf("modes collapse: %v", e)
	}
}
