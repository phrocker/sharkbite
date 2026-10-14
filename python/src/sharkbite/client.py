from __future__ import annotations

import ctypes as C
import asyncio
from collections import deque
import time
from dataclasses import dataclass
import threading
from typing import Callable, Iterator, Sequence
import weakref

from ._native import (
    CAP_HIGH_LEVEL_CLIENT,
    CAP_HIGH_LEVEL_SCANNER,
    CAP_BATCH_SCANNER,
    CAP_OWNED_SCAN_RESULT,
    CAP_STREAMING_SCAN_CURSOR,
    Bytes,
    ClientConfig,
    Column,
    ConnectorConfig,
    IteratorSetting,
    KeyValueView,
    NativeAPI,
    Range,
    ScannerConfig,
    as_bytes,
    c_bytes,
)
from .data import Key
from .compatibility import (
    PythonIterator,
    ScannerOptions,
    unsupported_python_iterator,
    unsupported_scanner_option,
)
from .configuration import AuthInfo, Instance, ZookeeperInstance
from .errors import ClosedError


class _Config:
    def __init__(
        self,
        api: NativeAPI,
        instance: str,
        zookeepers: str,
        username: str,
        password: str | bytes,
        accumulo_version: str = "4.0.0-SNAPSHOT",
        zookeeper_session_timeout_ms: int = 0,
    ) -> None:
        if not accumulo_version.startswith("4."):
            raise NotImplementedError(
                "Shoal compatibility targets Accumulo 4 only (SB-DIV-001)"
            )
        self.api = api
        self.config = ConnectorConfig()
        api.lib.shoal_connector_config_init(C.byref(self.config))
        self._keepalive: list[object] = []
        self.config.bootstrap = 2
        self.config.instance_name = self._string(instance)
        self.config.zookeeper_servers = self._string(zookeepers)
        self.config.principal = self._string(username)
        password_bytes = as_bytes(password)
        password_view, password_buffer = c_bytes(password_bytes)
        self.config.password = password_view.data
        self.config.password_length = password_view.length
        self._keepalive.append(password_buffer)
        self.config.accumulo_version = self._string(accumulo_version)
        self.config.zookeeper_session_timeout_ms = zookeeper_session_timeout_ms

    def _string(self, value: str) -> bytes:
        encoded = value.encode()
        self._keepalive.append(encoded)
        return encoded


