package ingestclient

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/apache/thrift/lib/go/thrift"
	clientpkg "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletingest"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

// ConditionalStatus is the authoritative result of one conditional mutation.
type ConditionalStatus uint8

const (
	ConditionalUnknown ConditionalStatus = iota
	ConditionalAccepted
	ConditionalRejected
)

// ConditionalOutcome records whether the mutation reached conditionalUpdate.
// A submitted Unknown outcome must never be retried without authoritative
// reconciliation.
type ConditionalOutcome struct {
	Status    ConditionalStatus
	Submitted bool
}

type conditionalRPC interface {
	StartConditionalUpdate(
		context.Context,
		*clientpkg.TInfo,
		*security.TCredentials,
		[][]byte,
		string,
		tabletingest.TDurability,
		string,
	) (*data.TConditionalSession, error)
	ConditionalUpdate(
		context.Context,
		*clientpkg.TInfo,
		data.UpdateID,
		data.CMBatch,
		[]string,
	) ([]*data.TCMResult_, error)
	InvalidateConditionalUpdate(
		context.Context,
		*clientpkg.TInfo,
		data.UpdateID,
	) error
	CloseConditionalUpdate(
		context.Context,
		*clientpkg.TInfo,
		data.UpdateID,
	) error
}

func (p *Pooled) newThriftConditionalRPC(transport io.Closer) (conditionalRPC, error) {
	rawTransport, ok := transport.(thrift.TTransport)
	if !ok {
		return nil, errors.New("ingestclient: conditional lease is not a thrift transport")
	}
	return newThriftClient(rawTransport, p.instanceID, p.accumuloVersion), nil
}

// ConditionalWrite sends one exact-row conditional mutation. This legacy
// convenience form does not expose whether an Unknown result was submitted;
// callers needing safe retry classification use ConditionalWriteWithDurability.
func (p *Pooled) ConditionalWrite(
	ctx context.Context,
	address, tableID string,
	extent *data.TKeyExtent,
	mutation *data.TConditionalMutation,
) (ConditionalStatus, error) {
	outcome, err := p.ConditionalWriteWithDurability(
		ctx, address, "", tableID, extent, mutation, DurabilitySync,
	)
	return outcome.Status, err
}

// ConditionalWriteWithDurability sends one exact-row conditional mutation
// and reports whether submission began.
func (p *Pooled) ConditionalWriteWithDurability(
	ctx context.Context,
	address, expectedServerLock, tableID string,
	extent *data.TKeyExtent,
	mutation *data.TConditionalMutation,
	durability Durability,
	authorizationSets ...[][]byte,
) (ConditionalOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ConditionalOutcome{}, err
	}
	if address == "" || tableID == "" || extent == nil || mutation == nil ||
		mutation.Mutation == nil || len(mutation.Mutation.Row) == 0 {
		return ConditionalOutcome{}, errors.New("ingestclient: invalid conditional mutation")
	}
	if durability > DurabilityNone {
		return ConditionalOutcome{}, errors.New("ingestclient: invalid conditional durability")
	}
	if len(authorizationSets) > 1 {
		return ConditionalOutcome{}, errors.New("ingestclient: multiple conditional authorization sets")
	}
	var authorizations [][]byte
	if len(authorizationSets) == 1 {
		authorizations = cloneConditionalAuthorizations(authorizationSets[0])
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return ConditionalOutcome{}, err
	}
	defer wipeCredentials(credentials)

	key := transportpool.Key{
		Address: address, Service: ingestServiceName,
		InstanceID: p.instanceID, ProtocolVersion: p.accumuloVersion,
	}
	lease, err := p.pool.Acquire(ctx, key, p.dial)
	if err != nil {
		return ConditionalOutcome{}, err
	}
	thriftClient, err := p.newConditionalClient(lease.Transport())
	if err != nil {
		return ConditionalOutcome{}, errors.Join(
			err,
			lease.Invalidate(),
		)
	}
	session, err := thriftClient.StartConditionalUpdate(
		ctx, clientpkg.NewTInfo(), credentials, authorizations, tableID,
		tabletingest.TDurability(durability), "",
	)
	if err != nil {
		return ConditionalOutcome{}, errors.Join(err, finishLease(lease, err))
	}
	if session == nil || session.SessionId == 0 || session.TserverLock == "" {
		_ = thriftClient.InvalidateConditionalUpdate(ctx, clientpkg.NewTInfo(), sessionID(session))
		return ConditionalOutcome{}, errors.Join(
			errors.New("ingestclient: invalid conditional session"),
			lease.Invalidate(),
		)
	}
	if expectedServerLock != "" && session.TserverLock != expectedServerLock {
		_ = thriftClient.InvalidateConditionalUpdate(
			ctx, clientpkg.NewTInfo(), data.UpdateID(session.SessionId),
		)
		return ConditionalOutcome{}, errors.Join(
			fmt.Errorf(
				"ingestclient: conditional server lock changed: located %q, connected %q",
				expectedServerLock, session.TserverLock,
			),
			lease.Invalidate(),
		)
	}

	outcome := ConditionalOutcome{Submitted: true}
	results, updateErr := thriftClient.ConditionalUpdate(
		ctx, clientpkg.NewTInfo(), data.UpdateID(session.SessionId),
		data.CMBatch{extent: []*data.TConditionalMutation{mutation}}, nil,
	)
	if updateErr != nil {
		_ = thriftClient.InvalidateConditionalUpdate(
			context.Background(), clientpkg.NewTInfo(), data.UpdateID(session.SessionId),
		)
		return outcome, errors.Join(updateErr, finishLease(lease, updateErr))
	}
	if len(results) != 1 || results[0] == nil || results[0].Cmid != mutation.ID {
		_ = thriftClient.InvalidateConditionalUpdate(
			context.Background(), clientpkg.NewTInfo(), data.UpdateID(session.SessionId),
		)
		return outcome, errors.Join(
			fmt.Errorf("ingestclient: invalid conditional result %#v", results),
			lease.Invalidate(),
		)
	}
	closeErr := thriftClient.CloseConditionalUpdate(
		context.Background(), clientpkg.NewTInfo(), data.UpdateID(session.SessionId),
	)
	cleanupErr := finishLease(lease, closeErr)
	switch results[0].Status {
	case data.TCMStatus_ACCEPTED:
		outcome.Status = ConditionalAccepted
		return outcome, errors.Join(closeErr, cleanupErr)
	case data.TCMStatus_REJECTED:
		outcome.Status = ConditionalRejected
		return outcome, errors.Join(closeErr, cleanupErr)
	default:
		return outcome, errors.Join(
			fmt.Errorf("ingestclient: conditional status %s", results[0].Status),
			closeErr, cleanupErr,
		)
	}
}

func sessionID(session *data.TConditionalSession) data.UpdateID {
	if session == nil {
		return 0
	}
	return data.UpdateID(session.SessionId)
}

func cloneConditionalAuthorizations(authorizations [][]byte) [][]byte {
	if len(authorizations) == 0 {
		return nil
	}
	cloned := make([][]byte, len(authorizations))
	for i, authorization := range authorizations {
		cloned[i] = append([]byte(nil), authorization...)
	}
	return cloned
}
