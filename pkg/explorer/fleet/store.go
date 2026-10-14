// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package fleet

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Stored struct {
	Descriptor         Descriptor
	RegistrationDigest [32]byte
	Digest             [32]byte
	Epoch              int64
}

type Mutation struct {
	RegistrationKey    shoal.ID
	ExpectedGeneration int64
	Descriptor         Descriptor
}

type StoredPage struct {
	Entries []Stored
	Next    []byte
}

// Store is the durable registry boundary. Apply must use a storage-level CAS
// over ExpectedGeneration and provide same-key identical replay semantics.
type Store interface {
	Apply(context.Context, Mutation) (Stored, error)
	Get(context.Context, shoal.ID) (Stored, error)
	List(context.Context, []byte, int) (StoredPage, error)
}

type Lifecycle struct {
	Operation     auth.Operation
	RequestID     shoal.ID
	CorrelationID shoal.ID
	Subject       shoal.ID
	Actor         shoal.ID
	ClientID      shoal.ID
	OnBehalfOf    []shoal.ID
	AgentID       shoal.ID
	// MutationDigest is the registry mutation digest the receipt is written
	// with: the v2 encoding (see registryMutationDigest), or zero for a read.
	MutationDigest [sha256.Size]byte
	// LegacyMutationDigest is the same mutation under the v1 encoding, which
	// receipts written before v2 carry. It is read, never written: a recorder
	// uses it only to reconcile a retry with such a receipt, and only for a
	// receipt whose identity says it holds a v1 digest.
	LegacyMutationDigest     [sha256.Size]byte
	ReasonCode               string
	ReasonDetail             string
	Deadline                 int64
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
	AuditPurpose             string
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
}

type InteractionSnapshotProvider interface {
	InteractionSnapshot(context.Context) (explorer.Snapshot, error)
}

// LifecycleRecorder is called before every privileged mutation. A recorder
// failure denies admission, so no mutation can succeed without its durable
// privileged-action record.
type LifecycleRecorder interface {
	RecordLifecycle(context.Context, Lifecycle) error
}
