package accumulo

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

func TestListTableSplitsOrdersAndCopiesRows(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
		"1": {
			{
				TableID: "1",
				PrevRow: []byte("x\x00y"),
			},
			{
				TableID: "1",
				PrevRow: []byte{},
				EndRow:  []byte("m"),
			},
			{
				TableID: "1",
				EndRow:  []byte{},
			},
			{
				TableID: "1",
				PrevRow: []byte("m"),
				EndRow:  []byte("x\x00y"),
			},
		},
	}}
	connector := testConnectorWithDiscovery(t, walker, &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
	})

	splits, err := connector.ListTableSplits(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if len(splits) != 3 {
		t.Fatalf("len(splits) = %d, want 3", len(splits))
	}
	if splits[0] == nil || len(splits[0]) != 0 {
		t.Fatalf("split[0] = %v, want empty but non-nil", splits[0])
	}
	want := [][]byte{
		{},
		[]byte("m"),
		[]byte("x\x00y"),
	}
	for i := range want {
		if !bytes.Equal(splits[i], want[i]) {
			t.Fatalf("split[%d] = %q, want %q", i, splits[i], want[i])
		}
	}

	splits[1][0] = 'z'
	splits[2][0] = 'q'

	again, err := connector.ListTableSplits(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again[1], []byte("m")) || !bytes.Equal(again[2], []byte("x\x00y")) {
		t.Fatalf("returned mutations leaked: %#v", again)
	}
	if walker.calls != 1 {
		t.Fatalf("walker calls = %d, want 1 cached lookup", walker.calls)
	}
}

func TestListTableSplitsErrorsAndCancellation(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		blocked := testConnectorWithDiscovery(t, &fakeTabletWalker{wait: true}, &fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		if _, err := blocked.ListTableSplits(ctx, "events"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ListTableSplits deadline error = %v, want DeadlineExceeded", err)
		}
	})

	t.Run("closed connector", func(t *testing.T) {
		connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		})
		if err := connector.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := connector.ListTableSplits(context.Background(), "events"); !errors.Is(err, ErrConnectorClosed) {
			t.Fatalf("ListTableSplits closed error = %v, want ErrConnectorClosed", err)
		}
	})

	t.Run("table not found", func(t *testing.T) {
		connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
		if _, err := connector.ListTableSplits(context.Background(), "missing"); err == nil ||
			!errors.Is(err, ErrTableNotFound) || err.Error() != `accumulo: table not found: table name "missing"` {
			t.Fatalf("ListTableSplits missing table error = %v", err)
		}
	})

	t.Run("table name discovery failure", func(t *testing.T) {
		connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{
			resolveIDErr: errors.New("name lookup exploded"),
		})
		_, err := connector.ListTableSplits(context.Background(), "events")
		if err == nil || err.Error() != `accumulo: resolve table name "events": name lookup exploded` {
			t.Fatalf("ListTableSplits name discovery error = %v", err)
		}
	})

	t.Run("metadata discovery failure", func(t *testing.T) {
		connector := testConnectorWithDiscovery(t, &fakeTabletWalker{
			tablets: map[string][]metadata.TabletInfo{"1": {{TableID: "1", EndRow: []byte("m")}}},
			err:     errors.New("metadata walk exploded"),
		}, &fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		})
		_, err := connector.ListTableSplits(context.Background(), "events")
		if err == nil || err.Error() != `accumulo: discover tablets for table 1: metadata walk exploded` {
			t.Fatalf("ListTableSplits metadata discovery error = %v", err)
		}
	})
}
