package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestOwnedClientCopiesAndSnapshotsMutableSettings(t *testing.T) {
	owner := newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{})
	authorization := []byte{'a', 0, 'b'}
	client := newOwnedClient(owner, "events", [][]byte{authorization}, 10)
	authorization[0] = 'z'

	snapshot, done, err := client.snapshot(true)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if snapshot.table != "events" || snapshot.threadCount != 10 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if got := snapshot.authorizations[0]; string(got) != "a\x00b" {
		t.Fatalf("authorization = %q, want copied binary value", got)
	}

	replacement := []byte{'x', 0, 'y'}
	if err := client.setTable("analytics"); err != nil {
		t.Fatal(err)
	}
	if err := client.setThreads(17); err != nil {
		t.Fatal(err)
	}
	if err := client.setAuthorizations([][]byte{replacement}); err != nil {
		t.Fatal(err)
	}
	replacement[0] = 'z'
	snapshot, done, err = client.snapshot(true)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if snapshot.table != "analytics" || snapshot.threadCount != 17 {
		t.Fatalf("updated snapshot = %#v", snapshot)
	}
	if got := snapshot.authorizations[0]; string(got) != "x\x00y" {
		t.Fatalf("updated authorization = %q, want copied binary value", got)
	}
}

func TestOwnedClientRequiresTableForScannerAndWriterSnapshot(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"",
		nil,
		10,
	)
	if _, _, err := client.snapshot(true); !errors.Is(err, accumulo.ErrTableNotFound) {
		t.Fatalf("snapshot error = %v, want table not found", err)
	}
}

func TestOwnedClientCloseCancelsAndJoinsActiveCall(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"events",
		nil,
		10,
	)
	ctx, done, err := client.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- client.close()
	}()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("client close did not cancel active call")
	}
	select {
	case <-closeReturned:
		t.Fatal("client close returned before active call completed")
	default:
	}
	done()
	select {
	case err := <-closeReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client close did not join active call")
	}
	if err := client.setTable("later"); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("set after close error = %v, want connector closed", err)
	}
}

func TestOwnedClientConcurrentSettingsAndSnapshots(t *testing.T) {
	client := newOwnedClient(
		newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{}),
		"events",
		[][]byte{[]byte("a")},
		10,
	)
	const callers = 16
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := range callers {
		go func() {
			defer wait.Done()
			if index%2 == 0 {
				_ = client.setThreads(int32(index + 1))
				_ = client.setAuthorizations([][]byte{[]byte{byte(index)}})
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
