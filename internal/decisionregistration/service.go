// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionregistration composes operator-pinned profiles, authenticated
// collector material and immutable request artifacts. It supplies no default
// source/purpose authority and accepts no caller task or model configuration.
package decisionregistration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	registrations "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var ErrUnavailable = shoal.NewError(shoal.ErrorUnavailable, "request registration unavailable")
var ErrConflict = shoal.NewError(shoal.ErrorConflict, "request registration conflict")
var ErrIndeterminate = errors.New("request registration mutation indeterminate")

// AccessAuthority is a trusted, mandatory current purpose/source and joint
// disclosure authority. Verify must finish its own policy IO before its final
// decision. Historical registration, a digest, or collector attestation alone
// grants neither execution nor training permission. For a new candidate before
// Begin, Registration.ID is empty; its frozen input commitments are complete.
type AccessAuthority interface {
	Authorize(context.Context, auth.Decision, auth.Operation, Profile, []shoal.ID) error
	Verify(context.Context, auth.Decision, auth.Operation, Profile, registrations.Registration, decisionartifacts.Record, []collectorregistry.Material) error
}
type Config struct {
	Resolver        auth.Resolver
	Registrations   *registrations.Store
	ArtifactBackend decisionstore.CAS
	Materials       *collectorregistry.MaterialResolver
	Profiles        []Profile
	Authority       AccessAuthority
	Clock           func() time.Time
	Visibility      []byte
}
type Service struct {
	config   Config
	profiles map[string]Profile
}

