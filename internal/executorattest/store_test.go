// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package executorattest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type storeEnv struct {
	store   *Store
	trust   *atomic.Pointer[Trust]
	backend decisionstore.CAS
	eng     *engine.Engine
	dir     string
}

func openEngine(t *testing.T, dir string, create bool) (*engine.Engine, decisionstore.CAS) {
	t.Helper()
	eng, e := engine.Open(dir, engine.Options{})
	if e != nil {
		t.Fatal(e)
	}
	if create {
		if e = eng.CreateTable(Table, engine.TableOptions{}); e != nil {
			_ = eng.Close()
			t.Fatal(e)
		}
	}
	backend, e := explorercoord.NewEngineStore(eng, Table)
	if e != nil {
		_ = eng.Close()
		t.Fatal(e)
	}
	return eng, backend
}

func newStoreEnv(t *testing.T) *storeEnv {
	t.Helper()
	dir := t.TempDir()
	eng, backend := openEngine(t, dir, true)
	t.Cleanup(func() { _ = eng.Close() })
	trust := &atomic.Pointer[Trust]{}
	trust.Store(trustWith(t, nil))
	s, e := NewStore(StoreConfig{Backend: backend, Trust: trust.Load})
	if e != nil {
		t.Fatal(e)
	}
	return &storeEnv{store: s, trust: trust, backend: backend, eng: eng, dir: dir}
}

func (v *storeEnv) present(t *testing.T, want Expectation, s Statement) (Record, error) {
	t.Helper()
	_, private := keys('a')
	return v.store.Present(context.Background(), want, sign(t, private, s))
}

func TestPresentAndCurrent(t *testing.T) {
	v := newStoreEnv(t)
	want := expectation()
	s := statement(want)
	record, e := v.present(t, want, s)
	if e != nil {
		t.Fatal(e)
	}
	id, expires, ok, e := v.store.Current(context.Background(), want.Principal, ref, now, now.Add(10*time.Minute))
	if e != nil || !ok || id != record.AttestationID || !expires.Equal(s.ExpiresAt) || record.VerifierID != verifierID || record.ImageDigest != image {
		t.Fatalf("current: %q %v %v %v (%+v)", id, expires, ok, e, record)
	}
	// Idempotent on the same statement, even under a later clock.
	later := want
	later.Now = now.Add(time.Minute)
	again, e := v.present(t, later, s)
	if e != nil || again != record {
		t.Fatalf("re-present: %+v %v", again, e)
	}
	// Another principal or ref has no row.
	other := want.Principal
	other.Subject = "worker:2"
	if _, _, ok, e := v.store.Current(context.Background(), other, ref, now, now); ok || e != nil {
		t.Fatalf("other principal: %v %v", ok, e)
	}
}

func TestNeedUntilBoundary(t *testing.T) {
	v := newStoreEnv(t)
	want := expectation()
	s := statement(want)
	if _, e := v.present(t, want, s); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if _, _, ok, _ := v.store.Current(ctx, want.Principal, ref, now, s.ExpiresAt); !ok {
		t.Fatal("needUntil == ExpiresAt refused")
	}
	if _, _, ok, _ := v.store.Current(ctx, want.Principal, ref, now, s.ExpiresAt.Add(time.Nanosecond)); ok {
		t.Fatal("needUntil after ExpiresAt accepted")
	}
	if _, _, ok, _ := v.store.Current(ctx, want.Principal, ref, s.ExpiresAt, s.ExpiresAt); ok {
		t.Fatal("expired at now accepted")
	}
}

