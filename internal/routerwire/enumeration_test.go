// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routerwire

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// manyHidden is more stored descriptors than the 64 pages of 1024 scans an
// earlier enumeration bound allowed, so a page-count bound would trip.
const manyHidden = 70_000

// hideDescriptors stores n descriptors under source B, sorted ahead of the
// visible one, as a registry holding them would.
func (w *world) hideDescriptors(n int) {
	stored := make([]fleet.Stored, n)
	for i := range stored {
		stored[i] = fleet.Stored{Descriptor: fleet.Descriptor{
			ID: shoal.ID(fmt.Sprintf("a-hidden-%06d", i)), Generation: 1, Subject: "admin", Actor: "admin-actor",
			AuthorizationDomain: domain, Scopes: []fleet.Scope{{SourceID: sourceB, PolicyID: policyB}},
			ExecutorRef: "exec", LeaseExpiresAt: at.Add(30 * time.Minute), UpdatedAt: at,
			Capabilities: []fleet.Capability{{Name: "secops", Actions: []fleet.Action{action("rotate_credentials", rotateSchema, true)}}},
		}}
	}
	w.registry.mu.Lock()
	w.registry.put(stored)
	w.registry.mu.Unlock()
}

// TestManyHiddenDescriptorsDoNotChangeTheOutcome: a caller who sees one
// descriptor gets the same proposals and errors whether the registry also
// holds 70,000 descriptors it cannot see or none. Only the scan count (and
// time) differs; that residual is documented.
func TestManyHiddenDescriptorsDoNotChangeTheOutcome(t *testing.T) {
	many := newWorld(t, authorized.NewMemoryPolicyStore(), false)
	none := newWorld(t, authorized.NewMemoryPolicyStore(), false)
	many.hideDescriptors(manyHidden)
	for _, text := range []string{"restart payments in prod", "rotate credentials for checkout", "flush the payments cache", "tell me a joke"} {
		m := many.route(many.alice(), text)
		n := none.route(none.alice(), text)
		if m.Err != "" || !bytes.Equal(m.Proposal, n.Proposal) || m.Err != n.Err {
			t.Fatalf("%q differs:\nmany %s %s\nnone %s %s", text, m.Proposal, m.Err, n.Proposal, n.Err)
		}
	}
	if many.registry.scanCount() <= none.registry.scanCount() {
		t.Fatal("the hidden descriptors were not scanned; the test does not exercise the listing")
	}
}

// TestTooManyVisibleDescriptorsFailClosed: what the caller can see is still
// bounded, even when the descriptors offer no actions.
func TestTooManyVisibleDescriptorsFailClosed(t *testing.T) {
	w := newWorld(t, authorized.NewMemoryPolicyStore(), false)
	stored := make([]fleet.Stored, router.MaxTargets)
	for i := range stored {
		stored[i] = fleet.Stored{Descriptor: fleet.Descriptor{
			ID: shoal.ID(fmt.Sprintf("b-visible-%04d", i)), Generation: 1, Subject: "admin", Actor: "admin-actor",
			AuthorizationDomain: domain, Scopes: []fleet.Scope{{SourceID: sourceA, PolicyID: policyA}},
			ExecutorRef: "exec", LeaseExpiresAt: at.Add(30 * time.Minute), UpdatedAt: at,
		}}
	}
	w.registry.mu.Lock()
	w.registry.put(stored)
	w.registry.mu.Unlock()
	if o := w.route(w.alice(), "restart payments in prod"); o.Err != router.ErrTooManyTargets.Error() {
		t.Fatalf("%d visible descriptors = %s %s", router.MaxTargets+1, o.Proposal, o.Err)
	}
}
