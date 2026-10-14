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

package coordination

import (
	"bytes"
	"crypto/sha256"
	"time"
)

type WriterAuthorityV1 struct {
	Term              AuthorityTerm
	Generation        Generation
	Owner             OwnerID
	LeaseUntil        time.Time
	Fence             Fence
	State             AuthorityState
	PredecessorDigest Digest
	Digest            Digest
}

func (a WriterAuthorityV1) computedDigest() Digest {
	e := newPayloadEncoder(MaxRootBytes)
	e.bytes("authority term", a.Term)
	e.u64(uint64(a.Generation))
	e.bytes("owner", a.Owner)
	e.timestamp(a.LeaseUntil)
	e.u64(uint64(a.Fence))
	e.byte(byte(a.State))
	e.digest(a.PredecessorDigest)
	return sha256.Sum256(e.data)
}

func (a WriterAuthorityV1) ComputeDigest() Digest { return a.computedDigest() }

func (a WriterAuthorityV1) Validate() error {
	if err := a.Term.Validate(); err != nil {
		return err
	}
	if err := a.Generation.Validate(); err != nil {
		return err
	}
	if err := a.Owner.Validate(); err != nil {
		return err
	}
	if err := a.Fence.Validate(); err != nil {
		return err
	}
	if err := a.State.Validate(); err != nil {
		return err
	}
	if a.State == AuthorityActive {
		if err := validateTime("authority lease", a.LeaseUntil, false); err != nil {
			return err
		}
	} else if !a.LeaseUntil.IsZero() {
		return invalid("inactive writer authority must not have a lease")
	}
	if a.Digest != a.computedDigest() {
		return invalid("writer-authority digest mismatch")
	}
	return nil
}

func ValidateWriterAuthorityAcquisition(previous *WriterAuthorityV1, next WriterAuthorityV1) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.State != AuthorityActive {
		return invalid("acquired writer authority must be active")
	}
	if previous == nil {
		if next.PredecessorDigest != (Digest{}) {
			return invalid("initial writer authority must have no predecessor")
		}
		return nil
	}
	if err := previous.Validate(); err != nil {
		return err
	}
	if next.Generation <= previous.Generation {
		return invalid("writer-authority generation must increase")
	}
	if next.Fence <= previous.Fence {
		return invalid("writer-authority fence must increase")
	}
	if next.PredecessorDigest != previous.Digest {
		return invalid("writer-authority predecessor digest mismatch")
	}
	if bytes.Equal(next.Term, previous.Term) {
		return invalid("new writer-authority acquisition requires a new term")
	}
	return nil
}

func ValidateWriterAuthorityTransition(previous, next WriterAuthorityV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.Term, next.Term) ||
		previous.Generation != next.Generation ||
		!bytes.Equal(previous.Owner, next.Owner) ||
		previous.Fence != next.Fence ||
		previous.PredecessorDigest != next.PredecessorDigest {
		return invalid("writer-authority term identity is immutable")
	}
	if previous.State != AuthorityActive {
		return invalid("inactive writer authority cannot transition")
	}
	switch next.State {
	case AuthorityActive:
		if next.LeaseUntil.Before(previous.LeaseUntil) {
			return invalid("writer-authority renewal cannot shorten its lease")
		}
	case AuthorityRevoked, AuthoritySuperseded:
		if !next.LeaseUntil.IsZero() {
			return invalid("inactive writer authority must clear its lease")
		}
	default:
		return invalid("illegal writer-authority transition")
	}
	return nil
}

func encodeWriterAuthority(e *encoder, a WriterAuthorityV1) {
	e.bytes("authority term", a.Term)
	e.u64(uint64(a.Generation))
	e.bytes("owner", a.Owner)
	e.timestamp(a.LeaseUntil)
	e.u64(uint64(a.Fence))
	e.byte(byte(a.State))
	e.digest(a.PredecessorDigest)
	e.digest(a.Digest)
}

func MarshalWriterAuthorityV1(a WriterAuthorityV1) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindWriterAuthority, VersionWriterAuthorityV1, MaxRootBytes,
		func(e *encoder) { encodeWriterAuthority(e, a) })
}

