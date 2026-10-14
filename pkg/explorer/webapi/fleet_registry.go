// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package webapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FleetRegistryProvider is the production provider seam implemented directly
// by fleet.Service. The transport never serializes the resolved host executor.
type FleetRegistryProvider interface {
	Register(context.Context, fleet.RegisterRequest) (fleet.Descriptor, error)
	Heartbeat(context.Context, fleet.HeartbeatRequest) (fleet.Descriptor, error)
	Revoke(context.Context, fleet.RevokeRequest) (fleet.Descriptor, error)
	Resolve(context.Context, fleet.ResolveRequest) (fleet.Resolved, error)
	List(context.Context, fleet.ListRequest) (fleet.ListPage, error)
}

// NewFleetRegistryHandler returns the fleet registry HTTP surface without
// adding another authentication boundary. Mount it through
// Handler.MountAuthenticated at /api/v1/fleet/.
func NewFleetRegistryHandler(provider FleetRegistryProvider) (http.Handler, error) {
	if provider == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet registry provider is required")
	}
	mux := http.NewServeMux()
	mountFleetRegistry(mux, provider)
	return mux, nil
}

// MountFleetRegistry is retained for registry-only tests and compatibility.
// Hosted startup must instead mount NewFleetHandler once at FleetRoutePrefix
// so registry and dispatch share one authenticated route subtree.
func (h *Handler) MountFleetRegistry(provider FleetRegistryProvider) error {
	if h == nil || isAbsentInterface(provider) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "fleet registry provider is required")
	}
	if isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "fleet registry requires authenticated transport")
	}
	mountFleetRegistry(h.mux, provider)
	return nil
}

