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
from datetime import datetime, timedelta, timezone

from shoal import ErrorCode, ShoalError, is_error_code
from shoal.retrieval import (
    Evidence,
    Mode,
    NativeRetriever,
    RemoteRetriever,
    Request,
    Response,
    Retriever,
    Scope,
)


class StubTransport:
    def __init__(self) -> None:
        self.requests: list[Request] = []

    def retrieve(self, request: Request) -> Response:
        self.requests.append(request)
        return Response(request_id="request-1")


class RetrievalTest(unittest.TestCase):
    def test_retrieval_modes_match_go_contract(self) -> None:
        self.assertEqual(
            [mode.value for mode in Mode],
            ["lexical", "vector", "tree", "graph"],
        )
        Request(
            text="query",
            modes=[Mode.LEXICAL, Mode.VECTOR, Mode.TREE, Mode.GRAPH],
        ).validate()

    def test_zero_values_match_go_contract(self) -> None:
        request = Request()
        evidence = Evidence()

        self.assertEqual(request.text, "")
        self.assertEqual(evidence.citation.document_id, "")
        self.assertEqual(evidence.path.nodes, ())
        with self.assertRaises(ShoalError) as raised:
            request.validate()
        self.assertTrue(
            is_error_code(raised.exception, ErrorCode.INVALID_ARGUMENT)
        )

    def test_retriever_uses_transport_neutral_values(self) -> None:
        transport = StubTransport()
        client: Retriever = NativeRetriever(transport)
        request = Request(
            text="why did the deployment fail?",
            top_k=5,
            modes=[Mode.TREE, Mode.GRAPH],
        )

        response = client.retrieve(request)

        self.assertIsInstance(client, Retriever)
        self.assertEqual(response.request_id, "request-1")
        self.assertEqual(transport.requests, [request])
        self.assertIsInstance(request.modes, tuple)

    def test_native_and_remote_adapters_validate_before_transport(self) -> None:
        for adapter_type in (NativeRetriever, RemoteRetriever):
            for text in ("  ", "\ud800"):
                with self.subTest(adapter=adapter_type.__name__, text=text):
                    transport = StubTransport()
                    client = adapter_type(transport)
                    with self.assertRaises(ShoalError) as raised:
                        client.retrieve(Request(text=text))
                    self.assertTrue(
                        is_error_code(
                            raised.exception,
                            ErrorCode.INVALID_ARGUMENT,
                        )
                    )
                    self.assertEqual(transport.requests, [])

    def test_request_rejects_unknown_mode(self) -> None:
        request = Request(text="query", modes=["cells"])
        with self.assertRaises(ShoalError) as raised:
            request.validate()
        self.assertTrue(
            is_error_code(raised.exception, ErrorCode.INVALID_ARGUMENT)
        )

    def test_request_rejects_top_k_outside_go_value_range(self) -> None:
        for top_k in (-1, 2**32, True):
            with self.subTest(top_k=top_k):
                with self.assertRaises(ShoalError) as raised:
                    Request(text="query", top_k=top_k).validate()
                self.assertTrue(
                    is_error_code(
                        raised.exception,
                        ErrorCode.INVALID_ARGUMENT,
                    )
                )

    def test_request_rejects_invalid_scope(self) -> None:
        invalid_requests = (
            Request(text="query", scope=None),
            Request(text="query", scope=Scope(document_ids=(1,))),
            Request(text="query", scope=Scope(node_ids=("\ud800",))),
        )
        for request in invalid_requests:
            with self.subTest(scope=request.scope):
                with self.assertRaises(ShoalError) as raised:
                    request.validate()
                self.assertTrue(
                    is_error_code(
                        raised.exception,
                        ErrorCode.INVALID_ARGUMENT,
                    )
                )

    def test_request_rejects_ambiguous_or_unrepresentable_as_of(self) -> None:
        invalid_values = (
            datetime(2026, 8, 24),
            datetime(
                1,
                1,
                1,
                tzinfo=timezone(timedelta(hours=1)),
            ),
            datetime(
                9999,
                12,
                31,
                23,
                59,
                59,
                tzinfo=timezone(timedelta(hours=-1)),
            ),
        )
        for as_of in invalid_values:
            with self.subTest(as_of=as_of):
                with self.assertRaises(ShoalError) as raised:
                    Request(text="query", as_of=as_of).validate()
                self.assertTrue(
                    is_error_code(
                        raised.exception,
                        ErrorCode.INVALID_ARGUMENT,
                    )
                )

    def test_request_accepts_representable_aware_as_of(self) -> None:
        Request(
            text="query",
            as_of=datetime(2026, 8, 24, tzinfo=timezone.utc),
        ).validate()


if __name__ == "__main__":
    unittest.main()
