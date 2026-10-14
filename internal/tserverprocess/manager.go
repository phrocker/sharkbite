package tserverprocess

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/ingestservice"
	"github.com/phrocker/shoal-oss/internal/protocol"
	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/tserverrpc"
)

const managerServiceName = "mgr"
const clientServiceName = "client"
const tablePermissionRead int8 = 0
const tablePermissionWrite int8 = 2

// ExactAuthenticator admits only explicitly configured trusted identities.
// It is used during system-table bootstrap, when no ClientService endpoint is
// available yet to authenticate the manager or the configured administrative
// user.
type ExactAuthenticator struct {
	Identities []*security.TCredentials
	Writers    []*security.TCredentials
}

func (a ExactAuthenticator) Authenticate(
	_ context.Context,
	candidate *security.TCredentials,
) error {
	for _, trusted := range a.Identities {
		if credentialsEqual(candidate, trusted) {
			return nil
		}
	}
	return errors.New("tserverprocess: credentials rejected")
}

func (a ExactAuthenticator) AuthorizeWrite(
	ctx context.Context,
	candidate *security.TCredentials,
	_ string,
) error {
	for _, trusted := range a.Writers {
		if credentialsEqual(candidate, trusted) {
			return nil
		}
	}
	if err := a.Authenticate(ctx, candidate); err != nil {
		return err
	}
	return errors.New("tserverprocess: credentials are not authorized to write")
}

func (a ExactAuthenticator) Validate(
	ctx context.Context,
	candidate *security.TCredentials,
	requested [][]byte,
	_ []string,
) error {
	if err := a.Authenticate(ctx, candidate); err != nil {
		return err
	}
	if len(requested) != 0 {
		return fmt.Errorf(
			"tserverprocess: principal %q requested unsupported authorizations",
			candidate.Principal)
	}
	return nil
}

func credentialsEqual(left, right *security.TCredentials) bool {
	return left != nil && right != nil &&
		left.Principal == right.Principal &&
		left.TokenClassName == right.TokenClassName &&
		left.InstanceId == right.InstanceId &&
		len(left.Token) == len(right.Token) &&
		subtle.ConstantTimeCompare(left.Token, right.Token) == 1
}

type AddressResolver interface {
	ManagerAddress(context.Context) (string, error)
}

type ReportConnector struct {
	Resolver        AddressResolver
	Credentials     *security.TCredentials
	InstanceID      string
	AccumuloVersion string
	ConnectTimeout  time.Duration
	RPCTimeout      time.Duration
}

func (c ReportConnector) Connect(ctx context.Context) (tserverrpc.ReportClient, error) {
	if c.Resolver == nil || c.Credentials == nil || c.InstanceID == "" || c.AccumuloVersion == "" {
		return nil, errors.New("tserverprocess: incomplete manager report connector")
	}
	address, err := c.Resolver.ManagerAddress(ctx)
	if err != nil {
		return nil, err
	}
	transport, standard, err := dialManagerService(
		ctx, address, c.InstanceID, c.AccumuloVersion, managerServiceName,
		c.ConnectTimeout, c.RPCTimeout,
	)
	if err != nil {
		return nil, err
	}
	return &reportClient{
		transport:   transport,
		raw:         manager.NewManagerClientServiceClient(standard),
		credentials: cloneCredentials(c.Credentials),
	}, nil
}

type reportClient struct {
	transport   thrift.TTransport
	raw         *manager.ManagerClientServiceClient
	credentials *security.TCredentials
}

// ManagerAuthenticator asks Accumulo's ClientService to authenticate the
// exact credentials supplied by a scanner, using the tserver's system
// identity as the trusted caller.
type ManagerAuthenticator struct {
	Resolver        AddressResolver
	System          *security.TCredentials
	InstanceID      string
	AccumuloVersion string
	ConnectTimeout  time.Duration
	RPCTimeout      time.Duration
	TableNames      TableNameResolver
}

func (a ManagerAuthenticator) Authenticate(
	ctx context.Context,
	candidate *security.TCredentials,
) error {
	if candidate == nil || candidate.InstanceId != a.InstanceID {
		return errors.New("tserverprocess: credentials name the wrong instance")
	}
	raw, closeTransport, err := a.securityClient(ctx)
	if err != nil {
		return err
	}
	defer closeTransport()
	ok, err := raw.AuthenticateUser(ctx, nil, a.System, candidate)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("tserverprocess: credentials rejected")
	}
	return nil
}

func (a ManagerAuthenticator) AuthorizeWrite(
	ctx context.Context,
	candidate *security.TCredentials,
	tableID string,
) error {
	if err := a.Authenticate(ctx, candidate); err != nil {
		return err
	}
	if a.TableNames == nil {
		return errors.New("tserverprocess: table permission resolver is unavailable")
	}
	tableName, err := a.TableNames.ResolveName(ctx, tableID)
	if err != nil {
		return err
	}
	raw, closeTransport, err := a.securityClient(ctx)
	if err != nil {
		return err
	}
	defer closeTransport()
	allowed, err := raw.HasTablePermission(
		ctx, nil, a.System, candidate.Principal, tableName, tablePermissionWrite,
	)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: principal %q lacks write permission for table %q",
			ingestservice.ErrPermissionDenied, candidate.Principal, tableName)
	}
	return nil
}

