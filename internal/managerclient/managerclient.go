package managerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/protocol"
	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

const (
	fateServiceName      = "fate"
	managerServiceName   = "mgr"
	clientServiceName    = "client"
	fateFinishTimeout    = 5 * time.Second
	waitForFlushMaxLoops = int64(1<<63 - 1)
	noWaitFlushMaxLoops  = int64(1)
)

type Operation int

const (
	TableCreate Operation = iota
	TableDelete
	TableRename
	// TableBulkImport submits Accumulo's Bulk Import V2 FATE operation
	// (TABLE_BULK_IMPORT2). It takes exactly 3 arguments: the destination
	// table's canonical ID (not its name — unlike every other Operation
	// here), the bulk directory path, and a "true"/"false" setTime flag. The
	// caller is responsible for staging the bulk directory (files flat,
	// loadmap.json written) before submitting; see internal/promotion.
	TableBulkImport
	// TableSplit submits Accumulo 4's manager split FATE operation
	// (TABLE_SPLIT). Arguments are positional and variadic: the table's
	// canonical ID, the target tablet's end row (empty for the unbounded
	// last tablet), the target tablet's previous end row (empty for the
	// unbounded first tablet), then one encoded split/mergeability payload
	// per new split row. See splits.go and REFERENCES.md for the payload
	// encoding.
	TableSplit
	NamespaceCreate
	NamespaceDelete
	NamespaceRename
)

type FateInstance int

const (
	FateUser FateInstance = iota
	FateMeta
)

type Request struct {
	Operation Operation
	Instance  FateInstance
	Arguments [][]byte
	Options   map[string]string
}

type VersionedProperties struct {
	Version    int64
	Properties map[string]string
}

type ErrorKind int

const (
	ErrorUnknown ErrorKind = iota
	ErrorTableExists
	ErrorTableNotFound
	ErrorNamespaceExists
	ErrorNamespaceNotFound
	ErrorInvalidName
	ErrorInvalidProperty
	ErrorSecurity
	ErrorNotActive
	ErrorTableOffline
	// ErrorConcurrentModification reports that a versioned property write lost
	// its compare-and-set: another writer changed the set first.
	ErrorConcurrentModification
)

type Error struct {
	Kind        ErrorKind
	User        string
	TableID     string
	TableName   string
	Property    string
	Value       string
	Description string
	Code        string
}

// PostResponseCleanupError reports that an RPC received a valid successful
// application response, but releasing its transport failed afterward.
type PostResponseCleanupError struct {
	Err error
}

func (e *PostResponseCleanupError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("managerclient: post-response cleanup: %v", e.Err)
}

func (e *PostResponseCleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	detail := e.Description
	if detail == "" {
		detail = e.Code
	}
	if detail == "" {
		detail = "operation failed"
	}
	return "managerclient: " + detail
}

type Adapter interface {
	Execute(context.Context, string, Request) error
	ExecuteStatus(context.Context, string, Request) (string, error)
	UpdateTabletMergeability(
		context.Context,
		string,
		string,
		[]MergeabilityUpdate,
	) ([]TabletExtent, error)
	FlushTable(context.Context, string, string, []byte, []byte, bool) error
	GetTableConfiguration(context.Context, string, string) (map[string]string, error)
	GetNamespaceConfiguration(context.Context, string, string) (map[string]string, error)
	GetNamespaceProperties(context.Context, string, string) (map[string]string, error)
	GetVersionedNamespaceProperties(context.Context, string, string) (VersionedProperties, error)
	GetVersionedTableProperties(context.Context, string, string) (VersionedProperties, error)
	ModifyTableProperties(context.Context, string, string, VersionedProperties) error
	SetTableProperty(context.Context, string, string, string, string) error
	RemoveTableProperty(context.Context, string, string, string) error
	SetNamespaceProperty(context.Context, string, string, string, string) error
	RemoveNamespaceProperty(context.Context, string, string, string) error
	Close() error
}

// SecurityAdapter is the version-neutral ClientService security RPC boundary.
type SecurityAdapter interface {
	CreateLocalUser(context.Context, string, string, []byte) error
	DropLocalUser(context.Context, string, string) error
	ChangeLocalUserPassword(context.Context, string, string, []byte) error
	ChangeUserAuthorizations(context.Context, string, string, [][]byte) error
	GetUserAuthorizations(context.Context, string, string) ([][]byte, error)
	HasSystemPermission(context.Context, string, string, int8) (bool, error)
	HasTablePermission(context.Context, string, string, string, int8) (bool, error)
	HasNamespacePermission(context.Context, string, string, string, int8) (bool, error)
	GrantSystemPermission(context.Context, string, string, int8) error
	RevokeSystemPermission(context.Context, string, string, int8) error
	GrantTablePermission(context.Context, string, string, string, int8) error
	RevokeTablePermission(context.Context, string, string, string, int8) error
	GrantNamespacePermission(context.Context, string, string, string, int8) error
	RevokeNamespacePermission(context.Context, string, string, string, int8) error
}

// FateID is the durable identity allocated by Accumulo for one FATE
// transaction.
type FateID struct {
	Type int32
	UUID string
}

type fateID = FateID

type fateRPC interface {
	Begin(context.Context, *security.TCredentials, FateInstance) (FateID, error)
	Execute(context.Context, *security.TCredentials, FateID, Request) error
	Wait(context.Context, *security.TCredentials, FateID) (string, error)
	Finish(context.Context, *security.TCredentials, FateID) error
}

// DurableFateAdapter exposes allocation separately from submission so a
// workflow can persist the identity before executing the transaction.
type DurableFateAdapter interface {
	BeginFate(context.Context, string, FateInstance) (FateID, error)
	ExecuteFate(context.Context, string, FateID, Request) error
	WaitFate(context.Context, string, FateID) (string, error)
	FinishFate(context.Context, string, FateID) error
}

