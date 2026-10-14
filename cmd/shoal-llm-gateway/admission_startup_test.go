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

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAnAdmissionURLTheClientRefusesFailsAtStartup pins that the admission
// client is built in run(), not on first use. Built lazily, each of these
// would start cleanly, pass both probes, and then answer every call
// plane_unavailable. A query used to be carried silently onto every admission
// path; userinfo would put a second credential on the wire.
func TestAnAdmissionURLTheClientRefusesFailsAtStartup(t *testing.T) {
	for name, url := range map[string]string{
		"userinfo":    "https://user:secret@workspace.test",
		"query":       "https://workspace.test/?tenant=a",
		"empty query": "https://workspace.test/?",
		"fragment":    "https://workspace.test/#frag",
	} {
		detail := refusal(t, proxyArgs("-admission-url", url))
		if !strings.Contains(detail, "-admission-url") ||
			!strings.Contains(detail, "invalid admission client configuration") {
			t.Errorf("%s: refusal = %q", name, detail)
		}
	}
}

// TestTheGatewayDoesNotLinkTheDecisionPlane pins the #390 separation in the
// build graph: the proxy handles untrusted prompt content, so it must not link
// the fleet package (the policy store and dispatch) or any internal package
// beyond an explicit allowlist. It speaks to the plane only through
// pkg/admission/api.
func TestTheGatewayDoesNotLinkTheDecisionPlane(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not available in this environment, so the " +
			"build graph cannot be listed; the separation is unchecked here")
	}
	output, err := exec.Command(goTool, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	const module = "github.com/phrocker/shoal-oss/"
	allowedInternal := map[string]bool{
		module + "internal/healthsurface": true,
		// A standard-library-only decoder (#426): a request carrying
		// shoal_attribution is decoded strictly so duplicate keys cannot make
		// the gateway and the provider read different values. It holds no
		// policy and no corpus.
		module + "internal/strictjson": true,
		// The Prometheus text-format content type and label escaping the
		// health surface's optional /metrics route uses. Standard-library
		// only (strings); it holds no policy, no corpus and no state.
		module + "internal/promtext": true,
	}
	sawAPI := false
	for _, dependency := range strings.Fields(string(output)) {
		switch {
		case dependency == module+"pkg/admission/api":
			sawAPI = true
		case strings.HasPrefix(dependency, module+"pkg/explorer"):
			t.Errorf("the gateway links %s", dependency)
		case strings.Contains("/"+dependency+"/", "/internal/") &&
			strings.HasPrefix(dependency, module) &&
			!allowedInternal[dependency]:
			t.Errorf("the gateway links %s, outside the internal allowlist", dependency)
		}
	}
	if !sawAPI {
		t.Fatal("the gateway does not link pkg/admission/api; the listing " +
			"is not the graph this test means to check")
	}
}
