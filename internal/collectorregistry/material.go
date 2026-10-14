// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxMaterials = 64
const MaxMaterialBytes = 16 << 20

// Material contains registered metadata, not original artifact bytes or a grant.
// Only the current enrollment is supported: superseded enrollment references
// are opaque not-found, even when they belong to the current generation.
type Material struct {
	Registration collector.Registration
	Enrollment   Enrollment
	Artifact     ArtifactRecord
	Observation  ObservationRecord
}

// MaterialAuthority is mandatory in addition to registry domain-read access.
// Authorize checks current per-source and purpose permissions for every ID,
// masking missing and denied IDs alike. Verify checks the entire batch jointly,
// including current grants and disclosure restrictions, after the final registry
// read. It must finish its own policy IO before its final authorization decision.
// Authorize runs again after IO, including failed reads. Verify receives nil on
// an incomplete resolution and must not treat that as a complete empty inventory.
// No registry reads occur after Verify; authorization is not an atomic lease.
type MaterialAuthority interface {
	Authorize(context.Context, auth.Decision, shoal.ID, []shoal.ID) error
	Verify(context.Context, auth.Decision, shoal.ID, []Material) error
}
type MaterialResolverConfig struct {
	Registry  *Registry
	Authority MaterialAuthority
}
type MaterialResolver struct{ config MaterialResolverConfig }

func NewMaterialResolver(c MaterialResolverConfig) (*MaterialResolver, error) {
	if c.Registry == nil || absent(c.Authority) {
		return nil, invalid()
	}
	return &MaterialResolver{c}, nil
}
func materialError(e error) error {
	if e == nil {
		return nil
	}
	if shoal.IsErrorCode(e, shoal.ErrorNotFound) || shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
		return auth.ObjectNotFound()
	}
	return ErrUnavailable
}

