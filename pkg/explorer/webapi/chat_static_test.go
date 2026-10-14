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

package webapi_test

import (
	"strings"
	"testing"
)

func TestStaticWorkspaceMountsRecordedChatGovernanceUI(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	for _, id := range []string{
		`id="chat-form"`,
		`id="workspace-id"`,
		`id="settings-json"`,
		`id="lens-select"`,
		`id="provenance-results"`,
	} {
		if !strings.Contains(index, id) {
			t.Fatalf("workspace HTML missing %s", id)
		}
	}

	app := readStaticAsset(t, "static/app.js")
	for _, required := range []string{
		`"/api/v1/ask"`,
		`"/api/v1/provenance"`,
		`"More provenance"`,
		`next.next_cursor`,
		`wirePath(state.workspaceID, "/lens")`,
		`headers["Shoal-Workspace-ID"] = state.workspaceID`,
		`value.durably_recorded === true`,
		`function resetIdentityBoundUI()`,
		`parsed.protocol === "http:" || parsed.protocol === "https:"`,
	} {
		if !strings.Contains(app, required) {
			t.Fatalf("workspace JavaScript missing %q", required)
		}
	}
}
