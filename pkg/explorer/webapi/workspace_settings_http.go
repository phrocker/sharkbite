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
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type WorkspaceSettingsResponse struct {
	WorkspaceID    string                     `json:"workspace_id"`
	SettingsID     string                     `json:"settings_id"`
	Owner          string                     `json:"owner"`
	Revision       uint64                     `json:"revision"`
	LastMutationID string                     `json:"last_mutation_id"`
	Settings       WorkspaceSettingsNarrowing `json:"settings"`
}

type WorkspaceSettingsUpdateRequest struct {
	ExpectedRevision uint64                      `json:"expected_revision"`
	MutationID       string                      `json:"mutation_id"`
	Settings         *WorkspaceSettingsNarrowing `json:"settings"`
}

type WorkspaceSettingsNarrowing struct {
	AllowedOperations  *[]string                  `json:"allowed_operations,omitempty"`
	PermittedSourceIDs *[]string                  `json:"permitted_source_ids,omitempty"`
	PermittedPolicyIDs *[]string                  `json:"permitted_policy_ids,omitempty"`
	Budgets            WorkspaceSettingsBudgets   `json:"budgets,omitempty"`
	OutputPolicies     []WorkspaceOutputPolicy    `json:"output_policies,omitempty"`
	SelectedOntology   *WorkspaceOntologyIdentity `json:"selected_ontology,omitempty"`
}

type WorkspaceSettingsBudgets struct {
	RetrievalTopK *uint32 `json:"retrieval_top_k,omitempty"`
	GraphDepth    *uint32 `json:"graph_depth,omitempty"`
	GraphFanout   *uint32 `json:"graph_fanout,omitempty"`
	GraphNodes    *uint32 `json:"graph_nodes,omitempty"`
	OutputBytes   *uint64 `json:"output_bytes,omitempty"`
}

type WorkspaceOutputPolicy struct {
	SourceID      string `json:"source_id"`
	GrantPolicyID string `json:"grant_policy_id"`
	Epoch         int64  `json:"epoch"`
}

type WorkspaceOntologyIdentity = OntologyIdentityProjection

type WorkspaceOntologyChoice struct {
	Identity OntologyIdentityProjection `json:"identity"`
	Version  string                     `json:"version"`
	Active   bool                       `json:"active"`
}

type WorkspaceOntologyChoicesResponse struct {
	WorkspaceID      string                     `json:"workspace_id"`
	SettingsID       string                     `json:"settings_id,omitempty"`
	SettingsRevision uint64                     `json:"settings_revision"`
	Active           OntologyIdentityProjection `json:"active"`
	SelectedOntology *WorkspaceOntologyIdentity `json:"selected_ontology,omitempty"`
	Choices          []WorkspaceOntologyChoice  `json:"choices"`
}

type WorkspaceOntologySelectionRequest struct {
	ExpectedRevision uint64                    `json:"expected_revision"`
	MutationID       string                    `json:"mutation_id"`
	SelectedOntology WorkspaceOntologyIdentity `json:"selected_ontology"`
}

func (h *Handler) registerWorkspaceSettingsRoutes() {
	h.mux.HandleFunc(
		"GET /api/v1/workspaces/{workspace}/settings",
		h.getWorkspaceSettings,
	)
	h.mux.HandleFunc(
		"PUT /api/v1/workspaces/{workspace}/settings",
		h.putWorkspaceSettings,
	)
	h.mux.HandleFunc(
		"GET /api/v1/workspaces/{workspace}/settings/lens",
		h.getWorkspaceLens,
	)
	h.mux.HandleFunc(
		"PUT /api/v1/workspaces/{workspace}/settings/lens",
		h.putWorkspaceLens,
	)
}

func (h *Handler) getWorkspaceSettings(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if isAbsentInterface(h.workspaceSettings) {
		writeError(writer, shoal.NewError(
			shoal.ErrorUnavailable, "workspace settings are unavailable"))
		return
	}
	workspaceID, err := decodeWorkspaceOpaqueID(
		"workspace ID", request.PathValue("workspace"))
	if err != nil {
		writeError(writer, err)
		return
	}
	settings, err := h.workspaceSettings.Get(request.Context(), workspaceID)
	if err != nil {
		writeError(writer, err)
		return
	}
	response, err := workspaceSettingsResponse(settings)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeResponse(writer, http.StatusOK, response)
}