func absentDependency(v any) bool {
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
func invalidRegistration() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid request registration")
}
func New(c Config) (*Service, error) {
	if absentDependency(c.Resolver) || c.Registrations == nil || absentDependency(c.ArtifactBackend) || c.Materials == nil || absentDependency(c.Authority) || c.Clock == nil || len(c.Visibility) > 4096 {
		return nil, invalidRegistration()
	}
	profiles, e := validateProfiles(c.Profiles)
	if e != nil {
		return nil, e
	}
	c.Profiles = nil
	c.Visibility = bytes.Clone(c.Visibility)
	return &Service{c, profiles}, nil
}
func registrationError(e error) error {
	if e == nil {
		return nil
	}
	if shoal.IsErrorCode(e, shoal.ErrorNotFound) || shoal.IsErrorCode(e, shoal.ErrorUnauthorized) || errors.Is(e, registrations.ErrNotFound) {
		return auth.ObjectNotFound()
	}
	if errors.Is(e, registrations.ErrConflict) || shoal.IsErrorCode(e, shoal.ErrorConflict) {
		return ErrConflict
	}
	if errors.Is(e, registrations.ErrIndeterminate) || errors.Is(e, decisionartifacts.ErrIndeterminate) {
		return errors.Join(ErrIndeterminate, ErrUnavailable)
	}
	return ErrUnavailable
}
func hashRegistration(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (s *Service) now() (time.Time, error) {
	t := s.config.Clock().Round(0).UTC()
	if t.IsZero() || t.Year() < 1 || t.Year() > 9999 {
		return t, ErrUnavailable
	}
	return t, nil
}
func (s *Service) caller(ctx context.Context, before *auth.Decision) (auth.Decision, error) {
	var zero auth.Decision
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	d, e := s.config.Resolver.Resolve(ctx)
	if e != nil {
		return zero, auth.ObjectNotFound()
	}
	now, e := s.now()
	if e != nil {
		return zero, e
	}
	fp, e := auth.AuthorizationFingerprint(d)
	// Delegated execution/admission needs a separately specified purpose and
	// source authority contract. This first registration service fails closed.
	if e != nil || len(d.OnBehalfOf()) != 0 || !now.Before(d.AuthenticationExpires()) {
		return zero, auth.ObjectNotFound()
	}
	if before != nil {
		old, e := auth.AuthorizationFingerprint(*before)
		if e != nil || old != fp {
			return zero, auth.ObjectNotFound()
		}
	}
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	return d, nil
}
func scopeFor(d auth.Decision) registrations.Scope {
	return registrations.Scope{Domain: d.AuthorizationDomain(), SubjectID: d.Subject(), ActorID: d.Actor(), ClientID: d.ClientID(), OnBehalfOf: d.OnBehalfOf()}
}
func (s *Service) profile(id, revision shoal.ID) (Profile, error) {
	p, ok := s.profiles[profileKey(id, revision)]
	if !ok {
		return Profile{}, auth.ObjectNotFound()
	}
	return normalizeProfile(p)
}
func sourceIDs(r registrations.Registration) []shoal.ID {
	ids := make([]shoal.ID, len(r.Frozen.Sources))
	for i, p := range r.Frozen.Sources {
		ids[i] = p.ObservationID
	}
	return ids
}
func selectionIDs(v Selection) []shoal.ID {
	ids := make([]shoal.ID, len(v.Sources))
	for i, p := range v.Sources {
		ids[i] = p.ObservationID
	}
	return ids
}
func detachedRegistration(r registrations.Registration) registrations.Registration {
	r.Scope.Domain = bytes.Clone(r.Scope.Domain)
	r.Scope.OnBehalfOf = append([]shoal.ID(nil), r.Scope.OnBehalfOf...)
	r.Frozen.Sources = append([]registrations.SourcePin(nil), r.Frozen.Sources...)
	return r
}

// Builder is immutable operator code. Snapshot every mutable data field without
// treating a valid function-backed Builder as unequal to itself.
func sameProfileData(a, b Profile) bool {
	a.Builder = nil
	b.Builder = nil
	return reflect.DeepEqual(a, b)
}
func cloneMaterials(in []collectorregistry.Material) ([]collectorregistry.Material, error) {
	out := make([]collectorregistry.Material, len(in))
	for i, m := range in {
		var e error
		m.Registration, e = m.Registration.Canonical()
		if e != nil {
			return nil, e
		}
		m.Enrollment.Request, e = m.Enrollment.Request.Canonical()
		if e != nil {
			return nil, e
		}
		if m.Enrollment.Attestation != nil {
			a := *m.Enrollment.Attestation
			m.Enrollment.Attestation = &a
		}
		m.Artifact.Domain = bytes.Clone(m.Artifact.Domain)
		m.Observation.Observation, e = collector.NewObservation(m.Observation.Observation.Config())
		if e != nil {
			return nil, e
		}
		out[i] = m
	}
	return out, nil
}
func (s *Service) authorize(ctx context.Context, d auth.Decision, op auth.Operation, p Profile, ids []shoal.ID) error {
	owned, e := normalizeProfile(p)
	if e != nil {
		return ErrUnavailable
	}
	original, e := normalizeProfile(owned)
	if e != nil {
		return ErrUnavailable
	}
	copiedIDs := append([]shoal.ID(nil), ids...)
	if e = s.config.Authority.Authorize(ctx, d, op, owned, copiedIDs); e != nil {
		return registrationError(e)
	}
	if !sameProfileData(owned, original) || !reflect.DeepEqual(copiedIDs, ids) {
		return ErrUnavailable
	}
	_, e = s.caller(ctx, &d)
	return e
}
func (s *Service) joint(ctx context.Context, d auth.Decision, op auth.Operation, p Profile, r registrations.Registration, record decisionartifacts.Record, registered, requireReady bool) error {
	// Every registration/catalog operation precedes this final material resolution
	// and trusted joint check. The result is not an atomic authorization lease.
	if registered {
		current, e := s.config.Registrations.LookupRequest(ctx, d.AuthorizationDomain(), d.Subject(), r.Frozen.RequestID)
		if gateErr := s.authorize(ctx, d, op, p, sourceIDs(r)); gateErr != nil {
			return gateErr
		}
		if e != nil {
			return registrationError(e)
		}
		if current.ID != r.ID || current.FrozenSHA256 != r.FrozenSHA256 || (requireReady && current.State != registrations.Ready) {
			return auth.ObjectNotFound()
		}
		r = current
	}
	if e := s.authorize(ctx, d, op, p, sourceIDs(r)); e != nil {
		return e
	}
	materials, e := s.config.Materials.Resolve(ctx, p.MaterialPurposeID, sourceIDs(r))
	if gateErr := s.authorize(ctx, d, op, p, sourceIDs(r)); gateErr != nil {
		return gateErr
	}
	if e != nil {
		return registrationError(e)
	}
	if e = validateRecord(ctx, p, r.Frozen, record, materials); e != nil {
		return registrationError(e)
	}
	raw, e := decisionartifacts.EncodeRecord(record)
	if e != nil {
		return ErrUnavailable
	}
	owned, e := decisionartifacts.DecodeRecord(raw)
	if e != nil {
		return ErrUnavailable
	}
	profile, e := normalizeProfile(p)
	if e != nil {
		return ErrUnavailable
	}
	copy := detachedRegistration(r)
	original := detachedRegistration(copy)
	originalProfile, e := normalizeProfile(profile)
	if e != nil {
		return ErrUnavailable
	}
	materialCopy, e := cloneMaterials(materials)
	if e != nil {
		return ErrUnavailable
	}
	originalMaterials, e := cloneMaterials(materialCopy)
	if e != nil {
		return ErrUnavailable
	}
	if e = s.config.Authority.Verify(ctx, d, op, profile, copy, owned, materialCopy); e != nil {
		return registrationError(e)
	}
	after, e := decisionartifacts.EncodeRecord(owned)
	if e != nil || !bytes.Equal(raw, after) || !reflect.DeepEqual(copy, original) || !sameProfileData(profile, originalProfile) || !reflect.DeepEqual(materialCopy, originalMaterials) {
		return ErrUnavailable
	}
	_, e = s.caller(ctx, &d)
	return e
}
func correlationID(d auth.Decision, key []byte, selectionSHA string) shoal.ID {
	scope := scopeFor(d)
	delegates := make([][]byte, len(scope.OnBehalfOf))
	for i, id := range scope.OnBehalfOf {
		delegates[i] = []byte(id)
	}
	b, _ := json.Marshal(struct {
		Domain, Subject, Actor, Client []byte
		Delegates                      [][]byte
		KeySHA256, SelectionSHA256     string
	}{scope.Domain, []byte(scope.SubjectID), []byte(scope.ActorID), []byte(scope.ClientID), delegates, hashRegistration(key), selectionSHA})
	return shoal.ID("registration-correlation:" + hashRegistration(b))
}

// frozenPreparation authorizes the OLD selection before reading retained bytes
// and after all IO, including when the caller supplied a conflicting selection.
func (s *Service) frozenPreparation(ctx context.Context, d auth.Decision, key []byte, old registrations.Registration) (registrations.Registration, Profile, decisionartifacts.Record, []byte, error) {
	var empty decisionartifacts.Record
	p, e := s.profile(old.Frozen.ProfileID, old.Frozen.ProfileRevisionID)
	if e != nil {
		return registrations.Registration{}, p, empty, nil, e
	}
	if e = s.authorize(ctx, d, auth.OperationIngest, p, sourceIDs(old)); e != nil {
		return registrations.Registration{}, p, empty, nil, e
	}
	current, raw, readErr := s.config.Registrations.LoadPrepared(ctx, scopeFor(d), key)
	if e = s.authorize(ctx, d, auth.OperationIngest, p, sourceIDs(old)); e != nil {
		return registrations.Registration{}, p, empty, nil, e
	}
	if readErr != nil {
		return registrations.Registration{}, p, empty, nil, registrationError(readErr)
	}
	if current.ID != old.ID || current.FrozenSHA256 != old.FrozenSHA256 {
		return registrations.Registration{}, p, empty, nil, ErrUnavailable
	}
	record, e := decisionartifacts.DecodeRecord(raw)
	if e != nil {
		return registrations.Registration{}, p, empty, nil, ErrUnavailable
	}
	if e = s.joint(ctx, d, auth.OperationIngest, p, current, record, false, false); e != nil {
		return registrations.Registration{}, p, empty, nil, e
	}
	return current, p, record, raw, nil
}

// Register admits exact collector-backed inputs under an operator profile. An
// existing key is read before building; retries recover its original bytes and
// times. A changed grant fingerprint cannot resume preparing via the existing
// catalog admission path. It never impersonates the old caller or rebuilds the
// preparation. An authorized ready replay can use refreshed current grants.
func (s *Service) Register(ctx context.Context, key []byte, selection Selection) (result registrations.Registration, err error) {
	if s == nil || len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return result, invalidRegistration()
	}
	key = bytes.Clone(key)
	selection, e := normalizeSelection(selection)
	if e != nil {
		return result, e
	}
	p, e := s.profile(selection.ProfileID, selection.ProfileRevisionID)
	if e != nil {
		return result, e
	}
	d, e := s.caller(ctx, nil)
	if e != nil {
		return result, e
	}
	if e = s.authorize(ctx, d, auth.OperationIngest, p, selectionIDs(selection)); e != nil {
		return result, e
	}
	scope := scopeFor(d)
	old, e := s.config.Registrations.ReadByKey(ctx, scope, key)
	if gateErr := s.authorize(ctx, d, auth.OperationIngest, p, selectionIDs(selection)); gateErr != nil {
		return result, gateErr
	}
	if e != nil && !errors.Is(e, registrations.ErrNotFound) {
		return result, registrationError(e)
	}
	var record decisionartifacts.Record
	var raw []byte
	var frozen registrations.Frozen
	if errors.Is(e, registrations.ErrNotFound) {
		materials, e := s.config.Materials.Resolve(ctx, p.MaterialPurposeID, selectionIDs(selection))
		if gateErr := s.authorize(ctx, d, auth.OperationIngest, p, selectionIDs(selection)); gateErr != nil {
			return result, gateErr
		}
		if e != nil {
			return result, registrationError(e)
		}
		accepted, e := s.now()
		if e != nil {
			return result, e
		}
		record, frozen, e = buildRecord(ctx, d, p, selection, materials, accepted, correlationID(d, key, selectionDigest(selection)))
		if e != nil {
			return result, registrationError(e)
		}
		raw, e = decisionartifacts.EncodeRecord(record)
		if e != nil {
			return result, registrationError(e)
		}
		candidate := registrations.Registration{Scope: scope, Frozen: frozen, State: registrations.Preparing}
		if e = s.joint(ctx, d, auth.OperationIngest, p, candidate, record, false, false); e != nil {
			return result, e
		}
	} else {
		var originalProfile Profile
		old, originalProfile, record, raw, e = s.frozenPreparation(ctx, d, key, old)
		if e != nil {
			return result, e
		}
		if old.Frozen.SelectionSHA256 != selectionDigest(selection) || old.Frozen.ProfileID != p.ID || old.Frozen.ProfileRevisionID != p.RevisionID {
			return result, ErrConflict
		}
		p = originalProfile
		frozen = old.Frozen
		if old.State == registrations.Ready {
			return s.read(ctx, d, old.Frozen.RequestID, auth.OperationIngest)
		}
	}

	// From the first Begin call, even an apparently read-like failure may follow
	// durable blob/primary/alias writes. Do not infer rollback from an HTTP code.
	defer func() {
		if err != nil {
			result = registrations.Registration{}
			err = errors.Join(ErrIndeterminate, err)
		}
	}()
	r, e := s.config.Registrations.Begin(ctx, scope, key, frozen, raw)
	if e != nil {
		originalErr := e
		// One bounded reconciliation handles two first callers that selected the
		// same inputs but independently sampled different server acceptance times.
		winner, readErr := s.config.Registrations.ReadByKey(ctx, scope, key)
		if readErr != nil {
			if gateErr := s.authorize(ctx, d, auth.OperationIngest, p, selectionIDs(selection)); gateErr != nil {
				return result, gateErr
			}
			return result, registrationError(originalErr)
		}
		var winnerProfile Profile
		winner, winnerProfile, record, raw, e = s.frozenPreparation(ctx, d, key, winner)
		if e != nil {
			return result, e
		}
		if !errors.Is(originalErr, registrations.ErrConflict) || winner.Frozen.SelectionSHA256 != selectionDigest(selection) || winner.Frozen.ProfileID != p.ID || winner.Frozen.ProfileRevisionID != p.RevisionID {
			return result, registrationError(originalErr)
		}
		p = winnerProfile
		frozen = winner.Frozen
		r, e = s.config.Registrations.Begin(ctx, scope, key, frozen, raw)
		if e != nil {
			if gateErr := s.joint(ctx, d, auth.OperationIngest, p, winner, record, false, false); gateErr != nil {
				return result, gateErr
			}
			return result, registrationError(e)
		}
	}
	if r.State == registrations.Ready {
		return s.read(ctx, d, r.Frozen.RequestID, auth.OperationIngest)
	}

	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil || fp.String() != r.Frozen.AuthorizationFingerprint {
		return result, auth.ObjectNotFound()
	}
	session, e := s.catalog(p, r, auth.OperationIngest, true)
	if e != nil {
		return result, e
	}
	retainErr := session.catalog.Retain(ctx, record)
	if gateErr := s.authorize(ctx, d, auth.OperationIngest, p, sourceIDs(r)); gateErr != nil {
		return result, gateErr
	}
	if retainErr != nil {
		return result, registrationError(retainErr)
	}
	guard := func(checkCtx context.Context, registered registrations.Registration, prepared []byte) error {
		if registered.FrozenSHA256 != r.FrozenSHA256 || hashRegistration(prepared) != r.Frozen.RecordSHA256 {
			return ErrUnavailable
		}
		current, e := s.caller(checkCtx, &d)
		if e != nil {
			return e
		}
		fresh, e := s.catalog(p, registered, auth.OperationIngest, true)
		if e != nil {
			return e
		}
		if _, e = fresh.catalog.LoadAuthorized(checkCtx, current, registered.Frozen.RequestID); e != nil {
			return registrationError(e)
		}
		if fresh.record == nil {
			return ErrUnavailable
		}
		return s.joint(checkCtx, current, auth.OperationIngest, p, registered, *fresh.record, true, false)
	}
	ready, e := s.config.Registrations.MarkReady(ctx, scope, key, r.FrozenSHA256, guard)
	if gateErr := s.authorize(ctx, d, auth.OperationIngest, p, sourceIDs(r)); gateErr != nil {
		return result, gateErr
	}
	if e != nil {
		return result, registrationError(e)
	}
	if e = s.joint(ctx, d, auth.OperationIngest, p, ready, record, true, true); e != nil {
		return result, e
	}
	return ready, nil
}

