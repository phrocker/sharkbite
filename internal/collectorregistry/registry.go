// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package collectorregistry is the engine-backed registry of external
// collectors: operator provisioning, collector enrollment, raw artifact
// references and extractor-versioned observations.
//
// Authority comes only from provisioning: Provision, a trusted Go API, or
// AdminProvision, its HTTP counterpart behind auth.OperationCollectorAdmin
// (admin.go). Every provisioning and revocation is recorded in the
// collector's change log in the same mutation. An enrollment that requests authority beyond provisioning is refused and
// nothing is written. Artifacts and observations are append-only; a new
// extractor version produces a new observation over the same artifact.
// Revoke bumps the collector's generation, and every artifact and observation
// recorded under an earlier generation reads as quarantined. Quarantine is
// computed at read time; stored rows are never rewritten.
package collectorregistry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorattest"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Table holds every collector row.
const Table = "_shoal_collectors"

const (
	maxStoredBytes = 256 * 1024
	maxCASAttempts = 8
)

var (
	ErrConflict    = shoal.NewError(shoal.ErrorConflict, "collector record conflict")
	ErrUnavailable = shoal.NewError(shoal.ErrorUnavailable, "collector registry unavailable")
)

func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid collector request")
}
func unauthorized() error {
	return shoal.NewError(shoal.ErrorUnauthorized, "authentication required")
}
func denied(reason string) error { return fmt.Errorf("%w: %s", api.ErrPermissionDenied, reason) }
func indeterminate(cause ...error) error {
	return errors.Join(append([]error{api.ErrIndeterminate}, cause...)...)
}

type Config struct {
	Backend     decisionstore.CAS
	Resolver    auth.Resolver
	Attestation collectorattest.Set
	Clock       func() time.Time
	Visibility  []byte
}

type Registry struct{ config Config }

func New(c Config) (*Registry, error) {
	if absent(c.Backend) || absent(c.Resolver) || c.Clock == nil || len(c.Visibility) > 4096 {
		return nil, invalid()
	}
	c.Visibility = bytes.Clone(c.Visibility)
	return &Registry{c}, nil
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

// Provisioning is the operator's grant to one authenticated principal.
type Provisioning struct {
	CollectorID        shoal.ID
	Subject            shoal.ID
	ClientID           shoal.ID
	Domain             []byte
	AuthorityPolicyIDs []shoal.ID
	Control            collector.Control
	Mode               collector.Mode
}

// Enrollment is the immutable record of one accepted enrollment.
type Enrollment struct {
	ID            shoal.ID
	Generation    int64
	KeyDigest     string
	RequestDigest string
	Request       collector.EnrollRequest
	Attestation   *collector.AttestationResult
	EnrolledAt    time.Time
}

// ArtifactRecord is an immutable raw artifact reference.
type ArtifactRecord struct {
	CollectorID shoal.ID
	// Domain is the collector's authorization domain when written. Reads
	// authorize against it, never against a later registration.
	Domain       []byte
	Ref          collector.ArtifactRef
	Generation   int64
	EnrollmentID shoal.ID
	ReceivedAt   time.Time
}

// ObservationRecord is an immutable observation plus read-time status.
type ObservationRecord struct {
	Observation    collector.Observation
	ArtifactDigest string
	Generation     int64
	EnrollmentID   shoal.ID
	ReceivedAt     time.Time
	// Quarantined is computed on read and never stored.
	Quarantined bool
}

type registrationRow struct {
	Registration collector.Registration
	Enrollment   *Enrollment
}
type enrollmentRow struct {
	CollectorID shoal.ID
	Enrollment  Enrollment
	// Prior is the enrollment this one replaces, empty for the first. An
	// enrollment applies only while Prior is still current.
	Prior shoal.ID
}
type observationRow struct {
	ID           shoal.ID
	Domain       []byte
	Config       collector.ObservationConfig
	Generation   int64
	EnrollmentID shoal.ID
	ReceivedAt   time.Time
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Registry) coordinate(kind string, parts ...string) allocator.Coordinate {
	return allocator.Coordinate{Row: []byte(kind + ":" + digest(append([]string{kind}, parts...)...)), Family: []byte("c"), Qualifier: []byte(kind), Visibility: bytes.Clone(r.config.Visibility)}
}
func (r *Registry) registrationCoordinate(id shoal.ID) allocator.Coordinate {
	return r.coordinate("registration", string(id))
}
func (r *Registry) enrollmentCoordinate(id shoal.ID, keyDigest string) allocator.Coordinate {
	return r.coordinate("enrollment", string(id), keyDigest)
}

// Artifacts are keyed per generation, so a later generation never sees or
// collides with an earlier generation's record.
func (r *Registry) artifactCoordinate(id shoal.ID, generation int64, artifact shoal.ID) allocator.Coordinate {
	return r.coordinate("artifact", string(id), fmt.Sprint(generation), string(artifact))
}
func (r *Registry) appliedCoordinate(id shoal.ID, keyDigest string) allocator.Coordinate {
	return r.coordinate("applied", string(id), keyDigest)
}
func (r *Registry) observationCoordinate(id shoal.ID) allocator.Coordinate {
	return r.coordinate("observation", string(id))
}
func sameCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}

