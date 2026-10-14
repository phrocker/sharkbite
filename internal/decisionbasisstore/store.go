// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionbasisstore retains immutable adjudication admission snapshots.
// Scope must come from trusted identity. The calling service MUST authorize all
// referenced inputs before retention or disclosure. Persistence grants no role,
// verifies no assertion, and confers no training eligibility. Retain a basis
// before appending its journal receipt; an unreferenced retained basis is safe.
package decisionbasisstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_adjudication_bases"
const MaxStoredBytes = 8 << 20

var (
	ErrNotFound      = errors.New("adjudication basis not found")
	ErrConflict      = errors.New("immutable adjudication basis conflict")
	ErrCorrupt       = errors.New("invalid retained adjudication basis")
	ErrUnavailable   = errors.New("adjudication basis unavailable")
	ErrIndeterminate = errors.New("adjudication basis retention indeterminate")
)

type Scope struct{ Domain []byte }
type Config struct {
	Backend    decisionstore.CAS
	Visibility []byte
}
type Store struct{ config Config }

func New(c Config) (*Store, error) {
	if absent(c.Backend) || len(c.Visibility) > 4096 {
		return nil, invalid()
	}
	c.Visibility = append([]byte(nil), c.Visibility...)
	return &Store{c}, nil
}
func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid adjudication basis request")
}
func hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func validBasisID(id shoal.ID) bool {
	const prefix = "decision:adjudication-basis:v1:"
	s := string(id)
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	digest := strings.TrimPrefix(s, prefix)
	b, e := hex.DecodeString(digest)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == digest
}
func (s *Store) coordinate(scope Scope, id shoal.ID) (allocator.Coordinate, error) {
	if len(scope.Domain) == 0 || len(scope.Domain) > auth.MaxPolicyComponentBytes || !validBasisID(id) {
		return allocator.Coordinate{}, invalid()
	}
	payload, _ := json.Marshal(struct {
		Version string
		Domain  []byte
		BasisID shoal.ID
	}{"adjudication-bases-v1", scope.Domain, id})
	return allocator.Coordinate{Row: []byte("basis:" + hash(payload)), Family: []byte("b"), Qualifier: []byte("snapshot"), Visibility: append([]byte(nil), s.config.Visibility...)}, nil
}
func equalCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func (s *Store) read(ctx context.Context, c allocator.Coordinate) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	cells, e := s.config.Backend.ReadExact(ctx, []allocator.Coordinate{c})
	if e != nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if len(cells) == 0 {
		return nil, ErrNotFound
	}
	if len(cells) != 1 || cells[0].Timestamp != 1 || !equalCoordinate(cells[0].Coordinate, c) || len(cells[0].Value) > MaxStoredBytes {
		return nil, ErrCorrupt
	}
	return append([]byte(nil), cells[0].Value...), nil
}

// Retain uses immutable absent-CAS and exact readback. An unknown acknowledgement
// is reconciled only by confirming the exact originally supplied bytes.
func (s *Store) Retain(ctx context.Context, scope Scope, basis decision.AdjudicationBasis) error {
	if basis.Validate() != nil {
		return invalid()
	}
	coordinate, e := s.coordinate(scope, basis.ID())
	if e != nil {
		return e
	}
	encoded, e := encode(basis)
	if e != nil {
		return e
	}
	existing, readErr := s.read(ctx, coordinate)
	if readErr == nil {
		if bytes.Equal(existing, encoded) {
			return nil
		}
		return ErrConflict
	}
	if !errors.Is(readErr, ErrNotFound) {
		return readErr
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	status, writeErr := s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}}, Updates: []allocator.Update{{Coordinate: coordinate, Timestamp: 1, Value: encoded}}})
	stored, readErr := s.read(ctx, coordinate)
	if readErr == nil && bytes.Equal(stored, encoded) {
		return nil
	}
	if status == allocator.StatusRejected && writeErr == nil && readErr == nil {
		return ErrConflict
	}
	if readErr == nil {
		readErr = ErrConflict
	}
	return errors.Join(ErrIndeterminate, readErr)
}

// Load needs the independently resolved original proposal to reconstruct the
// immutable basis identity. It must not accept a caller-supplied substitute.
func (s *Store) Load(ctx context.Context, scope Scope, basisID shoal.ID, proposal decision.AdjudicationProposal) (decision.AdjudicationBasis, error) {
	if proposal.Validate() != nil {
		return decision.AdjudicationBasis{}, invalid()
	}
	coordinate, e := s.coordinate(scope, basisID)
	if e != nil {
		return decision.AdjudicationBasis{}, e
	}
	raw, e := s.read(ctx, coordinate)
	if e != nil {
		return decision.AdjudicationBasis{}, e
	}
	basis, e := decode(raw, basisID, proposal)
	if e != nil {
		return decision.AdjudicationBasis{}, e
	}
	if ctx.Err() != nil {
		return decision.AdjudicationBasis{}, ErrUnavailable
	}
	return basis, nil
}
