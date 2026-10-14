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
 */

package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

var catalogMagic = [4]byte{'E', 'C', 'A', '2'}

type writer struct{ bytes.Buffer }

func (w *writer) raw(value []byte) { _, _ = w.Write(value) }
func (w *writer) u8(value byte)    { _ = w.WriteByte(value) }
func (w *writer) u32(value uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], value)
	w.raw(b[:])
}
func (w *writer) u64(value uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], value)
	w.raw(b[:])
}
func (w *writer) i64(value int64) { w.u64(uint64(value)) }
func (w *writer) blob(value []byte) {
	w.u32(uint32(len(value)))
	w.raw(value)
}
func (w *writer) digest(value coordination.Digest) { w.raw(value[:]) }
func (w *writer) timestamp(value time.Time) {
	w.i64(value.Unix())
	w.u32(uint32(value.Nanosecond()))
}

type reader struct {
	data []byte
	off  int
	err  error
}

func (r *reader) take(size int) []byte {
	if r.err != nil {
		return nil
	}
	if size < 0 || r.off > len(r.data)-size {
		r.err = errors.New("catalog: truncated record")
		return nil
	}
	value := r.data[r.off : r.off+size]
	r.off += size
	return value
}
func (r *reader) u8() byte {
	value := r.take(1)
	if len(value) == 0 {
		return 0
	}
	return value[0]
}
func (r *reader) u32() uint32 {
	value := r.take(4)
	if len(value) != 4 {
		return 0
	}
	return binary.BigEndian.Uint32(value)
}
func (r *reader) u64() uint64 {
	value := r.take(8)
	if len(value) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}
func (r *reader) i64() int64 { return int64(r.u64()) }
func (r *reader) blob(max int) []byte {
	size := r.u32()
	if size > uint32(max) {
		r.err = ErrBounds
		return nil
	}
	return append([]byte(nil), r.take(int(size))...)
}
func (r *reader) digest() coordination.Digest {
	var value coordination.Digest
	copy(value[:], r.take(sha256.Size))
	return value
}
func (r *reader) timestamp() time.Time {
	seconds := r.i64()
	nanos := r.u32()
	if nanos >= uint32(time.Second) {
		r.err = errors.New("catalog: invalid timestamp")
		return time.Time{}
	}
	return time.Unix(seconds, int64(nanos)).UTC()
}

func envelope(kind byte, payload []byte) []byte {
	value := make([]byte, 0, 4+1+4+len(payload)+sha256.Size)
	value = append(value, catalogMagic[:]...)
	value = append(value, kind)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	value = append(value, size[:]...)
	value = append(value, payload...)
	digest := sha256.Sum256(value)
	return append(value, digest[:]...)
}

func openEnvelope(data []byte, kind byte, maximum int) ([]byte, error) {
	if len(data) < 4+1+4+sha256.Size || len(data) > maximum {
		return nil, errors.Join(ErrCorruption, errors.New("catalog: invalid record size"))
	}
	if !bytes.Equal(data[:4], catalogMagic[:]) || data[4] != kind {
		return nil, errors.Join(ErrCorruption, errors.New("catalog: invalid record header"))
	}
	size := binary.BigEndian.Uint32(data[5:9])
	if int(size) != len(data)-9-sha256.Size {
		return nil, errors.Join(ErrCorruption, errors.New("catalog: invalid record length"))
	}
	expected := sha256.Sum256(data[:len(data)-sha256.Size])
	if !bytes.Equal(expected[:], data[len(data)-sha256.Size:]) {
		return nil, errors.Join(ErrCorruption, errors.New("catalog: record digest mismatch"))
	}
	return data[9 : len(data)-sha256.Size], nil
}

func marshalCounter(generation coordination.Generation, updated time.Time, reservationID []byte) ([]byte, error) {
	if err := generation.Validate(); err != nil {
		return nil, err
	}
	if err := validUTC(updated); err != nil {
		return nil, err
	}
	if len(reservationID) == 0 || len(reservationID) > coordination.MaxOpaqueIDBytes {
		return nil, ErrBounds
	}
	var w writer
	w.u64(uint64(generation))
	w.timestamp(updated)
	w.blob(reservationID)
	return envelope('H', w.Bytes()), nil
}