func TestReplayAndRollback(t *testing.T) {
	v := newStoreEnv(t)
	want := expectation()
	ctx := context.Background()
	newer := StatementFor(want, image, now, now.Add(30*time.Minute))
	first, e := v.present(t, want, newer)
	if e != nil {
		t.Fatal(e)
	}
	// The same statement presented under a different idempotency key.
	replay := want
	replay.Key = []byte("present-2")
	if _, e := v.present(t, replay, newer); !reasonIs(e, ReasonNonceMismatch) {
		t.Fatalf("replay: %v", e)
	}
	// An older, otherwise valid statement after a newer one.
	older := StatementFor(want, image, now.Add(-2*time.Minute), now.Add(40*time.Minute))
	if _, e := v.present(t, want, older); !reasonIs(e, ReasonRollback) {
		t.Fatalf("rollback: %v", e)
	}
	// Equal IssuedAt, different statement: not strictly greater.
	same := StatementFor(want, image, now, now.Add(20*time.Minute))
	if _, e := v.present(t, want, same); !reasonIs(e, ReasonRollback) {
		t.Fatalf("equal issued_at: %v", e)
	}
	id, _, ok, _ := v.store.Current(ctx, want.Principal, ref, now, now)
	if !ok || id != first.AttestationID {
		t.Fatal("rollback changed the row")
	}
	// A strictly newer statement replaces it.
	later := want
	later.Now = now.Add(time.Minute)
	next, e := v.present(t, later, StatementFor(want, image, now.Add(time.Minute), now.Add(50*time.Minute)))
	if e != nil || next.AttestationID == first.AttestationID {
		t.Fatalf("advance: %+v %v", next, e)
	}
	if id, _, ok, _ := v.store.Current(ctx, want.Principal, ref, later.Now, now.Add(45*time.Minute)); !ok || id != next.AttestationID {
		t.Fatal("advance not recorded")
	}
}

func TestAnotherPrincipalsStatementRefused(t *testing.T) {
	v := newStoreEnv(t)
	victim := expectation()
	attacker := victim
	attacker.Principal = Principal{Domain: []byte("domain"), Subject: "worker:2", ClientID: "client"}
	if _, e := v.present(t, attacker, statement(victim)); !reasonIs(e, ReasonSubjectMismatch) {
		t.Fatalf("got %v", e)
	}
	for _, p := range []Principal{attacker.Principal, victim.Principal} {
		if _, _, ok, e := v.store.Current(context.Background(), p, ref, now, now); ok || e != nil {
			t.Fatalf("row written for %s: %v %v", p.Subject, ok, e)
		}
	}
}

func TestTrustRemovalTakesEffectInCurrent(t *testing.T) {
	public, _ := keys('a')
	rotated, _ := keys('b')
	for name, mutate := range map[string]func(*ExecutorTrust){
		"image digest unpinned": func(x *ExecutorTrust) { x.ImageDigests = []string{otherImage} },
		"verifier removed": func(x *ExecutorTrust) {
			x.Verifiers = []VerifierTrust{{ID: "operator-key:2", PublicKey: rotated, MaxValidity: time.Hour}}
		},
		"key rotated under same ID": func(x *ExecutorTrust) {
			x.Verifiers = []VerifierTrust{{ID: verifierID, PublicKey: rotated, MaxValidity: time.Hour}}
		},
		"max validity narrowed": func(x *ExecutorTrust) {
			x.Verifiers = []VerifierTrust{{ID: verifierID, PublicKey: public, MaxValidity: 10 * time.Minute}}
		},
		"provenance now required": func(x *ExecutorTrust) { x.ProvenanceDigests = []string{provenance} },
		"ref removed":             nil,
	} {
		t.Run(name, func(t *testing.T) {
			v := newStoreEnv(t)
			want := expectation()
			if _, e := v.present(t, want, statement(want)); e != nil {
				t.Fatal(e)
			}
			if mutate == nil {
				empty, _ := NewTrust(nil)
				v.trust.Store(empty)
			} else {
				v.trust.Store(trustWith(t, mutate))
			}
			if _, _, ok, e := v.store.Current(context.Background(), want.Principal, ref, now, now); ok || e != nil {
				t.Fatalf("still current after trust change: %v %v", ok, e)
			}
		})
	}
}

