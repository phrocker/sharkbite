from __future__ import annotations

import ctypes as C
import struct
from dataclasses import dataclass

from ._native import (
    CAP_HDFS,
    CAP_RFILE,
    CAP_RFILE_LOCALITY_GROUPS,
    HDFSDirEntryView,
    NativeAPI,
    RFileEntry,
    RFileEntryView,
    RFileWriterConfig,
    as_bytes,
    c_bytes,
)
from .data import Key, KeyValue


def _timeout_ms(timeout: float | None) -> int:
    return 0 if timeout is None else max(1, int(timeout * 1000))


@dataclass(frozen=True)
class HdfsDirEnt:
    name: str
    owner: str
    group: str
    size: int
    modification_time_ms: int = 0
    mode: int = 0
    is_directory: bool = False

    def getName(self) -> str:
        return self.name

    def getOwner(self) -> str:
        return self.owner

    def getGroup(self) -> str:
        return self.group

    def getSize(self) -> int:
        return self.size

    def __str__(self) -> str:
        return f"{self.owner} {self.group} {self.size} {self.name}"


def _dir_entry(api: NativeAPI, result: C.c_void_p) -> HdfsDirEnt:
    view = HDFSDirEntryView()
    api.lib.shoal_hdfs_dir_entry_view_init(C.byref(view))
    error = C.c_void_p()
    status = api.lib.shoal_hdfs_dir_entry_result_get(
        result, C.byref(view), C.byref(error)
    )
    api.check(status, error)
    return _dir_entry_view(view)


def _dir_entry_view(view: HDFSDirEntryView) -> HdfsDirEnt:
    return HdfsDirEnt(
        (view.name or b"").decode(),
        (view.owner or b"").decode(),
        (view.group or b"").decode(),
        int(view.size),
        int(view.modification_time_ms),
        int(view.mode),
        bool(view.is_directory),
    )