type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

func encode(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(b)
	out, e := json.Marshal(envelope{1, b, hex.EncodeToString(sum[:])})
	if e != nil || len(out) > maxStoredBytes {
		return nil, errors.New("collector record exceeds bound")
	}
	return out, nil
}

// decode accepts only the canonical encoding it would itself produce.
func decode(raw []byte, out any) error {
	var env envelope
	strict := func(b []byte, v any) error {
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if e := d.Decode(v); e != nil {
			return e
		}
		if _, e := d.Token(); e != io.EOF {
			return errors.New("trailing data")
		}
		return nil
	}
	if len(raw) > maxStoredBytes || strict(raw, &env) != nil || env.Schema != 1 {
		return ErrUnavailable
	}
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.Checksum || strict(env.Payload, out) != nil {
		return ErrUnavailable
	}
	canonical, e := encode(out)
	if e != nil || !bytes.Equal(canonical, raw) {
		return ErrUnavailable
	}
	return nil
}

func (r *Registry) now() (time.Time, error) {
	now := r.config.Clock().Round(0).UTC()
	if now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
		return time.Time{}, ErrUnavailable
	}
	return now, nil
}

// read returns the newest cell at coord, or NotFound.
func (r *Registry) read(ctx context.Context, coord allocator.Coordinate) (allocator.Cell, error) {
	cells, e := r.config.Backend.ReadExact(ctx, []allocator.Coordinate{coord})
	if e != nil {
		return allocator.Cell{}, ErrUnavailable
	}
	if len(cells) == 0 {
		return allocator.Cell{}, auth.ObjectNotFound()
	}
	if len(cells) != 1 || !sameCoordinate(cells[0].Coordinate, coord) || cells[0].Timestamp <= 0 {
		return allocator.Cell{}, ErrUnavailable
	}
	return cells[0], nil
}

// putImmutable writes value at timestamp 1 if coord is absent and returns the
// stored bytes, which may belong to an earlier writer.
func (r *Registry) putImmutable(ctx context.Context, coord allocator.Coordinate, value []byte) ([]byte, error) {
	status, writeErr := r.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{{Coordinate: coord, Absent: true}}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: 1, Value: value}}})
	if writeErr == nil && status == allocator.StatusAccepted {
		return value, nil
	}
	// A lost acknowledgement can hide a successful write; reconcile by reading.
	cell, readErr := r.read(ctx, coord)
	if readErr == nil && cell.Timestamp == 1 {
		return cell.Value, nil
	}
	return nil, indeterminate(writeErr, readErr)
}

func (r *Registry) readRegistration(ctx context.Context, id shoal.ID) (registrationRow, allocator.Cell, error) {
	cell, e := r.read(ctx, r.registrationCoordinate(id))
	if e != nil {
		return registrationRow{}, cell, e
	}
	var row registrationRow
	if decode(cell.Value, &row) != nil || row.Registration.CollectorID != id || row.Registration.Generation <= 0 {
		return registrationRow{}, cell, ErrUnavailable
	}
	if _, e := row.Registration.Canonical(); e != nil {
		return registrationRow{}, cell, ErrUnavailable
	}
	if (row.Enrollment != nil) != (row.Registration.State == collector.Enrolled) {
		return registrationRow{}, cell, ErrUnavailable
	}
	return row, cell, nil
}

