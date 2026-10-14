package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// SystemPermission is an Accumulo system-permission wire ordinal.
type SystemPermission int8

const (
	SystemPermissionGrant                 SystemPermission = 0
	SystemPermissionCreateTable           SystemPermission = 1
	SystemPermissionDropTable             SystemPermission = 2
	SystemPermissionAlterTable            SystemPermission = 3
	SystemPermissionCreateUser            SystemPermission = 4
	SystemPermissionDropUser              SystemPermission = 5
	SystemPermissionAlterUser             SystemPermission = 6
	SystemPermissionSystem                SystemPermission = 7
	SystemPermissionCreateNamespace       SystemPermission = 8
	SystemPermissionDropNamespace         SystemPermission = 9
	SystemPermissionAlterNamespace        SystemPermission = 10
	SystemPermissionObtainDelegationToken SystemPermission = 11
)

// TablePermission is an Accumulo table-permission wire ordinal. Ordinals 0
// and 1 are intentionally invalid legacy gaps.
type TablePermission int8

const (
	TablePermissionRead         TablePermission = 2
	TablePermissionWrite        TablePermission = 3
	TablePermissionBulkImport   TablePermission = 4
	TablePermissionAlterTable   TablePermission = 5
	TablePermissionGrant        TablePermission = 6
	TablePermissionDropTable    TablePermission = 7
	TablePermissionGetSummaries TablePermission = 8
)

// NamespacePermission is an Accumulo namespace-permission wire ordinal.
type NamespacePermission int8

const (
	NamespacePermissionRead           NamespacePermission = 0
	NamespacePermissionWrite          NamespacePermission = 1
	NamespacePermissionAlterNamespace NamespacePermission = 2
	NamespacePermissionGrant          NamespacePermission = 3
	NamespacePermissionAlterTable     NamespacePermission = 4
	NamespacePermissionCreateTable    NamespacePermission = 5
	NamespacePermissionDropTable      NamespacePermission = 6
	NamespacePermissionBulkImport     NamespacePermission = 7
	NamespacePermissionDropNamespace  NamespacePermission = 8
)

// SecurityError describes an Accumulo Thrift security exception without
// exposing generated Thrift types.
type SecurityError struct {
	User string
	Code string
	Err  error
}

func (e *SecurityError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.User == "" {
		return fmt.Sprintf("accumulo: security error %s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("accumulo: security error %s for user %q: %v", e.Code, e.User, e.Err)
}

func (e *SecurityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CreateUser creates a local Accumulo user. The password is copied before
// the method returns. A non-nil empty password is valid.
func (c *Connector) CreateUser(ctx context.Context, user string, password []byte) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if password == nil {
		return ErrInvalidPassword
	}
	passwordCopy := append([]byte(nil), password...)
	return c.securityVoid(ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		return security.CreateLocalUser(ctx, address, user, passwordCopy)
	})
}

// DropUser drops a local Accumulo user.
func (c *Connector) DropUser(ctx context.Context, user string) error {
	if err := validateUser(user); err != nil {
		return err
	}
	return c.securityVoid(ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		return security.DropLocalUser(ctx, address, user)
	})
}

// ChangePassword changes a local user's password. When changing the
// connector principal's own password, all connector-owned RPC adapters are
// refreshed before a successful return. RPCs already in flight may retain
// their previous credential snapshot.
func (c *Connector) ChangePassword(ctx context.Context, user string, password []byte) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if password == nil {
		return ErrInvalidPassword
	}
	changeOwnPassword := c.Principal() == user
	if changeOwnPassword {
		c.passwordMu.Lock()
		defer c.passwordMu.Unlock()
	}
	passwordCopy := append([]byte(nil), password...)
	if err := c.securityVoid(ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		return security.ChangeLocalUserPassword(ctx, address, user, passwordCopy)
	}); err != nil {
		return err
	}
	if !changeOwnPassword {
		return nil
	}
	return c.replacePasswordCredentials(passwordCopy)
}

// ChangeUserAuthorizations replaces a user's complete authorization set.
// The input and every authorization value are copied.
func (c *Connector) ChangeUserAuthorizations(
	ctx context.Context,
	user string,
	authorizations [][]byte,
) error {
	if err := validateUser(user); err != nil {
		return err
	}
	normalized, err := normalizeAuthorizations(authorizations)
	if err != nil {
		return err
	}
	return c.securityVoid(ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		return security.ChangeUserAuthorizations(ctx, address, user, normalized)
	})
}

// GetUserAuthorizations returns a sorted, duplicate-free deep copy of the
// user's authorizations.
func (c *Connector) GetUserAuthorizations(ctx context.Context, user string) ([][]byte, error) {
	if err := validateUser(user); err != nil {
		return nil, err
	}
	auths, err := securityCall(c, ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) ([][]byte, error) {
		return security.GetUserAuthorizations(ctx, address, user)
	})
	if err != nil {
		return nil, err
	}
	return normalizeAuthorizations(auths)
}

