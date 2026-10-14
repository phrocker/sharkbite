package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
#include <string.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

type zooKeeperInstanceResolver func(context.Context, accumulo.ZooKeeperConfig) (accumulo.Instance, error)

func resolveZooKeeperIdentity(
	ctx context.Context,
	cfg accumulo.ZooKeeperConfig,
	resolve zooKeeperInstanceResolver,
) (accumulo.InstanceInfo, error) {
	instance, err := resolve(ctx, cfg)
	if err != nil {
		return accumulo.InstanceInfo{}, err
	}
	if instance == nil {
		return accumulo.InstanceInfo{}, errors.New("shoal: ZooKeeper resolver returned nil instance")
	}
	info := instance.Info()
	return info, instance.Close()
}

type connectorIdentitySource interface {
	capiConnectorIdentity(context.Context) (accumulo.InstanceInfo, string, error)
}

func readConnectorIdentity(ctx context.Context, connector *ownedConnector) (accumulo.InstanceInfo, string, error) {
	if err := ctx.Err(); err != nil {
		return accumulo.InstanceInfo{}, "", err
	}
	if source, ok := connector.connector.(connectorIdentitySource); ok {
		return source.capiConnectorIdentity(ctx)
	}
	return connector.identity, connector.principal, nil
}

//export shoal_connector_identity_view_init
func shoal_connector_identity_view_init(view *C.shoal_connector_identity_view) {
	if view == nil {
		return
	}
	C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE))
	view.struct_size = C.SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE
}

//export shoal_connector_get_identity
func shoal_connector_get_identity(handle *C.shoal_connector, timeout C.int64_t, out **C.shoal_connector_identity_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	identity, principal, err := readConnectorIdentity(ctx, connector)
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := buildConnectorIdentityResult(identity, principal)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_identity_get
func shoal_connector_identity_get(result *C.shoal_connector_identity_result, out *C.shoal_connector_identity_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: connector identity result is NULL"))
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_identity is required"))
	}
	if out.struct_size < C.SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE {
		return fail(
			outError,
			C.SHOAL_STATUS_INVALID_ARGUMENT,
			fmt.Errorf(
				"shoal: connector identity view struct_size %d is smaller than %d",
				uint64(out.struct_size),
				uint64(C.SHOAL_CONNECTOR_IDENTITY_VIEW_V1_SIZE),
			),
		)
	}
	if C.shoal_bridge_connector_identity_get(result, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid connector identity result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_identity_free
func shoal_connector_identity_free(result **C.shoal_connector_identity_result) {
	if result == nil || *result == nil {
		return
	}
	C.shoal_bridge_connector_identity_free(*result)
	*result = nil
}

//export shoal_zookeeper_resolve_instance
func shoal_zookeeper_resolve_instance(
	instanceName *C.char,
	zookeeperServers *C.char,
	sessionTimeout C.int64_t,
	bootstrapTimeout C.int64_t,
	instanceSecret *C.char,
	out **C.shoal_connector_identity_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	name, err := requiredString(instanceName, "instance_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	servers, err := parseServers(zookeeperServers)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	session, err := durationMilliseconds(sessionTimeout, "session_timeout_ms", 0)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	bootstrap, err := durationMilliseconds(
		bootstrapTimeout,
		"bootstrap_timeout_ms",
		defaultBootstrapTimeout,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootstrap)
	defer cancel()
	info, err := resolveZooKeeperIdentity(
		ctx,
		accumulo.ZooKeeperConfig{
			Servers:        servers,
			InstanceName:   name,
			SessionTimeout: session,
			InstanceSecret: optionalString(instanceSecret),
		},
		accumulo.NewZooKeeperInstance,
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fail(outError, C.SHOAL_STATUS_DEADLINE_EXCEEDED, err)
		}
		return failForError(outError, err)
	}
	result, code, err := buildConnectorIdentityResult(info, "")
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

func buildConnectorIdentityResult(identity accumulo.InstanceInfo, principal string) (*C.shoal_connector_identity_result, C.shoal_status, error) {
	name, code, err := bridgeCString(identity.Name, "instance name")
	if err != nil {
		return nil, code, err
	}
	id, code, err := bridgeCString(identity.ID, "instance id")
	if err != nil {
		C.shoal_bridge_string_free(name)
		return nil, code, err
	}
	cPrincipal, code, err := bridgeCString(principal, "principal")
	if err != nil {
		C.shoal_bridge_string_free(name)
		C.shoal_bridge_string_free(id)
		return nil, code, err
	}
	result := C.shoal_bridge_connector_identity_alloc(name, id, cPrincipal)
	if result == nil {
		C.shoal_bridge_string_free(name)
		C.shoal_bridge_string_free(id)
		C.shoal_bridge_string_free(cPrincipal)
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate connector identity result")
	}
	return result, C.SHOAL_STATUS_OK, nil
}
