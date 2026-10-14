// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package webapi

import (
	"context"
	"net/http"

	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const TeamOverviewRoute = "/api/v1/team/overview"

type TeamOverviewProvider interface {
	Overview(
		context.Context, teamoverview.Request,
	) (teamoverview.Response, error)
}

// NewTeamOverviewHandler constructs the additive authenticated team-overview
// endpoint. The enclosing workspace Handler remains the authentication gate.
func NewTeamOverviewHandler(
	provider TeamOverviewProvider,
) (http.Handler, error) {
	if isAbsentInterface(provider) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"team overview provider is required",
		)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+TeamOverviewRoute, func(
		writer http.ResponseWriter, request *http.Request,
	) {
		writer.Header().Set("Cache-Control", "no-store")
		var input teamoverview.Request
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(
				shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		response, err := provider.Overview(request.Context(), input)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeResponse(writer, http.StatusOK, response)
	})
	return mux, nil
}
