from __future__ import annotations

from functools import total_ordering
from typing import Iterable, Iterator

from ._native import as_bytes

MAX_TIMESTAMP = (1 << 63) - 1


def _text(value: bytes) -> str:
    return value.decode("utf-8", errors="surrogateescape")


def _escaped(value: bytes) -> str:
    escapes = {
        0: r"\u0000",
        7: r"\a",
        8: r"\b",
        9: r"\t",
        10: r"\n",
        11: r"\v",
        12: r"\f",
        13: r"\r",
        34: r"\"",
        39: r"\'",
        63: r"\?",
        92: r"\\",
    }
    return "".join(escapes.get(byte, chr(byte)) for byte in value)


@total_ordering
class Value:
    def __init__(self, value: str | bytes | bytearray | memoryview | None = None):
        self._value = None if value is None else as_bytes(value)

    def get(self) -> str:
        return "" if self._value is None else _text(self._value)

    def get_bytes(self) -> bytes:
        return b"" if self._value is None else bytes(self._value)

    def set(self, value: str | bytes | bytearray | memoryview) -> Value:
        self._value = as_bytes(value)
        return self

    def append(self, value: str | bytes | bytearray | memoryview) -> Value:
        self._value = self.get_bytes() + as_bytes(value)
        return self

    def __bytes__(self) -> bytes:
        return self.get_bytes()

    def __str__(self) -> str:
        return self.get()

    def __repr__(self) -> str:
        return "[]" if self._value is None else f"[{self.get()}]"

    def __eq__(self, other: object) -> bool:
        if isinstance(other, Value):
            return self._value == other._value
        if isinstance(other, (bytes, bytearray, memoryview)):
            return self.get_bytes() == bytes(other)
        return NotImplemented

    def __lt__(self, other: object) -> bool:
        if not isinstance(other, Value):
            return NotImplemented
        return self.get_bytes() < other.get_bytes()

    def __hash__(self) -> int:
        return hash(self._value)


@total_ordering
class Key:
    def __init__(
        self,
        row: str | bytes | bytearray | memoryview | None = None,
        column_family: str | bytes | bytearray | memoryview = b"",
        column_qualifier: str | bytes | bytearray | memoryview = b"",
        column_visibility: str | bytes | bytearray | memoryview = b"",
        timestamp: int = MAX_TIMESTAMP,
    ):
        self._null = row is None
        self.row = b"" if row is None else as_bytes(row)
        self.column_family = as_bytes(column_family)
        self.column_qualifier = as_bytes(column_qualifier)
        self.column_visibility = as_bytes(column_visibility)
        self.timestamp = int(timestamp)

    def copy(self) -> Key:
        if self._null:
            return Key()
        return Key(
            self.row,
            self.column_family,
            self.column_qualifier,
            self.column_visibility,
            self.timestamp,
        )

    def setRow(self, row: str | bytes = b"") -> Key:
        self._null = False
        self.row = as_bytes(row)
        return self

    def setColumnFamily(self, cf: str | bytes = b"") -> Key:
        self.column_family = as_bytes(cf)
        return self

    def setColumnQualifier(self, cq: str | bytes = b"") -> Key:
        self.column_qualifier = as_bytes(cq)
        return self

    def getRow(self) -> str:
        return _text(self.row)

    def get_row_bytes(self) -> bytes:
        return bytes(self.row)

    def getColumnFamily(self) -> str:
        return _text(self.column_family)

    def get_column_family_bytes(self) -> bytes:
        return bytes(self.column_family)

    def getColumnQualifier(self) -> str:
        return _text(self.column_qualifier)

    def get_column_qualifier_bytes(self) -> bytes:
        return bytes(self.column_qualifier)

    def getColumnVisibility(self) -> str:
        return _text(self.column_visibility)

    def get_column_visibility_bytes(self) -> bytes:
        return bytes(self.column_visibility)

    def getTimestamp(self) -> int:
        return self.timestamp

    def _ordering(self) -> tuple[bytes, bytes, bytes, bytes, int]:
        return (
            self.row,
            self.column_family,
            self.column_qualifier,
            self.column_visibility,
            -self.timestamp,
        )

    def __eq__(self, other: object) -> bool:
        return (
            isinstance(other, Key)
            and self._null == other._null
            and self._ordering() == other._ordering()
        )

    def __lt__(self, other: object) -> bool:
        if not isinstance(other, Key):
            return NotImplemented
        return self._ordering() < other._ordering()

    def __hash__(self) -> int:
        return hash((self._null, self._ordering()))

    def __str__(self) -> str:
        if self._null:
            return " : []"
        text = (
            f"{_escaped(self.row)} {_escaped(self.column_family)}:"
            f"{_escaped(self.column_qualifier)} [{_text(self.column_visibility)}]"
        )
        if self.timestamp != 0:
            text += f" {self.timestamp}"
        return text

    __repr__ = __str__