func TestStoreFailureIsAnError(t *testing.T) {
	v := newStoreEnv(t)
	want := expectation()
	if _, e := v.present(t, want, statement(want)); e != nil {
		t.Fatal(e)
	}
	broken, _ := NewStore(StoreConfig{Backend: failingCAS{}, Trust: v.trust.Load})
	if _, _, ok, e := broken.Current(context.Background(), want.Principal, ref, now, now); ok || !shoal.IsErrorCode(e, shoal.ErrorUnavailable) {
		t.Fatalf("got %v %v", ok, e)
	}
	_, private := keys('a')
	if _, e := broken.Present(context.Background(), want, sign(t, private, statement(want))); !shoal.IsErrorCode(e, shoal.ErrorUnavailable) {
		t.Fatalf("present: %v", e)
	}
	// A corrupted row is a store failure, never "not attested".
	coord := v.store.coordinate(want.Principal, ref)
	cells, e := v.backend.ReadExact(context.Background(), []allocator.Coordinate{coord})
	if e != nil || len(cells) != 1 {
		t.Fatal(e)
	}
	status, e := v.backend.CompareAndMutate(context.Background(), allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{{Coordinate: coord, Value: cells[0].Value, Timestamp: cells[0].Timestamp, TimestampSet: true}}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: cells[0].Timestamp + 1, Value: []byte(`{"Schema":1}`)}}})
	if e != nil || status != allocator.StatusAccepted {
		t.Fatal(status, e)
	}
	if _, _, ok, e := v.store.Current(context.Background(), want.Principal, ref, now, now); ok || !shoal.IsErrorCode(e, shoal.ErrorUnavailable) {
		t.Fatalf("corrupt row: %v %v", ok, e)
	}
}

func TestLostAcknowledgementReconciles(t *testing.T) {
	v := newStoreEnv(t)
	lossy, _ := NewStore(StoreConfig{Backend: lostAckCAS{v.backend}, Trust: v.trust.Load})
	want := expectation()
	_, private := keys('a')
	record, e := lossy.Present(context.Background(), want, sign(t, private, statement(want)))
	if e != nil {
		t.Fatal(e)
	}
	if id, _, ok, _ := v.store.Current(context.Background(), want.Principal, ref, now, now); !ok || id != record.AttestationID {
		t.Fatal("lost ack not reconciled")
	}
}

func TestRowIsDurable(t *testing.T) {
	v := newStoreEnv(t)
	want := expectation()
	record, e := v.present(t, want, statement(want))
	if e != nil {
		t.Fatal(e)
	}
	if e = v.eng.Close(); e != nil {
		t.Fatal(e)
	}
	eng, backend := openEngine(t, v.dir, false)
	t.Cleanup(func() { _ = eng.Close() })
	reopened, _ := NewStore(StoreConfig{Backend: backend, Trust: v.trust.Load})
	if id, _, ok, e := reopened.Current(context.Background(), want.Principal, ref, now, now); e != nil || !ok || id != record.AttestationID {
		t.Fatalf("after reopen: %v %v", ok, e)
	}
	older := StatementFor(want, image, now.Add(-2*time.Minute), now.Add(30*time.Minute))
	_, private := keys('a')
	if _, e := reopened.Present(context.Background(), want, sign(t, private, older)); !reasonIs(e, ReasonRollback) {
		t.Fatalf("rollback after reopen: %v", e)
	}
}

func reasonIs(e error, r Reason) bool {
	got, ok := ReasonOf(e)
	return ok && got == r
}

type failingCAS struct{}

func (failingCAS) ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error) {
	return nil, errors.New("disk on fire")
}
func (failingCAS) CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error) {
	return allocator.StatusUnknown, errors.New("disk on fire")
}

// lostAckCAS applies the write and then reports an indeterminate outcome.
type lostAckCAS struct{ decisionstore.CAS }

func (c lostAckCAS) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	if _, e := c.CAS.CompareAndMutate(ctx, m); e != nil {
		return allocator.StatusUnknown, e
	}
	return allocator.StatusUnknown, errors.New("connection reset")
}
