// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisioninventorystore persists a trusted host's guarded outcome
// inventory. Registration asserts a coverage barrier; it does not discover
// legacy or bypassed writes. Callers must authenticate every input and jointly
// authorize current sources before disclosure. Pending intents never expire.
package decisioninventorystore

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
	"unicode/utf8"

	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_decision_inventories"
const MaxEntries = 256
const MaxStoredBytes = 4 << 20

var (
	ErrNotFound      = errors.New("target inventory not found")
	ErrConflict      = errors.New("target inventory conflict")
	ErrCorrupt       = errors.New("target inventory corrupt")
	ErrUnavailable   = errors.New("target inventory unavailable")
	ErrIndeterminate = errors.New("target inventory write indeterminate")
	ErrLimit         = errors.New("target inventory limit")
)

type Scope struct{ Domain []byte }
type Binding struct{ CoverageID, TargetID, TaskID, PictureID, SubjectID, QuestionID shoal.ID }
type Attribution struct {
	SubjectID, ActorID, ClientID shoal.ID
	OnBehalfOf                   []shoal.ID
}
type Intent struct {
	ReceiptID, ObservationID, RequestID, PredictionID shoal.ID
	Reporter                                          Attribution
}
type State string

const (
	Pending   State = "pending"
	Published State = "published"
)

type Entry struct {
	Intent                            Intent
	State                             State
	OpenedAt, PublishedAt, ReceivedAt time.Time
	ReceiptDigest                     string
}
type Snapshot struct {
	ID                   shoal.ID
	Binding              Binding
	Version              int64
	CreatedAt, UpdatedAt time.Time
	Entries              []Entry
}

// Complete describes only the registered guarded namespace, not wider coverage.
// A zero value or absent row is never complete.
func (s Snapshot) Complete() bool {
	if s.ID == "" || s.Version < 1 {
		return false
	}
	for _, e := range s.Entries {
		if e.State != Published {
			return false
		}
	}
	return true
}

type Config struct {
	Backend    decisionstore.CAS
	Clock      func() time.Time
	Visibility []byte
}
type Store struct{ config Config }

func New(c Config) (*Store, error) {
	if absent(c.Backend) || c.Clock == nil || len(c.Visibility) > 4096 {
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
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid target inventory request")
}
func hash(b []byte) string   { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
func digest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func textID(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != "" && shoal.ValidateRequiredID("id", id) == nil
}
func receiptID(id shoal.ID) bool {
	return strings.HasPrefix(string(id), "outcome-receipt:") && digest(strings.TrimPrefix(string(id), "outcome-receipt:"))
}
func validBinding(b Binding) bool {
	for _, id := range []shoal.ID{b.CoverageID, b.TargetID, b.TaskID, b.PictureID, b.SubjectID, b.QuestionID} {
		if !textID(id) {
			return false
		}
	}
	target, e := decision.AdjudicationTargetID(b.TaskID, b.PictureID, b.SubjectID, b.QuestionID)
	return e == nil && target == b.TargetID
}
func validAttribution(a Attribution) bool {
	if len(a.SubjectID) == 0 || len(a.ActorID) == 0 || len(a.SubjectID) > shoal.MaxIDBytes || len(a.ActorID) > shoal.MaxIDBytes || len(a.ClientID) > shoal.MaxIDBytes || len(a.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return false
	}
	for _, id := range a.OnBehalfOf {
		if len(id) == 0 || len(id) > shoal.MaxIDBytes {
			return false
		}
	}
	return true
}
func cloneIntent(i Intent) Intent {
	i.Reporter.OnBehalfOf = append([]shoal.ID(nil), i.Reporter.OnBehalfOf...)
	return i
}
func validIntent(i Intent) bool {
	return receiptID(i.ReceiptID) && textID(i.ObservationID) && textID(i.RequestID) && textID(i.PredictionID) && validAttribution(i.Reporter)
}
func validTime(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && t == t.Round(0).UTC()
}
func (s *Store) now() (time.Time, error) {
	n := s.config.Clock().Round(0).UTC()
	if !validTime(n) {
		return n, ErrUnavailable
	}
	return n, nil
}
func scopeDigest(scope Scope) (string, error) {
	if len(scope.Domain) == 0 || len(scope.Domain) > auth.MaxPolicyComponentBytes {
		return "", invalid()
	}
	return hash(jsonBytes(struct{ Domain []byte }{scope.Domain})), nil
}
func (s *Store) coordinate(scope string, b Binding) allocator.Coordinate {
	return allocator.Coordinate{Row: []byte("target-inventory:" + hash(jsonBytes([]string{"target-inventory-v1", scope, string(b.TargetID)}))), Family: []byte("i"), Qualifier: []byte("inventory"), Visibility: append([]byte(nil), s.config.Visibility...)}
}
func equalCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func (s *Store) read(ctx context.Context, scope string, b Binding) (Snapshot, []byte, error) {
	var zero Snapshot
	if ctx.Err() != nil {
		return zero, nil, ErrUnavailable
	}
	c := s.coordinate(scope, b)
	cells, e := s.config.Backend.ReadExact(ctx, []allocator.Coordinate{c})
	if e != nil || ctx.Err() != nil {
		return zero, nil, ErrUnavailable
	}
	if len(cells) == 0 {
		return zero, nil, ErrNotFound
	}
	if len(cells) != 1 || !equalCoordinate(cells[0].Coordinate, c) || len(cells[0].Value) > MaxStoredBytes {
		return zero, nil, ErrCorrupt
	}
	snap, e := decode(cells[0].Value, scope)
	if e != nil || cells[0].Timestamp != snap.Version {
		return zero, nil, ErrCorrupt
	}
	if snap.Binding != b {
		return zero, nil, ErrConflict
	}
	return snap, append([]byte(nil), cells[0].Value...), nil
}
func (s *Store) Load(ctx context.Context, scope Scope, b Binding) (Snapshot, error) {
	sd, e := scopeDigest(scope)
	if e != nil || !validBinding(b) {
		return Snapshot{}, invalid()
	}
	v, _, e := s.read(ctx, sd, b)
	return v, e
}
func find(s Snapshot, i Intent) (int, error) {
	for n, e := range s.Entries {
		if e.Intent.ReceiptID == i.ReceiptID {
			if !reflect.DeepEqual(e.Intent, cloneIntent(i)) {
				return n, ErrConflict
			}
			return n, nil
		}
	}
	return -1, nil
}
func (s *Store) commit(ctx context.Context, scope string, b Binding, old Snapshot, raw []byte, next Snapshot, accept func(Snapshot) bool) (Snapshot, error) {
	var zero Snapshot
	encoded, e := encode(scope, next)
	if e != nil {
		return zero, e
	}
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	c := s.coordinate(scope, b)
	condition := allocator.Condition{Coordinate: c, Absent: old.Version == 0}
	if old.Version != 0 {
		condition.Value = raw
		condition.TimestampSet = true
		condition.Timestamp = old.Version
	}
	status, writeErr := s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: c.Row, Conditions: []allocator.Condition{condition}, Updates: []allocator.Update{{Coordinate: c, Timestamp: next.Version, Value: encoded}}})
	stored, _, readErr := s.read(ctx, scope, b)
	if readErr == nil && accept(stored) {
		return stored, nil
	}
	if status == allocator.StatusRejected && writeErr == nil && readErr == nil {
		return zero, ErrConflict
	}
	return zero, errors.Join(ErrIndeterminate, readErr)
}