// Read discloses a ready registration under current subject-scoped authority.
// Preparing aliases deliberately look absent outside private admission staging.
func (s *Service) Read(ctx context.Context, request shoal.ID) (registrations.Registration, error) {
	if s == nil {
		return registrations.Registration{}, ErrUnavailable
	}
	d, e := s.caller(ctx, nil)
	if e != nil {
		return registrations.Registration{}, e
	}
	return s.read(ctx, d, request, auth.OperationRead)
}
func (s *Service) read(ctx context.Context, d auth.Decision, request shoal.ID, op auth.Operation) (registrations.Registration, error) {
	r, _, e := s.loadReady(ctx, d, request, op)
	return r, e
}
func (s *Service) loadReady(ctx context.Context, d auth.Decision, request shoal.ID, op auth.Operation) (registrations.Registration, decisionservice.Bundle, error) {
	var zero registrations.Registration
	var empty decisionservice.Bundle
	r, e := s.config.Registrations.LookupRequest(ctx, d.AuthorizationDomain(), d.Subject(), request)
	if _, gateErr := s.caller(ctx, &d); gateErr != nil {
		return zero, empty, gateErr
	}
	if e != nil || r.State != registrations.Ready {
		return zero, empty, auth.ObjectNotFound()
	}
	p, e := s.profile(r.Frozen.ProfileID, r.Frozen.ProfileRevisionID)
	if e != nil {
		return zero, empty, e
	}
	if e = s.authorize(ctx, d, op, p, sourceIDs(r)); e != nil {
		return zero, empty, e
	}
	session, e := s.catalog(p, r, op, false)
	if e != nil {
		return zero, empty, e
	}
	bundle, e := session.catalog.LoadAuthorized(ctx, d, request)
	if gateErr := s.authorize(ctx, d, op, p, sourceIDs(r)); gateErr != nil {
		return zero, empty, gateErr
	}
	if e != nil {
		return zero, empty, registrationError(e)
	}
	if session.record == nil {
		return zero, empty, ErrUnavailable
	}
	if e = s.joint(ctx, d, op, p, r, *session.record, true, true); e != nil {
		return zero, empty, e
	}
	return r, bundle, nil
}

