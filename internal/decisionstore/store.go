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

// Package decisionstore persists decision reservations and committed predictions
// over Shoal's atomic row-CAS store. It is an internal, trusted storage boundary:
// the service must authenticate scopes and authorize all evidence/receipt access.
package decisionstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_decision_receipts"
const MaxLease = time.Hour
const maxStoredBytes = 2*decision.MaxManifestBytes + 64*1024

var (
	ErrNotFound      = errors.New("decision receipt not found")
	ErrConflict      = errors.New("decision receipt conflict")
	ErrExpired       = errors.New("decision lease or request expired")
	ErrIndeterminate = errors.New("decision receipt mutation indeterminate")
	ErrCorrupt       = errors.New("invalid persisted decision receipt")
)

// Scope must come from authenticated service context, never a request body.
// Domain separates identical principal IDs in different authorization domains.
// Additional service identity/delegation constraints belong in that domain.
type Scope struct {
	Domain    []byte
	Principal shoal.ID
}
type State string

const (
	Pending   State = "pending"
	Committed State = "committed"
)

// Receipt retains immutable artifact identities, not raw source or prompts.
// The service must retain authorized request/evidence artifacts independently.
// Result is the original validated provider response, not a fresh recomputation.
type Receipt struct {
	ID           string
	Version      int64
	State        State
	RequestID    shoal.ID
	TaskID       shoal.ID
	PictureID    shoal.ID
	PredictorID  shoal.ID
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LeaseUntil   time.Time
	PredictionID shoal.ID
	Result       *decision.ResultConfig
}

// Claim is returned only to a newly acquired reservation owner. Reads and
// pending retries never disclose its token. Versions fence expired workers.
type Claim struct {
	Version int64
	Token   string
}
type Reservation struct {
	Receipt Receipt
	Claim   *Claim
}
type row struct {
	Receipt Receipt
	Token   string
}
type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

type CAS interface {
	ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
	CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
}
type Store struct {
	backend    CAS
	visibility []byte
	now        func() time.Time
}

func New(backend CAS, visibility []byte, now func() time.Time) (*Store, error) {
	if backend == nil || now == nil || len(visibility) > 4096 {
		return nil, invalid("invalid decision store configuration")
	}
	return &Store{backend: backend, visibility: append([]byte(nil), visibility...), now: now}, nil
}

// Reserve returns a committed replay or an observable pending receipt without
// giving a second caller the live claim. Expired pending claims may be reclaimed
// with a new version/token while the request is still within its deadline.
func (s *Store) Reserve(ctx context.Context, scope Scope, key []byte, request decision.DecisionRequest, lease time.Duration) (Reservation, error) {
	coord, err := s.coordinate(scope, key, request)
	if err != nil {
		return Reservation{}, err
	}
	if lease <= 0 || lease > MaxLease {
		return Reservation{}, invalid("lease outside bounds")
	}
	current, old, err := s.read(ctx, coord, request)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Reservation{}, err
	}
	// Read latency must not extend a deadline or revive a stale clock sample.
	now, clockErr := s.clock()
	if clockErr != nil {
		return Reservation{}, clockErr
	}
	if err == nil {
		if current.Receipt.State == Committed || now.Before(current.Receipt.LeaseUntil) {
			return Reservation{Receipt: cloneReceipt(current.Receipt)}, nil
		}
		if now.Before(current.Receipt.UpdatedAt) {
			return Reservation{}, invalid("clock moved before stored receipt")
		}
	}
	rc := request.Config()
	if now.Before(rc.RequestedAt) || !now.Before(rc.Deadline) {
		return Reservation{}, ErrExpired
	}
	version := int64(1)
	created := now
	if current != nil {
		if current.Receipt.Version == math.MaxInt64 {
			return Reservation{}, ErrConflict
		}
		version = current.Receipt.Version + 1
		created = current.Receipt.CreatedAt
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return Reservation{}, err
	}
	next := row{Receipt: Receipt{ID: string(coord.Row), Version: version, State: Pending, RequestID: request.ID(), TaskID: request.TaskID(), PictureID: request.PictureID(), PredictorID: request.PredictorID(), CreatedAt: created, UpdatedAt: now, LeaseUntil: now.Add(lease)}, Token: hex.EncodeToString(token)}
	if err := s.write(ctx, coord, old, next); err != nil {
		return Reservation{}, err
	}
	// The CAS may itself have stalled. Preserve the durable pending receipt,
	// but never return permission to start inference on an already expired claim.
	returnedAt, err := s.clock()
	if err != nil {
		return Reservation{Receipt: cloneReceipt(next.Receipt)}, err
	}
	if returnedAt.Before(now) || !returnedAt.Before(rc.Deadline) || !returnedAt.Before(next.Receipt.LeaseUntil) {
		return Reservation{Receipt: cloneReceipt(next.Receipt)}, ErrExpired
	}
	return Reservation{Receipt: cloneReceipt(next.Receipt), Claim: &Claim{Version: version, Token: next.Token}}, nil
}
func (s *Store) Get(ctx context.Context, scope Scope, key []byte, request decision.DecisionRequest) (Receipt, error) {
	coord, err := s.coordinate(scope, key, request)
	if err != nil {
		return Receipt{}, err
	}
	record, _, err := s.read(ctx, coord, request)
	if err != nil {
		return Receipt{}, err
	}
	return cloneReceipt(record.Receipt), nil
}