func (a ManagerAuthenticator) securityClient(
	ctx context.Context,
) (*clientgen.ClientServiceClient, func(), error) {
	if a.Resolver == nil || a.System == nil || a.InstanceID == "" || a.AccumuloVersion == "" {
		return nil, func() {}, errors.New("tserverprocess: incomplete manager authenticator")
	}
	address, err := a.Resolver.ManagerAddress(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	transport, standard, err := dialManagerService(
		ctx, address, a.InstanceID, a.AccumuloVersion, clientServiceName,
		a.ConnectTimeout, a.RPCTimeout,
	)
	if err != nil {
		return nil, func() {}, err
	}
	return clientgen.NewClientServiceClient(standard), func() { _ = transport.Close() }, nil
}

type TableNameResolver interface {
	ResolveName(context.Context, string) (string, error)
}

func (a ManagerAuthenticator) Validate(
	ctx context.Context,
	candidate *security.TCredentials,
	requested [][]byte,
	tableIDs []string,
) error {
	if candidate == nil || candidate.InstanceId != a.InstanceID {
		return errors.New("tserverprocess: scan credentials name the wrong instance")
	}
	if a.Resolver == nil || a.System == nil || a.InstanceID == "" || a.AccumuloVersion == "" {
		return errors.New("tserverprocess: incomplete manager authenticator")
	}
	address, err := a.Resolver.ManagerAddress(ctx)
	if err != nil {
		return err
	}
	transport, standard, err := dialManagerService(
		ctx, address, a.InstanceID, a.AccumuloVersion, clientServiceName,
		a.ConnectTimeout, a.RPCTimeout,
	)
	if err != nil {
		return err
	}
	defer transport.Close()
	raw := clientgen.NewClientServiceClient(standard)
	ok, err := raw.AuthenticateUser(ctx, nil, a.System, candidate)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("tserverprocess: scan credentials rejected")
	}
	granted, err := raw.GetUserAuthorizations(ctx, nil, a.System, candidate.Principal)
	if err != nil {
		return err
	}
	if err := validateAuthorizations(candidate.Principal, requested, granted); err != nil {
		return err
	}
	if len(tableIDs) > 0 && a.TableNames == nil {
		return errors.New("tserverprocess: table permission resolver is unavailable")
	}
	for _, tableID := range tableIDs {
		tableName, err := a.TableNames.ResolveName(ctx, tableID)
		if err != nil {
			return err
		}
		allowed, err := raw.HasTablePermission(
			ctx, nil, a.System, candidate.Principal, tableName, tablePermissionRead,
		)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf(
				"tserverprocess: principal %q lacks read permission for table %q",
				candidate.Principal, tableName)
		}
	}
	return nil
}

func validateAuthorizations(principal string, requested, granted [][]byte) error {
	allowed := make(map[string]struct{}, len(granted))
	for _, authorization := range granted {
		allowed[string(authorization)] = struct{}{}
	}
	for _, authorization := range requested {
		if _, ok := allowed[string(authorization)]; !ok {
			return fmt.Errorf(
				"tserverprocess: principal %q requested an ungranted authorization",
				principal)
		}
	}
	return nil
}

func (c *reportClient) ReportTabletStatus(
	ctx context.Context,
	server string,
	state manager.TabletLoadState,
	extent tserver.Extent,
) error {
	return c.raw.ReportTabletStatus(ctx, nil, c.credentials, server, state, &data.TKeyExtent{
		Table: []byte(extent.TableID), PrevEndRow: extent.PrevEndRow, EndRow: extent.EndRow,
	})
}

func (c *reportClient) Close() error { return c.transport.Close() }

func dialManagerService(
	ctx context.Context,
	address, instanceID, accumuloVersion, service string,
	connectTimeout, rpcTimeout time.Duration,
) (thrift.TTransport, thrift.TClient, error) {
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	if rpcTimeout <= 0 {
		rpcTimeout = 30 * time.Second
	}
	conn, err := (&net.Dialer{Timeout: connectTimeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("dial manager %s: %w", address, err)
	}
	conf := &thrift.TConfiguration{ConnectTimeout: connectTimeout, SocketTimeout: rpcTimeout}
	socket := thrift.NewTSocketFromConnConf(conn, conf)
	framed := thrift.NewTFramedTransportConf(socket, conf)
	proto := protocol.NewClientFactory(instanceID, accumuloVersion).GetProtocol(framed)
	mux := thrift.NewTMultiplexedProtocol(proto, service)
	return framed, thrift.NewTStandardClient(mux, mux), nil
}

func cloneCredentials(in *security.TCredentials) *security.TCredentials {
	if in == nil {
		return nil
	}
	out := *in
	out.Token = append([]byte(nil), in.Token...)
	return &out
}
