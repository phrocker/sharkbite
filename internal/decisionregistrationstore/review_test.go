// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistrationstore

import (
	"context"
	"errors"
	"testing"
)

func TestReviewPostCommitGuardDenialPreservesUncertaintyAndState(t *testing.T) {
	scope, f, blob, now := fixture()
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	ctx := context.Background()
	key := []byte("key")
	original, err := s.Begin(ctx, scope, key, f, blob)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("current catalog permission revoked")
	calls := 0
	got, err := s.MarkReady(ctx, scope, key, original.FrozenSHA256, func(context.Context, Registration, []byte) error {
		calls++
		if calls == 2 {
			return denied
		}
		return nil
	})
	if got.ID != "" || !errors.Is(err, ErrIndeterminate) || !errors.Is(err, denied) || calls != 2 {
		t.Fatalf("commit denial leaked or lost uncertainty: %#v %v calls=%d", got, err, calls)
	}
	retained, err := s.ReadByKey(ctx, scope, key)
	if err != nil || retained.State != Ready {
		t.Fatal("fabricated rollback", err)
	}
	before := m.writes
	if got, err = s.MarkReady(ctx, scope, key, original.FrozenSHA256, func(context.Context, Registration, []byte) error { return denied }); got.ID != "" || !errors.Is(err, denied) || m.writes != before {
		t.Fatal("ready retry bypassed guard", err)
	}
	recovered, err := s.MarkReady(ctx, scope, key, original.FrozenSHA256, func(context.Context, Registration, []byte) error { return nil })
	if err != nil || recovered.ID != retained.ID || recovered.ReadyAt != retained.ReadyAt || m.writes != before {
		t.Fatal("exact recovery changed receipt", err)
	}
}

func TestReviewReadyCannotHideMissingPreparation(t *testing.T) {
	scope, f, blob, now := fixture()
	m := &memoryCAS{}
	s := openMemory(t, m, &now)
	ctx := context.Background()
	key := []byte("key")
	original, err := s.Begin(ctx, scope, key, f, blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkReady(ctx, scope, key, original.FrozenSHA256, func(context.Context, Registration, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	delete(m.cells, string(s.blobCoord(scope, f.RecordSHA256).Row))
	called := false
	got, err := s.MarkReady(ctx, scope, key, original.FrozenSHA256, func(context.Context, Registration, []byte) error { called = true; return nil })
	if err == nil || got.ID != "" || called {
		t.Fatal("ready shortcut hid lost preparation", err)
	}
}
