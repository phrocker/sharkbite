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

"""Public knowledge graph values."""

from __future__ import annotations

from dataclasses import field

from ._compat import frozen_dataclass
from .errors import ErrorCode, new_error
from .types import ID, Metadata, Score, freeze_metadata


@frozen_dataclass
class Node:
    """A schema-neutral knowledge graph node."""

    id: ID = ""
    kind: str = ""
    labels: tuple[str, ...] = ()
    properties: Metadata = field(default_factory=freeze_metadata)

    def __post_init__(self) -> None:
        object.__setattr__(self, "labels", tuple(self.labels))
        object.__setattr__(
            self,
            "properties",
            freeze_metadata(self.properties),
        )


@frozen_dataclass
class Edge:
    """A directed, typed relationship between two nodes."""

    id: ID = ""
    from_id: ID = ""
    to_id: ID = ""
    type: str = ""
    weight: Score = 0.0
    properties: Metadata = field(default_factory=freeze_metadata)

    def __post_init__(self) -> None:
        object.__setattr__(
            self,
            "properties",
            freeze_metadata(self.properties),
        )


@frozen_dataclass
class Path:
    """An ordered graph explanation."""

    nodes: tuple[Node, ...] = ()
    edges: tuple[Edge, ...] = ()

    def __post_init__(self) -> None:
        object.__setattr__(self, "nodes", tuple(self.nodes))
        object.__setattr__(self, "edges", tuple(self.edges))

    def validate(self) -> None:
        """Validate that the path is connected and structurally complete."""

        if not self.nodes:
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "graph path requires a node",
            )
        if any(not node.id for node in self.nodes):
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "graph path node requires an id",
            )
        if len(self.edges) != len(self.nodes) - 1:
            raise new_error(
                ErrorCode.INVALID_ARGUMENT,
                "graph path has inconsistent edges",
            )
        for index, edge in enumerate(self.edges):
            if not edge.id or not edge.type:
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "graph path edge requires an id and type",
                )
            if (
                edge.from_id != self.nodes[index].id
                or edge.to_id != self.nodes[index + 1].id
            ):
                raise new_error(
                    ErrorCode.INVALID_ARGUMENT,
                    "graph path is not connected",
                )