func unmarshalCounter(data []byte) (coordination.Generation, time.Time, []byte, error) {
	payload, err := openEnvelope(data, 'H', coordination.MaxOpaqueIDBytes+256)
	if err != nil {
		return 0, time.Time{}, nil, err
	}
	r := reader{data: payload}
	generation := coordination.Generation(r.u64())
	updated := r.timestamp()
	reservationID := r.blob(coordination.MaxOpaqueIDBytes)
	if r.err != nil || r.off != len(payload) || generation.Validate() != nil || validUTC(updated) != nil {
		return 0, time.Time{}, nil, ErrCorruption
	}
	return generation, updated, reservationID, nil
}

func marshalFence(value PolicyFence) ([]byte, error) {
	if err := validateFence(value); err != nil {
		return nil, err
	}
	var w writer
	w.blob(value.Request.LPART)
	w.u64(uint64(value.Request.CopyGeneration))
	w.digest(value.Request.VisibilityDigest)
	w.blob(value.Request.Owner)
	w.blob(value.Request.OperationID)
	w.timestamp(value.Request.LeaseUntil)
	w.u64(uint64(value.Request.Fence))
	w.u64(uint64(value.Request.AuthorityGeneration))
	w.u64(uint64(value.Request.RetentionGeneration))
	w.u64(uint64(value.RecordGeneration))
	w.timestamp(value.UpdatedAt)
	if value.Active {
		w.u8(1)
	} else {
		w.u8(0)
	}
	if value.publication != nil || value.retirement != nil {
		w.u8(1)
		flags := byte(0)
		if value.publication != nil {
			flags |= 1
		}
		if value.retirement != nil {
			flags |= 2
		}
		w.u8(flags)
		if value.publication != nil {
			mapping, err := coordination.MarshalPolicyCopyMapV3(value.publication.Map)
			if err != nil {
				return nil, err
			}
			w.blob(mapping)
			w.digest(value.publication.MapDigest)
		}
		if value.retirement != nil {
			w.u64(uint64(value.retirement.Through))
			w.digest(value.retirement.PublicationDigest)
			w.digest(value.retirement.PredecessorRootDigest)
			w.digest(value.retirement.SuccessorRootDigest)
		}
	}
	encoded := envelope('F', w.Bytes())
	if len(encoded) > coordination.MaxRootBytes {
		return nil, ErrBounds
	}
	return encoded, nil
}

func unmarshalFence(data []byte) (PolicyFence, error) {
	payload, err := openEnvelope(data, 'F', coordination.MaxRootBytes)
	if err != nil {
		return PolicyFence{}, err
	}
	r := reader{data: payload}
	value := PolicyFence{Request: PolicyFenceRequest{
		LPART:               coordination.LPART(r.blob(coordination.MaxOpaqueIDBytes)),
		CopyGeneration:      coordination.Generation(r.u64()),
		VisibilityDigest:    r.digest(),
		Owner:               coordination.OwnerID(r.blob(coordination.MaxOwnerBytes)),
		OperationID:         r.blob(coordination.MaxOpaqueIDBytes),
		LeaseUntil:          r.timestamp(),
		Fence:               coordination.Fence(r.u64()),
		AuthorityGeneration: coordination.Generation(r.u64()),
		RetentionGeneration: coordination.Generation(r.u64()),
	}, RecordGeneration: coordination.Generation(r.u64()), UpdatedAt: r.timestamp()}
	active := r.u8()
	if active > 1 {
		return PolicyFence{}, ErrCorruption
	}
	value.Active = active == 1
	if r.err == nil && r.off < len(payload) {
		if version := r.u8(); version != 1 {
			return PolicyFence{}, ErrCorruption
		}
		flags := r.u8()
		if flags == 0 || flags&^byte(3) != 0 || flags&2 != 0 && flags&1 == 0 {
			return PolicyFence{}, ErrCorruption
		}
		if flags&1 != 0 {
			encoded := r.blob(coordination.MaxRootBytes)
			mapping, decodeErr := coordination.UnmarshalPolicyCopyMapV3(encoded)
			if decodeErr != nil {
				return PolicyFence{}, ErrCorruption
			}
			value.publication = &policyPublicationMarker{Map: mapping, MapDigest: r.digest()}
		}
		if flags&2 != 0 {
			value.retirement = &policyRetirementMarker{
				Through:               coordination.Epoch(r.u64()),
				PublicationDigest:     r.digest(),
				PredecessorRootDigest: r.digest(),
				SuccessorRootDigest:   r.digest(),
			}
		}
	}
	if r.err != nil || r.off != len(payload) {
		return PolicyFence{}, ErrCorruption
	}
	if err := validateFence(value); err != nil {
		return PolicyFence{}, errors.Join(ErrCorruption, err)
	}
	return value, nil
}

