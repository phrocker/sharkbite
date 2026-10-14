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

from shoal import ErrorCode, ShoalError, is_error_code
from shoal.graph import Edge, Node, Path


class GraphTest(unittest.TestCase):
    def test_path_requires_at_least_one_node(self) -> None:
        with self.assertRaises(ShoalError) as raised:
            Path().validate()
        self.assertEqual(
            str(raised.exception),
            "invalid_argument: graph path requires a node",
        )

    def test_path_requires_one_edge_between_each_node(self) -> None:
        path = Path(nodes=[Node(id="a"), Node(id="b")])

        with self.assertRaises(ShoalError) as raised:
            path.validate()
        self.assertEqual(
            str(raised.exception),
            "invalid_argument: graph path has inconsistent edges",
        )

    def test_path_requires_node_ids(self) -> None:
        with self.assertRaises(ShoalError) as raised:
            Path(nodes=[Node()]).validate()
        self.assertEqual(
            str(raised.exception),
            "invalid_argument: graph path node requires an id",
        )

    def test_path_requires_edge_id_and_type(self) -> None:
        for edge in (
            Edge(from_id="a", to_id="b", type="supports"),
            Edge(id="edge-1", from_id="a", to_id="b"),
        ):
            with self.subTest(edge=edge):
                with self.assertRaises(ShoalError) as raised:
                    Path(
                        nodes=[Node(id="a"), Node(id="b")],
                        edges=[edge],
                    ).validate()
                self.assertEqual(
                    str(raised.exception),
                    "invalid_argument: graph path edge requires an id and type",
                )

    def test_path_requires_connected_edges(self) -> None:
        path = Path(
            nodes=[Node(id="a"), Node(id="b")],
            edges=[
                Edge(
                    id="edge-1",
                    from_id="a",
                    to_id="b",
                    type="supports",
                )
            ],
        )
        path.validate()
        self.assertIsInstance(path.nodes, tuple)
        self.assertIsInstance(path.edges, tuple)

        disconnected = Path(
            nodes=path.nodes,
            edges=[
                Edge(
                    id="edge-1",
                    from_id="a",
                    to_id="c",
                    type="supports",
                )
            ],
        )
        with self.assertRaises(ShoalError) as raised:
            disconnected.validate()
        self.assertTrue(
            is_error_code(raised.exception, ErrorCode.INVALID_ARGUMENT)
        )

    def test_node_copies_mutable_inputs(self) -> None:
        labels = ["deployment"]
        properties = {"state": "failed"}
        node = Node(id="node-1", labels=labels, properties=properties)
        labels.append("changed")
        properties["state"] = "changed"

        self.assertEqual(node.labels, ("deployment",))
        self.assertEqual(node.properties["state"], "failed")


if __name__ == "__main__":
    unittest.main()
