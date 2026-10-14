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

package explorerfleet

import (
	"context"
	"reflect"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DeliveryLeaseValidator adapts the registry to the fleet-events structural
// lease interface without making the public events package import this
// internal package. The supplied decision is never trusted directly: it must
// exactly match a fresh decision resolved from the bound context.
type DeliveryLeaseValidator struct {
	registry *fleet.Service
	resolver auth.Resolver
}

type fleetEventsLeaseContract interface {
	ValidateDelivery(context.Context, []byte, auth.Decision) error
}

var _ fleetEventsLeaseContract = (*DeliveryLeaseValidator)(nil)

func NewDeliveryLeaseValidator(
	registry *fleet.Service,
	resolver auth.Resolver,
) (*DeliveryLeaseValidator, error) {
	if registry == nil || resolver == nil ||
		(reflect.ValueOf(resolver).Kind() == reflect.Pointer &&
			reflect.ValueOf(resolver).IsNil()) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"delivery lease validator dependencies are required",
		)
	}
	return &DeliveryLeaseValidator{registry: registry, resolver: resolver}, nil
}

func (v *DeliveryLeaseValidator) ValidateDelivery(
	ctx context.Context,
	agentID []byte,
	supplied auth.Decision,
) error {
	fresh, err := v.resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	freshFingerprint, err := auth.AuthorizationFingerprint(fresh)
	if err != nil {
		return err
	}
	suppliedFingerprint, err := auth.AuthorizationFingerprint(supplied)
	if err != nil {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"delivery authorization does not match the bound request",
		)
	}
	if freshFingerprint != suppliedFingerprint ||
		fresh.RequestID() != supplied.RequestID() ||
		fresh.CorrelationID() != supplied.CorrelationID() ||
		fresh.AuditPurpose() != supplied.AuditPurpose() ||
		!fresh.AuthenticationExpires().Equal(supplied.AuthenticationExpires()) {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"delivery authorization does not match the bound request",
		)
	}
	return v.registry.ValidateCurrentDelivery(ctx, shoal.ID(string(agentID)))
}
