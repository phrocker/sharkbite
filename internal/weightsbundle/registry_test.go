// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func chainInput(c chain) PromotionInput {
	return PromotionInput{Candidate: c["candidate"], Evaluation: c["evaluation"], Artifact: c["artifact"], Approval: c["approval"]}
}

var chainLineage = DatasetLineage{DatasetManifestSHA256: strings.Repeat("d", 64), SourceIDs: []string{"src-1", "src-2"}}

func TestRegistryPromoteRollbackAndReopen(t *testing.T) {
	ctx := context.Background()
	c := loadChain(t)
	store := memory.New()
	r, err := OpenRegistry(ctx, store, "learning/releases")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(); !errors.Is(err, ErrNoActiveRelease) {
		t.Fatal(err)
	}
	first, err := r.Promote(ctx, chainInput(c), chainLineage)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != c.release(t, "first_release").ID {
		t.Fatal("registry release differs from pure promotion")
	}
	// A promotion decided against a stale pointer is refused.
	if _, err := r.Promote(ctx, chainInput(c), chainLineage); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale promotion: %v", err)
	}
	wrongLineage := DatasetLineage{DatasetManifestSHA256: strings.Repeat("e", 64)}
	in := chainInput(c)
	in.Predecessor = &first
	if _, err := r.Promote(ctx, in, wrongLineage); !errors.Is(err, ErrRefused) {
		t.Fatalf("lineage mismatch: %v", err)
	}
	second, err := r.Promote(ctx, in, chainLineage)
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := r.Active(); active.ID != second.ID {
		t.Fatal("pointer not advanced")
	}
	approval, err := NewRollbackApproval(second.ID, first.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := r.Rollback(ctx, first.ID, approval)
	if err != nil {
		t.Fatal(err)
	}
	if *rollback.BundleDigest != *first.BundleDigest || rollback.ArtifactID != first.ArtifactID {
		t.Fatal("rollback did not restore the prior bundle")
	}
	if _, err := r.Rollback(ctx, first.ID, approval); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("reused approval: %v", err)
	}
	if _, err := r.Rollback(ctx, "unknown", approval); !errors.Is(err, ErrRefused) {
		t.Fatalf("unknown prior: %v", err)
	}
	history := r.History()
	if len(history) != 3 || history[0].ID != first.ID || history[2].ID != rollback.ID {
		t.Fatalf("history %d", len(history))
	}

	reopened, err := OpenRegistry(ctx, store, "learning/releases")
	if err != nil {
		t.Fatal(err)
	}
	if active, ok := reopened.Active(); !ok || active.ID != rollback.ID {
		t.Fatal("reopened registry lost the active pointer")
	}
	if len(reopened.History()) != 3 {
		t.Fatal("reopened history")
	}
	if l, ok := reopened.Lineage(rollback.ID); !ok || len(l.SourceIDs) != 2 {
		t.Fatal("rollback lineage not carried")
	}
	// Two registries on one store cannot both advance the pointer.
	stale, _ := OpenRegistry(ctx, store, "learning/releases")
	if _, err := reopened.Promote(ctx, PromotionInput{Candidate: c["candidate"], Evaluation: c["evaluation"], Artifact: c["artifact"], Approval: c["approval"], Predecessor: &rollback}, chainLineage); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Promote(ctx, PromotionInput{Candidate: c["candidate"], Evaluation: c["evaluation"], Artifact: c["artifact"], Approval: c["approval"], Predecessor: &rollback}, chainLineage); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale registry advanced the pointer: %v", err)
	}
	if active, _ := stale.Active(); active.ID != rollback.ID {
		t.Fatal("stale registry swapped its pointer after a refused commit")
	}
}

func TestRegistryRefusesCorruptStore(t *testing.T) {
	ctx := context.Background()
	c := loadChain(t)
	store := memory.New()
	r, _ := OpenRegistry(ctx, store, "p")
	if _, err := r.Promote(ctx, chainInput(c), chainLineage); err != nil {
		t.Fatal(err)
	}
	first, _ := r.Active()
	store.Put("p/pointers/"+sequenceName(2), []byte(`{"schema":1,"kind":"release_pointer","sequence":2,"release_id":"missing","previous_release_id":"`+first.ID+`"}`))
	if _, err := OpenRegistry(ctx, store, "p"); err == nil || !strings.Contains(err.Error(), "unknown release") {
		t.Fatalf("%v", err)
	}
	store.Delete("p/pointers/" + sequenceName(2))
	raw, _ := EncodeRecord(mustRecord(t, first))
	store.Put("p/releases/"+first.ID+".json", []byte(strings.Replace(string(raw), `"active"`, `"retired"`, 1)))
	if _, err := OpenRegistry(ctx, store, "p"); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("%v", err)
	}
}

