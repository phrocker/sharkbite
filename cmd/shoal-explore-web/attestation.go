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

package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/executorattest"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// loadExecutorAttestationTrust reads the -fleet-executor-attestation trust
// file. Every attested ref must already be bound for external work by
// -fleet-external-executor-refs or -fleet-external-egress-executor-refs:
// attestation is only accepted on actions declaring the external effect, and
// a trust root for a ref that can never serve one is a values file that says
// something it cannot mean.
func loadExecutorAttestationTrust(
	path string, external externalFleetEffectBindings,
) (*executorattest.Trust, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-fleet-executor-attestation: %w", err)
	}
	trust, err := executorattest.ParseTrust(raw)
	if err != nil {
		return nil, fmt.Errorf("-fleet-executor-attestation: %w", err)
	}
	for _, ref := range trust.Refs() {
		if !slices.Contains(external.mutating, ref) &&
			!slices.Contains(external.transmitting, ref) {
			return nil, fmt.Errorf(
				"-fleet-executor-attestation configures executor reference %q, "+
					"which is not bound by -fleet-external-executor-refs or "+
					"-fleet-external-egress-executor-refs; attestation applies "+
					"only to external work", ref)
		}
	}
	return trust, nil
}

// executorAttestationAdapter adapts internal/executorattest to the
// interfaces the fleet defines: AttestationTrust for Register,
// ExecutorAttestations for every claim grant, and AttestationPresenter for
// the presentation route. The fleet never sees the trust file, a verifier
// key or a digest.
type executorAttestationAdapter struct {
	store *executorattest.Store
	trust func() *executorattest.Trust
}

var (
	_ fleet.AttestationTrust     = executorAttestationAdapter{}
	_ fleet.ExecutorAttestations = executorAttestationAdapter{}
	_ fleet.AttestationPresenter = executorAttestationAdapter{}
)

func attestationPrincipal(p fleet.AttestationPrincipal) executorattest.Principal {
	return executorattest.Principal{
		Domain: append([]byte(nil), p.Domain...), Subject: p.Subject, ClientID: p.ClientID,
	}
}

func (a executorAttestationAdapter) Configured(ref string) bool {
	return a.trust().Configured(ref)
}

// Current asks the store for the principal's attestation as it stands at
// now. needUntil is now: the lease-end comparison is the fleet gate's, made
// once, in applyClaim, so it is not also made here under a second definition.
func (a executorAttestationAdapter) Current(
	ctx context.Context, principal fleet.AttestationPrincipal, ref string, now time.Time,
) (fleet.ExecutorAttestation, error) {
	id, expires, ok, err := a.store.Current(ctx, attestationPrincipal(principal), ref, now, now)
	if err != nil {
		return fleet.ExecutorAttestation{}, err
	}
	return fleet.ExecutorAttestation{ID: id, ExpiresAt: expires, OK: ok}, nil
}

func (a executorAttestationAdapter) expectation(
	principal fleet.AttestationPrincipal, presentation fleet.AttestationPresentation, now time.Time,
) executorattest.Expectation {
	return executorattest.Expectation{
		Principal: attestationPrincipal(principal), ExecutorRef: presentation.ExecutorRef,
		Key: append([]byte(nil), presentation.IdempotencyKey...), Now: now,
	}
}

func (a executorAttestationAdapter) Verify(
	principal fleet.AttestationPrincipal, presentation fleet.AttestationPresentation, now time.Time,
) (fleet.AttestationReceipt, error) {
	result, err := executorattest.Verify(a.trust(),
		a.expectation(principal, presentation, now), presentation.Report)
	if err != nil {
		return fleet.AttestationReceipt{}, attestationRefusal(err)
	}
	return fleet.AttestationReceipt{AttestationID: result.AttestationID, ExpiresAt: result.ExpiresAt}, nil
}

func (a executorAttestationAdapter) Record(
	ctx context.Context, principal fleet.AttestationPrincipal,
	presentation fleet.AttestationPresentation, now time.Time,
) (fleet.AttestationReceipt, error) {
	record, err := a.store.Present(ctx, a.expectation(principal, presentation, now), presentation.Report)
	if err != nil {
		if _, refused := executorattest.ReasonOf(err); refused {
			return fleet.AttestationReceipt{}, attestationRefusal(err)
		}
		return fleet.AttestationReceipt{}, err
	}
	return fleet.AttestationReceipt{AttestationID: record.AttestationID, ExpiresAt: record.ExpiresAt}, nil
}

// attestationRefusal turns a verifier refusal into the fleet's, carrying the
// typed reason for audit. A malformed expectation is a refusal too.
func attestationRefusal(err error) error {
	reason, ok := executorattest.ReasonOf(err)
	if !ok {
		reason = executorattest.ReasonMalformed
	}
	return &fleet.AttestationRefusal{Reason: string(reason)}
}

// openExecutorAttestations opens the attestation store on the workspace's
// embedded engine, in its own table, and adapts it.
func openExecutorAttestations(
	eng *engine.Engine, trust *executorattest.Trust,
) (executorAttestationAdapter, error) {
	if !slices.Contains(eng.TableNames(), executorattest.Table) {
		if err := eng.CreateTable(executorattest.Table, engine.TableOptions{}); err != nil {
			return executorAttestationAdapter{}, fmt.Errorf("create executor attestation table: %w", err)
		}
	}
	backend, err := explorercoord.NewEngineStore(eng, executorattest.Table)
	if err != nil {
		return executorAttestationAdapter{}, err
	}
	load := func() *executorattest.Trust { return trust }
	store, err := executorattest.NewStore(executorattest.StoreConfig{Backend: backend, Trust: load})
	if err != nil {
		return executorAttestationAdapter{}, err
	}
	return executorAttestationAdapter{store: store, trust: load}, nil
}

// optionalAttestation keeps a nil service a nil interface, so the handler is
// mounted only when a trust file was configured.
func optionalAttestation(service *fleet.AttestationService) webapi.FleetAttestationProvider {
	if service == nil {
		return nil
	}
	return service
}
