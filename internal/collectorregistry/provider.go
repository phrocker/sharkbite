// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Provider projects registry results onto the public wire contracts. Write
// receipts carry identifiers and counters only; content is read back through
// ReadObservation.
type Provider struct{ registry *Registry }

func NewProvider(r *Registry) (*Provider, error) {
	if r == nil {
		return nil, invalid()
	}
	return &Provider{r}, nil
}

func (p *Provider) Enroll(ctx context.Context, key []byte, req collector.EnrollRequest) (api.EnrollReceipt, error) {
	e, err := p.registry.Enroll(ctx, key, req)
	if err != nil {
		return api.EnrollReceipt{}, err
	}
	out := api.EnrollReceipt{Schema: api.Schema, CollectorID: api.EncodeID(req.CollectorID), EnrollmentID: api.EncodeID(e.ID), Generation: e.Generation, State: string(collector.Enrolled)}
	if e.Attestation != nil {
		out.AttestationStatus = string(e.Attestation.Status)
	}
	return checked(out)
}

func (p *Provider) SubmitArtifact(ctx context.Context, id shoal.ID, ref collector.ArtifactRef) (api.ArtifactReceipt, error) {
	a, err := p.registry.SubmitArtifact(ctx, id, ref)
	if err != nil {
		return api.ArtifactReceipt{}, err
	}
	return checked(api.ArtifactReceipt{Schema: api.Schema, CollectorID: api.EncodeID(a.CollectorID), ArtifactID: api.EncodeID(a.Ref.ID), Generation: a.Generation, ReceivedAt: a.ReceivedAt})
}

func (p *Provider) SubmitObservation(ctx context.Context, o collector.Observation) (api.ObservationReceipt, error) {
	r, err := p.registry.SubmitObservation(ctx, o)
	if err != nil {
		return api.ObservationReceipt{}, err
	}
	c := r.Observation.Config()
	return checked(api.ObservationReceipt{Schema: api.Schema, ObservationID: api.EncodeID(r.Observation.ID()), CollectorID: api.EncodeID(c.CollectorID), ArtifactID: api.EncodeID(c.ArtifactID), Generation: r.Generation, ReceivedAt: r.ReceivedAt})
}

func (p *Provider) ReadObservation(ctx context.Context, id shoal.ID) (api.ObservationRecord, error) {
	r, err := p.registry.ReadObservation(ctx, id)
	if err != nil {
		return api.ObservationRecord{}, err
	}
	status := api.StatusActive
	if r.Quarantined {
		status = api.StatusQuarantined
	}
	out := api.ObservationRecord{Schema: api.Schema, ObservationID: api.EncodeID(r.Observation.ID()), Observation: api.EncodeObservation(r.Observation), ArtifactDigest: r.ArtifactDigest, Generation: r.Generation, ReceivedAt: r.ReceivedAt, Status: status}
	if out.Validate() != nil {
		return api.ObservationRecord{}, ErrUnavailable
	}
	return out, nil
}

// checked validates a write receipt. A projection failure after a durable
// write leaves the outcome for the caller to reconcile.
func checked[T interface{ Validate() error }](v T) (T, error) {
	if err := v.Validate(); err != nil {
		var zero T
		return zero, indeterminate(err)
	}
	return v, nil
}

var _ api.Provider = (*Provider)(nil)
