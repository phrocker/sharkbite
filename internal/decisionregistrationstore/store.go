// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionregistrationstore retains frozen request registration intents.
// It is a trusted storage primitive, not an authority. The host authenticates
// scopes, validates canonical artifact bytes, and authorizes current sources.
// Preparing records may only authorize admission staging; execution requires
// ready AND current authorization after every storage operation. Ready is a
// durable retention fact, never a grant or a freshness assertion.
package decisionregistrationstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_decision_registrations"
const MaxArtifactBytes = 48 << 20
const MaxDescriptorBytes = 1 << 20
const MaxSources = 256
const maxPrimaryBytes = MaxDescriptorBytes + (128 << 10)

var (
	ErrNotFound      = errors.New("decision registration not found")
	ErrConflict      = errors.New("decision registration conflict")
	ErrCorrupt       = errors.New("decision registration corrupt")
	ErrUnavailable   = errors.New("decision registration unavailable")
	ErrIndeterminate = errors.New("decision registration write indeterminate")
)

type Scope struct {
	Domain                       []byte
	SubjectID, ActorID, ClientID shoal.ID
	OnBehalfOf                   []shoal.ID
}
type SourcePin struct {
	// Mode records whether acquisition was imported or server observed. Neither
	// mode implies trustworthy content, verified truth, or a training grant.
	Mode                                                                    collector.Mode
	CollectorID, ObservationID, ArtifactID, EnrollmentID, AuthorityPolicyID shoal.ID
	Generation                                                              int64
	ArtifactSHA256, SourceSHA256                                            string
	ReceivedAt                                                              time.Time
}
type Frozen struct {
	SelectionSHA256                           string
	ProfileID, ProfileRevisionID, BuilderID   shoal.ID
	RequestID, TaskID, PictureID, PredictorID shoal.ID
	Sources                                   []SourcePin
	AcceptedAt, AuthenticationExpiresAt       time.Time
	AuthorizationFingerprint                  string
	RecordSHA256                              string
	RecordBytes                               int
}
type State string

const (
	Preparing State = "preparing"
	Ready     State = "ready"
)

type Registration struct {
	ID                            shoal.ID
	Scope                         Scope
	State                         State
	Version                       int64
	Frozen                        Frozen
	FrozenSHA256                  string
	CreatedAt, UpdatedAt, ReadyAt time.Time
}
type Config struct {
	Backend    decisionstore.CAS
	Clock      func() time.Time
	Visibility []byte
}
type Store struct{ config Config }

// VerifyRetention is a mandatory read-only trusted check of the exact catalog
// record and current rights. It receives detached values. The host must still
// reauthorize after the primitive returns; callbacks confer no persistent grant.
type VerifyRetention func(context.Context, Registration, []byte) error

