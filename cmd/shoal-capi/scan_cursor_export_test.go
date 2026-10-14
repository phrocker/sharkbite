package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

type fakeScanCursor struct {
	mu      sync.Mutex
	entries []accumulo.KeyValue
	index   int
	current accumulo.KeyValue
	err     error
	block   <-chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newFakeScanCursor(entries []accumulo.KeyValue) *fakeScanCursor {
	return &fakeScanCursor{
		entries: entries,
		closed:  make(chan struct{}),
	}
}

func (f *fakeScanCursor) Next() bool {
	if f.block != nil {
		select {
		case <-f.block:
		case <-f.closed:
			f.mu.Lock()
			f.err = accumulo.ErrStreamClosed
			f.mu.Unlock()
			return false
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.index >= len(f.entries) {
		return false
	}
	f.current = f.entries[f.index]
	f.index++
	return true
}

func (f *fakeScanCursor) Entry() accumulo.KeyValue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *fakeScanCursor) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeScanCursor) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func TestOwnedScanCursorChunksAndExhausts(t *testing.T) {
	source := newFakeScanCursor([]accumulo.KeyValue{
		{Key: accumulo.Key{Row: []byte("one")}, Value: []byte("1")},
		{Key: accumulo.Key{Row: []byte("two")}, Value: []byte("2")},
		{Key: accumulo.Key{Row: []byte("three")}, Value: []byte("3")},
	})
	var done atomic.Int32
	cursor := newOwnedScanCursor(context.Background(), source, func() {
		done.Add(1)
	})

	first, exhausted, err := cursor.next(2)
	if err != nil || exhausted || len(first) != 2 {
		t.Fatalf("first chunk = (%d, %t, %v), want (2, false, nil)", len(first), exhausted, err)
	}
	second, exhausted, err := cursor.next(2)
	if err != nil || !exhausted || len(second) != 1 {
		t.Fatalf("second chunk = (%d, %t, %v), want (1, true, nil)", len(second), exhausted, err)
	}
	third, exhausted, err := cursor.next(2)
	if err != nil || !exhausted || len(third) != 0 {
		t.Fatalf("terminal chunk = (%d, %t, %v), want (0, true, nil)", len(third), exhausted, err)
	}
	if done.Load() != 1 {
		t.Fatalf("done count = %d, want 1", done.Load())
	}
}

func TestOwnedScanCursorHugeChunkDoesNotPreallocate(t *testing.T) {
	cursor := newOwnedScanCursor(context.Background(), newFakeScanCursor(nil), func() {})
	values, exhausted, err := cursor.next(maxInt())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 || !exhausted {
		t.Fatalf("values = %v, exhausted = %v", values, exhausted)
	}
}

func TestOwnedScanCursorCloseCancelsAndJoinsBlockedNext(t *testing.T) {
	block := make(chan struct{})
	source := newFakeScanCursor(nil)
	source.block = block
	var done atomic.Int32
	cursor := newOwnedScanCursor(context.Background(), source, func() {
		done.Add(1)
	})

	nextDone := make(chan error, 1)
	entered := make(chan struct{})
	pullReturned := make(chan struct{})
	releasePull := make(chan struct{})
	go func() {
		exhausted, err := cursor.beginPull()
		if err != nil || exhausted {
			nextDone <- err
			return
		}
		close(entered)
		defer cursor.endPull()
		_, _, err = cursor.next(1)
		close(pullReturned)
		<-releasePull
		nextDone <- err
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- cursor.close()
	}()
	<-pullReturned
	select {
	case err := <-closeDone:
		t.Fatalf("close returned before pull exit: %v", err)
	default:
	}
	close(releasePull)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-nextDone; !errors.Is(err, accumulo.ErrStreamClosed) {
		t.Fatalf("next error = %v, want ErrStreamClosed", err)
	}
	if done.Load() != 1 {
		t.Fatalf("done count = %d, want 1", done.Load())
	}
	if _, _, err := cursor.next(1); !errors.Is(err, accumulo.ErrStreamClosed) {
		t.Fatalf("post-close next error = %v, want ErrStreamClosed", err)
	}
}

func TestOwnedScanCursorContextCancellationClosesOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := newFakeScanCursor(nil)
	var done atomic.Int32
	cursor := newOwnedScanCursor(ctx, source, func() {
		done.Add(1)
	})
	cancel()
	<-cursor.stopped
	if err := cursor.close(); err != nil {
		t.Fatal(err)
	}
	if done.Load() != 1 {
		t.Fatalf("done count = %d, want 1", done.Load())
	}
}

func TestScannerCloseCancelsAndJoinsOwnedCursor(t *testing.T) {
	scanner := newOwnedScanner(nil, nil, nil)
	ctx, done, err := scanner.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeScanCursor(nil)
	cursor := newOwnedScanCursor(ctx, source, done)

	closeDone := make(chan struct{})
	go func() {
		scanner.close()
		close(closeDone)
	}()
	<-closeDone
	<-cursor.stopped
	if _, _, err := cursor.next(1); !errors.Is(err, context.Canceled) {
		t.Fatalf("next error = %v, want context.Canceled", err)
	}
}

func TestClientCloseCancelsAndJoinsOwnedCursor(t *testing.T) {
	owner := newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{})
	client := newOwnedClient(owner, "events", nil, 1)
	ctx, _, done, err := client.beginSnapshot(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeScanCursor(nil)
	cursor := newOwnedScanCursor(ctx, source, done)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- client.close()
	}()
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	<-cursor.stopped
	if _, _, err := cursor.next(1); !errors.Is(err, context.Canceled) {
		t.Fatalf("next error = %v, want context.Canceled", err)
	}
}

func TestConnectorCloseCancelsAndJoinsOwnedScannerCursor(t *testing.T) {
	owner := newOwnedConnector(&fakeConnectorAPI{}, &fakeConnectorInstance{})
	scanner := newOwnedScanner(nil, nil, owner)
	ctx, done, err := scanner.beginStream(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeScanCursor(nil)
	cursor := newOwnedScanCursor(ctx, source, done)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- owner.close()
	}()
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	<-cursor.stopped
	if _, _, err := cursor.next(1); !errors.Is(err, context.Canceled) {
		t.Fatalf("next error = %v, want context.Canceled", err)
	}
}
