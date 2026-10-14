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

// See doc.go for the package-level doc comment.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// yamlDoc is one `---`-delimited document from a multi-document plain YAML
// file, along with its kind/name pulled out via a top-level-field regexp
// (deliberately not a full YAML parse — see the package doc comment).
type yamlDoc struct {
	kind string
	name string
	text string
}

var (
	kindPattern = regexp.MustCompile(`(?m)^kind:\s*(\S+)\s*$`)
	namePattern = regexp.MustCompile(`(?m)^\s*name:\s*(\S+)\s*$`)
)

// splitYAMLDocuments splits content on lines that are exactly "---" (a
// YAML document separator), which is sufficient for these manifests: none
// of them contain a literal "---" inside a string value.
func splitYAMLDocuments(content string) []yamlDoc {
	var docs []yamlDoc
	var cur strings.Builder
	flush := func() {
		text := cur.String()
		if strings.TrimSpace(text) != "" {
			doc := yamlDoc{text: text}
			if m := kindPattern.FindStringSubmatch(text); m != nil {
				doc.kind = m[1]
			}
			// The first top-level "name:" in a document is metadata.name
			// for every manifest in this repository (none of these
			// documents have another unindented "name:" field before it).
			if m := namePattern.FindStringSubmatch(text); m != nil {
				doc.name = m[1]
			}
			docs = append(docs, doc)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "---" {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	flush()
	return docs
}

func readManifest(t *testing.T, relPath string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return string(b)
}

// requireDoc finds the single document with the given kind and metadata
// name, failing the test with the full list of (kind, name) pairs found if
// it's missing — so a failure immediately shows what the file actually
// contains instead of just "not found".
func requireDoc(t *testing.T, docs []yamlDoc, kind, name string) yamlDoc {
	t.Helper()
	for _, d := range docs {
		if d.kind == kind && d.name == name {
			return d
		}
	}
	var found []string
	for _, d := range docs {
		found = append(found, d.kind+"/"+d.name)
	}
	t.Fatalf("no %s named %q found; documents present: %v", kind, name, found)
	return yamlDoc{}
}

func requireContains(t *testing.T, doc yamlDoc, want string) {
	t.Helper()
	if !strings.Contains(doc.text, want) {
		t.Errorf("%s/%s: want to contain %q, got:\n%s", doc.kind, doc.name, want, doc.text)
	}
}

// TestWriteTierManifestHasDisruptionAndRolloutHardening guards the
// write-tier plain-YAML manifest's disruption/rollout contract: a
// PodDisruptionBudget exists and targets the StatefulSet's own labels, and
// the StatefulSet declares its rollout strategy explicitly instead of
// relying on the apps/v1 default. It also re-asserts the pre-existing
// security/health hardening from PR #79/#100 to catch accidental removal.
func TestWriteTierManifestHasDisruptionAndRolloutHardening(t *testing.T) {
	docs := splitYAMLDocuments(readManifest(t, "k8s/write-tier.yaml"))
	if len(docs) != 3 {
		t.Fatalf("write-tier.yaml has %d documents, want 3 (Service, StatefulSet, PodDisruptionBudget)", len(docs))
	}

	sts := requireDoc(t, docs, "StatefulSet", "shoal-embed")
	requireContains(t, sts, "updateStrategy:")
	requireContains(t, sts, "runAsNonRoot: true")
	requireContains(t, sts, "allowPrivilegeEscalation: false")
	requireContains(t, sts, `drop: ["ALL"]`)
	requireContains(t, sts, "seccompProfile:")
	requireContains(t, sts, "readinessProbe:")
	requireContains(t, sts, "livenessProbe:")
	requireContains(t, sts, "terminationGracePeriodSeconds: 50")

	pdb := requireDoc(t, docs, "PodDisruptionBudget", "shoal-embed")
	requireContains(t, pdb, "maxUnavailable: 0")
	requireContains(t, pdb, "app.kubernetes.io/component: write-tier")
}

// TestReadFleetManifestHasDisruptionAndRolloutHardening is
// TestWriteTierManifestHasDisruptionAndRolloutHardening's read-fleet
// counterpart. The HTTP readiness probe is semantic: it covers ZooKeeper,
// metadata, storage, listener state, and scan-session admission.
func TestReadFleetManifestHasDisruptionAndRolloutHardening(t *testing.T) {
	docs := splitYAMLDocuments(readManifest(t, "k8s/read-fleet.yaml"))
	if len(docs) != 3 {
		t.Fatalf("read-fleet.yaml has %d documents, want 3 (Service, Deployment, PodDisruptionBudget)", len(docs))
	}

	dep := requireDoc(t, docs, "Deployment", "shoal-read")
	requireContains(t, dep, "strategy:")
	// maxUnavailable: 0 (not the apps/v1-default-shaped "1") is what
	// actually keeps all 3 replicas available throughout a rollout: the
	// controller cannot remove an old pod until the maxSurge pod is
	// ready, since it has zero unavailable budget to spend in between.
	requireContains(t, dep, "maxUnavailable: 0")
	requireContains(t, dep, "maxSurge: 1")
	requireContains(t, dep, "podAntiAffinity:")
	requireContains(t, dep, "path: /readyz")
	requireContains(t, dep, "path: /healthz")
	requireContains(t, dep, "-metrics-address=$(SHOAL_READ_METRICS_LISTEN)")
	requireContains(t, dep, "-drain-timeout=$(SHOAL_READ_DRAIN_TIMEOUT)")
	requireContains(t, dep, "readOnlyRootFilesystem: true")
	requireContains(t, dep, "terminationGracePeriodSeconds: 45")

	pdb := requireDoc(t, docs, "PodDisruptionBudget", "shoal-read")
	// The PDB's own maxUnavailable is intentionally a separate, more
	// permissive value (1) than the rollout strategy's maxUnavailable
	// (0) above: it governs voluntary disruptions outside a rollout
	// (node drains, cluster-autoscaler consolidation), which aren't
	// gated on a replacement pod being ready first the way a rollout is.
	requireContains(t, pdb, "maxUnavailable: 1")
	requireContains(t, pdb, "app.kubernetes.io/component: read-fleet")
}

func TestReadFleetMonitoringRulesUseExportedMetrics(t *testing.T) {
	alerts := readManifest(t, "monitoring/read-fleet-alerts.yaml")
	for _, metric := range []string{
		"shoal_read_accepting_sessions",
		"shoal_scan_backpressure_total",
		"shoal_scan_failures_total",
		"shoal_scan_sessions_expired_total",
	} {
		if !strings.Contains(alerts, metric) {
			t.Errorf("read-fleet alerts missing metric %q", metric)
		}
	}
}

func TestWriteRoleManifestsHaveSemanticProbesAndSafeRollouts(t *testing.T) {
	tserverDocs := splitYAMLDocuments(readManifest(t, "k8s/tserver.yaml"))
	tserver := requireDoc(t, tserverDocs, "StatefulSet", "shoal-tserver")
	for _, want := range []string{
		"updateStrategy:", "startupProbe:", "path: /startupz", "path: /readyz",
		"-enable-ingest", "readOnlyRootFilesystem: true", "terminationGracePeriodSeconds: 45",
	} {
		requireContains(t, tserver, want)
	}
	requireContains(t, requireDoc(t, tserverDocs, "PodDisruptionBudget", "shoal-tserver"), "maxUnavailable: 1")

	compactorDocs := splitYAMLDocuments(readManifest(t, "k8s/compactor.yaml"))
	compactor := requireDoc(t, compactorDocs, "StatefulSet", "shoal-compactor")
	for _, want := range []string{
		"updateStrategy:", "startupProbe:", "path: /startupz", "path: /readyz",
		"-shutdown-timeout=30s", "volumeClaimTemplates:", "readOnlyRootFilesystem: true",
	} {
		requireContains(t, compactor, want)
	}
	requireContains(t, requireDoc(t, compactorDocs, "PodDisruptionBudget", "shoal-compactor"), "maxUnavailable: 1")
}

func TestWriteTierMonitoringRulesUseExportedMetrics(t *testing.T) {
	alerts := readManifest(t, "monitoring/write-tier-alerts.yaml")
	for _, metric := range []string{
		"shoal_dependency_ready",
		"shoal_tserver_ingest_backpressure_total",
		"shoal_tserver_wal_failures_total",
		"shoal_tserver_minc_failures_total",
		"shoal_compactor_jobs_failed_total",
		"shoal_compactor_completion_ambiguous_total",
		"shoal_compactor_retries_total",
	} {
		if !strings.Contains(alerts, metric) {
			t.Errorf("write-tier alerts missing metric %q", metric)
		}
	}
}

func TestHelmWriteRolesDeclareTLSPDBAndPersistentRestartState(t *testing.T) {
	values := readManifest(t, "helm/shoal/values.yaml")
	for _, section := range []string{"mode: single", "tserver:", "compactor:", "shutdownTimeout:", "walStorageSize:", "stateStorageSize:"} {
		if !strings.Contains(values, section) {
			t.Errorf("values missing %q", section)
		}
	}
	for _, template := range []string{
		"helm/shoal/templates/tserver.yaml",
		"helm/shoal/templates/compactor.yaml",
	} {
		content := readManifest(t, template)
		for _, want := range []string{
			"PodDisruptionBudget", "volumeClaimTemplates:", "startupProbe:",
			"readinessProbe:", "tls.requireClientCert", "scheme: HTTPS", "tcpSocket:",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", template, want)
			}
		}
	}
}

func TestHelmDeploymentModesAndAuthorityValidation(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not found on PATH")
	}

	render := func(t *testing.T, valuesFile string) string {
		t.Helper()
		args := []string{"template", "shoal", "helm/shoal", "-f", valuesFile}
		out, err := exec.Command(helmPath, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	cases := []struct {
		name    string
		values  string
		want    []string
		notWant []string
	}{
		{
			name:    "single",
			values:  "helm/shoal/values-single.yaml",
			want:    []string{"app.kubernetes.io/component: write-tier"},
			notWant: []string{"app.kubernetes.io/component: read-fleet", "app.kubernetes.io/component: tserver", "app.kubernetes.io/component: compactor"},
		},
		{
			name:    "distributed",
			values:  "helm/shoal/values-distributed.yaml",
			want:    []string{"app.kubernetes.io/component: write-tier", "app.kubernetes.io/component: read-fleet"},
			notWant: []string{"app.kubernetes.io/component: tserver", "app.kubernetes.io/component: compactor"},
		},
		{
			name:    "accumulo",
			values:  "helm/shoal/values-accumulo.yaml",
			want:    []string{"app.kubernetes.io/component: read-fleet", "app.kubernetes.io/component: tserver", "app.kubernetes.io/component: compactor"},
			notWant: []string{"app.kubernetes.io/component: write-tier"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := render(t, tc.values)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("render missing %q", want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("render unexpectedly contains %q", notWant)
				}
			}
		})
	}

	invalid := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unknown mode",
			args: []string{"--set", "mode=legacy"},
			want: "mode must be one of: single, distributed, accumulo",
		},
		{
			name: "single dual writer",
			args: []string{"--set", "tserver.enabled=true"},
			want: "Shoal-only writeTier and Accumulo tserver cannot both be enabled",
		},
		{
			name: "accumulo dual writer",
			args: []string{"--set", "mode=accumulo", "--set", "writeTier.enabled=true"},
			want: "Shoal-only writeTier and Accumulo tserver cannot both be enabled",
		},
		{
			name: "distributed multiple writers",
			args: []string{"--set", "mode=distributed", "--set", "writeTier.replicas=2"},
			want: "distributed mode supports exactly one writable writeTier replica",
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "shoal", "helm/shoal"}, tc.args...)
			out, err := exec.Command(helmPath, args...).CombinedOutput()
			if err == nil {
				t.Fatalf("helm %s unexpectedly succeeded", strings.Join(args, " "))
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("helm error missing %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestPlainManifestsCarryNoUnconditionalTLS guards the plain-YAML
// documentation-only TLS example in write-tier.yaml: since plain
// manifests have no templating/conditionals, the only safe way to
// document the opt-in flags there is as commented-out YAML, not live
// config. This fails loudly if that guidance is ever uncommented without
// updating this test (and the accompanying Secret/volume) deliberately.
func TestPlainManifestsCarryNoUnconditionalTLS(t *testing.T) {
	content := readManifest(t, "k8s/write-tier.yaml")
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "--tls-cert=") || strings.Contains(trimmed, "--tls-key=") || strings.Contains(trimmed, "--tls-client-ca=") {
			t.Errorf("found an uncommented TLS flag in write-tier.yaml: %q; the plain manifest's TLS example must stay commented-out documentation (see deploy/README.md's TLS section) since it has no Secret provisioned by default", trimmed)
		}
	}
}

// TestPlainManifestDocumentsTLSProbeSwitch guards write-tier.yaml's
// operator guidance for adapting the readinessProbe/livenessProbe once
// the commented-out TLS example is uncommented: plain YAML has no
// conditionals, so (unlike the Helm chart's automatic switch, see
// TestHelmWriteTierTLSProbeTemplates) an operator enabling TLS here must
// manually add scheme: HTTPS, and switch to tcpSocket for mutual TLS. This
// fails loudly if that guidance comment is ever removed.
func TestPlainManifestDocumentsTLSProbeSwitch(t *testing.T) {
	content := readManifest(t, "k8s/write-tier.yaml")
	for _, want := range []string{
		"scheme: HTTPS",
		"tcpSocket",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("k8s/write-tier.yaml: want probe-guidance comment to mention %q", want)
		}
	}
}

