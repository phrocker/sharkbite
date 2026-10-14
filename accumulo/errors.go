package accumulo

import "errors"

var (
	// ErrUnsupportedVersion indicates that the requested Accumulo wire version
	// is not implemented by this client.
	ErrUnsupportedVersion = errors.New("accumulo: unsupported version")

	// ErrDiscoveryUnavailable indicates that the Instance was created without
	// ZooKeeper-backed metadata discovery.
	ErrDiscoveryUnavailable = errors.New("accumulo: discovery unavailable")

	// ErrTableNotFound indicates that no table matches the requested name or ID.
	ErrTableNotFound = errors.New("accumulo: table not found")

	// ErrTableExists indicates that a requested table name is already in use.
	ErrTableExists = errors.New("accumulo: table exists")

	// ErrInvalidTableName indicates that Accumulo rejected a table name.
	ErrInvalidTableName = errors.New("accumulo: invalid table name")

	// ErrInvalidProperty indicates that Accumulo rejected a property name or
	// value.
	ErrInvalidProperty = errors.New("accumulo: invalid property")

	// ErrNamespaceNotFound indicates that no namespace matches the requested
	// name or ID, or that a table operation targeted a namespace that does not
	// exist.
	ErrNamespaceNotFound = errors.New("accumulo: namespace not found")

	// ErrNamespaceExists indicates that a requested namespace name is already
	// in use.
	ErrNamespaceExists = errors.New("accumulo: namespace exists")

	// ErrNamespaceNotEmpty indicates that a namespace still owns one or more
	// tables and cannot be safely deleted.
	ErrNamespaceNotEmpty = errors.New("accumulo: namespace not empty")

	// ErrInvalidNamespaceName indicates that Accumulo rejected a namespace
	// name, including attempts to create, delete, or rename reserved namespaces.
	ErrInvalidNamespaceName = errors.New("accumulo: invalid namespace name")

	// ErrPermissionDenied indicates that the authenticated principal is not
	// authorized for the requested operation.
	ErrPermissionDenied = errors.New("accumulo: permission denied")

	// ErrManagerUnavailable indicates that no active Accumulo Manager address
	// is currently advertised in ZooKeeper.
	ErrManagerUnavailable = errors.New("accumulo: manager unavailable")

	// ErrClientServiceUnavailable indicates that no live server currently
	// advertises the Accumulo ClientService API in ZooKeeper.
	ErrClientServiceUnavailable = errors.New("accumulo: client service unavailable")

	// ErrNoTabletCoversRow indicates malformed or stale metadata with no extent
	// covering the requested row.
	ErrNoTabletCoversRow = errors.New("accumulo: no tablet covers row")

	// ErrTabletNotLocated indicates that the covering tablet has no current
	// tablet-server assignment.
	ErrTabletNotLocated = errors.New("accumulo: tablet has no current location")

	// ErrConnectorClosed indicates an operation attempted on a closed Connector.
	ErrConnectorClosed = errors.New("accumulo: connector is closed")

	// ErrInstanceClosed indicates a live-state topology accessor called after
	// Instance.Close. Close releases the instance permanently, so discovery
	// cannot reconnect; the accessors that report construction-time wiring
	// (Info, ZooKeepers, Root, Configuration) keep working.
	ErrInstanceClosed = errors.New("accumulo: instance is closed")

	// ErrInvalidBulkDir indicates that a bulk-import call was given an empty
	// or otherwise unusable bulk directory path.
	ErrInvalidBulkDir = errors.New("accumulo: invalid bulk directory")

	// ErrInvalidTableSplit indicates that a split-point collection was empty
	// or contained a nil or zero-length row.
	ErrInvalidTableSplit = errors.New("accumulo: invalid table split")

	// ErrInvalidTableRange indicates that a row range is malformed, such as an
	// end row that sorts before its start row.
	ErrInvalidTableRange = errors.New("accumulo: invalid table range")

	// ErrConstraintNumberUnavailable indicates that a constraint number could
	// not be allocated because concurrent writers kept taking the free ones.
	ErrConstraintNumberUnavailable = errors.New("accumulo: constraint number unavailable")

	// ErrTableOffline indicates that Accumulo rejected an operation because
	// the table is not online.
	ErrTableOffline = errors.New("accumulo: table offline")

	// ErrTableSplitsIncomplete indicates that split points remained unapplied
	// after the bounded tablet re-resolution retries were exhausted, which
	// happens when tablets keep moving or splitting underneath the client.
	ErrTableSplitsIncomplete = errors.New("accumulo: table splits incomplete")

	// ErrTableIdentityChanged indicates that AddTableSplitsForTable's own
	// fresh table-name resolution returned a table ID different from the
	// caller's expected/pinned one, meaning the table was deleted and
	// recreated under the same name after the caller resolved that ID. It
	// is returned before any split or mergeability mutation is attempted.
	ErrTableIdentityChanged = errors.New("accumulo: table changed identity")

	// ErrInvalidUser indicates that a user name is empty or otherwise invalid.
	ErrInvalidUser = errors.New("accumulo: invalid user")

	// ErrUserExists indicates that a requested user already exists.
	ErrUserExists = errors.New("accumulo: user exists")

	// ErrUserNotFound indicates that a requested user does not exist.
	ErrUserNotFound = errors.New("accumulo: user not found")

	// ErrInvalidPassword indicates that a nil password was supplied.
	ErrInvalidPassword = errors.New("accumulo: invalid password")

	// ErrBadCredentials indicates rejected, invalid, or expired credentials.
	ErrBadCredentials = errors.New("accumulo: bad credentials")

	// ErrInvalidAuthorizations indicates an invalid authorization set.
	ErrInvalidAuthorizations = errors.New("accumulo: invalid authorizations")

	// ErrInvalidPermission indicates a permission outside Accumulo's wire enum.
	ErrInvalidPermission = errors.New("accumulo: invalid permission")

	// ErrUnsupportedOperation indicates that the server rejected an operation
	// as unsupported.
	ErrUnsupportedOperation = errors.New("accumulo: unsupported operation")

	// ErrSecurityUnavailable indicates a server-side security subsystem failure.
	ErrSecurityUnavailable = errors.New("accumulo: security subsystem unavailable")

	// ErrDeletedRangeBound indicates a range bound that carries a deletion
	// marker. The scan wire's TKey has no field for it, so such a bound would
	// mean one thing locally and another on the server.
	ErrDeletedRangeBound = errors.New("accumulo: range bound carries a deletion marker")
)
