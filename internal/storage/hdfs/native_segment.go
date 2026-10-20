// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package hdfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// nativeSegmentCreator is deliberately separate from Client.Create. The
// pinned colinmarc/hdfs Client.Create sends CreateFlag=1 (CREATE only) to the
// namenode; a custom Client must explicitly promise the same no-overwrite
// behavior before it can be used for native WAL publication.
type nativeSegmentCreator interface {
	CreateNativeSegmentExclusive(string) (storage.Writer, error)
}

func (c clientAdapter) CreateNativeSegmentExclusive(name string) (storage.Writer, error) {
	return c.Client.Create(name)
}

// CreateNativeSegmentExclusive writes directly to the final HDFS path with a
// namenode-enforced no-overwrite create. A successful Close waits for HDFS
// replication and completion before callers may verify the bytes. Unlike
// CreateExclusive, this method does not stage and rename: the pinned client
// cannot atomically rename without overwrite. Failed writes can leave an
// unreferenced partial object, and Abort never removes the destination.
// The caller must validate the full WAL path and must not install metadata
// references or acknowledge mutations until Close and read-back succeed.
func (b *Backend) CreateNativeSegmentExclusive(ctx context.Context, objectPath string) (storage.AbortableWriter, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return nil, err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	creator, ok := lease.client.(nativeSegmentCreator)
	if !ok {
		_ = lease.release()
		return nil, fmt.Errorf("hdfs: direct no-overwrite create unavailable: %w", errors.ErrUnsupported)
	}
	resolved, qualifier, err := b.resolve(objectPath)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	if qualifier == "" {
		_ = lease.release()
		return nil, errors.New("hdfs: native WAL segment requires a qualified HDFS path")
	}
	if parent := path.Dir(resolved); parent != "." && parent != "/" {
		if err := lease.client.MkdirAll(parent, 0o755); err != nil {
			_ = lease.release()
			return nil, fmt.Errorf("hdfs: mkdir %s: %w", parent, err)
		}
	}
	writer, err := creator.CreateNativeSegmentExclusive(resolved)
	if err != nil {
		_ = lease.release()
		if isAlreadyExists(err) {
			return nil, errors.Join(storage.ErrExists, err)
		}
		return nil, fmt.Errorf("hdfs: create native WAL segment %s: %w", objectPath, err)
	}
	if writer == nil {
		_ = lease.release()
		return nil, errors.New("hdfs: native WAL creator returned a nil writer")
	}
	w := &nativeSegmentWriter{writer: writer, ctx: ctx, release: lease.release}
	if err := applyDeadline(ctx, writer); err != nil {
		return nil, errors.Join(fmt.Errorf("hdfs: set native WAL writer deadline: %w", err), w.Abort())
	}
	if err := b.registerHandle(w.shutdown, func(done func()) { w.complete = done }); err != nil {
		return nil, errors.Join(err, w.Abort())
	}
	return w, nil
}

// nativeSegmentWriter never removes its final path. Abort completes a partial
// file to release the HDFS lease when Close was not attempted; it can leave an
// orphan, which remains invisible to recovery without a metadata reference.
type nativeSegmentWriter struct {
	mu       sync.Mutex
	writer   storage.Writer
	ctx      context.Context
	release  func() error
	complete func()
	finished bool
	aborted  bool
	result   error
}

func (w *nativeSegmentWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return 0, ErrClosed
	}
	return w.writer.Write(p)
}

func (w *nativeSegmentWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return 0, ErrClosed
	}
	if err := syncDeadline(ctx, w.writer); err != nil {
		return 0, err
	}
	stopInterrupt := context.AfterFunc(contextOrBackground(ctx), func() {
		if setter, ok := w.writer.(deadlineSetter); ok {
			_ = setter.SetDeadline(time.Now())
		}
	})
	defer stopInterrupt()
	n, err := w.writer.Write(p)
	if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
		return n, errors.Join(err, ctxErr)
	}
	return n, err
}

func (w *nativeSegmentWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		if w.aborted {
			return errors.Join(ErrClosed, w.result)
		}
		return w.result
	}
	_, err := closeAfterReplication(w.ctx, w.writer)
	return w.finish(err)
}

func (w *nativeSegmentWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, err := closeAfterReplication(cleanupCtx, w.writer)
	w.aborted = true
	return w.finish(err)
}

func (w *nativeSegmentWriter) shutdown() error { return w.Abort() }

func (w *nativeSegmentWriter) finish(err error) error {
	w.finished = true
	if w.release != nil {
		err = errors.Join(err, w.release())
		w.release = nil
	}
	if w.complete != nil {
		w.complete()
		w.complete = nil
	}
	w.result = err
	return err
}

var _ io.Writer = (*nativeSegmentWriter)(nil)
var _ storage.Aborter = (*nativeSegmentWriter)(nil)
var _ nativeSegmentCreator = clientAdapter{}
