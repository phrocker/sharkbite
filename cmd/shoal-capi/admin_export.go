package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

type namespaceAdminAPI interface {
	Namespaces(context.Context) ([]accumulo.Namespace, error)
	NamespaceExists(context.Context, string) (bool, error)
	CreateNamespace(context.Context, string) error
	DeleteNamespace(context.Context, string) error
	RenameNamespace(context.Context, string, string) error
	SetNamespaceProperty(context.Context, string, string, string) error
	RemoveNamespaceProperty(context.Context, string, string) error
	EffectiveNamespaceProperties(context.Context, string) (map[string]string, error)
	NamespaceProperties(context.Context, string) (map[string]string, error)
	VersionedNamespaceProperties(context.Context, string) (accumulo.VersionedProperties, error)
}

type securityAdminAPI interface {
	CreateUser(context.Context, string, []byte) error
	DropUser(context.Context, string) error
	ChangePassword(context.Context, string, []byte) error
	ChangeUserAuthorizations(context.Context, string, [][]byte) error
	GetUserAuthorizations(context.Context, string) ([][]byte, error)
	HasSystemPermission(context.Context, string, accumulo.SystemPermission) (bool, error)
	HasTablePermission(context.Context, string, string, accumulo.TablePermission) (bool, error)
	HasNamespacePermission(context.Context, string, string, accumulo.NamespacePermission) (bool, error)
	GrantSystemPermission(context.Context, string, accumulo.SystemPermission) error
	RevokeSystemPermission(context.Context, string, accumulo.SystemPermission) error
	GrantTablePermission(context.Context, string, string, accumulo.TablePermission) error
	RevokeTablePermission(context.Context, string, string, accumulo.TablePermission) error
	GrantNamespacePermission(context.Context, string, string, accumulo.NamespacePermission) error
	RevokeNamespacePermission(context.Context, string, string, accumulo.NamespacePermission) error
}

type tableSplitAPI interface {
	ListTableSplits(context.Context, string) ([][]byte, error)
	AddTableSplits(context.Context, string, [][]byte) error
}

func connectorCapability[T any](owned *ownedConnector, name string) (T, error) {
	value, ok := owned.connector.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("shoal: connector does not support %s", name)
	}
	return value, nil
}

