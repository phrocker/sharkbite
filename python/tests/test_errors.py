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

import unittest

from shoal import ErrorCode, is_error_code, wrap_error


class ErrorTest(unittest.TestCase):
    def test_error_code_values_are_stable(self) -> None:
        self.assertEqual(
            [code.value for code in ErrorCode],
            [
                "invalid_argument",
                "not_found",
                "conflict",
                "unauthorized",
                "unavailable",
                "canceled",
                "deadline_exceeded",
                "internal",
            ],
        )

    def test_error_code_survives_exception_chaining(self) -> None:
        cause = OSError("backend unavailable")
        error = wrap_error(
            ErrorCode.UNAVAILABLE,
            "knowledge store unavailable",
            cause,
        )
        wrapped = RuntimeError("retrieve failed")
        wrapped.__cause__ = error

        self.assertTrue(is_error_code(wrapped, ErrorCode.UNAVAILABLE))
        self.assertIs(error.__cause__, cause)
        self.assertEqual(
            str(error),
            "unavailable: knowledge store unavailable",
        )


if __name__ == "__main__":
    unittest.main()
