package main

import (
	"context"
	"sync"
	"sync/atomic"

	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

type credentialAuthenticator interface {
	Authenticate(context.Context, *security.TCredentials) error
}

// compactorRole is the coordinator-facing CompactorService state for the
// single job this worker executes at a time.
type compactorRole struct {
	mu        sync.RWMutex
	job       *tabletserver.TExternalCompactionJob
	cancel    context.CancelFunc
	cancelled *atomic.Bool
	auth      credentialAuthenticator
}

func (r *compactorRole) authorize(ctx context.Context, credentials *security.TCredentials) error {
	if r.auth == nil {
		return nil
	}
	if err := r.auth.Authenticate(ctx, credentials); err != nil {
		user := ""
		if credentials != nil {
			user = credentials.Principal
		}
		return &clientgen.ThriftSecurityException{
			User: user,
			Code: clientgen.SecurityErrorCode_PERMISSION_DENIED,
		}
	}
	return nil
}

func (r *compactorRole) begin(
	job *tabletserver.TExternalCompactionJob,
	cancel context.CancelFunc,
	cancelled *atomic.Bool,
) {
	r.mu.Lock()
	r.job = job
	r.cancel = cancel
	r.cancelled = cancelled
	r.mu.Unlock()
}

func (r *compactorRole) end(ecid string) {
	r.mu.Lock()
	if r.job != nil && r.job.GetExternalCompactionId() == ecid {
		r.job = nil
		r.cancel = nil
		r.cancelled = nil
	}
	r.mu.Unlock()
}

func (r *compactorRole) GetRunningCompaction(
	ctx context.Context,
	_ *clientgen.TInfo,
	credentials *security.TCredentials,
) (*tabletserver.TExternalCompactionJob, error) {
	if err := r.authorize(ctx, credentials); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.job, nil
}

func (r *compactorRole) GetRunningCompactionId(
	ctx context.Context,
	_ *clientgen.TInfo,
	credentials *security.TCredentials,
) (string, error) {
	if err := r.authorize(ctx, credentials); err != nil {
		return "", err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.job == nil {
		return "", nil
	}
	return r.job.GetExternalCompactionId(), nil
}

func (r *compactorRole) GetActiveCompactions(
	ctx context.Context,
	_ *clientgen.TInfo,
	credentials *security.TCredentials,
) ([]*tabletserver.ActiveCompaction, error) {
	if err := r.authorize(ctx, credentials); err != nil {
		return nil, err
	}
	return nil, nil
}

func (r *compactorRole) Cancel(
	ctx context.Context,
	_ *clientgen.TInfo,
	credentials *security.TCredentials,
	ecid string,
) error {
	if err := r.authorize(ctx, credentials); err != nil {
		// Accumulo's cancel RPC has no declared security exception. Reject the
		// request without surfacing an internal Thrift application error.
		return nil
	}
	r.mu.RLock()
	job, cancel, cancelled := r.job, r.cancel, r.cancelled
	r.mu.RUnlock()
	if job == nil || job.GetExternalCompactionId() != ecid || cancel == nil {
		return nil
	}
	cancelled.Store(true)
	cancel()
	return nil
}
