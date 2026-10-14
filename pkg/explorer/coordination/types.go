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

// Package coordination defines byte-exact control records and row keys used
// by Explorer's distributed publication protocol. It deliberately does not
// encode public logical Explorer values.
package coordination

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"
)

const (
	MaxOpaqueIDBytes       = 1024
	MaxOwnerBytes          = 1024
	MaxResultIdentities    = 4096
	MaxResultIdentityBytes = 1024
	MaxLPARTs              = 64
	MaxRootBytes           = 64 << 10
	MaxManifestEntries     = 32_000_000
	MaxChunkEntries        = 4096
	MaxChunkBytes          = 1 << 20
	MaxCoordinateBytes     = 16 << 10
	MaxManifestValueBytes  = 1 << 20
	MaxPolicyCopyEntries   = 4096
	MaxIndexDeltaEntries   = 4096
	MaxPolicyCopyPins      = 64
	MaxIndexPins           = 64
	MaxBackendIDBytes      = 1024
	MaxObjectKindBytes     = 256
	MaxActiveReservations  = 10_000
)

type DomainID []byte
type TXN []byte
type LPART []byte
type OwnerID []byte
type IGEN []byte
type Family []byte
type EntityKind []byte
type EntityID []byte
type LeaseID []byte
type BackendID []byte
type AuthorityTerm []byte

type Digest [sha256.Size]byte

func (d Digest) String() string { return hex.EncodeToString(d[:]) }
func Sum(data []byte) Digest    { return sha256.Sum256(data) }

func (d Digest) Validate(name string) error {
	if d == (Digest{}) {
		return invalid(name + " is required")
	}
	return nil
}

type Epoch int64
type Generation int64
type Fence int64

type WriterMode uint8

const (
	WriterModeEmbeddedPrimary WriterMode = iota + 1
	WriterModeAccumuloPrimary
)

func (m WriterMode) Validate() error {
	if m != WriterModeEmbeddedPrimary && m != WriterModeAccumuloPrimary {
		return invalid("writer mode is unknown")
	}
	return nil
}

func validateOpaque(name string, value []byte, maximum int, required bool) error {
	if required && len(value) == 0 {
		return invalid(name + " is required")
	}
	if len(value) > maximum {
		return invalid(fmt.Sprintf("%s exceeds %d bytes", name, maximum))
	}
	return nil
}

func (v DomainID) Validate() error { return validateOpaque("domain ID", v, MaxOpaqueIDBytes, true) }
func (v TXN) Validate() error      { return validateOpaque("transaction ID", v, MaxOpaqueIDBytes, true) }
func (v LPART) Validate() error    { return validateOpaque("LPART", v, MaxOpaqueIDBytes, true) }
func (v OwnerID) Validate() error  { return validateOpaque("owner ID", v, MaxOwnerBytes, true) }
func (v IGEN) Validate() error {
	return validateOpaque("index generation ID", v, MaxOpaqueIDBytes, true)
}
func (v Family) Validate() error { return validateOpaque("index family", v, MaxOpaqueIDBytes, true) }
func (v EntityKind) Validate() error {
	return validateOpaque("entity kind", v, MaxObjectKindBytes, true)
}
func (v EntityID) Validate() error {
	return validateOpaque("entity ID", v, MaxOpaqueIDBytes, true)
}
func (v LeaseID) Validate() error {
	return validateOpaque("lease ID", v, MaxOpaqueIDBytes, true)
}
func (v BackendID) Validate() error {
	return validateOpaque("backend ID", v, MaxBackendIDBytes, true)
}
func (v AuthorityTerm) Validate() error {
	return validateOpaque("authority term", v, MaxOpaqueIDBytes, true)
}

func validatePositive(name string, value int64) error {
	if value <= 0 {
		return invalid(name + " must be between 1 and MaxInt64")
	}
	return nil
}

func (v Epoch) Validate() error      { return validatePositive("epoch", int64(v)) }
func (v Generation) Validate() error { return validatePositive("generation", int64(v)) }
func (v Fence) Validate() error      { return validatePositive("fence", int64(v)) }

func validateTime(name string, value time.Time, optional bool) error {
	if value.IsZero() {
		if optional {
			return nil
		}
		return invalid(name + " is required")
	}
	year := value.UTC().Year()
	if year < 1 || year > 9999 {
		return invalid(name + " is outside years 1 through 9999")
	}
	if value.Location() != time.UTC {
		return invalid(name + " must use UTC")
	}
	return nil
}

func checkedAdd(name string, total *uint64, value uint64) error {
	if value > math.MaxInt64 || *total > math.MaxInt64-value {
		return invalid(name + " overflows the supported int64 range")
	}
	*total += value
	return nil
}

type TxnState uint8

const (
	StateAbsent TxnState = iota
	StateClaimed
	StatePlanned
	StateGuardsAcquired
	StateEpochReserved
	StateWriting
	StateVerified
	StatePrepared
	StateCommitted
	StateAborted
	StateConflicted
	StatePoisoned
)