func mountFleetRegistry(mux *http.ServeMux, provider FleetRegistryProvider) {
	mux.HandleFunc("POST /api/v1/fleet/agents", func(writer http.ResponseWriter, request *http.Request) {
		var input fleetRegisterWire
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		decoded, err := input.decode()
		if err != nil {
			writeError(writer, err)
			return
		}
		descriptor, err := provider.Register(request.Context(), decoded)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeResponse(writer, http.StatusCreated, encodeFleetDescriptor(descriptor))
	})
	mux.HandleFunc("POST /api/v1/fleet/agents/{agent}/heartbeat", func(writer http.ResponseWriter, request *http.Request) {
		id, err := decodeID(request.PathValue("agent"))
		if err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, "agent ID "+err.Error()))
			return
		}
		var input fleetHeartbeatWire
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		decoded, err := input.decode(id)
		if err != nil {
			writeError(writer, err)
			return
		}
		descriptor, err := provider.Heartbeat(request.Context(), decoded)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeResponse(writer, http.StatusOK, encodeFleetDescriptor(descriptor))
	})
	mux.HandleFunc("POST /api/v1/fleet/agents/{agent}/revoke", func(writer http.ResponseWriter, request *http.Request) {
		id, err := decodeID(request.PathValue("agent"))
		if err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, "agent ID "+err.Error()))
			return
		}
		var input fleetRevokeWire
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		decoded, err := input.decode(id)
		if err != nil {
			writeError(writer, err)
			return
		}
		descriptor, err := provider.Revoke(request.Context(), decoded)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeResponse(writer, http.StatusOK, encodeFleetDescriptor(descriptor))
	})
	mux.HandleFunc("POST /api/v1/fleet/agents/{agent}/resolve", func(writer http.ResponseWriter, request *http.Request) {
		id, err := decodeID(request.PathValue("agent"))
		if err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, "agent ID "+err.Error()))
			return
		}
		var input fleetRequestContextWire
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		requestContext, err := input.decode()
		if err != nil {
			writeError(writer, err)
			return
		}
		resolved, err := provider.Resolve(request.Context(), fleet.ResolveRequest{
			Context: requestContext, ID: id,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeResponse(writer, http.StatusOK, encodeFleetDescriptor(resolved.Descriptor))
	})
	mux.HandleFunc("POST /api/v1/fleet/agents/resolve", func(writer http.ResponseWriter, request *http.Request) {
		var input fleetListWire
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		requestContext, err := input.Context.decode()
		if err != nil {
			writeError(writer, err)
			return
		}
		page, err := provider.List(request.Context(), fleet.ListRequest{
			Context:   requestContext,
			SourceIDs: cloneWireBytes(input.SourceIDs),
			PolicyIDs: cloneWireBytes(input.PolicyIDs),
			Cursor:    append([]byte(nil), input.Cursor...),
			Limit:     input.Limit,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		response := make([]fleetDescriptorWire, len(page.Descriptors))
		for i := range page.Descriptors {
			response[i] = encodeFleetDescriptor(page.Descriptors[i])
		}
		writeResponse(writer, http.StatusOK, struct {
			Agents []fleetDescriptorWire `json:"agents"`
			Next   []byte                `json:"next,omitempty"`
		}{Agents: response, Next: page.Next})
	})
}

type fleetRequestContextWire struct {
	RequestID     string    `json:"request_id"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	ReasonCode    string    `json:"reason_code"`
	ReasonDetail  string    `json:"reason_detail,omitempty"`
	Deadline      time.Time `json:"deadline"`
}

type fleetSpecWire struct {
	ID                  string             `json:"id"`
	ParentID            string             `json:"parent_id,omitempty"`
	AuthorizationDomain []byte             `json:"authorization_domain"`
	Scopes              []fleet.Scope      `json:"scopes"`
	ExecutorRef         string             `json:"executor_ref"`
	Capabilities        []fleet.Capability `json:"capabilities"`
	LeaseExpiresAt      time.Time          `json:"lease_expires_at"`
}

type fleetRegisterWire struct {
	Context            fleetRequestContextWire `json:"context"`
	RegistrationKey    string                  `json:"registration_key"`
	ExpectedGeneration int64                   `json:"expected_generation"`
	Descriptor         fleetSpecWire           `json:"descriptor"`
}

type fleetHeartbeatWire struct {
	Context            fleetRequestContextWire `json:"context"`
	RegistrationKey    string                  `json:"registration_key"`
	ExpectedGeneration int64                   `json:"expected_generation"`
	LeaseExpiresAt     time.Time               `json:"lease_expires_at"`
}

type fleetRevokeWire struct {
	Context            fleetRequestContextWire `json:"context"`
	RegistrationKey    string                  `json:"registration_key"`
	ExpectedGeneration int64                   `json:"expected_generation"`
}

type fleetListWire struct {
	Context   fleetRequestContextWire `json:"context"`
	SourceIDs [][]byte                `json:"source_ids,omitempty"`
	PolicyIDs [][]byte                `json:"policy_ids,omitempty"`
	Cursor    []byte                  `json:"cursor,omitempty"`
	Limit     int                     `json:"limit"`
}

type fleetDescriptorWire struct {
	ID                  string             `json:"id"`
	Generation          int64              `json:"generation"`
	Subject             string             `json:"subject"`
	Actor               string             `json:"actor"`
	ParentID            string             `json:"parent_id,omitempty"`
	AuthorizationDomain []byte             `json:"authorization_domain"`
	Scopes              []fleet.Scope      `json:"scopes"`
	ExecutorRef         string             `json:"executor_ref"`
	Capabilities        []fleet.Capability `json:"capabilities"`
	LeaseExpiresAt      time.Time          `json:"lease_expires_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	RevokedAt           time.Time          `json:"revoked_at,omitempty"`
}

func (w fleetRegisterWire) decode() (fleet.RegisterRequest, error) {
	contextValue, err := w.Context.decode()
	if err != nil {
		return fleet.RegisterRequest{}, err
	}
	key, err := decodeID(w.RegistrationKey)
	if err != nil {
		return fleet.RegisterRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "registration key "+err.Error())
	}
	id, err := decodeID(w.Descriptor.ID)
	if err != nil {
		return fleet.RegisterRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent ID "+err.Error())
	}
	parent, err := decodeOptionalID(w.Descriptor.ParentID)
	if err != nil {
		return fleet.RegisterRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "parent agent ID "+err.Error())
	}
	return fleet.RegisterRequest{
		Context: contextValue, RegistrationKey: key,
		ExpectedGeneration: w.ExpectedGeneration,
		Spec: fleet.Spec{
			ID: id, ParentID: parent,
			AuthorizationDomain: append([]byte(nil), w.Descriptor.AuthorizationDomain...),
			Scopes:              cloneFleetScopes(w.Descriptor.Scopes),
			ExecutorRef:         w.Descriptor.ExecutorRef,
			Capabilities:        cloneFleetCapabilities(w.Descriptor.Capabilities),
			LeaseExpiresAt:      w.Descriptor.LeaseExpiresAt,
		},
	}, nil
}

