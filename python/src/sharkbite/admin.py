from __future__ import annotations

import ctypes as C
from enum import IntEnum
from typing import Iterable

from .data import Authorizations
from .errors import AlreadyExistsError, ClientException, NotFoundError
from ._native import (
    CAP_NAMESPACE_ADMIN,
    CAP_SECURITY_ADMIN,
    CAP_TABLE_ADMIN,
    CAP_TABLE_MAINTENANCE,
    CAP_TABLE_SPLITS,
    Bytes,
    ConstraintView,
    NamespaceView,
    PropertyView,
    TableView,
    as_bytes,
    c_bytes,
)


class SystemPermissions(IntEnum):
    GRANT = 0
    CREATE_TABLE = 1
    DROP_TABLE = 2
    ALTER_TABLE = 3
    CREATE_USER = 4
    ALTER_USER = 6
    SYSTEM = 7
    CREATE_NAMESPACE = 8
    DROP_NAMESPACE = 9
    ALTER_NAMESPACE = 10


class ShoalSystemPermissions(IntEnum):
    GRANT = 0
    CREATE_TABLE = 1
    DROP_TABLE = 2
    ALTER_TABLE = 3
    CREATE_USER = 4
    DROP_USER = 5
    ALTER_USER = 6
    SYSTEM = 7
    CREATE_NAMESPACE = 8
    DROP_NAMESPACE = 9
    ALTER_NAMESPACE = 10
    OBTAIN_DELEGATION_TOKEN = 11


class TablePermissions(IntEnum):
    READ = 2
    WRITE = 3
    BULK_IMPORT = 4
    ALTER_TABLE = 5
    GRANT = 6
    DROP_TABLE = 7


class ShoalTablePermissions(IntEnum):
    READ = 2
    WRITE = 3
    BULK_IMPORT = 4
    ALTER_TABLE = 5
    GRANT = 6
    DROP_TABLE = 7
    GET_SUMMARIES = 8


class NamespacePermissions(IntEnum):
    READ = 0
    WRITE = 1
    ALTER_NAMESPACE = 2
    GRANT = 3
    ALTER_TABLE = 4
    CREATE_TABLE = 5
    DROP_TABLE = 6
    BULK_IMPORT = 7
    DROP_NAMESPACE = 8

class _Operations:
    def __init__(self, connector: object) -> None:
        self._connector = connector
        self._api = connector._api

    def _call(self, symbol: str, *args: object) -> None:
        error = C.c_void_p()
        status = getattr(self._api.lib, symbol)(
            self._connector._handle, *args, C.byref(error)
        )
        self._api.check(status, error)

    def _bool(self, symbol: str, *args: object) -> bool:
        result = C.c_uint8()
        error = C.c_void_p()
        status = getattr(self._api.lib, symbol)(
            self._connector._handle, *args, C.byref(result), C.byref(error)
        )
        self._api.check(status, error)
        return bool(result.value)

    def _pairs(
        self, symbol: str, count_symbol: str, get_symbol: str, free_symbol: str,
        view_type: type[C.Structure], *args: object
    ) -> list[tuple[str, str]]:
        result = C.c_void_p()
        error = C.c_void_p()
        status = getattr(self._api.lib, symbol)(
            self._connector._handle, *args, C.byref(result), C.byref(error)
        )
        self._api.check(status, error)
        values: list[tuple[str, str]] = []
        try:
            for index in range(getattr(self._api.lib, count_symbol)(result)):
                view = view_type()
                item_error = C.c_void_p()
                item_status = getattr(self._api.lib, get_symbol)(
                    result, index, C.byref(view), C.byref(item_error)
                )
                self._api.check(item_status, item_error)
                first = view.name if hasattr(view, "name") else view.key
                second = view.id if hasattr(view, "id") else view.value
                values.append(
                    (
                        first.decode("utf-8", "surrogateescape"),
                        second.decode("utf-8", "surrogateescape"),
                    )
                )
        finally:
            getattr(self._api.lib, free_symbol)(C.byref(result))
        return values

    def _bytes_list(self, symbol: str, *args: object) -> list[bytes]:
        result = C.c_void_p()
        error = C.c_void_p()
        status = getattr(self._api.lib, symbol)(
            self._connector._handle, *args, C.byref(result), C.byref(error)
        )
        self._api.check(status, error)
        values: list[bytes] = []
        try:
            for index in range(self._api.lib.shoal_bytes_list_count(result)):
                view = Bytes()
                item_error = C.c_void_p()
                item_status = self._api.lib.shoal_bytes_list_get(
                    result, index, C.byref(view), C.byref(item_error)
                )
                self._api.check(item_status, item_error)
                values.append(
                    C.string_at(view.data, view.length) if view.length else b""
                )
        finally:
            self._api.lib.shoal_bytes_list_free(C.byref(result))
        return values


