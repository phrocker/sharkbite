# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements. See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership. The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License. You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied. See the License for the
# specific language governing permissions and limitations
# under the License.

"""Transport-independent errors for Shoal's Python API."""

from __future__ import annotations

from typing import Optional, Union

from ._compat import StrEnum


class ErrorCode(StrEnum):
    """Stable, transport-independent failure categories."""

    INVALID_ARGUMENT = "invalid_argument"
    NOT_FOUND = "not_found"
    CONFLICT = "conflict"
    UNAUTHORIZED = "unauthorized"
    UNAVAILABLE = "unavailable"
    CANCELED = "canceled"
    DEADLINE_EXCEEDED = "deadline_exceeded"
    INTERNAL = "internal"


class ShoalError(Exception):
    """A public Shoal failure with an optional underlying cause."""

    def __init__(
        self,
        code: Union[ErrorCode, str],
        message: str = "",
        *,
        cause: Optional[BaseException] = None,
    ) -> None:
        self.code = ErrorCode(code)
        self.message = message
        super().__init__(str(self))
        if cause is not None:
            self.__cause__ = cause

    def __str__(self) -> str:
        if not self.message:
            return self.code.value
        return f"{self.code.value}: {self.message}"


def new_error(code: Union[ErrorCode, str], message: str = "") -> ShoalError:
    """Construct a public error without an underlying cause."""

    return ShoalError(code, message)


def wrap_error(
    code: Union[ErrorCode, str],
    message: str,
    cause: BaseException,
) -> ShoalError:
    """Construct a public error that retains its underlying cause."""

    return ShoalError(code, message, cause=cause)


def is_error_code(
    error: Optional[BaseException],
    code: Union[ErrorCode, str],
) -> bool:
    """Return whether an exception or an exception in its chain has ``code``."""

    expected = ErrorCode(code)
    current = error
    seen: set[int] = set()
    while current is not None and id(current) not in seen:
        seen.add(id(current))
        if isinstance(current, ShoalError) and current.code is expected:
            return True
        if current.__cause__ is not None:
            current = current.__cause__
        elif not current.__suppress_context__:
            current = current.__context__
        else:
            current = None
    return False