type managerRPC interface {
	InitiateFlush(context.Context, *security.TCredentials, string) (int64, error)
	WaitForFlush(context.Context, *security.TCredentials, string, []byte, []byte, int64, int64) error
	SetTableProperty(context.Context, *security.TCredentials, string, string, string) error
	ModifyTableProperties(context.Context, *security.TCredentials, string, VersionedProperties) error
	RemoveTableProperty(context.Context, *security.TCredentials, string, string) error
	UpdateTabletMergeability(
		context.Context,
		*security.TCredentials,
		string,
		[]MergeabilityUpdate,
	) ([]TabletExtent, error)
	SetNamespaceProperty(context.Context, *security.TCredentials, string, string, string) error
	RemoveNamespaceProperty(context.Context, *security.TCredentials, string, string) error
}

type clientRPC interface {
	GetTableConfiguration(
		context.Context,
		*security.TCredentials,
		string,
	) (map[string]string, error)
	GetNamespaceConfiguration(
		context.Context,
		*security.TCredentials,
		string,
	) (map[string]string, error)
	GetNamespaceProperties(
		context.Context,
		*security.TCredentials,
		string,
	) (map[string]string, error)
	GetVersionedNamespaceProperties(
		context.Context,
		*security.TCredentials,
		string,
	) (VersionedProperties, error)
	GetVersionedTableProperties(
		context.Context,
		*security.TCredentials,
		string,
	) (VersionedProperties, error)
	CreateLocalUser(context.Context, *security.TCredentials, string, []byte) error
	DropLocalUser(context.Context, *security.TCredentials, string) error
	ChangeLocalUserPassword(context.Context, *security.TCredentials, string, []byte) error
	ChangeUserAuthorizations(context.Context, *security.TCredentials, string, [][]byte) error
	GetUserAuthorizations(context.Context, *security.TCredentials, string) ([][]byte, error)
	HasSystemPermission(context.Context, *security.TCredentials, string, int8) (bool, error)
	HasTablePermission(context.Context, *security.TCredentials, string, string, int8) (bool, error)
	HasNamespacePermission(context.Context, *security.TCredentials, string, string, int8) (bool, error)
	GrantSystemPermission(context.Context, *security.TCredentials, string, int8) error
	RevokeSystemPermission(context.Context, *security.TCredentials, string, int8) error
	GrantTablePermission(context.Context, *security.TCredentials, string, string, int8) error
	RevokeTablePermission(context.Context, *security.TCredentials, string, string, int8) error
	GrantNamespacePermission(context.Context, *security.TCredentials, string, string, int8) error
	RevokeNamespacePermission(context.Context, *security.TCredentials, string, string, int8) error
}

type Pooled struct {
	pool            *transportpool.Pool
	instanceID      string
	accumuloVersion string
	dialTimeout     time.Duration
	finishTimeout   time.Duration

	mu          sync.RWMutex
	credentials *security.TCredentials
	closed      bool

	dial             transportpool.DialFunc
	newClient        func(io.Closer) (fateRPC, error)
	newManagerClient func(io.Closer) (managerRPC, error)
	newServiceClient func(io.Closer) (clientRPC, error)
}

var _ Adapter = (*Pooled)(nil)
var _ SecurityAdapter = (*Pooled)(nil)

func NewPooled(
	pool *transportpool.Pool,
	instanceID, accumuloVersion string,
	credentials *security.TCredentials,
	dialTimeout time.Duration,
) (*Pooled, error) {
	switch {
	case pool == nil:
		return nil, errors.New("managerclient: nil transport pool")
	case instanceID == "":
		return nil, errors.New("managerclient: empty instanceID")
	case accumuloVersion == "":
		return nil, errors.New("managerclient: empty accumuloVersion")
	case credentials == nil:
		return nil, errors.New("managerclient: nil Credentials")
	case dialTimeout < 0:
		return nil, errors.New("managerclient: negative dial timeout")
	}
	p := &Pooled{
		pool:            pool,
		instanceID:      instanceID,
		accumuloVersion: accumuloVersion,
		dialTimeout:     dialTimeout,
		finishTimeout:   fateFinishTimeout,
		credentials:     cloneCredentials(credentials),
	}
	p.dial = p.dialThrift
	p.newClient = p.newThriftRPC
	p.newManagerClient = p.newThriftManagerRPC
	p.newServiceClient = p.newThriftClientRPC
	return p, nil
}

func (p *Pooled) Execute(ctx context.Context, address string, req Request) error {
	_, err := p.ExecuteStatus(ctx, address, req)
	return err
}

func (p *Pooled) BeginFate(ctx context.Context, address string, instance FateInstance) (FateID, error) {
	if instance != FateUser && instance != FateMeta {
		return FateID{}, fmt.Errorf("managerclient: unknown FATE instance %d", instance)
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return FateID{}, err
	}
	id, err := withClient(p, ctx, address, func(rpc fateRPC) (FateID, error) {
		return rpc.Begin(ctx, credentials, instance)
	})
	if isPostResponseCleanup(err) {
		err = nil
	}
	return id, mapRPCError(err)
}

