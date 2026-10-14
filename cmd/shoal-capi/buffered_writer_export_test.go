package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

type recordingBufferedWriter struct {
	mu         sync.Mutex
	rows       [][]byte
	sizes      []int
	closeCalls int
	add        func(context.Context, *accumulo.Mutation) error
}

func (w *recordingBufferedWriter) Add(ctx context.Context, mutation *accumulo.Mutation) error {
	if w.add != nil {
		if err := w.add(ctx, mutation); err != nil {
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rows = append(w.rows, mutation.Row())
	w.sizes = append(w.sizes, mutation.Size())
	return nil
}

func (*recordingBufferedWriter) Flush(context.Context) error {
	return nil
}

func (w *recordingBufferedWriter) Close(context.Context) error {
	w.mu.Lock()
	w.closeCalls++
	w.mu.Unlock()
	return nil
}

func TestOwnedAccumuloWriterIsLazyAndFlushesOnRowChangeAndClose(t *testing.T) {
	backend := &recordingBufferedWriter{}
	var factoryCalls atomic.Int32
	writer := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		factoryCalls.Add(1)
		return backend, nil
	}, nil, time.Now)

	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("factory called during construction: %d", got)
	}
	ctx, done, err := writer.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	row := []byte("row-a")
	if err := writer.update(ctx, row, func(m *accumulo.Mutation) {
		m.Put([]byte("f"), []byte("q1"), nil, 1, []byte("v1"))
	}); err != nil {
		t.Fatal(err)
	}
	row[0] = 'X'
	if err := writer.update(ctx, []byte("row-a"), func(m *accumulo.Mutation) {
		m.Put([]byte("f"), []byte("q2"), nil, 2, []byte("v2"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.update(ctx, []byte("row-b"), func(m *accumulo.Mutation) {
		m.Delete([]byte("f"), []byte("q"), nil, 0)
	}); err != nil {
		t.Fatal(err)
	}
	done()

	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
	if len(backend.rows) != 1 || string(backend.rows[0]) != "row-a" || backend.sizes[0] != 2 {
		t.Fatalf("row-change flush = rows %q sizes %v", backend.rows, backend.sizes)
	}
	if err := writer.close(time.Second); err != nil {
		t.Fatal(err)
	}
	if len(backend.rows) != 2 || string(backend.rows[1]) != "row-b" || backend.sizes[1] != 1 {
		t.Fatalf("close flush = rows %q sizes %v", backend.rows, backend.sizes)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", backend.closeCalls)
	}
	if err := writer.close(time.Second); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
}

func TestOwnedAccumuloWriterConcurrentUpdatesAreSerialized(t *testing.T) {
	backend := &recordingBufferedWriter{}
	writer := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		return backend, nil
	}, nil, time.Now)

	const goroutines = 32
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for range goroutines {
		go func() {
			defer wait.Done()
			ctx, done, err := writer.begin(time.Second)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			defer done()
			if err := writer.update(ctx, []byte("shared"), func(m *accumulo.Mutation) {
				m.Put([]byte("f"), []byte("q"), nil, 1, []byte("v"))
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wait.Wait()
	if err := writer.close(time.Second); err != nil {
		t.Fatal(err)
	}
	if len(backend.rows) != 1 || backend.sizes[0] != goroutines {
		t.Fatalf("serialized flush = rows %q sizes %v", backend.rows, backend.sizes)
	}
}

func TestOwnedAccumuloWriterCloseCancelsAndJoinsActiveUpdate(t *testing.T) {
	started := make(chan struct{})
	var addCalls atomic.Int32
	backend := &recordingBufferedWriter{
		add: func(ctx context.Context, _ *accumulo.Mutation) error {
			if addCalls.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	}
	writer := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		return backend, nil
	}, nil, time.Now)

	ctx, done, err := writer.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.update(ctx, []byte("row-a"), func(m *accumulo.Mutation) {
		m.Put([]byte("f"), []byte("q"), nil, 1, []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	done()

	updateResult := make(chan error, 1)
	go func() {
		ctx, done, err := writer.begin(0)
		if err != nil {
			updateResult <- err
			return
		}
		defer done()
		updateResult <- writer.update(ctx, []byte("row-b"), func(m *accumulo.Mutation) {
			m.Put([]byte("f"), []byte("q"), nil, 2, []byte("v"))
		})
	}()
	<-started
	if err := writer.close(time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-updateResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("active update error = %v, want canceled", err)
	}
	if addCalls.Load() != 2 {
		t.Fatalf("add calls = %d, want canceled add plus close retry", addCalls.Load())
	}
	if backend.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", backend.closeCalls)
	}
}

func TestOwnedAccumuloWriterConnectorCloseCancelsActiveUpdate(t *testing.T) {
	started := make(chan struct{})
	backend := &recordingBufferedWriter{
		add: func(ctx context.Context, _ *accumulo.Mutation) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	owner := newOwnedConnector(&fakeConnectorAPI{}, nil)
	writer := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		return backend, nil
	}, owner, time.Now)

	ctx, done, err := writer.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.update(ctx, []byte("row-a"), func(m *accumulo.Mutation) {
		m.Put([]byte("f"), []byte("q"), nil, 1, []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	done()

	updateResult := make(chan error, 1)
	go func() {
		ctx, done, err := writer.begin(0)
		if err != nil {
			updateResult <- err
			return
		}
		defer done()
		updateResult <- writer.update(ctx, []byte("row-b"), func(m *accumulo.Mutation) {
			m.Put([]byte("f"), []byte("q"), nil, 2, []byte("v"))
		})
	}()
	<-started
	if err := owner.close(); err != nil {
		t.Fatalf("connector close: %v", err)
	}
	if err := <-updateResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("active update error = %v, want canceled", err)
	}
	if _, _, err := writer.begin(0); !errors.Is(err, accumulo.ErrConnectorClosed) {
		t.Fatalf("begin after connector close = %v", err)
	}
}

func TestOwnedAccumuloWriterTimestampSubstitution(t *testing.T) {
	fixed := time.UnixMilli(123456789)
	writer := newOwnedAccumuloWriter(nil, nil, func() time.Time { return fixed })
	if got := writer.putTimestamp(0); got != fixed.UnixMilli() {
		t.Fatalf("zero timestamp = %d", got)
	}
	if got := writer.putTimestamp(42); got != 42 {
		t.Fatalf("explicit timestamp = %d", got)
	}
}
