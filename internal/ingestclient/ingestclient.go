// Package ingestclient implements the internal Accumulo 4 tablet-ingest RPC
// boundary.
package ingestclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/protocol"
	clientpkg "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletingest"
)

const ingestServiceName = "ingest"

var (
	// ErrClosed indicates that the pooled adapter no longer accepts sessions.
	ErrClosed = errors.New("ingestclient: pooled client is closed")

	// ErrSessionClosed indicates that an update session is already terminal.
	ErrSessionClosed = errors.New("ingestclient: update session is closed")
)

// Durability selects the tablet server's write-ahead-log behavior.
type Durability uint8

const (
	DurabilityDefault Durability = iota
	DurabilitySync
	DurabilityFlush
	DurabilityLog
	DurabilityNone
)

// Lifecycle starts an update session on a tablet server.
type Lifecycle interface {
	Start(context.Context, string, Durability) (*Session, error)
}

// Adapter is a Lifecycle that can forget connector-owned credentials.
type Adapter interface {
	Lifecycle
	Close() error
}

type ingestRPC interface {
	Start(context.Context, *security.TCredentials, Durability) (data.UpdateID, error)
	Apply(context.Context, data.UpdateID, *data.TKeyExtent, []*data.TMutation) error
	Close(context.Context, data.UpdateID) (*data.UpdateErrors, error)
	Cancel(context.Context, data.UpdateID) (bool, error)
}

type thriftIngestRPC struct {
	raw *tabletingest.TabletIngestClientServiceClient
}

func (c thriftIngestRPC) Start(
	ctx context.Context,
	credentials *security.TCredentials,
	durability Durability,
) (data.UpdateID, error) {
	return c.raw.StartUpdate(
		ctx,
		clientpkg.NewTInfo(),
		credentials,
		tabletingest.TDurability(durability),
	)
}

func (c thriftIngestRPC) Apply(
	ctx context.Context,
	updateID data.UpdateID,
	extent *data.TKeyExtent,
	mutations []*data.TMutation,
) error {
	return c.raw.ApplyUpdates(ctx, clientpkg.NewTInfo(), updateID, extent, mutations)
}

func (c thriftIngestRPC) Close(
	ctx context.Context,
	updateID data.UpdateID,
) (*data.UpdateErrors, error) {
	return c.raw.CloseUpdate(ctx, clientpkg.NewTInfo(), updateID)
}

func (c thriftIngestRPC) Cancel(
	ctx context.Context,
	updateID data.UpdateID,
) (bool, error) {
	return c.raw.CancelUpdate(ctx, clientpkg.NewTInfo(), updateID)
}

func newThriftClient(
	transport thrift.TTransport,
	instanceID, accumuloVersion string,
) *tabletingest.TabletIngestClientServiceClient {
	proto := protocol.NewClientFactory(instanceID, accumuloVersion).GetProtocol(transport)
	muxed := thrift.NewTMultiplexedProtocol(proto, ingestServiceName)
	return tabletingest.NewTabletIngestClientServiceClient(
		thrift.NewTStandardClient(muxed, muxed),
	)
}

func dialTransport(
	ctx context.Context,
	address string,
	timeout time.Duration,
) (thrift.TTransport, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	config := &thrift.TConfiguration{}
	socket := thrift.NewTSocketFromConnConf(conn, config)
	return thrift.NewTFramedTransportConf(socket, config), nil
}

func newThriftRPC(
	transport io.Closer,
	instanceID, accumuloVersion string,
) (ingestRPC, error) {
	thriftTransport, ok := transport.(thrift.TTransport)
	if !ok {
		return nil, fmt.Errorf(
			"ingestclient: pooled transport %T is not a thrift transport",
			transport,
		)
	}
	return thriftIngestRPC{
		raw: newThriftClient(thriftTransport, instanceID, accumuloVersion),
	}, nil
}

func validDurability(durability Durability) bool {
	return durability <= DurabilityNone
}

func validateApply(extent *data.TKeyExtent, mutations []*data.TMutation) error {
	switch {
	case extent == nil:
		return errors.New("ingestclient: nil extent")
	case len(mutations) == 0:
		return errors.New("ingestclient: apply requires at least one mutation")
	}
	for index, mutation := range mutations {
		if mutation == nil {
			return fmt.Errorf("ingestclient: nil mutation at index %d", index)
		}
	}
	return nil
}

func isWireFailure(err error) bool {
	if err == nil {
		return false
	}
	var transportErr thrift.TTransportException
	if errors.As(err, &transportErr) {
		return true
	}
	var protocolErr thrift.TProtocolException
	if errors.As(err, &protocolErr) {
		return true
	}
	var thriftErr thrift.TException
	if !errors.As(err, &thriftErr) {
		return false
	}
	switch thriftErr.TExceptionType() {
	case thrift.TExceptionTypeProtocol, thrift.TExceptionTypeTransport:
		return true
	case thrift.TExceptionTypeApplication, thrift.TExceptionTypeCompiled:
		return false
	default:
		return true
	}
}