type readyArtifacts struct{ s *Service }

// Artifacts exposes only ready requests. No preparing-admission catalog escapes
// the service, and every read uses a fresh private verification session.
func (s *Service) Artifacts() decisionservice.Artifacts { return readyArtifacts{s} }
func (a readyArtifacts) LoadAuthorized(ctx context.Context, supplied auth.Decision, request shoal.ID) (decisionservice.Bundle, error) {
	if a.s == nil {
		return decisionservice.Bundle{}, ErrUnavailable
	}
	d, e := a.s.caller(ctx, &supplied)
	if e != nil {
		return decisionservice.Bundle{}, e
	}
	_, bundle, e := a.s.loadReady(ctx, d, request, auth.OperationInvoke)
	return bundle, e
}

type catalogSession struct {
	s         *Service
	profile   Profile
	expected  registrations.Registration
	operation auth.Operation
	preparing bool
	catalog   *decisionartifacts.Catalog
	record    *decisionartifacts.Record
}

func (s *Service) catalog(p Profile, r registrations.Registration, op auth.Operation, preparing bool) (*catalogSession, error) {
	a := &catalogSession{s: s, profile: p, expected: detachedRegistration(r), operation: op, preparing: preparing}
	c, e := decisionartifacts.New(decisionartifacts.Config{Backend: s.config.ArtifactBackend, Resolver: s.config.Resolver, Authority: a, Clock: s.config.Clock, Visibility: s.config.Visibility})
	if e != nil {
		return nil, e
	}
	a.catalog = c
	return a, nil
}
func (a *catalogSession) AuthorizeRequest(ctx context.Context, d auth.Decision, id shoal.ID) error {
	if id != a.expected.Frozen.RequestID {
		return auth.ObjectNotFound()
	}
	r, e := a.s.config.Registrations.LookupRequest(ctx, d.AuthorizationDomain(), d.Subject(), id)
	if gateErr := a.s.authorize(ctx, d, a.operation, a.profile, sourceIDs(a.expected)); gateErr != nil {
		return gateErr
	}
	if e != nil || r.ID != a.expected.ID || r.FrozenSHA256 != a.expected.FrozenSHA256 || (!a.preparing && r.State != registrations.Ready) {
		return auth.ObjectNotFound()
	}
	return a.s.authorize(ctx, d, a.operation, a.profile, sourceIDs(r))
}
func (a *catalogSession) Verify(ctx context.Context, d auth.Decision, r decisionartifacts.Record) error {
	raw, e := decisionartifacts.EncodeRecord(r)
	if e != nil || hashRegistration(raw) != a.expected.Frozen.RecordSHA256 || len(raw) != a.expected.Frozen.RecordBytes {
		return auth.ObjectNotFound()
	}
	if e = a.s.joint(ctx, d, a.operation, a.profile, a.expected, r, true, !a.preparing); e != nil {
		return e
	}
	owned, e := decisionartifacts.DecodeRecord(raw)
	if e != nil {
		return ErrUnavailable
	}
	a.record = &owned
	return nil
}