class Hdfs:
    def __init__(
        self,
        namenode: str = "",
        port: int | None = None,
        *,
        timeout: float | None = None,
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> None:
        self._api = _api or NativeAPI(library)
        self._api.require(CAP_HDFS)
        address = namenode
        if port is not None and namenode and "://" not in namenode:
            address = f"{namenode}:{port}"
        self._handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_create(
            address.encode(), _timeout_ms(timeout), C.byref(self._handle), C.byref(error)
        )
        self._api.check(status, error)
        self._closed = False

    def read(self, path: str, *, timeout: float | None = None) -> HdfsInputStream:
        handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_open(
            self._handle,
            path.encode(),
            _timeout_ms(timeout),
            C.byref(handle),
            C.byref(error),
        )
        self._api.check(status, error)
        return HdfsInputStream(self._api, handle)

    def write(self, path: str, *, timeout: float | None = None) -> HdfsOutputStream:
        handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_create_file(
            self._handle,
            path.encode(),
            _timeout_ms(timeout),
            C.byref(handle),
            C.byref(error),
        )
        self._api.check(status, error)
        return HdfsOutputStream(self._api, handle)

    def list(self, path: str, *, timeout: float | None = None) -> list[HdfsDirEnt]:
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_list(
            self._handle,
            path.encode(),
            _timeout_ms(timeout),
            C.byref(result),
            C.byref(error),
        )
        self._api.check(status, error)
        try:
            entries: list[HdfsDirEnt] = []
            for index in range(self._api.lib.shoal_hdfs_dir_list_count(result)):
                view = HDFSDirEntryView()
                self._api.lib.shoal_hdfs_dir_entry_view_init(C.byref(view))
                error = C.c_void_p()
                status = self._api.lib.shoal_hdfs_dir_list_get(
                    result, index, C.byref(view), C.byref(error)
                )
                self._api.check(status, error)
                entries.append(_dir_entry_view(view))
            return entries
        finally:
            self._api.lib.shoal_hdfs_dir_list_free(C.byref(result))

    def stat(self, path: str, *, timeout: float | None = None) -> HdfsDirEnt:
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_stat(
            self._handle,
            path.encode(),
            _timeout_ms(timeout),
            C.byref(result),
            C.byref(error),
        )
        self._api.check(status, error)
        try:
            return _dir_entry(self._api, result)
        finally:
            self._api.lib.shoal_hdfs_dir_entry_result_free(C.byref(result))

    def remove(
        self, path: str, recursive: bool = False, *, timeout: float | None = None
    ) -> None:
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_remove(
            self._handle,
            path.encode(),
            int(recursive),
            _timeout_ms(timeout),
            C.byref(error),
        )
        self._api.check(status, error)

    def rename(
        self, old_path: str, new_path: str, *, timeout: float | None = None
    ) -> None:
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_rename(
            self._handle,
            old_path.encode(),
            new_path.encode(),
            _timeout_ms(timeout),
            C.byref(error),
        )
        self._api.check(status, error)

    move = rename

    def mkdir(self, path: str, *, timeout: float | None = None) -> int:
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_mkdir(
            self._handle, path.encode(), _timeout_ms(timeout), C.byref(error)
        )
        self._api.check(status, error)
        return 0

    def chown(
        self,
        path: str,
        owner: str = "",
        group: str = "",
        *,
        timeout: float | None = None,
    ) -> int:
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_chown(
            self._handle,
            path.encode(),
            owner.encode() or None,
            group.encode() or None,
            _timeout_ms(timeout),
            C.byref(error),
        )
        self._api.check(status, error)
        return 0

    def close(self) -> None:
        if self._closed:
            return
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_client_close(
            self._handle, C.byref(error)
        )
        try:
            self._api.check(status, error)
        finally:
            self._api.lib.shoal_hdfs_client_free(C.byref(self._handle))
            self._closed = True

    def __enter__(self) -> Hdfs:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


class HdfsInputStream:
    def __init__(self, api: NativeAPI, handle: C.c_void_p) -> None:
        self._api = api
        self._handle = handle
        self._closed = False

    def readBytes(self, length: int, *, timeout: float | None = None) -> bytes:
        if length < 0:
            raise ValueError("length must not be negative")
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_input_stream_read(
            self._handle,
            length,
            _timeout_ms(timeout),
            C.byref(result),
            C.byref(error),
        )
        self._api.check(status, error)
        try:
            value = self._api.lib.shoal_bytes_result_get(result)
            return C.string_at(value.data, value.length) if value.length else b""
        finally:
            self._api.lib.shoal_bytes_result_free(C.byref(result))

    def readShort(self, *, timeout: float | None = None) -> int:
        return struct.unpack(">h", self._read_exact(2, timeout))[0]

    def readInt(self, *, timeout: float | None = None) -> int:
        return struct.unpack(">i", self._read_exact(4, timeout))[0]

    def readLong(self, *, timeout: float | None = None) -> int:
        return struct.unpack(">q", self._read_exact(8, timeout))[0]

    def readString(self, *, timeout: float | None = None) -> str:
        length = self._read_vlong(timeout)
        if length < 0:
            raise ValueError(f"negative HDFS string length {length}")
        return self._read_exact(length, timeout).decode()

    def _read_exact(self, length: int, timeout: float | None) -> bytes:
        data = bytearray()
        while len(data) < length:
            chunk = self.readBytes(length - len(data), timeout=timeout)
            if not chunk:
                raise EOFError("unexpected end of HDFS stream")
            data.extend(chunk)
        return bytes(data)

    def _read_vlong(self, timeout: float | None) -> int:
        head = struct.unpack("b", self._read_exact(1, timeout))[0]
        if head >= -112:
            return head
        negative = head < -120
        count = -(head + (120 if negative else 112))
        value = int.from_bytes(self._read_exact(count, timeout), "big")
        return ~value if negative else value

    def close(self) -> None:
        if self._closed:
            return
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_input_stream_close(
            self._handle, C.byref(error)
        )
        try:
            self._api.check(status, error)
        finally:
            self._api.lib.shoal_hdfs_input_stream_free(C.byref(self._handle))
            self._closed = True

    def __enter__(self) -> HdfsInputStream:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


class HdfsOutputStream:
    def __init__(self, api: NativeAPI, handle: C.c_void_p) -> None:
        self._api = api
        self._handle = handle
        self._closed = False

    def write(
        self, value: str | bytes, length: int | None = None, *, timeout: float | None = None
    ) -> int:
        data = as_bytes(value)
        if length is not None:
            data = data[:length]
        native, keepalive = c_bytes(data)
        written = C.c_size_t()
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_output_stream_write(
            self._handle,
            native,
            _timeout_ms(timeout),
            C.byref(written),
            C.byref(error),
        )
        self._api.check(status, error)
        _ = keepalive
        return int(written.value)

    def writeShort(self, value: int, *, timeout: float | None = None) -> int:
        return self.write(struct.pack(">h", value), timeout=timeout)

    def writeInt(self, value: int, *, timeout: float | None = None) -> int:
        return self.write(struct.pack(">i", value), timeout=timeout)

    def writeLong(self, value: int, *, timeout: float | None = None) -> int:
        return self.write(struct.pack(">q", value), timeout=timeout)

    def writeString(self, value: str, *, timeout: float | None = None) -> int:
        data = value.encode()
        return self.write(_encode_vlong(len(data)) + data, timeout=timeout)

    def close(self) -> None:
        if self._closed:
            return
        error = C.c_void_p()
        status = self._api.lib.shoal_hdfs_output_stream_close(
            self._handle, C.byref(error)
        )
        try:
            self._api.check(status, error)
        finally:
            self._api.lib.shoal_hdfs_output_stream_free(C.byref(self._handle))
            self._closed = True

    def __enter__(self) -> HdfsOutputStream:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


def _encode_vlong(value: int) -> bytes:
    if -112 <= value <= 127:
        return struct.pack("b", value)
    head = -112
    encoded = value
    if value < 0:
        encoded = ~value
        head = -120
    count = max(1, (encoded.bit_length() + 7) // 8)
    return struct.pack("b", head - count) + encoded.to_bytes(count, "big")


class SequentialRFile:
    def __init__(
        self,
        api: NativeAPI,
        handle: C.c_void_p,
        *,
        writing: bool,
        timeout: float | None = None,
    ) -> None:
        self._api = api
        self._handle = handle
        self._writing = writing
        self._timeout = timeout
        self._closed = False

    def append(self, value: KeyValue) -> bool:
        if not self._writing:
            raise TypeError("RFile is open for reading")
        entry = RFileEntry()
        self._api.lib.shoal_rfile_entry_init(C.byref(entry))
        keepalive: list[object] = []
        for field, data in (
            ("row", value.key.row),
            ("column_family", value.key.column_family),
            ("column_qualifier", value.key.column_qualifier),
            ("column_visibility", value.key.column_visibility),
        ):
            native, buffer = c_bytes(data)
            setattr(entry.key, field, native)
            keepalive.append(buffer)
        native_value, buffer = c_bytes(bytes(value.value))
        entry.value = native_value
        keepalive.append(buffer)
        entry.key.timestamp = value.key.timestamp
        entry.deleted = value.deleted
        error = C.c_void_p()
        status = self._api.lib.shoal_rfile_writer_append(
            self._handle,
            C.byref(entry),
            _timeout_ms(self._timeout),
            C.byref(error),
        )
        self._api.check(status, error)
        return True

    def addLocalityGroup(self, name: str) -> None:
        self._api.require(CAP_RFILE_LOCALITY_GROUPS)
        error = C.c_void_p()
        status = self._api.lib.shoal_rfile_writer_add_locality_group(
            self._handle,
            name.encode(),
            _timeout_ms(self._timeout),
            C.byref(error),
        )
        self._api.check(status, error)

    def hasNext(self) -> bool:
        return bool(self._api.lib.shoal_rfile_reader_has_top(self._handle))

    def getTop(self) -> KeyValue:
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_rfile_reader_top(
            self._handle, C.byref(result), C.byref(error)
        )
        self._api.check(status, error)
        try:
            view = RFileEntryView()
            self._api.lib.shoal_rfile_entry_view_init(C.byref(view))
            error = C.c_void_p()
            status = self._api.lib.shoal_rfile_entry_result_get(
                result, C.byref(view), C.byref(error)
            )
            self._api.check(status, error)
            key = Key(
                _native_bytes(view.key.row),
                _native_bytes(view.key.column_family),
                _native_bytes(view.key.column_qualifier),
                _native_bytes(view.key.column_visibility),
                int(view.key.timestamp),
            )
            return KeyValue(key, _native_bytes(view.value), bool(view.deleted))
        finally:
            self._api.lib.shoal_rfile_entry_result_free(C.byref(result))

    def getTopKey(self) -> Key:
        return self.getTop().key

    def getTopValue(self) -> bytes:
        return self.getTop().value

    def next(self) -> None:
        if not self.hasNext():
            raise StopIteration
        error = C.c_void_p()
        status = self._api.lib.shoal_rfile_reader_next(
            self._handle, _timeout_ms(self._timeout), C.byref(error)
        )
        self._api.check(status, error)

    def close(self) -> None:
        if self._closed:
            return
        error = C.c_void_p()
        close = (
            self._api.lib.shoal_rfile_writer_close
            if self._writing
            else self._api.lib.shoal_rfile_reader_close
        )
        free = (
            self._api.lib.shoal_rfile_writer_free
            if self._writing
            else self._api.lib.shoal_rfile_reader_free
        )
        status = close(self._handle, C.byref(error))
        try:
            self._api.check(status, error)
        finally:
            free(C.byref(self._handle))
            self._closed = True

    def __enter__(self) -> SequentialRFile:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __iter__(self) -> SequentialRFile:
        return self

    def __next__(self) -> KeyValue:
        if not self.hasNext():
            raise StopIteration
        value = self.getTop()
        self.next()
        return value


def _native_bytes(value: object) -> bytes:
    data = getattr(value, "data")
    length = getattr(value, "length")
    return C.string_at(data, length) if length else b""


class RFileOperations:
    @staticmethod
    def openForWrite(
        path: str,
        *,
        codec: str = "",
        block_size: int = 0,
        timeout: float | None = None,
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> SequentialRFile:
        api = _api or NativeAPI(library)
        api.require(CAP_RFILE)
        config = RFileWriterConfig()
        api.lib.shoal_rfile_writer_config_init(C.byref(config))
        codec_bytes = codec.encode()
        config.codec = codec_bytes or None
        config.block_size = block_size
        handle = C.c_void_p()
        error = C.c_void_p()
        status = api.lib.shoal_rfile_writer_create(
            path.encode(),
            C.byref(config),
            _timeout_ms(timeout),
            C.byref(handle),
            C.byref(error),
        )
        api.check(status, error)
        return SequentialRFile(api, handle, writing=True, timeout=timeout)

    @staticmethod
    def sequentialRead(
        path: str,
        *,
        timeout: float | None = None,
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> SequentialRFile:
        api = _api or NativeAPI(library)
        api.require(CAP_RFILE)
        handle = C.c_void_p()
        error = C.c_void_p()
        status = api.lib.shoal_rfile_reader_open_sequential(
            path.encode(),
            _timeout_ms(timeout),
            C.byref(handle),
            C.byref(error),
        )
        api.check(status, error)
        return SequentialRFile(api, handle, writing=False, timeout=timeout)