func marshalBuild(value IndexBuild) ([]byte, error) {
	manifest, err := coordination.MarshalIndexGenerationV2(value.Manifest)
	if err != nil {
		return nil, err
	}
	if err := value.Owner.Validate(); err != nil {
		return nil, err
	}
	if len(value.OperationID) == 0 || len(value.OperationID) > coordination.MaxOpaqueIDBytes {
		return nil, ErrBounds
	}
	if err := value.Fence.Validate(); err != nil {
		return nil, err
	}
	if err := value.AuthorityGeneration.Validate(); err != nil {
		return nil, err
	}
	if err := value.RetentionGeneration.Validate(); err != nil {
		return nil, err
	}
	if err := value.RecordGeneration.Validate(); err != nil {
		return nil, err
	}
	if err := validUTC(value.UpdatedAt); err != nil {
		return nil, err
	}
	var w writer
	w.blob(manifest)
	w.blob(value.Owner)
	w.blob(value.OperationID)
	w.u64(uint64(value.Fence))
	w.u64(uint64(value.AuthorityGeneration))
	w.u64(uint64(value.RetentionGeneration))
	w.u64(uint64(value.RecordGeneration))
	w.timestamp(value.UpdatedAt)
	return envelope('B', w.Bytes()), nil
}

func unmarshalBuild(data []byte) (IndexBuild, error) {
	payload, err := openEnvelope(data, 'B', coordination.MaxRootBytes*2)
	if err != nil {
		return IndexBuild{}, err
	}
	r := reader{data: payload}
	manifestBytes := r.blob(coordination.MaxRootBytes)
	value := IndexBuild{
		Owner: coordination.OwnerID(r.blob(coordination.MaxOwnerBytes)), OperationID: r.blob(coordination.MaxOpaqueIDBytes),
		Fence: coordination.Fence(r.u64()), AuthorityGeneration: coordination.Generation(r.u64()),
		RetentionGeneration: coordination.Generation(r.u64()), RecordGeneration: coordination.Generation(r.u64()),
		UpdatedAt: r.timestamp(),
	}
	if r.err != nil || r.off != len(payload) {
		return IndexBuild{}, ErrCorruption
	}
	value.Manifest, err = coordination.UnmarshalIndexGenerationV2(manifestBytes)
	if err != nil {
		return IndexBuild{}, errors.Join(ErrCorruption, err)
	}
	if _, err := marshalBuild(value); err != nil {
		return IndexBuild{}, errors.Join(ErrCorruption, err)
	}
	return value, nil
}

func validUTC(value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC || value.Year() < 1 || value.Year() > 9999 {
		return errors.New("catalog: timestamp must be supported UTC")
	}
	return nil
}

func digestParts(parts ...[]byte) coordination.Digest {
	h := sha256.New()
	for _, part := range parts {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(part)
	}
	var value coordination.Digest
	copy(value[:], h.Sum(nil))
	return value
}

func copyGenerationIGEN(value coordination.Generation) coordination.IGEN {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	return coordination.IGEN(encoded[:])
}

func generationFromIGEN(value coordination.IGEN) (coordination.Generation, error) {
	if len(value) != 8 {
		return 0, fmt.Errorf("%w: generation identifier has invalid size", ErrCorruption)
	}
	generation := coordination.Generation(binary.BigEndian.Uint64(value))
	if err := generation.Validate(); err != nil {
		return 0, errors.Join(ErrCorruption, err)
	}
	return generation, nil
}
