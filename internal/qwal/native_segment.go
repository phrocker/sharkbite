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

package qwal

import (
	"bytes"
	"fmt"
	"io"
)

// DecodeNativeSegment reads a complete pinned v4 no-crypto WAL file. The
// caller supplies the byte limit before reading the file; the decoder rejects
// malformed entries and requires a clean end after the final value.
func DecodeNativeSegment(data []byte) ([]Entry, error) {
	const maxSegmentBytes = 64 << 20
	if len(data) > maxSegmentBytes {
		return nil, fmt.Errorf("qwal: native segment exceeds %d bytes", maxSegmentBytes)
	}
	d := newDataInput(bytes.NewReader(data))
	if err := d.readHeader(); err != nil {
		return nil, err
	}
	var entries []Entry
	for {
		key, err := d.readKey()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("qwal: native key %d: %w", len(entries), err)
		}
		value, err := d.readValue()
		if err != nil {
			return nil, fmt.Errorf("qwal: native value %d: %w", len(entries), err)
		}
		entries = append(entries, Entry{Key: key, Value: value})
	}
	if len(entries) == 0 || entries[0].Key.Event != EventOpen {
		return nil, fmt.Errorf("qwal: native segment has no OPEN entry")
	}
	return entries, nil
}