class TableOperations(_Operations):
    def __init__(self, connector: object, table: str) -> None:
        super().__init__(connector)
        self._api.require(CAP_TABLE_ADMIN)
        self.table = table

    def exists(self, createIfNot: bool = False, *, timeout_ms: int = 0) -> bool:
        exists = self._bool(
            "shoal_connector_table_exists", self.table.encode(), timeout_ms
        )
        if not exists and createIfNot:
            self.create(timeout_ms=timeout_ms)
            return True
        return exists

    def create(self, recreate: bool = False, *, timeout_ms: int = 0) -> bool:
        if recreate and self.exists(timeout_ms=timeout_ms):
            self.remove(timeout_ms=timeout_ms)
        try:
            self._call(
                "shoal_connector_create_table", self.table.encode(), timeout_ms
            )
        except (AlreadyExistsError, ClientException) as exc:
            if exc.status == 19:
                return False
            raise
        return True

    def remove(self, *, timeout_ms: int = 0) -> bool:
        try:
            self._call(
                "shoal_connector_delete_table", self.table.encode(), timeout_ms
            )
        except NotFoundError as exc:
            raise ClientException(str(exc), status=exc.status) from exc
        return True

    def rename(self, new_name: str, *, timeout_ms: int = 0) -> bool:
        self._call(
            "shoal_connector_rename_table",
            self.table.encode(),
            new_name.encode(),
            timeout_ms,
        )
        self.table = new_name
        return True

    def flush(
        self,
        startRow: str | bytes | None = None,
        endRow: str | bytes | None = None,
        wait: bool = True,
        *,
        timeout_ms: int = 0,
    ) -> int:
        if startRow is None and endRow is None:
            self._call(
                "shoal_connector_flush_table",
                self.table.encode(),
                int(wait),
                timeout_ms,
            )
        else:
            self._api.require(CAP_TABLE_MAINTENANCE)
            start = c_bytes(as_bytes(startRow)) if startRow is not None else None
            end = c_bytes(as_bytes(endRow)) if endRow is not None else None
            self._call(
                "shoal_connector_flush_table_range",
                self.table.encode(),
                C.byref(start[0]) if start else None,
                C.byref(end[0]) if end else None,
                int(wait),
                timeout_ms,
            )
        return 0

    def setProperty(self, key: str, value: str, *, timeout_ms: int = 0) -> int:
        if not key:
            return -1
        self._call(
            "shoal_connector_set_table_property",
            self.table.encode(),
            key.encode(),
            value.encode(),
            timeout_ms,
        )
        return 0

    def removeProperty(self, key: str, *, timeout_ms: int = 0) -> int:
        if not key:
            return -1
        self._call(
            "shoal_connector_remove_table_property",
            self.table.encode(),
            key.encode(),
            timeout_ms,
        )
        return 0

    def getProperties(self, *, timeout_ms: int = 0) -> dict[str, str]:
        return dict(
            self._pairs(
                "shoal_connector_effective_table_properties",
                "shoal_table_properties_count",
                "shoal_table_properties_get",
                "shoal_table_properties_free",
                PropertyView,
                self.table.encode(),
                timeout_ms,
            )
        )

    def listSplits(self, *, timeout_ms: int = 0) -> list[bytes]:
        self._api.require(CAP_TABLE_SPLITS)
        return self._bytes_list(
            "shoal_connector_list_table_splits", self.table.encode(), timeout_ms
        )

    def addSplits(
        self, splits: Iterable[str | bytes], *, timeout_ms: int = 0
    ) -> None:
        self._api.require(CAP_TABLE_SPLITS)
        values = [c_bytes(as_bytes(value)) for value in splits]
        array = (Bytes * len(values))(*(value[0] for value in values))
        self._call(
            "shoal_connector_add_table_splits",
            self.table.encode(),
            array if values else None,
            len(values),
            timeout_ms,
        )

    def addConstraint(self, class_name: str, *, timeout_ms: int = 0) -> int:
        self._api.require(CAP_TABLE_MAINTENANCE)
        number = C.c_int32()
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_add_table_constraint(
            self._connector._handle,
            self.table.encode(),
            class_name.encode(),
            timeout_ms,
            C.byref(number),
            C.byref(error),
        )
        self._api.check(status, error)
        return int(number.value)

    def listConstraints(self, *, timeout_ms: int = 0) -> dict[int, str]:
        self._api.require(CAP_TABLE_MAINTENANCE)
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_list_table_constraints(
            self._connector._handle,
            self.table.encode(),
            timeout_ms,
            C.byref(result),
            C.byref(error),
        )
        self._api.check(status, error)
        values: dict[int, str] = {}
        try:
            for index in range(self._api.lib.shoal_table_constraint_list_count(result)):
                view = ConstraintView()
                self._api.lib.shoal_table_constraint_view_init(C.byref(view))
                item_error = C.c_void_p()
                item_status = self._api.lib.shoal_table_constraint_list_get(
                    result, index, C.byref(view), C.byref(item_error)
                )
                self._api.check(item_status, item_error)
                values[int(view.number)] = view.class_name.decode()
        finally:
            self._api.lib.shoal_table_constraint_list_free(C.byref(result))
        return values

    def removeConstraint(self, number: int, *, timeout_ms: int = 0) -> None:
        self._api.require(CAP_TABLE_MAINTENANCE)
        self._call(
            "shoal_connector_remove_table_constraint",
            self.table.encode(),
            number,
            timeout_ms,
        )

    def createWriter(
        self, auths: object = (), threads: int = 10
    ) -> object:
        if auths is None:
            from .errors import ClientException
            raise ClientException("authorizations must not be None")
        from .writer import BatchWriter, BatchWriterOptions
        return BatchWriter(
            self._connector,
            self.table,
            options=BatchWriterOptions(max_write_threads=threads),
        )

    def createScanner(
        self, auths: object = (), threads: int = 10
    ) -> object:
        if auths is None:
            from .errors import ClientException

            raise ClientException("authorizations must not be None")
        from .client import BatchScanner

        return BatchScanner(self._connector, self.table, auths, threads)

    def import_directory(
        self,
        directory: str,
        fail_path: str,
        setTime: bool = False,
    ) -> bool:
        del directory, fail_path, setTime
        raise NotImplementedError(
            "legacy Sharkbite bulk import is an approved compatibility divergence "
            "(SB-DIV-019): Shoal Bulk Import V2 requires a staged load map"
        )

    def compact(
        self, startRow: str, endRow: str, wait: bool
    ) -> int:
        del startRow, endRow, wait
        raise NotImplementedError(
            "online compaction is an approved compatibility divergence "
            "(SB-DIV-018): Accumulo 4 exposes no compatible RPC or IDL payload"
        )


