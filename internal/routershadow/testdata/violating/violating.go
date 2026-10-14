// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package violating is a deliberately violating fixture for the authority
// check in authority_test.go: every function here reaches an operation that
// runs, creates or mutates work a different way, and the check must flag each
// one. It is under testdata, so it is never built.
package violating

import (
	"context"
	"os/exec"
	"reflect"
	_ "unsafe"

	"example.test/elsewhere"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

type enqueuer interface {
	Enqueue(context.Context, fleet.EnqueueRequest) (fleet.ActionRecord, error)
}

// enqueueVar stores Enqueue in a package-level func variable.
var enqueueVar = (*fleet.DispatchService).Enqueue

// Direct calls Enqueue on the dispatch service.
func Direct(ctx context.Context, s *fleet.DispatchService) {
	_, _ = s.Enqueue(ctx, fleet.EnqueueRequest{})
}

// MethodValue takes Enqueue and Invoke as method values.
func MethodValue(s *fleet.DispatchService) (any, any) {
	f := s.Invoke
	g := s.Enqueue
	return f, g
}

// ViaInterface reaches Enqueue through a locally declared interface.
func ViaInterface(ctx context.Context, e enqueuer) {
	_, _ = e.Enqueue(ctx, fleet.EnqueueRequest{})
}

// ViaVar calls the stored func variable.
func ViaVar(ctx context.Context, s *fleet.DispatchService) {
	_, _ = enqueueVar(s, ctx, fleet.EnqueueRequest{})
}

// ViaElsewhere calls a func variable bound to Enqueue in another package.
func ViaElsewhere(ctx context.Context, s *fleet.DispatchService) {
	_, _ = elsewhere.Enqueue(s, ctx, fleet.EnqueueRequest{})
}

// Claims claims, runs and cancels work.
func Claims(ctx context.Context, s *fleet.DispatchService, r fleet.ActionRecord) {
	_, _ = s.Claim(ctx, fleet.ClaimRequest{})
	_, _ = s.ExecuteClaim(ctx, r)
	_, _ = s.Cancel(ctx, fleet.CancelRequest{})
}

// Registry registers, heartbeats and revokes descriptors.
func Registry(ctx context.Context, s *fleet.Service) {
	_, _ = s.Register(ctx, fleet.RegisterRequest{})
	_, _ = s.Heartbeat(ctx, fleet.HeartbeatRequest{})
	_, _ = s.Revoke(ctx, fleet.RevokeRequest{})
}

// Reflective looks the method up by name.
func Reflective(s *fleet.DispatchService) {
	_ = reflect.ValueOf(s).MethodByName("Enqueue")
	_ = exec.Command("true")
}

//go:linkname linkedEnqueue github.com/phrocker/shoal-oss/pkg/explorer/fleet.(*DispatchService).Enqueue
func linkedEnqueue()
