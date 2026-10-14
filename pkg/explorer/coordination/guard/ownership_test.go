// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package guard

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

// TestNoModeAdoptsAForeignHead: the head key is (domain, kind, entity ID) and
// names no store, so a head is identified as a store's own by the LPART and
// logical policy it was created with. A writer naming a different one is
// refused in every mode that can act on an existing head. Before, only
// ModeAbsentOrIdentical compared them. ModeMutate and ModeRetire compared
// only the expected epoch and digest, which a foreign writer can read off
// the head, so it could adopt another store's entity. The owner's own
// mutate, retire and append still succeed: the control.
func TestNoModeAdoptsAForeignHead(t *testing.T) {
	store := newMemoryStore()
	client, _, _ := newClient(t, store)
	ctx := context.Background()
	create := testIntent("shared", "txn-create", "owner", ModeAbsentOrIdentical)
	acquired, err := client.Acquire(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	head := commitAcquisition(t, client, acquired, 11)

	foreign := map[string]func(*Intent){
		"another logical policy": func(i *Intent) {
			i.LogicalPolicyID = []byte("other-policy")
		},
		"another logical policy and LPART": func(i *Intent) {
			i.LogicalPolicyID = []byte("other-policy")
			i.LPART = coordination.LPART("other-lpart")
		},
	}
	for name, change := range foreign {
		for _, mode := range []Mode{ModeMutate, ModeRetire, ModeAppend, ModeAbsentOrIdentical} {
			intent := testIntent("shared", "txn-foreign-"+name, "intruder", mode)
			change(&intent)
			intent.ExpectedEpoch, intent.ExpectedDigest = head.Epoch, head.LogicalDigest
			if mode == ModeAbsentOrIdentical || mode == ModeAppend {
				intent.ExpectedEpoch, intent.ExpectedDigest = 0, coordination.Digest{}
				intent.DesiredDigest = head.LogicalDigest
			}
			if mode == ModeRetire {
				intent.DesiredState = StateTombstone
			} else {
				intent.DesiredDigest = digest("foreign")
			}
			if mode == ModeAbsentOrIdentical {
				intent.DesiredDigest = head.LogicalDigest
			}
			// The refusal is a conflict, so every handler keyed on
			// ErrConflict is unchanged, and it is ErrForeignHead, so a
			// caller can tell it is permanent rather than a race.
			if _, err := client.Acquire(ctx, intent); !errors.Is(err, ErrConflict) ||
				!errors.Is(err, ErrForeignHead) {
				t.Fatalf("%v by a writer with %s = %v, want ErrForeignHead", mode, name, err)
			}
		}
	}
	current, _, err := client.Read(ctx, create.Entity)
	if err != nil || current == nil || current.Epoch != head.Epoch {
		t.Fatalf("the owner's head moved: %+v, %v", current, err)
	}

	// The owner's own race (a mutate against an epoch that has moved on) is
	// a conflict, but not an ownership refusal: it may be retried.
	stale := testIntent("shared", "txn-stale", "owner", ModeMutate)
	stale.ExpectedEpoch, stale.ExpectedDigest = head.Epoch+1, head.LogicalDigest
	stale.DesiredDigest = digest("stale")
	if _, err := client.Acquire(ctx, stale); !errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrForeignHead) {
		t.Fatalf("the owner's stale mutate = %v, want a plain conflict", err)
	}

	// The owner, by contrast, may still do everything, including under
	// another LPART: one store may partition one entity differently between
	// writes (fleet-event subscription receipt slots do).
	mutate := testIntent("shared", "txn-mutate", "owner", ModeMutate)
	mutate.LPART = coordination.LPART("repartitioned")
	mutate.ExpectedEpoch, mutate.ExpectedDigest = head.Epoch, head.LogicalDigest
	mutate.DesiredDigest = digest("shared-v2")
	mutated, err := client.Acquire(ctx, mutate)
	if err != nil || mutated.Decision != DecisionMutate {
		t.Fatalf("the owner's mutate = %#v, %v", mutated, err)
	}
	head = commitAcquisition(t, client, mutated, 12)
	appended := testIntent("shared", "txn-append", "owner", ModeAppend)
	if result, err := client.Acquire(ctx, appended); err != nil ||
		result.Decision != DecisionAppend {
		t.Fatalf("the owner's append = %#v, %v", result, err)
	} else if err := client.Abort(ctx, result.Pending, false); err != nil {
		t.Fatal(err)
	}
	retire := testIntent("shared", "txn-retire", "owner", ModeRetire)
	retire.ExpectedEpoch, retire.ExpectedDigest = head.Epoch, head.LogicalDigest
	retire.DesiredState, retire.DesiredDigest = StateTombstone, digest("shared-tombstone")
	if result, err := client.Acquire(ctx, retire); err != nil ||
		result.Decision != DecisionRetire {
		t.Fatalf("the owner's retire = %#v, %v", result, err)
	}
}
