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

package tserverrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

// ReportClient is one connection to the currently authoritative manager.
type ReportClient interface {
	ReportTabletStatus(context.Context, string, manager.TabletLoadState, tserver.Extent) error
	io.Closer
}

// ReportConnector resolves and connects on every attempt. That makes a retry
// observe manager failover instead of retrying a dead cached endpoint.
type ReportConnector interface {
	Connect(context.Context) (ReportClient, error)
}

type RetryingReporter struct {
	Connector  ReportConnector
	Server     string
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func (r *RetryingReporter) Report(
	ctx context.Context,
	state manager.TabletLoadState,
	extent tserver.Extent,
) error {
	if r == nil || r.Connector == nil {
		return fmt.Errorf("%w: manager report connector is unavailable", ErrUnsupported)
	}
	if r.Server == "" {
		return fmt.Errorf("%w: empty tablet server name", ErrInvalidRequest)
	}
	min := r.MinBackoff
	if min <= 0 {
		min = 10 * time.Millisecond
	}
	max := r.MaxBackoff
	if max <= 0 {
		max = time.Second
	}
	if max < min {
		max = min
	}
	backoff := min
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return errors.Join(err, last)
			}
			return err
		}
		client, err := r.Connector.Connect(ctx)
		if err == nil {
			if client == nil {
				err = errors.New("tserverrpc: manager connector returned a nil client")
			} else {
				err = client.ReportTabletStatus(ctx, r.Server, state, extent)
				closeErr := client.Close()
				if err == nil && closeErr == nil {
					return nil
				}
				err = errors.Join(err, closeErr)
			}
		}
		last = err

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), last)
		case <-timer.C:
		}
		if backoff < max {
			backoff *= 2
			if backoff > max {
				backoff = max
			}
		}
	}
}

var _ StatusReporter = (*RetryingReporter)(nil)
