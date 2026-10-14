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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// HostConfig contains the host-owned dependencies needed to compose the
// durable registry and event service around one shared runtime.
type HostConfig struct {
	Runtime     *explorercoord.Runtime
	Domain      coordination.DomainID
	Resolver    auth.Resolver
	Generations auth.GenerationReader
	// Interaction must be the authorization-enforcing result sink for the
	// request context, not the underlying corpus sink.
	Interaction interaction.ResultSink
	// EventInteraction returns an authorization-enforcing sink bound to the
	// supplied fleet operation. It must not derive trust from session content.
	EventInteraction func(auth.Operation) interaction.ResultSink
	// InteractionStorage is the underlying durable sink. It is checked during
	// startup without requiring a request-bound authorization decision.
	InteractionStorage interaction.Sink
	// InteractionReader is the same-corpus authoritative reader used only to
	// reconcile durable receipts; it must not require request read permission.
	InteractionReader fleetevents.InteractionReceiptReader
	Snapshots         fleet.InteractionSnapshotProvider
	Executors         fleet.ExecutorRegistry
	CursorKeys        CursorKeyStore
	Visibility        []byte
	Clock             func() time.Time
	// EvidenceVisibility is the reader label evaluator applied at delivery
	// (authorized.LabelVisibility). Nil withholds labelled evidence from
	// every subscriber.
	EvidenceVisibility evidencelabels.Visibility
	// EvidenceNodes is the current-rule node gate (authorized.NodeGate). Nil
	// withholds every reference that names a node.
	EvidenceNodes evidencelabels.NodeGate
}

// HostedServices is the compact production construction and mounting seam.
type HostedServices struct {
	registry     *fleet.Service
	events       *fleetevents.Service
	actionEvents *ActionEventPublisher
}

// ConfigureHostedRuntime registers every physical table required by the
// registry and event log before explorercoord.OpenExplorer.
func ConfigureHostedRuntime(config *explorercoord.Config) {
	if config == nil {
		return
	}
	*config = explorerfleet.ConfigureRuntime(*config)
	ConfigureRuntime(config)
}

// ComposeHosted constructs the registry and event service with one durable
// interaction sink and a domain-separated event cursor key.
func ComposeHosted(ctx context.Context, config HostConfig) (*HostedServices, error) {
	if ctx == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	if config.InteractionStorage == nil || config.Interaction == nil ||
		config.EventInteraction == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction sinks are required")
	}
	if err := config.InteractionStorage.EnsureInteractionSink(ctx); err != nil {
		return nil, err
	}
	if config.InteractionReader == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"authoritative interaction receipt reader is required")
	}
	lifecycleRecorder, err := explorerfleet.NewLifecycleRecorderWithReader(
		config.Interaction, config.InteractionReader)
	if err != nil {
		return nil, err
	}
	cursorKey, err := LoadOrCreateCursorKey(ctx, config.CursorKeys)
	if err != nil {
		return nil, err
	}
	registry, err := explorerfleet.Compose(
		config.Runtime, config.Resolver, lifecycleRecorder,
		config.Snapshots, config.Executors,
		config.Visibility, config.Clock)
	if err != nil {
		return nil, err
	}
	events, actionEvents, err := ComposeWithPublisherAndReader(
		config.Runtime, config.Domain, config.Resolver, config.Generations,
		config.EventInteraction, config.InteractionReader, config.Snapshots,
		registry, cursorKey, config.Clock, config.EvidenceVisibility,
		config.EvidenceNodes)
	if err != nil {
		return nil, err
	}
	return &HostedServices{
		registry: registry, events: events, actionEvents: actionEvents,
	}, nil
}

// DispatchDependencies returns the durable registry and event publisher
// required by explorerfleet.ComposeDispatch.
func (s *HostedServices) DispatchDependencies() (
	*fleet.Service, fleet.ActionEventPublisher, error,
) {
	if s == nil || s.registry == nil || s.actionEvents == nil {
		return nil, nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "hosted fleet services are required")
	}
	return s.registry, s.actionEvents, nil
}

// Mount adds only the more-specific event subtree to the authenticated
// Handler. Hosted startup must mount the combined registry/dispatch handler
// separately with webapi.NewFleetHandler at webapi.FleetRoutePrefix.
func (s *HostedServices) Mount(handler *webapi.Handler) error {
	if s == nil || handler == nil || s.events == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "hosted fleet services are required")
	}
	return handler.MountFleetEvents(s.events)
}

// CursorKeyStore provides a durable, load-or-create corpus key. Implementations
// must return the same 32-byte key after restart.
type CursorKeyStore interface {
	ChangeCursorSealKey(context.Context) ([]byte, error)
}

// LoadOrCreateCursorKey derives the fleet-event cursor key from durable corpus
// state. It rejects ambient, truncated, or oversized key material.
func LoadOrCreateCursorKey(
	ctx context.Context, store CursorKeyStore,
) ([]byte, error) {
	if ctx == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	if store == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet event cursor key store is required")
	}
	root, err := store.ChangeCursorSealKey(ctx)
	if err != nil {
		return nil, err
	}
	if len(root) != sha256.Size {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet event cursor root key must contain exactly 32 bytes")
	}
	mac := hmac.New(sha256.New, root)
	_, _ = mac.Write([]byte("shoal-explore-web/fleet-event-cursor/v1"))
	return mac.Sum(nil), nil
}
