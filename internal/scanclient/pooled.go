package scanclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

// Lifecycle is the version-neutral internal tablet-scan RPC boundary.
type Lifecycle interface {
	Start(context.Context, string, StartRequest) (*data.InitialScan, error)
	Continue(context.Context, string, data.ScanID, int64) (*data.ScanResult_, error)
	CloseScan(context.Context, string, data.ScanID) error
}

// MultiLifecycle is the grouped tablet-scan RPC boundary.
type MultiLifecycle interface {
	StartMulti(context.Context, string, MultiStartRequest) (*data.InitialMultiScan, error)
	ContinueMulti(context.Context, string, data.ScanID, int64) (*data.MultiScanResult_, error)
	CloseMultiScan(context.Context, string, data.ScanID) error
}

// Adapter is a Lifecycle that can forget connector-owned credentials.
type Adapter interface {
	Lifecycle
	Close() error
}

// MultiAdapter supports both single and grouped scan lifecycles.
type MultiAdapter interface {
	Adapter
	MultiLifecycle
}

// SimpleScanLifecycle drains a metadata-style scan through its stateful
// continuation session, then closes the server-side session.
func SimpleScanLifecycle(
	ctx context.Context,
	lifecycle Lifecycle,
	address string,
	req SimpleScanRequest,
) (*data.InitialScan, error) {
	if lifecycle == nil {
		return nil, errors.New("scanclient: nil lifecycle")
	}
	batchSize := req.BatchSize
	if batchSize == 0 {
		batchSize = defaultBatchSize
	}
	scan, err := lifecycle.Start(ctx, address, StartRequest{
		Credentials:        req.Credentials,
		Extent:             req.Extent,
		Range:              req.Range,
		BatchSize:          batchSize,
		Authorizations:     req.Authorizations,
		ReadaheadThreshold: int64(defaultBatchSize),
	})
	if err != nil {
		return nil, err
	}
	if scan == nil || scan.ScanID == 0 {
		return scan, nil
	}
	id := scan.ScanID
	for scan.Result_ != nil && scan.Result_.More {
		next, continueErr := lifecycle.Continue(ctx, address, id, 0)
		if continueErr != nil {
			_ = lifecycle.CloseScan(context.WithoutCancel(ctx), address, id)
			return nil, continueErr
		}
		if next == nil {
			_ = lifecycle.CloseScan(context.WithoutCancel(ctx), address, id)
			return nil, errors.New("scanclient: nil continuation result")
		}
		scan.Result_.Results = append(scan.Result_.Results, next.Results...)
		scan.Result_.More = next.More
	}
	if closeErr := lifecycle.CloseScan(ctx, address, id); closeErr != nil {
		return scan, &CleanupError{ScanID: id, Err: closeErr}
	}
	scan.ScanID = 0
	return scan, nil
}

// Pooled is the connector-facing tablet-scan RPC adapter.
type Pooled struct {
	pool            *transportpool.Pool
	instanceID      string
	accumuloVersion string
	dialTimeout     time.Duration

	mu          sync.RWMutex
	credentials *security.TCredentials
	closed      bool

	dial      transportpool.DialFunc
	newClient func(io.Closer) (scanRPC, error)
}

var _ MultiAdapter = (*Pooled)(nil)

// NewPooled constructs a pooled tablet-scan adapter.
func NewPooled(
	pool *transportpool.Pool,
	instanceID, accumuloVersion string,
	credentials *security.TCredentials,
	dialTimeout time.Duration,
) (*Pooled, error) {
	switch {
	case pool == nil:
		return nil, errors.New("scanclient: nil transport pool")
	case instanceID == "":
		return nil, errors.New("scanclient: empty instanceID")
	case accumuloVersion == "":
		return nil, errors.New("scanclient: empty accumuloVersion")
	case credentials == nil:
		return nil, errors.New("scanclient: nil Credentials")
	case dialTimeout < 0:
		return nil, errors.New("scanclient: negative dial timeout")
	}

	p := &Pooled{
		pool:            pool,
		instanceID:      instanceID,
		accumuloVersion: accumuloVersion,
		dialTimeout:     dialTimeout,
		credentials:     cloneCredentials(credentials),
	}
	p.dial = p.dialThrift
	p.newClient = p.newThriftRPC
	return p, nil
}

// Start acquires an exclusive scan-service transport and starts a scan.
func (p *Pooled) Start(
	ctx context.Context,
	address string,
	req StartRequest,
) (*data.InitialScan, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	req.Credentials = credentials
	if err := validateStartRequest(req); err != nil {
		return nil, err
	}
	return withLease(p, ctx, address, func(client scanRPC) (*data.InitialScan, error) {
		return client.Start(ctx, req)
	})
}