func UnmarshalWriterAuthorityV1(data []byte) (WriterAuthorityV1, error) {
	payload, err := verifyEnvelope(data, KindWriterAuthority, VersionWriterAuthorityV1, MaxRootBytes)
	if err != nil {
		return WriterAuthorityV1{}, err
	}
	d := &decoder{data: payload}
	a := WriterAuthorityV1{
		Term:              AuthorityTerm(d.bytes("authority term", MaxOpaqueIDBytes, true)),
		Generation:        Generation(d.positive("generation")),
		Owner:             OwnerID(d.bytes("owner", MaxOwnerBytes, true)),
		LeaseUntil:        d.timestamp("lease"),
		Fence:             Fence(d.positive("fence")),
		State:             AuthorityState(d.byte("state")),
		PredecessorDigest: d.digest("predecessor digest"),
		Digest:            d.digest("digest"),
	}
	if d.err != nil {
		return WriterAuthorityV1{}, d.err
	}
	if err := a.Validate(); err != nil {
		return WriterAuthorityV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeWriterAuthority(e, a) }, MaxRootBytes); err != nil {
		return WriterAuthorityV1{}, err
	}
	return a, nil
}

type BackendObservationV1 struct {
	Backend             BackendID
	AuthorityGeneration Generation
	AuthorityFence      Fence
	ObservedFrontier    Epoch
	State               BackendState
	ObservedDigest      Digest
	ObservedAt          time.Time
}

func (o BackendObservationV1) Validate() error {
	if err := o.Backend.Validate(); err != nil {
		return err
	}
	if err := o.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := o.AuthorityFence.Validate(); err != nil {
		return err
	}
	if err := o.ObservedFrontier.Validate(); err != nil {
		return err
	}
	if err := o.State.Validate(); err != nil {
		return err
	}
	if err := o.ObservedDigest.Validate("backend observed digest"); err != nil {
		return err
	}
	return validateTime("backend observed_at", o.ObservedAt, false)
}

func (o BackendObservationV1) RejectsAuthority(generation Generation, fence Fence) bool {
	return generation < o.AuthorityGeneration ||
		generation == o.AuthorityGeneration && fence < o.AuthorityFence
}

func ValidateBackendObservationSuccessor(previous, next BackendObservationV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.Backend, next.Backend) {
		return invalid("backend observation identity is immutable")
	}
	if next.AuthorityGeneration < previous.AuthorityGeneration {
		return invalid("backend observation rejected stale authority generation")
	}
	if next.AuthorityGeneration == previous.AuthorityGeneration &&
		next.AuthorityFence < previous.AuthorityFence {
		return invalid("backend observation rejected stale authority fence")
	}
	if next.AuthorityGeneration > previous.AuthorityGeneration &&
		next.AuthorityFence <= previous.AuthorityFence {
		return invalid("new backend authority generation requires a greater fence")
	}
	if next.ObservedFrontier < previous.ObservedFrontier {
		return invalid("backend observed frontier must not decrease")
	}
	if next.ObservedAt.Before(previous.ObservedAt) {
		return invalid("backend observation time must not decrease")
	}
	return nil
}

func encodeBackendObservation(e *encoder, o BackendObservationV1) {
	e.bytes("backend", o.Backend)
	e.u64(uint64(o.AuthorityGeneration))
	e.u64(uint64(o.AuthorityFence))
	e.u64(uint64(o.ObservedFrontier))
	e.byte(byte(o.State))
	e.digest(o.ObservedDigest)
	e.timestamp(o.ObservedAt)
}

func MarshalBackendObservationV1(o BackendObservationV1) ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindBackendObservation, VersionBackendObservationV1, MaxRootBytes,
		func(e *encoder) { encodeBackendObservation(e, o) })
}

func UnmarshalBackendObservationV1(data []byte) (BackendObservationV1, error) {
	payload, err := verifyEnvelope(data, KindBackendObservation, VersionBackendObservationV1, MaxRootBytes)
	if err != nil {
		return BackendObservationV1{}, err
	}
	d := &decoder{data: payload}
	o := BackendObservationV1{
		Backend:             BackendID(d.bytes("backend", MaxBackendIDBytes, true)),
		AuthorityGeneration: Generation(d.positive("authority generation")),
		AuthorityFence:      Fence(d.positive("authority fence")),
		ObservedFrontier:    Epoch(d.positive("observed frontier")),
		State:               BackendState(d.byte("state")),
		ObservedDigest:      d.digest("observed digest"),
		ObservedAt:          d.timestamp("observed_at"),
	}
	if d.err != nil {
		return BackendObservationV1{}, d.err
	}
	if err := o.Validate(); err != nil {
		return BackendObservationV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeBackendObservation(e, o) }, MaxRootBytes); err != nil {
		return BackendObservationV1{}, err
	}
	return o, nil
}
