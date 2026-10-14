// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestRetainedSnapshotRoundTripAndScopeBinding(t *testing.T) {
	b, i, r, now := retainedFixture(t)
	s, e := New(Config{Backend: new(adversarialCAS), Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	scope := Scope{Domain: []byte{0, 255, 1}}
	registered, e := s.Register(ctx, scope, b)
	if e != nil {
		t.Fatal(e)
	}
	pending, e := s.Begin(ctx, scope, b, i)
	if e != nil {
		t.Fatal(e)
	}
	published, e := s.Publish(ctx, scope, b, i, r)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []Snapshot{registered, pending, published} {
		raw, e := EncodeSnapshot(scope, want)
		if e != nil {
			t.Fatal(e)
		}
		got, e := DecodeSnapshot(scope, raw)
		if e != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("opaque snapshot changed", e)
		}
		if _, e = DecodeSnapshot(Scope{Domain: []byte("other")}, raw); e == nil {
			t.Fatal("cross-domain replay")
		}
		bad := append([]byte(nil), raw...)
		bad[len(bad)/2] ^= 1
		if _, e = DecodeSnapshot(scope, bad); e == nil {
			t.Fatal("corrupt capture accepted")
		}
		want.ID = "forged"
		if _, e = EncodeSnapshot(scope, want); e == nil {
			t.Fatal("forged identity accepted")
		}
	}
	if _, e = DecodeSnapshot(scope, make([]byte, MaxStoredBytes+1)); e == nil {
		t.Fatal("oversized capture accepted")
	}
}