// StartMulti acquires an exclusive scan-service transport and starts a grouped scan.
func (p *Pooled) StartMulti(
	ctx context.Context,
	address string,
	req MultiStartRequest,
) (*data.InitialMultiScan, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	req.Credentials = credentials
	if err := validateMultiStartRequest(req); err != nil {
		return nil, err
	}
	return withLease(p, ctx, address, func(client scanRPC) (*data.InitialMultiScan, error) {
		return client.StartMulti(ctx, req)
	})
}

// Continue acquires an exclusive scan-service transport and continues scanID.
func (p *Pooled) Continue(
	ctx context.Context,
	address string,
	scanID data.ScanID,
	busyTimeout int64,
) (*data.ScanResult_, error) {
	return withLease(p, ctx, address, func(client scanRPC) (*data.ScanResult_, error) {
		return client.Continue(ctx, scanID, busyTimeout)
	})
}

// ContinueMulti acquires an exclusive scan-service transport and continues scanID.
func (p *Pooled) ContinueMulti(
	ctx context.Context,
	address string,
	scanID data.ScanID,
	busyTimeout int64,
) (*data.MultiScanResult_, error) {
	return withLease(p, ctx, address, func(client scanRPC) (*data.MultiScanResult_, error) {
		return client.ContinueMulti(ctx, scanID, busyTimeout)
	})
}

// CloseScan acquires an exclusive scan-service transport and closes scanID.
func (p *Pooled) CloseScan(
	ctx context.Context,
	address string,
	scanID data.ScanID,
) error {
	_, err := withLease(p, ctx, address, func(client scanRPC) (struct{}, error) {
		return struct{}{}, client.Close(ctx, scanID)
	})
	return err
}

// CloseMultiScan acquires an exclusive scan-service transport and closes scanID.
func (p *Pooled) CloseMultiScan(
	ctx context.Context,
	address string,
	scanID data.ScanID,
) error {
	_, err := withLease(p, ctx, address, func(client scanRPC) (struct{}, error) {
		return struct{}{}, client.CloseMulti(ctx, scanID)
	})
	return err
}

// Close forgets the adapter's credential copy. Connector owns and closes the
// shared transport pool separately.
func (p *Pooled) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.credentials != nil {
		for i := range p.credentials.Token {
			p.credentials.Token[i] = 0
		}
		p.credentials.Token = nil
	}
	return nil
}

// UpdateCredentials atomically replaces the adapter's private credential copy.
func (p *Pooled) UpdateCredentials(credentials *security.TCredentials) error {
	if credentials == nil {
		return errors.New("scanclient: nil Credentials")
	}
	replacement := cloneCredentials(credentials)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for i := range replacement.Token {
			replacement.Token[i] = 0
		}
		return errors.New("scanclient: pooled client is closed")
	}
	old := p.credentials
	p.credentials = replacement
	if old != nil {
		for i := range old.Token {
			old.Token[i] = 0
		}
	}
	return nil
}

func withLease[T any](
	p *Pooled,
	ctx context.Context,
	address string,
	call func(scanRPC) (T, error),
) (result T, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := p.credentialsForRPC(); err != nil {
		return result, err
	}

	key := transportpool.Key{
		Address:         address,
		Service:         scanServiceName,
		InstanceID:      p.instanceID,
		ProtocolVersion: p.accumuloVersion,
	}
	lease, err := p.pool.Acquire(ctx, key, p.dial)
	if err != nil {
		return result, err
	}

	client, err := p.newClient(lease.Transport())
	if err != nil {
		return result, errors.Join(err, lease.Invalidate())
	}
	if err := ctx.Err(); err != nil {
		return result, errors.Join(err, lease.Close())
	}

	result, rpcErr := call(client)
	var cleanupErr error
	if isWireFailure(rpcErr) {
		cleanupErr = lease.Invalidate()
	} else {
		cleanupErr = lease.Close()
	}
	return result, errors.Join(rpcErr, cleanupErr)
}

func (p *Pooled) credentialsForRPC() (*security.TCredentials, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errors.New("scanclient: pooled client is closed")
	}
	return cloneCredentials(p.credentials), nil
}

func (p *Pooled) dialThrift(
	ctx context.Context,
	key transportpool.Key,
) (io.Closer, error) {
	return dialTransport(ctx, key.Address, p.dialTimeout)
}

func (p *Pooled) newThriftRPC(transport io.Closer) (scanRPC, error) {
	thriftTransport, ok := transport.(thrift.TTransport)
	if !ok {
		return nil, fmt.Errorf("scanclient: pooled transport %T is not a thrift transport", transport)
	}
	raw := newThriftClient(thriftTransport, p.instanceID, p.accumuloVersion)
	return thriftScanRPC{raw: raw}, nil
}

func cloneCredentials(credentials *security.TCredentials) *security.TCredentials {
	if credentials == nil {
		return nil
	}
	clone := *credentials
	clone.Token = append([]byte(nil), credentials.Token...)
	return &clone
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
