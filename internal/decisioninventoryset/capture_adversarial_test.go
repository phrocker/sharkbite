// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventoryset

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
)

func TestAdversarialMutationAtEveryCollectionBoundary(t *testing.T) {
	// A change between one row's two reads must reject the collection; changes
	// before both or after both may still describe the advertised historical cut.
	const count = 3
	for _, index := range []int{0, count - 1} {
		for event := 1; event <= 2*count; event++ {
			t.Run(fmt.Sprintf("target%d-after-read%d", index, event), func(t *testing.T) {
				r, s, b, scope, bindings, now := fixture(t, count)
				sort.Slice(bindings, func(i, j int) bool { return bindings[i].TargetID < bindings[j].TargetID })
				target := bindings[index]
				b.afterRead = func(n int) {
					if n != event {
						return
					}
					b.afterRead = nil
					i, receipt := report(target, now)
					if _, err := s.Begin(context.Background(), scope, target, i); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Publish(context.Background(), scope, target, i, receipt); err != nil {
						t.Fatal(err)
					}
				}
				got, err := r.CaptureSet(context.Background(), scope, bindings)
				changedBetweenReads := event >= index+1 && event < count+index+1
				if changedBetweenReads {
					if !errors.Is(err, ErrChanged) || !reflect.DeepEqual(got, Capture{}) {
						t.Fatalf("changed row admitted: %+v %v", got, err)
					}
				} else {
					if err != nil || len(got.Snapshots) != count {
						t.Fatalf("stable historical cut rejected: %v", err)
					}
					captured := got.Snapshots[index]
					if event < index+1 && len(captured.Entries) != 1 {
						t.Fatal("pre-read publication omitted")
					}
					if event >= count+index+1 && len(captured.Entries) != 0 {
						t.Fatal("post-cut publication invented")
					}
				}
			})
		}
	}
}
func TestAdversarialCaptureDetachesCallerInputsAndReturnedSnapshots(t *testing.T) {
	r, s, b, scope, bindings, now := fixture(t, 2)
	originalScope := inventory.Scope{Domain: append([]byte(nil), scope.Domain...)}
	originalBindings := append([]inventory.Binding(nil), bindings...)
	i, receipt := report(bindings[0], now)
	if _, err := s.Begin(context.Background(), scope, bindings[0], i); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(context.Background(), scope, bindings[0], i, receipt); err != nil {
		t.Fatal(err)
	}
	b.reads = 0
	b.afterRead = func(n int) {
		if n == 1 {
			scope.Domain[0] ^= 0xff
			bindings[0].CoverageID = "substituted"
			bindings[1].SubjectID = "substituted"
		}
	}
	got, err := r.CaptureSet(context.Background(), scope, bindings)
	if err != nil {
		t.Fatal("caller mutated captured inputs", err)
	}
	b.afterRead = nil
	fresh, err := r.CaptureSet(context.Background(), originalScope, originalBindings)
	if err != nil || got.VectorID != fresh.VectorID {
		t.Fatal("input mutation changed vector")
	}
	for j := range got.Snapshots {
		if len(got.Snapshots[j].Entries) > 0 {
			got.Snapshots[j].Entries[0].Intent.Reporter.SubjectID = "substituted"
		}
		got.Snapshots[j].Binding.CoverageID = "substituted"
	}
	again, err := r.CaptureSet(context.Background(), originalScope, originalBindings)
	if err != nil || !reflect.DeepEqual(again.Snapshots, fresh.Snapshots) {
		t.Fatal("returned snapshot aliased persisted data")
	}
}
func TestAdversarialSecondPassReadFailureNeverReturnsFirstPass(t *testing.T) {
	r, _, b, scope, bindings, _ := fixture(t, 3)
	b.afterRead = func(n int) {
		if n == 3 {
			b.readErr = true
		}
	}
	got, err := r.CaptureSet(context.Background(), scope, bindings)
	if err == nil || !reflect.DeepEqual(got, Capture{}) {
		t.Fatal("failed second pass exposed first-pass vector")
	}
}
