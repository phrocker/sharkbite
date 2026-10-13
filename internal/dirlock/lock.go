/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package dirlock provides process- and OS-exclusive ownership of a storage
// directory for embedded stores that cannot safely share WAL state.
package dirlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrLocked = errors.New("directory lock: already held")

var held = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

type Lock struct {
	mu       sync.Mutex
	file     *os.File
	identity string
	closed   bool
}

// Acquire creates directory when needed, resolves its canonical path, rejects
// duplicate ownership in this process, and acquires a nonblocking OS lock.
// name must be one plain file name and should be stable for the store type.
func Acquire(directory, name string) (*Lock, error) {
	return acquire(directory, name, true)
}

// AcquireExisting is Acquire for a directory that must already exist: it
// never creates one, so a caller that must not conjure an empty store from
// a mistyped path has no check-then-create window. A missing directory is an
// error wrapping fs.ErrNotExist.
func AcquireExisting(directory, name string) (*Lock, error) {
	return acquire(directory, name, false)
}

// AcquireExistingFile is AcquireExisting for a lock file that must already
// exist too, for a reader that must not write the directory at all: it
// creates neither, and only opens and locks the lock file. A missing
// directory or lock file is an error wrapping fs.ErrNotExist.
func AcquireExistingFile(directory, name string) (*Lock, error) {
	return acquireLock(directory, name, false, false)
}

func acquire(directory, name string, create bool) (*Lock, error) {
	return acquireLock(directory, name, create, true)
}

func acquireLock(directory, name string, create, createFile bool) (*Lock, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("directory lock: directory is required")
	}
	if name == "" || name == "." || name == ".." ||
		filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return nil, errors.New("directory lock: lock file name is invalid")
	}
	if create {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, fmt.Errorf("directory lock: create directory: %w", err)
		}
	}
	canonical, err := canonicalDirectory(directory)
	if err != nil {
		return nil, err
	}
	identity := pathIdentity(filepath.Join(canonical, name))
	held.Lock()
	if _, exists := held.paths[identity]; exists {
		held.Unlock()
		return nil, ErrLocked
	}
	held.paths[identity] = struct{}{}
	held.Unlock()

	// Opened for writing even when it must not be created, though nothing is
	// written to it: NFS emulates flock with a write lock, which needs it.
	flags := os.O_RDWR
	if createFile {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(filepath.Join(canonical, name), flags, 0o600)
	if err != nil {
		releaseIdentity(identity)
		return nil, fmt.Errorf("directory lock: open lock file: %w", err)
	}
	if err := tryLockFile(file); err != nil {
		_ = file.Close()
		releaseIdentity(identity)
		return nil, lockError(err)
	}
	return &Lock{file: file, identity: identity}, nil
}

// lockError classifies a failed nonblocking lock. Only contention, another
// holder, is ErrLocked; anything else (no lock support on the filesystem,
// a bad descriptor, a refused access) is returned as itself, so a caller does
// not tell the operator to stop a holder that does not exist.
func lockError(err error) error {
	if isContention(err) {
		return errors.Join(ErrLocked, err)
	}
	return fmt.Errorf("directory lock: lock file: %w", err)
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	releaseIdentity(l.identity)
	return errors.Join(unlockErr, closeErr)
}

func canonicalDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("directory lock: resolve absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("directory lock: resolve canonical path: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func releaseIdentity(identity string) {
	held.Lock()
	delete(held.paths, identity)
	held.Unlock()
}