func (p *Pooled) ExecuteFate(ctx context.Context, address string, id FateID, req Request) error {
	if id.UUID == "" {
		return errors.New("managerclient: empty FATE ID")
	}
	if err := validateRequest(req); err != nil {
		return err
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withClient(p, ctx, address, func(rpc fateRPC) (struct{}, error) {
		return struct{}{}, rpc.Execute(ctx, credentials, id, req)
	})
	if isPostResponseCleanup(err) {
		err = nil
	}
	return mapRPCError(err)
}

func (p *Pooled) WaitFate(ctx context.Context, address string, id FateID) (string, error) {
	if id.UUID == "" {
		return "", errors.New("managerclient: empty FATE ID")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return "", err
	}
	status, err := withClient(p, ctx, address, func(rpc fateRPC) (string, error) {
		return rpc.Wait(ctx, credentials, id)
	})
	if isPostResponseCleanup(err) {
		err = nil
	}
	return status, mapRPCError(err)
}

func (p *Pooled) FinishFate(ctx context.Context, address string, id FateID) error {
	if id.UUID == "" {
		return errors.New("managerclient: empty FATE ID")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withClient(p, ctx, address, func(rpc fateRPC) (struct{}, error) {
		return struct{}{}, rpc.Finish(ctx, credentials, id)
	})
	if isPostResponseCleanup(err) {
		err = nil
	}
	return mapRPCError(err)
}

func isPostResponseCleanup(err error) bool {
	var cleanupErr *PostResponseCleanupError
	return errors.As(err, &cleanupErr)
}

// ExecuteStatus runs a FATE operation and returns the status string the
// manager reported from waitForFateOperation. Accumulo uses that string to
// distinguish "the operation ran" from "the operation exited without work"
// — TABLE_SPLIT returns SplitSucceeded only when the requested tablet was
// actually split. The FATE transaction is always finished, even when the
// call fails or ctx is already done, and any cleanup error is joined onto
// the returned error.
func (p *Pooled) ExecuteStatus(
	ctx context.Context,
	address string,
	req Request,
) (status string, err error) {
	if err := validateRequest(req); err != nil {
		return "", err
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return "", err
	}
	id, err := withClient(p, ctx, address, func(rpc fateRPC) (FateID, error) {
		return rpc.Begin(ctx, credentials, req.Instance)
	})
	if id.UUID != "" {
		defer func() {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.finishTimeout)
			defer cancel()
			_, finishErr := withClient(p, finishCtx, address, func(rpc fateRPC) (struct{}, error) {
				return struct{}{}, rpc.Finish(finishCtx, credentials, id)
			})
			err = errors.Join(err, mapRPCError(finishErr))
		}()
	}
	if err != nil {
		return "", mapRPCError(err)
	}

	var executeCleanupErr error
	if _, err := withClient(p, ctx, address, func(rpc fateRPC) (struct{}, error) {
		return struct{}{}, rpc.Execute(ctx, credentials, id, req)
	}); err != nil {
		var cleanupErr *PostResponseCleanupError
		if errors.As(err, &cleanupErr) {
			executeCleanupErr = cleanupErr.Err
		} else {
			return "", mapRPCError(err)
		}
	}
	status, err = withClient(p, ctx, address, func(rpc fateRPC) (string, error) {
		return rpc.Wait(ctx, credentials, id)
	})
	if err != nil {
		var cleanupErr *PostResponseCleanupError
		if errors.As(err, &cleanupErr) {
			return status, errors.Join(executeCleanupErr, cleanupErr.Err)
		}
		return status, errors.Join(executeCleanupErr, mapRPCError(err))
	}
	return status, executeCleanupErr
}

func (p *Pooled) SetTableProperty(
	ctx context.Context,
	address, tableName, property, value string,
) error {
	if err := validatePropertyRequest(tableName, property); err != nil {
		return err
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withManagerClient(p, ctx, address, func(rpc managerRPC) (struct{}, error) {
		return struct{}{}, rpc.SetTableProperty(ctx, credentials, tableName, property, value)
	})
	return mapRPCError(err)
}

// FlushTable initiates a flush and then waits for it. startRow and endRow
// bound the flush to the tablets that hold rows between them; nil on either
// side is unbounded, which is the whole-table flush Accumulo performs when the
// row fields are absent.
func (p *Pooled) FlushTable(
	ctx context.Context,
	address, tableID string,
	startRow, endRow []byte,
	wait bool,
) error {
	if tableID == "" {
		return errors.New("managerclient: empty table ID")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	flushID, err := withManagerClient(p, ctx, address, func(rpc managerRPC) (int64, error) {
		return rpc.InitiateFlush(ctx, credentials, tableID)
	})
	if err != nil {
		return mapRPCError(err)
	}
	maxLoops := noWaitFlushMaxLoops
	if wait {
		maxLoops = waitForFlushMaxLoops
	}
	start, end := cloneRow(startRow), cloneRow(endRow)
	_, err = withManagerClient(p, ctx, address, func(rpc managerRPC) (struct{}, error) {
		return struct{}{}, rpc.WaitForFlush(ctx, credentials, tableID, start, end, flushID, maxLoops)
	})
	return mapRPCError(err)
}

// GetVersionedTableProperties reads a table's own properties together with the
// version a compare-and-set write must present.
func (p *Pooled) GetVersionedTableProperties(
	ctx context.Context,
	address, tableName string,
) (VersionedProperties, error) {
	if tableName == "" {
		return VersionedProperties{}, errors.New("managerclient: empty table name")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return VersionedProperties{}, err
	}
	properties, err := withClientService(p, ctx, address, func(rpc clientRPC) (VersionedProperties, error) {
		return rpc.GetVersionedTableProperties(ctx, credentials, tableName)
	})
	if err != nil {
		return VersionedProperties{}, mapRPCError(err)
	}
	return properties, nil
}

// ModifyTableProperties replaces a table's own properties, but only while the
// stored version still matches the one read. A losing writer gets
// ErrorConcurrentModification rather than silently overwriting.
func (p *Pooled) ModifyTableProperties(
	ctx context.Context,
	address, tableName string,
	properties VersionedProperties,
) error {
	if tableName == "" {
		return errors.New("managerclient: empty table name")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withManagerClient(p, ctx, address, func(rpc managerRPC) (struct{}, error) {
		return struct{}{}, rpc.ModifyTableProperties(ctx, credentials, tableName, properties)
	})
	return mapRPCError(err)
}

func (p *Pooled) GetTableConfiguration(
	ctx context.Context,
	address, tableName string,
) (map[string]string, error) {
	if tableName == "" {
		return nil, errors.New("managerclient: empty table name")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	properties, err := withClientService(p, ctx, address, func(rpc clientRPC) (map[string]string, error) {
		return rpc.GetTableConfiguration(ctx, credentials, tableName)
	})
	if err != nil {
		return nil, mapRPCError(err)
	}
	return cloneOptions(properties), nil
}

func (p *Pooled) GetNamespaceConfiguration(
	ctx context.Context,
	address, namespace string,
) (map[string]string, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	properties, err := withClientService(p, ctx, address, func(rpc clientRPC) (map[string]string, error) {
		return rpc.GetNamespaceConfiguration(ctx, credentials, namespace)
	})
	if err != nil {
		return nil, mapRPCError(err)
	}
	return cloneOptions(properties), nil
}

func (p *Pooled) GetNamespaceProperties(
	ctx context.Context,
	address, namespace string,
) (map[string]string, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	properties, err := withClientService(p, ctx, address, func(rpc clientRPC) (map[string]string, error) {
		return rpc.GetNamespaceProperties(ctx, credentials, namespace)
	})
	if err != nil {
		return nil, mapRPCError(err)
	}
	return cloneOptions(properties), nil
}

func (p *Pooled) GetVersionedNamespaceProperties(
	ctx context.Context,
	address, namespace string,
) (VersionedProperties, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return VersionedProperties{}, err
	}
	properties, err := withClientService(p, ctx, address, func(rpc clientRPC) (VersionedProperties, error) {
		return rpc.GetVersionedNamespaceProperties(ctx, credentials, namespace)
	})
	if err != nil {
		return VersionedProperties{}, mapRPCError(err)
	}
	properties.Properties = cloneOptions(properties.Properties)
	return properties, nil
}

func (p *Pooled) CreateLocalUser(ctx context.Context, address, principal string, password []byte) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.CreateLocalUser(ctx, credentials, principal, append([]byte(nil), password...))
	})
}

func (p *Pooled) DropLocalUser(ctx context.Context, address, principal string) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.DropLocalUser(ctx, credentials, principal)
	})
}

