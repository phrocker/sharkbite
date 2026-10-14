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
	"io"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

type connectorResult struct {
	client ReportClient
	err    error
}

type sequenceConnector struct {
	mu      sync.Mutex
	results []connectorResult
	calls   int
}

func (c *sequenceConnector) Connect(context.Context) (ReportClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if len(c.results) == 0 {
		return nil, errors.New("no manager")
	}
	result := c.results[0]
	c.results = c.results[1:]
	return result.client, result.err
}

type fakeReportClient struct {
	err      error
	reported chan string
	closed   bool
}

func (c *fakeReportClient) ReportTabletStatus(
	_ context.Context,
	server string,
	_ manager.TabletLoadState,
	_ tserver.Extent,
) error {
	if c.reported != nil {
		c.reported <- server
	}
	return c.err
}

func (c *fakeReportClient) Close() error {
	c.closed = true
	return nil
}

func TestRetryingReporterReconnectsAcrossManagerFailover(t *testing.T) {
	dead := &fakeReportClient{err: io.EOF}
	live := &fakeReportClient{reported: make(chan string, 1)}
	connector := &sequenceConnector{results: []connectorResult{
		{client: dead},
		{err: errors.New("manager moving")},
		{client: live},
	}}
	reporter := &RetryingReporter{
		Connector:  connector,
		Server:     "shoal:9997",
		MinBackoff: time.Millisecond,
		MaxBackoff: time.Millisecond,
	}
	if err := reporter.Report(
		context.Background(),
		manager.TabletLoadState_LOADED,
		tserver.Extent{TableID: "2", EndRow: []byte("m")},
	); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !dead.closed || !live.closed {
		t.Fatalf("clients closed = dead:%t live:%t", dead.closed, live.closed)
	}
	if connector.calls != 3 {
		t.Fatalf("connect calls = %d, want 3", connector.calls)
	}
	select {
	case server := <-live.reported:
		if server != "shoal:9997" {
			t.Fatalf("reported server = %q", server)
		}
	default:
		t.Fatal("replacement manager received no report")
	}
}

func TestRetryingReporterStopsOnCancellation(t *testing.T) {
	connector := &sequenceConnector{}
	reporter := &RetryingReporter{
		Connector:  connector,
		Server:     "shoal:9997",
		MinBackoff: time.Millisecond,
		MaxBackoff: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reporter.Report(
		ctx,
		manager.TabletLoadState_LOAD_FAILURE,
		tserver.Extent{TableID: "2"},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("Report error = %v, want context.Canceled", err)
	}
}