//export shoal_connector_list_namespaces
func shoal_connector_list_namespaces(handle *C.shoal_connector, timeout C.int64_t, out **C.shoal_namespace_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	owned, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	_ = owned
	if err != nil {
		return fail(outError, code, err)
	}
	namespaces, err := func() ([]accumulo.Namespace, error) {
		defer done()
		return admin.Namespaces(ctx)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := buildNamespaceListResult(namespaces)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_namespace_exists
func shoal_connector_namespace_exists(handle *C.shoal_connector, name *C.char, timeout C.int64_t, outExists *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if outExists != nil {
		*outExists = 0
	}
	defer recoverStatus(&status, outError)
	if outExists == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_exists is required"))
	}
	namespace, err := requiredStringAllowEmpty(name, "namespace_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	_, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	exists, err := func() (bool, error) {
		defer done()
		return admin.NamespaceExists(ctx, namespace)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	if exists {
		*outExists = 1
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_create_namespace
func shoal_connector_create_namespace(handle *C.shoal_connector, name *C.char, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return mutateNamespace(handle, name, nil, false, timeout, outError, func(ctx context.Context, admin namespaceAdminAPI, first, _ string) error {
		return admin.CreateNamespace(ctx, first)
	})
}

//export shoal_connector_delete_namespace
func shoal_connector_delete_namespace(handle *C.shoal_connector, name *C.char, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return mutateNamespace(handle, name, nil, true, timeout, outError, func(ctx context.Context, admin namespaceAdminAPI, first, _ string) error {
		return admin.DeleteNamespace(ctx, first)
	})
}

//export shoal_connector_rename_namespace
func shoal_connector_rename_namespace(handle *C.shoal_connector, name, newName *C.char, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return mutateNamespace(handle, name, newName, true, timeout, outError, func(ctx context.Context, admin namespaceAdminAPI, first, second string) error {
		return admin.RenameNamespace(ctx, first, second)
	})
}

//export shoal_connector_set_namespace_property
func shoal_connector_set_namespace_property(handle *C.shoal_connector, name, property, value *C.char, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	namespace, err := requiredStringAllowEmpty(name, "namespace_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	propertyName, err := requiredString(property, "property_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	propertyValue, err := requiredStringAllowEmpty(value, "property_value")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	_, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return admin.SetNamespaceProperty(ctx, namespace, propertyName, propertyValue)
	}()
	return failOrOK(outError, err)
}

//export shoal_connector_remove_namespace_property
func shoal_connector_remove_namespace_property(handle *C.shoal_connector, name, property *C.char, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	namespace, err := requiredStringAllowEmpty(name, "namespace_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	propertyName, err := requiredString(property, "property_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	_, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return admin.RemoveNamespaceProperty(ctx, namespace, propertyName)
	}()
	return failOrOK(outError, err)
}

//export shoal_connector_effective_namespace_properties
func shoal_connector_effective_namespace_properties(handle *C.shoal_connector, name *C.char, timeout C.int64_t, out **C.shoal_namespace_properties_result, outError **C.shoal_error) C.shoal_status {
	return readNamespaceProperties(handle, name, timeout, out, outError, func(ctx context.Context, admin namespaceAdminAPI, namespace string) (map[string]string, error) {
		return admin.EffectiveNamespaceProperties(ctx, namespace)
	})
}

//export shoal_connector_namespace_properties
func shoal_connector_namespace_properties(handle *C.shoal_connector, name *C.char, timeout C.int64_t, out **C.shoal_namespace_properties_result, outError **C.shoal_error) C.shoal_status {
	return readNamespaceProperties(handle, name, timeout, out, outError, func(ctx context.Context, admin namespaceAdminAPI, namespace string) (map[string]string, error) {
		return admin.NamespaceProperties(ctx, namespace)
	})
}

//export shoal_connector_versioned_namespace_properties
func shoal_connector_versioned_namespace_properties(handle *C.shoal_connector, name *C.char, timeout C.int64_t, out **C.shoal_versioned_properties_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	namespace, err := requiredStringAllowEmpty(name, "namespace_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	_, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	properties, err := func() (accumulo.VersionedProperties, error) {
		defer done()
		return admin.VersionedNamespaceProperties(ctx, namespace)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := buildVersionedPropertiesResult(properties)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

func beginNamespaceAdmin(handle *C.shoal_connector, timeout C.int64_t) (*ownedConnector, namespaceAdminAPI, context.Context, func(), C.shoal_status, error) {
	owned, err := lookupConnector(handle)
	if err != nil {
		return nil, nil, nil, nil, C.SHOAL_STATUS_INVALID_HANDLE, err
	}
	admin, err := connectorCapability[namespaceAdminAPI](owned, "namespace administration")
	if err != nil {
		return nil, nil, nil, nil, C.SHOAL_STATUS_UNSUPPORTED, err
	}
	ctx, done, code, err := beginConnectorOperation(owned, timeout)
	return owned, admin, ctx, done, code, err
}

func mutateNamespace(handle *C.shoal_connector, first, second *C.char, allowEmptyFirst bool, timeout C.int64_t, outError **C.shoal_error, call func(context.Context, namespaceAdminAPI, string, string) error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	parseFirst := requiredString
	if allowEmptyFirst {
		parseFirst = requiredStringAllowEmpty
	}
	firstName, err := parseFirst(first, "namespace_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	secondName := ""
	if second != nil {
		secondName, err = requiredString(second, "new_namespace_name")
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
	}
	_, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return call(ctx, admin, firstName, secondName)
	}()
	return failOrOK(outError, err)
}

func readNamespaceProperties(handle *C.shoal_connector, name *C.char, timeout C.int64_t, out **C.shoal_namespace_properties_result, outError **C.shoal_error, call func(context.Context, namespaceAdminAPI, string) (map[string]string, error)) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	namespace, err := requiredStringAllowEmpty(name, "namespace_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	_, admin, ctx, done, code, err := beginNamespaceAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	properties, err := func() (map[string]string, error) {
		defer done()
		return call(ctx, admin, namespace)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := buildNamespacePropertiesResult(properties)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_create_user
func shoal_connector_create_user(handle *C.shoal_connector, user *C.char, password *C.shoal_bytes, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return mutateUserPassword(handle, user, password, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name string, value []byte) error {
		return admin.CreateUser(ctx, name, value)
	})
}

//export shoal_connector_drop_user
func shoal_connector_drop_user(handle *C.shoal_connector, user *C.char, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	name, err := requiredString(user, "user")
	if err != nil {
		clearError(outError)
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return securityVoid(handle, timeout, outError, func(ctx context.Context, admin securityAdminAPI) error {
		return admin.DropUser(ctx, name)
	})
}

//export shoal_connector_change_password
func shoal_connector_change_password(handle *C.shoal_connector, user *C.char, password *C.shoal_bytes, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return mutateUserPassword(handle, user, password, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name string, value []byte) error {
		return admin.ChangePassword(ctx, name, value)
	})
}

//export shoal_connector_change_user_authorizations
func shoal_connector_change_user_authorizations(handle *C.shoal_connector, user *C.char, values *C.shoal_bytes, count C.size_t, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	name, err := requiredString(user, "user")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	authorizations, err := copyBytesArray(values, count, "authorizations")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	status = securityVoid(handle, timeout, outError, func(ctx context.Context, admin securityAdminAPI) error {
		return admin.ChangeUserAuthorizations(ctx, name, authorizations)
	})
	return status
}

//export shoal_connector_get_user_authorizations
func shoal_connector_get_user_authorizations(handle *C.shoal_connector, user *C.char, timeout C.int64_t, out **C.shoal_bytes_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	name, err := requiredString(user, "user")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	owned, admin, ctx, done, code, err := beginSecurityAdmin(handle, timeout)
	_ = owned
	if err != nil {
		return fail(outError, code, err)
	}
	values, err := func() ([][]byte, error) {
		defer done()
		return admin.GetUserAuthorizations(ctx, name)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := buildBytesListResult(values)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_has_system_permission
func shoal_connector_has_system_permission(handle *C.shoal_connector, user *C.char, permission C.shoal_system_permission, timeout C.int64_t, out *C.uint8_t, outError **C.shoal_error) C.shoal_status {
	return securityBool(handle, user, nil, nil, timeout, out, outError, func(ctx context.Context, admin securityAdminAPI, name, _ string) (bool, error) {
		return admin.HasSystemPermission(ctx, name, accumulo.SystemPermission(permission))
	})
}

//export shoal_connector_has_table_permission
func shoal_connector_has_table_permission(handle *C.shoal_connector, user, table *C.char, permission C.shoal_table_permission, timeout C.int64_t, out *C.uint8_t, outError **C.shoal_error) C.shoal_status {
	return securityBool(handle, user, table, requiredString, timeout, out, outError, func(ctx context.Context, admin securityAdminAPI, name, target string) (bool, error) {
		return admin.HasTablePermission(ctx, name, target, accumulo.TablePermission(permission))
	})
}

//export shoal_connector_has_namespace_permission
func shoal_connector_has_namespace_permission(handle *C.shoal_connector, user, namespace *C.char, permission C.shoal_namespace_permission, timeout C.int64_t, out *C.uint8_t, outError **C.shoal_error) C.shoal_status {
	return securityBool(handle, user, namespace, requiredStringAllowEmpty, timeout, out, outError, func(ctx context.Context, admin securityAdminAPI, name, target string) (bool, error) {
		return admin.HasNamespacePermission(ctx, name, target, accumulo.NamespacePermission(permission))
	})
}

//export shoal_connector_grant_system_permission
func shoal_connector_grant_system_permission(handle *C.shoal_connector, user *C.char, permission C.shoal_system_permission, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return securityPermissionChange(handle, user, nil, nil, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name, _ string) error {
		return admin.GrantSystemPermission(ctx, name, accumulo.SystemPermission(permission))
	})
}

//export shoal_connector_revoke_system_permission
func shoal_connector_revoke_system_permission(handle *C.shoal_connector, user *C.char, permission C.shoal_system_permission, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return securityPermissionChange(handle, user, nil, nil, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name, _ string) error {
		return admin.RevokeSystemPermission(ctx, name, accumulo.SystemPermission(permission))
	})
}

//export shoal_connector_grant_table_permission
func shoal_connector_grant_table_permission(handle *C.shoal_connector, user, table *C.char, permission C.shoal_table_permission, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return securityPermissionChange(handle, user, table, requiredString, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name, target string) error {
		return admin.GrantTablePermission(ctx, name, target, accumulo.TablePermission(permission))
	})
}

//export shoal_connector_revoke_table_permission
func shoal_connector_revoke_table_permission(handle *C.shoal_connector, user, table *C.char, permission C.shoal_table_permission, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return securityPermissionChange(handle, user, table, requiredString, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name, target string) error {
		return admin.RevokeTablePermission(ctx, name, target, accumulo.TablePermission(permission))
	})
}

//export shoal_connector_grant_namespace_permission
func shoal_connector_grant_namespace_permission(handle *C.shoal_connector, user, namespace *C.char, permission C.shoal_namespace_permission, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return securityPermissionChange(handle, user, namespace, requiredStringAllowEmpty, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name, target string) error {
		return admin.GrantNamespacePermission(ctx, name, target, accumulo.NamespacePermission(permission))
	})
}

//export shoal_connector_revoke_namespace_permission
func shoal_connector_revoke_namespace_permission(handle *C.shoal_connector, user, namespace *C.char, permission C.shoal_namespace_permission, timeout C.int64_t, outError **C.shoal_error) C.shoal_status {
	return securityPermissionChange(handle, user, namespace, requiredStringAllowEmpty, timeout, outError, func(ctx context.Context, admin securityAdminAPI, name, target string) error {
		return admin.RevokeNamespacePermission(ctx, name, target, accumulo.NamespacePermission(permission))
	})
}

func beginSecurityAdmin(handle *C.shoal_connector, timeout C.int64_t) (*ownedConnector, securityAdminAPI, context.Context, func(), C.shoal_status, error) {
	owned, err := lookupConnector(handle)
	if err != nil {
		return nil, nil, nil, nil, C.SHOAL_STATUS_INVALID_HANDLE, err
	}
	admin, err := connectorCapability[securityAdminAPI](owned, "security administration")
	if err != nil {
		return nil, nil, nil, nil, C.SHOAL_STATUS_UNSUPPORTED, err
	}
	ctx, done, code, err := beginConnectorOperation(owned, timeout)
	return owned, admin, ctx, done, code, err
}

func securityVoid(handle *C.shoal_connector, timeout C.int64_t, outError **C.shoal_error, call func(context.Context, securityAdminAPI) error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	_, admin, ctx, done, code, err := beginSecurityAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return call(ctx, admin)
	}()
	return failOrOK(outError, err)
}

func mutateUserPassword(handle *C.shoal_connector, user *C.char, password *C.shoal_bytes, timeout C.int64_t, outError **C.shoal_error, call func(context.Context, securityAdminAPI, string, []byte) error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	name, err := requiredString(user, "user")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if password == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: password is required"))
	}
	value, err := copyBytes(password.data, password.length, "password")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if value == nil {
		value = []byte{}
	}
	defer zeroBytes(value)
	status = securityVoid(handle, timeout, outError, func(ctx context.Context, admin securityAdminAPI) error {
		return call(ctx, admin, name, value)
	})
	return status
}

func securityBool(handle *C.shoal_connector, user, target *C.char, parseTarget func(*C.char, string) (string, error), timeout C.int64_t, out *C.uint8_t, outError **C.shoal_error, call func(context.Context, securityAdminAPI, string, string) (bool, error)) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_has_permission is required"))
	}
	name, err := requiredString(user, "user")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	targetName := ""
	if parseTarget != nil {
		targetName, err = parseTarget(target, "target_name")
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
	}
	_, admin, ctx, done, code, err := beginSecurityAdmin(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	has, err := func() (bool, error) {
		defer done()
		return call(ctx, admin, name, targetName)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	if has {
		*out = 1
	}
	return C.SHOAL_STATUS_OK
}

func securityPermissionChange(handle *C.shoal_connector, user, target *C.char, parseTarget func(*C.char, string) (string, error), timeout C.int64_t, outError **C.shoal_error, call func(context.Context, securityAdminAPI, string, string) error) C.shoal_status {
	name, err := requiredString(user, "user")
	if err != nil {
		clearError(outError)
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	targetName := ""
	if parseTarget != nil {
		targetName, err = parseTarget(target, "target_name")
		if err != nil {
			clearError(outError)
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
	}
	return securityVoid(handle, timeout, outError, func(ctx context.Context, admin securityAdminAPI) error {
		return call(ctx, admin, name, targetName)
	})
}

//export shoal_connector_list_table_splits
func shoal_connector_list_table_splits(handle *C.shoal_connector, table *C.char, timeout C.int64_t, out **C.shoal_bytes_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	name, err := requiredString(table, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	owned, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	admin, err := connectorCapability[tableSplitAPI](owned, "table split administration")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_UNSUPPORTED, err)
	}
	ctx, done, code, err := beginConnectorOperation(owned, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	values, err := func() ([][]byte, error) {
		defer done()
		return admin.ListTableSplits(ctx, name)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := buildBytesListResult(values)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_add_table_splits
func shoal_connector_add_table_splits(handle *C.shoal_connector, table *C.char, values *C.shoal_bytes, count C.size_t, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	name, err := requiredString(table, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if count == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, accumulo.ErrInvalidTableSplit)
	}
	splits, err := copyBytesArray(values, count, "splits")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	owned, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	admin, err := connectorCapability[tableSplitAPI](owned, "table split administration")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_UNSUPPORTED, err)
	}
	ctx, done, code, err := beginConnectorOperation(owned, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return admin.AddTableSplits(ctx, name, splits)
	}()
	return failOrOK(outError, err)
}

func copyBytesArray(values *C.shoal_bytes, count C.size_t, name string) ([][]byte, error) {
	if count == 0 {
		return [][]byte{}, nil
	}
	if values == nil {
		return nil, fmt.Errorf("shoal: %s is NULL with non-zero count", name)
	}
	maxInt := uint64(^uint(0) >> 1)
	inputElementSize := uint64(unsafe.Sizeof(*values))
	resultElementSize := uint64(unsafe.Sizeof([]byte(nil)))
	maxCount := min(maxInt/inputElementSize, maxInt/resultElementSize)
	if uint64(count) > maxCount {
		return nil, fmt.Errorf("shoal: %s count is too large", name)
	}
	input := unsafe.Slice(values, int(count))
	result := make([][]byte, len(input))
	for index := range input {
		value, err := copyBytes(input[index].data, input[index].length, fmt.Sprintf("%s[%d]", name, index))
		if err != nil {
			return nil, err
		}
		result[index] = value
	}
	return result, nil
}

func buildNamespaceListResult(namespaces []accumulo.Namespace) (*C.shoal_namespace_list_result, C.shoal_status, error) {
	result := C.shoal_bridge_namespace_list_alloc(C.size_t(len(namespaces)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate namespace list result")
	}
	for index, namespace := range namespaces {
		name, code, err := bridgeCString(namespace.Name, fmt.Sprintf("namespace %d name", index))
		if err != nil {
			C.shoal_bridge_namespace_list_free(result)
			return nil, code, err
		}
		id, code, err := bridgeCString(namespace.ID, fmt.Sprintf("namespace %d id", index))
		if err != nil {
			C.shoal_bridge_string_free(name)
			C.shoal_bridge_namespace_list_free(result)
			return nil, code, err
		}
		ok := C.shoal_bridge_namespace_list_set(result, C.size_t(index), name, id) != 0
		C.shoal_bridge_string_free(name)
		C.shoal_bridge_string_free(id)
		if !ok {
			C.shoal_bridge_namespace_list_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate namespace list entry")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func sortedPropertyKeys(properties map[string]string) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func buildNamespacePropertiesResult(properties map[string]string) (*C.shoal_namespace_properties_result, C.shoal_status, error) {
	keys := sortedPropertyKeys(properties)
	result := C.shoal_bridge_namespace_properties_alloc(C.size_t(len(keys)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate namespace properties result")
	}
	for index, key := range keys {
		cKey, code, err := bridgeCString(key, fmt.Sprintf("property %d key", index))
		if err != nil {
			C.shoal_bridge_namespace_properties_free(result)
			return nil, code, err
		}
		cValue, code, err := bridgeCString(properties[key], fmt.Sprintf("property %d value", index))
		if err != nil {
			C.shoal_bridge_string_free(cKey)
			C.shoal_bridge_namespace_properties_free(result)
			return nil, code, err
		}
		ok := C.shoal_bridge_namespace_properties_set(result, C.size_t(index), cKey, cValue) != 0
		C.shoal_bridge_string_free(cKey)
		C.shoal_bridge_string_free(cValue)
		if !ok {
			C.shoal_bridge_namespace_properties_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate namespace property")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func buildVersionedPropertiesResult(properties accumulo.VersionedProperties) (*C.shoal_versioned_properties_result, C.shoal_status, error) {
	keys := sortedPropertyKeys(properties.Properties)
	result := C.shoal_bridge_versioned_properties_alloc(C.int64_t(properties.Version), C.size_t(len(keys)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate versioned properties result")
	}
	for index, key := range keys {
		cKey, code, err := bridgeCString(key, fmt.Sprintf("property %d key", index))
		if err != nil {
			C.shoal_bridge_versioned_properties_free(result)
			return nil, code, err
		}
		cValue, code, err := bridgeCString(properties.Properties[key], fmt.Sprintf("property %d value", index))
		if err != nil {
			C.shoal_bridge_string_free(cKey)
			C.shoal_bridge_versioned_properties_free(result)
			return nil, code, err
		}
		ok := C.shoal_bridge_versioned_properties_set(result, C.size_t(index), cKey, cValue) != 0
		C.shoal_bridge_string_free(cKey)
		C.shoal_bridge_string_free(cValue)
		if !ok {
			C.shoal_bridge_versioned_properties_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate versioned property")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func buildBytesListResult(values [][]byte) (*C.shoal_bytes_list_result, C.shoal_status, error) {
	result := C.shoal_bridge_bytes_list_alloc(C.size_t(len(values)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate bytes list result")
	}
	for index, value := range values {
		var data *C.uint8_t
		if len(value) != 0 {
			data = (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(value)))
		}
		if C.shoal_bridge_bytes_list_set(result, C.size_t(index), data, C.size_t(len(value))) == 0 {
			C.shoal_bridge_bytes_list_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate bytes list entry")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

//export shoal_namespace_list_count
func shoal_namespace_list_count(result *C.shoal_namespace_list_result) C.size_t {
	return C.shoal_bridge_namespace_list_count(result)
}

//export shoal_namespace_list_get
func shoal_namespace_list_get(result *C.shoal_namespace_list_result, index C.size_t, out *C.shoal_namespace_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil || out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: namespace result and output are required"))
	}
	if C.shoal_bridge_namespace_list_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: namespace list index is out of bounds"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_namespace_list_free
func shoal_namespace_list_free(result **C.shoal_namespace_list_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_namespace_list_free(*result)
		*result = nil
	}
}

//export shoal_namespace_properties_count
func shoal_namespace_properties_count(result *C.shoal_namespace_properties_result) C.size_t {
	return C.shoal_bridge_namespace_properties_count(result)
}

//export shoal_namespace_properties_get
func shoal_namespace_properties_get(result *C.shoal_namespace_properties_result, index C.size_t, out *C.shoal_table_property_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil || out == nil || C.shoal_bridge_namespace_properties_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid namespace property result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_namespace_properties_free
func shoal_namespace_properties_free(result **C.shoal_namespace_properties_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_namespace_properties_free(*result)
		*result = nil
	}
}

//export shoal_versioned_properties_version
func shoal_versioned_properties_version(result *C.shoal_versioned_properties_result) C.int64_t {
	return C.shoal_bridge_versioned_properties_version(result)
}

//export shoal_versioned_properties_count
func shoal_versioned_properties_count(result *C.shoal_versioned_properties_result) C.size_t {
	return C.shoal_bridge_versioned_properties_count(result)
}

//export shoal_versioned_properties_get
func shoal_versioned_properties_get(result *C.shoal_versioned_properties_result, index C.size_t, out *C.shoal_table_property_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil || out == nil || C.shoal_bridge_versioned_properties_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid versioned property result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_versioned_properties_free
func shoal_versioned_properties_free(result **C.shoal_versioned_properties_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_versioned_properties_free(*result)
		*result = nil
	}
}

//export shoal_bytes_list_count
func shoal_bytes_list_count(result *C.shoal_bytes_list_result) C.size_t {
	return C.shoal_bridge_bytes_list_count(result)
}

//export shoal_bytes_list_get
func shoal_bytes_list_get(result *C.shoal_bytes_list_result, index C.size_t, out *C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil || out == nil || C.shoal_bridge_bytes_list_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid bytes list result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_bytes_list_free
func shoal_bytes_list_free(result **C.shoal_bytes_list_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_bytes_list_free(*result)
		*result = nil
	}
}
