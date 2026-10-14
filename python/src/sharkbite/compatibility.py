from __future__ import annotations

import ctypes as C
import json
import logging
from enum import IntFlag

from ._native import (
    CAP_CLIENT_PARITY_CONTROLS,
    CAP_STORAGE_ERROR_PARITY,
    LogCallback,
    NativeAPI,
)


class ScannerOptions(IntFlag):
    HedgedReads = 0x1
    RFileScanOnly = 0x2
    ENABLE_HEDGED_READS = HedgedReads
    ENABLE_RFILE_SCANNER = RFileScanOnly


class IterInfo:
    def __init__(self, *args: object, **kwargs: object):
        iterator_type = kwargs.pop("type", None)
        if kwargs:
            name = next(iter(kwargs))
            raise TypeError(f"unexpected keyword argument {name!r}")
        if len(args) == 3 and isinstance(args[2], int):
            first, second, priority = args
            script_like = (
                iterator_type is not None
                or "\n" in str(first)
                or str(first).lstrip().startswith(("class ", "def ", "lambda "))
            )
            if iterator_type is not None and str(iterator_type) != "Python":
                raise ValueError("script IterInfo type must be 'Python'")
            if str(second) == "Python" or script_like:
                self._script = str(first)
                self._name = str(second) if str(second) != "Python" else str(first)
                self._class_name = "org.poma.accumulo.JythonIterator"
            else:
                self._script = ""
                self._name = str(first)
                self._class_name = str(second)
            self._priority = int(priority)
        elif len(args) == 4:
            script, name, priority, positional_type = args
            if iterator_type is not None:
                raise TypeError("IterInfo type was provided twice")
            if str(positional_type) != "Python":
                raise ValueError("script IterInfo type must be 'Python'")
            self._script = str(script)
            self._name = str(name)
            self._class_name = "org.poma.accumulo.JythonIterator"
            self._priority = int(priority)
        else:
            raise TypeError(
                "IterInfo expects (name, class_name, priority) or "
                "(script, iterator_name, priority, type='Python')"
            )

    def getPriority(self) -> int:
        return self._priority

    def priority(self) -> int:
        return self.getPriority()

    def getName(self) -> str:
        return self._name

    def name(self) -> str:
        return self.getName()

    def getClass(self) -> str:
        return self._class_name

    def _class_alias(self) -> str:
        return self.getClass()

    def __eq__(self, other: object) -> bool:
        return (
            isinstance(other, IterInfo)
            and self._name == other._name
            and self._class_name == other._class_name
            and self._priority == other._priority
            and self._script == other._script
        )

    def __hash__(self) -> int:
        return hash((self._name, self._class_name, self._priority, self._script))

    def __repr__(self) -> str:
        return (
            f"IterInfo({self._name!r}, {self._class_name!r}, "
            f"{self._priority!r})"
        )


setattr(IterInfo, "class", IterInfo._class_alias)


class PythonIterator(IterInfo):
    def __init__(self, name: str, script_or_priority: str | int, priority: int | None = None):
        if priority is None:
            super().__init__(name, "org.poma.accumulo.JythonIterator", int(script_or_priority))
            self._script = ""
        else:
            super().__init__(name, "org.poma.accumulo.JythonIterator", int(priority))
            self._script = str(script_or_priority)

    def onNext(self, source: str) -> PythonIterator:
        if self._script:
            raise RuntimeError(
                "Cannot provide -onNext when a python script is provided"
            )
        self._script = source
        return self

    def getPriority(self) -> int:
        return self._priority

    def priority(self) -> int:
        return self.getPriority()

    def getName(self) -> str:
        return self._name

    def name(self) -> str:
        return self.getName()

    def getClass(self) -> str:
        return "org.poma.accumulo.JythonIterator"

    @property
    def script(self) -> str:
        return self._script


def unsupported_scanner_option(option: ScannerOptions | int) -> NotImplementedError:
    try:
        normalized = ScannerOptions(option)
    except (TypeError, ValueError):
        return NotImplementedError(f"unsupported Sharkbite scanner option: {option!r}")
    if normalized & ScannerOptions.HedgedReads:
        return NotImplementedError(
            "ScannerOptions.HedgedReads is an approved stable divergence "
            "(SB-DIV-008): Shoal does not race RPC and RFile scans"
        )
    if normalized & ScannerOptions.RFileScanOnly:
        return NotImplementedError(
            "ScannerOptions.RFileScanOnly is an approved stable divergence "
            "(SB-DIV-008): use RFileOperations for explicit RFile access"
        )
    return NotImplementedError(f"unsupported Sharkbite scanner option: {option!r}")


def unsupported_python_iterator() -> NotImplementedError:
    return NotImplementedError(
        "server-side Python iterator execution is an approved stable divergence "
        "(SB-DIV-007); the pinned Sharkbite execution path was non-functional"
    )


class LoggingConfiguration:
    _callback: LogCallback | None = None
    _api: NativeAPI | None = None

    @staticmethod
    def _set(level: int, api: NativeAPI | None = None) -> None:
        native = api or NativeAPI()
        native.require(CAP_CLIENT_PARITY_CONTROLS)
        error = C.c_void_p()
        status = native.lib.shoal_logging_set_level(level, C.byref(error))
        native.check(status, error)

    @staticmethod
    def enableDebugLogger() -> None:
        LoggingConfiguration._set(1)

    @staticmethod
    def enableTraceLogger() -> None:
        LoggingConfiguration._set(2)

    @staticmethod
    def disableLogger() -> None:
        LoggingConfiguration._set(0)

    @staticmethod
    def configure(logger: logging.Logger | None, *, api: NativeAPI | None = None) -> None:
        native = api or NativeAPI()
        native.require(CAP_STORAGE_ERROR_PARITY)
        error = C.c_void_p()
        if logger is None:
            callback = LogCallback()
            status = native.lib.shoal_logging_set_callback(
                callback, None, C.byref(error)
            )
            native.check(status, error)
            LoggingConfiguration._callback = None
            LoggingConfiguration._api = None
            return

        def emit(
            level: int, event_name: bytes, attributes_json: bytes, _: object
        ) -> None:
            try:
                attributes = json.loads((attributes_json or b"{}").decode())
            except (UnicodeDecodeError, json.JSONDecodeError):
                attributes = {}
            python_level = logging.DEBUG if level >= 1 else logging.INFO
            logger.log(
                python_level,
                (event_name or b"shoal").decode("utf-8", "replace"),
                extra={"shoal": attributes},
            )

        callback = LogCallback(emit)
        status = native.lib.shoal_logging_set_callback(
            callback, None, C.byref(error)
        )
        native.check(status, error)
        LoggingConfiguration._callback = callback
        LoggingConfiguration._api = native
