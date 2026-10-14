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

"""Transport-neutral knowledge retrieval contracts."""

from __future__ import annotations

from dataclasses import field
from datetime import datetime, timezone
from types import MappingProxyType
from typing import List, Mapping, Optional, Protocol, Tuple, Union, runtime_checkable

from ._compat import StrEnum, frozen_dataclass
from .document import Citation
from .errors import ErrorCode, new_error
from .graph import Path
from .types import ID, Score


class Mode(StrEnum):
    """A retrieval strategy; multiple modes request a hybrid plan."""

    LEXICAL = "lexical"
    VECTOR = "vector"
    TREE = "tree"
    GRAPH = "graph"


@frozen_dataclass
class Scope:
    """Bounds retrieval to known documents or graph nodes."""

    document_ids: tuple[ID, ...] = ()
    node_ids: tuple[ID, ...] = ()

    def __post_init__(self) -> None:
        object.__setattr__(self, "document_ids", tuple(self.document_ids))
        object.__setattr__(self, "node_ids", tuple(self.node_ids))

    def validate(self) -> None:
        """Validate identifiers for transport as protobuf strings."""

        for name, values in (
            ("document ID", self.document_ids),
            ("node ID", self.node_ids),
        ):
            for value in values:
                if not _is_wire_string(value):
                    raise new_error(
                        ErrorCode.INVALID_ARGUMENT,
                        f"retrieval {name} must be a valid UTF-8 string",
                    )


@frozen_dataclass
class Request:
    """One coarse knowledge retrieval operation."""

    text: str = ""
    top_k: int = 0
    modes: Tuple[Union[Mode, str], ...] = ()
    scope: Scope = field(default_factory=Scope)
    as_of: Optional[datetime] = None
    explain: bool = False

    def __post_init__(self) -> None:
        normalized_modes: List[Union[Mode, str]] = []
        for mode in self.modes:
            try:
                normalized_modes.append(Mode(mode))
            except (TypeError, ValueError):
                normalized_modes.append(mode)
        object.__setattr__(self, "modes", tuple(normalized_modes))

    def validate(self) -> None:
        """Validate transport-independent request invariants."""

        if not _is_wire_string(self.text):
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "retrieval text must be a valid UTF-8 string",
            )
        if not self.text.strip():
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "retrieval text is required",
            )
        if (
            isinstance(self.top_k, bool)
            or not isinstance(self.top_k, int)
            or self.top_k < 0
            or self.top_k > 2**32 - 1
        ):
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "top_k must be an unsigned 32-bit integer",
            )
        for mode in self.modes:
            try:
                Mode(mode)
            except (TypeError, ValueError):
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "unknown retrieval mode",
                ) from None
        if not isinstance(self.scope, Scope):
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "scope must be a Scope",
            )
        self.scope.validate()
        if self.as_of is not None:
            if not isinstance(self.as_of, datetime):
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "as_of must be a datetime",
                )
            if self.as_of.utcoffset() is None:
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "as_of must be timezone-aware",
                )
            try:
                utc_as_of = self.as_of.astimezone(timezone.utc)
            except (OverflowError, ValueError):
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "as_of is outside the supported UTC range",
                ) from None
            if utc_as_of.year < 1 or utc_as_of.year > 9999:
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "as_of is outside the supported UTC range",
                )


def _is_wire_string(value: object) -> bool:
    if not isinstance(value, str):
        return False
    try:
        value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return True


@frozen_dataclass
class Evidence:
    """Immutable source evidence and an optional graph path."""

    citation: Citation = field(default_factory=Citation)
    quote: str = ""
    path: Path = field(default_factory=Path)
    score: Score = 0.0


@frozen_dataclass
class Explanation:
    """Why a result was selected, without an execution or storage plan."""

    modes: tuple[Mode, ...] = ()
    summary: str = ""
    scores: Mapping[str, Score] = field(
        default_factory=lambda: MappingProxyType({})
    )

    def __post_init__(self) -> None:
        object.__setattr__(self, "modes", tuple(self.modes))
        object.__setattr__(
            self,
            "scores",
            MappingProxyType(dict(self.scores)),
        )


@frozen_dataclass
class Result:
    """One ranked, evidence-addressable retrieval result."""

    id: ID = ""
    score: Score = 0.0
    evidence: tuple[Evidence, ...] = ()
    explanation: Optional[Explanation] = None

    def __post_init__(self) -> None:
        object.__setattr__(self, "evidence", tuple(self.evidence))


@frozen_dataclass
class Response:
    """The complete result of one retrieval request."""

    request_id: ID = ""
    results: tuple[Result, ...] = ()

    def __post_init__(self) -> None:
        object.__setattr__(self, "results", tuple(self.results))


@runtime_checkable
class Retriever(Protocol):
    """The transport-neutral retrieval client contract."""

    def retrieve(self, request: Request) -> Response:
        """Retrieve ranked knowledge for ``request``."""

        ...


@runtime_checkable
class NativeTransport(Protocol):
    """The boundary a future native transport must implement."""

    def retrieve(self, request: Request) -> Response:
        """Perform a native retrieval operation."""

        ...


@runtime_checkable
class RemoteTransport(Protocol):
    """The boundary a future remote transport must implement."""

    def retrieve(self, request: Request) -> Response:
        """Perform a remote retrieval operation."""

        ...


@frozen_dataclass
class NativeRetriever:
    """Validating adapter for a native retrieval transport."""

    transport: NativeTransport

    def retrieve(self, request: Request) -> Response:
        request.validate()
        return self.transport.retrieve(request)


@frozen_dataclass
class RemoteRetriever:
    """Validating adapter for a remote retrieval transport."""

    transport: RemoteTransport

    def retrieve(self, request: Request) -> Response:
        request.validate()
        return self.transport.retrieve(request)
