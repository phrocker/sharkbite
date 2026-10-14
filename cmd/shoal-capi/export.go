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
	"strings"
	"time"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
	publichdfs "github.com/phrocker/shoal-oss/hdfs"
)

const defaultBootstrapTimeout = 30 * time.Second

// Mutable only so lifecycle tests can shorten the exported free bound.
var connectorFreeTimeout = 5 * time.Second

var abiCapabilityWords = [...]uint64{
	uint64(C.SHOAL_ABI_CAPABILITY_WORD0),
}

type connectorConfig struct {
	bootstrap        int32
	instanceName     string
	instanceID       string
	zookeeperServers []string
	principal        string
	password         []byte
	accumuloVersion  string
	zkSessionTimeout time.Duration
	bootstrapTimeout time.Duration
	instanceSecret   string
	dialTimeout      time.Duration
}

func abiVersionCompatibility() uint32 {
	return uint32(C.SHOAL_ABI_VERSION)
}

func abiVersionMajor() uint32 {
	return uint32(C.SHOAL_ABI_VERSION_MAJOR)
}

func abiVersionMinor() uint32 {
	return uint32(C.SHOAL_ABI_VERSION_MINOR)
}

func abiVersionPatch() uint32 {
	return uint32(C.SHOAL_ABI_VERSION_PATCH)
}

func abiVersionPacked() uint32 {
	return uint32(C.SHOAL_ABI_VERSION_PACKED)
}

func abiCapabilityCount() uint32 {
	return uint32(C.SHOAL_ABI_CAPABILITY_COUNT)
}

func abiCapabilityWordCount() uint32 {
	return uint32(len(abiCapabilityWords))
}

func abiCapabilityWord(wordIndex uint32) uint64 {
	if wordIndex >= abiCapabilityWordCount() {
		return 0
	}
	return abiCapabilityWords[wordIndex]
}

func abiHasCapability(capabilityID uint32) bool {
	wordIndex := capabilityID / 64
	bitIndex := capabilityID % 64
	return abiCapabilityWord(wordIndex)&(uint64(1)<<bitIndex) != 0
}

//export shoal_abi_version
func shoal_abi_version() C.uint32_t {
	return C.uint32_t(abiVersionCompatibility())
}

//export shoal_abi_version_major
func shoal_abi_version_major() C.uint32_t {
	return C.uint32_t(abiVersionMajor())
}

//export shoal_abi_version_minor
func shoal_abi_version_minor() C.uint32_t {
	return C.uint32_t(abiVersionMinor())
}

//export shoal_abi_version_patch
func shoal_abi_version_patch() C.uint32_t {
	return C.uint32_t(abiVersionPatch())
}

//export shoal_abi_version_packed
func shoal_abi_version_packed() C.uint32_t {
	return C.uint32_t(abiVersionPacked())
}

//export shoal_abi_capability_count
func shoal_abi_capability_count() C.uint32_t {
	return C.uint32_t(abiCapabilityCount())
}

//export shoal_abi_capability_word_count
func shoal_abi_capability_word_count() C.uint32_t {
	return C.uint32_t(abiCapabilityWordCount())
}

//export shoal_abi_capability_word
func shoal_abi_capability_word(wordIndex C.uint32_t) C.uint64_t {
	return C.uint64_t(abiCapabilityWord(uint32(wordIndex)))
}

//export shoal_abi_has_capability
func shoal_abi_has_capability(capabilityID C.uint32_t) C.uint8_t {
	if abiHasCapability(uint32(capabilityID)) {
		return 1
	}
	return 0
}

//export shoal_connector_config_init
func shoal_connector_config_init(config *C.shoal_connector_config) {
	C.shoal_bridge_connector_config_init(config)
}