func (p *Pooled) ChangeLocalUserPassword(
	ctx context.Context,
	address, principal string,
	password []byte,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.ChangeLocalUserPassword(ctx, credentials, principal, append([]byte(nil), password...))
	})
}

func (p *Pooled) ChangeUserAuthorizations(
	ctx context.Context,
	address, principal string,
	authorizations [][]byte,
) error {
	authCopy := cloneArguments(authorizations)
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.ChangeUserAuthorizations(ctx, credentials, principal, authCopy)
	})
}

func (p *Pooled) GetUserAuthorizations(
	ctx context.Context,
	address, principal string,
) ([][]byte, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	auths, err := withClientService(p, ctx, address, func(rpc clientRPC) ([][]byte, error) {
		return rpc.GetUserAuthorizations(ctx, credentials, principal)
	})
	if err != nil {
		return nil, mapRPCError(err)
	}
	return cloneArguments(auths), nil
}

func (p *Pooled) HasSystemPermission(
	ctx context.Context,
	address, principal string,
	permission int8,
) (bool, error) {
	return p.securityBool(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) (bool, error) {
		return rpc.HasSystemPermission(ctx, credentials, principal, permission)
	})
}

func (p *Pooled) HasTablePermission(
	ctx context.Context,
	address, principal, tableName string,
	permission int8,
) (bool, error) {
	return p.securityBool(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) (bool, error) {
		return rpc.HasTablePermission(ctx, credentials, principal, tableName, permission)
	})
}

func (p *Pooled) HasNamespacePermission(
	ctx context.Context,
	address, principal, namespace string,
	permission int8,
) (bool, error) {
	return p.securityBool(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) (bool, error) {
		return rpc.HasNamespacePermission(ctx, credentials, principal, namespace, permission)
	})
}

func (p *Pooled) GrantSystemPermission(
	ctx context.Context,
	address, principal string,
	permission int8,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.GrantSystemPermission(ctx, credentials, principal, permission)
	})
}

func (p *Pooled) RevokeSystemPermission(
	ctx context.Context,
	address, principal string,
	permission int8,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.RevokeSystemPermission(ctx, credentials, principal, permission)
	})
}

func (p *Pooled) GrantTablePermission(
	ctx context.Context,
	address, principal, tableName string,
	permission int8,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.GrantTablePermission(ctx, credentials, principal, tableName, permission)
	})
}

func (p *Pooled) RevokeTablePermission(
	ctx context.Context,
	address, principal, tableName string,
	permission int8,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.RevokeTablePermission(ctx, credentials, principal, tableName, permission)
	})
}

func (p *Pooled) GrantNamespacePermission(
	ctx context.Context,
	address, principal, namespace string,
	permission int8,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.GrantNamespacePermission(ctx, credentials, principal, namespace, permission)
	})
}

func (p *Pooled) RevokeNamespacePermission(
	ctx context.Context,
	address, principal, namespace string,
	permission int8,
) error {
	return p.securityVoid(ctx, address, func(rpc clientRPC, credentials *security.TCredentials) error {
		return rpc.RevokeNamespacePermission(ctx, credentials, principal, namespace, permission)
	})
}

func (p *Pooled) securityVoid(
	ctx context.Context,
	address string,
	call func(clientRPC, *security.TCredentials) error,
) error {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withClientService(p, ctx, address, func(rpc clientRPC) (struct{}, error) {
		return struct{}{}, call(rpc, credentials)
	})
	return mapRPCError(err)
}