func (h *Handler) putWorkspaceSettings(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if isAbsentInterface(h.workspaceSettings) {
		writeError(writer, shoal.NewError(
			shoal.ErrorUnavailable, "workspace settings are unavailable"))
		return
	}
	if err := requireSameOrigin(request); err != nil {
		writeError(writer, err)
		return
	}
	workspaceID, err := decodeWorkspaceOpaqueID(
		"workspace ID", request.PathValue("workspace"))
	if err != nil {
		writeError(writer, err)
		return
	}
	var input WorkspaceSettingsUpdateRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
		return
	}
	update, err := workspaceSettingsUpdate(input)
	if err != nil {
		writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
		return
	}
	settings, err := h.workspaceSettings.Update(
		request.Context(), workspaceID, update)
	if err != nil {
		writeError(writer, err)
		return
	}
	response, err := workspaceSettingsResponse(settings)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if settings.Revision == 1 {
		status = http.StatusCreated
	}
	writeResponse(writer, status, response)
}

func (h *Handler) getWorkspaceLens(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if isAbsentInterface(h.workspaceSettings) {
		writeError(writer, shoal.NewError(
			shoal.ErrorUnavailable, "workspace settings are unavailable"))
		return
	}
	workspaceID, err := decodeWorkspaceOpaqueID(
		"workspace ID", request.PathValue("workspace"))
	if err != nil {
		writeError(writer, err)
		return
	}
	choices, err := h.workspaceSettings.ListOntologyChoices(
		request.Context(), workspaceID)
	if err != nil {
		writeError(writer, err)
		return
	}
	response, err := workspaceOntologyChoicesResponse(choices)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeResponse(writer, http.StatusOK, response)
}

func (h *Handler) putWorkspaceLens(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if isAbsentInterface(h.workspaceSettings) {
		writeError(writer, shoal.NewError(
			shoal.ErrorUnavailable, "workspace settings are unavailable"))
		return
	}
	if err := requireSameOrigin(request); err != nil {
		writeError(writer, err)
		return
	}
	workspaceID, err := decodeWorkspaceOpaqueID(
		"workspace ID", request.PathValue("workspace"))
	if err != nil {
		writeError(writer, err)
		return
	}
	var input WorkspaceOntologySelectionRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
		return
	}
	mutationID, err := decodeWorkspaceOpaqueID(
		"mutation_id", input.MutationID)
	if err != nil {
		writeError(writer, err)
		return
	}
	identity, err := workspaceOntologyIdentityValue(input.SelectedOntology)
	if err != nil {
		writeError(writer, shoal.NewError(
			shoal.ErrorInvalidArgument, "selected_ontology: "+err.Error()))
		return
	}
	settings, err := h.workspaceSettings.SelectOntology(
		request.Context(), workspaceID, input.ExpectedRevision,
		mutationID, identity)
	if err != nil {
		writeError(writer, err)
		return
	}
	response, err := workspaceSettingsResponse(settings)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if settings.Revision == 1 {
		status = http.StatusCreated
	}
	writeResponse(writer, status, response)
}

func workspaceSettingsUpdate(
	value WorkspaceSettingsUpdateRequest,
) (workspace.UpdateRequest, error) {
	mutationID, err := decodeWorkspaceOpaqueID(
		"mutation_id", value.MutationID)
	if err != nil {
		return workspace.UpdateRequest{}, err
	}
	if value.Settings == nil {
		return workspace.UpdateRequest{}, fmt.Errorf("settings is required")
	}
	settings := *value.Settings
	operations := workspace.OperationSelection{}
	if settings.AllowedOperations != nil {
		operations.Present = true
		for index, name := range *settings.AllowedOperations {
			operation, err := auth.ParseOperation(name)
			if err != nil {
				return workspace.UpdateRequest{}, fmt.Errorf(
					"allowed_operations[%d]: %w", index, err)
			}
			operations.Values = append(operations.Values, operation)
		}
	}
	sources, err := decodeComponentSelection(settings.PermittedSourceIDs)
	if err != nil {
		return workspace.UpdateRequest{}, fmt.Errorf(
			"permitted_source_ids: %w", err)
	}
	policies, err := decodeComponentSelection(settings.PermittedPolicyIDs)
	if err != nil {
		return workspace.UpdateRequest{}, fmt.Errorf(
			"permitted_policy_ids: %w", err)
	}
	outputPolicies := make(
		[]workspace.OutputPolicySpec, 0, len(settings.OutputPolicies))
	for index, policy := range settings.OutputPolicies {
		sourceID, err := auth.DecodeComponent(policy.SourceID)
		if err != nil {
			return workspace.UpdateRequest{}, fmt.Errorf(
				"output_policies[%d].source_id: %w", index, err)
		}
		grantPolicyID, err := auth.DecodeComponent(policy.GrantPolicyID)
		if err != nil {
			return workspace.UpdateRequest{}, fmt.Errorf(
				"output_policies[%d].grant_policy_id: %w", index, err)
		}
		outputPolicies = append(outputPolicies, workspace.OutputPolicySpec{
			SourceID: sourceID, GrantPolicyID: grantPolicyID,
			Epoch: policy.Epoch,
		})
	}
	var selected workspace.OntologySelection
	if settings.SelectedOntology != nil {
		identity, err := workspaceOntologyIdentityValue(
			*settings.SelectedOntology)
		if err != nil {
			return workspace.UpdateRequest{}, fmt.Errorf(
				"selected_ontology: %w", err)
		}
		selected = workspace.OntologySelection{
			Present: true, Identity: identity,
		}
	}
	return workspace.UpdateRequest{
		ExpectedRevision: value.ExpectedRevision,
		MutationID:       mutationID,
		Narrowing: workspace.UpdateNarrowing{
			AllowedOperations:  operations,
			PermittedSourceIDs: sources,
			PermittedPolicyIDs: policies,
			Budgets: workspace.Budgets{
				RetrievalTopK: settings.Budgets.RetrievalTopK,
				GraphDepth:    settings.Budgets.GraphDepth,
				GraphFanout:   settings.Budgets.GraphFanout,
				GraphNodes:    settings.Budgets.GraphNodes,
				OutputBytes:   settings.Budgets.OutputBytes,
			},
			OutputPolicies:   outputPolicies,
			SelectedOntology: selected,
		},
	}, nil
}