//export shoal_connector_create
func shoal_connector_create(
	config *C.shoal_connector_config,
	outConnector **C.shoal_connector,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outConnector != nil {
		*outConnector = nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			status = fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: internal panic: %v", recovered))
		}
	}()
	if outConnector == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_connector is required"))
	}

	parsed, err := parseConnectorConfig(config)
	if err != nil {
		if errors.Is(err, accumulo.ErrUnsupportedVersion) {
			return fail(outError, C.SHOAL_STATUS_UNSUPPORTED, err)
		}
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	defer zeroBytes(parsed.password)
	owned, code, err := openConnector(parsed)
	if err != nil {
		return fail(outError, code, err)
	}

	id, ok := connectors.add(owned)
	if !ok {
		_ = owned.close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: connector handle space exhausted"))
	}
	handle := C.shoal_bridge_connector_alloc(C.uint64_t(id))
	if handle == nil {
		removed, _ := connectors.remove(id)
		_ = removed.close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate connector handle"))
	}
	*outConnector = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_close
func shoal_connector_close(
	handle *C.shoal_connector,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer func() {
		if recovered := recover(); recovered != nil {
			status = fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: internal panic: %v", recovered))
		}
	}()
	owned, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := owned.close(); err != nil {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: close connector: %w", err))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_free
func shoal_connector_free(handle **C.shoal_connector) {
	defer func() {
		_ = recover()
	}()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_connector_id(value))
	if owned, ok := connectors.remove(id); ok {
		_ = owned.closeBounded(connectorFreeTimeout)
	}
	C.shoal_bridge_connector_free(value)
}

//export shoal_error_code
func shoal_error_code(err *C.shoal_error) C.shoal_status {
	return C.shoal_bridge_error_code(err)
}

//export shoal_error_message
func shoal_error_message(err *C.shoal_error) *C.char {
	return C.shoal_bridge_error_message(err)
}

//export shoal_error_security_user
func shoal_error_security_user(err *C.shoal_error) *C.char {
	return C.shoal_bridge_error_security_user(err)
}

//export shoal_error_security_code
func shoal_error_security_code(err *C.shoal_error) *C.char {
	return C.shoal_bridge_error_security_code(err)
}

//export shoal_error_visibility_parse
func shoal_error_visibility_parse(err *C.shoal_error, out *C.shoal_visibility_parse_error_view) C.shoal_status {
	if err == nil || out == nil {
		return C.SHOAL_STATUS_INVALID_ARGUMENT
	}
	if uint64(out.struct_size) < uint64(C.SHOAL_VISIBILITY_PARSE_ERROR_VIEW_V1_SIZE) {
		return C.SHOAL_STATUS_INVALID_ARGUMENT
	}
	if C.shoal_bridge_error_visibility_parse(err, out) == 0 {
		return C.SHOAL_STATUS_NOT_FOUND
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_error_source
func shoal_error_source(err *C.shoal_error) C.shoal_error_source_class {
	return C.shoal_bridge_error_source(err)
}

//export shoal_error_source_name
func shoal_error_source_name(err *C.shoal_error) *C.char {
	return (*C.char)(unsafe.Pointer(C.shoal_bridge_error_source_name(err)))
}

//export shoal_error_compatibility
func shoal_error_compatibility(err *C.shoal_error) C.shoal_error_compatibility_class {
	return C.shoal_bridge_error_compatibility(err)
}

//export shoal_error_compatibility_name
func shoal_error_compatibility_name(err *C.shoal_error) *C.char {
	return (*C.char)(unsafe.Pointer(C.shoal_bridge_error_compatibility_name(err)))
}

//export shoal_error_compatibility_code
func shoal_error_compatibility_code(err *C.shoal_error) C.int16_t {
	return C.shoal_bridge_error_compatibility_code(err)
}

//export shoal_error_free
func shoal_error_free(err **C.shoal_error) {
	if err == nil || *err == nil {
		return
	}
	C.shoal_bridge_error_free(*err)
	*err = nil
}

func parseConnectorConfig(config *C.shoal_connector_config) (connectorConfig, error) {
	if config == nil {
		return connectorConfig{}, errors.New("shoal: connector config is required")
	}
	requiredSize := uint64(C.shoal_bridge_connector_config_v1_size())
	if uint64(config.struct_size) < requiredSize {
		return connectorConfig{}, fmt.Errorf(
			"shoal: config struct_size is %d, need at least %d",
			uint64(config.struct_size),
			requiredSize,
		)
	}
	instanceName, err := requiredString(config.instance_name, "instance_name")
	if err != nil {
		return connectorConfig{}, err
	}
	principal, err := requiredString(config.principal, "principal")
	if err != nil {
		return connectorConfig{}, err
	}
	password, err := copyBytes(config.password, config.password_length, "password")
	if err != nil {
		return connectorConfig{}, err
	}
	parsed := connectorConfig{
		bootstrap:       int32(config.bootstrap),
		instanceName:    instanceName,
		principal:       principal,
		password:        password,
		instanceSecret:  optionalString(config.instance_secret),
		accumuloVersion: optionalString(config.accumulo_version),
	}
	if parsed.accumuloVersion != "" && !strings.HasPrefix(parsed.accumuloVersion, "4.") {
		zeroBytes(parsed.password)
		return connectorConfig{}, fmt.Errorf(
			"%w: only Accumulo 4.x is supported, got %q",
			accumulo.ErrUnsupportedVersion,
			parsed.accumuloVersion,
		)
	}
	parsed.dialTimeout, err = durationMilliseconds(config.dial_timeout_ms, "dial_timeout_ms", 0)
	if err != nil {
		zeroBytes(parsed.password)
		return connectorConfig{}, err
	}

	switch config.bootstrap {
	case C.SHOAL_BOOTSTRAP_STATIC:
		parsed.instanceID, err = requiredString(config.instance_id, "instance_id")
	case C.SHOAL_BOOTSTRAP_ZOOKEEPER:
		parsed.zookeeperServers, err = parseServers(config.zookeeper_servers)
		if err == nil {
			parsed.zkSessionTimeout, err = durationMilliseconds(
				config.zookeeper_session_timeout_ms,
				"zookeeper_session_timeout_ms",
				0,
			)
		}
		if err == nil {
			parsed.bootstrapTimeout, err = durationMilliseconds(
				config.bootstrap_timeout_ms,
				"bootstrap_timeout_ms",
				defaultBootstrapTimeout,
			)
		}
	default:
		err = fmt.Errorf("shoal: unsupported bootstrap mode %d", int32(config.bootstrap))
	}
	if err != nil {
		zeroBytes(parsed.password)
		return connectorConfig{}, err
	}
	return parsed, nil
}

func openConnector(config connectorConfig) (*ownedConnector, C.shoal_status, error) {
	credentials, err := accumulo.PasswordCredentials(config.principal, config.password)
	if err != nil {
		return nil, C.SHOAL_STATUS_INVALID_ARGUMENT, err
	}

	var instance accumulo.Instance
	switch config.bootstrap {
	case int32(C.SHOAL_BOOTSTRAP_STATIC):
		instance, err = accumulo.NewStaticInstance(config.instanceName, config.instanceID)
	case int32(C.SHOAL_BOOTSTRAP_ZOOKEEPER):
		ctx, cancel := context.WithTimeout(context.Background(), config.bootstrapTimeout)
		defer cancel()
		instance, err = accumulo.NewZooKeeperInstance(ctx, accumulo.ZooKeeperConfig{
			Servers:        config.zookeeperServers,
			InstanceName:   config.instanceName,
			SessionTimeout: config.zkSessionTimeout,
			InstanceSecret: config.instanceSecret,
		})
	}
	if err != nil {
		return nil, C.SHOAL_STATUS_BOOTSTRAP_FAILED, err
	}

	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{
		AccumuloVersion: config.accumuloVersion,
		DialTimeout:     config.dialTimeout,
	})
	if err != nil {
		_ = instance.Close()
		if errors.Is(err, accumulo.ErrUnsupportedVersion) {
			return nil, C.SHOAL_STATUS_UNSUPPORTED, err
		}
		return nil, C.SHOAL_STATUS_INTERNAL, err
	}
	return newOwnedConnector(connector, instance), C.SHOAL_STATUS_OK, nil
}

func lookupConnector(handle *C.shoal_connector) (*ownedConnector, error) {
	if handle == nil {
		return nil, errors.New("shoal: connector handle is NULL")
	}
	id := uint64(C.shoal_bridge_connector_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: connector handle is invalid")
	}
	connector, ok := connectors.get(id)
	if !ok {
		return nil, errors.New("shoal: connector handle is unknown or freed")
	}
	return connector, nil
}

func requiredString(value *C.char, name string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("shoal: %s is required", name)
	}
	converted := C.GoString(value)
	if converted == "" {
		return "", fmt.Errorf("shoal: %s is required", name)
	}
	return converted, nil
}

func requiredStringAllowEmpty(value *C.char, name string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("shoal: %s is required", name)
	}
	return C.GoString(value), nil
}

func optionalString(value *C.char) string {
	if value == nil {
		return ""
	}
	return C.GoString(value)
}

func parseServers(value *C.char) ([]string, error) {
	raw, err := requiredString(value, "zookeeper_servers")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(raw, ",")
	servers := make([]string, 0, len(parts))
	for _, part := range parts {
		server := strings.TrimSpace(part)
		if server == "" {
			return nil, errors.New("shoal: zookeeper_servers contains an empty server")
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func copyBytes(value *C.uint8_t, length C.size_t, name string) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	if value == nil {
		return nil, fmt.Errorf("shoal: %s is NULL with non-zero length", name)
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(length) > maxInt {
		return nil, fmt.Errorf("shoal: %s is too large", name)
	}
	source := unsafe.Slice((*byte)(unsafe.Pointer(value)), int(length))
	return append([]byte(nil), source...), nil
}

func durationMilliseconds(value C.int64_t, name string, defaultValue time.Duration) (time.Duration, error) {
	milliseconds := int64(value)
	if milliseconds < 0 {
		return 0, fmt.Errorf("shoal: %s must not be negative", name)
	}
	if milliseconds == 0 {
		return defaultValue, nil
	}
	if milliseconds > int64(^uint64(0)>>1)/int64(time.Millisecond) {
		return 0, fmt.Errorf("shoal: %s is too large", name)
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func clearError(outError **C.shoal_error) {
	if outError != nil {
		*outError = nil
	}
}

func cStringData(value string) *C.char {
	if len(value) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(unsafe.StringData(value)))
}

func bridgeErrorAlloc(code C.shoal_status, err error) *C.shoal_error {
	message := ""
	user := ""
	securityCode := ""
	visibilityTerms := []byte(nil)
	visibilityReason := ""
	visibilityOffset := 0
	hasVisibilityParse := false
	if err != nil {
		message = err.Error()
		var securityErr *accumulo.SecurityError
		if errors.As(err, &securityErr) {
			user = securityErr.User
			securityCode = securityErr.Code
		}
		var visibilityErr *accumulo.VisibilityParseError
		if errors.As(err, &visibilityErr) {
			visibilityTerms = visibilityErr.Terms
			visibilityReason = visibilityErr.Reason
			if visibilityErr.Offset >= 0 {
				visibilityOffset = visibilityErr.Offset
			}
			hasVisibilityParse = true
		}
	}
	source, compatibility := compatibilityClassesForError(int32(code), err)
	if errors.Is(err, accumulo.ErrVisibilityParse) {
		source = errorSourceVisibilityParseException
		compatibility = errorCompatibilityClientException
	}
	return C.shoal_bridge_error_alloc(
		code,
		C.shoal_error_source_class(source),
		C.shoal_error_compatibility_class(compatibility),
		C.int16_t(compatibilityCodeForError(err)),
		cStringData(message),
		C.size_t(len(message)),
		cStringData(user),
		C.size_t(len(user)),
		cStringData(securityCode),
		C.size_t(len(securityCode)),
		(*C.uint8_t)(bytePointer(visibilityTerms)),
		C.size_t(len(visibilityTerms)),
		cStringData(visibilityReason),
		C.size_t(len(visibilityReason)),
		C.size_t(visibilityOffset),
		boolToCUint8(hasVisibilityParse),
	)
}

const (
	errorSourceRuntime                       int32 = 0
	errorSourceClientException               int32 = 1
	errorSourceIllegalStateException         int32 = 2
	errorSourceIterationInterruptedException int32 = 3
	errorSourceVisibilityParseException      int32 = 4
	errorSourceHDFSException                 int32 = 5
	errorSourceIllegalArgumentException      int32 = 6

	errorCompatibilityRuntimeError    int32 = 0
	errorCompatibilityClientException int32 = 1
)

func compatibilityClassesForError(status int32, err error) (int32, int32) {
	var hdfsErr *publichdfs.Error
	if errors.As(err, &hdfsErr) {
		return errorSourceHDFSException, errorCompatibilityRuntimeError
	}
	if status == int32(C.SHOAL_STATUS_INVALID_ARGUMENT) {
		return errorSourceIllegalArgumentException, errorCompatibilityRuntimeError
	}
	if errors.Is(err, accumulo.ErrVisibilityParse) {
		return errorSourceVisibilityParseException, errorCompatibilityClientException
	}
	switch status {
	case int32(C.SHOAL_STATUS_CLOSED):
		return errorSourceIllegalStateException, errorCompatibilityRuntimeError
	case int32(C.SHOAL_STATUS_CANCELLED):
		return errorSourceIterationInterruptedException, errorCompatibilityRuntimeError
	}
	if isClientCompatibilityError(err) {
		return errorSourceClientException, errorCompatibilityClientException
	}
	return errorSourceRuntime, errorCompatibilityRuntimeError
}

func isClientCompatibilityError(err error) bool {
	return errors.Is(err, accumulo.ErrUnsupportedVersion) ||
		errors.Is(err, accumulo.ErrDiscoveryUnavailable) ||
		errors.Is(err, accumulo.ErrTableNotFound) ||
		errors.Is(err, accumulo.ErrTableExists) ||
		errors.Is(err, accumulo.ErrNamespaceNotFound) ||
		errors.Is(err, accumulo.ErrNamespaceExists) ||
		errors.Is(err, accumulo.ErrNamespaceNotEmpty) ||
		errors.Is(err, accumulo.ErrPermissionDenied) ||
		errors.Is(err, accumulo.ErrManagerUnavailable) ||
		errors.Is(err, accumulo.ErrClientServiceUnavailable) ||
		errors.Is(err, accumulo.ErrNoTabletCoversRow) ||
		errors.Is(err, accumulo.ErrTabletNotLocated) ||
		errors.Is(err, accumulo.ErrTableOffline) ||
		errors.Is(err, accumulo.ErrTableSplitsIncomplete) ||
		errors.Is(err, accumulo.ErrUserNotFound) ||
		errors.Is(err, accumulo.ErrBadCredentials) ||
		errors.Is(err, accumulo.ErrSecurityUnavailable) ||
		errors.Is(err, accumulo.ErrBatchWriterRetryExhausted) ||
		errors.Is(err, accumulo.ErrBatchWriterFailed)
}

func compatibilityClassesForStatus(status int32) (int32, int32) {
	if status == int32(C.SHOAL_STATUS_INVALID_ARGUMENT) {
		return errorSourceRuntime, errorCompatibilityRuntimeError
	}
	switch status {
	case int32(C.SHOAL_STATUS_CLOSED):
		return errorSourceIllegalStateException, errorCompatibilityRuntimeError
	case int32(C.SHOAL_STATUS_CANCELLED):
		return errorSourceIterationInterruptedException, errorCompatibilityRuntimeError
	case int32(C.SHOAL_STATUS_NOT_FOUND):
		return errorSourceClientException, errorCompatibilityClientException
	default:
		return errorSourceRuntime, errorCompatibilityRuntimeError
	}
}

func compatibilityCodeForError(err error) int16 {
	switch {
	case errors.Is(err, accumulo.ErrBadCredentials):
		return 5
	case errors.Is(err, accumulo.ErrDiscoveryUnavailable):
		return 4
	case errors.Is(err, accumulo.ErrTabletNotLocated),
		errors.Is(err, accumulo.ErrNoTabletCoversRow):
		return 8
	case errors.Is(err, accumulo.ErrTableNotFound):
		return 9
	case errors.Is(err, accumulo.ErrInvalidTableRange):
		return 10
	case errors.Is(err, accumulo.ErrNamespaceNotEmpty):
		return 11
	case errors.Is(err, accumulo.ErrInvalidTableName),
		errors.Is(err, accumulo.ErrInvalidNamespaceName),
		errors.Is(err, accumulo.ErrInvalidUser),
		errors.Is(err, accumulo.ErrInvalidPassword),
		errors.Is(err, accumulo.ErrInvalidAuthorizations),
		errors.Is(err, accumulo.ErrInvalidPermission),
		errors.Is(err, publichdfs.ErrInvalidArgument):
		return 12
	default:
		return -1
	}
}

func fail(outError **C.shoal_error, code C.shoal_status, err error) C.shoal_status {
	if outError == nil {
		return code
	}
	*outError = nil
	*outError = bridgeErrorAlloc(code, err)
	if *outError == nil {
		return C.SHOAL_STATUS_OUT_OF_MEMORY
	}
	return code
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
