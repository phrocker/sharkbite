from __future__ import annotations

import ctypes as C
import ctypes.util
import hashlib
import json
import os
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

from .errors import NativeErrorContext, exception_for_status

ABI_MAJOR = 1
MIN_ABI_VERSION = (1, 19, 0)
CAP_OWNED_SCAN_RESULT = 5
CAP_SCANNER = 3
CAP_BATCH_SCANNER = 4
CAP_MUTATION = 6
CAP_BATCH_WRITER = 7
CAP_STRUCTURED_WRITE_FAILURE = 8
CAP_TABLE_ADMIN = 9
CAP_NAMESPACE_ADMIN = 10
CAP_SECURITY_ADMIN = 11
CAP_TABLE_SPLITS = 12
CAP_TABLE_MAINTENANCE = 19
CAP_HIGH_LEVEL_CLIENT = 21
CAP_HIGH_LEVEL_SCANNER = 22
CAP_COMPATIBILITY_ERRORS = 23
CAP_STREAMING_SCAN_CURSOR = 24
CAP_RFILE = 16
CAP_HDFS = 27
CAP_RFILE_LOCALITY_GROUPS = 28
CAP_CLIENT_PARITY_CONTROLS = 29
CAP_STORAGE_ERROR_PARITY = 30
CAP_ZOOKEEPER_INSTANCE = 31
LogCallback = C.CFUNCTYPE(
    None, C.c_int32, C.c_char_p, C.c_char_p, C.c_void_p
)

_IMPORT_PID = os.getpid()
_FORKED_CHILD = False


class ForkSafetyError(RuntimeError):
    """Raised before an inherited native library or handle can be reused."""


def _mark_forked_child() -> None:
    global _FORKED_CHILD
    _FORKED_CHILD = True


if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_mark_forked_child)


def _ensure_process_safe() -> None:
    if _FORKED_CHILD or os.getpid() != _IMPORT_PID:
        raise ForkSafetyError(
            "Shoal native state cannot be reused after fork; exec a fresh "
            "interpreter before creating or using Shoal objects"
        )


class _ProcessBoundFunction:
    def __init__(self, function: object) -> None:
        object.__setattr__(self, "_function", function)

    def __call__(self, *args: object) -> object:
        _ensure_process_safe()
        return self._function(*args)

    def __getattr__(self, name: str) -> object:
        return getattr(self._function, name)

    def __setattr__(self, name: str, value: object) -> None:
        setattr(self._function, name, value)


class _ProcessBoundLibrary:
    def __init__(self, library: C.CDLL) -> None:
        self._library = library
        self._functions: dict[str, _ProcessBoundFunction] = {}

    def __getattr__(self, name: str) -> object:
        _ensure_process_safe()
        value = getattr(self._library, name)
        if not callable(value):
            return value
        function = self._functions.get(name)
        if function is None:
            function = _ProcessBoundFunction(value)
            self._functions[name] = function
        return function

