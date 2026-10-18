"""Wire and mixed-version behavior of the packaged gRPC client."""

from concurrent.futures import ThreadPoolExecutor
import unittest

import grpc

from shoal_embed_client import (
    EmbedClient, IncompatibleServer, PROTOCOL_VERSION, __version__,
    embed_pb2 as pb, embed_pb2_grpc as rpc,
)


FEATURES = (
    "rpc.conditional_write", "rpc.create_table_v2", "rpc.scan_v2",
    "safety.conditional_write.conditions",
    "safety.create_table_v2.default_embedding",
    "safety.scan_v2.vector_identity", "safety.scan_v2.vector_search",
)


class Server(rpc.ShoalEmbedServicer):
    def __init__(self, features=FEATURES, protocol=PROTOCOL_VERSION):
        self.features = features
        self.protocol = protocol
        self.calls = []
        self.reject_scan_v2 = False
        self.reject_conditional_write = False

    def Status(self, request, context):
        self.calls.append("Status")
        return pb.StatusResponse(
            tables=["graph"], build_version="1.4.4", protocol_version=self.protocol,
            features=self.features,
        )

    def Scan(self, request, context):
        self.calls.append("Scan")
        yield pb.ScanResponse(cells=[pb.Cell(row=b"plain")])

    def ScanV2(self, request, context):
        self.calls.append("ScanV2")
        if self.reject_scan_v2:
            context.abort(grpc.StatusCode.UNIMPLEMENTED, "old replica")
        yield pb.ScanResponse(cells=[pb.Cell(row=b"vector")])

    def Write(self, request, context):
        self.calls.append("Write")
        return pb.WriteResponse(written=len(request.mutations))

    def ConditionalWrite(self, request, context):
        self.calls.append("ConditionalWrite")
        if self.reject_conditional_write:
            context.abort(grpc.StatusCode.UNIMPLEMENTED, "old replica")
        return pb.WriteResponse(written=len(request.mutations))

    def CreateTable(self, request, context):
        self.calls.append("CreateTable")
        return pb.CreateTableResponse(table=request.table)

    def CreateTableV2(self, request, context):
        self.calls.append("CreateTableV2")
        return pb.CreateTableResponse(table=request.table)

    def Flush(self, request, context):
        self.calls.append("Flush")
        return pb.FlushResponse()

    def Compact(self, request, context):
        self.calls.append("Compact")
        return pb.CompactResponse(table=request.table)