// updateRegistration applies change under compare-and-set, retrying a bounded
// number of times when another writer moved the row.
func (r *Registry) updateRegistration(ctx context.Context, id shoal.ID, create bool, change func(*registrationRow, bool) (bool, error)) (registrationRow, error) {
	row, _, e := r.updateRegistrationAudited(ctx, id, create, change, nil)
	return row, e
}

// updateRegistrationAudited is updateRegistration that, when record is set,
// writes the change record describes to the collector's change log in the
// same one-row mutation as the registration write (admin.go), so a change is
// never durable without its entry or the reverse. It reports whether it
// wrote a change.
func (r *Registry) updateRegistrationAudited(ctx context.Context, id shoal.ID, create bool, change func(*registrationRow, bool) (bool, error), record func(registrationRow) (Change, error)) (registrationRow, bool, error) {
	coord := r.registrationCoordinate(id)
	for range maxCASAttempts {
		current, cell, e := r.readRegistration(ctx, id)
		exists := e == nil
		if e != nil && !(create && shoal.IsErrorCode(e, shoal.ErrorNotFound)) {
			return registrationRow{}, false, e
		}
		next := current
		if exists {
			next = cloneRow(current)
		}
		write, e := change(&next, exists)
		if e != nil || !write {
			return next, false, e
		}
		canonical, e := next.Registration.Canonical()
		if e != nil {
			return registrationRow{}, false, invalid()
		}
		next.Registration = canonical
		value, e := encode(next)
		if e != nil {
			return registrationRow{}, false, invalid()
		}
		condition := allocator.Condition{Coordinate: coord, Absent: !exists}
		timestamp := int64(1)
		if exists {
			condition.Value, condition.Timestamp, condition.TimestampSet = cell.Value, cell.Timestamp, true
			timestamp = cell.Timestamp + 1
		}
		mutation := allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{condition}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: timestamp, Value: value}}}
		var entry auditWrite
		if record != nil {
			if entry, e = r.prepareChange(id, next, record); e != nil {
				return registrationRow{}, false, e
			}
			mutation.Conditions = append(mutation.Conditions, entry.conditions...)
			mutation.Updates = append(mutation.Updates, entry.updates...)
		}
		status, writeErr := r.config.Backend.CompareAndMutate(ctx, mutation)
		if writeErr == nil && status == allocator.StatusAccepted {
			return next, true, nil
		}
		// With an entry, this attempt's own uniquely qualified cell is proof
		// that its mutation committed, whatever the registration row has
		// done since (an enrollment may already have moved it on).
		if record != nil {
			if applied, checkErr := r.changeApplied(ctx, id, entry); checkErr == nil && applied {
				return next, true, nil
			}
		}
		stored, readErr := r.read(ctx, coord)
		if readErr == nil && stored.Timestamp == timestamp && bytes.Equal(stored.Value, value) {
			// An identical registration write is not proof that this
			// mutation applied when another writer could have produced the
			// same bytes; with an entry, this attempt's own cell decides.
			if record == nil {
				return next, true, nil
			}
			if applied, checkErr := r.changeApplied(ctx, id, entry); checkErr == nil && applied {
				return next, true, nil
			} else if checkErr != nil && !(writeErr == nil && status == allocator.StatusRejected) {
				// A definite rejection did not apply; only an unknown
				// outcome with an unreadable own cell needs reconciling.
				return registrationRow{}, false, indeterminate(writeErr, checkErr)
			}
		}
		if writeErr != nil || status != allocator.StatusRejected {
			return registrationRow{}, false, indeterminate(writeErr, readErr)
		}
	}
	return registrationRow{}, false, ErrConflict
}

func cloneRow(row registrationRow) registrationRow {
	row.Registration.Domain = bytes.Clone(row.Registration.Domain)
	row.Registration.AuthorityPolicyIDs = slices.Clone(row.Registration.AuthorityPolicyIDs)
	if row.Enrollment != nil {
		e := *row.Enrollment
		row.Enrollment = &e
	}
	return row
}

