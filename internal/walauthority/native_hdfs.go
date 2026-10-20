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

package walauthority

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
)

// HDFSNativeStore uses a complete, no-replace HDFS object as each native WAL
// segment. The authoritative tablet metadata reference is installed only
// after Publish has closed and read back the object.
type HDFSNativeStore struct{ Backend *hdfs.Backend }

func (s HDFSNativeStore) Publish(ctx context.Context, path string, data []byte) error {
	if s.Backend == nil {
		return ErrInvalidConfig
	}
	return s.Backend.PublishUnreferenced(ctx, path, data)
}

func (s HDFSNativeStore) Read(ctx context.Context, path string) ([]byte, error) {
	if s.Backend == nil {
		return nil, ErrInvalidConfig
	}
	f, err := s.Backend.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	const limit = 64 << 20
	if f.Size() < 0 || f.Size() > limit {
		return nil, errors.Join(fmt.Errorf("walauthority: native WAL size %d exceeds limit", f.Size()), f.Close())
	}
	data := make([]byte, int(f.Size()))
	_, err = io.ReadFull(io.NewSectionReader(f, 0, f.Size()), data)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return data, nil
}

func (s HDFSNativeStore) Remove(ctx context.Context, path string) error {
	if s.Backend == nil {
		return ErrInvalidConfig
	}
	return s.Backend.Remove(ctx, path)
}

var _ NativeStore = HDFSNativeStore{}
