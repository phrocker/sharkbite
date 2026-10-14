// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package health

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
)

// Tracker implements the gRPC Health Checking Protocol (grpc.health.v1.Health).
// It uses the segment manager to determine whether the sidecar is healthy.
type Tracker struct {
	mu      sync.RWMutex
	serving bool
	mgr     *segment.Manager
}

// NewTracker creates a Tracker that reports SERVING immediately.
func NewTracker(mgr *segment.Manager) *Tracker {
	return &Tracker{
		serving: true,
		mgr:     mgr,
	}
}

// SetServing sets the health status. Call with false during shutdown.
func (t *Tracker) SetServing(serving bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.serving = serving
}

// IsServing returns the current health status.
func (t *Tracker) IsServing() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.serving
}

// Check implements grpc_health_v1.HealthServer.
func (t *Tracker) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	st := grpc_health_v1.HealthCheckResponse_NOT_SERVING
	if t.serving {
		st = grpc_health_v1.HealthCheckResponse_SERVING
	}
	return &grpc_health_v1.HealthCheckResponse{Status: st}, nil
}

// Watch implements grpc_health_v1.HealthServer. For simplicity, this
// implementation sends the current status once and returns. A full
// implementation would stream status changes.
func (t *Tracker) Watch(req *grpc_health_v1.HealthCheckRequest, stream grpc_health_v1.Health_WatchServer) error {
	t.mu.RLock()
	st := grpc_health_v1.HealthCheckResponse_NOT_SERVING
	if t.serving {
		st = grpc_health_v1.HealthCheckResponse_SERVING
	}
	t.mu.RUnlock()

	return stream.Send(&grpc_health_v1.HealthCheckResponse{Status: st})
}

// List implements grpc_health_v1.HealthServer. It reports the current status
// for the default (empty) service name.
func (t *Tracker) List(ctx context.Context, req *grpc_health_v1.HealthListRequest) (*grpc_health_v1.HealthListResponse, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	st := grpc_health_v1.HealthCheckResponse_NOT_SERVING
	if t.serving {
		st = grpc_health_v1.HealthCheckResponse_SERVING
	}
	return &grpc_health_v1.HealthListResponse{
		Statuses: map[string]*grpc_health_v1.HealthCheckResponse{
			"": {Status: st},
		},
	}, nil
}

// RegisterServer registers the health service on the given gRPC server.
func RegisterServer(srv *grpc.Server, tracker *Tracker) {
	grpc_health_v1.RegisterHealthServer(srv, tracker)
}
