package ingestclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

// Pooled starts update sessions on exclusive transports from a shared pool.
type Pooled struct {
	pool            *transportpool.Pool
	instanceID      string
	accumuloVersion string
	dialTimeout     time.Duration

	mu          sync.RWMutex
	credentials *security.TCredentials
	closed      bool

	dial                 transportpool.DialFunc
	newClient            func(io.Closer) (ingestRPC, error)
	newConditionalClient func(io.Closer) (conditionalRPC, error)
}

var _ Adapter = (*Pooled)(nil)

// NewPooled constructs a pooled tablet-ingest adapter.
func NewPooled(
	pool *transportpool.Pool,
	instanceID, accumuloVersion string,
	credentials *security.TCredentials,
	dialTimeout time.Duration,
) (*Pooled, error) {
	switch {
	case pool == nil:
		return nil, errors.New("ingestclient: nil transport pool")
	case instanceID == "":
		return nil, errors.New("ingestclient: empty instanceID")
	case accumuloVersion == "":
		return nil, errors.New("ingestclient: empty accumuloVersion")
	case credentials == nil:
		return nil, errors.New("ingestclient: nil credentials")
	case dialTimeout < 0:
		return nil, errors.New("ingestclient: negative dial timeout")
	}

	pooled := &Pooled{
		pool:            pool,
		instanceID:      instanceID,
		accumuloVersion: accumuloVersion,
		dialTimeout:     dialTimeout,
		credentials:     cloneCredentials(credentials),
	}
	pooled.dial = pooled.dialThrift
	pooled.newClient = pooled.newThriftRPC
	pooled.newConditionalClient = pooled.newThriftConditionalRPC
	return pooled, nil
}

// Start acquires one transport for the complete update RPC lifecycle.
func (p *Pooled) Start(
	ctx context.Context,
	address string,
	durability Durability,
) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validDurability(durability) {
		return nil, errors.New("ingestclient: invalid durability")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	defer wipeCredentials(credentials)

	key := transportpool.Key{
		Address:         address,
		Service:         ingestServiceName,
		InstanceID:      p.instanceID,
		ProtocolVersion: p.accumuloVersion,
	}
	lease, err := p.pool.Acquire(ctx, key, p.dial)
	if err != nil {
		return nil, err
	}
	client, err := p.newClient(lease.Transport())
	if err != nil {
		return nil, errors.Join(err, lease.Invalidate())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, lease.Close())
	}

	updateID, rpcErr := client.Start(ctx, credentials, durability)
	if rpcErr != nil {
		return nil, errors.Join(rpcErr, finishLease(lease, rpcErr))
	}
	return &Session{
		lease:    lease,
		client:   client,
		updateID: updateID,
	}, nil
}

// Close forgets the adapter's credential copy. Active sessions retain their
// exclusive leases until they close, cancel, or encounter a wire failure.
func (p *Pooled) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	wipeCredentials(p.credentials)
	p.credentials = nil
	return nil
}

// UpdateCredentials atomically replaces the adapter's private credential copy.
func (p *Pooled) UpdateCredentials(credentials *security.TCredentials) error {
	if credentials == nil {
		return errors.New("ingestclient: nil Credentials")
	}
	replacement := cloneCredentials(credentials)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		wipeCredentials(replacement)
		return ErrClosed
	}
	old := p.credentials
	p.credentials = replacement
	wipeCredentials(old)
	return nil
}

func (p *Pooled) credentialsForRPC() (*security.TCredentials, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, ErrClosed
	}
	return cloneCredentials(p.credentials), nil
}

func (p *Pooled) dialThrift(
	ctx context.Context,
	key transportpool.Key,
) (io.Closer, error) {
	return dialTransport(ctx, key.Address, p.dialTimeout)
}

func (p *Pooled) newThriftRPC(transport io.Closer) (ingestRPC, error) {
	return newThriftRPC(transport, p.instanceID, p.accumuloVersion)
}

type sessionState uint8

const (
	sessionActive sessionState = iota
	sessionClosed
	sessionCancelled
	sessionBroken
)

// Session owns one update ID and one exclusive pooled transport.
type Session struct {
	mu       sync.Mutex
	lease    *transportpool.Lease
	client   ingestRPC
	updateID data.UpdateID
	state    sessionState

	closeResult *data.UpdateErrors
	closeErr    error
}

// UpdateID returns the server-assigned update identifier.
func (s *Session) UpdateID() int64 {
	return int64(s.updateID)
}

// Apply sends one extent's mutation batch on the update session.
func (s *Session) Apply(
	ctx context.Context,
	extent *data.TKeyExtent,
	mutations []*data.TMutation,
) error {
	if err := validateApply(extent, mutations); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sessionActive {
		return ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	rpcErr := s.client.Apply(ctx, s.updateID, extent, mutations)
	if !isWireFailure(rpcErr) {
		return rpcErr
	}
	cleanupErr := s.lease.Invalidate()
	s.state = sessionBroken
	return errors.Join(rpcErr, cleanupErr)
}

// Close commits the update session and returns Accumulo's per-extent errors.
// Repeated Close calls return the first result without another RPC.
func (s *Session) Close(ctx context.Context) (*data.UpdateErrors, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case sessionClosed:
		return s.closeResult, s.closeErr
	case sessionActive:
	default:
		return nil, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	result, rpcErr := s.client.Close(ctx, s.updateID)
	s.closeResult = result
	s.closeErr = errors.Join(rpcErr, finishLease(s.lease, rpcErr))
	s.state = sessionClosed
	return s.closeResult, s.closeErr
}

// Cancel abandons the update session and releases its transport.
func (s *Session) Cancel(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sessionActive {
		return false, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	cancelled, rpcErr := s.client.Cancel(ctx, s.updateID)
	cleanupErr := finishLease(s.lease, rpcErr)
	s.state = sessionCancelled
	return cancelled, errors.Join(rpcErr, cleanupErr)
}

func finishLease(lease *transportpool.Lease, rpcErr error) error {
	if isWireFailure(rpcErr) {
		return lease.Invalidate()
	}
	return lease.Close()
}

func cloneCredentials(credentials *security.TCredentials) *security.TCredentials {
	if credentials == nil {
		return nil
	}
	cloned := *credentials
	cloned.Token = append([]byte(nil), credentials.Token...)
	return &cloned
}

func wipeCredentials(credentials *security.TCredentials) {
	if credentials == nil {
		return
	}
	for index := range credentials.Token {
		credentials.Token[index] = 0
	}
	credentials.Token = nil
}