class NamespaceOperations(_Operations):
    def __init__(self, connector: object, namespace: str = "") -> None:
        super().__init__(connector)
        self._api.require(CAP_NAMESPACE_ADMIN)
        self.namespace = namespace

    def _name(self, name: str) -> str:
        return name or self.namespace

    def list_with_ids(self, *, timeout_ms: int = 0) -> dict[str, str]:
        return dict(
            self._pairs(
                "shoal_connector_list_namespaces",
                "shoal_namespace_list_count",
                "shoal_namespace_list_get",
                "shoal_namespace_list_free",
                NamespaceView,
                timeout_ms,
            )
        )

    def list(self, *, timeout_ms: int = 0) -> list[str]:
        return list(self.list_with_ids(timeout_ms=timeout_ms))

    def exists(self, nm: str = "", *, timeout_ms: int = 0) -> bool:
        return self._bool(
            "shoal_connector_namespace_exists", self._name(nm).encode(), timeout_ms
        )

    def create(self, nm: str = "", *, timeout_ms: int = 0) -> None:
        self._call(
            "shoal_connector_create_namespace", self._name(nm).encode(), timeout_ms
        )

    def remove(self, nm: str = "", *, timeout_ms: int = 0) -> bool:
        self._call(
            "shoal_connector_delete_namespace", self._name(nm).encode(), timeout_ms
        )
        return True

    def rename(
        self, newName: str, oldName: str = "", *, timeout_ms: int = 0
    ) -> None:
        old_name = self._name(oldName)
        self._call(
            "shoal_connector_rename_namespace",
            old_name.encode(),
            newName.encode(),
            timeout_ms,
        )
        if old_name == self.namespace:
            self.namespace = newName

    def setProperty(
        self, property: str, value: str, nm: str = "", *, timeout_ms: int = 0
    ) -> None:
        self._call(
            "shoal_connector_set_namespace_property",
            self._name(nm).encode(),
            property.encode(),
            value.encode(),
            timeout_ms,
        )

    def removeProperty(
        self, property: str, nm: str = "", *, timeout_ms: int = 0
    ) -> None:
        self._call(
            "shoal_connector_remove_namespace_property",
            self._name(nm).encode(),
            property.encode(),
            timeout_ms,
        )

    def getProperties(
        self, nm: str = "", *, timeout_ms: int = 0
    ) -> dict[str, str]:
        return dict(
            self._pairs(
                "shoal_connector_effective_namespace_properties",
                "shoal_namespace_properties_count",
                "shoal_namespace_properties_get",
                "shoal_namespace_properties_free",
                PropertyView,
                self._name(nm).encode(),
                timeout_ms,
            )
        )

    def getLocalProperties(
        self, nm: str = "", *, timeout_ms: int = 0
    ) -> dict[str, str]:
        return dict(
            self._pairs(
                "shoal_connector_namespace_properties",
                "shoal_namespace_properties_count",
                "shoal_namespace_properties_get",
                "shoal_namespace_properties_free",
                PropertyView,
                self._name(nm).encode(),
                timeout_ms,
            )
        )

    def getVersionedProperties(
        self, nm: str = "", *, timeout_ms: int = 0
    ) -> tuple[int, dict[str, str]]:
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_connector_versioned_namespace_properties(
            self._connector._handle,
            self._name(nm).encode(),
            timeout_ms,
            C.byref(result),
            C.byref(error),
        )
        self._api.check(status, error)
        properties: dict[str, str] = {}
        try:
            version = int(self._api.lib.shoal_versioned_properties_version(result))
            for index in range(
                self._api.lib.shoal_versioned_properties_count(result)
            ):
                view = PropertyView()
                item_error = C.c_void_p()
                item_status = self._api.lib.shoal_versioned_properties_get(
                    result, index, C.byref(view), C.byref(item_error)
                )
                self._api.check(item_status, item_error)
                properties[view.key.decode("utf-8", "surrogateescape")] = (
                    view.value.decode("utf-8", "surrogateescape")
                )
        finally:
            self._api.lib.shoal_versioned_properties_free(C.byref(result))
        return version, properties


