// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hdfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// noOverwriteCreator is a stronger contract than Client.Create: its Create
// operation must fail atomically if the destination already exists. The
// production adapter uses HDFS CreateFlag.CREATE without OVERWRITE. Injected
// clients must opt in explicitly rather than silently weakening this rule.
type noOverwriteCreator interface {
	CreateNoOverwrite(string) (storage.Writer, error)
}

// PublishUnreferenced creates a complete, immutable file at a unique path.
// HDFS can expose the file while it is being written. A caller must not add an
// authoritative metadata reference until this method returns nil. A failed
// call may leave an unreferenced orphan; it must never be treated as committed.
// This is deliberately separate from CreateExclusive, whose writer promises
// that the destination is invisible until Close.
func (b *Backend) PublishUnreferenced(ctx context.Context, objectPath string, data []byte) error {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return err
	}
	creator, ok := lease.client.(noOverwriteCreator)
	if !ok {
		_ = lease.release()
		return fmt.Errorf("hdfs: atomic create without overwrite unavailable: %w", errors.ErrUnsupported)
	}
	resolved, _, err := b.resolve(objectPath)
	if err != nil {
		_ = lease.release()
		return err
	}
	if isReplacementArtifactName(path.Base(resolved)) {
		_ = lease.release()
		return fmt.Errorf("hdfs: destination %s uses a reserved internal namespace", objectPath)
	}
	if parent := path.Dir(resolved); parent != "." && parent != "/" {
		if err := lease.client.MkdirAll(parent, 0o755); err != nil {
			_ = lease.release()
			return fmt.Errorf("hdfs: mkdir %s: %w", parent, err)
		}
	}
	writer, err := creator.CreateNoOverwrite(resolved)
	if err != nil {
		_ = lease.release()
		if isAlreadyExists(err) {
			return fmt.Errorf("%w: %s", storage.ErrExists, objectPath)
		}
		return fmt.Errorf("hdfs: create immutable %s: %w", objectPath, err)
	}
	if err := applyDeadline(ctx, writer); err != nil {
		closeErr := writer.Close()
		return errors.Join(err, closeErr, lease.release())
	}
	n, writeErr := writer.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	closed, closeErr := closeAfterReplication(ctx, writer)
	releaseErr := lease.release()
	if !closed && closeErr == nil {
		closeErr = errors.New("hdfs: immutable writer did not close")
	}
	if writeErr != nil || closeErr != nil || !closed || releaseErr != nil {
		return errors.Join(writeErr, closeErr, releaseErr)
	}
	reader, err := b.Open(ctx, objectPath)
	if err != nil {
		return fmt.Errorf("hdfs: read back immutable %s: %w", objectPath, err)
	}
	defer reader.Close()
	if reader.Size() != int64(len(data)) {
		return fmt.Errorf("hdfs: immutable %s readback length %d, want %d", objectPath, reader.Size(), len(data))
	}
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if _, err := reader.ReadAt(buf[:end-off], int64(off)); err != nil {
			return fmt.Errorf("hdfs: read back immutable %s: %w", objectPath, err)
		}
		if !bytes.Equal(buf[:end-off], data[off:end]) {
			return fmt.Errorf("hdfs: immutable %s readback differs at offset %d", objectPath, off)
		}
	}
	return nil
}

var _ noOverwriteCreator = clientAdapter{}

// colinmarc/hdfs Client.Create checks existence and issues a NameNode create
// RPC with CREATE only (no OVERWRITE). The RPC, rather than its prior stat,
// supplies the atomic no-replace guarantee.
func (c clientAdapter) CreateNoOverwrite(name string) (storage.Writer, error) {
	return c.Client.Create(name)
}
