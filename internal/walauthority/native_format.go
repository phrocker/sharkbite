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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/qwal"
)

const nativeSessionPrefix = "shoal-op-v1:"

type nativeIdentity struct {
	OperationID string              `json:"operation_id"`
	SessionID   string              `json:"session_id"`
	RequestID   string              `json:"request_id"`
	Extent      ingestrouter.Extent `json:"extent"`
	Fence       ingestrouter.Fence  `json:"fence"`
	Sequence    int64               `json:"sequence"`
	Digest      string              `json:"digest"`
	WireDigest  string              `json:"wire_digest"`
}

func encodeNativeOperation(op operation) ([]byte, error) {
	if op.Sequence < 1 || op.Sequence > (math.MaxInt64-1)/2 ||
		op.OperationID == "" || op.SessionID == "" || op.RequestID == "" ||
		!op.Fence.Valid() || op.Extent.Validate() != nil || len(op.Mutations) == 0 ||
		op.Digest != operationDigest(op) {
		return nil, ErrInvalidCommit
	}
	mutations := make([]qwal.ServerMutation, len(op.Mutations))
	for i, mutation := range op.Mutations {
		if len(mutation.Row) == 0 || len(mutation.Updates) == 0 {
			return nil, ErrInvalidCommit
		}
		mutations[i].Row = append([]byte{}, mutation.Row...)
		mutations[i].Updates = make([]qwal.ColumnUpdate, len(mutation.Updates))
		for j, update := range mutation.Updates {
			if !update.Timestamp.Set {
				return nil, fmt.Errorf("%w: native WAL requires assigned timestamps", ErrInvalidCommit)
			}
			mutations[i].Updates[j] = qwal.ColumnUpdate{
				ColumnFamily:     append([]byte{}, update.ColumnFamily...),
				ColumnQualifier:  append([]byte{}, update.ColumnQualifier...),
				ColumnVisibility: append([]byte{}, update.ColumnVisibility...),
				HasTimestamp:     true, Timestamp: update.Timestamp.Value,
				Deleted: update.Delete, Value: append([]byte{}, update.Value...),
			}
		}
	}
	wireDigest, err := nativeMutationsDigest(mutations)
	if err != nil {
		return nil, err
	}
	identity := nativeIdentity{
		OperationID: op.OperationID, SessionID: op.SessionID, RequestID: op.RequestID,
		Extent: op.Extent, Fence: op.Fence, Sequence: op.Sequence,
		Digest: op.Digest, WireDigest: wireDigest,
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	session := nativeSessionPrefix + base64.RawURLEncoding.EncodeToString(identityJSON)
	var out bytes.Buffer
	w, err := qwal.NewNativeWriter(&out)
	if err != nil {
		return nil, err
	}
	entries := []qwal.Entry{
		{Key: qwal.LogFileKey{Event: qwal.EventOpen, TServerSession: session}},
		{Key: qwal.LogFileKey{Event: qwal.EventDefineTablet, TabletID: 1,
			Seq: op.Sequence * 2, Tablet: &qwal.KeyExtent{
				TableID: op.Extent.TableID, PrevEndRow: op.Extent.PrevEndRow,
				EndRow: op.Extent.EndRow,
			}}},
		{Key: qwal.LogFileKey{Event: qwal.EventManyMutations,
			TabletID: 1, Seq: op.Sequence*2 + 1},
			Value: qwal.LogFileValue{Mutations: mutations}},
	}
	for _, entry := range entries {
		if err := w.WriteEntry(entry); err != nil {
			return nil, err
		}
	}
	if out.Len() > 64<<20 {
		return nil, fmt.Errorf("%w: native WAL exceeds 64 MiB", ErrInvalidCommit)
	}
	return out.Bytes(), nil
}

func decodeNativeOperation(data []byte, expected ingestrouter.Extent) (operation, error) {
	var empty operation
	entries, err := qwal.DecodeNativeSegment(data)
	if err != nil {
		return empty, err
	}
	if len(entries) != 3 || entries[0].Key.Event != qwal.EventOpen ||
		entries[1].Key.Event != qwal.EventDefineTablet ||
		entries[2].Key.Event != qwal.EventManyMutations ||
		entries[1].Key.TabletID != 1 || entries[2].Key.TabletID != 1 ||
		entries[1].Key.Tablet == nil {
		return empty, ErrCorrupt
	}
	session := entries[0].Key.TServerSession
	if !strings.HasPrefix(session, nativeSessionPrefix) {
		return empty, ErrCorrupt
	}
	identityJSON, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(session, nativeSessionPrefix))
	if err != nil {
		return empty, errors.Join(ErrCorrupt, err)
	}
	var identity nativeIdentity
	if err := json.Unmarshal(identityJSON, &identity); err != nil {
		return empty, errors.Join(ErrCorrupt, err)
	}
	if identity.Sequence < 1 || identity.Sequence > (math.MaxInt64-1)/2 ||
		identity.OperationID == "" || identity.SessionID == "" || identity.RequestID == "" ||
		!identity.Fence.Valid() || !identity.Extent.Equal(expected) ||
		!nativeBoundaryEqual(identity.Extent.PrevEndRow, expected.PrevEndRow) ||
		!nativeBoundaryEqual(identity.Extent.EndRow, expected.EndRow) ||
		entries[1].Key.Seq != identity.Sequence*2 ||
		entries[2].Key.Seq != identity.Sequence*2+1 ||
		entries[1].Key.Tablet.TableID != expected.TableID ||
		!nativeBoundaryEqual(entries[1].Key.Tablet.PrevEndRow, expected.PrevEndRow) ||
		!nativeBoundaryEqual(entries[1].Key.Tablet.EndRow, expected.EndRow) {
		return empty, ErrCorrupt
	}
	mutations := entries[2].Value.Mutations
	if len(mutations) == 0 {
		return empty, ErrCorrupt
	}
	wireDigest, err := nativeMutationsDigest(mutations)
	if err != nil || wireDigest != identity.WireDigest {
		return empty, errors.Join(ErrCorrupt, err)
	}
	op := operation{
		Sequence: identity.Sequence, OperationID: identity.OperationID,
		SessionID: identity.SessionID, RequestID: identity.RequestID,
		Extent: identity.Extent, Fence: identity.Fence,
		Digest: identity.Digest, Mutations: make([]ingestrouter.Mutation, len(mutations)),
	}
	for i, mutation := range mutations {
		if len(mutation.Row) == 0 || len(mutation.Updates) == 0 {
			return empty, ErrCorrupt
		}
		op.Mutations[i].Row = append([]byte(nil), mutation.Row...)
		op.Mutations[i].Updates = make([]ingestrouter.Update, len(mutation.Updates))
		for j, update := range mutation.Updates {
			if !update.HasTimestamp {
				return empty, ErrCorrupt
			}
			op.Mutations[i].Updates[j] = ingestrouter.Update{
				ColumnFamily:     append([]byte(nil), update.ColumnFamily...),
				ColumnQualifier:  append([]byte(nil), update.ColumnQualifier...),
				ColumnVisibility: append([]byte(nil), update.ColumnVisibility...),
				Timestamp:        ingestrouter.Timestamp{Set: true, Value: update.Timestamp},
				Delete:           update.Deleted, Value: append([]byte(nil), update.Value...),
			}
		}
	}
	if identity.Digest == "" || operationDigest(op) != identity.Digest {
		return empty, ErrCorrupt
	}
	return op, nil
}

func nativeBoundaryEqual(a, b []byte) bool {
	return (a == nil) == (b == nil) && bytes.Equal(a, b)
}

func nativeMutationsDigest(mutations []qwal.ServerMutation) (string, error) {
	canonical := make([]qwal.ServerMutation, len(mutations))
	for i, mutation := range mutations {
		canonical[i].Row = append([]byte{}, mutation.Row...)
		canonical[i].SystemTime = mutation.SystemTime
		canonical[i].Updates = make([]qwal.ColumnUpdate, len(mutation.Updates))
		for j, update := range mutation.Updates {
			canonical[i].Updates[j] = qwal.ColumnUpdate{
				ColumnFamily:     append([]byte{}, update.ColumnFamily...),
				ColumnQualifier:  append([]byte{}, update.ColumnQualifier...),
				ColumnVisibility: append([]byte{}, update.ColumnVisibility...),
				Timestamp:        update.Timestamp, HasTimestamp: update.HasTimestamp,
				Deleted: update.Deleted, Value: append([]byte{}, update.Value...),
			}
		}
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
