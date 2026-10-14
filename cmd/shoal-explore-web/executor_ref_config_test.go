// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/executorattest"
	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/executorref/executorreftest"
)

// TestExecutorRefConfigParity: the host's configured executor references
// (-fleet-executor-refs, the two external-effect lists, -fleet-ask-executor-ref
// and the -fleet-executor-attestation trust file) are held to the same rule
// as registration, attestation and the executor binding (#391), on the whole
// probe table pkg/atpl's TestExecutorRefParity runs. A refusal names the flag
// and the entry's position and never echoes the value.
func TestExecutorRefConfigParity(t *testing.T) {
	public := ed25519.NewKeyFromSeed(
		[]byte(strings.Repeat("k", ed25519.SeedSize))).Public().(ed25519.PublicKey)
	sites := map[string]func(ref string) error{
		"-fleet-executor-refs": func(ref string) error {
			_, err := newConfiguredFleetExecutors([]string{"plain", ref})
			return err
		},
		"-fleet-external-executor-refs": func(ref string) error {
			// The allowlist is built without validation so the external
			// flag's own check is what is exercised.
			executors := configuredFleetExecutors{
				"plain": configuredFleetExecutor{reference: "plain"},
				ref:     configuredFleetExecutor{reference: ref},
			}
			return bindExternalFleetEffects(executors, externalFleetEffectBindings{
				mutating: []string{"plain", ref},
			})
		},
		"-fleet-external-egress-executor-refs": func(ref string) error {
			executors := configuredFleetExecutors{
				"plain": configuredFleetExecutor{reference: "plain"},
				ref:     configuredFleetExecutor{reference: ref},
			}
			return bindExternalFleetEffects(executors, externalFleetEffectBindings{
				transmitting: []string{"plain", ref},
			})
		},
		"-fleet-ask-executor-ref": func(ref string) error {
			if ref == "" {
				// An unset ask flag is valid and binds nothing; the empty
				// row is covered by every other site.
				return executorref.ValidExecutorRef(ref)
			}
			return bindExternalFleetEffects(
				configuredFleetExecutors{}, externalFleetEffectBindings{askReference: ref})
		},
		"-fleet-executor-attestation": func(ref string) error {
			_, err := executorattest.NewTrust(map[string]executorattest.ExecutorTrust{
				ref: {
					Verifiers: []executorattest.VerifierTrust{{
						ID: "verifier", PublicKey: public,
						MaxValidity: time.Hour, ClockSkew: time.Minute,
					}},
					ImageDigests: []string{"sha256:" + strings.Repeat("c", 64)},
				},
			})
			return err
		},
	}
	for _, probe := range executorreftest.Probes() {
		for site, check := range sites {
			err := check(probe.Ref)
			if probe.Valid && err != nil {
				t.Errorf("%s refuses %s: %v", site, probe.Name, err)
			}
			if !probe.Valid {
				if err == nil {
					t.Errorf("%s accepts %s (%q)", site, probe.Name, probe.Ref)
					continue
				}
				if probe.Ref != "" && strings.Contains(err.Error(), probe.Ref) {
					t.Errorf("%s echoes the refused value of %s: %v",
						site, probe.Name, err)
				}
			}
		}
	}

	// The refusal names the flag and the 1-based position of the entry.
	_, err := newConfiguredFleetExecutors([]string{"plain", "wоrker"})
	if err == nil || !strings.Contains(err.Error(), "-fleet-executor-refs entry 2:") {
		t.Fatalf("allowlist refusal = %v", err)
	}
	err = bindExternalFleetEffects(configuredFleetExecutors{},
		externalFleetEffectBindings{transmitting: []string{"worker a"}})
	if err == nil || !strings.Contains(
		err.Error(), "-fleet-external-egress-executor-refs entry 1:") {
		t.Fatalf("external refusal = %v", err)
	}
	err = bindExternalFleetEffects(configuredFleetExecutors{},
		externalFleetEffectBindings{askReference: "-ask"})
	if err == nil || !strings.Contains(err.Error(), "-fleet-ask-executor-ref:") {
		t.Fatalf("ask refusal = %v", err)
	}
}
