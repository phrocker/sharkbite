/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *   https://www.apache.org/licenses/LICENSE-2.0
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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// The synthetic SVM job's output, unchanged, is a one-file weights bundle.
func TestSyntheticSVMServesAsOneFileBundle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	model := modelBytes("task")
	if err := os.WriteFile(filepath.Join(root, "model.json"), model, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := weightsbundle.Build(root, map[string]string{"model.json": weightsbundle.RoleWeights})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := weightsbundle.Stage(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := LoadBundle(root, digest, "release:1")
	if err != nil {
		t.Fatal(err)
	}
	direct := load(t, model)
	if provider.Identity().ID() != direct.Identity().ID() {
		t.Fatal("bundle-served identity differs from direct load")
	}
	if _, err := LoadBundle(root, "0"+digest[1:], "release:1"); !errors.Is(err, weightsbundle.ErrVerification) {
		t.Fatalf("mismatched release digest loaded: %v", err)
	}
	// Altered after publication: refused before parsing.
	if err := os.WriteFile(filepath.Join(root, "model.json"), modelBytes("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(root, digest, "release:1"); !errors.Is(err, weightsbundle.ErrVerification) {
		t.Fatalf("altered bundle loaded: %v", err)
	}
}