class KeyValue:
    def __init__(
        self,
        key: Key | None = None,
        value: Value | str | bytes | bytearray | memoryview | None = None,
        deleted: bool = False,
    ):
        self.key = key.copy() if key is not None else None
        self.value = (
            Value(value.get_bytes()) if isinstance(value, Value) else Value(value)
        )
        self.deleted = bool(deleted)

    def getKey(self) -> Key:
        return Key() if self.key is None else self.key.copy()

    def getValue(self) -> Value:
        return Value(self.value.get_bytes()) if self.value._value is not None else Value()

    def __eq__(self, other: object) -> bool:
        return (
            isinstance(other, KeyValue)
            and self.key == other.key
            and self.value == other.value
            and self.deleted == other.deleted
        )

    def __hash__(self) -> int:
        return hash((self.key, self.value, self.deleted))

    def __str__(self) -> str:
        return f"{self.getKey()} -> {self.value}"

    __repr__ = __str__


class Range:
    def __init__(self, *args: object, update: bool = False):
        self._start: Key | None
        self._stop: Key | None
        self._start_inclusive = True
        self._stop_inclusive = False
        if not args:
            self._start = self._stop = None
        elif len(args) == 1:
            row = as_bytes(args[0])  # type: ignore[arg-type]
            self._start = Key(row)
            self._stop = Key(row)
            self._stop_inclusive = True
        elif len(args) == 2 and isinstance(args[0], Key):
            self._start = args[0].copy()
            self._stop = None
            self._start_inclusive = bool(args[1])
        elif len(args) in (4, 5):
            if len(args) == 5:
                update = bool(args[4])
            start, start_inclusive, stop, stop_inclusive = args[:4]
            self._start = self._bound(start)
            self._stop = self._bound(stop)
            self._start_inclusive = bool(start_inclusive)
            self._stop_inclusive = bool(stop_inclusive)
            if update and self._stop_inclusive and self._stop is not None:
                self._stop.row += b"\x00"
                if not isinstance(stop, Key):
                    self._stop_inclusive = False
        else:
            raise TypeError("Range expects 0, 1, 2, 4, or 5 arguments")

    @staticmethod
    def _bound(value: object) -> Key | None:
        if value is None or value == "" or value == b"":
            return None
        if isinstance(value, Key):
            return value.copy()
        return Key(as_bytes(value))  # type: ignore[arg-type]

    def get_start_key(self) -> Key:
        return Key() if self._start is None else self._start.copy()

    def get_stop_key(self) -> Key:
        return Key() if self._stop is None else self._stop.copy()

    def start_key_inclusive(self) -> bool:
        return self._start_inclusive

    def stop_key_inclusive(self) -> bool:
        return self._stop_inclusive

    def inifinite_start_key(self) -> bool:
        return self._start is None

    def infinite_start_key(self) -> bool:
        return self.inifinite_start_key()

    def inifinite_stop_key(self) -> bool:
        return self._stop is None

    def infinite_stop_key(self) -> bool:
        return self.inifinite_stop_key()

    def after_end_key(self, key: Key) -> bool:
        if self._stop is None:
            return False
        return key > self._stop if self._stop_inclusive else key >= self._stop

    def before_start_key(self, key: Key) -> bool:
        if self._start is None:
            return False
        return key < self._start if self._start_inclusive else key <= self._start

    def __str__(self) -> str:
        start = "(-inf" if self._start is None else (
            ("[" if self._start_inclusive else "(") + str(self._start)
        )
        stop = "+inf) " if self._stop is None else (
            str(self._stop) + ("] " if self._stop_inclusive else ") ")
        )
        return f"Range {start},{stop}"

    __repr__ = __str__


class Authorizations:
    def __init__(self, values: str | bytes | Iterable[str | bytes] = ()):
        if isinstance(values, (str, bytes)):
            values = (values,)
        self._labels = sorted({as_bytes(value) for value in values})

    def addAuthorization(self, auth: str | bytes) -> None:
        value = as_bytes(auth)
        if value not in self._labels:
            self._labels.append(value)
            self._labels.sort()

    def contains(self, auth: str | bytes) -> bool:
        return as_bytes(auth) in self._labels

    def get_authorizations(self) -> list[str]:
        return [_text(value) for value in self._labels]

    def getAuthorizations(self) -> list[bytes]:
        return [bytes(value) for value in self._labels]

    def __iter__(self) -> Iterator[bytes]:
        return iter(self.getAuthorizations())

    def __len__(self) -> int:
        return len(self._labels)

    def __eq__(self, other: object) -> bool:
        return isinstance(other, Authorizations) and self._labels == other._labels

    def __hash__(self) -> int:
        return hash(tuple(self._labels))

    def __str__(self) -> str:
        return "[ ]" if not self._labels else f"[ {', '.join(map(_text, self._labels))} ]"

    __repr__ = __str__