// Resolve returns a detached, all-or-nothing batch in the requested order.
// IDs must be unique; arbitrary material/attestation claims are never accepted.
func (m *MaterialResolver) Resolve(ctx context.Context, purpose shoal.ID, ids []shoal.ID) ([]Material, error) {
	if shoal.ValidateRequiredID("purpose", purpose) != nil || len(ids) == 0 || len(ids) > MaxMaterials {
		return nil, invalid()
	}
	ids = append([]shoal.ID(nil), ids...)
	seen := map[shoal.ID]bool{}
	for _, id := range ids {
		if !collector.ValidObservationID(id) || seen[id] {
			return nil, invalid()
		}
		seen[id] = true
	}
	r := m.config.Registry
	d, fp, _, err := r.caller(ctx, auth.OperationRead)
	if err != nil {
		return nil, auth.ObjectNotFound()
	}
	authorize := func() error {
		return materialError(m.config.Authority.Authorize(ctx, d, purpose, append([]shoal.ID(nil), ids...)))
	}
	if err = authorize(); err != nil {
		return nil, err
	}
	if err = r.recheck(ctx, auth.OperationRead, fp); err != nil {
		return nil, auth.ObjectNotFound()
	}
	// A per-call read-only wrapper caps all raw bytes before existing typed codecs.
	local := *r
	local.config.Backend = &materialBackend{CAS: r.config.Backend}
	materials := make([]Material, 0, len(ids))
	initial := make(map[shoal.ID]allocator.Cell)
	var collectErr error
	for _, id := range ids {
		material, cell, e := local.material(ctx, d, id)
		if e != nil {
			collectErr = e
			break
		}
		if old, ok := initial[material.Registration.CollectorID]; ok && (old.Timestamp != cell.Timestamp || !bytes.Equal(old.Value, cell.Value)) {
			collectErr = auth.ObjectNotFound()
			break
		}
		initial[material.Registration.CollectorID] = cell
		materials = append(materials, material)
	}
	// Finish every registry read before joint verification. Compare exact current
	// registration rows; monotonically versioned CAS prevents unnoticed rotation.
	if collectErr == nil {
		for _, v := range materials {
			old, ok := initial[v.Registration.CollectorID]
			if !ok {
				continue
			}
			delete(initial, v.Registration.CollectorID)
			_, cell, e := local.readRegistration(ctx, v.Registration.CollectorID)
			if e != nil {
				collectErr = e
				break
			}
			if old.Timestamp != cell.Timestamp || !bytes.Equal(old.Value, cell.Value) {
				collectErr = auth.ObjectNotFound()
				break
			}
		}
	}
	if err = authorize(); err != nil {
		return nil, err
	}
	if err = r.recheck(ctx, auth.OperationRead, fp); err != nil {
		return nil, auth.ObjectNotFound()
	}
	var verification []Material
	if collectErr == nil {
		verification, err = cloneMaterials(materials)
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	before, err := cloneMaterials(verification)
	if err != nil {
		return nil, ErrUnavailable
	}
	verifyErr := m.config.Authority.Verify(ctx, d, purpose, verification)
	// These checks do not read the registry. Resolver must revalidate actual auth.
	if err = r.recheck(ctx, auth.OperationRead, fp); err != nil {
		return nil, auth.ObjectNotFound()
	}
	if err = ctx.Err(); err != nil {
		return nil, auth.ObjectNotFound()
	}
	if verifyErr != nil {
		return nil, materialError(verifyErr)
	}
	if !reflect.DeepEqual(before, verification) {
		return nil, ErrUnavailable
	}
	if collectErr != nil {
		return nil, materialError(collectErr)
	}
	return cloneMaterials(materials)
}

func (r *Registry) material(ctx context.Context, d auth.Decision, id shoal.ID) (Material, allocator.Cell, error) {
	var zero Material
	row, o, e := r.readObservation(ctx, id)
	if e != nil {
		return zero, allocator.Cell{}, e
	}
	if !bytes.Equal(row.Domain, d.AuthorizationDomain()) {
		return zero, allocator.Cell{}, auth.ObjectNotFound()
	}
	reg, cell, e := r.readRegistration(ctx, row.Config.CollectorID)
	if e != nil {
		return zero, cell, e
	}
	if reg.Registration.State != collector.Enrolled || reg.Enrollment == nil || reg.Registration.Generation != row.Generation || !bytes.Equal(reg.Registration.Domain, row.Domain) || reg.Enrollment.ID != row.EnrollmentID {
		return zero, cell, auth.ObjectNotFound()
	}
	a, e := r.readArtifact(ctx, row.Config.CollectorID, row.Generation, row.Config.ArtifactID)
	if e != nil {
		return zero, cell, e
	}
	if !bytes.Equal(a.Domain, row.Domain) || a.EnrollmentID != row.EnrollmentID {
		return zero, cell, auth.ObjectNotFound()
	}
	enCell, e := r.read(ctx, r.enrollmentCoordinate(row.Config.CollectorID, reg.Enrollment.KeyDigest))
	if e != nil {
		return zero, cell, e
	}
	var en enrollmentRow
	if enCell.Timestamp != 1 || decode(enCell.Value, &en) != nil || en.CollectorID != row.Config.CollectorID || !reflect.DeepEqual(en.Enrollment, *reg.Enrollment) {
		return zero, cell, ErrUnavailable
	}
	enrollment := en.Enrollment
	requestDigest, e := enrollment.Request.Digest()
	if e != nil || !collector.ValidDigest(enrollment.KeyDigest) || requestDigest != enrollment.RequestDigest || enrollment.Request.CollectorID != row.Config.CollectorID || enrollment.Generation != row.Generation || !enrollment.Request.HasExtractor(row.Config.Extractor) || !reg.Registration.Permits(enrollment.Request.RequestedAuthorityPolicyIDs) {
		return zero, cell, ErrUnavailable
	}
	wantID := shoal.ID("enrollment:" + digest("collector-enrollment-v1", string(row.Config.CollectorID), fmt.Sprint(row.Generation), enrollment.KeyDigest))
	if enrollment.ID != wantID {
		return zero, cell, ErrUnavailable
	}
	if enrollment.Attestation != nil && (enrollment.Attestation.Validate() != nil || enrollment.Attestation.Subject != row.Config.CollectorID) {
		return zero, cell, ErrUnavailable
	}
	now, e := r.now()
	if e != nil {
		return zero, cell, e
	}
	for _, t := range []time.Time{enrollment.EnrolledAt, a.ReceivedAt, row.ReceivedAt} {
		if t.IsZero() || t.Year() < 1 || t.Year() > 9999 || t.After(now) {
			return zero, cell, ErrUnavailable
		}
	}
	if a.ReceivedAt.Before(enrollment.EnrolledAt) || row.ReceivedAt.Before(enrollment.EnrolledAt) || row.ReceivedAt.Before(a.ReceivedAt) || row.Config.ObservedAt.After(row.ReceivedAt) || a.Ref.ObservedAt.After(a.ReceivedAt) {
		return zero, cell, ErrUnavailable
	}
	return Material{reg.Registration, enrollment, a, ObservationRecord{Observation: o, ArtifactDigest: a.Ref.Digest, Generation: row.Generation, EnrollmentID: row.EnrollmentID, ReceivedAt: row.ReceivedAt}}, cell, nil
}
func cloneMaterials(in []Material) ([]Material, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]Material, len(in))
	for i, v := range in {
		reg, e := v.Registration.Canonical()
		if e != nil {
			return nil, ErrUnavailable
		}
		b, e := encode(v.Enrollment)
		if e != nil {
			return nil, ErrUnavailable
		}
		var en Enrollment
		if decode(b, &en) != nil {
			return nil, ErrUnavailable
		}
		o, e := collector.NewObservation(v.Observation.Observation.Config())
		if e != nil {
			return nil, ErrUnavailable
		}
		v.Registration = reg
		v.Enrollment = en
		v.Artifact.Domain = bytes.Clone(v.Artifact.Domain)
		v.Observation.Observation = o
		out[i] = v
	}
	return out, nil
}

