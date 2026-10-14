// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package deploy holds dependency-free structural regression tests for the
// static Kubernetes manifests and Helm chart under deploy/. The repository
// deliberately carries no YAML-parsing dependency (see cmd/shoal-embed's
// zero-dependency observability surface for the same philosophy), so these
// tests validate structure with plain string/line scanning rather than a
// full YAML unmarshal. They exist to catch manifest regressions (a
// PodDisruptionBudget silently dropped, a rollout strategy reverted, an
// existing securityContext field removed) directly in `go test`, without
// requiring kubectl or a live cluster.
//
// go test-independent evidence — `helm lint` / `helm template` output —
// belongs in the PR body; TestHelmChartLintsAndRenders in manifest_test.go
// additionally runs both, but only when a `helm` binary is on PATH (skipped
// otherwise, e.g. in this repository's current CI, which has neither
// kubectl nor helm installed).
//
// This file exists (with no declarations beyond the package clause) purely
// so the package has a non-test Go file: `go build` treats a package that
// contains only _test.go files as an error ("no non-test Go files"), even
// though `go vet` and `go test` handle it fine.
package deploy
