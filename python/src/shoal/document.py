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

"""Public hierarchical document values."""

from __future__ import annotations

from dataclasses import field
from datetime import datetime
from typing import Optional

from ._compat import frozen_dataclass
from .errors import ErrorCode, ShoalError, new_error
from .types import ID, Metadata, freeze_metadata

_MAX_INT64 = (1 << 63) - 1
_MAX_INT32 = (1 << 31) - 1


@frozen_dataclass
class SourcePosition:
    """A zero-based content offset and optional one-based source page."""

    offset: int = 0
    page: int = 0


@frozen_dataclass
class SourceRange:
    """A half-open source interval ``[start, end)``."""

    start: SourcePosition = field(default_factory=SourcePosition)
    end: SourcePosition = field(default_factory=SourcePosition)

    def validate(self) -> None:
        """Validate ordering and coordinate invariants."""

        coordinates = (
            ("start offset", self.start.offset, _MAX_INT64),
            ("end offset", self.end.offset, _MAX_INT64),
            ("start page", self.start.page, _MAX_INT32),
            ("end page", self.end.page, _MAX_INT32),
        )
        for name, value, maximum in coordinates:
            if type(value) is not int or value < 0 or value > maximum:
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    f"{name} must be an in-range integer",
                )
        if self.start.offset < 0 or self.end.offset < self.start.offset:
            raise new_error(ErrorCode.INVALID_ARGUMENT, "invalid source offsets")
        if (
            self.start.page > 0
            and self.end.page > 0
            and self.end.page < self.start.page
        ):
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "invalid source page range",
            )


@frozen_dataclass
class Revision:
    """An immutable version of a document."""

    id: ID = ""
    document_id: ID = ""
    created_at: Optional[datetime] = None
    source_version: str = ""
    metadata: Metadata = field(default_factory=freeze_metadata)

    def __post_init__(self) -> None:
        object.__setattr__(self, "metadata", freeze_metadata(self.metadata))


@frozen_dataclass
class Document:
    """The revision-specific root of a hierarchical source."""

    id: ID = ""
    revision_id: ID = ""
    title: str = ""
    root_section_id: ID = ""
    metadata: Metadata = field(default_factory=freeze_metadata)

    def __post_init__(self) -> None:
        object.__setattr__(self, "metadata", freeze_metadata(self.metadata))


@frozen_dataclass
class Section:
    """An ordered node in a document tree."""

    id: ID = ""
    document_id: ID = ""
    revision_id: ID = ""
    parent_id: ID = ""
    order: int = 0
    heading: str = ""
    range: SourceRange = field(default_factory=SourceRange)
    metadata: Metadata = field(default_factory=freeze_metadata)

    def __post_init__(self) -> None:
        object.__setattr__(self, "metadata", freeze_metadata(self.metadata))


@frozen_dataclass
class Span:
    """An ordered, directly attributable piece of source content."""

    id: ID = ""
    document_id: ID = ""
    revision_id: ID = ""
    section_id: ID = ""
    order: int = 0
    range: SourceRange = field(default_factory=SourceRange)
    text: str = ""
    metadata: Metadata = field(default_factory=freeze_metadata)

    def __post_init__(self) -> None:
        object.__setattr__(self, "metadata", freeze_metadata(self.metadata))


@frozen_dataclass
class Citation:
    """An exact reference to evidence in one document revision."""

    document_id: ID = ""
    revision_id: ID = ""
    section_id: ID = ""
    span_id: ID = ""
    range: SourceRange = field(default_factory=SourceRange)

    def validate(self) -> None:
        """Validate that the citation identifies immutable source evidence."""

        if not self.document_id or not self.revision_id:
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "citation requires document and revision IDs",
            )
        if not self.section_id and not self.span_id:
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "citation requires a section or span ID",
            )
        try:
            self.range.validate()
        except ShoalError as error:
            raise RuntimeError(f"citation: {error}") from error
