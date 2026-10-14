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

"""Values shared by Shoal's public Python knowledge APIs."""

from .errors import ErrorCode, ShoalError, is_error_code, new_error, wrap_error
from .types import ID, Metadata, Score, freeze_metadata

__all__ = [
    "ErrorCode",
    "ID",
    "Metadata",
    "Score",
    "ShoalError",
    "freeze_metadata",
    "is_error_code",
    "new_error",
    "wrap_error",
]