class Connector:
    def __init__(
        self,
        instance: str | AuthInfo,
        zookeepers: str | Instance,
        username: str | None = None,
        password: str | bytes | None = None,
        *,
        accumulo_version: str = "4.0.0-SNAPSHOT",
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> None:
        self._api = _api or NativeAPI(library)
        self._api.require(0, 1, 2)
        if isinstance(instance, AuthInfo):
            if not isinstance(zookeepers, ZookeeperInstance):
                raise TypeError("AuthInfo construction requires a ZookeeperInstance")
            if username is not None or password is not None:
                raise TypeError(
                    "username and password are not accepted with AuthInfo"
                )
            auth = instance
            zk_instance = zookeepers
            if auth.getInstanceId() != zk_instance.getInstanceId():
                raise ValueError("AuthInfo instance ID does not match Instance")
            instance_name = zk_instance.getInstanceName()
            zookeeper_servers = zk_instance.getZookeepers()
            username = auth.getUserName()
            password = auth._password_bytes()
            session_timeout_ms = zk_instance.session_timeout_ms
        else:
            if not isinstance(zookeepers, str):
                raise TypeError("zookeepers must be a comma-separated string")
            if username is None or password is None:
                raise TypeError("username and password are required")
            instance_name = instance
            zookeeper_servers = zookeepers
            # Sharkbite's convenience constructor pins 1000 ms.
            session_timeout_ms = 1000
        config = _Config(
            self._api,
            instance_name,
            zookeeper_servers,
            username,
            password,
            accumulo_version,
            session_timeout_ms,
        )
        self._handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_create(
            C.byref(config.config), C.byref(self._handle), C.byref(error)
        )
        self._api.check(status, error)
        self._closed = False

    @property
    def closed(self) -> bool:
        return self._closed

    def close(self) -> None:
        if self._closed:
            return
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_close(self._handle, C.byref(error))
        try:
            self._api.check(status, error)
        finally:
            self._api.lib.shoal_connector_free(C.byref(self._handle))
            self._closed = True

    def __enter__(self) -> Connector:
        self._ensure_open()
        return self

    def __copy__(self) -> Connector:
        raise TypeError("Shoal connector handles cannot be copied")

    def __deepcopy__(self, _: object) -> Connector:
        raise TypeError("Shoal connector handles cannot be copied")

    def mutation(self, row: str | bytes) -> object:
        self._ensure_open()
        from .writer import Mutation
        return Mutation(row, _api=self._api)

    def create_batch_writer(
        self, table: str, *, options: object | None = None
    ) -> object:
        self._ensure_open()
        from .writer import BatchWriter
        return BatchWriter(self, table, options=options)

    def tableOps(self, table: str) -> object:
        self._ensure_open()
        from .admin import TableOperations
        return TableOperations(self, table)

    def namespaceOps(self, nm: str = "") -> object:
        self._ensure_open()
        from .admin import NamespaceOperations
        return NamespaceOperations(self, nm)

    def securityOps(self) -> object:
        self._ensure_open()
        from .admin import SecurityOperations
        return SecurityOperations(self)

    def tableInfo(self) -> object:
        self._ensure_open()
        from .admin import TableInfo
        return TableInfo(self)

    def getStatistics(self) -> None:
        self._ensure_open()
        raise NotImplementedError(
            "Accumulo 4 removed the legacy manager-monitor statistics API; "
            "cluster statistics are unavailable (approved divergence SB-DIV-016)"
        )

    def _ensure_open(self) -> None:
        if self._closed:
            raise ClosedError("connector is closed")

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class Client:
    def __init__(
        self,
        instance: str,
        zookeepers: str,
        username: str,
        password: str | bytes,
        table: str | None = None,
        auths: Sequence[str | bytes] | None = None,
        *,
        threads: int = 10,
        accumulo_version: str = "4.0.0-SNAPSHOT",
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> None:
        self._api = _api or NativeAPI(library)
        self._api.require(
            CAP_HIGH_LEVEL_CLIENT, CAP_HIGH_LEVEL_SCANNER, CAP_OWNED_SCAN_RESULT
        )
        connector = _Config(
            self._api, instance, zookeepers, username, password, accumulo_version
        )
        config = ClientConfig()
        self._api.lib.shoal_client_config_init(C.byref(config))
        config.connector = C.pointer(connector.config)
        keepalive: list[object] = [connector]
        if table is not None:
            table_bytes = table.encode()
            keepalive.append(table_bytes)
            config.table_name = table_bytes
        auth_values = [c_bytes(as_bytes(value)) for value in (auths or ())]
        if auth_values:
            auth_array = (type(auth_values[0][0]) * len(auth_values))(
                *(item[0] for item in auth_values)
            )
            config.authorizations = auth_array
            config.authorization_count = len(auth_values)
            keepalive.extend([auth_array, *(item[1] for item in auth_values)])
        config.thread_count = threads
        self._handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_client_create(
            C.byref(config), C.byref(self._handle), C.byref(error)
        )
        self._api.check(status, error)
        self._closed = False

    @property
    def closed(self) -> bool:
        return self._closed

    def set_threads(self, thread_count: int) -> None:
        error = C.c_void_p()
        status = self._api.lib.shoal_client_set_threads(
            self._handle, thread_count, C.byref(error)
        )
        self._api.check(status, error)

    def set_table(self, table: str) -> None:
        error = C.c_void_p()
        status = self._api.lib.shoal_client_set_table(
            self._handle, table.encode(), C.byref(error)
        )
        self._api.check(status, error)

    def select_column(self, family: str | bytes, qualifier: str | bytes | None = None) -> None:
        family_view, family_buffer = c_bytes(as_bytes(family))
        qualifier_pointer = None
        qualifier_view = None
        qualifier_buffer = None
        if qualifier is not None:
            qualifier_view, qualifier_buffer = c_bytes(as_bytes(qualifier))
            qualifier_pointer = C.byref(qualifier_view)
        error = C.c_void_p()
        status = self._api.lib.shoal_client_select_column(
            self._handle, family_view, qualifier_pointer, C.byref(error)
        )
        self._api.check(status, error)
        _ = (family_buffer, qualifier_buffer)

    def scanner(self) -> Scanner:
        return Scanner(self)

    def list_tables(self, *, timeout_ms: int = 0) -> dict[str, str]:
        from ._native import TableView
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_client_list_tables(
            self._handle, timeout_ms, C.byref(result), C.byref(error)
        )
        self._api.check(status, error)
        tables: dict[str, str] = {}
        try:
            for index in range(self._api.lib.shoal_table_list_count(result)):
                view = TableView()
                item_error = C.c_void_p()
                item_status = self._api.lib.shoal_table_list_get(
                    result, index, C.byref(view), C.byref(item_error)
                )
                self._api.check(item_status, item_error)
                tables[view.name.decode()] = view.id.decode()
        finally:
            self._api.lib.shoal_table_list_free(C.byref(result))
        return tables

    def scan(
        self,
        begin_row: str | bytes,
        end_row: str | bytes | None = None,
        *,
        timeout_ms: int = 0,
    ) -> list[tuple[Key, bytes]]:
        return self.scanner().scan(begin_row, end_row, timeout_ms=timeout_ms)

    def getStatistics(self) -> None:
        raise NotImplementedError(
            "Accumulo 4 removed the legacy manager-monitor statistics API; "
            "cluster statistics are unavailable (approved divergence SB-DIV-016)"
        )

    def close(self) -> None:
        if self._closed:
            return
        error = C.c_void_p()
        status = self._api.lib.shoal_client_close(self._handle, C.byref(error))
        try:
            self._api.check(status, error)
        finally:
            self._api.lib.shoal_client_free(C.byref(self._handle))
            self._closed = True

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class Scanner:
    def __init__(self, client: Client) -> None:
        self._client = client
        self._closed = False

    @property
    def closed(self) -> bool:
        return self._closed

    def select_column(self, family: str | bytes, qualifier: str | bytes | None = None) -> None:
        self._ensure_open()
        self._client.select_column(family, qualifier)

    def setOption(self, option: ScannerOptions | int) -> None:
        raise unsupported_scanner_option(option)

    def removeOption(self, option: ScannerOptions | int) -> None:
        raise unsupported_scanner_option(option)

    def addIterator(self, iterator: object) -> None:
        if isinstance(iterator, PythonIterator):
            raise unsupported_python_iterator()
        raise NotImplementedError(
            "mutable legacy scanner iterator configuration is not supported; "
            "configure Shoal scanner iterators at construction"
        )

    def scan(
        self,
        begin_row: str | bytes,
        end_row: str | bytes | None = None,
        *,
        timeout_ms: int = 0,
    ) -> list[tuple[Key, bytes]]:
        self._ensure_open()
        api = self._client._api
        scan_range = Range()
        api.lib.shoal_range_init(C.byref(scan_range))
        begin_view, begin_buffer = c_bytes(as_bytes(begin_row))
        scan_range.start.kind = 1
        scan_range.start.row = begin_view
        scan_range.start_inclusive = 1
        keepalive: list[object] = [begin_buffer]
        if end_row is not None:
            end_view, end_buffer = c_bytes(as_bytes(end_row))
            scan_range.end.kind = 1
            scan_range.end.row = end_view
            scan_range.end_inclusive = 0
            keepalive.append(end_buffer)
        result = C.c_void_p()
        error = C.c_void_p()
        status = api.lib.shoal_client_scan_range(
            self._client._handle,
            C.byref(scan_range),
            timeout_ms,
            C.byref(result),
            C.byref(error),
        )
        api.check(status, error)
        entries: list[tuple[Key, bytes]] = []
        try:
            for index in range(api.lib.shoal_scan_result_count(result)):
                view = KeyValueView()
                item_error = C.c_void_p()
                item_status = api.lib.shoal_scan_result_get(
                    result, index, C.byref(view), C.byref(item_error)
                )
                api.check(item_status, item_error)
                raw_key, value = api.copy_view(view)
                entries.append((Key(*raw_key), value))
        finally:
            api.lib.shoal_scan_result_free(C.byref(result))
        return entries

    def close(self) -> None:
        self._closed = True

    def _ensure_open(self) -> None:
        if self._closed:
            raise RuntimeError("scanner is closed")

    def __enter__(self) -> Scanner:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


@dataclass(frozen=True)
class _RangeBoundSnapshot:
    kind: int
    row: bytes
    key: tuple[bytes, bytes, bytes, bytes, int]


@dataclass(frozen=True)
class _RangeSnapshot:
    start: _RangeBoundSnapshot
    end: _RangeBoundSnapshot
    start_inclusive: bool
    end_inclusive: bool

    @staticmethod
    def _bytes(value: Bytes) -> bytes:
        if not value.data or not value.length:
            return b""
        return C.string_at(value.data, value.length)

    @classmethod
    def _bound(cls, bound: object) -> _RangeBoundSnapshot:
        key = bound.key
        return _RangeBoundSnapshot(
            int(bound.kind),
            cls._bytes(bound.row),
            (
                cls._bytes(key.row),
                cls._bytes(key.column_family),
                cls._bytes(key.column_qualifier),
                cls._bytes(key.column_visibility),
                int(key.timestamp),
            ),
        )

    @classmethod
    def from_value(cls, value: object) -> _RangeSnapshot:
        converter = getattr(value, "_as_native_range", None)
        if converter is not None:
            value = converter()
        if not isinstance(value, Range):
            raise TypeError("range must be a sharkbite.Range")
        return cls(
            cls._bound(value.start),
            cls._bound(value.end),
            bool(value.start_inclusive),
            bool(value.end_inclusive),
        )

    def native(self, api: NativeAPI) -> tuple[Range, list[object]]:
        result = Range()
        api.lib.shoal_range_init(C.byref(result))
        keepalive: list[object] = []
        for name, snapshot in (("start", self.start), ("end", self.end)):
            bound = getattr(result, name)
            bound.kind = snapshot.kind
            row, row_buffer = c_bytes(snapshot.row)
            bound.row = row
            keepalive.append(row_buffer)
            for field, value in zip(
                (
                    "row",
                    "column_family",
                    "column_qualifier",
                    "column_visibility",
                ),
                snapshot.key[:4],
            ):
                native, buffer = c_bytes(value)
                setattr(bound.key, field, native)
                keepalive.append(buffer)
            bound.key.timestamp = snapshot.key[4]
        result.start_inclusive = self.start_inclusive
        result.end_inclusive = self.end_inclusive
        return result, keepalive


class Results(Iterator[object]):
    _chunk_size = 1024

    def __init__(
        self,
        opener: Callable[[], tuple[C.c_void_p, C.c_void_p]],
        api: NativeAPI,
    ) -> None:
        self._opener = opener
        self._api = api
        self._lock = threading.RLock()
        self._next_lock = threading.Lock()
        self._scanner = C.c_void_p()
        self._cursor = C.c_void_p()
        self._pending: deque[object] = deque()
        self._exhausted = False
        self._closed = False
        self._disposed = False
        self._terminal_error: BaseException | None = None

    def _restart(self) -> None:
        if self._disposed:
            raise RuntimeError("results are closed")
        self._close_native()
        self._scanner, self._cursor = self._opener()
        self._pending.clear()
        self._exhausted = False
        self._closed = False
        self._terminal_error = None

    def __iter__(self) -> Results:
        with self._lock:
            self._restart()
        return self

    def __next__(self) -> object:
        with self._next_lock:
            with self._lock:
                if self._closed:
                    raise StopIteration
                if not self._cursor.value and not self._exhausted:
                    self._restart()
                if self._pending:
                    return self._pending.popleft()
                if self._terminal_error is not None:
                    error = self._terminal_error
                    self._terminal_error = None
                    self._finish()
                    raise error
                if self._exhausted:
                    self._finish()
                    raise StopIteration
            self._pull()
            with self._lock:
                if self._closed:
                    raise StopIteration
                if self._pending:
                    return self._pending.popleft()
                if self._terminal_error is not None:
                    error = self._terminal_error
                    self._terminal_error = None
                    self._finish()
                    raise error
                self._finish()
                raise StopIteration

    def _pull(self) -> None:
        result = C.c_void_p()
        exhausted = C.c_uint8()
        error = C.c_void_p()
        status = self._api.lib.shoal_scan_cursor_next(
            self._cursor,
            self._chunk_size,
            C.byref(result),
            C.byref(exhausted),
            C.byref(error),
        )
        try:
            if result.value:
                from .storage import KeyValue

                for index in range(self._api.lib.shoal_scan_result_count(result)):
                    view = KeyValueView()
                    item_error = C.c_void_p()
                    item_status = self._api.lib.shoal_scan_result_get(
                        result, index, C.byref(view), C.byref(item_error)
                    )
                    self._api.check(item_status, item_error)
                    raw_key, value = self._api.copy_view(view)
                    with self._lock:
                        if not self._closed:
                            self._pending.append(KeyValue(Key(*raw_key), value))
        finally:
            if result.value:
                self._api.lib.shoal_scan_result_free(C.byref(result))
        with self._lock:
            self._exhausted = bool(exhausted.value)
        if status:
            try:
                self._api.check(status, error)
            except BaseException as exc:
                with self._lock:
                    if self._pending:
                        self._terminal_error = exc
                        return
                raise

    def __aiter__(self) -> Results:
        return iter(self)

    async def __anext__(self) -> object:
        def advance() -> tuple[bool, object | None]:
            try:
                return True, self.__next__()
            except StopIteration:
                return False, None

        available, value = await asyncio.to_thread(advance)
        if not available:
            raise StopAsyncIteration
        return value

    def __await__(self) -> Iterator[object]:
        async def ready() -> Results:
            return iter(self)

        return ready().__await__()

    def close(self) -> None:
        with self._lock:
            if self._disposed:
                return
            self._disposed = True
            self._closed = True
            self._close_native()
            self._pending.clear()

    def _finish(self) -> None:
        self._close_native()
        self._pending.clear()

    def _close_native(self) -> None:
        cursor_error: BaseException | None = None
        if self._cursor.value:
            error = C.c_void_p()
            status = self._api.lib.shoal_scan_cursor_close(
                self._cursor, C.byref(error)
            )
            try:
                self._api.check(status, error)
            except BaseException as exc:
                cursor_error = exc
            finally:
                self._api.lib.shoal_scan_cursor_free(C.byref(self._cursor))
        if self._scanner.value:
            error = C.c_void_p()
            status = self._api.lib.shoal_batch_scanner_close(
                self._scanner, C.byref(error)
            )
            try:
                self._api.check(status, error)
            finally:
                self._api.lib.shoal_batch_scanner_free(C.byref(self._scanner))
        if cursor_error is not None:
            raise cursor_error

    def __enter__(self) -> Results:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class BatchScanner:
    def __init__(
        self,
        connector: Connector,
        table: str,
        auths: Sequence[str | bytes],
        threads: int = 10,
    ) -> None:
        if auths is None:
            from .errors import ClientException

            raise ClientException("authorizations must not be None")
        if threads <= 0:
            raise ValueError("threads must be positive")
        self._connector = connector
        self._api = connector._api
        self._api.require(
            CAP_BATCH_SCANNER, CAP_OWNED_SCAN_RESULT, CAP_STREAMING_SCAN_CURSOR
        )
        self._table = table
        self._auths = tuple(as_bytes(value) for value in auths)
        self._threads = threads
        self._ranges: list[_RangeSnapshot] = []
        self._columns: list[tuple[bytes, bytes | None]] = []
        self._iterators: list[tuple[str, str, int]] = []
        self._results: weakref.WeakSet[Results] = weakref.WeakSet()
        self._lock = threading.RLock()
        self._closed = False

    @property
    def closed(self) -> bool:
        with self._lock:
            return self._closed

    def addRange(self, scan_range: object) -> None:
        with self._lock:
            self._ensure_open()
            self._ranges.append(_RangeSnapshot.from_value(scan_range))

    def withRange(self, scan_range: object) -> BatchScanner:
        self.addRange(scan_range)
        return self

    def fetchColumn(
        self, column_family: str | bytes, column_qualifier: str | bytes | None = None
    ) -> None:
        with self._lock:
            self._ensure_open()
            self._columns.append(
                (
                    as_bytes(column_family),
                    None if column_qualifier is None else as_bytes(column_qualifier),
                )
            )

    def addIterator(self, iterator: object) -> None:
        if isinstance(iterator, PythonIterator):
            raise unsupported_python_iterator()
        try:
            name = iterator.getName()
            class_name = iterator.getClass()
            priority = iterator.getPriority()
        except AttributeError as exc:
            raise TypeError("iterator must be an IterInfo or PythonIterator") from exc
        with self._lock:
            self._ensure_open()
            self._iterators.append((str(name), str(class_name), int(priority)))

    def setOption(self, option: ScannerOptions | int) -> None:
        raise unsupported_scanner_option(option)

    def removeOption(self, option: ScannerOptions | int) -> None:
        raise unsupported_scanner_option(option)

    def getResultSet(self, *, timeout_ms: int = 0) -> Results:
        with self._lock:
            self._ensure_open()
            if not self._ranges:
                raise ValueError("at least one range is required")
            snapshot = (
                tuple(self._ranges),
                tuple(self._columns),
                tuple(self._iterators),
            )
        result = Results(
            lambda: self._open(snapshot, timeout_ms),
            self._api,
        )
        with self._lock:
            self._ensure_open()
            self._results.add(result)
        return result

    def _open(
        self,
        snapshot: tuple[
            tuple[_RangeSnapshot, ...],
            tuple[tuple[bytes, bytes | None], ...],
            tuple[tuple[str, str, int], ...],
        ],
        timeout_ms: int,
    ) -> tuple[C.c_void_p, C.c_void_p]:
        ranges, columns, iterators = snapshot
        config = ScannerConfig()
        self._api.lib.shoal_scanner_config_init(C.byref(config))
        keepalive: list[object] = []
        table = self._table.encode()
        keepalive.append(table)
        config.table_name = table
        config.parallelism = self._threads

        auth_values = [c_bytes(value) for value in self._auths]
        if auth_values:
            auth_array = (Bytes * len(auth_values))(*(item[0] for item in auth_values))
            config.authorizations = auth_array
            config.authorization_count = len(auth_values)
            keepalive.extend([auth_array, *(item[1] for item in auth_values)])

        column_values: list[Column] = []
        for family, qualifier in columns:
            family_view, family_buffer = c_bytes(family)
            keepalive.append(family_buffer)
            value = Column()
            value.family = family_view
            if qualifier is not None:
                qualifier_view, qualifier_buffer = c_bytes(qualifier)
                value.qualifier = qualifier_view
                value.has_qualifier = 1
                keepalive.append(qualifier_buffer)
            column_values.append(value)
        if column_values:
            column_array = (Column * len(column_values))(*column_values)
            config.columns = column_array
            config.column_count = len(column_values)
            keepalive.append(column_array)

        iterator_values: list[IteratorSetting] = []
        for name, class_name, priority in iterators:
            native = IteratorSetting()
            name_bytes = name.encode()
            class_bytes = class_name.encode()
            native.name = name_bytes
            native.class_name = class_bytes
            native.priority = priority
            iterator_values.append(native)
            keepalive.extend((name_bytes, class_bytes))
        if iterator_values:
            iterator_array = (IteratorSetting * len(iterator_values))(*iterator_values)
            config.iterators = iterator_array
            config.iterator_count = len(iterator_values)
            keepalive.append(iterator_array)

        scanner = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_create_batch_scanner(
            self._connector._handle,
            C.byref(config),
            C.byref(scanner),
            C.byref(error),
        )
        self._api.check(status, error)

        native_ranges: list[Range] = []
        for scan_range in ranges:
            native, retained = scan_range.native(self._api)
            native_ranges.append(native)
            keepalive.extend(retained)
        range_array = (Range * len(native_ranges))(*native_ranges)
        keepalive.append(range_array)
        cursor = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_batch_scanner_stream(
            scanner,
            range_array,
            len(native_ranges),
            timeout_ms,
            C.byref(cursor),
            C.byref(error),
        )
        try:
            self._api.check(status, error)
        except BaseException:
            close_error = C.c_void_p()
            self._api.lib.shoal_batch_scanner_close(
                scanner, C.byref(close_error)
            )
            self._api.lib.shoal_batch_scanner_free(C.byref(scanner))
            raise
        return scanner, cursor

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
            results = tuple(self._results)
            self._results.clear()
        first_error: BaseException | None = None
        for result in results:
            try:
                result.close()
            except BaseException as exc:
                if first_error is None:
                    first_error = exc
        if first_error is not None:
            raise first_error

    def _ensure_open(self) -> None:
        if self._closed:
            raise RuntimeError("batch scanner is closed")

    def __enter__(self) -> BatchScanner:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class AccumuloBase(Client):
    def list_tables(self, *, timeout_ms: int = 0) -> list[str]:
        return list(super().list_tables(timeout_ms=timeout_ms))


class AccumuloScanner(AccumuloBase):
    def setOption(self, option: ScannerOptions | int) -> None:
        raise unsupported_scanner_option(option)

    def removeOption(self, option: ScannerOptions | int) -> None:
        raise unsupported_scanner_option(option)

    def addIterator(self, iterator: object) -> None:
        if isinstance(iterator, PythonIterator):
            raise unsupported_python_iterator()
        raise NotImplementedError(
            "mutable legacy scanner iterator configuration is not supported"
        )

    def get(self, *_: object, **__: object) -> None:
        raise NotImplementedError(
            "legacy chunked AccumuloIterator is not part of the first Python delivery slice; "
            "use scan()"
        )

    def fetch_range(self, *_: object, **__: object) -> None:
        raise NotImplementedError(
            "legacy mutable scanner ranges are not part of the first Python delivery slice"
        )

    def fetch_ranges(self, *_: object, **__: object) -> None:
        raise NotImplementedError(
            "legacy mutable scanner ranges are not part of the first Python delivery slice"
        )


class AccumuloWriter:
    def __init__(
        self,
        instance: str,
        zookeepers: str,
        username: str,
        password: str | bytes,
        table: str | None = None,
        auths: Sequence[str | bytes] | None = None,
        *,
        threads: int = 10,
        accumulo_version: str = "4.0.0-SNAPSHOT",
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> None:
        del auths
        if not table:
            raise ValueError("table is required")
        from .writer import BatchWriter, BatchWriterOptions
        self._connector = Connector(
            instance,
            zookeepers,
            username,
            password,
            accumulo_version=accumulo_version,
            library=library,
            _api=_api,
        )
        try:
            self._writer = BatchWriter(
                self._connector,
                table,
                options=BatchWriterOptions(max_write_threads=threads),
            )
        except BaseException:
            self._connector.close()
            raise
        self._closed = False

    def put(
        self,
        row: str | bytes,
        cf: str | bytes,
        cq: str | bytes,
        cv: str | bytes | None = None,
        timestamp: int = 0,
        value: str | bytes | None = None,
        *,
        timeout_ms: int = 0,
    ) -> None:
        if timestamp == 0:
            timestamp = int(time.time() * 1000)
        with self._writer.mutation(row) as mutation:
            mutation.put(cf, cq, cv or b"", timestamp, value or b"")
            self._writer.add_mutation(mutation, timeout_ms=timeout_ms)

    def putDelete(
        self,
        row: str | bytes,
        cf: str | bytes,
        cq: str | bytes,
        cv: str | bytes = b"",
        timestamp: int = 0,
        *,
        timeout_ms: int = 0,
    ) -> None:
        with self._writer.mutation(row) as mutation:
            mutation.delete(cf, cq, cv, timestamp)
            self._writer.add_mutation(mutation, timeout_ms=timeout_ms)

    def delete(self, key: Key, *, timeout_ms: int = 0) -> None:
        self.putDelete(
            key.row,
            key.column_family,
            key.column_qualifier,
            key.column_visibility,
            key.timestamp,
            timeout_ms=timeout_ms,
        )

    def flush(self, *, timeout_ms: int = 0) -> bool:
        return self._writer.flush(timeout_ms=timeout_ms)

    def close(self, *, timeout_ms: int = 0) -> None:
        if self._closed:
            return
        writer_error: BaseException | None = None
        try:
            self._writer.close(timeout_ms=timeout_ms)
        except BaseException as exc:
            writer_error = exc
        try:
            self._connector.close()
        except BaseException:
            if writer_error is None:
                raise
        finally:
            self._closed = True
        if writer_error is not None:
            raise writer_error

    def __enter__(self) -> AccumuloWriter:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class AccumuloIterator(Iterator[tuple[Key, bytes]]):
    def __init__(self, *_: object, **__: object) -> None:
        raise NotImplementedError(
            "legacy AccumuloIterator is not part of the first Python delivery slice"
        )

    def __next__(self) -> tuple[Key, bytes]:
        raise StopIteration