func (s TxnState) String() string {
	switch s {
	case StateAbsent:
		return "ABSENT"
	case StateClaimed:
		return "CLAIMED"
	case StatePlanned:
		return "PLANNED"
	case StateGuardsAcquired:
		return "GUARDS_ACQUIRED"
	case StateEpochReserved:
		return "EPOCH_RESERVED"
	case StateWriting:
		return "WRITING"
	case StateVerified:
		return "VERIFIED"
	case StatePrepared:
		return "PREPARED"
	case StateCommitted:
		return "COMMITTED"
	case StateAborted:
		return "ABORTED"
	case StateConflicted:
		return "CONFLICTED"
	case StatePoisoned:
		return "POISONED"
	default:
		return fmt.Sprintf("TxnState(%d)", s)
	}
}

func (s TxnState) ValidatePersisted() error {
	if s < StateClaimed || s > StatePoisoned {
		return invalid("transaction state is not persistable")
	}
	return nil
}

func (s TxnState) Terminal() bool {
	return s == StateCommitted || s == StateAborted ||
		s == StateConflicted || s == StatePoisoned
}

func (s TxnState) Nonterminal() bool {
	return s >= StateClaimed && s <= StatePrepared
}

func ValidateTransition(from, to TxnState) error {
	if from == StateAbsent {
		if to == StateClaimed {
			return nil
		}
		return invalid("ABSENT may transition only to CLAIMED")
	}
	if err := from.ValidatePersisted(); err != nil {
		return err
	}
	if err := to.ValidatePersisted(); err != nil {
		return err
	}
	if from.Terminal() {
		return invalid("terminal transaction state cannot transition")
	}
	if to == StateAborted || to == StateConflicted || to == StatePoisoned {
		return nil
	}
	if to == from+1 {
		return nil
	}
	return invalid(fmt.Sprintf("illegal transaction transition %s to %s", from, to))
}

func invalid(message string) error { return fmt.Errorf("coordination: %s", message) }

type GuardState uint8

const (
	GuardStateHeld GuardState = iota + 1
	GuardStateReleased
	GuardStateCommitted
	GuardStateAborted
	GuardStateConflicted
	GuardStatePoisoned
)

func (s GuardState) Validate() error {
	if s < GuardStateHeld || s > GuardStatePoisoned {
		return invalid("guard state is unknown")
	}
	return nil
}

func (s GuardState) Terminal() bool { return s >= GuardStateCommitted }

type LifecycleState uint8

const (
	LifecycleBuilding LifecycleState = iota + 1
	LifecycleVerified
	LifecycleActive
	LifecycleRetired
	LifecyclePoisoned
)

func (s LifecycleState) Validate() error {
	if s < LifecycleBuilding || s > LifecyclePoisoned {
		return invalid("lifecycle state is unknown")
	}
	return nil
}

type IndexGenerationState uint8

const (
	IndexGenerationBuilding IndexGenerationState = 1
	IndexGenerationSealed   IndexGenerationState = 2
	IndexGenerationPoisoned IndexGenerationState = 5
)

func (s IndexGenerationState) Validate() error {
	switch s {
	case IndexGenerationBuilding, IndexGenerationSealed, IndexGenerationPoisoned:
		return nil
	default:
		return invalid("index-generation state is unknown")
	}
}

type CopyState uint8

const (
	CopyStateBuilding CopyState = iota + 1
	CopyStateSealed
	CopyStateActive
	CopyStateRetired
	CopyStatePoisoned
)

func (s CopyState) Validate() error {
	if s < CopyStateBuilding || s > CopyStatePoisoned {
		return invalid("policy-copy state is unknown")
	}
	return nil
}

type ActivationKind uint8

const (
	ActivationContentTXN ActivationKind = iota + 1
	ActivationPolicyRoot
)

func (k ActivationKind) Validate() error {
	if k != ActivationContentTXN && k != ActivationPolicyRoot {
		return invalid("activation kind is unknown")
	}
	return nil
}

type LeaseState uint8

const (
	LeaseStateActive LeaseState = iota + 1
	LeaseStateReleased
	LeaseStateExpired
)

func (s LeaseState) Validate() error {
	if s < LeaseStateActive || s > LeaseStateExpired {
		return invalid("lease state is unknown")
	}
	return nil
}

type RetirementState uint8

const (
	RetirementCandidate RetirementState = iota + 1
	RetirementApproved
	RetirementRejected
	RetirementApplied
	RetirementPoisoned
)

func (s RetirementState) Validate() error {
	if s < RetirementCandidate || s > RetirementPoisoned {
		return invalid("retirement state is unknown")
	}
	return nil
}

type AuthorityState uint8

const (
	AuthorityActive AuthorityState = iota + 1
	AuthorityRevoked
	AuthoritySuperseded
)

func (s AuthorityState) Validate() error {
	if s < AuthorityActive || s > AuthoritySuperseded {
		return invalid("authority state is unknown")
	}
	return nil
}

type BackendState uint8

const (
	BackendWriteClosed BackendState = iota + 1
	BackendPrimary
	BackendReplica
)

func (s BackendState) Validate() error {
	if s < BackendWriteClosed || s > BackendReplica {
		return invalid("backend state is unknown")
	}
	return nil
}
