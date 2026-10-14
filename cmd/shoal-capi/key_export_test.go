package main

import (
	"bytes"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestOwnedKeyStateConcurrentGetSet(t *testing.T) {
	state := &ownedKeyState{key: accumulo.NewKeyWithColumns(
		[]byte("start"), []byte("start"), nil, nil, accumulo.DefaultKeyTimestamp,
	)}
	const workers = 24
	const iterations = 500

	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := range workers {
		go func() {
			defer wait.Done()
			for iteration := range iterations {
				if worker%2 == 0 {
					value := []byte{byte(worker), byte(iteration)}
					state.mutate(func(key *accumulo.Key) {
						key.SetRow(value)
						key.SetColumnFamily(value)
					})
					value[0] ^= 0xff
				} else {
					snapshot := state.snapshot()
					if len(snapshot.Row) != len(snapshot.ColumnFamily) ||
						!bytes.Equal(snapshot.Row, snapshot.ColumnFamily) {
						t.Errorf("torn key snapshot: row=%v family=%v", snapshot.Row, snapshot.ColumnFamily)
						return
					}
				}
			}
		}()
	}
	wait.Wait()
}

func TestOwnedKeySnapshotIsIndependent(t *testing.T) {
	state := &ownedKeyState{key: accumulo.NewKeyWithColumns(
		[]byte("row"), []byte("cf"), []byte("cq"), []byte("A"), 9,
	)}
	snapshot := state.snapshot()

	state.mutate(func(key *accumulo.Key) { key.SetRow([]byte("changed")) })

	if string(snapshot.Row) != "row" {
		t.Fatalf("snapshot row = %q, want row", snapshot.Row)
	}
}

func TestOwnedKeySelfComparisonRemainsReflexiveDuringMutation(t *testing.T) {
	state := &ownedKeyState{key: accumulo.NewKey([]byte("start"))}
	const iterations = 2000

	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := range iterations {
			state.mutate(func(key *accumulo.Key) {
				key.SetRow([]byte{byte(index), byte(index >> 8)})
			})
		}
	}()

	for range iterations {
		if order := compareOwnedKeyStates(state, state, func(left, right accumulo.Key) int {
			return left.Compare(right)
		}); order != 0 {
			t.Fatalf("self comparison = %d, want 0", order)
		}
	}
	<-done
}