func New(c Config) (*Store, error) {
	if absent(c.Backend) || c.Clock == nil || len(c.Visibility) > 4096 {
		return nil, invalid()
	}
	c.Visibility = bytes.Clone(c.Visibility)
	return &Store{c}, nil
}
func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid decision registration")
}
func hash(b []byte) string   { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
func digest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func validTime(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && t == t.Round(0).UTC()
}
func textID(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != "" && len(id) <= shoal.MaxIDBytes
}
func cloneScope(s Scope) Scope {
	s.Domain = bytes.Clone(s.Domain)
	s.OnBehalfOf = append([]shoal.ID(nil), s.OnBehalfOf...)
	return s
}
func cloneRegistration(r Registration) Registration {
	r.Scope = cloneScope(r.Scope)
	r.Frozen.Sources = append([]SourcePin(nil), r.Frozen.Sources...)
	return r
}
func validScope(s Scope) bool {
	if len(s.Domain) == 0 || len(s.Domain) > auth.MaxPolicyComponentBytes || len(s.SubjectID) == 0 || len(s.SubjectID) > shoal.MaxIDBytes || len(s.ActorID) > shoal.MaxIDBytes || len(s.ClientID) > shoal.MaxIDBytes || len(s.OnBehalfOf) > 64 {
		return false
	}
	for _, id := range s.OnBehalfOf {
		if len(id) == 0 || len(id) > shoal.MaxIDBytes {
			return false
		}
	}
	return true
}
func normalizeFrozen(f Frozen) (Frozen, error) {
	if !digest(f.SelectionSHA256) || !digest(f.RecordSHA256) || f.RecordBytes < 1 || f.RecordBytes > MaxArtifactBytes || len(f.Sources) == 0 || len(f.Sources) > MaxSources || !validTime(f.AcceptedAt) || !validTime(f.AuthenticationExpiresAt) || !f.AcceptedAt.Before(f.AuthenticationExpiresAt) || !strings.HasPrefix(f.AuthorizationFingerprint, "auth-sha256:") || !digest(strings.TrimPrefix(f.AuthorizationFingerprint, "auth-sha256:")) {
		return Frozen{}, invalid()
	}
	for _, id := range []shoal.ID{f.ProfileID, f.ProfileRevisionID, f.BuilderID, f.RequestID, f.TaskID, f.PictureID, f.PredictorID} {
		if !textID(id) {
			return Frozen{}, invalid()
		}
	}
	for _, p := range f.Sources {
		for _, id := range []shoal.ID{p.CollectorID, p.ObservationID, p.ArtifactID, p.EnrollmentID, p.AuthorityPolicyID} {
			if !textID(id) {
				return Frozen{}, invalid()
			}
		}
		if (p.Mode != collector.Imported && p.Mode != collector.ServerObserved) || p.Generation < 1 || !digest(p.ArtifactSHA256) || !digest(p.SourceSHA256) || !validTime(p.ReceivedAt) || p.ReceivedAt.After(f.AcceptedAt) {
			return Frozen{}, invalid()
		}
	}
	f.Sources = append([]SourcePin(nil), f.Sources...)
	sort.Slice(f.Sources, func(i, j int) bool { return f.Sources[i].ObservationID < f.Sources[j].ObservationID })
	for i := 1; i < len(f.Sources); i++ {
		if f.Sources[i].ObservationID == f.Sources[i-1].ObservationID {
			return Frozen{}, invalid()
		}
	}
	if len(jsonBytes(f)) > MaxDescriptorBytes {
		return Frozen{}, invalid()
	}
	return f, nil
}
func (s *Store) now() (time.Time, error) {
	t := s.config.Clock().Round(0).UTC()
	if !validTime(t) {
		return t, ErrUnavailable
	}
	return t, nil
}
func primaryID(scope Scope, keyHash string) shoal.ID {
	return shoal.ID("decision-registration:" + hash(jsonBytes(struct {
		Scope     scopeWire
		KeySHA256 string
	}{scopeToWire(scope), keyHash})))
}
func (s *Store) coord(kind, key string) allocator.Coordinate {
	return allocator.Coordinate{Row: []byte(kind + ":" + key), Family: []byte("r"), Qualifier: []byte(kind), Visibility: bytes.Clone(s.config.Visibility)}
}
func (s *Store) primaryCoord(id shoal.ID) allocator.Coordinate {
	return s.coord("registration", string(id))
}
func (s *Store) blobCoord(scope Scope, d string) allocator.Coordinate {
	return s.coord("preparation", hash(jsonBytes(struct {
		Scope  scopeWire
		SHA256 string
	}{scopeToWire(scope), d})))
}
func (s *Store) aliasCoord(domain []byte, subject, request shoal.ID) allocator.Coordinate {
	return s.coord("request", hash(jsonBytes(struct {
		Domain, Subject []byte
		RequestID       shoal.ID
	}{domain, []byte(subject), request})))
}
func sameCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func (s *Store) readRaw(ctx context.Context, c allocator.Coordinate, max int) ([]byte, int64, error) {
	if ctx.Err() != nil {
		return nil, 0, ErrUnavailable
	}
	cells, e := s.config.Backend.ReadExact(ctx, []allocator.Coordinate{c})
	if e != nil || ctx.Err() != nil {
		return nil, 0, ErrUnavailable
	}
	if len(cells) == 0 {
		return nil, 0, ErrNotFound
	}
	if len(cells) != 1 || !sameCoordinate(cells[0].Coordinate, c) || len(cells[0].Value) == 0 || len(cells[0].Value) > max {
		return nil, 0, ErrCorrupt
	}
	return bytes.Clone(cells[0].Value), cells[0].Timestamp, nil
}
func (s *Store) load(ctx context.Context, id shoal.ID) (Registration, string, []byte, error) {
	raw, stamp, e := s.readRaw(ctx, s.primaryCoord(id), maxPrimaryBytes)
	if e != nil {
		return Registration{}, "", nil, e
	}
	r, key, e := decodePrimary(raw)
	if e != nil || r.ID != id || stamp != r.Version {
		return Registration{}, "", nil, ErrCorrupt
	}
	return r, key, raw, nil
}
func (s *Store) blob(ctx context.Context, r Registration) ([]byte, error) {
	raw, stamp, e := s.readRaw(ctx, s.blobCoord(r.Scope, r.Frozen.RecordSHA256), MaxArtifactBytes)
	if e != nil {
		return nil, e
	}
	if stamp != 1 || len(raw) != r.Frozen.RecordBytes || hash(raw) != r.Frozen.RecordSHA256 {
		return nil, ErrCorrupt
	}
	return raw, nil
}
func aliasFor(r Registration) alias {
	return alias{r.ID, r.Frozen.RequestID, r.FrozenSHA256, r.Frozen.RecordSHA256}
}
func (s *Store) checkAlias(ctx context.Context, r Registration) error {
	raw, stamp, e := s.readRaw(ctx, s.aliasCoord(r.Scope.Domain, r.Scope.SubjectID, r.Frozen.RequestID), 8192)
	if e != nil {
		return e
	}
	var a alias
	if decode(raw, &a) != nil || stamp != 1 {
		return ErrCorrupt
	}
	if a != aliasFor(r) {
		return ErrConflict
	}
	return nil
}
func (s *Store) putImmutable(ctx context.Context, c allocator.Coordinate, b []byte, max int) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	status, writeErr := s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: c.Row, Conditions: []allocator.Condition{{Coordinate: c, Absent: true}}, Updates: []allocator.Update{{Coordinate: c, Timestamp: 1, Value: b}}})
	raw, stamp, readErr := s.readRaw(ctx, c, max)
	if readErr == nil && stamp == 1 && bytes.Equal(raw, b) {
		return nil
	}
	if status == allocator.StatusRejected && writeErr == nil && readErr == nil {
		return ErrConflict
	}
	return errors.Join(ErrIndeterminate, readErr)
}

