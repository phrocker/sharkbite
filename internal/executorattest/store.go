// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package executorattest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Table holds every executor attestation row.
const Table = "_shoal_executor_attestations"

const (
	rowTag         = "shoal-executor-attestation-row-v1"
	maxStoredBytes = 64 << 10
	maxCASAttempts = 8
)

var (
	ErrUnavailable   = shoal.NewError(shoal.ErrorUnavailable, "executor attestation store unavailable")
	ErrConflict      = shoal.NewError(shoal.ErrorConflict, "executor attestation conflict")
	ErrIndeterminate = shoal.NewError(shoal.ErrorUnavailable, "executor attestation write indeterminate")
)

// Record is the latest verified result for one (principal, executor ref).
type Record struct {
	AttestationID    shoal.ID
	VerifierID       shoal.ID
	KeyDigest        string
	ImageDigest      string
	ProvenanceDigest string `json:",omitempty"`
	IssuedAt         time.Time
	ExpiresAt        time.Time
}

type storedRow struct {
	Domain      []byte
	Subject     shoal.ID
	ClientID    shoal.ID
	ExecutorRef string
	Record      Record
}

type rowEnvelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

// StoreConfig configures a Store. Trust returns the current trust
// configuration; it is consulted on every Present and Current, so removing
// trust takes effect for the next check.
type StoreConfig struct {
	Backend    decisionstore.CAS
	Trust      func() *Trust
	Visibility []byte
}

// Store records the latest verified attestation per (domain, subject, client,
// executor ref).
type Store struct{ config StoreConfig }

func NewStore(c StoreConfig) (*Store, error) {
	if c.Backend == nil || c.Trust == nil || len(c.Visibility) > 4096 {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "invalid executor attestation store")
	}
	c.Visibility = bytes.Clone(c.Visibility)
	return &Store{c}, nil
}

// Present verifies evidence against want under the current trust and records
// it as the latest result for want's principal and ref. Re-presenting the
// recorded statement is idempotent. A statement whose IssuedAt is not strictly
// after the recorded one is refused (ReasonRollback).
func (s *Store) Present(ctx context.Context, want Expectation, evidence []byte) (Record, error) {
	result, e := Verify(s.config.Trust(), want, evidence)
	if e != nil {
		return Record{}, e
	}
	next := storedRow{
		Domain: bytes.Clone(want.Principal.Domain), Subject: want.Principal.Subject, ClientID: want.Principal.ClientID, ExecutorRef: want.ExecutorRef,
		Record: Record{AttestationID: result.AttestationID, VerifierID: result.VerifierID, KeyDigest: result.KeyDigest, ImageDigest: result.ImageDigest, ProvenanceDigest: result.ProvenanceDigest, IssuedAt: result.IssuedAt, ExpiresAt: result.ExpiresAt},
	}
	value, e := encode(next)
	if e != nil {
		return Record{}, shoal.NewError(shoal.ErrorInvalidArgument, "executor attestation record exceeds bound")
	}
	coord := s.coordinate(want.Principal, want.ExecutorRef)
	for range maxCASAttempts {
		current, cell, found, e := s.read(ctx, coord, want.Principal, want.ExecutorRef)
		if e != nil {
			return Record{}, e
		}
		condition := allocator.Condition{Coordinate: coord, Absent: !found}
		timestamp := int64(1)
		if found {
			if current.Record.AttestationID == next.Record.AttestationID {
				return current.Record, nil
			}
			if !next.Record.IssuedAt.After(current.Record.IssuedAt) {
				return Record{}, refuse(ReasonRollback)
			}
			condition.Value, condition.Timestamp, condition.TimestampSet = cell.Value, cell.Timestamp, true
			timestamp = cell.Timestamp + 1
		}
		status, writeErr := s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{condition}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: timestamp, Value: value}}})
		if writeErr == nil && status == allocator.StatusAccepted {
			return next.Record, nil
		}
		// A lost acknowledgement can hide a successful write; reconcile by reading.
		stored, readCell, ok, readErr := s.read(ctx, coord, want.Principal, want.ExecutorRef)
		if readErr == nil && ok && readCell.Timestamp == timestamp && stored.Record.AttestationID == next.Record.AttestationID {
			return next.Record, nil
		}
		if writeErr != nil || status != allocator.StatusRejected {
			return Record{}, errors.Join(ErrIndeterminate, writeErr, readErr)
		}
	}
	return Record{}, ErrConflict
}