func (p *Pooled) securityBool(
	ctx context.Context,
	address string,
	call func(clientRPC, *security.TCredentials) (bool, error),
) (bool, error) {
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return false, err
	}
	result, err := withClientService(p, ctx, address, func(rpc clientRPC) (bool, error) {
		return call(rpc, credentials)
	})
	return result, mapRPCError(err)
}

func (p *Pooled) RemoveTableProperty(
	ctx context.Context,
	address, tableName, property string,
) error {
	if err := validatePropertyRequest(tableName, property); err != nil {
		return err
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withManagerClient(p, ctx, address, func(rpc managerRPC) (struct{}, error) {
		return struct{}{}, rpc.RemoveTableProperty(ctx, credentials, tableName, property)
	})
	return mapRPCError(err)
}

func (p *Pooled) SetNamespaceProperty(
	ctx context.Context,
	address, namespace, property, value string,
) error {
	if property == "" {
		return errors.New("managerclient: empty property")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withManagerClient(p, ctx, address, func(rpc managerRPC) (struct{}, error) {
		return struct{}{}, rpc.SetNamespaceProperty(ctx, credentials, namespace, property, value)
	})
	return mapRPCError(err)
}

func (p *Pooled) RemoveNamespaceProperty(
	ctx context.Context,
	address, namespace, property string,
) error {
	if property == "" {
		return errors.New("managerclient: empty property")
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return err
	}
	_, err = withManagerClient(p, ctx, address, func(rpc managerRPC) (struct{}, error) {
		return struct{}{}, rpc.RemoveNamespaceProperty(ctx, credentials, namespace, property)
	})
	return mapRPCError(err)
}

func withClient[T any](
	p *Pooled,
	ctx context.Context,
	address string,
	call func(fateRPC) (T, error),
) (result T, err error) {
	return withServiceClient(p, ctx, address, fateServiceName, p.newClient, call)
}

func withManagerClient[T any](
	p *Pooled,
	ctx context.Context,
	address string,
	call func(managerRPC) (T, error),
) (result T, err error) {
	return withServiceClient(p, ctx, address, managerServiceName, p.newManagerClient, call)
}

func withClientService[T any](
	p *Pooled,
	ctx context.Context,
	address string,
	call func(clientRPC) (T, error),
) (result T, err error) {
	return withServiceClient(p, ctx, address, clientServiceName, p.newServiceClient, call)
}

func withServiceClient[T any, C any](
	p *Pooled,
	ctx context.Context,
	address, service string,
	newClient func(io.Closer) (C, error),
	call func(C) (T, error),
) (result T, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	key := transportpool.Key{
		Address:         address,
		Service:         service,
		InstanceID:      p.instanceID,
		ProtocolVersion: p.accumuloVersion,
	}
	lease, err := p.pool.Acquire(ctx, key, p.dial)
	if err != nil {
		return result, err
	}
	client, err := newClient(lease.Transport())
	if err != nil {
		return result, errors.Join(err, lease.Invalidate())
	}
	if err := ctx.Err(); err != nil {
		return result, errors.Join(err, lease.Close())
	}
	result, rpcErr := call(client)
	var cleanupErr error
	if shouldInvalidateTransport(rpcErr) {
		cleanupErr = lease.Invalidate()
	} else {
		cleanupErr = lease.Close()
	}
	if rpcErr == nil && cleanupErr != nil {
		return result, &PostResponseCleanupError{Err: cleanupErr}
	}
	return result, errors.Join(rpcErr, cleanupErr)
}

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
		return errors.New("managerclient: nil Credentials")
	}
	replacement := cloneCredentials(credentials)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for i := range replacement.Token {
			replacement.Token[i] = 0
		}
		return errors.New("managerclient: pooled client is closed")
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

func (p *Pooled) credentialsForRPC() (*security.TCredentials, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errors.New("managerclient: pooled client is closed")
	}
	return cloneCredentials(p.credentials), nil
}

