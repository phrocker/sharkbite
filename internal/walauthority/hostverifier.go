package walauthority

import (
	"context"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

// HostedOwnerVerifier binds the WAL authority to the exact Host assignment
// attempt and ServiceLock generations that brought the tablet online.
type HostedOwnerVerifier struct {
	Host    *tserver.Host
	Fence   tserver.Fence
	Attempt tserver.Attempt
}

func (v HostedOwnerVerifier) Verify(ctx context.Context, fence ingestrouter.Fence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.Host == nil || !v.Attempt.Valid() {
		return ErrStaleOwner
	}
	if fence.ServerGeneration != v.Fence.Server.String() ||
		fence.ManagerGeneration != v.Fence.Manager.String() ||
		fence.Assignment != v.Attempt.Assignment() {
		return fmt.Errorf("%w: request fence does not name hosted attempt", ErrStaleOwner)
	}
	return v.Host.VerifyHosted(v.Fence, v.Attempt)
}

// AssignedOwnerVerifier is the recovery-phase counterpart to
// HostedOwnerVerifier. It accepts the loading assignment but still rejects an
// unload, replacement attempt, or ServiceLock change.
type AssignedOwnerVerifier HostedOwnerVerifier

func (v AssignedOwnerVerifier) Verify(ctx context.Context, fence ingestrouter.Fence) error {
	hosted := HostedOwnerVerifier(v)
	if err := ctx.Err(); err != nil {
		return err
	}
	if hosted.Host == nil || !hosted.Attempt.Valid() {
		return ErrStaleOwner
	}
	if fence.ServerGeneration != hosted.Fence.Server.String() ||
		fence.ManagerGeneration != hosted.Fence.Manager.String() ||
		fence.Assignment != hosted.Attempt.Assignment() {
		return fmt.Errorf("%w: request fence does not name assigned attempt", ErrStaleOwner)
	}
	return hosted.Host.VerifyAssigned(hosted.Fence, hosted.Attempt)
}