CAPABILITY_SYMBOLS = {
    CAP_OWNED_SCAN_RESULT: {
        "shoal_scan_result_count",
        "shoal_scan_result_get",
        "shoal_scan_result_free",
    },
    CAP_SCANNER: {
        "shoal_scanner_config_init",
        "shoal_connector_create_scanner",
        "shoal_scanner_close",
        "shoal_scanner_free",
    },
    CAP_BATCH_SCANNER: {
        "shoal_scanner_config_init",
        "shoal_connector_create_batch_scanner",
        "shoal_batch_scanner_close",
        "shoal_batch_scanner_free",
    },
    CAP_STREAMING_SCAN_CURSOR: {
        "shoal_batch_scanner_stream",
        "shoal_scan_cursor_next",
        "shoal_scan_cursor_close",
        "shoal_scan_cursor_free",
    },
    CAP_HIGH_LEVEL_CLIENT: {
        "shoal_client_config_init",
        "shoal_client_create",
        "shoal_client_close",
        "shoal_client_free",
        "shoal_client_list_tables",
        "shoal_client_set_threads",
        "shoal_client_set_table",
    },
    CAP_HIGH_LEVEL_SCANNER: {
        "shoal_range_init",
        "shoal_client_select_column",
        "shoal_client_scan_range",
    },
    CAP_MUTATION: {
        "shoal_mutation_create",
        "shoal_mutation_put",
        "shoal_mutation_delete",
        "shoal_mutation_free",
    },
    CAP_BATCH_WRITER: {
        "shoal_batch_writer_config_init",
        "shoal_connector_create_batch_writer",
        "shoal_batch_writer_add",
        "shoal_batch_writer_flush",
        "shoal_batch_writer_close",
        "shoal_batch_writer_free",
    },
    CAP_CLIENT_PARITY_CONTROLS: {
        "shoal_batch_writer_size",
        "shoal_logging_set_level",
        "shoal_logging_get_level",
    },
    CAP_STORAGE_ERROR_PARITY: {
        "shoal_hdfs_client_mkdir",
        "shoal_hdfs_client_chown",
        "shoal_error_compatibility_code",
        "shoal_logging_set_callback",
    },
    CAP_ZOOKEEPER_INSTANCE: {
        "shoal_zookeeper_resolve_instance",
        "shoal_connector_identity_view_init",
        "shoal_connector_identity_get",
        "shoal_connector_identity_free",
    },
    CAP_STRUCTURED_WRITE_FAILURE: {
        "shoal_write_failure_get_flags",
        "shoal_write_failure_failed_extent_count",
        "shoal_write_failure_get_failed_extent",
        "shoal_write_failure_constraint_count",
        "shoal_write_failure_get_constraint",
        "shoal_write_failure_authorization_count",
        "shoal_write_failure_get_authorization",
        "shoal_write_failure_cleanup_count",
        "shoal_write_failure_get_cleanup",
        "shoal_write_failure_free",
    },
    CAP_TABLE_ADMIN: {
        "shoal_connector_list_tables",
        "shoal_connector_table_exists",
        "shoal_connector_create_table",
        "shoal_connector_delete_table",
        "shoal_connector_rename_table",
        "shoal_connector_flush_table",
        "shoal_connector_set_table_property",
        "shoal_connector_remove_table_property",
        "shoal_connector_effective_table_properties",
        "shoal_table_list_count",
        "shoal_table_list_get",
        "shoal_table_list_free",
        "shoal_table_properties_count",
        "shoal_table_properties_get",
        "shoal_table_properties_free",
    },
    CAP_NAMESPACE_ADMIN: {
        "shoal_connector_list_namespaces",
        "shoal_connector_namespace_exists",
        "shoal_connector_create_namespace",
        "shoal_connector_delete_namespace",
        "shoal_connector_rename_namespace",
        "shoal_connector_set_namespace_property",
        "shoal_connector_remove_namespace_property",
        "shoal_connector_effective_namespace_properties",
        "shoal_connector_namespace_properties",
        "shoal_connector_versioned_namespace_properties",
        "shoal_namespace_list_count",
        "shoal_namespace_list_get",
        "shoal_namespace_list_free",
        "shoal_namespace_properties_count",
        "shoal_namespace_properties_get",
        "shoal_namespace_properties_free",
        "shoal_versioned_properties_version",
        "shoal_versioned_properties_count",
        "shoal_versioned_properties_get",
        "shoal_versioned_properties_free",
    },
    CAP_SECURITY_ADMIN: {
        "shoal_connector_create_user",
        "shoal_connector_drop_user",
        "shoal_connector_change_password",
        "shoal_connector_change_user_authorizations",
        "shoal_connector_get_user_authorizations",
        "shoal_connector_has_system_permission",
        "shoal_connector_has_table_permission",
        "shoal_connector_has_namespace_permission",
        "shoal_connector_grant_system_permission",
        "shoal_connector_revoke_system_permission",
        "shoal_connector_grant_table_permission",
        "shoal_connector_revoke_table_permission",
        "shoal_connector_grant_namespace_permission",
        "shoal_connector_revoke_namespace_permission",
        "shoal_bytes_list_count",
        "shoal_bytes_list_get",
        "shoal_bytes_list_free",
    },
    CAP_TABLE_SPLITS: {
        "shoal_connector_list_table_splits",
        "shoal_connector_add_table_splits",
        "shoal_bytes_list_count",
        "shoal_bytes_list_get",
        "shoal_bytes_list_free",
    },
    CAP_TABLE_MAINTENANCE: {
        "shoal_connector_flush_table_range",
        "shoal_connector_add_table_constraint",
        "shoal_connector_list_table_constraints",
        "shoal_connector_remove_table_constraint",
        "shoal_table_constraint_view_init",
        "shoal_table_constraint_list_count",
        "shoal_table_constraint_list_get",
        "shoal_table_constraint_list_free",
    },
}


class Bytes(C.Structure):
    _fields_ = [("data", C.POINTER(C.c_uint8)), ("length", C.c_size_t)]


class Key(C.Structure):
    _fields_ = [
        ("row", Bytes),
        ("column_family", Bytes),
        ("column_qualifier", Bytes),
        ("column_visibility", Bytes),
        ("timestamp", C.c_int64),
    ]


class RFileWriterConfig(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("codec", C.c_char_p),
        ("block_size", C.c_int64),
    ]


class RFileEntry(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("key", Key),
        ("value", Bytes),
        ("deleted", C.c_uint8),
    ]


class RFileEntryView(C.Structure):
    _fields_ = RFileEntry._fields_


class HDFSDirEntryView(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("name", C.c_char_p),
        ("owner", C.c_char_p),
        ("group", C.c_char_p),
        ("size", C.c_int64),
        ("modification_time_ms", C.c_int64),
        ("mode", C.c_uint32),
        ("is_directory", C.c_uint8),
    ]


class RangeBound(C.Structure):
    _fields_ = [("kind", C.c_int32), ("row", Bytes), ("key", Key)]


class Range(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("start", RangeBound),
        ("end", RangeBound),
        ("start_inclusive", C.c_uint8),
        ("end_inclusive", C.c_uint8),
    ]


class Column(C.Structure):
    _fields_ = [
        ("family", Bytes),
        ("qualifier", Bytes),
        ("has_qualifier", C.c_uint8),
    ]


class IteratorOption(C.Structure):
    _fields_ = [("key", C.c_char_p), ("value", C.c_char_p)]