class SecurityOperations(_Operations):
    def __init__(self, connector: object) -> None:
        super().__init__(connector)
        self._api.require(CAP_SECURITY_ADMIN)

    def _password_call(
        self, symbol: str, user: str, password: str | bytes, timeout_ms: int
    ) -> int:
        view, buffer = c_bytes(as_bytes(password))
        self._call(symbol, user.encode(), C.byref(view), timeout_ms)
        _ = buffer
        return 0

    def create_user(self, user: str, password: str | bytes, *, timeout_ms: int = 0) -> int:
        if not user:
            return -1
        try:
            self._password_call(
                "shoal_connector_create_user", user, password, timeout_ms
            )
        except (AlreadyExistsError, ClientException) as exc:
            if exc.status == 19:
                return 0
            raise
        return 1

    def change_password(self, user: str, password: str | bytes, *, timeout_ms: int = 0) -> int:
        if not user:
            return -1
        return self._password_call(
            "shoal_connector_change_password", user, password, timeout_ms
        )

    def remove_user(self, user: str, *, timeout_ms: int = 0) -> int:
        if not user:
            return -1
        self._call("shoal_connector_drop_user", user.encode(), timeout_ms)
        return 0

    def get_auths(self, user: str, *, timeout_ms: int = 0) -> Authorizations:
        if not user:
            raise ClientException("argument cannot be empty")
        return Authorizations(
            self._bytes_list(
                "shoal_connector_get_user_authorizations", user.encode(), timeout_ms
            )
        )

    def grantAuthorizations(
        self, auths: Iterable[str | bytes] | None, user: str, *, timeout_ms: int = 0
    ) -> int:
        if auths is None:
            return -2
        if not user:
            return -1
        values = [c_bytes(as_bytes(value)) for value in auths]
        array = (Bytes * len(values))(*(value[0] for value in values))
        self._call(
            "shoal_connector_change_user_authorizations",
            user.encode(),
            array if values else None,
            len(values),
            timeout_ms,
        )
        return 1

    @staticmethod
    def _validate_permission(permission: IntEnum, expected: type[IntEnum]) -> None:
        if not isinstance(permission, expected):
            raise TypeError(
                f"permission must be {expected.__name__}, not "
                f"{type(permission).__name__}"
            )

    def _permission_bool(
        self, scope: str, user: str, target: str | None, permission: IntEnum,
        permission_type: type[IntEnum], timeout_ms: int
    ) -> bool:
        if not user:
            raise ClientException("argument cannot be empty")
        self._validate_permission(permission, permission_type)
        args = [user.encode()]
        if target is not None:
            args.append(target.encode())
        args.extend([int(permission), timeout_ms])
        return self._bool(f"shoal_connector_has_{scope}_permission", *args)

    def _permission_call(
        self, action: str, scope: str, user: str, target: str | None,
        permission: IntEnum, permission_type: type[IntEnum], timeout_ms: int
    ) -> int:
        if not user:
            return -1
        self._validate_permission(permission, permission_type)
        args = [user.encode()]
        if target is not None:
            args.append(target.encode())
        args.extend([int(permission), timeout_ms])
        self._call(f"shoal_connector_{action}_{scope}_permission", *args)
        return 1

    def has_system_permission(self, user: str, permission: SystemPermissions, *, timeout_ms: int = 0) -> bool:
        return self._permission_bool("system", user, None, permission, SystemPermissions, timeout_ms)

    def has_table_permission(self, user: str, table: str, permission: TablePermissions, *, timeout_ms: int = 0) -> bool:
        return self._permission_bool("table", user, table, permission, TablePermissions, timeout_ms)

    def has_namespace_permission(self, user: str, namespace: str, permission: NamespacePermissions, *, timeout_ms: int = 0) -> bool:
        return self._permission_bool("namespace", user, namespace, permission, NamespacePermissions, timeout_ms)

    def grant_system_permission(self, user: str, permission: SystemPermissions, *, timeout_ms: int = 0) -> int:
        return self._permission_call("grant", "system", user, None, permission, SystemPermissions, timeout_ms)

    def revoke_system_permission(self, user: str, permission: SystemPermissions, *, timeout_ms: int = 0) -> int:
        return self._permission_call("revoke", "system", user, None, permission, SystemPermissions, timeout_ms)

    def grant_table_permission(self, user: str, table: str, permission: TablePermissions, *, timeout_ms: int = 0) -> int:
        return self._permission_call("grant", "table", user, table, permission, TablePermissions, timeout_ms)

    def revoke_table_permission(self, user: str, table: str, permission: TablePermissions, *, timeout_ms: int = 0) -> int:
        return self._permission_call("revoke", "table", user, table, permission, TablePermissions, timeout_ms)

    def grant_namespace_permission(self, user: str, namespace: str, permission: NamespacePermissions, *, timeout_ms: int = 0) -> int:
        return self._permission_call("grant", "namespace", user, namespace, permission, NamespacePermissions, timeout_ms)

    def revoke_namespace_permission(self, user: str, namespace: str, permission: NamespacePermissions, *, timeout_ms: int = 0) -> int:
        return self._permission_call("revoke", "namespace", user, namespace, permission, NamespacePermissions, timeout_ms)


