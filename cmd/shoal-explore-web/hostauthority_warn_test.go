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
	"strings"
	"testing"
)

// TestHostAuthorityStartupWarningFiresOnPublicBindWithoutAllowedHost proves the
// warning fires exactly when the workspace will fail closed on every request —
// a non-loopback or wildcard bind with no -allowed-host — and names both the
// bound address and the 421 consequence so the operator can act. This is the
// guarded scenario a reviewer flagged as the most common trip.
func TestHostAuthorityStartupWarningFiresOnPublicBindWithoutAllowedHost(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8098", "[::]:8098", "10.0.0.5:8098"} {
		warning := hostAuthorityStartupWarning(address, "")
		if warning == "" {
			t.Fatalf("no warning for public bind %s without -allowed-host", address)
		}
		if !strings.Contains(warning, address) {
			t.Fatalf("warning for %s did not name the bound address: %q", address, warning)
		}
		if !strings.Contains(warning, "421") || !strings.Contains(warning, "-allowed-host") {
			t.Fatalf("warning for %s omitted the reason or the remedy: %q", address, warning)
		}
	}
}

// TestHostAuthorityStartupWarningSilentWhenServing proves the warning stays
// silent for a configuration that will actually serve: the loopback default
// (no config needed), and any bind once -allowed-host is set. A warning here
// would be false noise on a working deployment.
func TestHostAuthorityStartupWarningSilentWhenServing(t *testing.T) {
	silentCases := []struct {
		name    string
		address string
		allowed string
	}{
		{"loopback default", "127.0.0.1:8098", ""},
		{"ipv6 loopback default", "[::1]:8098", ""},
		{"public bind with allowed-host", "0.0.0.0:8098", "explorer.example.test"},
		{"loopback with allowed-host", "127.0.0.1:8098", "explorer.example.test"},
	}
	for _, testCase := range silentCases {
		if warning := hostAuthorityStartupWarning(
			testCase.address, testCase.allowed); warning != "" {
			t.Fatalf("%s produced a spurious warning: %q", testCase.name, warning)
		}
	}
}
