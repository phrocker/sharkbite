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
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// RecoveryMode names the replay contract the external gateway actually used
// for the first prepared send. A later route configuration cannot reinterpret
// it. The first revision supports the gateway's three closed modes.
type RecoveryMode string

const (
	RecoveryKey         RecoveryMode = "key"
	RecoveryNatural     RecoveryMode = "natural"
	RecoveryUnprotected RecoveryMode = "unprotected"
)

const RecoveryCommitmentVersion = 1

// RecoveryCommitment is the action-bound statement a future
// prepare protocol will persist before the first external target send. The
// request digest covers a versioned canonical encoding of the bound method,
// full target URL, non-credential headers (including the idempotency key),
// and body. TargetScopeDigest separately commits to a stable operator-supplied
// target account or credential scope, not a rotating credential value. The
// digests are internal: a low-entropy request could be guessed from its hash.
//
// The digest recipe and authenticated prepare operation are specified in
// docs/gateway-recovery-commitment.md. This model alone does not authorize a
// send; a worker must verify an acknowledged durable prepare while its claim
// is still live. Legacy records have a nil commitment.
type RecoveryCommitment struct {
	Version           uint8
	Mode              RecoveryMode
	RequestDigest     [32]byte
	TargetScopeDigest [32]byte
	PreparedFence     uint64
	PreparedAt        time.Time
}

func (c RecoveryCommitment) validate() error {
	if c.Version != RecoveryCommitmentVersion {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action recovery commitment version is invalid")
	}
	switch c.Mode {
	case RecoveryKey, RecoveryNatural, RecoveryUnprotected:
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "action recovery mode is invalid")
	}
	if c.RequestDigest == ([32]byte{}) || c.TargetScopeDigest == ([32]byte{}) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action recovery commitment digest is missing")
	}
	if c.PreparedFence != 1 || c.PreparedAt.IsZero() || c.PreparedAt.Location() != time.UTC {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action recovery commitment origin is invalid")
	}
	return nil
}
