package embedstore_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

func TestConditionalWriteConcurrentCreateIfAbsent(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store := embedstore.New(eng)
	if err := store.CreateTable(ctx, "leases", nil); err != nil {
		t.Fatal(err)
	}

	const writers = 32
	start := make(chan struct{})
	accepted := make(chan bool, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			results, err := store.WriteWithResults(ctx, "leases", []*embedpb.Mutation{{
				Row: []byte("lease"),
				Entries: []*embedpb.Entry{{
					ColumnFamily: []byte("meta"), ColumnQualifier: []byte("owner"),
					Value: []byte(fmt.Sprintf("writer-%d", id)),
				}},
				Conditions: []*embedpb.Condition{{
					ColumnFamily: []byte("meta"), ColumnQualifier: []byte("owner"),
					Predicate: &embedpb.Condition_Absent{Absent: true},
				}},
			}})
			if err != nil {
				errs <- err
				return
			}
			accepted <- results[0].Status == embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED
		}(i)
	}
	close(start)
	wg.Wait()
	close(accepted)
	close(errs)
	for err := range errs {
		t.Errorf("conditional write: %v", err)
	}
	count := 0
	for ok := range accepted {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("accepted = %d, want exactly 1", count)
	}

	cells, err := store.Scan(ctx, "leases", &embedpb.ScanRequest{RowPrefix: "lease"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 {
		t.Fatalf("persisted cells = %d, want 1", len(cells))
	}
}

func TestConditionalWriteVisibilityTimestampDeleteAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store := embedstore.New(eng)
	if err := store.CreateTable(ctx, "cas", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, "cas", []*embedpb.Mutation{{
		Row: []byte("row"),
		Entries: []*embedpb.Entry{
			{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("state"), ColumnVisibility: []byte("A"), Timestamp: 10, Value: []byte("old")},
			{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("state"), ColumnVisibility: []byte("A"), Timestamp: 20, Value: []byte("current")},
			{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("state"), ColumnVisibility: []byte("B"), Timestamp: 30, Value: []byte("other-vis")},
			{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("deleted"), Timestamp: 30, Value: []byte("before-delete")},
			{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("deleted"), Timestamp: 40, Delete: true},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	ts10, ts30 := int64(10), int64(30)
	tests := []struct {
		name      string
		condition *embedpb.Condition
		want      embedpb.MutationStatus
	}{
		{
			name:      "newest value matches exact visibility",
			condition: valueCondition("state", "A", nil, "current"),
			want:      embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED,
		},
		{
			name:      "stale newest value rejected",
			condition: valueCondition("state", "A", nil, "old"),
			want:      embedpb.MutationStatus_MUTATION_STATUS_REJECTED,
		},
		{
			name:      "exact historical timestamp",
			condition: valueCondition("state", "A", &ts10, "old"),
			want:      embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED,
		},
		{
			name:      "different visibility is absent",
			condition: absentCondition("state", "C", nil),
			want:      embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED,
		},
		{
			name:      "live coordinate is not absent",
			condition: absentCondition("state", "A", nil),
			want:      embedpb.MutationStatus_MUTATION_STATUS_REJECTED,
		},
		{
			name:      "newest tombstone is absent",
			condition: absentCondition("deleted", "", nil),
			want:      embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED,
		},
		{
			name:      "exact value before newer tombstone",
			condition: valueCondition("deleted", "", &ts30, "before-delete"),
			want:      embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := store.WriteWithResults(ctx, "cas", []*embedpb.Mutation{{
				Row: []byte("row"),
				Entries: []*embedpb.Entry{{
					ColumnFamily: []byte("result"), ColumnQualifier: []byte(fmt.Sprintf("%d", i)),
					Value: []byte("written"),
				}},
				Conditions: []*embedpb.Condition{tt.condition},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := results[0].Status; got != tt.want {
				t.Fatalf("status = %s, want %s", got, tt.want)
			}
		})
	}

	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store = embedstore.New(eng)

	results, err := store.WriteWithResults(ctx, "cas", []*embedpb.Mutation{
		{
			Row: []byte("row"),
			Entries: []*embedpb.Entry{{
				ColumnFamily: []byte("result"), ColumnQualifier: []byte("historical-after-restart"),
				Value: []byte("persisted"),
			}},
			Conditions: []*embedpb.Condition{valueCondition("state", "A", &ts10, "old")},
		},
		{
			Row: []byte("row"),
			Entries: []*embedpb.Entry{{
				ColumnFamily: []byte("cf"), ColumnQualifier: []byte("state"),
				ColumnVisibility: []byte("A"), Value: []byte("after-restart"),
			}},
			Conditions: []*embedpb.Condition{valueCondition("state", "A", nil, "current")},
		},
		{
			Row: []byte("row"),
			Entries: []*embedpb.Entry{{
				ColumnFamily: []byte("result"), ColumnQualifier: []byte("rejected"),
				Value: []byte("must-not-persist"),
			}},
			Conditions: []*embedpb.Condition{valueCondition("state", "A", nil, "stale")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED ||
		results[1].Status != embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED ||
		results[2].Status != embedpb.MutationStatus_MUTATION_STATUS_REJECTED {
		t.Fatalf("restart results = %v", results)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store = embedstore.New(eng)
	cells, err := store.Scan(ctx, "cas", &embedpb.ScanRequest{RowPrefix: "row"})
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, cell := range cells {
		values[string(cell.ColumnFamily)+":"+string(cell.ColumnQualifier)+":"+string(cell.ColumnVisibility)] = string(cell.Value)
	}
	if got := values["cf:state:A"]; got != "after-restart" {
		t.Errorf("state after second restart = %q", got)
	}
	if _, ok := values["result:rejected:"]; ok {
		t.Error("rejected mutation persisted")
	}
}

func TestConditionalWriteAfterParquetFlushAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("cas", engine.TableOptions{
		TabletOptions: tablet.Options{FileFormat: tablet.FormatParquet},
	}); err != nil {
		t.Fatal(err)
	}
	store := embedstore.New(eng)
	if err := store.Write(ctx, "cas", []*embedpb.Mutation{{
		Row: []byte("row"),
		Entries: []*embedpb.Entry{{
			ColumnFamily: []byte("cf"), ColumnQualifier: []byte("state"),
			Timestamp: 10, Value: []byte("before"),
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(ctx, "cas"); err != nil {
		t.Fatal(err)
	}
	results, err := store.WriteWithResults(ctx, "cas", []*embedpb.Mutation{{
		Row: []byte("row"),
		Entries: []*embedpb.Entry{{
			ColumnFamily: []byte("cf"), ColumnQualifier: []byte("state"),
			Value: []byte("after"),
		}},
		Conditions: []*embedpb.Condition{valueCondition("state", "", nil, "before")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED {
		t.Fatalf("status = %s", results[0].Status)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store = embedstore.New(eng)
	cells, err := store.Scan(ctx, "cas", &embedpb.ScanRequest{RowPrefix: "row"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 || string(cells[0].Value) != "after" {
		t.Fatalf("cells after restart = %v", cellValues(cells))
	}
}

func valueCondition(cq, visibility string, timestamp *int64, value string) *embedpb.Condition {
	return &embedpb.Condition{
		ColumnFamily: []byte("cf"), ColumnQualifier: []byte(cq),
		ColumnVisibility: []byte(visibility), Timestamp: timestamp,
		Predicate: &embedpb.Condition_ValueEquals{ValueEquals: []byte(value)},
	}
}

func absentCondition(cq, visibility string, timestamp *int64) *embedpb.Condition {
	return &embedpb.Condition{
		ColumnFamily: []byte("cf"), ColumnQualifier: []byte(cq),
		ColumnVisibility: []byte(visibility), Timestamp: timestamp,
		Predicate: &embedpb.Condition_Absent{Absent: true},
	}
}