func (p *Pooled) dialThrift(ctx context.Context, key transportpool.Key) (io.Closer, error) {
	dialer := &net.Dialer{Timeout: p.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", key.Address)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	conf := &thrift.TConfiguration{}
	socket := thrift.NewTSocketFromConnConf(conn, conf)
	return thrift.NewTFramedTransportConf(socket, conf), nil
}

func (p *Pooled) newThriftRPC(transport io.Closer) (fateRPC, error) {
	thriftTransport, ok := transport.(thrift.TTransport)
	if !ok {
		return nil, fmt.Errorf("managerclient: pooled transport %T is not a thrift transport", transport)
	}
	proto := protocol.NewClientFactory(p.instanceID, p.accumuloVersion).GetProtocol(thriftTransport)
	muxed := thrift.NewTMultiplexedProtocol(proto, fateServiceName)
	raw := manager.NewFateServiceClient(thrift.NewTStandardClient(muxed, muxed))
	return thriftFateRPC{raw: raw}, nil
}

func (p *Pooled) newThriftManagerRPC(transport io.Closer) (managerRPC, error) {
	thriftTransport, ok := transport.(thrift.TTransport)
	if !ok {
		return nil, fmt.Errorf("managerclient: pooled transport %T is not a thrift transport", transport)
	}
	proto := protocol.NewClientFactory(p.instanceID, p.accumuloVersion).GetProtocol(thriftTransport)
	muxed := thrift.NewTMultiplexedProtocol(proto, managerServiceName)
	raw := manager.NewManagerClientServiceClient(thrift.NewTStandardClient(muxed, muxed))
	return thriftManagerRPC{raw: raw}, nil
}

func (p *Pooled) newThriftClientRPC(transport io.Closer) (clientRPC, error) {
	thriftTransport, ok := transport.(thrift.TTransport)
	if !ok {
		return nil, fmt.Errorf("managerclient: pooled transport %T is not a thrift transport", transport)
	}
	proto := protocol.NewClientFactory(p.instanceID, p.accumuloVersion).GetProtocol(thriftTransport)
	muxed := thrift.NewTMultiplexedProtocol(proto, clientServiceName)
	raw := clientgen.NewClientServiceClient(thrift.NewTStandardClient(muxed, muxed))
	return thriftClientRPC{raw: raw}, nil
}

type thriftFateRPC struct {
	raw *manager.FateServiceClient
}

func (r thriftFateRPC) Begin(
	ctx context.Context,
	credentials *security.TCredentials,
	instance FateInstance,
) (FateID, error) {
	id, err := r.raw.BeginFateOperation(
		ctx,
		&clientgen.TInfo{},
		credentials,
		thriftFateInstance(instance),
	)
	if err != nil {
		return FateID{}, err
	}
	if id == nil {
		return FateID{}, errors.New("managerclient: begin returned nil FATE ID")
	}
	return FateID{Type: int32(id.Type), UUID: id.TxUUIDStr}, nil
}

type thriftManagerRPC struct {
	raw *manager.ManagerClientServiceClient
}

type thriftClientRPC struct {
	raw *clientgen.ClientServiceClient
}

func (r thriftClientRPC) GetTableConfiguration(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName string,
) (map[string]string, error) {
	return r.raw.GetTableConfiguration(
		ctx,
		clientgen.NewTInfo(),
		credentials,
		tableName,
	)
}

func (r thriftClientRPC) GetNamespaceConfiguration(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace string,
) (map[string]string, error) {
	return r.raw.GetNamespaceConfiguration(ctx, clientgen.NewTInfo(), credentials, namespace)
}

func (r thriftClientRPC) GetNamespaceProperties(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace string,
) (map[string]string, error) {
	return r.raw.GetNamespaceProperties(ctx, clientgen.NewTInfo(), credentials, namespace)
}

func (r thriftClientRPC) GetVersionedNamespaceProperties(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace string,
) (VersionedProperties, error) {
	properties, err := r.raw.GetVersionedNamespaceProperties(
		ctx,
		clientgen.NewTInfo(),
		credentials,
		namespace,
	)
	if err != nil {
		return VersionedProperties{}, err
	}
	if properties == nil {
		return VersionedProperties{}, errors.New("managerclient: versioned namespace properties returned nil")
	}
	return VersionedProperties{
		Version:    properties.Version,
		Properties: cloneOptions(properties.Properties),
	}, nil
}

func (r thriftClientRPC) GetVersionedTableProperties(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName string,
) (VersionedProperties, error) {
	properties, err := r.raw.GetVersionedTableProperties(
		ctx,
		clientgen.NewTInfo(),
		credentials,
		tableName,
	)
	if err != nil {
		return VersionedProperties{}, err
	}
	if properties == nil {
		return VersionedProperties{}, errors.New("managerclient: versioned table properties returned nil")
	}
	return VersionedProperties{
		Version:    properties.Version,
		Properties: cloneOptions(properties.Properties),
	}, nil
}

func (r thriftClientRPC) CreateLocalUser(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
	password []byte,
) error {
	return r.raw.CreateLocalUser(ctx, clientgen.NewTInfo(), credentials, principal, password)
}

func (r thriftClientRPC) DropLocalUser(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
) error {
	return r.raw.DropLocalUser(ctx, clientgen.NewTInfo(), credentials, principal)
}

func (r thriftClientRPC) ChangeLocalUserPassword(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
	password []byte,
) error {
	return r.raw.ChangeLocalUserPassword(ctx, clientgen.NewTInfo(), credentials, principal, password)
}

func (r thriftClientRPC) ChangeUserAuthorizations(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
	authorizations [][]byte,
) error {
	return r.raw.ChangeAuthorizations(
		ctx,
		clientgen.NewTInfo(),
		credentials,
		principal,
		cloneArguments(authorizations),
	)
}

func (r thriftClientRPC) GetUserAuthorizations(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
) ([][]byte, error) {
	return r.raw.GetUserAuthorizations(ctx, clientgen.NewTInfo(), credentials, principal)
}

func (r thriftClientRPC) HasSystemPermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
	permission int8,
) (bool, error) {
	return r.raw.HasSystemPermission(ctx, clientgen.NewTInfo(), credentials, principal, permission)
}

func (r thriftClientRPC) HasTablePermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal, tableName string,
	permission int8,
) (bool, error) {
	return r.raw.HasTablePermission(
		ctx, clientgen.NewTInfo(), credentials, principal, tableName, permission,
	)
}

func (r thriftClientRPC) HasNamespacePermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal, namespace string,
	permission int8,
) (bool, error) {
	return r.raw.HasNamespacePermission(
		ctx, clientgen.NewTInfo(), credentials, principal, namespace, permission,
	)
}

func (r thriftClientRPC) GrantSystemPermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
	permission int8,
) error {
	return r.raw.GrantSystemPermission(
		ctx, clientgen.NewTInfo(), credentials, principal, permission,
	)
}

func (r thriftClientRPC) RevokeSystemPermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal string,
	permission int8,
) error {
	return r.raw.RevokeSystemPermission(
		ctx, clientgen.NewTInfo(), credentials, principal, permission,
	)
}

func (r thriftClientRPC) GrantTablePermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal, tableName string,
	permission int8,
) error {
	return r.raw.GrantTablePermission(
		ctx, clientgen.NewTInfo(), credentials, principal, tableName, permission,
	)
}

