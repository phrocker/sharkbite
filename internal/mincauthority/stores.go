// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mincauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/storage"
)

// BackendOutputStore adapts native Shoal storage backends. RFile bytes become
// visible only through the backend writer's durable Close contract.
type BackendOutputStore struct {
	Backend storage.Backend
}

func (s BackendOutputStore) Publish(ctx context.Context, path string, data []byte) error {
	if s.Backend == nil {
		return ErrInvalidConfig
	}
	existing, err := storage.ReadAll(ctx, s.Backend, path)
	switch {
	case err == nil && bytes.Equal(existing, data):
		return nil
	case err == nil:
		return fmt.Errorf("%w: refusing to replace different bytes at %s", ErrCorruptOutput, path)
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	return storage.WriteAll(ctx, s.Backend, path, data)
}

func (s BackendOutputStore) Read(ctx context.Context, path string) ([]byte, error) {
	if s.Backend == nil {
		return nil, ErrInvalidConfig
	}
	return storage.ReadAll(ctx, s.Backend, path)
}

func (s BackendOutputStore) Remove(ctx context.Context, path string) error {
	if s.Backend == nil {
		return ErrInvalidConfig
	}
	remover, ok := s.Backend.(storage.Remover)
	if !ok {
		return storage.ErrReadOnly
	}
	return remover.Remove(ctx, path)
}

// FileStateStore persists one checksummed JSON checkpoint per operation.
type FileStateStore struct {
	Dir string
	mu  sync.Mutex
}

// Pending returns the latest durable checkpoint for every incomplete
// operation matching one tablet generation.
func (s *FileStateStore) Pending(
	ctx context.Context,
	extent ingestrouter.Extent,
	fence ingestrouter.Fence,
) ([]State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.Dir == "" {
		return nil, ErrInvalidConfig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	latest := make(map[string]State)
	completed := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
			strings.Contains(entry.Name(), ".new-") {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		state, err := decodeAnyState(encoded)
		if err != nil {
			return nil, err
		}
		if !state.Extent.Equal(extent) || state.Fence != fence {
			continue
		}
		if state.Phase == PhaseComplete {
			completed[state.OperationID] = struct{}{}
			delete(latest, state.OperationID)
			continue
		}
		if _, ok := completed[state.OperationID]; ok {
			continue
		}
		if prior, ok := latest[state.OperationID]; !ok || state.Phase > prior.Phase {
			latest[state.OperationID] = *state
		}
	}
	out := make([]State, 0, len(latest))
	for _, state := range latest {
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationID < out[j].OperationID })
	return out, nil
}

type stateEnvelope struct {
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

func (s *FileStateStore) Load(ctx context.Context, operationID string) (*State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.Dir == "" || operationID == "" {
		return nil, ErrInvalidConfig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for phase := PhaseComplete; phase >= PhaseSnapshotted; phase-- {
		data, err := os.ReadFile(s.path(operationID, phase))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		state, err := decodeState(data, operationID)
		if err != nil {
			return nil, err
		}
		if state.Phase != phase {
			return nil, fmt.Errorf("%w: checkpoint phase mismatch", ErrInvalidSnapshot)
		}
		return state, nil
	}
	return nil, nil
}

func decodeState(data []byte, operationID string) (*State, error) {
	state, err := decodeAnyState(data)
	if err != nil {
		return nil, err
	}
	if state.OperationID != operationID {
		return nil, fmt.Errorf("%w: checkpoint operation mismatch", ErrInvalidSnapshot)
	}
	return state, nil
}

func decodeAnyState(data []byte) (*State, error) {
	var envelope stateEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode checkpoint: %v", ErrInvalidSnapshot, err)
	}
	sum := sha256.Sum256(envelope.State)
	if envelope.Checksum != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("%w: checkpoint checksum mismatch", ErrInvalidSnapshot)
	}
	var state State
	if err := json.Unmarshal(envelope.State, &state); err != nil {
		return nil, fmt.Errorf("%w: decode state: %v", ErrInvalidSnapshot, err)
	}
	if state.OperationID == "" {
		return nil, fmt.Errorf("%w: empty checkpoint operation", ErrInvalidSnapshot)
	}
	return &state, nil
}

func (s *FileStateStore) Save(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.Dir == "" || state.OperationID == "" {
		return ErrInvalidConfig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	data, err := json.Marshal(stateEnvelope{Checksum: hex.EncodeToString(sum[:]), State: raw})
	if err != nil {
		return err
	}
	final := s.path(state.OperationID, state.Phase)
	if existing, err := os.ReadFile(final); err == nil {
		_, decodeErr := decodeState(existing, state.OperationID)
		if decodeErr != nil {
			return decodeErr
		}
		if string(existing) != string(data) {
			return fmt.Errorf("%w: checkpoint phase already has different state", ErrInvalidSnapshot)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(s.Dir, filepath.Base(final)+".new-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		existing, readErr := os.ReadFile(final)
		if readErr != nil || string(existing) != string(data) {
			return errors.Join(err, readErr)
		}
	}
	cleanup = false
	return syncDirectory(s.Dir)
}

func (s *FileStateStore) path(operationID string, phase Phase) string {
	sum := sha256.Sum256([]byte(operationID))
	return filepath.Join(s.Dir, fmt.Sprintf("%s.%d.json", hex.EncodeToString(sum[:]), phase))
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