class IteratorSetting(C.Structure):
    _fields_ = [
        ("name", C.c_char_p),
        ("class_name", C.c_char_p),
        ("priority", C.c_int32),
        ("options", C.POINTER(IteratorOption)),
        ("option_count", C.c_size_t),
    ]


class ScannerConfig(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("table_name", C.c_char_p),
        ("table_id", C.c_char_p),
        ("batch_size", C.c_int32),
        ("authorizations", C.POINTER(Bytes)),
        ("authorization_count", C.c_size_t),
        ("columns", C.POINTER(Column)),
        ("column_count", C.c_size_t),
        ("iterators", C.POINTER(IteratorSetting)),
        ("iterator_count", C.c_size_t),
        ("parallelism", C.c_int32),
        ("use_multi_scan", C.c_uint8),
    ]


class KeyValueView(C.Structure):
    _fields_ = [
        ("row", Bytes),
        ("column_family", Bytes),
        ("column_qualifier", Bytes),
        ("column_visibility", Bytes),
        ("timestamp", C.c_int64),
        ("value", Bytes),
    ]


class ConnectorConfig(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("bootstrap", C.c_int32),
        ("instance_name", C.c_char_p),
        ("instance_id", C.c_char_p),
        ("zookeeper_servers", C.c_char_p),
        ("principal", C.c_char_p),
        ("password", C.POINTER(C.c_uint8)),
        ("password_length", C.c_size_t),
        ("accumulo_version", C.c_char_p),
        ("zookeeper_session_timeout_ms", C.c_int64),
        ("bootstrap_timeout_ms", C.c_int64),
        ("instance_secret", C.c_char_p),
        ("dial_timeout_ms", C.c_int64),
    ]


class ConnectorIdentityView(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("instance_name", C.c_char_p),
        ("instance_id", C.c_char_p),
        ("principal", C.c_char_p),
    ]


class ClientConfig(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("connector", C.POINTER(ConnectorConfig)),
        ("table_name", C.c_char_p),
        ("authorizations", C.POINTER(Bytes)),
        ("authorization_count", C.c_size_t),
        ("thread_count", C.c_int32),
    ]


class BatchWriterConfig(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("table_name", C.c_char_p),
        ("table_id", C.c_char_p),
        ("max_memory_bytes", C.c_int64),
        ("max_batch_bytes", C.c_int64),
        ("max_latency_ms", C.c_int64),
        ("max_write_threads", C.c_int32),
        ("max_retries", C.c_int32),
        ("retry_backoff_ms", C.c_int64),
        ("durability", C.c_int32),
    ]


class TableView(C.Structure):
    _fields_ = [("name", C.c_char_p), ("id", C.c_char_p)]


class PropertyView(C.Structure):
    _fields_ = [("key", C.c_char_p), ("value", C.c_char_p)]


class NamespaceView(C.Structure):
    _fields_ = [("name", C.c_char_p), ("id", C.c_char_p)]


class ConstraintView(C.Structure):
    _fields_ = [
        ("struct_size", C.c_uint32),
        ("number", C.c_int32),
        ("class_name", C.c_char_p),
    ]


class FailedExtentView(C.Structure):
    _fields_ = [
        ("server", C.c_char_p),
        ("table_id", C.c_char_p),
        ("prev_row", Bytes),
        ("end_row", Bytes),
        ("has_prev_row", C.c_uint8),
        ("has_end_row", C.c_uint8),
        ("submitted", C.c_size_t),
        ("committed", C.c_int64),
    ]


class ConstraintViolationView(C.Structure):
    _fields_ = [
        ("server", C.c_char_p),
        ("constraint_class", C.c_char_p),
        ("violation_code", C.c_int16),
        ("description", C.c_char_p),
        ("violating_mutation_count", C.c_int64),
    ]


class AuthorizationFailureView(C.Structure):
    _fields_ = [
        ("server", C.c_char_p),
        ("table_id", C.c_char_p),
        ("prev_row", Bytes),
        ("end_row", Bytes),
        ("has_prev_row", C.c_uint8),
        ("has_end_row", C.c_uint8),
        ("code", C.c_char_p),
    ]


class CleanupFailureView(C.Structure):
    _fields_ = [("server", C.c_char_p), ("message", C.c_char_p)]


@dataclass(frozen=True)
class RuntimeInfo:
    path: str
    version: tuple[int, int, int]
    capabilities: frozenset[int]


def _library_names() -> tuple[str, ...]:
    if sys.platform == "win32":
        return ("shoal.dll", "libshoal.dll")
    if sys.platform == "darwin":
        return ("libshoal.dylib", "shoal.dylib")
    return ("libshoal.so", "shoal.so")


def library_candidates(explicit: str | os.PathLike[str] | None = None) -> Iterable[str]:
    seen: set[str] = set()
    values: list[str] = []
    if explicit:
        values.append(str(Path(explicit).expanduser().resolve()))
    configured = os.environ.get("SHOAL_LIBRARY")
    if configured:
        configured_path = Path(configured).expanduser()
        if not configured_path.is_absolute():
            raise ImportError("SHOAL_LIBRARY must be an absolute path")
        values.append(str(configured_path.resolve()))
    package_libs = Path(__file__).resolve().parent / ".libs"
    for root in (package_libs,):
        values.extend(str(root / name) for name in _library_names())
    if os.environ.get("SHOAL_ALLOW_SYSTEM_LIBRARY") == "1":
        found = ctypes.util.find_library("shoal")
        if found:
            values.append(found)
        values.extend(_library_names())
    for value in values:
        if value not in seen:
            seen.add(value)
            yield value