// Provision grants a principal the right to act as a collector with the
// given authority ceiling, control and mode. It is the only way authority is
// assigned. A collector's identity (domain, subject, client) is fixed for its
// lifetime: provisioning an existing collector ID with a different identity is
// a conflict, even after revocation; use a new collector ID. Re-provisioning
// an active registration with different terms is a conflict; revoke first.
// Provisioning a revoked registration reactivates it at its new generation,
// possibly with new terms, leaving everything from earlier generations
// quarantined.
func (r *Registry) Provision(ctx context.Context, p Provisioning) (collector.Registration, error) {
	by, e := r.operator()
	if e != nil {
		return collector.Registration{}, e
	}
	row, _, e := r.provision(ctx, p, 0, by)
	return row.Registration, e
}

// provision implements Provision and, with generation set, AdminProvision.
// generation 0 keeps Provision's Go API semantics. A positive generation is
// the one the caller expects the registration to hold afterwards, so the
// request alone decides the outcome: it creates only at generation 1,
// reactivates a revoked registration only at its current generation, and is
// a no-op only for an active registration at that generation with the same
// terms. Anything else is a conflict, never a merge, so a retry cannot undo a
// revocation that happened in between.
func (r *Registry) provision(ctx context.Context, p Provisioning, generation int64, by attribution) (registrationRow, bool, error) {
	want, e := collector.Registration{CollectorID: p.CollectorID, Subject: p.Subject, ClientID: p.ClientID, Domain: p.Domain, AuthorityPolicyIDs: p.AuthorityPolicyIDs, Control: p.Control, Mode: p.Mode, Generation: 1, State: collector.Provisioned}.Canonical()
	if e != nil || generation < 0 {
		return registrationRow{}, false, invalid()
	}
	return r.updateRegistrationAudited(ctx, p.CollectorID, true, func(row *registrationRow, exists bool) (bool, error) {
		if !exists {
			if generation > 1 {
				return false, ErrConflict
			}
			*row = registrationRow{Registration: want}
			return true, nil
		}
		current := row.Registration
		if current.Subject != want.Subject || current.ClientID != want.ClientID || !bytes.Equal(current.Domain, want.Domain) {
			return false, ErrConflict
		}
		if generation != 0 && current.Generation != generation {
			return false, ErrConflict
		}
		if current.State == collector.Revoked {
			next := want
			next.Generation = current.Generation
			*row = registrationRow{Registration: next}
			return true, nil
		}
		same := current.Subject == want.Subject && current.ClientID == want.ClientID && bytes.Equal(current.Domain, want.Domain) && slices.Equal(current.AuthorityPolicyIDs, want.AuthorityPolicyIDs) && current.Control == want.Control && current.Mode == want.Mode
		if !same {
			return false, ErrConflict
		}
		return false, nil
	}, by.record(ChangeProvision))
}

// Revoke ends a collector's current generation. Everything it recorded so far
// reads as quarantined and further submissions are refused until it is
// provisioned and enrolled again.
func (r *Registry) Revoke(ctx context.Context, id shoal.ID) (collector.Registration, error) {
	by, e := r.operator()
	if e != nil {
		return collector.Registration{}, e
	}
	row, _, e := r.revoke(ctx, id, 0, by, nil)
	return row.Registration, e
}

// revoke implements Revoke and, with generation set, AdminRevoke: a positive
// generation is the one being revoked. Revoking it again is a no-op; revoking
// any other generation is a conflict. permit, when set, re-checks the row the
// write is computed from.
func (r *Registry) revoke(ctx context.Context, id shoal.ID, generation int64, by attribution, permit func(collector.Registration) error) (registrationRow, bool, error) {
	if generation < 0 {
		return registrationRow{}, false, invalid()
	}
	return r.updateRegistrationAudited(ctx, id, false, func(row *registrationRow, _ bool) (bool, error) {
		if permit != nil {
			if e := permit(row.Registration); e != nil {
				return false, e
			}
		}
		if row.Registration.State == collector.Revoked {
			if generation != 0 && row.Registration.Generation != generation+1 {
				return false, ErrConflict
			}
			return false, nil
		}
		if generation != 0 && row.Registration.Generation != generation {
			return false, ErrConflict
		}
		row.Registration.State = collector.Revoked
		row.Registration.Generation++
		row.Enrollment = nil
		return true, nil
	}, by.record(ChangeRevoke))
}

