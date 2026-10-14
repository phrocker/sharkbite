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

package effectsgateway

import (
	"os/exec"
	"strings"
	"testing"
)

// TestTheGatewayDoesNotLinkTheExplorer pins the #446 separation in the build
// graph: the effects gateway performs real external effects with a target
// credential, so it must not link the explorer — the fleet package (the
// dispatch service, its store and policy), the web API, or any other
// pkg/explorer package — nor any internal package beyond an explicit
// allowlist. It speaks to the explorer only through pkg/dispatch/api and
// pkg/attestation/api. Both this package and the production binary are
// checked.
func TestTheGatewayDoesNotLinkTheExplorer(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not available in this environment, so the " +
			"build graph cannot be listed; the separation is unchecked here")
	}
	const module = "github.com/phrocker/shoal-oss/"
	allowedInternal := map[string]bool{
		module + "internal/effectsgateway":            true,
		module + "internal/effectsgateway/gatewaycmd": true,
		// The single-instance lock on the unrecorded log directory.
		module + "internal/dirlock": true,
		// The Prometheus text-format content type and label escaping the
		// health surface's optional /metrics route uses.
		module + "internal/promtext": true,
		// The role-operation names the command reports under.
		module + "internal/roleops": true,
	}
	for _, target := range []string{".", "../../cmd/shoal-gateway"} {
		output, err := exec.Command(goTool, "list", "-deps", target).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", target, err)
		}
		sawAPI := false
		for _, dependency := range strings.Fields(string(output)) {
			switch {
			case dependency == module+"pkg/dispatch/api":
				sawAPI = true
			case strings.HasPrefix(dependency, module+"pkg/explorer"):
				t.Errorf("%s links %s", target, dependency)
			case strings.Contains("/"+dependency+"/", "/internal/") &&
				strings.HasPrefix(dependency, module) &&
				!allowedInternal[dependency]:
				t.Errorf("%s links %s, outside the internal allowlist", target, dependency)
			}
		}
		if !sawAPI {
			t.Fatalf("%s does not link pkg/dispatch/api; the listing is not "+
				"the graph this test means to check", target)
		}
	}
}
