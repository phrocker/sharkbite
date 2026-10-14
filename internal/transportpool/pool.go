// Package transportpool provides exclusive, reusable transport leases for
// Accumulo RPC clients.
package transportpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	// ErrClosed indicates that the pool was closed.
	ErrClosed = errors.New("transportpool: closed")
)

// Config controls idle transport retention.
type Config struct {
	IdleTimeout        time.Duration
	MaxIdlePerEndpoint int
}

// Key identifies a wire-compatible endpoint and service.
type Key struct {
	Address         string
	Service         string
	InstanceID      string
	ProtocolVersion string
}

// DialFunc creates and opens a transport for key.
type DialFunc func(context.Context, Key) (io.Closer, error)

type entry struct {
	key       Key
	transport io.Closer
	idleSince time.Time
	timer     *time.Timer
	idle      bool
	closed    bool
}

// Pool owns idle and leased transports. A transport is never handed to more
// than one Lease at a time.
type Pool struct {
	mu     sync.Mutex
	config Config
	idle   map[Key][]*entry
	leased map[*entry]struct{}
	closed bool
}

// New constructs an empty Pool.
func New(config Config) (*Pool, error) {
	if config.IdleTimeout < 0 {
		return nil, errors.New("transportpool: IdleTimeout must not be negative")
	}
	if config.MaxIdlePerEndpoint < 0 {
		return nil, errors.New("transportpool: MaxIdlePerEndpoint must not be negative")
	}
	return &Pool{
		config: config,
		idle:   make(map[Key][]*entry),
		leased: make(map[*entry]struct{}),
	}, nil
}

// Lease is an exclusive checkout from a Pool.
type Lease struct {
	pool  *Pool
	entry *entry
	once  sync.Once
	err   error
}

// Transport returns the exclusively leased transport.
func (l *Lease) Transport() io.Closer { return l.entry.transport }

// Close returns a healthy transport to the idle pool.
func (l *Lease) Close() error {
	l.once.Do(func() {
		l.err = l.pool.release(l.entry, true)
	})
	return l.err
}

// Invalidate closes the transport instead of returning it to the pool.
func (l *Lease) Invalidate() error {
	l.once.Do(func() {
		l.err = l.pool.release(l.entry, false)
	})
	return l.err
}

// Acquire returns an exclusive transport for key, reusing a healthy idle
// transport when possible. The pool lock is never held while dial runs.
func (p *Pool) Acquire(ctx context.Context, key Key, dial DialFunc) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if dial == nil {
		return nil, errors.New("transportpool: dial function is required")
	}

	for {
		now := time.Now()
		var expired *entry

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		entries := p.idle[key]
		if len(entries) > 0 {
			candidate := entries[len(entries)-1]
			entries = entries[:len(entries)-1]
			if len(entries) == 0 {
				delete(p.idle, key)
			} else {
				p.idle[key] = entries
			}
			if p.config.IdleTimeout > 0 && now.Sub(candidate.idleSince) >= p.config.IdleTimeout {
				candidate.closed = true
				candidate.idle = false
				if candidate.timer != nil {
					candidate.timer.Stop()
					candidate.timer = nil
				}
				expired = candidate
			} else {
				if err := ctx.Err(); err != nil {
					p.idle[key] = append(p.idle[key], candidate)
					p.mu.Unlock()
					return nil, err
				}
				candidate.idle = false
				if candidate.timer != nil {
					candidate.timer.Stop()
					candidate.timer = nil
				}
				p.leased[candidate] = struct{}{}
				p.mu.Unlock()
				return &Lease{pool: p, entry: candidate}, nil
			}
		}
		p.mu.Unlock()

		if expired != nil {
			_ = expired.transport.Close()
			continue
		}
		break
	}

	transport, err := dial(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("transportpool: dial %s/%s: %w", key.Address, key.Service, err)
	}
	if transport == nil {
		return nil, errors.New("transportpool: dial returned a nil transport")
	}
	if err := ctx.Err(); err != nil {
		_ = transport.Close()
		return nil, err
	}
	e := &entry{key: key, transport: transport}

	p.mu.Lock()
	if p.closed {
		e.closed = true
		p.mu.Unlock()
		_ = transport.Close()
		return nil, ErrClosed
	}
	p.leased[e] = struct{}{}
	p.mu.Unlock()
	return &Lease{pool: p, entry: e}, nil
}

func (p *Pool) release(e *entry, reusable bool) error {
	p.mu.Lock()
	if e.closed {
		p.mu.Unlock()
		return nil
	}
	delete(p.leased, e)
	if reusable && !p.closed && p.config.MaxIdlePerEndpoint > 0 {
		entries := p.idle[e.key]
		if len(entries) < p.config.MaxIdlePerEndpoint {
			e.idleSince = time.Now()
			e.idle = true
			p.idle[e.key] = append(entries, e)
			if p.config.IdleTimeout > 0 {
				e.timer = time.AfterFunc(p.config.IdleTimeout, func() {
					p.expire(e)
				})
			}
			p.mu.Unlock()
			return nil
		}
	}
	e.closed = true
	e.idle = false
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	p.mu.Unlock()
	return e.transport.Close()
}

func (p *Pool) expire(e *entry) {
	p.mu.Lock()
	if p.closed || e.closed || !e.idle {
		p.mu.Unlock()
		return
	}
	entries := p.idle[e.key]
	for i, candidate := range entries {
		if candidate != e {
			continue
		}
		entries = append(entries[:i], entries[i+1:]...)
		if len(entries) == 0 {
			delete(p.idle, e.key)
		} else {
			p.idle[e.key] = entries
		}
		e.idle = false
		e.closed = true
		e.timer = nil
		p.mu.Unlock()
		_ = e.transport.Close()
		return
	}
	p.mu.Unlock()
}

// Close closes all idle and leased transports. It is idempotent.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var entries []*entry
	for _, idle := range p.idle {
		entries = append(entries, idle...)
	}
	for e := range p.leased {
		entries = append(entries, e)
	}
	p.idle = make(map[Key][]*entry)
	p.leased = make(map[*entry]struct{})
	for _, e := range entries {
		e.closed = true
		e.idle = false
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
	}
	p.mu.Unlock()

	var closeErr error
	for _, e := range entries {
		if err := e.transport.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
}

func validateKey(key Key) error {
	switch {
	case key.Address == "":
		return errors.New("transportpool: key Address is required")
	case key.Service == "":
		return errors.New("transportpool: key Service is required")
	case key.InstanceID == "":
		return errors.New("transportpool: key InstanceID is required")
	case key.ProtocolVersion == "":
		return errors.New("transportpool: key ProtocolVersion is required")
	default:
		return nil
	}
}