// TestHelmValuesDeclareDisruptionRolloutAndTLSDefaults is
// values.yaml's regression guard: the new keys backing PDBs, explicit
// rollout strategies, and the TLS opt-in toggle must stay present and
// off/safe by default (podDisruptionBudget enabled with a conservative
// budget is a safety default, not a functional opt-in the way TLS is).
func TestHelmValuesDeclareDisruptionRolloutAndTLSDefaults(t *testing.T) {
	values := readManifest(t, "helm/shoal/values.yaml")
	for _, want := range []string{
		"updateStrategy:",
		"partition: 0",
		"podDisruptionBudget:",
		"tls:",
		"enabled: false",
		"strategy:",
		"maxSurge: 1",
		"readinessInterval: 10s",
		"drainTimeout: 30s",
		"terminationGracePeriodSeconds: 45",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("helm/shoal/values.yaml: want to contain %q", want)
		}
	}

	// readFleet is the last top-level section in values.yaml, so scoping
	// to everything from its header onward isolates its own
	// strategy.maxUnavailable (0 — keeps all replicas available
	// throughout a rollout) from its separate, more permissive
	// podDisruptionBudget.maxUnavailable (1 — voluntary disruptions
	// outside a rollout). A plain whole-file substring check can't tell
	// those two apart, so checking them unscoped could keep passing even
	// if the strategy value regressed back to the weaker "1" as long as
	// the PDB's "1" was still present.
	idx := strings.Index(values, "\nreadFleet:")
	if idx == -1 {
		t.Fatal("helm/shoal/values.yaml: no top-level readFleet: section found")
	}
	readFleetSection := values[idx:]
	if !strings.Contains(readFleetSection, "maxUnavailable: 0") {
		t.Error("helm/shoal/values.yaml: readFleet.strategy.maxUnavailable must be 0 to keep all replicas available throughout a rollout (see deploy/README.md's rollout section)")
	}
	if !strings.Contains(readFleetSection, "maxUnavailable: 1") {
		t.Error("helm/shoal/values.yaml: readFleet.podDisruptionBudget.maxUnavailable must stay 1 (voluntary-disruption budget, deliberately separate from the rollout strategy above)")
	}
}

