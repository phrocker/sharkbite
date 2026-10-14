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

package decisionlinear

import (
	"runtime"
	"testing"
)

// TestIdentityForMatchesNew: the pure identity function is exactly what New
// computes for the running toolchain, and the toolchain enters it.
func TestIdentityForMatchesNew(t *testing.T) {
	b := modelBytes("task")
	p := load(t, b)
	want, err := IdentityFor(digest(b), "features:1", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if p.Identity().ID() != want.ID() || p.Identity().Config() != want.Config() {
		t.Fatalf("New identity %s, IdentityFor %s", p.Identity().ID(), want.ID())
	}
	other, err := IdentityFor(digest(b), "features:1", "go0.0.0", runtime.GOOS, runtime.GOARCH)
	if err != nil || other.ID() == want.ID() {
		t.Fatal("the toolchain does not enter the identity")
	}
}