// Commit validates the result against the original request before mutation. A
// matching committed replay returns the original receipt, including after lease
// expiry. A different result never replaces the winner. Late timeout/failure
// results are permitted by the prediction contract while the claim remains live.
func (s *Store) Commit(ctx context.Context, scope Scope, key []byte, request decision.DecisionRequest, claim Claim, result decision.ResultConfig) (Receipt, error) {
	coord, err := s.coordinate(scope, key, request)
	if err != nil {
		return Receipt{}, err
	}
	if claim.Version < 1 || !digestValid(claim.Token) {
		return Receipt{}, invalid("invalid reservation claim")
	}
	prediction, err := decision.NewPredictionRecord(request, result)
	if err != nil {
		return Receipt{}, err
	}
	current, old, err := s.read(ctx, coord, request)
	if err != nil {
		return Receipt{}, err
	}
	if current.Receipt.State == Committed {
		if current.Token == claim.Token && current.Receipt.Version == claim.Version+1 && current.Receipt.PredictionID == prediction.ID() {
			return cloneReceipt(current.Receipt), nil
		}
		return Receipt{}, ErrConflict
	}
	if current.Token != claim.Token || current.Receipt.Version != claim.Version {
		return Receipt{}, ErrConflict
	}
	now, err := s.clock()
	if err != nil {
		return Receipt{}, err
	}
	if now.Before(current.Receipt.UpdatedAt) {
		return Receipt{}, invalid("clock moved before stored receipt")
	}
	if !now.Before(current.Receipt.LeaseUntil) {
		return Receipt{}, ErrExpired
	}
	if result.Status != decision.Failed && now.After(request.Config().Deadline) {
		return Receipt{}, ErrExpired
	}
	if result.CompletedAt.Before(current.Receipt.UpdatedAt) {
		return Receipt{}, invalid("result completion predates active claim")
	}
	if result.CompletedAt.After(now) {
		return Receipt{}, invalid("result completion is in the future")
	}
	if current.Receipt.Version == math.MaxInt64 {
		return Receipt{}, ErrConflict
	}
	next := *current
	next.Receipt.Version++
	next.Receipt.State = Committed
	next.Receipt.UpdatedAt = now
	next.Receipt.PredictionID = prediction.ID()
	normalized := prediction.Config()
	next.Receipt.Result = &normalized
	if err := s.write(ctx, coord, old, next); err != nil {
		return Receipt{}, err
	}
	return cloneReceipt(next.Receipt), nil
}
func (s *Store) coordinate(scope Scope, key []byte, request decision.DecisionRequest) (allocator.Coordinate, error) {
	if err := request.Validate(); err != nil {
		return allocator.Coordinate{}, err
	}
	if len(scope.Domain) == 0 || len(scope.Domain) > 4096 || len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return allocator.Coordinate{}, invalid("invalid receipt scope/key")
	}
	if err := shoal.ValidateRequiredID("principal", scope.Principal); err != nil {
		return allocator.Coordinate{}, err
	}
	if scope.Principal != request.Config().PrincipalID {
		return allocator.Coordinate{}, invalid("request principal does not match scope")
	}
	h := sha256.New()
	var n [8]byte
	for _, v := range [][]byte{[]byte("decision-receipt-v1"), scope.Domain, []byte(scope.Principal), key} {
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write(v)
	}
	id := "receipt:" + hex.EncodeToString(h.Sum(nil))
	return allocator.Coordinate{Row: []byte(id), Family: []byte("r"), Qualifier: []byte("receipt"), Visibility: append([]byte(nil), s.visibility...)}, nil
}
func (s *Store) clock() (time.Time, error) {
	t := s.now().Round(0).UTC()
	if t.IsZero() || t.Year() < 1 || t.Year() > 9999 {
		return time.Time{}, invalid("invalid store clock")
	}
	return t, nil
}
func (s *Store) read(ctx context.Context, coord allocator.Coordinate, request decision.DecisionRequest) (*row, *allocator.Cell, error) {
	cells, err := s.backend.ReadExact(ctx, []allocator.Coordinate{coord})
	if err != nil {
		return nil, nil, err
	}
	if len(cells) == 0 {
		return nil, nil, ErrNotFound
	}
	if len(cells) != 1 || !sameCoordinate(cells[0].Coordinate, coord) {
		return nil, nil, ErrCorrupt
	}
	record, err := decode(cells[0].Value)
	if err != nil {
		return nil, nil, err
	}
	r := record.Receipt
	if r.ID != string(coord.Row) || r.Version != cells[0].Timestamp {
		return nil, nil, ErrCorrupt
	}
	// Check immutable request binding before returning any persisted payload.
	if r.RequestID != request.ID() {
		return nil, nil, ErrConflict
	}
	if r.TaskID != request.TaskID() || r.PictureID != request.PictureID() || r.PredictorID != request.PredictorID() {
		return nil, nil, ErrCorrupt
	}
	if r.CreatedAt.Before(request.Config().RequestedAt) {
		return nil, nil, ErrCorrupt
	}
	if r.Result != nil {
		prediction, err := decision.NewPredictionRecord(request, *r.Result)
		if err != nil || prediction.ID() != r.PredictionID || r.Result.CompletedAt.After(r.UpdatedAt) {
			return nil, nil, ErrCorrupt
		}
	}
	return &record, &cells[0], nil
}
func (s *Store) write(ctx context.Context, coord allocator.Coordinate, old *allocator.Cell, next row) error {
	value, err := encode(next)
	if err != nil {
		return err
	}
	condition := allocator.Condition{Coordinate: coord, Absent: old == nil}
	if old != nil {
		condition.Value = old.Value
		condition.Timestamp = old.Timestamp
		condition.TimestampSet = true
	}
	status, writeErr := s.backend.CompareAndMutate(ctx, allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{condition}, Updates: []allocator.Update{{Coordinate: coord, Value: value, Timestamp: next.Receipt.Version}}})
	if writeErr == nil && status == allocator.StatusAccepted {
		return nil
	}
	// A lost acknowledgement can hide a successful write. Never report rollback:
	// reconcile exact stored bytes or expose indeterminate state to the caller.
	cells, readErr := s.backend.ReadExact(ctx, []allocator.Coordinate{coord})
	if readErr == nil && len(cells) == 1 && sameCoordinate(cells[0].Coordinate, coord) && cells[0].Timestamp == next.Receipt.Version && bytes.Equal(cells[0].Value, value) {
		return nil
	}
	if writeErr == nil && status == allocator.StatusRejected {
		return ErrConflict
	}
	return errors.Join(ErrIndeterminate, writeErr, readErr)
}
func encode(r row) ([]byte, error) {
	if err := validateRow(r); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	b, err := json.Marshal(envelope{Schema: 1, Payload: payload, Checksum: hex.EncodeToString(sum[:])})
	if err != nil {
		return nil, err
	}
	if len(b) > maxStoredBytes {
		return nil, invalid("receipt exceeds storage bound")
	}
	return b, nil
}
func decode(b []byte) (row, error) {
	if len(b) > maxStoredBytes {
		return row{}, ErrCorrupt
	}
	var e envelope
	if err := strictJSON(b, &e); err != nil || e.Schema != 1 {
		return row{}, ErrCorrupt
	}
	sum := sha256.Sum256(e.Payload)
	if hex.EncodeToString(sum[:]) != e.Checksum {
		return row{}, ErrCorrupt
	}
	var r row
	if err := strictJSON(e.Payload, &r); err != nil {
		return row{}, ErrCorrupt
	}
	if err := validateRow(r); err != nil {
		return row{}, ErrCorrupt
	}
	canonical, err := encode(r)
	if err != nil || !bytes.Equal(canonical, b) {
		return row{}, ErrCorrupt
	}
	return r, nil
}
func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ErrCorrupt
	}
	return nil
}
func validateRow(r row) error {
	v := r.Receipt
	if v.Version < 1 || len(v.ID) != len("receipt:")+64 || v.ID[:len("receipt:")] != "receipt:" || !digestValid(v.ID[len("receipt:"):]) || !digestValid(r.Token) {
		return ErrCorrupt
	}
	for _, id := range []shoal.ID{v.RequestID, v.TaskID, v.PictureID, v.PredictorID} {
		if err := shoal.ValidateRequiredID("stored reference", id); err != nil {
			return ErrCorrupt
		}
	}
	for _, t := range []time.Time{v.CreatedAt, v.UpdatedAt, v.LeaseUntil} {
		if t.IsZero() || t.Year() < 1 || t.Year() > 9999 || t.Location() != time.UTC {
			return ErrCorrupt
		}
	}
	if v.UpdatedAt.Before(v.CreatedAt) || !v.LeaseUntil.After(v.CreatedAt) {
		return ErrCorrupt
	}
	switch v.State {
	case Pending:
		if v.Result != nil || v.PredictionID != "" || !v.LeaseUntil.After(v.UpdatedAt) {
			return ErrCorrupt
		}
	case Committed:
		if v.Result == nil || v.PredictionID == "" || v.Version < 2 || !v.UpdatedAt.Before(v.LeaseUntil) {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}
func cloneReceipt(r Receipt) Receipt {
	if r.Result != nil {
		c := *r.Result
		c.Answers = append([]decision.Answer(nil), c.Answers...)
		for i := range c.Answers {
			c.Answers[i].Distribution = append([]decision.LabelProbability(nil), c.Answers[i].Distribution...)
			if c.Answers[i].Probability != nil {
				p := *c.Answers[i].Probability
				c.Answers[i].Probability = &p
			}
		}
		r.Result = &c
	}
	return r
}
func sameCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func digestValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(b) == s
}
func invalid(s string) error { return shoal.NewError(shoal.ErrorInvalidArgument, s) }