// Registration is a trusted operator read with no caller check.
func (r *Registry) Registration(ctx context.Context, id shoal.ID) (collector.Registration, error) {
	row, _, e := r.readRegistration(ctx, id)
	return row.Registration, e
}

// caller resolves the trusted decision and requires op in its own domain.
func (r *Registry) caller(ctx context.Context, op auth.Operation) (auth.Decision, auth.Fingerprint, time.Time, error) {
	if ctx.Err() != nil {
		return auth.Decision{}, auth.Fingerprint{}, time.Time{}, ErrUnavailable
	}
	d, e := r.config.Resolver.Resolve(ctx)
	if e != nil {
		return d, auth.Fingerprint{}, time.Time{}, unauthorized()
	}
	now, e := r.now()
	if e != nil {
		return d, auth.Fingerprint{}, time.Time{}, e
	}
	if d.Authorize(op, auth.ResourceRequest{AuthorizationDomain: d.AuthorizationDomain()}, now) != nil {
		return d, auth.Fingerprint{}, time.Time{}, unauthorized()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return d, auth.Fingerprint{}, time.Time{}, unauthorized()
	}
	return d, fp, now, nil
}

// recheck confirms the caller's authorization did not change during the call.
func (r *Registry) recheck(ctx context.Context, op auth.Operation, before auth.Fingerprint) error {
	_, after, _, e := r.caller(ctx, op)
	if e != nil || after != before {
		return unauthorized()
	}
	return nil
}

// bound reports whether d is the principal provisioned for the registration.
// Delegated decisions never act as a collector.
func bound(reg collector.Registration, d auth.Decision) bool {
	return bytes.Equal(reg.Domain, d.AuthorizationDomain()) && reg.Subject == d.Subject() && reg.ClientID == d.ClientID() && len(d.OnBehalfOf()) == 0
}

// boundRegistration reads id's registration and hides it from anyone but its
// provisioned principal.
func (r *Registry) boundRegistration(ctx context.Context, id shoal.ID, d auth.Decision) (registrationRow, error) {
	row, _, e := r.readRegistration(ctx, id)
	if e != nil {
		return registrationRow{}, e
	}
	if !bound(row.Registration, d) {
		return registrationRow{}, auth.ObjectNotFound()
	}
	return row, nil
}

func currentEnrollment(row registrationRow) shoal.ID {
	if row.Enrollment == nil {
		return ""
	}
	return row.Enrollment.ID
}

