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

package guard

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

const (
	codecMagic     = "SHOALG\x00"
	codecVersion   = uint16(1)
	recordHead     = byte(1)
	recordPending  = byte(2)
	maxRecordBytes = coordination.MaxRootBytes
)

type writer struct {
	bytes.Buffer
	err error
}

func (w *writer) u8(v byte)    { w.WriteByte(v) }
func (w *writer) u32(v uint32) { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); w.Write(b[:]) }
func (w *writer) u64(v uint64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], v); w.Write(b[:]) }
func (w *writer) blob(v []byte) {
	if len(v) > coordination.MaxOpaqueIDBytes {
		w.err = ErrBounds
		return
	}
	w.u32(uint32(len(v)))
	w.Write(v)
}
func (w *writer) digest(v coordination.Digest) { w.Write(v[:]) }
func (w *writer) instant(v time.Time) {
	w.u64(uint64(v.Unix()))
	w.u32(uint32(v.Nanosecond()))
}

type reader struct {
	data []byte
	off  int
	err  error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.data)-r.off {
		r.err = errors.New("entity guard: record is truncated")
		return nil
	}
	v := r.data[r.off : r.off+n]
	r.off += n
	return v
}
func (r *reader) u8() byte {
	v := r.take(1)
	if v == nil {
		return 0
	}
	return v[0]
}
func (r *reader) u32() uint32 {
	v := r.take(4)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}
func (r *reader) u64() uint64 {
	v := r.take(8)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}
func (r *reader) blob() []byte {
	n := r.u32()
	if n > coordination.MaxOpaqueIDBytes {
		r.err = ErrBounds
		return nil
	}
	return append([]byte(nil), r.take(int(n))...)
}
func (r *reader) digest() coordination.Digest {
	var d coordination.Digest
	copy(d[:], r.take(len(d)))
	return d
}
func (r *reader) instant() time.Time {
	seconds, nanos := int64(r.u64()), int64(r.u32())
	if nanos < 0 || nanos >= int64(time.Second) {
		r.err = errors.New("entity guard: timestamp is invalid")
		return time.Time{}
	}
	return time.Unix(seconds, nanos).UTC()
}

func envelope(kind byte, encode func(*writer)) ([]byte, error) {
	w := &writer{}
	w.WriteString(codecMagic)
	var version [2]byte
	binary.BigEndian.PutUint16(version[:], codecVersion)
	w.Write(version[:])
	w.u8(kind)
	encode(w)
	if w.err != nil {
		return nil, w.err
	}
	if w.Len()+sha256.Size > maxRecordBytes {
		return nil, ErrBounds
	}
	sum := sha256.Sum256(w.Bytes())
	w.Write(sum[:])
	return w.Bytes(), nil
}

func open(data []byte, kind byte) (*reader, error) {
	if len(data) < len(codecMagic)+3+sha256.Size || len(data) > maxRecordBytes {
		return nil, errors.New("entity guard: invalid record length")
	}
	body := data[:len(data)-sha256.Size]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], data[len(data)-sha256.Size:]) {
		return nil, errors.New("entity guard: record checksum mismatch")
	}
	if !bytes.Equal(body[:len(codecMagic)], []byte(codecMagic)) ||
		binary.BigEndian.Uint16(body[len(codecMagic):]) != codecVersion ||
		body[len(codecMagic)+2] != kind {
		return nil, errors.New("entity guard: record header is invalid")
	}
	return &reader{data: body[len(codecMagic)+3:]}, nil
}

func encodeHead(w *writer, h Head) {
	w.u64(uint64(h.Generation))
	w.instant(h.UpdatedAt)
	w.u8(byte(h.State))
	w.blob(h.WinnerID)
	w.u64(uint64(h.Epoch))
	w.blob(h.TXN)
	w.digest(h.LogicalDigest)
	w.blob(h.LPART)
	w.blob(h.LogicalPolicyID)
	w.u64(uint64(h.RetirementGeneration))
}

func MarshalHead(h Head) ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return envelope(recordHead, func(w *writer) { encodeHead(w, h) })
}

func MarshalEntityHeadV1(h EntityHeadV1) ([]byte, error) { return MarshalHead(h) }