// Current reports whether principal holds a current verified attestation for
// ref that covers needUntil. ok is true only if a verified row exists, its
// verifier (ID and key), image digest and, when configured, provenance digest
// are still pinned in the current trust, its validity fits the verifier's
// current max_validity, now is before its expiry and its expiry is at or
// after needUntil. A store failure is returned as err; callers map it to
// unavailable and must not treat it as "not attested".
func (s *Store) Current(ctx context.Context, principal Principal, ref string, now, needUntil time.Time) (shoal.ID, time.Time, bool, error) {
	if principal.validate() != nil || !validRef(ref) || now.IsZero() || needUntil.IsZero() {
		return "", time.Time{}, false, shoal.NewError(shoal.ErrorInvalidArgument, "invalid attestation check")
	}
	row, _, found, e := s.read(ctx, s.coordinate(principal, ref), principal, ref)
	if e != nil {
		return "", time.Time{}, false, e
	}
	if !found {
		return "", time.Time{}, false, nil
	}
	r := row.Record
	executor, ok := s.config.Trust().executor(ref)
	if !ok {
		return "", time.Time{}, false, nil
	}
	verifier, ok := executor.verifier(r.VerifierID)
	if !ok || keyDigest(verifier.PublicKey) != r.KeyDigest || pinned(executor, r.ImageDigest, r.ProvenanceDigest) != "" || r.ExpiresAt.Sub(r.IssuedAt) > verifier.MaxValidity {
		return "", time.Time{}, false, nil
	}
	if !now.UTC().Before(r.ExpiresAt) || r.ExpiresAt.Before(needUntil.UTC()) {
		return "", time.Time{}, false, nil
	}
	return r.AttestationID, r.ExpiresAt, true, nil
}

func (s *Store) coordinate(p Principal, ref string) allocator.Coordinate {
	h := sha256.New()
	var n [8]byte
	for _, v := range [][]byte{[]byte(rowTag), p.Domain, []byte(p.Subject), []byte(p.ClientID), []byte(ref)} {
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write(v)
	}
	return allocator.Coordinate{Row: []byte("executor-attestation:" + hex.EncodeToString(h.Sum(nil))), Family: []byte("a"), Qualifier: []byte("latest"), Visibility: bytes.Clone(s.config.Visibility)}
}

// read returns the row at coord, whether it exists, or ErrUnavailable for a
// store failure or a row that is not this principal's canonical record.
func (s *Store) read(ctx context.Context, coord allocator.Coordinate, p Principal, ref string) (storedRow, allocator.Cell, bool, error) {
	cells, e := s.config.Backend.ReadExact(ctx, []allocator.Coordinate{coord})
	if e != nil {
		return storedRow{}, allocator.Cell{}, false, errors.Join(ErrUnavailable, e)
	}
	if len(cells) == 0 {
		return storedRow{}, allocator.Cell{}, false, nil
	}
	c := cells[0]
	if len(cells) != 1 || !bytes.Equal(c.Coordinate.Row, coord.Row) || !bytes.Equal(c.Coordinate.Family, coord.Family) || !bytes.Equal(c.Coordinate.Qualifier, coord.Qualifier) || !bytes.Equal(c.Coordinate.Visibility, coord.Visibility) || c.Timestamp <= 0 {
		return storedRow{}, allocator.Cell{}, false, ErrUnavailable
	}
	var row storedRow
	if decode(c.Value, &row) != nil || !bytes.Equal(row.Domain, p.Domain) || row.Subject != p.Subject || row.ClientID != p.ClientID || row.ExecutorRef != ref {
		return storedRow{}, allocator.Cell{}, false, ErrUnavailable
	}
	r := row.Record
	if r.AttestationID == "" || r.VerifierID == "" || r.KeyDigest == "" || !ValidDigest(r.ImageDigest) || !r.ExpiresAt.After(r.IssuedAt) {
		return storedRow{}, allocator.Cell{}, false, ErrUnavailable
	}
	return row, c, true, nil
}

func encode(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(b)
	out, e := json.Marshal(rowEnvelope{1, b, hex.EncodeToString(sum[:])})
	if e != nil || len(out) > maxStoredBytes {
		return nil, errors.New("executor attestation record exceeds bound")
	}
	return out, nil
}

// decode accepts only the canonical encoding encode would produce.
func decode(raw []byte, out *storedRow) error {
	var env rowEnvelope
	if len(raw) > maxStoredBytes || strictDecode(raw, &env) != nil || env.Schema != 1 {
		return ErrUnavailable
	}
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.Checksum || strictDecode(env.Payload, out) != nil {
		return ErrUnavailable
	}
	if canonical, e := encode(*out); e != nil || !bytes.Equal(canonical, raw) {
		return ErrUnavailable
	}
	return nil
}
