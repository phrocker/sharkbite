"""Read ShoalEmbed status and run a vector scan over authenticated TLS.

Example:
  SHOAL_EMBED_TOKEN=... python secure_client.py \
      --target embed.example.org:9876 --ca ca.pem --table graph \
      --embedding-space 'model-v1' --query-hex 3f800000
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path

import grpc

from shoal_embed_client import EmbedClient, embed_pb2


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", required=True, help="TLS server host:port")
    parser.add_argument("--ca", required=True, help="trusted CA PEM file")
    parser.add_argument("--table", required=True)
    parser.add_argument("--embedding-space", required=True)
    parser.add_argument("--query-hex", required=True, help="packed float32 query bytes in hex")
    args = parser.parse_args()

    token = os.environ["SHOAL_EMBED_TOKEN"]
    tls = grpc.ssl_channel_credentials(root_certificates=Path(args.ca).read_bytes())
    bearer = grpc.access_token_call_credentials(token)
    credentials = grpc.composite_channel_credentials(tls, bearer)
    with grpc.secure_channel(args.target, credentials) as channel:
        client = EmbedClient(channel)
        status = client.require_features(
            "rpc.scan_v2", "safety.scan_v2.vector_identity",
            "safety.scan_v2.vector_search", timeout=10,
        )
        print(f"ShoalEmbed {status.build_version} ({status.build_commit})")
        request = embed_pb2.ScanRequest(
            table=args.table,
            vector_search=embed_pb2.VectorSearch(
                query=bytes.fromhex(args.query_hex),
                embedding_space=args.embedding_space,
            ),
        )
        for response in client.scan(request, timeout=30):
            for cell in response.cells:
                print(cell.row.hex(), cell.column_family.hex(), cell.value.hex())


if __name__ == "__main__":
    main()