func TestHelmReadFleetTLSProbeTemplates(t *testing.T) {
	template := readManifest(t, "helm/shoal/templates/read-deployment.yaml")
	for _, want := range []string{
		"readFleet.tls.enabled",
		"readFleet.tls.requireClientCert",
		"scheme: HTTPS",
		"path: /readyz",
		"tcpSocket:",
		"readOnlyRootFilesystem: true",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("helm read deployment: want to contain %q", want)
		}
	}
}

// TestHelmWriteTierTLSProbeTemplates guards the chart-specific probe wiring
// needed once the write tier's health listener is TLS-wrapped. Plain TLS
// needs kubelet HTTP probes to use HTTPS; mutual TLS cannot use kubelet
// httpGet probes because kubelet does not present a client certificate, so
// the chart intentionally falls back to TCP reachability probes in that mode.
func TestHelmWriteTierTLSProbeTemplates(t *testing.T) {
	template := readManifest(t, "helm/shoal/templates/embed-statefulset.yaml")
	for _, want := range []string{
		"scheme: HTTPS",
		"if and .Values.writeTier.tls.enabled .Values.writeTier.tls.requireClientCert",
		"tcpSocket:",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("helm/shoal/templates/embed-statefulset.yaml: want to contain %q", want)
		}
	}
}

