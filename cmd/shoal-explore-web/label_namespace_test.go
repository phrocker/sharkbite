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

package main

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestDevelopmentIdentityIsOutsideLabelNamespace: the -dev-auth identity
// grants workspaceGrantPolicyID, which is also the static ingest source
// policy. Label grants come only from the label grant file, so it must never
// be a label policy ID.
func TestDevelopmentIdentityIsOutsideLabelNamespace(t *testing.T) {
	if auth.IsLabelPolicyID(workspaceGrantPolicyID) {
		t.Fatalf("development grant policy %q is in the label namespace",
			workspaceGrantPolicyID)
	}
}