// HasSystemPermission reports whether a user has a system permission.
func (c *Connector) HasSystemPermission(
	ctx context.Context,
	user string,
	permission SystemPermission,
) (bool, error) {
	if err := validateUser(user); err != nil {
		return false, err
	}
	if !permission.valid() {
		return false, invalidPermission("system", int8(permission))
	}
	return securityCall(c, ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) (bool, error) {
		return security.HasSystemPermission(ctx, address, user, int8(permission))
	})
}

// HasTablePermission reports whether a user has a permission on a table.
func (c *Connector) HasTablePermission(
	ctx context.Context,
	user, tableName string,
	permission TablePermission,
) (bool, error) {
	if err := validateUser(user); err != nil {
		return false, err
	}
	if err := validateExistingTableName(tableName); err != nil {
		return false, err
	}
	if !permission.valid() {
		return false, invalidPermission("table", int8(permission))
	}
	return securityCall(c, ctx, securityTargetTable, func(
		security managerclient.SecurityAdapter,
		address string,
	) (bool, error) {
		return security.HasTablePermission(ctx, address, user, tableName, int8(permission))
	})
}

// HasNamespacePermission reports whether a user has a permission on a namespace.
func (c *Connector) HasNamespacePermission(
	ctx context.Context,
	user, namespace string,
	permission NamespacePermission,
) (bool, error) {
	if err := validateNamespacePermissionRequest(user, namespace, permission); err != nil {
		return false, err
	}
	return securityCall(c, ctx, securityTargetNamespace, func(
		security managerclient.SecurityAdapter,
		address string,
	) (bool, error) {
		return security.HasNamespacePermission(ctx, address, user, namespace, int8(permission))
	})
}

// GrantSystemPermission grants a system permission to a user.
func (c *Connector) GrantSystemPermission(
	ctx context.Context,
	user string,
	permission SystemPermission,
) error {
	return c.changeSystemPermission(ctx, user, permission, true)
}

// RevokeSystemPermission revokes a system permission from a user.
func (c *Connector) RevokeSystemPermission(
	ctx context.Context,
	user string,
	permission SystemPermission,
) error {
	return c.changeSystemPermission(ctx, user, permission, false)
}

// GrantTablePermission grants a table permission to a user.
func (c *Connector) GrantTablePermission(
	ctx context.Context,
	user, tableName string,
	permission TablePermission,
) error {
	return c.changeTablePermission(ctx, user, tableName, permission, true)
}

// RevokeTablePermission revokes a table permission from a user.
func (c *Connector) RevokeTablePermission(
	ctx context.Context,
	user, tableName string,
	permission TablePermission,
) error {
	return c.changeTablePermission(ctx, user, tableName, permission, false)
}

// GrantNamespacePermission grants a namespace permission to a user.
func (c *Connector) GrantNamespacePermission(
	ctx context.Context,
	user, namespace string,
	permission NamespacePermission,
) error {
	return c.changeNamespacePermission(ctx, user, namespace, permission, true)
}

// RevokeNamespacePermission revokes a namespace permission from a user.
func (c *Connector) RevokeNamespacePermission(
	ctx context.Context,
	user, namespace string,
	permission NamespacePermission,
) error {
	return c.changeNamespacePermission(ctx, user, namespace, permission, false)
}

func (c *Connector) changeSystemPermission(
	ctx context.Context,
	user string,
	permission SystemPermission,
	grant bool,
) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if !permission.valid() {
		return invalidPermission("system", int8(permission))
	}
	return c.securityVoid(ctx, securityTargetUser, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		if grant {
			return security.GrantSystemPermission(ctx, address, user, int8(permission))
		}
		return security.RevokeSystemPermission(ctx, address, user, int8(permission))
	})
}

func (c *Connector) changeTablePermission(
	ctx context.Context,
	user, tableName string,
	permission TablePermission,
	grant bool,
) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if err := validateExistingTableName(tableName); err != nil {
		return err
	}
	if !permission.valid() {
		return invalidPermission("table", int8(permission))
	}
	return c.securityVoid(ctx, securityTargetTable, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		if grant {
			return security.GrantTablePermission(ctx, address, user, tableName, int8(permission))
		}
		return security.RevokeTablePermission(ctx, address, user, tableName, int8(permission))
	})
}

func (c *Connector) changeNamespacePermission(
	ctx context.Context,
	user, namespace string,
	permission NamespacePermission,
	grant bool,
) error {
	if err := validateNamespacePermissionRequest(user, namespace, permission); err != nil {
		return err
	}
	return c.securityVoid(ctx, securityTargetNamespace, func(
		security managerclient.SecurityAdapter,
		address string,
	) error {
		if grant {
			return security.GrantNamespacePermission(ctx, address, user, namespace, int8(permission))
		}
		return security.RevokeNamespacePermission(ctx, address, user, namespace, int8(permission))
	})
}