func workspaceOntologyChoicesResponse(
	value workspace.OntologyChoiceSet,
) (WorkspaceOntologyChoicesResponse, error) {
	response := WorkspaceOntologyChoicesResponse{
		WorkspaceID:      encodeID(value.WorkspaceID),
		SettingsRevision: value.SettingsRevision,
		Choices:          make([]WorkspaceOntologyChoice, 0, len(value.Choices)),
		Active: OntologyIdentityProjection{
			Known: false, Reading: string(ontology.OntologyUnresolved),
		},
	}
	if value.SettingsID != "" {
		response.SettingsID = encodeID(value.SettingsID)
	}
	if value.SelectedOntology.Present {
		projected, err := ProjectOntologyIdentity(
			value.SelectedOntology.Identity)
		if err != nil {
			return WorkspaceOntologyChoicesResponse{}, err
		}
		response.SelectedOntology = &projected
	}
	for _, choice := range value.Choices {
		projected, err := ProjectOntologyIdentity(choice.Identity)
		if err != nil {
			return WorkspaceOntologyChoicesResponse{}, err
		}
		if choice.Active {
			response.Active = projected
		}
		response.Choices = append(response.Choices, WorkspaceOntologyChoice{
			Identity: projected,
			Version:  choice.Version,
			Active:   choice.Active,
		})
	}
	return response, nil
}

func workspaceOntologyIdentityValue(
	value WorkspaceOntologyIdentity,
) (ontology.OntologyIdentity, error) {
	identity, err := ParseOntologyIdentityProjection(value)
	if err != nil {
		return ontology.OntologyIdentity{}, err
	}
	if !identity.Known() {
		return ontology.OntologyIdentity{}, fmt.Errorf(
			"selected ontology must be known")
	}
	canonical, err := ProjectOntologyIdentity(identity)
	if err != nil {
		return ontology.OntologyIdentity{}, err
	}
	if canonical != value {
		return ontology.OntologyIdentity{}, fmt.Errorf(
			"selected ontology must use the canonical identity projection")
	}
	return identity, nil
}

