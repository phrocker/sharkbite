// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

package tserverprocess

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/internal/tserver"
)

type GenerationFactory func() (*tserver.ServiceLock, tserver.ServiceLockData, error)

type Supervisor struct {
	Host          *tserver.Host
	NewGeneration GenerationFactory
	Release       func([]tserver.Extent)
	RetryBackoff  time.Duration
	OnError       func(error)
	Participate   func(context.Context, *tserver.ServiceLock, *tserver.Host, tserver.ServiceLockData, func([]tserver.Extent)) error
}

// Run reacquires a fresh ServiceLock after a lost ZooKeeper generation. It
// never reuses a ServiceLock UUID, and a non-monotonic recreated lock
// directory fails closed through Host.AdoptLock.
func (s Supervisor) Run(ctx context.Context) error {
	if s.Host == nil || s.NewGeneration == nil {
		return errors.New("tserverprocess: incomplete supervisor")
	}
	participate := s.Participate
	if participate == nil {
		participate = tserver.Participate
	}
	backoff := s.RetryBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for {
		lock, data, err := s.NewGeneration()
		if err == nil {
			err = participate(ctx, lock, s.Host, data, s.Release)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, tserver.ErrLockLost) {
			return fmt.Errorf("tserverprocess: participation: %w", err)
		}
		if s.OnError != nil && err != nil {
			s.OnError(err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
