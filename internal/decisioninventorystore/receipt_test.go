// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

import (
	"context"
	"testing"
	"time"
)

func TestReceiptDigestMatchesPublishedCommitment(t *testing.T) {
	b, i, r, now := retainedFixture(t)
	backend := new(adversarialCAS)
	s, e := New(Config{Backend: backend, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	scope := Scope{Domain: []byte("digest-test")}
	ctx := context.Background()
	if _, e = s.Register(ctx, scope, b); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Begin(ctx, scope, b, i); e != nil {
		t.Fatal(e)
	}
	want, e := ReceiptDigest(b, i, r)
	if e != nil {
		t.Fatal(e)
	}
	published, e := s.Publish(ctx, scope, b, i, r)
	if e != nil || published.Entries[0].ReceiptDigest != want {
		t.Fatal("digest diverged from persisted format", e)
	}
	r.AuthorizationFingerprint = "auth-sha256:" + hash([]byte("different historical grant"))
	changed, e := ReceiptDigest(b, i, r)
	if e != nil || changed == want {
		t.Fatal("commitment omitted original authorization", e)
	}
	b.CoverageID = ""
	if _, e = ReceiptDigest(b, i, r); e == nil {
		t.Fatal("invalid binding accepted")
	}
}