func (w fleetHeartbeatWire) decode(id shoal.ID) (fleet.HeartbeatRequest, error) {
	contextValue, err := w.Context.decode()
	if err != nil {
		return fleet.HeartbeatRequest{}, err
	}
	key, err := decodeID(w.RegistrationKey)
	if err != nil {
		return fleet.HeartbeatRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "registration key "+err.Error())
	}
	return fleet.HeartbeatRequest{
		Context: contextValue, RegistrationKey: key, ID: id,
		ExpectedGeneration: w.ExpectedGeneration, LeaseExpiresAt: w.LeaseExpiresAt,
	}, nil
}

func (w fleetRevokeWire) decode(id shoal.ID) (fleet.RevokeRequest, error) {
	contextValue, err := w.Context.decode()
	if err != nil {
		return fleet.RevokeRequest{}, err
	}
	key, err := decodeID(w.RegistrationKey)
	if err != nil {
		return fleet.RevokeRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "registration key "+err.Error())
	}
	return fleet.RevokeRequest{
		Context: contextValue, RegistrationKey: key, ID: id,
		ExpectedGeneration: w.ExpectedGeneration,
	}, nil
}

func (w fleetRequestContextWire) decode() (fleet.RequestContext, error) {
	requestID, err := decodeID(w.RequestID)
	if err != nil {
		return fleet.RequestContext{}, shoal.NewError(shoal.ErrorInvalidArgument, "request ID "+err.Error())
	}
	correlationID, err := decodeOptionalID(w.CorrelationID)
	if err != nil {
		return fleet.RequestContext{}, shoal.NewError(shoal.ErrorInvalidArgument, "correlation ID "+err.Error())
	}
	return fleet.RequestContext{
		RequestID: requestID, CorrelationID: correlationID,
		ReasonCode: w.ReasonCode, ReasonDetail: w.ReasonDetail, Deadline: w.Deadline,
	}, nil
}

func encodeFleetDescriptor(descriptor fleet.Descriptor) fleetDescriptorWire {
	return fleetDescriptorWire{
		ID: encodeFleetID(descriptor.ID), Generation: descriptor.Generation,
		Subject: encodeFleetID(descriptor.Subject), Actor: encodeFleetID(descriptor.Actor),
		ParentID:            encodeFleetID(descriptor.ParentID),
		AuthorizationDomain: append([]byte(nil), descriptor.AuthorizationDomain...),
		Scopes:              cloneFleetScopes(descriptor.Scopes), ExecutorRef: descriptor.ExecutorRef,
		Capabilities:   cloneFleetCapabilities(descriptor.Capabilities),
		LeaseExpiresAt: descriptor.LeaseExpiresAt, UpdatedAt: descriptor.UpdatedAt,
		RevokedAt: descriptor.RevokedAt,
	}
}

func encodeFleetID(id shoal.ID) string {
	if id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func cloneFleetScopes(input []fleet.Scope) []fleet.Scope {
	result := make([]fleet.Scope, len(input))
	for i := range input {
		result[i] = fleet.Scope{
			SourceID: append([]byte(nil), input[i].SourceID...),
			PolicyID: append([]byte(nil), input[i].PolicyID...),
		}
	}
	return result
}

func cloneFleetCapabilities(input []fleet.Capability) []fleet.Capability {
	result := make([]fleet.Capability, len(input))
	for i := range input {
		result[i].Name = input[i].Name
		result[i].Actions = make([]fleet.Action, len(input[i].Actions))
		for j := range input[i].Actions {
			result[i].Actions[j] = fleet.Action{
				Name: input[i].Actions[j].Name,
				// Copied, not aliased: a returned descriptor must not be
				// usable to mutate the registry's declared effects.
				Effects:      append(fleet.Effects(nil), input[i].Actions[j].Effects...),
				InputSchema:  append(json.RawMessage(nil), input[i].Actions[j].InputSchema...),
				OutputSchema: append(json.RawMessage(nil), input[i].Actions[j].OutputSchema...),
				// Dropping it here would register, and report, an action
				// that no longer requires approval.
				RequiresApproval: input[i].Actions[j].RequiresApproval,
				// Likewise for attestation.
				RequiresAttestation: input[i].Actions[j].RequiresAttestation,
			}
		}
	}
	return result
}

func cloneWireBytes(input [][]byte) [][]byte {
	result := make([][]byte, len(input))
	for i := range input {
		result[i] = append([]byte(nil), input[i]...)
	}
	return result
}
