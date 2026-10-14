from __future__ import annotations

import ctypes as C
from dataclasses import dataclass
import threading

from ._native import (
    CAP_BATCH_WRITER,
    CAP_MUTATION,
    CAP_STRUCTURED_WRITE_FAILURE,
    CAP_CLIENT_PARITY_CONTROLS,
    BatchWriterConfig,
    NativeAPI,
    as_bytes,
    c_bytes,
)
from .errors import ClientException


@dataclass(frozen=True)
class BatchWriterOptions:
    max_memory_bytes: int = 0
    max_batch_bytes: int = 0
    max_latency_ms: int = 0
    max_write_threads: int = 0
    max_retries: int = 0
    retry_backoff_ms: int = 0
    durability: int = 0


class Mutation:
    def __init__(
        self,
        row: str | bytes | bytearray | memoryview,
        *,
        library: str | None = None,
        _api: NativeAPI | None = None,
    ) -> None:
        self._api = _api or NativeAPI(library)
        self._api.require(CAP_MUTATION)
        row_bytes = as_bytes(row)
        if not row_bytes:
            raise ClientException("mutation row must be non-empty")
        row_view, row_buffer = c_bytes(row_bytes)
        self._handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_mutation_create(
            row_view, C.byref(self._handle), C.byref(error)
        )
        self._api.check(status, error)
        self._closed = False
        _ = row_buffer

    @property
    def closed(self) -> bool:
        return self._closed

    def put(
        self,
        cf: str | bytes = b"",
        cq: str | bytes = b"",
        cv: str | bytes = b"",
        timestamp: int = 0,
        value: str | bytes = b"",
        **aliases: str | bytes,
    ) -> Mutation:
        cf, cq, cv = self._column_aliases(cf, cq, cv, aliases)
        self._update(
            "shoal_mutation_put",
            cf,
            cq,
            cv,
            timestamp,
            value,
        )
        return self

    def put_latest(
        self,
        column_family: str | bytes = b"",
        column_qualifier: str | bytes = b"",
        column_visibility: str | bytes = b"",
        value: str | bytes = b"",
    ) -> Mutation:
        self._update_latest(
            "shoal_mutation_put_latest",
            column_family,
            column_qualifier,
            column_visibility,
            value,
        )
        return self

    def delete(
        self,
        cf: str | bytes = b"",
        cq: str | bytes = b"",
        cv: str | bytes = b"",
        timestamp: int = 0,
        **aliases: str | bytes,
    ) -> Mutation:
        cf, cq, cv = self._column_aliases(cf, cq, cv, aliases)
        self._ensure_open()
        views = [
            c_bytes(as_bytes(value))
            for value in (cf, cq, cv)
        ]
        error = C.c_void_p()
        status = self._api.lib.shoal_mutation_delete(
            self._handle,
            *(item[0] for item in views),
            timestamp,
            C.byref(error),
        )
        self._api.check(status, error)
        return self

    def putDelete(
        self,
        cf: str | bytes = b"",
        cq: str | bytes = b"",
        cv: str | bytes = b"",
        timestamp: int = 0,
        **aliases: str | bytes,
    ) -> Mutation:
        return self.delete(cf, cq, cv, timestamp, **aliases)

    def delete_latest(
        self,
        column_family: str | bytes = b"",
        column_qualifier: str | bytes = b"",
        column_visibility: str | bytes = b"",
    ) -> Mutation:
        self._ensure_open()
        views = [
            c_bytes(as_bytes(value))
            for value in (column_family, column_qualifier, column_visibility)
        ]
        error = C.c_void_p()
        status = self._api.lib.shoal_mutation_delete_latest(
            self._handle, *(item[0] for item in views), C.byref(error)
        )
        self._api.check(status, error)
        return self

    def size(self) -> int:
        self._ensure_open()
        result = C.c_size_t()
        error = C.c_void_p()
        status = self._api.lib.shoal_mutation_size(
            self._handle, C.byref(result), C.byref(error)
        )
        self._api.check(status, error)
        return int(result.value)

    def _update(
        self,
        symbol: str,
        column_family: str | bytes,
        column_qualifier: str | bytes,
        column_visibility: str | bytes,
        timestamp: int,
        value: str | bytes,
    ) -> None:
        self._ensure_open()
        views = [
            c_bytes(as_bytes(item))
            for item in (
                column_family,
                column_qualifier,
                column_visibility,
                value,
            )
        ]
        error = C.c_void_p()
        status = getattr(self._api.lib, symbol)(
            self._handle,
            views[0][0],
            views[1][0],
            views[2][0],
            timestamp,
            views[3][0],
            C.byref(error),
        )
        self._api.check(status, error)

    @staticmethod
    def _column_aliases(
        cf: str | bytes,
        cq: str | bytes,
        cv: str | bytes,
        aliases: dict[str, str | bytes],
    ) -> tuple[str | bytes, str | bytes, str | bytes]:
        supported = {
            "column_family": "cf",
            "column_qualifier": "cq",
            "column_visibility": "cv",
        }
        values = {"cf": cf, "cq": cq, "cv": cv}
        for alias, value in aliases.items():
            target = supported.get(alias)
            if target is None:
                raise TypeError(f"unexpected keyword argument {alias!r}")
            if values[target] not in (b"", ""):
                raise TypeError(f"{target!r} and {alias!r} are mutually exclusive")
            values[target] = value
        return values["cf"], values["cq"], values["cv"]

    def _update_latest(
        self,
        symbol: str,
        column_family: str | bytes,
        column_qualifier: str | bytes,
        column_visibility: str | bytes,
        value: str | bytes,
    ) -> None:
        self._ensure_open()
        views = [
            c_bytes(as_bytes(item))
            for item in (
                column_family,
                column_qualifier,
                column_visibility,
                value,
            )
        ]
        error = C.c_void_p()
        status = getattr(self._api.lib, symbol)(
            self._handle,
            views[0][0],
            views[1][0],
            views[2][0],
            views[3][0],
            C.byref(error),
        )
        self._api.check(status, error)

    def close(self) -> None:
        if not self._closed:
            self._api.lib.shoal_mutation_free(C.byref(self._handle))
            self._closed = True

    def _ensure_open(self) -> None:
        if self._closed:
            raise RuntimeError("mutation is closed")

    def __enter__(self) -> Mutation:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class BatchWriter:
    def __init__(
        self,
        connector: object,
        table: str,
        *,
        options: BatchWriterOptions | None = None,
    ) -> None:
        self._connector = connector
        self._api: NativeAPI = connector._api
        self._api.require(
            CAP_MUTATION, CAP_BATCH_WRITER, CAP_STRUCTURED_WRITE_FAILURE
        )
        config = BatchWriterConfig()
        self._api.lib.shoal_batch_writer_config_init(C.byref(config))
        table_bytes = table.encode()
        config.table_name = table_bytes
        if options:
            for name in options.__dataclass_fields__:
                setattr(config, name, getattr(options, name))
        self._handle = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_create_batch_writer(
            connector._handle, C.byref(config), C.byref(self._handle), C.byref(error)
        )
        self._api.check(status, error)
        self._closed = False
        self._closing = False
        self._active_calls = 0
        self._close_error: BaseException | None = None
        self._condition = threading.Condition()

    @property
    def closed(self) -> bool:
        with self._condition:
            return self._closed

    def mutation(self, row: str | bytes) -> Mutation:
        with self._condition:
            self._ensure_open()
        return Mutation(row, _api=self._api)

    def add_mutation(self, mutation: Mutation, *, timeout_ms: int = 0) -> bool:
        self._begin_call()
        try:
            mutation._ensure_open()
            if mutation.size() == 0:
                return True
            failure = C.c_void_p()
            error = C.c_void_p()
            status = self._api.lib.shoal_batch_writer_add(
                self._handle,
                mutation._handle,
                timeout_ms,
                C.byref(failure),
                C.byref(error),
            )
            self._api.check_write(status, failure, error)
            return True
        finally:
            self._end_call()

    addMutation = add_mutation

    def flush(self, override: bool = False, *, timeout_ms: int = 0) -> bool:
        del override
        self._begin_call()
        try:
            failure = C.c_void_p()
            error = C.c_void_p()
            status = self._api.lib.shoal_batch_writer_flush(
                self._handle, timeout_ms, C.byref(failure), C.byref(error)
            )
            self._api.check_write(status, failure, error)
            return True
        finally:
            self._end_call()

    def size(self, *, timeout_ms: int = 0) -> int:
        self._begin_call()
        try:
            self._api.require(CAP_CLIENT_PARITY_CONTROLS)
            result = C.c_size_t()
            error = C.c_void_p()
            status = self._api.lib.shoal_batch_writer_size(
                self._handle, timeout_ms, C.byref(result), C.byref(error)
            )
            self._api.check(status, error)
            return int(result.value)
        finally:
            self._end_call()

    def close(self, *, timeout_ms: int = 0) -> None:
        with self._condition:
            while self._closing and not self._closed:
                self._condition.wait()
            if self._closed:
                if self._close_error is not None:
                    raise self._close_error
                return
            self._closing = True
            while self._active_calls:
                self._condition.wait()
        failure = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_batch_writer_close(
            self._handle, timeout_ms, C.byref(failure), C.byref(error)
        )
        try:
            self._api.check_write(status, failure, error)
        except BaseException as exc:
            self._close_error = exc
            raise
        finally:
            self._api.lib.shoal_batch_writer_free(C.byref(self._handle))
            with self._condition:
                self._closed = True
                self._closing = False
                self._condition.notify_all()

    def _ensure_open(self) -> None:
        if self._closed or self._closing:
            raise RuntimeError("batch writer is closed")

    def _begin_call(self) -> None:
        with self._condition:
            self._ensure_open()
            self._active_calls += 1

    def _end_call(self) -> None:
        with self._condition:
            self._active_calls -= 1
            if not self._active_calls:
                self._condition.notify_all()

    def __enter__(self) -> BatchWriter:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass
