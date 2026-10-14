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

"""Common public value types."""

from __future__ import annotations

from types import MappingProxyType
from typing import Mapping, Optional

ID = str
Score = float
Metadata = Mapping[str, str]


def freeze_metadata(metadata: Optional[Mapping[str, str]] = None) -> Metadata:
    """Return an immutable snapshot of application-defined metadata."""

    values = dict(metadata or {})
    if any(not isinstance(key, str) for key in values):
        raise TypeError("metadata keys must be strings")
    if any(not isinstance(value, str) for value in values.values()):
        raise TypeError("metadata values must be strings")
    return MappingProxyType(values)
