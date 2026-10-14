package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestOwnedClientCopiesColumnSelectionsIntoSnapshots(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"events",
		nil,
		10,
	)
	family := []byte{'f', 0, 'm'}
	qualifier := []byte{'q', 0}
	if err := client.addColumn(accumulo.NewColumnFamily(family)); err != nil {
		t.Fatal(err)
	}
	if err := client.addColumn(accumulo.NewColumn(family, qualifier)); err != nil {
		t.Fatal(err)
	}
	family[0] = 'x'
	qualifier[0] = 'x'

	snapshot, done, err := client.snapshot(true)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(snapshot.columns) != 2 {
		t.Fatalf("column count = %d, want 2", len(snapshot.columns))
	}
	if got := snapshot.columns[0].Family(); !bytes.Equal(got, []byte{'f', 0, 'm'}) {
		t.Fatalf("family = %q, want copied binary family", got)
	}
	if got := snapshot.columns[0].Qualifier(); got != nil {
		t.Fatalf("family-only qualifier = %v, want nil", got)
	}
	if got := snapshot.columns[1].Qualifier(); !bytes.Equal(got, []byte{'q', 0}) {
		t.Fatalf("qualifier = %q, want copied binary qualifier", got)
	}
}

func TestOwnedClientScanHooksReceiveAtomicSnapshot(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"events",
		[][]byte{[]byte("A")},
		7,
	)
	if err := client.addColumn(accumulo.NewColumnFamily([]byte("cf"))); err != nil {
		t.Fatal(err)
	}
	var got clientSnapshot
	client.scanOne = func(
		_ context.Context,
		snapshot clientSnapshot,
		_ *accumulo.Range,
	) ([]accumulo.KeyValue, error) {
		got = snapshot
		return nil, nil
	}
	ctx, snapshot, done, err := client.beginSnapshot(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.scanOne(ctx, snapshot, nil)
	done()
	if err != nil {
		t.Fatal(err)
	}
	if got.table != "events" || got.threadCount != 7 ||
		len(got.authorizations) != 1 || len(got.columns) != 1 {
		t.Fatalf("scan snapshot = %#v", got)
	}
}

func TestOwnedClientScanCancellationJoinsBeforeCloseReturns(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"events",
		nil,
		10,
	)
	started := make(chan struct{})
	release := make(chan struct{})
	client.scanOne = func(
		ctx context.Context,
		_ clientSnapshot,
		_ *accumulo.Range,
	) ([]accumulo.KeyValue, error) {
		close(started)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}
	scanDone := make(chan error, 1)
	go func() {
		ctx, snapshot, done, err := client.beginSnapshot(true, 0)
		if err != nil {
			scanDone <- err
			return
		}
		defer done()
		_, err = client.scanOne(ctx, snapshot, nil)
		scanDone <- err
	}()
	<-started

	closeDone := make(chan error, 1)
	go func() { closeDone <- client.close() }()
	select {
	case <-closeDone:
		t.Fatal("close returned before the scan released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-scanDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want canceled", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestOwnedClientConcurrentColumnSelectionAndSnapshots(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"events",
		nil,
		10,
	)
	const callers = 32
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := range callers {
		go func() {
			defer wait.Done()
			if index%2 == 0 {
				_ = client.addColumn(accumulo.NewColumnFamily([]byte{byte(index)}))
				return
			}
			_, done, err := client.snapshot(true)
			if err == nil {
				done()
			}
		}()
	}
	wait.Wait()
}
