// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"net/http"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const FleetRoutePrefix = "/api/v1/fleet/"

// NewFleetHandler returns the registry and dispatch HTTP surface for the one
// authenticated FleetRoutePrefix mount used by hosted startup. The separate,
// more-specific fleet-events subtree remains additive. Authentication and
// decision binding remain the responsibility of the enclosing workspace
// Handler.
func NewFleetHandler(
	registry FleetRegistryProvider,
	dispatch FleetDispatchProvider,
) (http.Handler, error) {
	if registry == nil || dispatch == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet registry and dispatch providers are required",
		)
	}
	mux := http.NewServeMux()
	mountFleetRegistry(mux, registry)
	mountFleetDispatch(mux, dispatch)
	return mux, nil
}