// Enroll records a collector's enrollment. key is the enrollment's
// idempotency key: the same key and request return the original receipt, a
// different request under the same key conflicts, and a key used under an
// earlier generation is refused, so a captured attestation statement cannot
// be replayed after re-provisioning.
func (r *Registry) Enroll(ctx context.Context, key []byte, req collector.EnrollRequest) (Enrollment, error) {
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return Enrollment{}, invalid()
	}
	req, e := req.Canonical()
	if e != nil {
		return Enrollment{}, invalid()
	}
	requestDigest, e := req.Digest()
	if e != nil {
		return Enrollment{}, invalid()
	}
	d, fp, now, e := r.caller(ctx, auth.OperationIngest)
	if e != nil {
		return Enrollment{}, e
	}
	row, e := r.boundRegistration(ctx, req.CollectorID, d)
	if e != nil {
		return Enrollment{}, e
	}
	if row.Registration.State == collector.Revoked {
		return Enrollment{}, denied("collector revoked")
	}
	keyDigest := digest("collector-enroll-key-v1", string(key))
	coord := r.enrollmentCoordinate(req.CollectorID, keyDigest)
	cell, e := r.read(ctx, coord)
	var record enrollmentRow
	switch {
	case e == nil:
		if decode(cell.Value, &record) != nil {
			return Enrollment{}, ErrUnavailable
		}
	case shoal.IsErrorCode(e, shoal.ErrorNotFound):
		// Refuse, never trim: nothing is written for excess authority.
		if !row.Registration.Permits(req.RequestedAuthorityPolicyIDs) {
			return Enrollment{}, denied("requested authority exceeds provisioning")
		}
		var attestation *collector.AttestationResult
		if req.Attestation != nil {
			result, e := r.config.Attestation.Evaluate(ctx, *req.Attestation, collectorattest.Expectation{CollectorID: req.CollectorID, Nonce: collector.EnrollNonce(req.CollectorID, key), Now: now})
			if e != nil {
				return Enrollment{}, denied("attestation refused")
			}
			attestation = &result
		}
		generation := row.Registration.Generation
		enrollment := Enrollment{ID: shoal.ID("enrollment:" + digest("collector-enrollment-v1", string(req.CollectorID), fmt.Sprint(generation), keyDigest)), Generation: generation, KeyDigest: keyDigest, RequestDigest: requestDigest, Request: req, Attestation: attestation, EnrolledAt: now}
		record = enrollmentRow{CollectorID: req.CollectorID, Enrollment: enrollment, Prior: currentEnrollment(row)}
		value, e := encode(record)
		if e != nil {
			return Enrollment{}, invalid()
		}
		stored, e := r.putImmutable(ctx, coord, value)
		if e != nil {
			return Enrollment{}, e
		}
		if decode(stored, &record) != nil {
			return Enrollment{}, indeterminate(ErrUnavailable)
		}
	default:
		return Enrollment{}, e
	}
	if record.CollectorID != req.CollectorID || record.Enrollment.KeyDigest != keyDigest {
		return Enrollment{}, ErrUnavailable
	}
	if record.Enrollment.RequestDigest != requestDigest || record.Enrollment.Generation != row.Registration.Generation {
		return Enrollment{}, ErrConflict
	}
	enrollment := record.Enrollment
	// An enrollment applied and later superseded still returns its receipt.
	if cell, e := r.read(ctx, r.appliedCoordinate(req.CollectorID, keyDigest)); e == nil {
		if cell.Timestamp != 1 || shoal.ID(cell.Value) != enrollment.ID {
			return Enrollment{}, ErrUnavailable
		}
		return enrollment, r.recheck(ctx, auth.OperationIngest, fp)
	} else if !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		return Enrollment{}, e
	}
	// Superseding marks the current enrollment applied first, so its own
	// retries keep returning its receipt.
	if current, _, e := r.readRegistration(ctx, req.CollectorID); e != nil {
		return Enrollment{}, indeterminate(e)
	} else if current.Enrollment != nil && current.Enrollment.ID == record.Prior {
		if e := r.markApplied(ctx, req.CollectorID, *current.Enrollment); e != nil {
			return Enrollment{}, indeterminate(e)
		}
	}
	_, e = r.updateRegistration(ctx, req.CollectorID, false, func(row *registrationRow, _ bool) (bool, error) {
		switch {
		case !bound(row.Registration, d) || row.Registration.Generation != enrollment.Generation || row.Registration.State == collector.Revoked:
			return false, ErrConflict
		case currentEnrollment(*row) == enrollment.ID:
			return false, nil
		case currentEnrollment(*row) != record.Prior:
			// A later enrollment superseded the one this key would apply.
			return false, ErrConflict
		}
		row.Registration.State = collector.Enrolled
		e := enrollment
		row.Enrollment = &e
		return true, nil
	})
	if e != nil {
		if errors.Is(e, api.ErrIndeterminate) || shoal.IsErrorCode(e, shoal.ErrorConflict) {
			return Enrollment{}, e
		}
		return Enrollment{}, indeterminate(e)
	}
	if e = r.recheck(ctx, auth.OperationIngest, fp); e != nil {
		return Enrollment{}, indeterminate(e)
	}
	return enrollment, nil
}

// markApplied records that an enrollment once became current.
func (r *Registry) markApplied(ctx context.Context, id shoal.ID, e Enrollment) error {
	stored, err := r.putImmutable(ctx, r.appliedCoordinate(id, e.KeyDigest), []byte(e.ID))
	if err != nil {
		return err
	}
	if shoal.ID(stored) != e.ID {
		return ErrUnavailable
	}
	return nil
}

// activeRegistration requires the caller's bound, enrolled registration.
func (r *Registry) activeRegistration(ctx context.Context, id shoal.ID, d auth.Decision) (registrationRow, error) {
	row, e := r.boundRegistration(ctx, id, d)
	if e != nil {
		return registrationRow{}, e
	}
	if row.Registration.State != collector.Enrolled {
		return registrationRow{}, denied("collector is not enrolled")
	}
	return row, nil
}

