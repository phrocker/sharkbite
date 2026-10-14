// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionoutcomes retains proposed observations, never verified labels.
package decisionoutcomes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_decision_outcomes"
const MaxOutcomeAncestors = decision.MaxOutcomeAncestors

var (
	ErrConflict      = shoal.NewError(shoal.ErrorConflict, "outcome receipt conflict")
	ErrUnavailable   = shoal.NewError(shoal.ErrorUnavailable, "outcomes unavailable")
	ErrIndeterminate = errors.New("outcome append indeterminate")
)

// Resolve must reauthorize the original prediction, ALL contributing sources,
// registered label policy and reporting (Ingest) or reading (Read) rights.
// Verify receives the complete ordered chain (current, parent, oldest) and must
// CURRENTLY authorize all original sources, asserted evidence/provenance and
// joint disclosure as one authoritative operation after ancestry collection. Neither method may turn an assertion into verified training data.
type Authority interface {
	Resolve(context.Context, auth.Decision, shoal.ID, shoal.ID, auth.Operation) (decision.PredictionRecord, error)
	Verify(context.Context, auth.Decision, []decision.OutcomeObservation) error
}
type Config struct {
	Backend    decisionstore.CAS
	Resolver   auth.Resolver
	Authority  Authority
	Admission  Admission
	Clock      func() time.Time
	Visibility []byte
}
type Store struct{ config Config }
type Receipt struct {
	ID                       shoal.ID
	ObservationID            shoal.ID
	ObservationConfig        decision.OutcomeObservationConfig
	SubmitterID              shoal.ID
	ActorID                  shoal.ID
	ClientID                 shoal.ID
	OnBehalfOf               []shoal.ID
	AuthorizationFingerprint string
	ReceivedAt               time.Time
	State                    string
}
type row struct {
	Receipt     Receipt
	ScopeDigest string
	KeyDigest   string
}

