// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionregistrationhttp projects the trusted registration service.
// It grants no source access and exposes neither preparing records nor bytes.
package decisionregistrationhttp

import (
	"bytes"
	"context"
	"errors"

	"github.com/phrocker/shoal-oss/internal/decisionregistration"
	registrations "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Adapter struct{ service *decisionregistration.Service }

func New(s *decisionregistration.Service) (*Adapter, error) {
	if s == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "registration service required")
	}
	return &Adapter{s}, nil
}
func unavailable() error { return shoal.NewError(shoal.ErrorUnavailable, "registration unavailable") }
func project(r registrations.Registration) (api.RegistrationReceipt, error) {
	f := r.Frozen
	out := api.RegistrationReceipt{ID: r.ID, Scope: api.RegistrationScope{Domain: bytes.Clone(r.Scope.Domain), SubjectID: r.Scope.SubjectID, ActorID: r.Scope.ActorID, ClientID: r.Scope.ClientID, OnBehalfOf: append([]shoal.ID(nil), r.Scope.OnBehalfOf...)}, State: string(r.State), Version: r.Version, FrozenSHA256: r.FrozenSHA256, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ReadyAt: r.ReadyAt, Snapshot: api.RegistrationSnapshot{SelectionSHA256: f.SelectionSHA256, ProfileID: f.ProfileID, ProfileRevisionID: f.ProfileRevisionID, BuilderID: f.BuilderID, RequestID: f.RequestID, TaskID: f.TaskID, PictureID: f.PictureID, PredictorID: f.PredictorID, AcceptedAt: f.AcceptedAt, AuthenticationExpiresAt: f.AuthenticationExpiresAt, AuthorizationFingerprint: f.AuthorizationFingerprint, RecordSHA256: f.RecordSHA256, RecordBytes: f.RecordBytes, Sources: []api.RegistrationSourcePin{}}}
	for _, s := range f.Sources {
		out.Snapshot.Sources = append(out.Snapshot.Sources, api.RegistrationSourcePin{CollectorID: s.CollectorID, ObservationID: s.ObservationID, ArtifactID: s.ArtifactID, EnrollmentID: s.EnrollmentID, AuthorityPolicyID: s.AuthorityPolicyID, Mode: string(s.Mode), Generation: s.Generation, ArtifactSHA256: s.ArtifactSHA256, SourceSHA256: s.SourceSHA256, ReceivedAt: s.ReceivedAt})
	}
	if api.ValidateRegistrationReceipt(out) != nil {
		return api.RegistrationReceipt{}, unavailable()
	}
	return out, nil
}
func registerError(e error) error {
	if errors.Is(e, decisionregistration.ErrIndeterminate) {
		return errors.Join(api.ErrIndeterminate, unavailable())
	}
	return e
}

func (a *Adapter) Register(ctx context.Context, s api.RegistrationSelection, key []byte) (api.RegistrationReceipt, error) {
	if a == nil || a.service == nil {
		return api.RegistrationReceipt{}, unavailable()
	}
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return api.RegistrationReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid registration key")
	}
	normalized, e := api.NormalizeRegistrationSelection(s)
	if e != nil {
		return api.RegistrationReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid registration selection")
	}
	selection := decisionregistration.Selection{ProfileID: normalized.ProfileID, ProfileRevisionID: normalized.ProfileRevisionID, Sources: make([]decisionregistration.SourceInput, len(normalized.Sources))}
	for i, x := range normalized.Sources {
		selection.Sources[i] = decisionregistration.SourceInput{ObservationID: x.ObservationID, Bytes: bytes.Clone(x.Bytes)}
	}
	r, e := a.service.Register(ctx, bytes.Clone(key), selection)
	if e != nil {
		return api.RegistrationReceipt{}, registerError(e)
	}
	out, e := project(r)
	if e != nil || ctx.Err() != nil || !api.MatchRegistrationSelection(normalized, out) {
		return api.RegistrationReceipt{}, errors.Join(api.ErrIndeterminate, unavailable())
	}
	return out, nil
}
func (a *Adapter) Read(ctx context.Context, request shoal.ID) (api.RegistrationReceipt, error) {
	if a == nil || a.service == nil {
		return api.RegistrationReceipt{}, unavailable()
	}
	if _, e := api.DecodeID(api.EncodeID(request)); e != nil {
		return api.RegistrationReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid registration request ID")
	}
	r, e := a.service.Read(ctx, request)
	if e != nil {
		return api.RegistrationReceipt{}, e
	}
	out, e := project(r)
	if e != nil || ctx.Err() != nil || out.Snapshot.RequestID != request {
		return api.RegistrationReceipt{}, unavailable()
	}
	return out, nil
}

var _ api.RegistrationProvider = (*Adapter)(nil)
