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

package webapi

import (
	"net/http"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func (h *Handler) SetInteractionProvider(provider InteractionProvider) error {
	if h == nil || isAbsentInterface(provider) ||
		isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return chatInvalid("provenance requires a provider and an authenticated handler")
	}
	if h.interactionProvider != nil {
		return chatInvalid("provenance provider is already configured")
	}
	h.interactionProvider = provider
	h.mux.HandleFunc("GET /api/v1/provenance", h.listProvenance)
	h.mux.HandleFunc("GET /api/v1/provenance/{session}", h.inspectProvenance)
	h.mux.HandleFunc("POST /api/v1/provenance/fold", h.foldProvenance)
	h.mux.HandleFunc("POST /api/v1/provenance/unfold", h.unfoldProvenance)
	return nil
}

func (h *Handler) listProvenance(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	input := ProvenanceListRequest{
		Cursor: request.URL.Query().Get("cursor"),
	}
	if encoded := request.URL.Query().Get("limit"); encoded != "" {
		limit, err := strconv.ParseUint(encoded, 10, 32)
		if err != nil {
			writeError(writer, chatInvalid("provenance list limit is invalid"))
			return
		}
		input.Limit = uint32(limit)
	}
	response, err := h.interactionProvider.ListProvenance(
		request.Context(), input)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeResponse(writer, http.StatusOK, response)
}

func (h *Handler) inspectProvenance(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	sessionID, err := decodeID(request.PathValue("session"))
	if err != nil {
		writeError(writer, shoal.NewError(
			shoal.ErrorInvalidArgument, "session ID "+err.Error()))
		return
	}
	response, err := h.interactionProvider.InspectProvenance(
		request.Context(), sessionID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeResponse(writer, http.StatusOK, response)
}

func (h *Handler) foldProvenance(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if err := requireSameOrigin(request); err != nil {
		writeError(writer, err)
		return
	}
	var input ProvenanceFoldRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, chatInvalid(err.Error()))
		return
	}
	response, err := h.interactionProvider.FoldProvenance(request.Context(), input)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if response.Created {
		status = http.StatusCreated
	}
	writeResponse(writer, status, response)
}

func (h *Handler) unfoldProvenance(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if err := requireSameOrigin(request); err != nil {
		writeError(writer, err)
		return
	}
	var input ProvenanceUnfoldRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, chatInvalid(err.Error()))
		return
	}
	response, err := h.interactionProvider.UnfoldProvenance(request.Context(), input)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeResponse(writer, http.StatusOK, response)
}
