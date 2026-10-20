"""Versioned ShoalEmbed gRPC client; independent of native sharkbite bindings."""

from . import embed_pb2, embed_pb2_grpc
from ._version import PROTOCOL_VERSION, __version__
from .client import EmbedClient, IncompatibleServer

__all__ = [
    "EmbedClient", "IncompatibleServer", "PROTOCOL_VERSION", "__version__",
    "embed_pb2", "embed_pb2_grpc",
]