type securityTarget uint8

const (
	securityTargetUser securityTarget = iota
	securityTargetTable
	securityTargetNamespace
)

func securityCall[T any](
	c *Connector,
	ctx context.Context,
	target securityTarget,
	call func(managerclient.SecurityAdapter, string) (T, error),
) (result T, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return result, ErrConnectorClosed
	}
	resolver := c.clientAddr
	security := c.security
	c.mu.RUnlock()
	if resolver == nil {
		return result, ErrDiscoveryUnavailable
	}
	addresses, err := resolver.Addresses(ctx)
	if errors.Is(err, zk.ErrClientServiceUnavailable) {
		return result, ErrClientServiceUnavailable
	}
	if err != nil {
		return result, fmt.Errorf("accumulo: discover client service: %w", err)
	}
	var endpointErr error
	for _, address := range addresses {
		result, err = call(security, address)
		if err == nil {
			return result, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		if !managerclient.IsRetryableEndpointError(err) {
			return result, mapSecurityError(target, err)
		}
		endpointErr = errors.Join(endpointErr, fmt.Errorf("%s: %w", address, err))
	}
	if endpointErr == nil {
		return result, ErrClientServiceUnavailable
	}
	return result, fmt.Errorf("%w: %w", ErrClientServiceUnavailable, endpointErr)
}

func (c *Connector) securityVoid(
	ctx context.Context,
	target securityTarget,
	call func(managerclient.SecurityAdapter, string) error,
) error {
	_, err := securityCall(c, ctx, target, func(
		security managerclient.SecurityAdapter,
		address string,
	) (struct{}, error) {
		return struct{}{}, call(security, address)
	})
	var cleanupErr *managerclient.PostResponseCleanupError
	if errors.As(err, &cleanupErr) {
		return nil
	}
	return err
}

func mapSecurityError(target securityTarget, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: security operation: %w", err)
	}
	if managerErr.User != "" ||
		managerErr.Code == "TABLE_DOESNT_EXIST" ||
		managerErr.Code == "NAMESPACE_DOESNT_EXIST" {
		mapped := securityCodeError(managerErr.Code)
		if mapped == ErrNamespaceNotFound && target == securityTargetTable {
			mapped = ErrTableNotFound
		}
		return &SecurityError{User: managerErr.User, Code: managerErr.Code, Err: mapped}
	}
	switch managerErr.Kind {
	case managerclient.ErrorTableNotFound:
		return fmt.Errorf("%w: %q", ErrTableNotFound, managerErr.TableName)
	case managerclient.ErrorNamespaceNotFound:
		if target == securityTargetTable {
			return fmt.Errorf("%w: %q", ErrTableNotFound, managerErr.TableName)
		}
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, managerErr.TableName)
	case managerclient.ErrorInvalidName:
		if target == securityTargetNamespace {
			return fmt.Errorf("%w: %q", ErrInvalidNamespaceName, managerErr.TableName)
		}
		return fmt.Errorf("%w: %q", ErrInvalidTableName, managerErr.TableName)
	case managerclient.ErrorSecurity:
		return &SecurityError{
			User: managerErr.User,
			Code: managerErr.Code,
			Err:  securityCodeError(managerErr.Code),
		}
	default:
		return fmt.Errorf("accumulo: security operation: %w", managerErr)
	}
}

func securityCodeError(code string) error {
	switch code {
	case "BAD_CREDENTIALS", "INVALID_TOKEN", "TOKEN_EXPIRED":
		return ErrBadCredentials
	case "PERMISSION_DENIED":
		return ErrPermissionDenied
	case "USER_DOESNT_EXIST":
		return ErrUserNotFound
	case "USER_EXISTS":
		return ErrUserExists
	case "GRANT_INVALID":
		return ErrInvalidPermission
	case "BAD_AUTHORIZATIONS":
		return ErrInvalidAuthorizations
	case "TABLE_DOESNT_EXIST":
		return ErrTableNotFound
	case "NAMESPACE_DOESNT_EXIST":
		return ErrNamespaceNotFound
	case "UNSUPPORTED_OPERATION":
		return ErrUnsupportedOperation
	case "CONNECTION_ERROR":
		return ErrClientServiceUnavailable
	case "AUTHORIZOR_FAILED":
		return ErrSecurityUnavailable
	default:
		return ErrSecurityUnavailable
	}
}

type credentialUpdater interface {
	UpdateCredentials(*security.TCredentials) error
}

type credentialAdapterCandidate struct {
	name    string
	adapter any
}

type namedCredentialUpdater struct {
	name    string
	updater credentialUpdater
}

