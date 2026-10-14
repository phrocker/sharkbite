// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"net/http"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DecisionSurface is the complete trusted provider set for the browser
// Decisions console. A host must supply every provider and the same resolver
// used by its authenticated transport; there is no partial or anonymous
// surface. Providers remain responsible for current source, task, outcome,
// and adjudication authorization.
type DecisionSurface struct {
	Resolver      auth.Resolver
	Decisions     decisionapi.Provider
	Registrations decisionapi.RegistrationProvider
	Datasets      decisionapi.DatasetProvider
	Outcomes      decisionapi.OutcomeProvider
	Adjudications decisionapi.AdjudicationProvider
}

func (s DecisionSurface) validate() error {
	if isAbsentInterface(s.Resolver) ||
		isAbsentInterface(s.Decisions) ||
		isAbsentInterface(s.Registrations) ||
		isAbsentInterface(s.Datasets) ||
		isAbsentInterface(s.Outcomes) ||
		isAbsentInterface(s.Adjudications) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"complete decision surface and trusted resolver required",
		)
	}
	return nil
}

// MountDecisionSurface validates the complete host composition before adding
// any route. Mounting then proceeds through the existing route-specific
// constructors, preserving their authentication, origin, and conflict gates.
func (h *Handler) MountDecisionSurface(surface DecisionSurface) error {
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "decision surface requires authenticated transport")
	}
	if err := surface.validate(); err != nil {
		return err
	}
	for _, route := range []string{
		DecisionsRoute, RegistrationsRoute, DatasetExportsRoute,
		OutcomesRoute, AdjudicationsRoute,
	} {
		for _, method := range []string{
			http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodHead,
		} {
			if exactMuxPatternRegistered(h.mux, method, route) {
				return authenticatedMountConflict()
			}
		}
	}
	if err := h.MountDecisions(surface.Decisions, surface.Resolver); err != nil {
		return err
	}
	if err := h.MountRegistrations(surface.Registrations, surface.Resolver); err != nil {
		return err
	}
	if err := h.MountDatasetExports(surface.Datasets, surface.Resolver); err != nil {
		return err
	}
	if err := h.MountOutcomes(surface.Outcomes, surface.Resolver); err != nil {
		return err
	}
	return h.MountAdjudications(surface.Adjudications, surface.Resolver)
}
