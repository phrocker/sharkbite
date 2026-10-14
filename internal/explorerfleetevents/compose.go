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

package explorerfleetevents

import (
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
)

// ConfigureRuntime adds the fleet-event physical table before
// explorercoord.Open. Repeated calls are safe.
func ConfigureRuntime(config *explorercoord.Config) {
	if config == nil {
		return
	}
	for _, table := range config.PhysicalTables {
		if table == Table {
			return
		}
	}
	config.PhysicalTables = append(append([]string(nil), config.PhysicalTables...), Table)
}

// Compose constructs the production fleet-event service around the shared
// transaction runtime, authorization generation source, durable interaction
// recorder, and durable registry lease validator.
func Compose(
	runtime *explorercoord.Runtime,
	domain coordination.DomainID,
	resolver auth.Resolver,
	generations auth.GenerationReader,
	interactionRecorder *interaction.Recorder,
	snapshots fleet.InteractionSnapshotProvider,
	leases fleetevents.LeaseValidator,
	cursorKey []byte,
	clock func() time.Time,
) (*fleetevents.Service, error) {
	service, _, err := ComposeWithPublisher(
		runtime, domain, resolver, generations, interactionRecorder,
		snapshots, leases, cursorKey, clock,
	)
	return service, err
}

// ComposeWithPublisher constructs the public subscription service and the
// trusted dispatch-transition publisher over the same durable adapter.
func ComposeWithPublisher(
	runtime *explorercoord.Runtime,
	domain coordination.DomainID,
	resolver auth.Resolver,
	generations auth.GenerationReader,
	interactionRecorder *interaction.Recorder,
	snapshots fleet.InteractionSnapshotProvider,
	leases fleetevents.LeaseValidator,
	cursorKey []byte,
	clock func() time.Time,
) (*fleetevents.Service, *ActionEventPublisher, error) {
	backend, err := New(runtime, domain)
	if err != nil {
		return nil, nil, err
	}
	auditor, err := fleetevents.NewInteractionAuditor(
		interactionRecorder, snapshots)
	if err != nil {
		return nil, nil, err
	}
	return composeWithPublisher(
		backend, resolver, generations, auditor, leases, cursorKey, clock, nil, nil)
}

// ComposeWithPublisherAndReader constructs the production event service with
// authoritative interaction-receipt reconciliation for exact retries.
// evidenceVisibility is the reader label evaluator applied to every
// delivered envelope's evidence (#562, #564); nil withholds labelled evidence
// from every subscriber.
func ComposeWithPublisherAndReader(
	runtime *explorercoord.Runtime,
	domain coordination.DomainID,
	resolver auth.Resolver,
	generations auth.GenerationReader,
	interactionSinkFor func(auth.Operation) interaction.ResultSink,
	interactionReader fleetevents.InteractionReceiptReader,
	snapshots fleet.InteractionSnapshotProvider,
	leases fleetevents.LeaseValidator,
	cursorKey []byte,
	clock func() time.Time,
	evidenceVisibility evidencelabels.Visibility,
	evidenceNodes evidencelabels.NodeGate,
	options ...ComposeOption,
) (*fleetevents.Service, *ActionEventPublisher, error) {
	backend, err := New(runtime, domain)
	if err != nil {
		return nil, nil, err
	}
	auditor, err := fleetevents.NewOperationInteractionAuditorWithReader(
		interactionSinkFor, interactionReader, snapshots)
	if err != nil {
		return nil, nil, err
	}
	return composeWithPublisher(
		backend, resolver, generations, auditor, leases, cursorKey, clock,
		evidenceVisibility, evidenceNodes, options...)
}

// ComposeOption adds optional wiring to the composed event service.
type ComposeOption func(*composeOptions) error

type composeOptions struct {
	approvals          fleetevents.ApprovalLedger
	approvalVisibility fleetevents.ApprovalVisibility
}

// WithApprovalEvents admits approval lifecycle events (#653). transitions is
// the durable approval outbox the ledger confirms every approval publication
// against. visibility is the approval concealment rule asked at delivery. A
// host binds it to fleet.ApprovalService.VisibleTo, typically late, since the
// approval service is composed after this one. Without this option, approval
// events are neither admitted nor delivered.
func WithApprovalEvents(
	transitions fleet.ApprovalTransitionStore,
	visibility fleetevents.ApprovalVisibility,
) ComposeOption {
	return func(options *composeOptions) error {
		ledger, err := NewApprovalLedger(transitions)
		if err != nil {
			return err
		}
		if visibility == nil {
			return errors.New("fleet events: approval visibility is required")
		}
		options.approvals, options.approvalVisibility = ledger, visibility
		return nil
	}
}

func composeWithPublisher(
	backend fleetevents.Backend,
	resolver auth.Resolver,
	generations auth.GenerationReader,
	auditor fleetevents.Auditor,
	leases fleetevents.LeaseValidator,
	cursorKey []byte,
	clock func() time.Time,
	evidenceVisibility evidencelabels.Visibility,
	evidenceNodes evidencelabels.NodeGate,
	options ...ComposeOption,
) (*fleetevents.Service, *ActionEventPublisher, error) {
	var optional composeOptions
	for _, option := range options {
		if err := option(&optional); err != nil {
			return nil, nil, err
		}
	}
	capability := explorerfleetcap.New()
	service, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: backend, Resolver: resolver, GenerationReader: generations,
		LeaseValidator: leases, Auditor: auditor, CursorKey: cursorKey,
		Clock: clock, EvidenceVisibility: evidenceVisibility,
		EvidenceNodes: evidenceNodes, Approvals: optional.approvals,
		ApprovalVisibility: optional.approvalVisibility,
	}, capability)
	if err != nil {
		return nil, nil, err
	}
	publisher, err := NewActionEventPublisher(
		service, resolver, capability, clock)
	if err != nil {
		return nil, nil, err
	}
	return service, publisher, nil
}
