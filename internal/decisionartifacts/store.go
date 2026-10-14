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

// Package decisionartifacts retains immutable decision inputs over Shoal row-CAS.
// Retention is not a grant: every admission and rehydration consults current
// authority, and no default authority accepts source-authored provenance claims.
package decisionartifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_decision_artifacts"

var ErrUnavailable = shoal.NewError(shoal.ErrorUnavailable, "decision artifacts unavailable")
var ErrIndeterminate = shoal.NewError(shoal.ErrorUnavailable, "decision artifact retention indeterminate")
var ErrConflict = shoal.NewError(shoal.ErrorConflict, "decision artifact conflict")

// Authority is a mandatory trusted integration with the task/evidence registry.
// AuthorizeRequest checks current access to the registered request before any
// retained row is inspected; absent and denied requests must both be NotFound.
// Verify checks EVERY contributing source/anchor/outcome, source registration,
// role/control/origin/attestation and snapshot/measurement claims, and the input
// serialization against the pinned builder. It also validates task/release and
// policy registrations; hashes alone do not authenticate any of these claims.
// Both methods run on admission and load. Implementations must account for
// joint-disclosure restrictions, not merely independent source grants.
// Non-disclosure denial is NotFound/Unauthorized; other failures are unavailable.
// No integration is supplied here for graph, outcome or model attestation.
type Authority interface {
	AuthorizeRequest(context.Context, auth.Decision, shoal.ID) error
	Verify(context.Context, auth.Decision, Record) error
}
type Config struct {
	Backend    decisionstore.CAS
	Resolver   auth.Resolver
	Authority  Authority
	Visibility []byte
	Clock      func() time.Time
}
type Catalog struct{ config Config }

var _ decisionservice.Artifacts = (*Catalog)(nil)

func New(c Config) (*Catalog, error) {
	if nilDependency(c.Backend) || nilDependency(c.Resolver) || nilDependency(c.Authority) || c.Clock == nil || len(c.Visibility) > 4096 {
		return nil, invalid()
	}
	c.Visibility = append([]byte(nil), c.Visibility...)
	return &Catalog{c}, nil
}

// Retain registers exact bytes once. Retries can confirm the same record but
// cannot replace task policy, serialized inputs or source bytes under its ID.
// The request must already exist in the trusted registry. This is not a public
// registration endpoint and never authorizes caller-supplied task definitions.
func (c *Catalog) Retain(ctx context.Context, r Record) error {
	id := r.Bundle.Request.ID()
	d, err := c.resolve(ctx)
	if err != nil {
		return err
	}
	if err := c.preflight(ctx, d, id); err != nil {
		return err
	}
	encoded, err := encode(r)
	if err != nil {
		return err
	}
	// Decode before persistence: canonical JSON must round-trip all identities.
	owned, err := decode(encoded)
	if err != nil {
		return invalid()
	}
	if err := c.check(ctx, d, owned, encoded, true); err != nil {
		return err
	}
	coord := c.coordinate(d, id)
	status, writeErr := c.config.Backend.CompareAndMutate(ctx, allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{{Coordinate: coord, Absent: true}}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: 1, Value: encoded}}})
	if writeErr != nil || status != allocator.StatusAccepted {
		// A lost acknowledgment is success only if the exact immutable row is read
		// back. A failed read cannot prove rollback, even after a CAS rejection.
		stored, readErr := c.read(ctx, coord)
		// Errors can disclose storage state too. Recheck access after the last
		// I/O before distinguishing a conflicting row from an absent one.
		if _, err := c.reauthorize(ctx, d, id); err != nil {
			// Confirmed denial masks storage state. Cancellation or an authority
			// outage cannot establish rollback of an unresolved mutation.
			unresolved := readErr != nil || (!bytes.Equal(stored, encoded) && (writeErr != nil || status != allocator.StatusRejected))
			if unresolved && !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return errors.Join(ErrIndeterminate, err)
			}
			return err
		}
		if readErr != nil {
			return ErrIndeterminate
		}
		if !bytes.Equal(stored, encoded) {
			if writeErr == nil && status == allocator.StatusRejected {
				return ErrConflict
			}
			return ErrIndeterminate
		}
	}
	// Revocation during persistence may leave a retained row, but cannot turn
	// into a successful authorized response. No deletion/rollback is fabricated.
	current, err := c.resolveSame(ctx, d)
	if err != nil {
		return err
	}
	return c.check(ctx, current, owned, encoded, true)
}

