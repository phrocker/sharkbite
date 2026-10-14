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

"""Compatibility helpers for Shoal's supported Python versions."""

import sys
from dataclasses import dataclass
from enum import Enum
from functools import partial


class StrEnum(str, Enum):
    """A Python 3.9-compatible subset of :class:`enum.StrEnum`."""

    def __str__(self) -> str:
        return str.__str__(self)


frozen_dataclass = partial(
    dataclass,
    frozen=True,
    **({"slots": True} if sys.version_info >= (3, 10) else {}),
)