// Begin freezes a complete exact preparation. ReadByKey BEFORE building on a
// retry: changed timestamps, profiles, auth pins or bytes conflict, never replace
// an existing intent. Unreferenced immutable blobs are not registrations.
func (s *Store) Begin(ctx context.Context, scope Scope, key []byte, f Frozen, artifact []byte) (result Registration, err error) {
	if !validScope(scope) || len(key) == 0 || len(key) > shoal.MaxIDBytes || len(artifact) == 0 || len(artifact) > MaxArtifactBytes {
		return result, invalid()
	}
	f, e := normalizeFrozen(f)
	if e != nil || len(artifact) != f.RecordBytes || hash(artifact) != f.RecordSHA256 {
		return result, invalid()
	}
	scope = cloneScope(scope)
	kh := hash(key)
	id := primaryID(scope, kh)
	fd := hash(jsonBytes(f))
	old, _, _, e := s.load(ctx, id)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return result, e
	}
	if e == nil && old.FrozenSHA256 != fd {
		return result, ErrConflict
	}
	mutated := false
	defer func() {
		if err != nil && mutated {
			err = errors.Join(ErrIndeterminate, err)
			result = Registration{}
		}
	}()
	if errors.Is(e, ErrNotFound) {
		now, clockErr := s.now()
		if clockErr != nil || now.Before(f.AcceptedAt) {
			return result, ErrUnavailable
		}
		candidate := Registration{ID: id, Scope: scope, State: Preparing, Version: 1, Frozen: f, FrozenSHA256: fd, CreatedAt: now, UpdatedAt: now}
		// Copy before external storage; no caller buffer can change a committed blob.
		owned := bytes.Clone(artifact)
		mutated = true
		if e = s.putImmutable(ctx, s.blobCoord(scope, f.RecordSHA256), owned, MaxArtifactBytes); e != nil {
			return result, e
		}
		encoded, e := encodePrimary(candidate, kh)
		if e != nil {
			return result, e
		}
		c := s.primaryCoord(id)
		status, writeErr := s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: c.Row, Conditions: []allocator.Condition{{Coordinate: c, Absent: true}}, Updates: []allocator.Update{{Coordinate: c, Timestamp: 1, Value: encoded}}})
		old, _, _, e = s.load(ctx, id)
		if e != nil {
			return result, errors.Join(ErrIndeterminate, e)
		}
		if old.FrozenSHA256 != fd {
			if writeErr == nil && status == allocator.StatusRejected {
				return result, ErrConflict
			}
			return result, ErrIndeterminate
		}
	}
	if _, e = s.blob(ctx, old); e != nil {
		return result, e
	}
	a, _ := encode(aliasFor(old))
	mutated = true
	if e = s.putImmutable(ctx, s.aliasCoord(scope.Domain, scope.SubjectID, f.RequestID), a, 8192); e != nil {
		return result, e
	}
	current, _, _, e := s.load(ctx, id)
	if e != nil || current.FrozenSHA256 != fd {
		return result, errors.Join(ErrIndeterminate, e)
	}
	return current, nil
}

