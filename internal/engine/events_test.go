package engine_test

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// TestEngine_RFileEvents proves the event bus fires on every path that mints a
// new RFile: an auto-flush (memtable crosses the threshold inside Write), an
// explicit Flush, and a Compaction. This is what lets the in-process syncer
// ship RFiles as soon as they land instead of on a fixed interval.
func TestEngine_RFileEvents(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	events, cancel := eng.Subscribe()
	defer cancel()

	// Tiny flush threshold so each 2-cell Write auto-flushes.
	if err := eng.CreateTable("graph", engine.TableOptions{
		TabletOptions: tablet.Options{FlushThreshold: 2},
	}); err != nil {
		t.Fatal(err)
	}

	write := func(row string) {
		m, _ := cclient.NewMutation([]byte(row))
		m.PutLatest([]byte("content"), []byte("a"), nil, []byte("x"))
		m.PutLatest([]byte("content"), []byte("b"), nil, []byte("y"))
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}

	next := func(want string) engine.RFileEvent {
		t.Helper()
		select {
		case ev := <-events:
			if ev.Table != "graph" {
				t.Fatalf("event table = %q, want graph", ev.Table)
			}
			if ev.Kind != want {
				t.Fatalf("event kind = %q, want %q", ev.Kind, want)
			}
			if ev.File == "" {
				t.Fatalf("event file is empty")
			}
			return ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q event", want)
			return engine.RFileEvent{}
		}
	}

	// Two auto-flushes (each Write exceeds the threshold) -> two RFiles.
	write("evt:0001")
	next("flush")
	write("evt:0002")
	next("flush")

	// An explicit Flush of an empty memtable produces nothing; write one more
	// cell first so the explicit Flush has work and emits an event.
	m, _ := cclient.NewMutation([]byte("evt:0003"))
	m.PutLatest([]byte("content"), []byte("a"), nil, []byte("z"))
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	next("flush")

	// Now there are >=2 RFiles; compaction merges them and emits a compact event.
	if err := eng.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}
	next("compact")
}

// TestEngine_SubscribeCancel verifies cancel unsubscribes (no further events)
// and is safe to call more than once.
func TestEngine_SubscribeCancel(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	events, cancel := eng.Subscribe()
	cancel()
	cancel() // idempotent

	if err := eng.CreateTable("graph", engine.TableOptions{
		TabletOptions: tablet.Options{FlushThreshold: 1},
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("evt:0001"))
	m.PutLatest([]byte("content"), []byte("a"), nil, []byte("x"))
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}

	// Channel was closed by cancel; a receive must not block and must report closed.
	select {
	case _, ok := <-events:
		if ok {
			t.Fatalf("received event after cancel")
		}
	case <-time.After(time.Second):
		t.Fatalf("receive on cancelled subscription blocked")
	}
}
