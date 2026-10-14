//go:build !embed

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

package cclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/protocol"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/compactioncoordinator"
)

// coordinatorServiceName is the multiplex name the manager registers
// CompactionCoordinatorService under. From core/.../rpc/clients/
// ThriftClientTypes.java: COORDINATOR = ...ThriftClient("coordinator").
// The coordinator runs inside the manager process; the manager's
// TMultiplexedProcessor demuxes on this name.
const coordinatorServiceName = "coordinator"

// Default I/O bounds for a coordinator connection. Every socket
// operation must be bounded: thrift's TSocket only installs a read/write
// deadline when TConfiguration.SocketTimeout is positive, so an
// unbounded client blocks forever against a manager that accepts the
// connection and then goes silent (host blackholed, kernel alive but
// process wedged). That would pin the compactor to a dead manager and
// defeat both failover and shutdown. Java bounds the same calls through
// ThriftUtil/general.rpc.timeout.
const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultRPCTimeout     = 30 * time.Second
)

// DialOptions bounds coordinator socket I/O.
type DialOptions struct {
	// ConnectTimeout caps the TCP handshake. Zero uses
	// DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// RPCTimeout caps each read and write on the established socket, so a
	// silent peer surfaces as a transport error instead of a permanent
	// block. Zero uses DefaultRPCTimeout.
	RPCTimeout time.Duration
}

func (o DialOptions) connectTimeout() time.Duration {
	if o.ConnectTimeout <= 0 {
		return DefaultConnectTimeout
	}
	return o.ConnectTimeout
}

func (o DialOptions) rpcTimeout() time.Duration {
	if o.RPCTimeout <= 0 {
		return DefaultRPCTimeout
	}
	return o.RPCTimeout
}

// CoordinatorClient is a connected Thrift client to the
// CompactionCoordinatorService — the side of Bet 1 that doles out
// compaction jobs to external compactors. Construct with DialCoordinator;
// close with Close.
//
// Wire layering matches scanclient.Dial:
// TSocket → TFramedTransport → AccumuloProtocol(TCompactProtocol),
// multiplexed under "coordinator".
type CoordinatorClient struct {
	transport thrift.TTransport
	raw       *compactioncoordinator.CompactionCoordinatorServiceClient
}

// DialCoordinator opens a connection to the compaction coordinator at
// addr (host:port). The coordinator address is published in the manager's
// ServiceLock data in ZooKeeper under ThriftService.COORDINATOR; resolving
// it from ZK is the caller's job — use zk.CoordinatorAddress, and
// re-resolve before each dial so manager failover moves the connection to
// the new primary (see cmd/shoal-compactor's poll loop).
//
// The handshake honours ctx and opts.ConnectTimeout, and every
// subsequent read/write on the returned client is bounded by
// opts.RPCTimeout, so a wedged manager can never stall the caller
// indefinitely. Cancelling ctx after the dial does not interrupt an
// in-flight RPC (thrift transports are not context-aware); callers that
// need immediate cancellation should Close the client, which unblocks
// the pending read.
func DialCoordinator(
	ctx context.Context,
	addr, instanceID, accumuloVersion string,
	opts DialOptions,
) (*CoordinatorClient, error) {
	if addr == "" {
		return nil, errors.New("cclient: empty coordinator addr")
	}
	if instanceID == "" {
		return nil, errors.New("cclient: empty instanceID")
	}
	if accumuloVersion == "" {
		return nil, errors.New("cclient: empty accumuloVersion")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: opts.connectTimeout()}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cclient: open transport to %s: %w", addr, err)
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// The conn is already established, so the socket must not be Opened
	// again; matches scanclient/managerclient's dialTransport.
	conf := &thrift.TConfiguration{
		ConnectTimeout: opts.connectTimeout(),
		SocketTimeout:  opts.rpcTimeout(),
	}
	socket := thrift.NewTSocketFromConnConf(conn, conf)
	framed := thrift.NewTFramedTransportConf(socket, conf)

	proto := protocol.NewClientFactory(instanceID, accumuloVersion).GetProtocol(framed)
	muxed := thrift.NewTMultiplexedProtocol(proto, coordinatorServiceName)
	raw := compactioncoordinator.NewCompactionCoordinatorServiceClient(
		thrift.NewTStandardClient(muxed, muxed))

	return &CoordinatorClient{transport: framed, raw: raw}, nil
}

// Close terminates the underlying transport. It is safe to call
// concurrently with an in-flight RPC — thrift's socketConn guards its
// closed flag atomically and net.Conn.Close unblocks a pending read —
// which is how callers abort a blocked call against a wedged manager.
func (c *CoordinatorClient) Close() error {
	return c.transport.Close()
}

// Raw returns the generated Thrift client for the full coordinator
// surface (getCompactionJob, compactionCompleted, compactionFailed,
// updateCompactionStatus, …).
func (c *CoordinatorClient) Raw() *compactioncoordinator.CompactionCoordinatorServiceClient {
	return c.raw
}