// TestHelmChartLintsAndRenders runs `helm lint` and `helm template`
// (default values, then TLS + mutual-TLS enabled) against the chart when
// a `helm` binary is available, and fails on any non-zero exit or on
// template output that doesn't parse as well-formed multi-document YAML
// (checked the same dependency-free way as the plain manifests above:
// every document must have a recognizable "kind:" line). It skips
// entirely when helm isn't on PATH — true in this repository's CI today —
// so this test only ever strengthens local/future-CI coverage, never
// blocks it.
func TestHelmChartLintsAndRenders(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not found on PATH; skipping (CI currently has no helm installed either — see .github/workflows/ci.yml)")
	}

	if out, err := exec.Command(helmPath, "lint", "helm/shoal").CombinedOutput(); err != nil {
		t.Fatalf("helm lint helm/shoal: %v\n%s", err, out)
	}

	cases := []struct {
		name            string
		args            []string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:            "defaults",
			wantNotContains: []string{"scheme: HTTPS", "tcpSocket:"},
		},
		{
			// The only mode that renders the HTTPS httpGet branch: TLS
			// enabled, but requireClientCert left false (its chart
			// default). Distinct from "tls and mTLS enabled" below,
			// which renders the tcpSocket branch instead — a
			// string-presence check against the raw template source
			// (see TestHelmWriteTierTLSProbeTemplates) can't tell these
			// two rendered shapes apart or catch malformed YAML
			// specific to this branch, so this case renders it for
			// real.
			name: "tls enabled, no mutual TLS",
			args: []string{
				"--set", "writeTier.tls.enabled=true",
				"--set", "writeTier.tls.secretName=shoal-embed-tls",
			},
			wantContains:    []string{"scheme: HTTPS", "httpGet:"},
			wantNotContains: []string{"tcpSocket:"},
		},
		{name: "tls and mTLS enabled", args: []string{
			"--set", "writeTier.tls.enabled=true",
			"--set", "writeTier.tls.secretName=shoal-embed-tls",
			"--set", "writeTier.tls.requireClientCert=true",
		},
			wantContains:    []string{"tcpSocket:"},
			wantNotContains: []string{"scheme: HTTPS"},
		},
		{name: "read fleet TLS enabled", args: []string{
			"--set", "mode=distributed",
			"--set", "readFleet.tls.enabled=true",
			"--set", "readFleet.tls.secretName=shoal-read-tls",
		}},
		{name: "read fleet mTLS enabled", args: []string{
			"--set", "mode=distributed",
			"--set", "readFleet.tls.enabled=true",
			"--set", "readFleet.tls.secretName=shoal-read-tls",
			"--set", "readFleet.tls.requireClientCert=true",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "shoal", "helm/shoal"}, tc.args...)
			out, err := exec.Command(helmPath, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
			}
			docs := splitYAMLDocuments(string(out))
			if len(docs) == 0 {
				t.Fatal("helm template produced no documents")
			}
			for _, d := range docs {
				if d.kind == "" {
					t.Errorf("a rendered document has no recognizable \"kind:\" line:\n%s", d.text)
				}
			}

			// Scope the probe-shape assertions to the write-tier
			// StatefulSet specifically: the read-fleet Deployment always
			// renders its own unconditional tcpSocket probes regardless
			// of writeTier.tls settings, so an unscoped whole-output
			// check would false-fail the "no tcpSocket" cases above.
			if len(tc.wantContains) > 0 || len(tc.wantNotContains) > 0 {
				sts := requireDoc(t, docs, "StatefulSet", "shoal-embed")
				for _, want := range tc.wantContains {
					requireContains(t, sts, want)
				}
				for _, notWant := range tc.wantNotContains {
					if strings.Contains(sts.text, notWant) {
						t.Errorf("shoal-embed StatefulSet unexpectedly contains %q:\n%s", notWant, sts.text)
					}
				}
			}
		})
	}
}