// materialBackend is read-only and owns cell buffers before typed decoding.
type materialBackend struct {
	decisionstore.CAS
	used int
}

func (b *materialBackend) CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error) {
	return allocator.StatusRejected, ErrUnavailable
}
func (b *materialBackend) ReadExact(ctx context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	cells, e := b.CAS.ReadExact(ctx, coords)
	if e != nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if len(cells) > 1 {
		return nil, ErrUnavailable
	}
	out := append([]allocator.Cell(nil), cells...)
	for i := range out {
		raw := out[i].Value
		if len(raw) > maxStoredBytes || b.used > MaxMaterialBytes-len(raw) {
			return nil, ErrUnavailable
		}
		b.used += len(raw)
		raw = bytes.Clone(raw)
		if materialShape(raw) != nil {
			return nil, ErrUnavailable
		}
		out[i].Value = raw
	}
	return out, nil
}
func materialShape(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	nodes := 0
	var value func(int) error
	value = func(depth int) error {
		nodes++
		if depth > 16 || nodes > 100000 {
			return ErrUnavailable
		}
		tok, e := dec.Token()
		if e != nil {
			return e
		}
		if delim, ok := tok.(json.Delim); ok {
			count := 0
			switch delim {
			case '{':
				for dec.More() {
					count++
					if count > 64 {
						return ErrUnavailable
					}
					if _, e = dec.Token(); e != nil {
						return e
					}
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			case '[':
				for dec.More() {
					count++
					if count > 1024 {
						return ErrUnavailable
					}
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			default:
				return ErrUnavailable
			}
			_, e = dec.Token()
			return e
		}
		return nil
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := dec.Token(); e != io.EOF {
		return ErrUnavailable
	}
	return nil
}
