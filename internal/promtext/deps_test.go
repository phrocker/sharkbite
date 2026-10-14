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

package promtext

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// TestPromtextLinksOnlyTheStandardLibrary pins the claim the LLM gateway's
// internal allowlist admits this package on: it links nothing but the
// standard library. The gateway's own test only refuses internal/ and
// pkg/explorer packages, so a pkg/ or third-party import here would reach
// the gateway, which handles untrusted prompt content, without failing
// anything. This test is where that fails.
func TestPromtextLinksOnlyTheStandardLibrary(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not available in this environment, so the " +
			"build graph cannot be listed; the dependency claim is unchecked here")
	}
	// The command runs in this package's directory, so "." names promtext.
	// -deps lists only the non-test build graph, which is what the gateway
	// links.
	output, err := exec.Command(goTool, "list", "-deps", "-json", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps -json: %v", err)
	}

	const self = "github.com/phrocker/shoal-oss/internal/promtext"
	sawSelf := false
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var pkg struct {
			ImportPath string
			Standard   bool
		}
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		switch {
		case pkg.ImportPath == self:
			sawSelf = true
		case !pkg.Standard:
			t.Errorf("promtext links %s, which is not in the standard library", pkg.ImportPath)
		}
	}
	if !sawSelf {
		t.Fatal("go list did not report promtext itself; the listing is not " +
			"the graph this test means to check")
	}
}