class ClientTest(unittest.TestCase):
    def setUp(self):
        self.server = Server()
        self.grpc_server = grpc.server(ThreadPoolExecutor(max_workers=4))
        rpc.add_ShoalEmbedServicer_to_server(self.server, self.grpc_server)
        port = self.grpc_server.add_insecure_port("127.0.0.1:0")
        self.grpc_server.start()
        self.channel = grpc.insecure_channel(f"127.0.0.1:{port}")
        self.client = EmbedClient(self.channel)

    def tearDown(self):
        self.channel.close()
        self.grpc_server.stop(grace=0).wait()

    def test_versions_and_all_rpcs_are_exposed(self):
        self.assertEqual(__version__, "1.4.4")
        self.assertEqual(PROTOCOL_VERSION, "shoal.embed.v1")
        self.assertEqual(self.client.status().protocol_version, PROTOCOL_VERSION)
        self.client.create_table(pb.CreateTableRequest(table="graph"))
        self.client.write(pb.WriteRequest(table="graph"))
        list(self.client.scan(pb.ScanRequest(table="graph")))
        list(self.client.scan_v2(pb.ScanRequest(table="graph")))
        self.client.conditional_write(pb.WriteRequest(table="graph"))
        self.client.create_table_v2(pb.CreateTableRequest(table="graph"))
        self.client.flush(pb.FlushRequest(table="graph"))
        self.client.compact(pb.CompactRequest(table="graph"))
        self.assertTrue({"CreateTable", "Write", "Scan", "ScanV2", "ConditionalWrite",
                         "CreateTableV2", "Flush", "Compact"}.issubset(self.server.calls))

    def test_old_status_allows_plain_scan_but_refuses_safety_calls(self):
        self.server.features = ()
        self.server.protocol = ""  # pre-capability server: these fields did not exist
        plain = list(self.client.scan(pb.ScanRequest(table="graph")))
        self.assertEqual(plain[0].cells[0].row, b"plain")
        vector = pb.ScanRequest(table="graph", vector_search=pb.VectorSearch(query=b"1234"))
        with self.assertRaisesRegex(IncompatibleServer, "rpc.scan_v2"):
            self.client.scan(vector)
        with self.assertRaisesRegex(IncompatibleServer, "rpc.conditional_write"):
            self.client.write(pb.WriteRequest(table="graph", mutations=[pb.Mutation(
                row=b"a", conditions=[pb.Condition(absent=True)],
            )]))
        with self.assertRaisesRegex(IncompatibleServer, "rpc.create_table_v2"):
            self.client.create_table(pb.CreateTableRequest(table="graph", default_embedding="unknown"))
        with self.assertRaisesRegex(IncompatibleServer, "rpc.scan_v2"):
            self.client.scan(pb.ScanRequest(table="graph", as_of=100))
        with self.assertRaisesRegex(IncompatibleServer, "rpc.create_table_v2"):
            self.client.create_table(pb.CreateTableRequest(
                table="graph", workload=pb.TABLE_WORKLOAD_ANALYTICAL,
            ))
        self.assertNotIn("ScanV2", self.server.calls)
        self.assertNotIn("ConditionalWrite", self.server.calls)
        self.assertNotIn("CreateTableV2", self.server.calls)

    def test_safety_fields_route_to_versioned_rpcs(self):
        vector = pb.ScanRequest(table="graph", vector_search=pb.VectorSearch(query=b"1234"))
        self.assertEqual(list(self.client.scan(vector))[0].cells[0].row, b"vector")
        self.client.write(pb.WriteRequest(table="graph", mutations=[pb.Mutation(
            row=b"a", conditions=[pb.Condition(absent=True)],
        )]))
        self.client.create_table(pb.CreateTableRequest(table="graph", default_embedding="unknown"))
        list(self.client.scan(pb.ScanRequest(table="graph", as_of=100)))
        list(self.client.scan(pb.ScanRequest(
            table="graph", term_filter=pb.TermFilter(term_rows=[b"idx:term"]),
        )))
        self.client.create_table(pb.CreateTableRequest(
            table="graph", file_format=pb.TABLE_FILE_FORMAT_PARQUET,
        ))
        self.assertIn("ScanV2", self.server.calls)
        self.assertIn("ConditionalWrite", self.server.calls)
        self.assertIn("CreateTableV2", self.server.calls)
        self.assertNotIn("Scan", self.server.calls)
        self.assertNotIn("Write", self.server.calls)
        self.assertNotIn("CreateTable", self.server.calls)

    def test_mixed_replica_unimplemented_stream_is_incompatible(self):
        self.server.reject_scan_v2 = True
        vector = pb.ScanRequest(table="graph", vector_search=pb.VectorSearch(query=b"1234"))
        with self.assertRaisesRegex(IncompatibleServer, "old|mixed-version|ScanV2"):
            list(self.client.scan(vector))

    def test_mixed_replica_unimplemented_unary_is_incompatible(self):
        self.server.reject_conditional_write = True
        conditional = pb.WriteRequest(table="graph", mutations=[pb.Mutation(
            row=b"a", conditions=[pb.Condition(absent=True)],
        )])
        with self.assertRaisesRegex(IncompatibleServer, "ConditionalWrite"):
            self.client.write(conditional)
        self.assertNotIn("Write", self.server.calls)

    def test_individual_missing_safety_feature_refuses_effect(self):
        self.server.features = tuple(
            feature for feature in FEATURES
            if feature != "safety.conditional_write.conditions"
        )
        conditional = pb.WriteRequest(table="graph", mutations=[pb.Mutation(
            row=b"a", conditions=[pb.Condition(absent=True)],
        )])
        with self.assertRaisesRegex(IncompatibleServer, "safety.conditional_write.conditions"):
            self.client.write(conditional)
        self.assertNotIn("ConditionalWrite", self.server.calls)

    def test_capabilities_without_protocol_do_not_authorize_safety_calls(self):
        self.server.protocol = ""
        vector = pb.ScanRequest(table="graph", vector_search=pb.VectorSearch(query=b"1234"))
        with self.assertRaisesRegex(IncompatibleServer, "unspecified"):
            self.client.scan(vector)
        self.assertNotIn("ScanV2", self.server.calls)
        self.assertEqual(len(list(self.client.scan(pb.ScanRequest(table="graph")))), 1)

    def test_other_protocol_refuses_even_plain_operations(self):
        self.server.protocol = "shoal.embed.v2"
        with self.assertRaisesRegex(IncompatibleServer, "protocol"):
            list(self.client.scan(pb.ScanRequest(table="graph")))
        self.assertNotIn("Scan", self.server.calls)


if __name__ == "__main__":
    unittest.main()