def _verify_bundled_library(path: Path) -> None:
    package_libs = Path(__file__).resolve().parent / ".libs"
    try:
        path.resolve().relative_to(package_libs.resolve())
    except ValueError:
        return
    manifest_path = package_libs / "_shoal_bundle.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        entry = manifest["library"]
        expected_name = entry["filename"]
        expected_hash = entry["sha256"]
    except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise ImportError("bundled Shoal library manifest is missing or invalid") from exc
    if path.name != expected_name:
        raise ImportError(f"bundled Shoal library {path.name!r} is not in its manifest")
    actual_hash = hashlib.sha256(path.read_bytes()).hexdigest()
    if actual_hash != expected_hash:
        raise ImportError("bundled Shoal library checksum does not match its manifest")


def _bytes(value: Bytes) -> bytes:
    if not value.data or value.length == 0:
        return b""
    return C.string_at(value.data, value.length)


class NativeAPI:
    def __init__(self, library: str | os.PathLike[str] | None = None) -> None:
        _ensure_process_safe()
        failures: list[str] = []
        raw_library: C.CDLL
        self.path = ""
        self._dll_directory = None
        for candidate in library_candidates(library):
            try:
                candidate_path = Path(candidate)
                if candidate_path.is_absolute():
                    _verify_bundled_library(candidate_path)
                    if sys.platform == "win32" and candidate_path.parent.exists():
                        self._dll_directory = os.add_dll_directory(
                            str(candidate_path.parent)
                        )
                raw_library = C.CDLL(candidate)
                self.path = candidate
                break
            except (ImportError, OSError) as exc:
                failures.append(f"{candidate}: {exc}")
        else:
            raise ImportError(
                "unable to load the Shoal shared library; set SHOAL_LIBRARY. "
                + " | ".join(failures[-3:])
            )
        self.lib = _ProcessBoundLibrary(raw_library)
        self._symbols: set[str] = set()
        self._bind_discovery()
        major = int(self.lib.shoal_abi_version_major())
        if major != ABI_MAJOR:
            raise ImportError(f"unsupported Shoal ABI major {major}; expected {ABI_MAJOR}")
        self.version = (
            major,
            int(self.lib.shoal_abi_version_minor()),
            int(self.lib.shoal_abi_version_patch()),
        )
        if self.version < MIN_ABI_VERSION:
            raise ImportError(
                "Shoal ABI "
                + ".".join(map(str, self.version))
                + " is older than required "
                + ".".join(map(str, MIN_ABI_VERSION))
            )
        packed = int(self.lib.shoal_abi_version_packed())
        expected_packed = (major << 16) | (self.version[1] << 8) | self.version[2]
        if packed != expected_packed:
            raise ImportError("Shoal ABI version queries are inconsistent")
        count = int(self.lib.shoal_abi_capability_count())
        self.capabilities = frozenset(
            capability
            for capability in range(count)
            if self.lib.shoal_abi_has_capability(capability)
        )
        self._bind()
        self.info = RuntimeInfo(self.path, self.version, self.capabilities)

    def _function(
        self, name: str, restype: object, *argtypes: object, required: bool = True
    ) -> bool:
        try:
            fn = getattr(self.lib, name)
        except AttributeError as exc:
            if required:
                raise ImportError(f"Shoal library lacks required symbol {name}") from exc
            return False
        fn.restype = restype
        fn.argtypes = list(argtypes)
        self._symbols.add(name)
        return True

    def _bind_discovery(self) -> None:
        self._function("shoal_abi_version_major", C.c_uint32)
        self._function("shoal_abi_version_minor", C.c_uint32)
        self._function("shoal_abi_version_patch", C.c_uint32)
        self._function("shoal_abi_version_packed", C.c_uint32)
        self._function("shoal_abi_capability_count", C.c_uint32)
        self._function("shoal_abi_has_capability", C.c_uint8, C.c_uint32)

    def _bind(self) -> None:
        P = C.c_void_p
        PP = C.POINTER(P)
        self._function("shoal_connector_config_init", None, C.POINTER(ConnectorConfig))
        self._function("shoal_connector_create", C.c_int32, C.POINTER(ConnectorConfig), PP, PP)
        self._function("shoal_connector_close", C.c_int32, P, PP)
        self._function("shoal_connector_free", None, PP)
        optional = {"required": False}
        self._function(
            "shoal_zookeeper_resolve_instance",
            C.c_int32,
            C.c_char_p,
            C.c_char_p,
            C.c_int64,
            C.c_int64,
            C.c_char_p,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_connector_identity_view_init",
            None,
            C.POINTER(ConnectorIdentityView),
            **optional,
        )
        self._function(
            "shoal_connector_identity_get",
            C.c_int32,
            P,
            C.POINTER(ConnectorIdentityView),
            PP,
            **optional,
        )
        self._function("shoal_connector_identity_free", None, PP, **optional)
        self._function(
            "shoal_batch_writer_config_init",
            None,
            C.POINTER(BatchWriterConfig),
            **optional,
        )
        self._function(
            "shoal_batch_writer_size",
            C.c_int32,
            P,
            C.c_int64,
            C.POINTER(C.c_size_t),
            PP,
            **optional,
        )
        self._function(
            "shoal_logging_set_level", C.c_int32, C.c_int32, PP, **optional
        )
        self._function("shoal_logging_get_level", C.c_int32, **optional)
        self._function(
            "shoal_logging_set_callback",
            C.c_int32,
            LogCallback,
            P,
            PP,
            **optional,
        )
        self._function(
            "shoal_client_config_init", None, C.POINTER(ClientConfig), **optional
        )
        self._function("shoal_range_init", None, C.POINTER(Range), **optional)
        self._function(
            "shoal_client_create",
            C.c_int32,
            C.POINTER(ClientConfig),
            PP,
            PP,
            **optional,
        )
        self._function("shoal_client_close", C.c_int32, P, PP, **optional)
        self._function("shoal_client_free", None, PP, **optional)
        self._function(
            "shoal_client_set_threads", C.c_int32, P, C.c_int32, PP, **optional
        )
        self._function(
            "shoal_client_set_table", C.c_int32, P, C.c_char_p, PP, **optional
        )
        self._function(
            "shoal_client_list_tables", C.c_int32, P, C.c_int64, PP, PP, **optional
        )
        self._function(
            "shoal_client_select_column",
            C.c_int32,
            P,
            Bytes,
            C.POINTER(Bytes),
            PP,
            **optional,
        )
        self._function(
            "shoal_client_scan_range",
            C.c_int32,
            P,
            C.POINTER(Range),
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_scanner_config_init",
            None,
            C.POINTER(ScannerConfig),
            **optional,
        )
        self._function(
            "shoal_connector_create_batch_scanner",
            C.c_int32,
            P,
            C.POINTER(ScannerConfig),
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_batch_scanner_close", C.c_int32, P, PP, **optional
        )
        self._function("shoal_batch_scanner_free", None, PP, **optional)
        self._function(
            "shoal_batch_scanner_stream",
            C.c_int32,
            P,
            C.POINTER(Range),
            C.c_size_t,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_scan_cursor_next",
            C.c_int32,
            P,
            C.c_size_t,
            PP,
            C.POINTER(C.c_uint8),
            PP,
            **optional,
        )
        self._function("shoal_scan_cursor_close", C.c_int32, P, PP, **optional)
        self._function("shoal_scan_cursor_free", None, PP, **optional)
        self._function("shoal_scan_result_count", C.c_size_t, P, **optional)
        self._function(
            "shoal_scan_result_get",
            C.c_int32,
            P,
            C.c_size_t,
            C.POINTER(KeyValueView),
            PP,
            **optional,
        )
        self._function("shoal_scan_result_free", None, PP, **optional)
        self._function("shoal_bytes_result_get", Bytes, P, **optional)
        self._function("shoal_bytes_result_free", None, PP, **optional)
        self._function(
            "shoal_rfile_writer_config_init",
            None,
            C.POINTER(RFileWriterConfig),
            **optional,
        )
        self._function("shoal_rfile_entry_init", None, C.POINTER(RFileEntry), **optional)
        self._function(
            "shoal_rfile_entry_view_init",
            None,
            C.POINTER(RFileEntryView),
            **optional,
        )
        self._function(
            "shoal_rfile_writer_create",
            C.c_int32,
            C.c_char_p,
            C.POINTER(RFileWriterConfig),
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_rfile_writer_append",
            C.c_int32,
            P,
            C.POINTER(RFileEntry),
            C.c_int64,
            PP,
            **optional,
        )
        self._function(
            "shoal_rfile_writer_add_locality_group",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_int64,
            PP,
            **optional,
        )
        self._function("shoal_rfile_writer_close", C.c_int32, P, PP, **optional)
        self._function("shoal_rfile_writer_free", None, PP, **optional)
        self._function(
            "shoal_rfile_reader_open_sequential",
            C.c_int32,
            C.c_char_p,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function("shoal_rfile_reader_has_top", C.c_uint8, P, **optional)
        self._function("shoal_rfile_reader_top", C.c_int32, P, PP, PP, **optional)
        self._function(
            "shoal_rfile_entry_result_get",
            C.c_int32,
            P,
            C.POINTER(RFileEntryView),
            PP,
            **optional,
        )
        self._function("shoal_rfile_entry_result_free", None, PP, **optional)
        self._function(
            "shoal_rfile_reader_next",
            C.c_int32,
            P,
            C.c_int64,
            PP,
            **optional,
        )
        self._function("shoal_rfile_reader_close", C.c_int32, P, PP, **optional)
        self._function("shoal_rfile_reader_free", None, PP, **optional)
        self._function(
            "shoal_hdfs_dir_entry_view_init",
            None,
            C.POINTER(HDFSDirEntryView),
            **optional,
        )
        self._function(
            "shoal_hdfs_client_create",
            C.c_int32,
            C.c_char_p,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function("shoal_hdfs_client_close", C.c_int32, P, PP, **optional)
        self._function("shoal_hdfs_client_free", None, PP, **optional)
        self._function(
            "shoal_hdfs_client_open",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_create_file",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_list",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_stat",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_remove",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_uint8,
            C.c_int64,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_rename",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_char_p,
            C.c_int64,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_mkdir",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_int64,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_client_chown",
            C.c_int32,
            P,
            C.c_char_p,
            C.c_char_p,
            C.c_char_p,
            C.c_int64,
            PP,
            **optional,
        )
        self._function(
            "shoal_hdfs_input_stream_read",
            C.c_int32,
            P,
            C.c_size_t,
            C.c_int64,
            PP,
            PP,
            **optional,
        )
        self._function("shoal_hdfs_input_stream_close", C.c_int32, P, PP, **optional)
        self._function("shoal_hdfs_input_stream_free", None, PP, **optional)
        self._function(
            "shoal_hdfs_output_stream_write",
            C.c_int32,
            P,
            Bytes,
            C.c_int64,
            C.POINTER(C.c_size_t),
            PP,
            **optional,
        )
        self._function("shoal_hdfs_output_stream_close", C.c_int32, P, PP, **optional)
        self._function("shoal_hdfs_output_stream_free", None, PP, **optional)
        self._function("shoal_hdfs_dir_list_count", C.c_size_t, P, **optional)
        self._function(
            "shoal_hdfs_dir_list_get",
            C.c_int32,
            P,
            C.c_size_t,
            C.POINTER(HDFSDirEntryView),
            PP,
            **optional,
        )
        self._function("shoal_hdfs_dir_list_free", None, PP, **optional)
        self._function(
            "shoal_hdfs_dir_entry_result_get",
            C.c_int32,
            P,
            C.POINTER(HDFSDirEntryView),
            PP,
            **optional,
        )
        self._function("shoal_hdfs_dir_entry_result_free", None, PP, **optional)
        self._function("shoal_mutation_create", C.c_int32, Bytes, PP, PP, **optional)
        self._function(
            "shoal_mutation_put",
            C.c_int32,
            P,
            Bytes,
            Bytes,
            Bytes,
            C.c_int64,
            Bytes,
            PP,
            **optional,
        )
        self._function(
            "shoal_mutation_delete",
            C.c_int32,
            P,
            Bytes,
            Bytes,
            Bytes,
            C.c_int64,
            PP,
            **optional,
        )
        self._function(
            "shoal_mutation_put_latest",
            C.c_int32,
            P,
            Bytes,
            Bytes,
            Bytes,
            Bytes,
            PP,
            **optional,
        )
        self._function(
            "shoal_mutation_delete_latest",
            C.c_int32,
            P,
            Bytes,
            Bytes,
            Bytes,
            PP,
            **optional,
        )
        self._function("shoal_mutation_size", C.c_int32, P, C.POINTER(C.c_size_t), PP, **optional)
        self._function("shoal_mutation_free", None, PP, **optional)
        self._function(
            "shoal_connector_create_batch_writer",
            C.c_int32,
            P,
            C.POINTER(BatchWriterConfig),
            PP,
            PP,
            **optional,
        )
        for name in ("shoal_batch_writer_add",):
            self._function(name, C.c_int32, P, P, C.c_int64, PP, PP, **optional)
        for name in ("shoal_batch_writer_flush", "shoal_batch_writer_close"):
            self._function(name, C.c_int32, P, C.c_int64, PP, PP, **optional)
        self._function("shoal_batch_writer_free", None, PP, **optional)
        self._function("shoal_write_failure_get_flags", C.c_uint32, P, **optional)
        for stem, view in (
            ("failed_extent", FailedExtentView),
            ("constraint", ConstraintViolationView),
            ("authorization", AuthorizationFailureView),
            ("cleanup", CleanupFailureView),
        ):
            self._function(
                f"shoal_write_failure_{stem}_count", C.c_size_t, P, **optional
            )
            self._function(
                f"shoal_write_failure_get_{stem}",
                C.c_int32,
                P,
                C.c_size_t,
                C.POINTER(view),
                PP,
                **optional,
            )
        self._function("shoal_write_failure_free", None, PP, **optional)
        self._bind_admin(P, PP, optional)
        self._function("shoal_error_code", C.c_int32, P)
        self._function("shoal_error_message", C.c_char_p, P)
        self._function("shoal_error_free", None, PP)
        self._function("shoal_error_source", C.c_int32, P, **optional)
        self._function("shoal_error_source_name", C.c_char_p, P, **optional)
        if hasattr(self.lib, "shoal_error_compatibility"):
            self._function("shoal_error_compatibility", C.c_int32, P)
        self._function(
            "shoal_error_compatibility_code", C.c_int16, P, **optional
        )

    def _bind_admin(self, P: object, PP: object, optional: dict[str, bool]) -> None:
        i32, i64, u8, size = C.c_int32, C.c_int64, C.c_uint8, C.c_size_t
        cp, bp = C.c_char_p, C.POINTER(Bytes)
        for prefix, view in (
            ("table", TableView),
            ("namespace", NamespaceView),
        ):
            self._function(f"shoal_{prefix}_list_count", size, P, **optional)
            self._function(
                f"shoal_{prefix}_list_get", i32, P, size, C.POINTER(view), PP, **optional
            )
            self._function(f"shoal_{prefix}_list_free", None, PP, **optional)
        for prefix in ("table", "namespace"):
            self._function(f"shoal_{prefix}_properties_count", size, P, **optional)
            self._function(
                f"shoal_{prefix}_properties_get",
                i32,
                P,
                size,
                C.POINTER(PropertyView),
                PP,
                **optional,
            )
            self._function(f"shoal_{prefix}_properties_free", None, PP, **optional)
        self._function("shoal_bytes_list_count", size, P, **optional)
        self._function("shoal_bytes_list_get", i32, P, size, bp, PP, **optional)
        self._function("shoal_bytes_list_free", None, PP, **optional)
        self._function("shoal_connector_list_tables", i32, P, i64, PP, PP, **optional)
        self._function("shoal_connector_table_exists", i32, P, cp, i64, C.POINTER(u8), PP, **optional)
        for name in ("create", "delete"):
            self._function(f"shoal_connector_{name}_table", i32, P, cp, i64, PP, **optional)
        self._function("shoal_connector_rename_table", i32, P, cp, cp, i64, PP, **optional)
        self._function("shoal_connector_flush_table", i32, P, cp, u8, i64, PP, **optional)
        self._function("shoal_connector_flush_table_range", i32, P, cp, bp, bp, u8, i64, PP, **optional)
        for name in ("set", "remove"):
            args = (P, cp, cp, cp, i64, PP) if name == "set" else (P, cp, cp, i64, PP)
            self._function(f"shoal_connector_{name}_table_property", i32, *args, **optional)
        self._function("shoal_connector_effective_table_properties", i32, P, cp, i64, PP, PP, **optional)
        self._function("shoal_connector_list_table_splits", i32, P, cp, i64, PP, PP, **optional)
        self._function("shoal_connector_add_table_splits", i32, P, cp, bp, size, i64, PP, **optional)
        self._function("shoal_table_constraint_view_init", None, C.POINTER(ConstraintView), **optional)
        self._function("shoal_connector_add_table_constraint", i32, P, cp, cp, i64, C.POINTER(i32), PP, **optional)
        self._function("shoal_connector_list_table_constraints", i32, P, cp, i64, PP, PP, **optional)
        self._function("shoal_table_constraint_list_count", size, P, **optional)
        self._function("shoal_table_constraint_list_get", i32, P, size, C.POINTER(ConstraintView), PP, **optional)
        self._function("shoal_table_constraint_list_free", None, PP, **optional)
        self._function("shoal_connector_remove_table_constraint", i32, P, cp, i32, i64, PP, **optional)
        self._function("shoal_connector_list_namespaces", i32, P, i64, PP, PP, **optional)
        self._function("shoal_connector_namespace_exists", i32, P, cp, i64, C.POINTER(u8), PP, **optional)
        for name in ("create", "delete"):
            self._function(f"shoal_connector_{name}_namespace", i32, P, cp, i64, PP, **optional)
        self._function("shoal_connector_rename_namespace", i32, P, cp, cp, i64, PP, **optional)
        for name in ("set", "remove"):
            args = (P, cp, cp, cp, i64, PP) if name == "set" else (P, cp, cp, i64, PP)
            self._function(f"shoal_connector_{name}_namespace_property", i32, *args, **optional)
        for name in ("effective_namespace_properties", "namespace_properties"):
            self._function(f"shoal_connector_{name}", i32, P, cp, i64, PP, PP, **optional)
        self._function(
            "shoal_connector_versioned_namespace_properties",
            i32,
            P,
            cp,
            i64,
            PP,
            PP,
            **optional,
        )
        self._function("shoal_versioned_properties_version", i64, P, **optional)
        self._function("shoal_versioned_properties_count", size, P, **optional)
        self._function(
            "shoal_versioned_properties_get",
            i32,
            P,
            size,
            C.POINTER(PropertyView),
            PP,
            **optional,
        )
        self._function("shoal_versioned_properties_free", None, PP, **optional)
        self._function("shoal_connector_create_user", i32, P, cp, bp, i64, PP, **optional)
        self._function("shoal_connector_drop_user", i32, P, cp, i64, PP, **optional)
        self._function("shoal_connector_change_password", i32, P, cp, bp, i64, PP, **optional)
        self._function("shoal_connector_change_user_authorizations", i32, P, cp, bp, size, i64, PP, **optional)
        self._function("shoal_connector_get_user_authorizations", i32, P, cp, i64, PP, PP, **optional)
        for scope in ("system", "table", "namespace"):
            middle = (cp,) if scope == "system" else (cp, cp)
            self._function(f"shoal_connector_has_{scope}_permission", i32, P, *middle, C.c_int8, i64, C.POINTER(u8), PP, **optional)
            for action in ("grant", "revoke"):
                self._function(f"shoal_connector_{action}_{scope}_permission", i32, P, *middle, C.c_int8, i64, PP, **optional)

    def require(self, *capabilities: int) -> None:
        missing_capabilities = [
            str(cap) for cap in capabilities if cap not in self.capabilities
        ]
        missing_symbols = sorted(
            symbol
            for capability in capabilities
            for symbol in CAPABILITY_SYMBOLS.get(capability, ())
            if symbol not in self._symbols
        )
        if missing_capabilities or missing_symbols:
            details = []
            if missing_capabilities:
                details.append("capabilities " + ", ".join(missing_capabilities))
            if missing_symbols:
                details.append("symbols " + ", ".join(missing_symbols))
            raise NotImplementedError(
                "loaded Shoal library lacks required ABI " + "; ".join(details)
            )

    def check(self, status: int, error: C.c_void_p) -> None:
        if status == 0:
            return
        message = f"Shoal operation failed with status {status}"
        compatibility: int | None = None
        compatibility_code = -1
        source_class: int | None = None
        source_name: str | None = None
        try:
            if error.value:
                raw = self.lib.shoal_error_message(error)
                if raw:
                    message = raw.decode("utf-8", "replace")
                if CAP_COMPATIBILITY_ERRORS in self.capabilities and hasattr(
                    self.lib, "shoal_error_compatibility"
                ):
                    compatibility = int(self.lib.shoal_error_compatibility(error))
                if hasattr(self.lib, "shoal_error_compatibility_code"):
                    compatibility_code = int(
                        self.lib.shoal_error_compatibility_code(error)
                    )
                if hasattr(self.lib, "shoal_error_source"):
                    source_class = int(self.lib.shoal_error_source(error))
                if hasattr(self.lib, "shoal_error_source_name"):
                    raw_source = self.lib.shoal_error_source_name(error)
                    if raw_source:
                        source_name = raw_source.decode("utf-8", "replace")
        finally:
            if error.value:
                self.lib.shoal_error_free(C.byref(error))
        exc = exception_for_status(
            status,
            message,
            compatibility_class=compatibility,
            compatibility_code=compatibility_code,
            source_class=source_class,
            source_name=source_name,
        )
        if source_class is not None and source_name is not None:
            raise exc from NativeErrorContext(status, source_class, source_name)
        raise exc

    def check_write(
        self, status: int, failure: C.c_void_p, error: C.c_void_p
    ) -> None:
        details: dict[str, object] | None = None
        try:
            if failure.value:
                details = self.copy_write_failure(failure)
            self.check(status, error)
        except BaseException as exc:
            if details is not None:
                setattr(exc, "write_failure", details)
                setattr(exc, "write_failure_flags", details["flags"])
            raise
        finally:
            if failure.value:
                self.lib.shoal_write_failure_free(C.byref(failure))

    def copy_write_failure(self, failure: C.c_void_p) -> dict[str, object]:
        def text(value: bytes | None) -> str:
            return value.decode("utf-8", "surrogateescape") if value else ""

        def collect(stem: str, view_type: type[C.Structure], convert: object) -> list[object]:
            values: list[object] = []
            count = getattr(self.lib, f"shoal_write_failure_{stem}_count")(failure)
            getter = getattr(self.lib, f"shoal_write_failure_get_{stem}")
            for index in range(count):
                view = view_type()
                item_error = C.c_void_p()
                status = getter(failure, index, C.byref(view), C.byref(item_error))
                self.check(status, item_error)
                values.append(convert(view))
            return values

        def extent(view: FailedExtentView) -> dict[str, object]:
            return {
                "server": text(view.server),
                "table_id": text(view.table_id),
                "prev_row": _bytes(view.prev_row) if view.has_prev_row else None,
                "end_row": _bytes(view.end_row) if view.has_end_row else None,
                "submitted": int(view.submitted),
                "committed": int(view.committed),
            }

        def violation(view: ConstraintViolationView) -> dict[str, object]:
            return {
                "server": text(view.server),
                "constraint_class": text(view.constraint_class),
                "violation_code": int(view.violation_code),
                "description": text(view.description),
                "violating_mutation_count": int(view.violating_mutation_count),
            }

        def authorization(view: AuthorizationFailureView) -> dict[str, object]:
            return {
                "server": text(view.server),
                "table_id": text(view.table_id),
                "prev_row": _bytes(view.prev_row) if view.has_prev_row else None,
                "end_row": _bytes(view.end_row) if view.has_end_row else None,
                "code": text(view.code),
            }

        def cleanup(view: CleanupFailureView) -> dict[str, object]:
            return {"server": text(view.server), "message": text(view.message)}

        return {
            "flags": int(self.lib.shoal_write_failure_get_flags(failure)),
            "failed_extents": collect("failed_extent", FailedExtentView, extent),
            "constraint_violations": collect(
                "constraint", ConstraintViolationView, violation
            ),
            "authorization_failures": collect(
                "authorization", AuthorizationFailureView, authorization
            ),
            "cleanup_failures": collect("cleanup", CleanupFailureView, cleanup),
        }

    @staticmethod
    def copy_view(view: KeyValueView) -> tuple[tuple[bytes, bytes, bytes, bytes, int], bytes]:
        return (
            (
                _bytes(view.row),
                _bytes(view.column_family),
                _bytes(view.column_qualifier),
                _bytes(view.column_visibility),
                int(view.timestamp),
            ),
            _bytes(view.value),
        )


def as_bytes(value: str | bytes | bytearray | memoryview) -> bytes:
    return value.encode() if isinstance(value, str) else bytes(value)


def c_bytes(value: bytes) -> tuple[Bytes, object]:
    if not value:
        return Bytes(None, 0), None
    buffer = (C.c_uint8 * len(value)).from_buffer_copy(value)
    return Bytes(C.cast(buffer, C.POINTER(C.c_uint8)), len(value)), buffer