func (r thriftClientRPC) RevokeTablePermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal, tableName string,
	permission int8,
) error {
	return r.raw.RevokeTablePermission(
		ctx, clientgen.NewTInfo(), credentials, principal, tableName, permission,
	)
}

func (r thriftClientRPC) GrantNamespacePermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal, namespace string,
	permission int8,
) error {
	return r.raw.GrantNamespacePermission(
		ctx, clientgen.NewTInfo(), credentials, principal, namespace, permission,
	)
}

func (r thriftClientRPC) RevokeNamespacePermission(
	ctx context.Context,
	credentials *security.TCredentials,
	principal, namespace string,
	permission int8,
) error {
	return r.raw.RevokeNamespacePermission(
		ctx, clientgen.NewTInfo(), credentials, principal, namespace, permission,
	)
}

func (r thriftManagerRPC) SetTableProperty(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName, property, value string,
) error {
	return r.raw.SetTableProperty(
		ctx,
		&clientgen.TInfo{},
		credentials,
		tableName,
		property,
		value,
	)
}

func (r thriftManagerRPC) InitiateFlush(
	ctx context.Context,
	credentials *security.TCredentials,
	tableID string,
) (int64, error) {
	return r.raw.InitiateFlush(ctx, &clientgen.TInfo{}, credentials, tableID)
}

func (r thriftManagerRPC) WaitForFlush(
	ctx context.Context,
	credentials *security.TCredentials,
	tableID string,
	startRow, endRow []byte,
	flushID, maxLoops int64,
) error {
	// Accumulo uses absent row fields for an unbounded full-table flush, and
	// bounds the flush to the tablets covering [startRow, endRow] otherwise.
	return r.raw.WaitForFlush(
		ctx,
		&clientgen.TInfo{},
		credentials,
		tableID,
		startRow,
		endRow,
		flushID,
		maxLoops,
	)
}

func (r thriftManagerRPC) ModifyTableProperties(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName string,
	properties VersionedProperties,
) error {
	return r.raw.ModifyTableProperties(
		ctx,
		&clientgen.TInfo{},
		credentials,
		tableName,
		&clientgen.TVersionedProperties{
			Version:    properties.Version,
			Properties: cloneOptions(properties.Properties),
		},
	)
}

func (r thriftManagerRPC) RemoveTableProperty(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName, property string,
) error {
	return r.raw.RemoveTableProperty(
		ctx,
		&clientgen.TInfo{},
		credentials,
		tableName,
		property,
	)
}

func (r thriftManagerRPC) SetNamespaceProperty(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace, property, value string,
) error {
	return r.raw.SetNamespaceProperty(
		ctx,
		&clientgen.TInfo{},
		credentials,
		namespace,
		property,
		value,
	)
}

func (r thriftManagerRPC) RemoveNamespaceProperty(
	ctx context.Context,
	credentials *security.TCredentials,
	namespace, property string,
) error {
	return r.raw.RemoveNamespaceProperty(
		ctx,
		&clientgen.TInfo{},
		credentials,
		namespace,
		property,
	)
}

func thriftFateInstance(instance FateInstance) manager.TFateInstanceType {
	switch instance {
	case FateUser:
		return manager.TFateInstanceType_USER
	case FateMeta:
		return manager.TFateInstanceType_META
	default:
		panic("managerclient: validated unknown FATE instance")
	}
}

func (r thriftFateRPC) Execute(
	ctx context.Context,
	credentials *security.TCredentials,
	id FateID,
	req Request,
) error {
	return r.raw.ExecuteFateOperation(
		ctx,
		&clientgen.TInfo{},
		credentials,
		thriftFateID(id),
		thriftOperation(req.Operation),
		cloneArguments(req.Arguments),
		cloneOptions(req.Options),
		false,
	)
}

func (r thriftFateRPC) Wait(
	ctx context.Context,
	credentials *security.TCredentials,
	id FateID,
) (string, error) {
	return r.raw.WaitForFateOperation(ctx, &clientgen.TInfo{}, credentials, thriftFateID(id))
}

func (r thriftFateRPC) Finish(
	ctx context.Context,
	credentials *security.TCredentials,
	id FateID,
) error {
	return r.raw.FinishFateOperation(ctx, &clientgen.TInfo{}, credentials, thriftFateID(id))
}

func thriftFateID(id FateID) *manager.TFateId {
	return &manager.TFateId{
		Type:      manager.TFateInstanceType(id.Type),
		TxUUIDStr: id.UUID,
	}
}

func thriftOperation(op Operation) manager.TFateOperation {
	switch op {
	case TableCreate:
		return manager.TFateOperation_TABLE_CREATE
	case TableDelete:
		return manager.TFateOperation_TABLE_DELETE
	case TableRename:
		return manager.TFateOperation_TABLE_RENAME
	case TableBulkImport:
		return manager.TFateOperation_TABLE_BULK_IMPORT2
	case TableSplit:
		return manager.TFateOperation_TABLE_SPLIT
	case NamespaceCreate:
		return manager.TFateOperation_NAMESPACE_CREATE
	case NamespaceDelete:
		return manager.TFateOperation_NAMESPACE_DELETE
	case NamespaceRename:
		return manager.TFateOperation_NAMESPACE_RENAME
	default:
		panic("managerclient: validated unknown operation")
	}
}