func (r *Registry) readArtifact(ctx context.Context, collectorID shoal.ID, generation int64, artifactID shoal.ID) (ArtifactRecord, error) {
	cell, e := r.read(ctx, r.artifactCoordinate(collectorID, generation, artifactID))
	if e != nil {
		return ArtifactRecord{}, e
	}
	var record ArtifactRecord
	if cell.Timestamp != 1 || decode(cell.Value, &record) != nil || record.CollectorID != collectorID || record.Generation != generation || len(record.Domain) == 0 || record.Ref.ID != artifactID || record.Ref.Validate() != nil {
		return ArtifactRecord{}, ErrUnavailable
	}
	return record, nil
}

// SubmitArtifact records a raw artifact reference for the caller's collector.
// Resubmitting an identical reference returns the original record.
func (r *Registry) SubmitArtifact(ctx context.Context, collectorID shoal.ID, ref collector.ArtifactRef) (ArtifactRecord, error) {
	if ref.Validate() != nil || shoal.ValidateRequiredID("collector ID", collectorID) != nil {
		return ArtifactRecord{}, invalid()
	}
	d, fp, now, e := r.caller(ctx, auth.OperationIngest)
	if e != nil {
		return ArtifactRecord{}, e
	}
	row, e := r.activeRegistration(ctx, collectorID, d)
	if e != nil {
		return ArtifactRecord{}, e
	}
	if ref.ObservedAt.After(now) {
		return ArtifactRecord{}, invalid()
	}
	generation := row.Registration.Generation
	existing, e := r.readArtifact(ctx, collectorID, generation, ref.ID)
	if e == nil {
		if !reflect.DeepEqual(existing.Ref, ref) {
			return ArtifactRecord{}, ErrConflict
		}
		return existing, r.recheck(ctx, auth.OperationIngest, fp)
	}
	if !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		return ArtifactRecord{}, e
	}
	record := ArtifactRecord{CollectorID: collectorID, Domain: bytes.Clone(row.Registration.Domain), Ref: ref, Generation: generation, EnrollmentID: row.Enrollment.ID, ReceivedAt: now}
	value, e := encode(record)
	if e != nil {
		return ArtifactRecord{}, invalid()
	}
	stored, e := r.putImmutable(ctx, r.artifactCoordinate(collectorID, generation, ref.ID), value)
	if e != nil {
		return ArtifactRecord{}, e
	}
	var got ArtifactRecord
	if decode(stored, &got) != nil {
		return ArtifactRecord{}, indeterminate(ErrUnavailable)
	}
	if !reflect.DeepEqual(got.Ref, ref) || got.CollectorID != collectorID || got.Generation != generation {
		return ArtifactRecord{}, ErrConflict
	}
	if e = r.recheck(ctx, auth.OperationIngest, fp); e != nil {
		return ArtifactRecord{}, indeterminate(e)
	}
	return got, nil
}

func (r *Registry) readObservation(ctx context.Context, id shoal.ID) (observationRow, collector.Observation, error) {
	cell, e := r.read(ctx, r.observationCoordinate(id))
	if e != nil {
		return observationRow{}, collector.Observation{}, e
	}
	var row observationRow
	if cell.Timestamp != 1 || decode(cell.Value, &row) != nil || row.ID != id || len(row.Domain) == 0 || row.Generation <= 0 {
		return observationRow{}, collector.Observation{}, ErrUnavailable
	}
	o, e := collector.NewObservation(row.Config)
	if e != nil || o.ID() != id {
		return observationRow{}, collector.Observation{}, ErrUnavailable
	}
	return row, o, nil
}