// ReadByKey returns frozen metadata, not permission to execute or disclose it.
func (s *Store) ReadByKey(ctx context.Context, scope Scope, key []byte) (Registration, error) {
	if !validScope(scope) || len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return Registration{}, invalid()
	}
	r, _, _, e := s.load(ctx, primaryID(scope, hash(key)))
	return r, e
}
func (s *Store) LoadPrepared(ctx context.Context, scope Scope, key []byte) (Registration, []byte, error) {
	r, e := s.ReadByKey(ctx, scope, key)
	if e != nil {
		return Registration{}, nil, e
	}
	b, e := s.blob(ctx, r)
	if e != nil {
		return Registration{}, nil, e
	}
	return r, b, nil
}

// LookupRequest joins the immutable subject-scoped alias to its original full
// attribution. Only a trusted authority may use this cross-actor lookup. The
// returned state is not a current execution or source-disclosure grant.
func (s *Store) LookupRequest(ctx context.Context, domain []byte, subject, request shoal.ID) (Registration, error) {
	if len(domain) == 0 || len(domain) > auth.MaxPolicyComponentBytes || len(subject) == 0 || len(subject) > shoal.MaxIDBytes || !textID(request) {
		return Registration{}, invalid()
	}
	raw, stamp, e := s.readRaw(ctx, s.aliasCoord(domain, subject, request), 8192)
	if e != nil {
		return Registration{}, e
	}
	var a alias
	if stamp != 1 || decode(raw, &a) != nil {
		return Registration{}, ErrCorrupt
	}
	r, _, _, e := s.load(ctx, a.PrimaryID)
	if e != nil {
		return Registration{}, e
	}
	if !bytes.Equal(domain, r.Scope.Domain) || subject != r.Scope.SubjectID || request != r.Frozen.RequestID || a != aliasFor(r) {
		return Registration{}, ErrCorrupt
	}
	return r, nil
}
func (s *Store) verify(ctx context.Context, r Registration, guard VerifyRetention) error {
	if e := s.checkAlias(ctx, r); e != nil {
		return e
	}
	b, e := s.blob(ctx, r)
	if e != nil {
		return e
	}
	copy := cloneRegistration(r)
	before := cloneRegistration(copy)
	if e = guard(ctx, copy, b); e != nil {
		return e
	}
	if !reflect.DeepEqual(copy, before) || hash(b) != r.Frozen.RecordSHA256 || len(b) != r.Frozen.RecordBytes {
		return ErrConflict
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	return nil
}

// MarkReady never expires/aborts a preparation. Every retry (including already
// ready) verifies the alias and exact catalog commitment. A failed return after
// mutation cannot establish rollback. The frozen descriptor never changes.
func (s *Store) MarkReady(ctx context.Context, scope Scope, key []byte, frozenSHA string, guard VerifyRetention) (result Registration, err error) {
	if guard == nil || !digest(frozenSHA) || !validScope(scope) || len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return result, invalid()
	}
	r, kh, raw, e := s.load(ctx, primaryID(scope, hash(key)))
	if e != nil {
		return result, e
	}
	if r.FrozenSHA256 != frozenSHA {
		return result, ErrConflict
	}
	if e = s.verify(ctx, r, guard); e != nil {
		return result, e
	}
	if r.State == Ready {
		return r, nil
	}
	now, e := s.now()
	if e != nil || now.Before(r.UpdatedAt) {
		return result, ErrUnavailable
	}
	next := cloneRegistration(r)
	next.State = Ready
	next.Version = 2
	next.UpdatedAt = now
	next.ReadyAt = now
	encoded, e := encodePrimary(next, kh)
	if e != nil {
		return result, e
	}
	if ctx.Err() != nil {
		return result, ErrUnavailable
	}
	c := s.primaryCoord(r.ID)
	_, _ = s.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: c.Row, Conditions: []allocator.Condition{{Coordinate: c, Value: raw, TimestampSet: true, Timestamp: 1}}, Updates: []allocator.Update{{Coordinate: c, Timestamp: 2, Value: encoded}}})
	current, _, _, e := s.load(ctx, r.ID)
	if e != nil || current.State != Ready || current.FrozenSHA256 != frozenSHA {
		return result, errors.Join(ErrIndeterminate, e)
	}
	if e = s.verify(ctx, current, guard); e != nil {
		return result, errors.Join(ErrIndeterminate, e)
	}
	return current, nil
}
