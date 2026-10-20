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
	"bytes"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
)

func TestNativeOperationRoundTripAndBounds(t *testing.T) {
	op := operation{
		Sequence: 3, OperationID: "op-1", SessionID: "session-1", RequestID: "request-1",
		Extent: ingestrouter.Extent{TableID: "5", EndRow: []byte("z")},
		Fence:  ingestrouter.Fence{ServerGeneration: "s", ManagerGeneration: "m", Assignment: 1},
		Mutations: []ingestrouter.Mutation{{Row: []byte("row"), Updates: []ingestrouter.Update{{
			ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
			Timestamp: ingestrouter.Timestamp{Set: true, Value: 42}, Value: []byte("value"),
		}}}},
	}
	op.Digest = operationDigest(op)
	data, err := encodeNativeOperation(op)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeNativeOperation(data, op.Extent)
	if err != nil {
		t.Fatal(err)
	}
	if got.OperationID != op.OperationID || got.Digest != op.Digest || got.Sequence != op.Sequence ||
		!bytes.Equal(got.Mutations[0].Updates[0].Value, []byte("value")) ||
		got.Mutations[0].Updates[0].Timestamp.Value != 42 {
		t.Fatalf("native replay differs: %+v", got)
	}
	if operationDigest(got) != op.Digest {
		t.Fatal("recovered operation digest differs")
	}
	if _, err := decodeNativeOperation(data, ingestrouter.Extent{TableID: "other"}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("cross-tablet replay = %v, want corrupt", err)
	}
	if _, err := decodeNativeOperation(data, ingestrouter.Extent{
		TableID: "5", PrevEndRow: []byte{}, EndRow: []byte("z"),
	}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("nil-versus-empty boundary replay = %v, want corrupt", err)
	}
	if _, err := decodeNativeOperation(data[:len(data)-1], op.Extent); err == nil {
		t.Fatal("truncated native WAL was accepted")
	}
	data[len(data)-1] ^= 1
	if _, err := decodeNativeOperation(data, op.Extent); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("changed mutation bytes = %v, want corrupt", err)
	}
}