func decodeWorkspaceOpaqueID(name, value string) (shoal.ID, error) {
	decoded, err := decodeID(value)
	if err != nil {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, name+" "+err.Error())
	}
	if encodeID(decoded) != value {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			name+" must be canonical unpadded base64url",
		)
	}
	if err := shoal.ValidateRequiredID(name, decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func workspaceSettingsResponse(
	value workspace.Settings,
) (WorkspaceSettingsResponse, error) {
	settings := WorkspaceSettingsNarrowing{
		Budgets: WorkspaceSettingsBudgets{
			RetrievalTopK: value.Narrowing.Budgets.RetrievalTopK,
			GraphDepth:    value.Narrowing.Budgets.GraphDepth,
			GraphFanout:   value.Narrowing.Budgets.GraphFanout,
			GraphNodes:    value.Narrowing.Budgets.GraphNodes,
			OutputBytes:   value.Narrowing.Budgets.OutputBytes,
		},
	}
	if value.Narrowing.AllowedOperations.Present {
		values := make([]string, 0, len(value.Narrowing.AllowedOperations.Values))
		for _, operation := range value.Narrowing.AllowedOperations.Values {
			values = append(values, string(operation))
		}
		settings.AllowedOperations = &values
	}
	var err error
	settings.PermittedSourceIDs, err = encodeComponentSelection(
		value.Narrowing.PermittedSourceIDs)
	if err != nil {
		return WorkspaceSettingsResponse{}, err
	}
	settings.PermittedPolicyIDs, err = encodeComponentSelection(
		value.Narrowing.PermittedPolicyIDs)
	if err != nil {
		return WorkspaceSettingsResponse{}, err
	}
	for _, policy := range value.Narrowing.OutputPolicies {
		sourceID, err := auth.EncodeComponent(policy.SourceID())
		if err != nil {
			return WorkspaceSettingsResponse{}, err
		}
		grantPolicyID, err := auth.EncodeComponent(policy.GrantPolicyID())
		if err != nil {
			return WorkspaceSettingsResponse{}, err
		}
		settings.OutputPolicies = append(
			settings.OutputPolicies,
			WorkspaceOutputPolicy{
				SourceID: sourceID, GrantPolicyID: grantPolicyID,
				Epoch: policy.Epoch(),
			},
		)
	}
	if value.Narrowing.SelectedOntology.Present {
		projected, err := ProjectOntologyIdentity(
			value.Narrowing.SelectedOntology.Identity)
		if err != nil {
			return WorkspaceSettingsResponse{}, err
		}
		settings.SelectedOntology = &projected
	}
	return WorkspaceSettingsResponse{
		WorkspaceID:    encodeID(value.WorkspaceID),
		SettingsID:     encodeID(value.SettingsID),
		Owner:          encodeID(value.Owner),
		Revision:       value.Revision,
		LastMutationID: encodeID(value.LastMutationID),
		Settings:       settings,
	}, nil
}

func decodeComponentSelection(values *[]string) (workspace.IDSelection, error) {
	if values == nil {
		return workspace.IDSelection{}, nil
	}
	selection := workspace.IDSelection{Present: true}
	for index, value := range *values {
		decoded, err := auth.DecodeComponent(value)
		if err != nil {
			return workspace.IDSelection{}, fmt.Errorf("[%d]: %w", index, err)
		}
		selection.Values = append(selection.Values, decoded)
	}
	return selection, nil
}

func encodeComponentSelection(
	selection workspace.IDSelection,
) (*[]string, error) {
	if !selection.Present {
		return nil, nil
	}
	values := make([]string, 0, len(selection.Values))
	for _, value := range selection.Values {
		encoded, err := auth.EncodeComponent(value)
		if err != nil {
			return nil, err
		}
		values = append(values, encoded)
	}
	return &values, nil
}

func requireSameOrigin(request *http.Request) error {
	site := request.Header.Get("Sec-Fetch-Site")
	switch site {
	case "", "none", "same-origin":
	default:
		return shoal.NewError(
			shoal.ErrorUnauthorized, "cross-origin settings mutation denied")
	}
	origin := request.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return shoal.NewError(
			shoal.ErrorUnauthorized, "cross-origin settings mutation denied")
	}
	if !sameOriginAuthority(parsed, request.Host) {
		return shoal.NewError(
			shoal.ErrorUnauthorized, "cross-origin settings mutation denied")
	}
	if site != "same-origin" {
		requestScheme := "http"
		if request.TLS != nil {
			requestScheme = "https"
		}
		if !strings.EqualFold(parsed.Scheme, requestScheme) {
			return shoal.NewError(
				shoal.ErrorUnauthorized,
				"cross-origin settings mutation denied",
			)
		}
	}
	return nil
}

func sameOriginAuthority(origin *url.URL, requestAuthority string) bool {
	requestURL, err := url.Parse("//" + requestAuthority)
	if err != nil || requestURL.Host == "" || requestURL.User != nil ||
		requestURL.Path != "" || requestURL.RawQuery != "" ||
		requestURL.Fragment != "" {
		return false
	}
	return strings.EqualFold(origin.Hostname(), requestURL.Hostname()) &&
		originPort(origin.Scheme, origin.Port()) ==
			originPort(origin.Scheme, requestURL.Port())
}

func originPort(scheme, port string) string {
	if port != "" {
		return port
	}
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
