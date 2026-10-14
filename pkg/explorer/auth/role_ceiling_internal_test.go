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

package auth

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPreflightRejectsMalformedDecisionAboveServiceRoleCeiling(t *testing.T) {
	now := time.Date(2026, 8, 26, 21, 0, 0, 0, time.UTC)
	decision := Decision{
		subject:                "subject",
		actor:                  "actor",
		domain:                 []byte("domain"),
		operations:             []Operation{OperationIngest},
		policyGeneration:       1,
		expiresAt:              now.Add(time.Hour),
		requestID:              "request",
		serviceRole:            ServiceRoleDataRead,
		serviceCeilingIdentity: "ceiling",
	}

	if _, err := decision.preflight(OperationIngest, now); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("preflight above role ceiling = %v", err)
	}
}