func TestConcurrentReadersObserveOneRelease(t *testing.T) {
	ctx := context.Background()
	c := loadChain(t)
	r := NewRegistry()
	first, err := r.Promote(ctx, chainInput(c), chainLineage)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var mu sync.Mutex
	seen := map[string]int{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rel, err := r.Resolve()
				if err != nil {
					t.Error(err)
					return
				}
				// A resolved release is complete: every bound field is present.
				if rel.ID == "" || rel.ArtifactID == "" || rel.BundleDigest == nil {
					t.Error("partial release observed")
					return
				}
				mu.Lock()
				seen[rel.ID]++
				mu.Unlock()
			}
		}()
	}
	current := first
	for i := 0; i < 20; i++ {
		in := chainInput(c)
		in.Predecessor = &current
		next, err := r.Promote(ctx, in, chainLineage)
		if err != nil {
			t.Fatal(err)
		}
		approval, _ := NewRollbackApproval(next.ID, current.ID, "owner")
		back, err := r.Rollback(ctx, current.ID, approval)
		if err != nil {
			t.Fatal(err)
		}
		current = back
	}
	close(stop)
	wg.Wait()
	for id := range seen {
		if _, ok := r.Get(id); !ok {
			t.Fatalf("observed unrecorded release %s", id)
		}
	}
}

func TestWithdrawSourceMarksReleasesIneligible(t *testing.T) {
	ctx := context.Background()
	c := loadChain(t)
	store := memory.New()
	r, _ := OpenRegistry(ctx, store, "w")
	first, err := r.Promote(ctx, chainInput(c), chainLineage)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := r.WithdrawSource(ctx, "src-9", time.Now()); err != nil || len(affected) != 0 {
		t.Fatalf("%v %v", affected, err)
	}
	affected, err := r.WithdrawSource(ctx, "src-2", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	if err != nil || len(affected) != 1 || affected[0] != first.ID {
		t.Fatalf("%v %v", affected, err)
	}
	if _, err := r.Resolve(); !errors.Is(err, ErrIneligible) {
		t.Fatalf("withdrawn release resolved: %v", err)
	}
	if active, ok := r.Active(); !ok || active.ID != first.ID {
		t.Fatal("withdrawal rewrote the pointer")
	}
	e := r.EligibilityOf(first.ID)
	if e.Status != EligibilityWithdrawnSource || e.SourceID != "src-2" {
		t.Fatalf("%+v", e)
	}
	events := r.Events()
	if len(events) != 1 || events[0].Kind != EventSourceWithdrawn || !strings.Contains(events[0].Detail, "does not claim removal") {
		t.Fatalf("%+v", events)
	}
	// Rolling back to a withdrawn release is refused; promoting a successor is not.
	in := chainInput(c)
	in.Predecessor = &first
	second, err := r.Promote(ctx, in, DatasetLineage{DatasetManifestSHA256: strings.Repeat("d", 64), SourceIDs: []string{"src-1"}})
	if err != nil {
		t.Fatal(err)
	}
	approval, _ := NewRollbackApproval(second.ID, first.ID, "owner")
	if _, err := r.Rollback(ctx, first.ID, approval); !errors.Is(err, ErrIneligible) {
		t.Fatalf("rollback to withdrawn release: %v", err)
	}
	if _, err := r.Resolve(); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordEvent(ctx, Event{Kind: EventLoadFailure, ReleaseID: second.ID, Detail: "altered \"model.json\""}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordEvent(ctx, Event{Kind: EventLoadFailure, ReleaseID: "nope"}); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(ctx, store, "w")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.EligibilityOf(first.ID).Status != EligibilityWithdrawnSource || len(reopened.Events()) != 2 {
		t.Fatal("events not replayed")
	}
}

func TestSourcesFromExportManifest(t *testing.T) {
	manifest := []byte(`{"schema":1,"kind":"authorized-numeric-training-export","rows":[{"id":"r1","sources":[{"Digest":"s1"},{"Digest":"s2"}]},{"id":"r2","sources":[{"Digest":"s2"}]}]}`)
	l, err := SourcesFromExportManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(l.SourceIDs, ",") != "s1,s2" || l.DatasetManifestSHA256 != SHA256Hex(manifest) {
		t.Fatalf("%+v", l)
	}
	if _, err := SourcesFromExportManifest([]byte(`{"kind":"other"}`)); err == nil {
		t.Fatal("wrong kind accepted")
	}
}

func mustRecord(t *testing.T, r Release) Record {
	t.Helper()
	rec, err := r.Record()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}