func New(c Config) (*Store, error) {
	if (c.Admission != nil && absent(c.Admission)) || absent(c.Backend) || absent(c.Resolver) || absent(c.Authority) || c.Clock == nil || len(c.Visibility) > 4096 {
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
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return r.IsNil()
	}
	return false
}
func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid outcome observation")
}
func hash(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func scopeFor(d auth.Decision) string {
	// Match decisionservice's stable principal boundary, including delegation.
	payload := struct {
		Domain                 []byte
		Subject, Actor, Client []byte
		Delegates              [][]byte
	}{Domain: d.AuthorizationDomain(), Subject: []byte(d.Subject()), Actor: []byte(d.Actor()), Client: []byte(d.ClientID())}
	for _, id := range d.OnBehalfOf() {
		payload.Delegates = append(payload.Delegates, []byte(id))
	}
	b, _ := json.Marshal(payload)
	return hash(b)
}
func receiptID(scope string, request shoal.ID, keyDigest string) shoal.ID {
	b, _ := json.Marshal([]string{"outcome-receipt-v1", scope, string(request), keyDigest})
	return shoal.ID("outcome-receipt:" + hash(b))
}
func (s *Store) coordinate(id shoal.ID) allocator.Coordinate {
	return allocator.Coordinate{Row: []byte(id), Family: []byte("o"), Qualifier: []byte("observation"), Visibility: append([]byte(nil), s.config.Visibility...)}
}
func sameCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func sanitized(e error) error {
	if e == nil {
		return nil
	}
	if shoal.IsErrorCode(e, shoal.ErrorNotFound) || shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
		return auth.ObjectNotFound()
	}
	return ErrUnavailable
}
func (s *Store) now() (time.Time, error) {
	now := s.config.Clock().Round(0).UTC()
	if now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
		return time.Time{}, ErrUnavailable
	}
	return now, nil
}
func (s *Store) current(d auth.Decision) error {
	now, e := s.now()
	if e != nil {
		return e
	}
	if !now.Before(d.AuthenticationExpires()) {
		return auth.ObjectNotFound()
	}
	return nil
}
func (s *Store) resolveCaller(ctx context.Context, before *auth.Decision) (auth.Decision, error) {
	if ctx.Err() != nil {
		return auth.Decision{}, ErrUnavailable
	}
	d, e := s.config.Resolver.Resolve(ctx)
	if e != nil {
		return d, sanitized(e)
	}
	if e = s.current(d); e != nil {
		return d, e
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return d, auth.ObjectNotFound()
	}
	if before != nil {
		original, e := auth.AuthorizationFingerprint(*before)
		if e != nil || fp != original {
			return d, auth.ObjectNotFound()
		}
	}
	if ctx.Err() != nil {
		return d, ErrUnavailable
	}
	return d, nil
}
func (s *Store) resolve(ctx context.Context, before *auth.Decision, request, prediction shoal.ID, op auth.Operation) (auth.Decision, decision.PredictionRecord, error) {
	var zero decision.PredictionRecord
	if ctx.Err() != nil {
		return auth.Decision{}, zero, ErrUnavailable
	}
	d, e := s.config.Resolver.Resolve(ctx)
	if e != nil {
		return d, zero, sanitized(e)
	}
	if e = s.current(d); e != nil {
		return d, zero, e
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return d, zero, auth.ObjectNotFound()
	}
	if before != nil {
		original, e := auth.AuthorizationFingerprint(*before)
		if e != nil || fp != original {
			return d, zero, auth.ObjectNotFound()
		}
	}
	p, e := s.config.Authority.Resolve(ctx, d, request, prediction, op)
	if e != nil {
		return d, zero, sanitized(e)
	}
	if p.Validate() != nil || p.ID() != prediction || p.Request().ID() != request {
		return d, zero, ErrUnavailable
	}
	if ctx.Err() != nil {
		return d, zero, ErrUnavailable
	}
	if e = s.current(d); e != nil {
		return d, zero, e
	}
	return d, p, nil
}
func (s *Store) read(ctx context.Context, id shoal.ID) ([]byte, error) {
	coordinate := s.coordinate(id)
	cells, e := s.config.Backend.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if e != nil {
		return nil, ErrUnavailable
	}
	if len(cells) == 0 {
		return nil, auth.ObjectNotFound()
	}
	if len(cells) != 1 || cells[0].Timestamp != 1 || !sameCoordinate(cells[0].Coordinate, coordinate) || len(cells[0].Value) > maxStoredBytes {
		return nil, ErrUnavailable
	}
	return append([]byte(nil), cells[0].Value...), nil
}
func (s *Store) validateRow(raw []byte, id shoal.ID, d auth.Decision, p decision.PredictionRecord) (row, decision.OutcomeObservation, error) {
	r, e := decode(raw)
	if e != nil {
		return row{}, decision.OutcomeObservation{}, ErrUnavailable
	}
	c := r.Receipt.ObservationConfig
	now, e := s.now()
	if e != nil {
		return row{}, decision.OutcomeObservation{}, e
	}
	if r.ScopeDigest != scopeFor(d) || r.Receipt.SubmitterID != d.Subject() || r.Receipt.ActorID != d.Actor() || r.Receipt.ClientID != d.ClientID() || !reflect.DeepEqual(r.Receipt.OnBehalfOf, d.OnBehalfOf()) {
		return row{}, decision.OutcomeObservation{}, auth.ObjectNotFound()
	}
	// A valid row belonging to another prediction/request is outside this
	// authorized lookup. Do not expose its existence through a validation error.
	if c.RequestID != p.Request().ID() || c.PredictionID != p.ID() {
		return row{}, decision.OutcomeObservation{}, auth.ObjectNotFound()
	}
	if !validHash(r.KeyDigest) || r.Receipt.ID != id || receiptID(r.ScopeDigest, c.RequestID, r.KeyDigest) != id || r.Receipt.State != "proposed" || !strings.HasPrefix(r.Receipt.AuthorizationFingerprint, "auth-sha256:") || !validHash(strings.TrimPrefix(r.Receipt.AuthorizationFingerprint, "auth-sha256:")) || r.Receipt.ReceivedAt.IsZero() || r.Receipt.ReceivedAt.After(now) || r.Receipt.ReceivedAt.Year() < 1 || r.Receipt.ReceivedAt.Year() > 9999 || r.Receipt.ReceivedAt != r.Receipt.ReceivedAt.Round(0).UTC() || r.Receipt.ReceivedAt.Before(p.Config().CompletedAt) || c.ObservedAt.After(r.Receipt.ReceivedAt) {
		return row{}, decision.OutcomeObservation{}, ErrUnavailable
	}
	o, e := decision.NewOutcomeObservation(p, c)
	if e != nil || o.ID() != r.Receipt.ObservationID || !reflect.DeepEqual(c, o.Config()) {
		return row{}, decision.OutcomeObservation{}, ErrUnavailable
	}
	return r, o, nil
}