// Register is an explicit trusted coverage assertion. Exact retries preserve its
// original CreatedAt; they return the current snapshot without advancing its
// version. Begin never creates this barrier.
func (s *Store) Register(ctx context.Context, scope Scope, b Binding) (Snapshot, error) {
	var zero Snapshot
	sd, e := scopeDigest(scope)
	if e != nil || !validBinding(b) {
		return zero, invalid()
	}
	old, raw, e := s.read(ctx, sd, b)
	if e == nil {
		return old, nil
	}
	if !errors.Is(e, ErrNotFound) {
		return zero, e
	}
	now, e := s.now()
	if e != nil {
		return zero, e
	}
	next := Snapshot{Binding: b, Version: 1, CreatedAt: now, UpdatedAt: now}
	return s.commit(ctx, sd, b, old, raw, next, func(v Snapshot) bool { return v.Binding == b })
}

// Begin durably marks incompleteness before the caller attempts the outcome CAS.
// Exact retries return the original intent even after publication.
func (s *Store) Begin(ctx context.Context, scope Scope, b Binding, i Intent) (Snapshot, error) {
	var zero Snapshot
	sd, e := scopeDigest(scope)
	if e != nil || !validBinding(b) || !validIntent(i) {
		return zero, invalid()
	}
	i = cloneIntent(i)
	old, raw, e := s.read(ctx, sd, b)
	if e != nil {
		return zero, e
	}
	n, e := find(old, i)
	if e != nil {
		return zero, e
	}
	if n >= 0 {
		return old, nil
	}
	if len(old.Entries) >= MaxEntries {
		return zero, ErrLimit
	}
	now, e := s.now()
	if e != nil || now.Before(old.UpdatedAt) {
		return zero, ErrUnavailable
	}
	next := old
	next.Version++
	next.UpdatedAt = now
	next.Entries = append(append([]Entry(nil), old.Entries...), Entry{Intent: i, State: Pending, OpenedAt: now})
	return s.commit(ctx, sd, b, old, raw, next, func(v Snapshot) bool { n, e := find(v, i); return e == nil && n >= 0 })
}

// Publish requires an independently authenticated immutable outcome receipt.
// Structural checks and a full byte-preserving digest bind it, but this storage
// layer cannot establish that the receipt came from the actual outcome store.
func (s *Store) Publish(ctx context.Context, scope Scope, b Binding, i Intent, r outcomes.Receipt) (Snapshot, error) {
	var zero Snapshot
	sd, e := scopeDigest(scope)
	if e != nil || !validBinding(b) || !validIntent(i) {
		return zero, invalid()
	}
	i = cloneIntent(i)
	rd, e := receiptDigest(b, i, r)
	if e != nil {
		return zero, e
	}
	old, raw, e := s.read(ctx, sd, b)
	if e != nil {
		return zero, e
	}
	n, e := find(old, i)
	if e != nil {
		return zero, e
	}
	if n < 0 {
		return zero, ErrNotFound
	}
	if old.Entries[n].State == Published {
		if old.Entries[n].ReceiptDigest != rd {
			return zero, ErrConflict
		}
		return old, nil
	}
	now, e := s.now()
	if e != nil || now.Before(old.UpdatedAt) || r.ReceivedAt.After(now) {
		return zero, ErrUnavailable
	}
	next := old
	next.Version++
	next.UpdatedAt = now
	next.Entries = append([]Entry(nil), old.Entries...)
	entry := &next.Entries[n]
	entry.State = Published
	entry.PublishedAt = now
	entry.ReceivedAt = r.ReceivedAt
	entry.ReceiptDigest = rd
	return s.commit(ctx, sd, b, old, raw, next, func(v Snapshot) bool {
		n, e := find(v, i)
		return e == nil && n >= 0 && v.Entries[n].State == Published && v.Entries[n].ReceiptDigest == rd
	})
}
