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
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// LoadBundle serves a published weights bundle whose weights file is a
// linear-svm artifact. Every member digest and the bundle digest are
// verified before a byte of the model is parsed; expectedDigest, when set,
// must equal the bundle digest so a release serves exactly the bundle its
// record binds. The existing one-file SVM artifact is the smallest bundle.
func LoadBundle(root string, expectedDigest string, release shoal.ID) (*Provider, error) {
	manifest, digest, err := weightsbundle.Load(root)
	if err != nil {
		return nil, err
	}
	if expectedDigest != "" && expectedDigest != digest {
		return nil, fmt.Errorf("%w: bundle digest %s does not match release digest %s", weightsbundle.ErrVerification, digest, expectedDigest)
	}
	weights, ok := manifest.File(weightsbundle.RoleWeights)
	if !ok {
		return nil, fmt.Errorf("%w: no weights file", weightsbundle.ErrVerification)
	}
	if weights.Size > MaxModelBytes {
		return nil, errors.New("weights file exceeds model bound")
	}
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(weights.Path)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", weightsbundle.ErrVerification, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxModelBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", weightsbundle.ErrVerification, err)
	}
	return New(Config{ModelBytes: b, ExpectedSHA256: weights.SHA256, ReleaseID: release})
}