// check reauthorizes every ancestor, preventing a correction from laundering
// withdrawn evidence. Corrections branch; there is deliberately no mutable head.
func (s *Store) check(ctx context.Context, before auth.Decision, o decision.OutcomeObservation, receipt shoal.ID, received time.Time, op auth.Operation) error {
	chain := []decision.OutcomeObservation{o}
	// Collection has no outcome-evidence verification that a later storage read
	// could invalidate. Even failures are withheld until current authorization of
	// the collected observations has been rechecked. Incompatible or incomplete
	// links remain opaque NotFound: uncollected ancestry may be unauthorized.
	collectErr := func() error {
		seen := map[shoal.ID]bool{receipt: true}
		current := o.Config()
		previousTime := received
		for n := 0; current.Supersedes != ""; n++ {
			if n >= MaxOutcomeAncestors || seen[current.Supersedes] {
				return auth.ObjectNotFound()
			}
			seen[current.Supersedes] = true
			d, p, e := s.resolve(ctx, &before, o.RequestID(), o.PredictionID(), auth.OperationRead)
			if e != nil {
				return e
			}
			raw, readErr := s.read(ctx, current.Supersedes)
			if _, _, e = s.resolve(ctx, &before, o.RequestID(), o.PredictionID(), auth.OperationRead); e != nil {
				return e
			}
			if readErr != nil {
				return readErr
			}
			predecessor, po, e := s.validateRow(raw, current.Supersedes, d, p)
			if e != nil {
				return e
			}
			chain = append(chain, po) // Verify even a valid but incompatible predecessor before disclosing the link error.
			pc := po.Config()
			if pc.SubjectID != current.SubjectID || pc.QuestionID != current.QuestionID || pc.Kind != current.Kind || pc.ActionID != current.ActionID || predecessor.Receipt.ReceivedAt.After(previousTime) {
				return auth.ObjectNotFound()
			}
			current = pc
			previousTime = predecessor.Receipt.ReceivedAt
		}
		return nil
	}()
	if o.Config().Supersedes != "" {
		if _, _, e := s.resolve(ctx, &before, o.RequestID(), o.PredictionID(), auth.OperationRead); e != nil {
			return e
		}
	}
	d, _, e := s.resolve(ctx, &before, o.RequestID(), o.PredictionID(), op)
	if e != nil {
		return e
	}
	// No backend or authority Resolve call may follow this joint verification:
	// those calls could block across evidence revocation and stale this decision.
	if e = s.config.Authority.Verify(ctx, d, chain); e != nil {
		return sanitized(e)
	}
	if _, e = s.resolveCaller(ctx, &before); e != nil {
		return e
	}
	return collectErr
}

