from __future__ import annotations

import copy
import ctypes as C
from typing import Any

from ._native import (
    CAP_ZOOKEEPER_INSTANCE,
    ConnectorIdentityView,
    NativeAPI,
    as_bytes,
)
from .errors import ClientException


class Configuration:
    def __init__(self, source: Configuration | None = None) -> None:
        self._values = dict(source._values) if source is not None else {}

    def set(self, name: str, value: str) -> None:
        self._values[str(name)] = str(value)

    def get(self, name: str, default: str = "") -> str:
        return self._values.get(str(name), default)

    def getLong(self, name: str, default: int = 0) -> int:
        value = self._values.get(str(name))
        if value is None:
            return int(default) & 0xFFFFFFFF
        text = value.strip()
        index = int(bool(text) and text[0] in "+-")
        while index < len(text) and text[index].isdigit():
            index += 1
        numeric = text[:index]
        if not numeric or numeric in {"+", "-"}:
            return 0
        return max(0, min(int(numeric), 0xFFFFFFFF))

    def clone(self) -> Configuration:
        return Configuration(self)

    def __copy__(self) -> Configuration:
        return self.clone()

    def __deepcopy__(self, _: dict[int, Any]) -> Configuration:
        return self.clone()


class Instance:
    pass


class ZookeeperInstance(Instance):
    DEFAULT_SESSION_TIMEOUT_MS = 30_000

    def __init__(
        self,
        instance: str | ZookeeperInstance,
        zookeepers: str | None = None,
        timeoutMs: int = 0,
        configuration: Configuration | None = None,
        *,
        _api: NativeAPI | None = None,
    ) -> None:
        if isinstance(instance, ZookeeperInstance):
            if zookeepers is not None or configuration is not None:
                raise TypeError("copy construction accepts only the source instance")
            source = instance
            instance = source.getInstanceName()
            zookeepers = source._zookeepers
            timeoutMs = source._requested_timeout_ms
            configuration = source.getConfiguration().clone()
            _api = _api or source._api
        if not isinstance(instance, str) or not instance:
            raise ClientException("instance name is required")
        if not isinstance(zookeepers, str) or not zookeepers:
            raise ClientException("ZooKeeper servers are required")
        if timeoutMs < 0:
            raise ClientException("ZooKeeper session timeout must not be negative")
        if configuration is None:
            configuration = Configuration()
        if not isinstance(configuration, Configuration):
            raise TypeError("configuration must be a Configuration")

        self._api = _api or NativeAPI()
        self._api.require(CAP_ZOOKEEPER_INSTANCE)
        self._instance_name = instance
        self._zookeepers = zookeepers
        self._requested_timeout_ms = int(timeoutMs)
        self._session_timeout_ms = (
            int(timeoutMs) if timeoutMs else self.DEFAULT_SESSION_TIMEOUT_MS
        )
        self._configuration = configuration.clone()
        self._closed = False
        self._instance_id = self._resolve()

    def _resolve(self) -> str:
        result = C.c_void_p()
        error = C.c_void_p()
        status = self._api.lib.shoal_zookeeper_resolve_instance(
            self._instance_name.encode(),
            self._zookeepers.encode(),
            self._requested_timeout_ms,
            0,
            None,
            C.byref(result),
            C.byref(error),
        )
        self._api.check(status, error)
        view = ConnectorIdentityView()
        try:
            self._api.lib.shoal_connector_identity_view_init(C.byref(view))
            item_error = C.c_void_p()
            item_status = self._api.lib.shoal_connector_identity_get(
                result, C.byref(view), C.byref(item_error)
            )
            self._api.check(item_status, item_error)
            return view.instance_id.decode("utf-8", "surrogateescape")
        finally:
            self._api.lib.shoal_connector_identity_free(C.byref(result))

    @property
    def closed(self) -> bool:
        return self._closed

    @property
    def session_timeout_ms(self) -> int:
        return self._session_timeout_ms

    def getInstanceName(self) -> str:
        return self._instance_name

    def instance_name(self) -> str:
        return self.getInstanceName()

    def getInstanceId(self, retry: bool = False) -> str:
        del retry
        return self._instance_id

    def instance_id(self, retry: bool = False) -> str:
        return self.getInstanceId(retry)

    def getConfiguration(self) -> Configuration:
        return self._configuration

    def getZookeepers(self) -> str:
        return self._zookeepers

    def close(self) -> None:
        self._closed = True

    def __enter__(self) -> ZookeeperInstance:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def __copy__(self) -> ZookeeperInstance:
        return ZookeeperInstance(self)

    def __deepcopy__(self, _: dict[int, Any]) -> ZookeeperInstance:
        return ZookeeperInstance(self)


class AuthInfo:
    __slots__ = ("_username", "_password", "_instance_id")

    def __init__(
        self, username: str, password: str | bytes, instanceId: str
    ) -> None:
        if not username:
            raise ClientException("username is required")
        if not instanceId:
            raise ClientException("instance ID is required")
        self._username = username
        self._password = bytearray(as_bytes(password))
        self._instance_id = instanceId

    def getUserName(self) -> str:
        return self._username

    def username(self) -> str:
        return self.getUserName()

    def getInstanceId(self) -> str:
        return self._instance_id

    def instance_id(self) -> str:
        return self.getInstanceId()

    def getPassword(self) -> str:
        raise AttributeError(
            "password readback is disabled by approved divergence SB-DIV-002"
        )

    def password(self) -> str:
        return self.getPassword()

    def _password_bytes(self) -> bytes:
        return bytes(self._password)

    def __repr__(self) -> str:
        return (
            f"AuthInfo(username={self._username!r}, "
            f"instance_id={self._instance_id!r}, password=<redacted>)"
        )


class AccumuloInfo:
    pass


class TabletServerStatus:
    pass


class TableRates:
    pass


class TableCompactions:
    pass


class Compacting:
    pass


class RecoveryStatus:
    pass


class DeadServer:
    pass