func UnmarshalHead(data []byte) (Head, error) {
	r, err := open(data, recordHead)
	if err != nil {
		return Head{}, err
	}
	h := Head{
		Generation: coordination.Generation(r.u64()), UpdatedAt: r.instant(),
		State: EntityState(r.u8()), WinnerID: r.blob(), Epoch: coordination.Epoch(r.u64()),
		TXN: coordination.TXN(r.blob()), LogicalDigest: r.digest(),
		LPART: coordination.LPART(r.blob()), LogicalPolicyID: r.blob(),
		RetirementGeneration: coordination.Generation(r.u64()),
	}
	if r.err != nil || r.off != len(r.data) {
		return Head{}, errors.New("entity guard: malformed head record")
	}
	if err := h.Validate(); err != nil {
		return Head{}, err
	}
	canonical, _ := MarshalHead(h)
	if !bytes.Equal(canonical, data) {
		return Head{}, errors.New("entity guard: noncanonical head record")
	}
	return h, nil
}

func UnmarshalEntityHeadV1(data []byte) (EntityHeadV1, error) { return UnmarshalHead(data) }

func encodeIntent(w *writer, i Intent) {
	w.u8(i.Entity.Kind)
	w.blob(i.Entity.ID)
	w.blob(i.TXN)
	w.blob(i.Owner)
	w.instant(i.LeaseUntil)
	w.u64(uint64(i.Fence))
	w.u64(uint64(i.AuthorityGeneration))
	w.u64(uint64(i.AuthorityFence))
	w.u64(uint64(i.RetentionGeneration))
	w.u64(uint64(i.RetirementGeneration))
	w.u64(uint64(i.HistoryFloor))
	w.u8(byte(i.Mode))
	w.u64(uint64(i.ExpectedEpoch))
	w.digest(i.ExpectedDigest)
	w.u8(byte(i.DesiredState))
	w.blob(i.DesiredWinnerID)
	w.digest(i.DesiredDigest)
	w.blob(i.LPART)
	w.blob(i.LogicalPolicyID)
	w.u32(i.ManifestChunk)
	w.u32(i.ManifestEntry)
	w.u32(i.Ordinal)
	w.digest(i.PhysicalDigest)
}

func decodeIntent(r *reader) Intent {
	return Intent{
		Entity: Entity{Kind: r.u8(), ID: coordination.EntityID(r.blob())},
		TXN:    coordination.TXN(r.blob()), Owner: coordination.OwnerID(r.blob()),
		LeaseUntil: r.instant(), Fence: coordination.Fence(r.u64()),
		AuthorityGeneration:  coordination.Generation(r.u64()),
		AuthorityFence:       coordination.Fence(r.u64()),
		RetentionGeneration:  coordination.Generation(r.u64()),
		RetirementGeneration: coordination.Generation(r.u64()),
		HistoryFloor:         coordination.Epoch(r.u64()),
		Mode:                 Mode(r.u8()), ExpectedEpoch: coordination.Epoch(r.u64()), ExpectedDigest: r.digest(),
		DesiredState: EntityState(r.u8()), DesiredWinnerID: r.blob(), DesiredDigest: r.digest(),
		LPART: coordination.LPART(r.blob()), LogicalPolicyID: r.blob(),
		ManifestChunk: r.u32(), ManifestEntry: r.u32(), Ordinal: r.u32(), PhysicalDigest: r.digest(),
	}
}

func MarshalPending(p Pending) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return envelope(recordPending, func(w *writer) {
		w.u64(uint64(p.Generation))
		w.instant(p.UpdatedAt)
		if p.Active {
			w.u8(1)
		} else {
			w.u8(0)
		}
		if p.Prepared {
			w.u8(1)
		} else {
			w.u8(0)
		}
		w.u8(byte(p.Decision))
		if p.Active {
			encodeIntent(w, p.Intent)
		}
	})
}

func MarshalPendingGuardV1(p PendingGuardV1) ([]byte, error) { return MarshalPending(p) }

func UnmarshalPending(data []byte) (Pending, error) {
	r, err := open(data, recordPending)
	if err != nil {
		return Pending{}, err
	}

	p := Pending{Generation: coordination.Generation(r.u64()), UpdatedAt: r.instant()}
	active, prepared := r.u8(), r.u8()
	if active > 1 || prepared > 1 {
		return Pending{}, errors.New("entity guard: invalid pending flags")
	}
	p.Active, p.Prepared, p.Decision = active == 1, prepared == 1, Decision(r.u8())
	if p.Active {
		p.Intent = decodeIntent(r)
	}
	if r.err != nil || r.off != len(r.data) {
		return Pending{}, errors.New("entity guard: malformed pending record")
	}
	if err := p.Validate(); err != nil {
		return Pending{}, err
	}
	canonical, _ := MarshalPending(p)
	if !bytes.Equal(canonical, data) {
		return Pending{}, errors.New("entity guard: noncanonical pending record")
	}
	return p, nil
}

func UnmarshalPendingGuardV1(data []byte) (PendingGuardV1, error) {
	return UnmarshalPending(data)
}