func (s *Store) Append(ctx context.Context, request, prediction shoal.ID, key []byte, cfg decision.OutcomeObservationConfig) (receipt Receipt, err error) {
	coordinated := false
	defer func() {
		if coordinated && err != nil {
			err = errors.Join(ErrIndeterminate, err)
		}
	}()
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return Receipt{}, invalid()
	}
	d, p, e := s.resolve(ctx, nil, request, prediction, auth.OperationIngest)
	if e != nil {
		return Receipt{}, e
	}
	if cfg.RequestID != request || cfg.PredictionID != prediction {
		return Receipt{}, invalid()
	}
	o, e := decision.NewOutcomeObservation(p, cfg)
	if e != nil {
		return Receipt{}, invalid()
	}
	cfg = o.Config()
	id := receiptID(scopeFor(d), request, hash(key))
	raw, readErr := s.read(ctx, id)
	if _, _, e = s.resolve(ctx, &d, request, prediction, auth.OperationIngest); e != nil {
		return Receipt{}, e
	}
	if readErr == nil {
		original, e := s.replay(ctx, d, p, id, raw, o, auth.OperationIngest)
		if e != nil || s.config.Admission == nil {
			return original, e
		}
		coordinated = true
		if e = s.beginAdmission(ctx, d, p, o, id, original.ReceivedAt); e != nil {
			return Receipt{}, e
		}
		return s.publishAdmission(ctx, d, p, o, original)
	}
	if !shoal.IsErrorCode(readErr, shoal.ErrorNotFound) {
		return Receipt{}, readErr
	}
	now, e := s.now()
	if e != nil {
		return Receipt{}, e
	}
	if now.IsZero() || cfg.ObservedAt.After(now) || now.Before(p.Config().CompletedAt) {
		return Receipt{}, invalid()
	}
	fp, _ := auth.AuthorizationFingerprint(d)
	value := row{Receipt: Receipt{ID: id, ObservationID: o.ID(), ObservationConfig: cfg, SubmitterID: d.Subject(), ActorID: d.Actor(), ClientID: d.ClientID(), OnBehalfOf: d.OnBehalfOf(), AuthorizationFingerprint: fp.String(), ReceivedAt: now, State: "proposed"}, ScopeDigest: scopeFor(d), KeyDigest: hash(key)}
	if e = s.check(ctx, d, o, id, now, auth.OperationIngest); e != nil {
		return Receipt{}, e
	}
	encoded, e := encode(value)
	if e != nil {
		return Receipt{}, invalid()
	}
	if s.config.Admission != nil {
		coordinated = true
		if e = s.beginAdmission(ctx, d, p, o, id, now); e != nil {
			return Receipt{}, e
		}
	}
	coordinate := s.coordinate(id)
	status, writeErr := s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}}, Updates: []allocator.Update{{Coordinate: coordinate, Timestamp: 1, Value: encoded}}})
	// Regardless of the acknowledgement, read the immutable winner back. Any
	// error after the attempted CAS is uncertain unless an authorized exact
	// observation replay is confirmed; denial cannot establish rollback.
	stored, readErr := s.read(ctx, id)
	if _, _, e = s.resolve(ctx, &d, request, prediction, auth.OperationIngest); e != nil {
		return Receipt{}, errors.Join(ErrIndeterminate, e)
	}
	if readErr != nil {
		return Receipt{}, errors.Join(ErrIndeterminate, readErr)
	}
	result, e := s.replay(ctx, d, p, id, stored, o, auth.OperationIngest)
	if status == allocator.StatusRejected && writeErr == nil && errors.Is(e, ErrConflict) {
		return Receipt{}, ErrConflict
	}
	if e != nil {
		return Receipt{}, errors.Join(ErrIndeterminate, e)
	}
	// Exact authorized readback reconciles even an unknown acknowledgement.
	if s.config.Admission != nil {
		return s.publishAdmission(ctx, d, p, o, result)
	}
	return result, nil
}
func (s *Store) replay(ctx context.Context, d auth.Decision, p decision.PredictionRecord, id shoal.ID, raw []byte, want decision.OutcomeObservation, op auth.Operation) (Receipt, error) {
	value, o, e := s.validateRow(raw, id, d, p)
	if e != nil {
		return Receipt{}, e
	}
	if e = s.check(ctx, d, o, id, value.Receipt.ReceivedAt, op); e != nil {
		return Receipt{}, e
	}
	if o.ID() != want.ID() {
		return Receipt{}, ErrConflict
	}
	return value.Receipt, nil
}
func (s *Store) Read(ctx context.Context, request, prediction shoal.ID, key []byte) (Receipt, error) {
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return Receipt{}, invalid()
	}
	d, p, e := s.resolve(ctx, nil, request, prediction, auth.OperationRead)
	if e != nil {
		return Receipt{}, e
	}
	id := receiptID(scopeFor(d), request, hash(key))
	raw, readErr := s.read(ctx, id)
	if _, _, e = s.resolve(ctx, &d, request, prediction, auth.OperationRead); e != nil {
		return Receipt{}, e
	}
	if readErr != nil {
		return Receipt{}, readErr
	}
	value, o, e := s.validateRow(raw, id, d, p)
	if e != nil {
		return Receipt{}, e
	}
	if e = s.check(ctx, d, o, id, value.Receipt.ReceivedAt, auth.OperationRead); e != nil {
		return Receipt{}, e
	}
	return value.Receipt, nil
}

// Used by the strict codec and tests to distinguish absent/forged receipt IDs.
func validReceiptID(id shoal.ID) bool {
	return strings.HasPrefix(string(id), "outcome-receipt:") && validHash(strings.TrimPrefix(string(id), "outcome-receipt:"))
}