// SubmitObservation records one observation from the caller's collector over
// an artifact that same collector recorded in its current generation, by an
// extractor its current enrollment declares.
func (r *Registry) SubmitObservation(ctx context.Context, o collector.Observation) (ObservationRecord, error) {
	if o.Validate() != nil {
		return ObservationRecord{}, invalid()
	}
	c := o.Config()
	d, fp, now, e := r.caller(ctx, auth.OperationIngest)
	if e != nil {
		return ObservationRecord{}, e
	}
	row, e := r.activeRegistration(ctx, c.CollectorID, d)
	if e != nil {
		return ObservationRecord{}, e
	}
	if !row.Enrollment.Request.HasExtractor(c.Extractor) {
		return ObservationRecord{}, denied("extractor not declared by enrollment")
	}
	if c.ObservedAt.After(now) {
		return ObservationRecord{}, invalid()
	}
	// Only this generation's artifacts are visible here; an earlier
	// generation's record under the same ID reads as absent.
	artifact, e := r.readArtifact(ctx, c.CollectorID, row.Registration.Generation, c.ArtifactID)
	if e != nil {
		return ObservationRecord{}, e
	}
	existing, _, e := r.readObservation(ctx, o.ID())
	if e != nil && !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		return ObservationRecord{}, e
	}
	if e != nil {
		value, e := encode(observationRow{ID: o.ID(), Domain: bytes.Clone(row.Registration.Domain), Config: c, Generation: row.Registration.Generation, EnrollmentID: row.Enrollment.ID, ReceivedAt: now})
		if e != nil {
			return ObservationRecord{}, invalid()
		}
		stored, e := r.putImmutable(ctx, r.observationCoordinate(o.ID()), value)
		if e != nil {
			return ObservationRecord{}, e
		}
		if decode(stored, &existing) != nil || existing.ID != o.ID() {
			return ObservationRecord{}, indeterminate(ErrUnavailable)
		}
	}
	if !reflect.DeepEqual(existing.Config, c) {
		return ObservationRecord{}, ErrConflict
	}
	// Identical content first recorded under a revoked generation stays
	// quarantined; it is not re-activated by resubmission.
	if existing.Generation != row.Registration.Generation {
		return ObservationRecord{}, denied("observation recorded under a revoked generation")
	}
	if e = r.recheck(ctx, auth.OperationIngest, fp); e != nil {
		return ObservationRecord{}, indeterminate(e)
	}
	return ObservationRecord{Observation: o, ArtifactDigest: artifact.Ref.Digest, Generation: existing.Generation, EnrollmentID: existing.EnrollmentID, ReceivedAt: existing.ReceivedAt}, nil
}

// ReadObservation returns an observation to any caller holding Read in the
// authorization domain recorded with it when it was written. Quarantine is computed from the collector's
// current registration: a revoked collector, or any generation other than the
// current one, reads as quarantined.
func (r *Registry) ReadObservation(ctx context.Context, id shoal.ID) (ObservationRecord, error) {
	if !collector.ValidObservationID(id) {
		return ObservationRecord{}, invalid()
	}
	d, fp, _, e := r.caller(ctx, auth.OperationRead)
	if e != nil {
		return ObservationRecord{}, e
	}
	stored, o, e := r.readObservation(ctx, id)
	if e != nil {
		return ObservationRecord{}, e
	}
	// Authorize against the domain recorded with the row, not the current
	// registration, so no later provisioning can move old content.
	if !bytes.Equal(stored.Domain, d.AuthorizationDomain()) {
		return ObservationRecord{}, auth.ObjectNotFound()
	}
	reg, _, e := r.readRegistration(ctx, stored.Config.CollectorID)
	if e != nil || !bytes.Equal(reg.Registration.Domain, stored.Domain) {
		return ObservationRecord{}, ErrUnavailable
	}
	artifact, e := r.readArtifact(ctx, stored.Config.CollectorID, stored.Generation, stored.Config.ArtifactID)
	if e != nil {
		return ObservationRecord{}, ErrUnavailable
	}
	if e = r.recheck(ctx, auth.OperationRead, fp); e != nil {
		return ObservationRecord{}, auth.ObjectNotFound()
	}
	if !bytes.Equal(artifact.Domain, stored.Domain) {
		return ObservationRecord{}, ErrUnavailable
	}
	quarantined := reg.Registration.State == collector.Revoked || reg.Registration.Generation != stored.Generation
	return ObservationRecord{Observation: o, ArtifactDigest: artifact.Ref.Digest, Generation: stored.Generation, EnrollmentID: stored.EnrollmentID, ReceivedAt: stored.ReceivedAt, Quarantined: quarantined}, nil
}
