// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

// compatCell is one stored cell as the golden fixture records it.
type compatCell struct {
	Step      string `json:"step"`
	Row       string `json:"row"`
	Family    string `json:"family"`
	Qualifier string `json:"qualifier"`
	Timestamp int64  `json:"timestamp"`
	Value     string `json:"value"`
}

// TestPersistedCollectorRowsAreUnchanged pins the bytes every pre-existing
// row kind is stored as, captured from main before the admin routes (#446
// slice 5) were added. The admin routes add cells of their own, in their own
// column family; they must not change any cell the existing flows write, or
// rows already on disk would stop decoding.
//
// Regenerate only for a deliberate, versioned format change:
// SHOAL_UPDATE_GOLDEN=1 go test ./internal/collectorregistry -run TestPersistedCollectorRowsAreUnchanged
func TestPersistedCollectorRowsAreUnchanged(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	var cells []compatCell
	capture := func(step string, coords ...allocator.Coordinate) {
		t.Helper()
		for _, coord := range coords {
			cell, e := v.registry.read(ctx, coord)
			if e != nil {
				t.Fatalf("%s: %v", step, e)
			}
			cells = append(cells, compatCell{Step: step, Row: string(coord.Row), Family: string(coord.Family), Qualifier: string(coord.Qualifier), Timestamp: cell.Timestamp, Value: hex.EncodeToString(cell.Value)})
		}
	}
	registration := v.registry.registrationCoordinate(collectorID)

	v.provision(t, collectorID, "tail", "authority:logs", "authority:metrics")
	capture("provision", registration)

	tail := v.as(t, "tail")
	enrollment, e := v.registry.Enroll(tail, []byte("key-1"), enrollRequest(collectorID, "authority:logs"))
	if e != nil {
		t.Fatal(e)
	}
	capture("enroll", registration, v.registry.enrollmentCoordinate(collectorID, enrollment.KeyDigest))

	v.clock.Advance(time.Second)
	ref := artifact(t, v, "artifact:1", "line one\n")
	if _, e := v.registry.SubmitArtifact(tail, collectorID, ref); e != nil {
		t.Fatal(e)
	}
	capture("artifact", v.registry.artifactCoordinate(collectorID, 1, ref.ID))
	o, e := v.registry.SubmitObservation(tail, observation(t, v, "artifact:1", v1))
	if e != nil {
		t.Fatal(e)
	}
	capture("observation", v.registry.observationCoordinate(o.Observation.ID()))

	v.clock.Advance(time.Second)
	if _, e := v.registry.Enroll(tail, []byte("key-2"), enrollRequest(collectorID, "authority:metrics")); e != nil {
		t.Fatal(e)
	}
	capture("re-enroll", registration, v.registry.appliedCoordinate(collectorID, enrollment.KeyDigest))

	if _, e := v.registry.Revoke(ctx, collectorID); e != nil {
		t.Fatal(e)
	}
	capture("revoke", registration)
	v.provision(t, collectorID, "tail", "authority:logs")
	capture("reprovision", registration)

	got, e := json.MarshalIndent(cells, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "persisted_rows.golden.json")
	if os.Getenv("SHOAL_UPDATE_GOLDEN") == "1" {
		if e := os.MkdirAll("testdata", 0o755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, got, 0o644); e != nil {
			t.Fatal(e)
		}
	}
	want, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if string(got) != string(want) {
		t.Fatalf("persisted collector rows changed; diff %s against the golden fixture:\n%s", path, firstDifference(string(want), string(got)))
	}
}

func firstDifference(want, got string) string {
	for i := 0; i < len(want) && i < len(got); i++ {
		if want[i] != got[i] {
			lo := max(0, i-80)
			return fmt.Sprintf("at byte %d\nwant ...%q\ngot  ...%q", i, want[lo:min(len(want), i+80)], got[lo:min(len(got), i+80)])
		}
	}
	return fmt.Sprintf("lengths differ: want %d, got %d", len(want), len(got))
}