func validateRequest(req Request) error {
	switch req.Instance {
	case FateUser, FateMeta:
	default:
		return fmt.Errorf("managerclient: unknown FATE instance %d", req.Instance)
	}
	switch req.Operation {
	case TableCreate:
		if len(req.Arguments) != 5 {
			return fmt.Errorf("managerclient: create requires 5 arguments, got %d", len(req.Arguments))
		}
	case TableDelete:
		if len(req.Arguments) != 1 {
			return fmt.Errorf("managerclient: delete requires 1 argument, got %d", len(req.Arguments))
		}
	case TableRename:
		if len(req.Arguments) != 2 {
			return fmt.Errorf("managerclient: rename requires 2 arguments, got %d", len(req.Arguments))
		}
	case TableBulkImport:
		if len(req.Arguments) != 3 {
			return fmt.Errorf("managerclient: bulk import requires 3 arguments, got %d", len(req.Arguments))
		}
	case TableSplit:
		if len(req.Arguments) < splitMinimumArguments {
			return fmt.Errorf(
				"managerclient: split requires at least %d arguments, got %d",
				splitMinimumArguments,
				len(req.Arguments),
			)
		}
	case NamespaceCreate, NamespaceDelete:
		if len(req.Arguments) != 1 {
			return fmt.Errorf("managerclient: namespace operation requires 1 argument, got %d", len(req.Arguments))
		}
	case NamespaceRename:
		if len(req.Arguments) != 2 {
			return fmt.Errorf("managerclient: namespace rename requires 2 arguments, got %d", len(req.Arguments))
		}
	default:
		return fmt.Errorf("managerclient: unknown operation %d", req.Operation)
	}
	for i, argument := range req.Arguments {
		if argument == nil {
			return fmt.Errorf("managerclient: nil argument %d", i)
		}
	}
	return nil
}

func validatePropertyRequest(tableName, property string) error {
	if tableName == "" {
		return errors.New("managerclient: empty table name")
	}
	if property == "" {
		return errors.New("managerclient: empty property")
	}
	return nil
}

func mapRPCError(err error) error {
	if err == nil {
		return nil
	}
	type joinError interface {
		Unwrap() []error
	}
	if joined, ok := err.(joinError); ok {
		mapped := make([]error, 0, len(joined.Unwrap()))
		for _, cause := range joined.Unwrap() {
			if cause == nil {
				continue
			}
			mapped = append(mapped, mapRPCError(cause))
		}
		switch len(mapped) {
		case 0:
			return nil
		case 1:
			return mapped[0]
		default:
			return errors.Join(mapped...)
		}
	}
	var tableErr *clientgen.ThriftTableOperationException
	if errors.As(err, &tableErr) {
		kind := ErrorUnknown
		switch tableErr.Type {
		case clientgen.TableOperationExceptionType_EXISTS:
			kind = ErrorTableExists
		case clientgen.TableOperationExceptionType_NOTFOUND:
			kind = ErrorTableNotFound
		case clientgen.TableOperationExceptionType_NAMESPACE_EXISTS:
			kind = ErrorNamespaceExists
		case clientgen.TableOperationExceptionType_NAMESPACE_NOTFOUND:
			kind = ErrorNamespaceNotFound
		case clientgen.TableOperationExceptionType_INVALID_NAME:
			kind = ErrorInvalidName
		case clientgen.TableOperationExceptionType_OFFLINE:
			kind = ErrorTableOffline
		}
		return &Error{
			Kind:        kind,
			TableID:     tableErr.TableId,
			TableName:   tableErr.TableName,
			Description: tableErr.Description,
			Code:        tableErr.Type.String(),
		}
	}
	var propertyErr *manager.ThriftPropertyException
	if errors.As(err, &propertyErr) {
		return &Error{
			Kind:        ErrorInvalidProperty,
			Property:    propertyErr.Property,
			Value:       propertyErr.Value,
			Description: propertyErr.Description,
			Code:        "INVALID_PROPERTY",
		}
	}
	var securityErr *clientgen.ThriftSecurityException
	if errors.As(err, &securityErr) {
		kind := ErrorSecurity
		if securityErr.Code == clientgen.SecurityErrorCode_TABLE_DOESNT_EXIST {
			kind = ErrorTableNotFound
		} else if securityErr.Code == clientgen.SecurityErrorCode_NAMESPACE_DOESNT_EXIST {
			kind = ErrorNamespaceNotFound
		}
		return &Error{Kind: kind, User: securityErr.User, Code: securityErr.Code.String()}
	}
	var concurrentErr *clientgen.ThriftConcurrentModificationException
	if errors.As(err, &concurrentErr) {
		return &Error{
			Kind:        ErrorConcurrentModification,
			Description: concurrentErr.Description,
			Code:        "CONCURRENT_MODIFICATION",
		}
	}
	var inactiveErr *clientgen.ThriftNotActiveServiceException
	if errors.As(err, &inactiveErr) {
		return &Error{
			Kind:        ErrorNotActive,
			Description: inactiveErr.Description,
			Code:        inactiveErr.Serv,
		}
	}
	return err
}

func cloneCredentials(credentials *security.TCredentials) *security.TCredentials {
	if credentials == nil {
		return nil
	}
	clone := *credentials
	clone.Token = append([]byte(nil), credentials.Token...)
	return &clone
}

func cloneArguments(arguments [][]byte) [][]byte {
	cloned := make([][]byte, len(arguments))
	for i := range arguments {
		cloned[i] = append([]byte(nil), arguments[i]...)
	}
	return cloned
}

func cloneOptions(options map[string]string) map[string]string {
	cloned := make(map[string]string, len(options))
	for key, value := range options {
		cloned[key] = value
	}
	return cloned
}

func shouldInvalidateTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// TTransportException is an interface; errors.As takes its address.
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

// IsRetryableEndpointError reports whether an RPC failed before receiving a
// valid application response and can be retried against another advertised
// ClientService endpoint.
func IsRetryableEndpointError(err error) bool {
	if err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var cleanupErr *PostResponseCleanupError
	if errors.As(err, &cleanupErr) {
		return false
	}
	var managerErr *Error
	if errors.As(err, &managerErr) {
		return false
	}
	var transportErr thrift.TTransportException
	if errors.As(err, &transportErr) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}