func (c *Connector) replacePasswordCredentials(password []byte) error {
	replacement, err := PasswordCredentials(c.Principal(), password)
	if err != nil {
		return err
	}
	thriftCredentials, err := replacement.thrift(c.instance.ID)
	if err != nil {
		return err
	}
	defer wipeThriftCredentials(thriftCredentials)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrConnectorClosed
	}
	if err := updateCredentialAdapters(thriftCredentials, []credentialAdapterCandidate{
		{name: "scan", adapter: c.scan},
		{name: "ingest", adapter: c.ingest},
		{name: "manager", adapter: c.manager},
	}); err != nil {
		return err
	}
	for i := range c.credentials.token {
		c.credentials.token[i] = 0
	}
	c.credentials = replacement.clone()
	return nil
}

func updateCredentialAdapters(
	credentials *security.TCredentials,
	candidates []credentialAdapterCandidate,
) error {
	updaters := make([]namedCredentialUpdater, len(candidates))
	for i, candidate := range candidates {
		updater, ok := candidate.adapter.(credentialUpdater)
		if !ok {
			return fmt.Errorf("accumulo: %s adapter cannot update credentials", candidate.name)
		}
		updaters[i] = namedCredentialUpdater{name: candidate.name, updater: updater}
	}
	for _, candidate := range updaters {
		if err := candidate.updater.UpdateCredentials(credentials); err != nil {
			return fmt.Errorf("accumulo: update %s credentials: %w", candidate.name, err)
		}
	}
	return nil
}

func wipeThriftCredentials(credentials *security.TCredentials) {
	if credentials == nil {
		return
	}
	for i := range credentials.Token {
		credentials.Token[i] = 0
	}
	credentials.Token = nil
}

func validateUser(user string) error {
	if user == "" {
		return fmt.Errorf("%w: empty user name", ErrInvalidUser)
	}
	return nil
}

func validateExistingNamespaceName(namespace string) error {
	// Accumulo's default namespace has the valid empty name.
	if namespace == "" {
		return nil
	}
	if !isAccumuloNameSegment(namespace) {
		return fmt.Errorf("%w: %q", ErrInvalidNamespaceName, namespace)
	}
	return nil
}

func validateExistingTableName(tableName string) error {
	dot := strings.IndexByte(tableName, '.')
	if dot == 0 {
		return fmt.Errorf("%w: namespace missing before dot in %q", ErrInvalidTableName, tableName)
	}
	tablePart := tableName
	if dot > 0 {
		if !isAccumuloNameSegment(tableName[:dot]) {
			return fmt.Errorf("%w: invalid namespace in %q", ErrInvalidTableName, tableName)
		}
		tablePart = tableName[dot+1:]
	}
	if strings.TrimSpace(tablePart) == "" || !isAccumuloNameSegment(tablePart) {
		return fmt.Errorf("%w: %q", ErrInvalidTableName, tableName)
	}
	return nil
}

func isAccumuloNameSegment(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		b := value[i]
		if (b < 'a' || b > 'z') &&
			(b < 'A' || b > 'Z') &&
			(b < '0' || b > '9') &&
			b != '_' {
			return false
		}
	}
	return true
}

func validateNamespacePermissionRequest(
	user, namespace string,
	permission NamespacePermission,
) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if err := validateExistingNamespaceName(namespace); err != nil {
		return err
	}
	if !permission.valid() {
		return invalidPermission("namespace", int8(permission))
	}
	return nil
}

func normalizeAuthorizations(authorizations [][]byte) ([][]byte, error) {
	if authorizations == nil {
		return nil, fmt.Errorf("%w: nil authorization set", ErrInvalidAuthorizations)
	}
	result := make([][]byte, len(authorizations))
	for i, authorization := range authorizations {
		if len(authorization) == 0 {
			return nil, fmt.Errorf("%w: empty authorization at index %d", ErrInvalidAuthorizations, i)
		}
		result[i] = append([]byte(nil), authorization...)
	}
	sort.Slice(result, func(i, j int) bool { return bytes.Compare(result[i], result[j]) < 0 })
	deduped := result[:0]
	for _, authorization := range result {
		if len(deduped) == 0 || !bytes.Equal(deduped[len(deduped)-1], authorization) {
			deduped = append(deduped, authorization)
		}
	}
	return deduped, nil
}

func invalidPermission(kind string, value int8) error {
	return fmt.Errorf("%w: %s ordinal %d", ErrInvalidPermission, kind, value)
}

func (p SystemPermission) valid() bool {
	return p >= SystemPermissionGrant && p <= SystemPermissionObtainDelegationToken
}

func (p TablePermission) valid() bool {
	return p >= TablePermissionRead && p <= TablePermissionGetSummaries
}

func (p NamespacePermission) valid() bool {
	return p >= NamespacePermissionRead && p <= NamespacePermissionDropNamespace
}