func (c *Catalog) LoadAuthorized(ctx context.Context, supplied auth.Decision, id shoal.ID) (decisionservice.Bundle, error) {
	d, err := c.resolveSame(ctx, supplied)
	if err != nil {
		return decisionservice.Bundle{}, err
	}
	if err := c.preflight(ctx, d, id); err != nil {
		return decisionservice.Bundle{}, err
	}
	encoded, readErr := c.read(ctx, c.coordinate(d, id))
	current, err := c.reauthorize(ctx, d, id)
	if err != nil {
		return decisionservice.Bundle{}, err
	}
	if readErr != nil {
		return decisionservice.Bundle{}, readErr
	}
	r, err := decode(encoded)
	if err != nil {
		return decisionservice.Bundle{}, ErrUnavailable
	}
	if r.Bundle.Request.ID() != id {
		return decisionservice.Bundle{}, ErrUnavailable
	}
	if err := c.check(ctx, current, r, encoded, false); err != nil {
		return decisionservice.Bundle{}, err
	}
	return r.Bundle, nil
}
func (c *Catalog) preflight(ctx context.Context, d auth.Decision, id shoal.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if shoal.ValidateRequiredID("request ID", id) != nil {
		return invalid()
	}
	if err := c.config.Authority.AuthorizeRequest(ctx, d, id); err != nil {
		return authorityError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.now()
	if now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
		return ErrUnavailable
	}
	if !d.AuthenticationExpires().After(now) {
		return auth.ObjectNotFound()
	}
	return nil
}
func (c *Catalog) check(ctx context.Context, d auth.Decision, r Record, encoded []byte, admit bool) error {
	if err := c.preflight(ctx, d, r.Bundle.Request.ID()); err != nil {
		return err
	}
	if r.Bundle.Request.Config().PrincipalID != d.Subject() || !bytes.Equal(r.Bundle.TaskResource.AuthorizationDomain, d.AuthorizationDomain()) {
		return auth.ObjectNotFound()
	}
	if admit {
		if err := d.AuthorizeObject(auth.OperationInvoke, r.Bundle.TaskResource, c.now()); err != nil {
			return authorityError(err)
		}
		fp, err := auth.AuthorizationFingerprint(d)
		if err != nil || shoal.ID(fp.String()) != r.Bundle.Request.Picture().Authorization().Fingerprint() {
			return auth.ObjectNotFound()
		}
	}
	// A verifier receives an independent copy and cannot rewrite bytes after
	// validation or poison the bundle that will be returned to the service.
	copy, err := decode(encoded)
	if err != nil {
		return ErrUnavailable
	}
	if err := c.config.Authority.Verify(ctx, d, copy); err != nil {
		return authorityError(err)
	}
	current, err := c.resolveSame(ctx, d)
	if err != nil {
		return err
	}
	return c.preflight(ctx, current, r.Bundle.Request.ID())
}
func (c *Catalog) resolve(ctx context.Context) (auth.Decision, error) {
	if err := ctx.Err(); err != nil {
		return auth.Decision{}, err
	}
	d, err := c.config.Resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, authorityError(err)
	}
	now := c.now()
	_, fingerprintErr := auth.AuthorizationFingerprint(d)
	if now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
		return auth.Decision{}, ErrUnavailable
	}
	if fingerprintErr != nil || !d.AuthenticationExpires().After(now) {
		return auth.Decision{}, auth.ObjectNotFound()
	}
	return d, nil
}

// reauthorize protects both successful payloads and storage-state errors.
func (c *Catalog) reauthorize(ctx context.Context, before auth.Decision, id shoal.ID) (auth.Decision, error) {
	current, err := c.resolveSame(ctx, before)
	if err != nil {
		return auth.Decision{}, err
	}
	if err := c.preflight(ctx, current, id); err != nil {
		return auth.Decision{}, err
	}
	return current, nil
}

func (c *Catalog) resolveSame(ctx context.Context, before auth.Decision) (auth.Decision, error) {
	d, err := c.resolve(ctx)
	if err != nil {
		return auth.Decision{}, err
	}
	a, ea := auth.AuthorizationFingerprint(before)
	b, eb := auth.AuthorizationFingerprint(d)
	if ea != nil || eb != nil || a != b {
		return auth.Decision{}, auth.ObjectNotFound()
	}
	return d, nil
}
func (c *Catalog) now() time.Time { return c.config.Clock().Round(0).UTC() }
func (c *Catalog) coordinate(d auth.Decision, id shoal.ID) allocator.Coordinate {
	h := sha256.New()
	var n [8]byte
	for _, part := range [][]byte{[]byte("decision-artifacts-v1"), d.AuthorizationDomain(), []byte(d.Subject()), []byte(id)} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return allocator.Coordinate{Row: []byte("artifacts:" + hex.EncodeToString(h.Sum(nil))), Family: []byte("a"), Qualifier: []byte("record"), Visibility: append([]byte(nil), c.config.Visibility...)}
}
func (c *Catalog) read(ctx context.Context, coord allocator.Coordinate) ([]byte, error) {
	cells, err := c.config.Backend.ReadExact(ctx, []allocator.Coordinate{coord})
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(cells) == 0 {
		return nil, auth.ObjectNotFound()
	}
	if len(cells) != 1 || cells[0].Timestamp != 1 || !sameCoordinate(cells[0].Coordinate, coord) {
		return nil, ErrUnavailable
	}
	return cells[0].Value, nil
}
func sameCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func authorityError(err error) error {
	if err == nil {
		return nil
	}
	if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) || shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return auth.ObjectNotFound()
	}
	return ErrUnavailable
}
func nilDependency(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}