class TableInfo(_Operations):
    def list(self, *, timeout_ms: int = 0) -> dict[str, str]:
        self._api.require(CAP_TABLE_ADMIN)
        return dict(
            self._pairs(
                "shoal_connector_list_tables",
                "shoal_table_list_count",
                "shoal_table_list_get",
                "shoal_table_list_free",
                TableView,
                timeout_ms,
            )
        )

    def list_tables(self, *, timeout_ms: int = 0) -> list[str]:
        return list(self.list(timeout_ms=timeout_ms))

    def table_id(self, table: str, *, timeout_ms: int = 0) -> str:
        try:
            return self.list(timeout_ms=timeout_ms)[table]
        except KeyError as exc:
            from .errors import NotFoundError
            raise NotFoundError(f"table not found: {table}", status=9) from exc

    def table_name(self, tableid: str, *, timeout_ms: int = 0) -> str:
        for name, current_id in self.list(timeout_ms=timeout_ms).items():
            if current_id == tableid:
                return name
        from .errors import NotFoundError
        raise NotFoundError(f"table ID not found: {tableid}", status=9)

    def exists(self, table: str, *, timeout_ms: int = 0) -> bool:
        return self._bool(
            "shoal_connector_table_exists", table.encode(), timeout_ms
        )


setattr(TableOperations, "import", TableOperations.import_directory)
