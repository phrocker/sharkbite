"""Negotiated ShoalEmbed calls over a caller-owned gRPC channel."""

from __future__ import annotations

import grpc
from time import monotonic

from . import embed_pb2, embed_pb2_grpc
from ._version import PROTOCOL_VERSION


class IncompatibleServer(RuntimeError):
    """The server cannot safely perform the requested RPC."""


class EmbedClient:
    """Thin, fail-closed wrapper around the versioned ShoalEmbed stub.

    The caller owns the channel and its TLS/auth credentials. Status is read
    before every operation so capabilities are not cached across deployments.
    Safety-critical operations also use distinct versioned RPCs; if a load
    balancer sends the subsequent call to an older replica, UNIMPLEMENTED is
    reported as an incompatibility rather than a successful fallback.
    """

    def __init__(self, channel: grpc.Channel):
        self._stub = embed_pb2_grpc.ShoalEmbedStub(channel)

    def status(self, **rpc_options) -> embed_pb2.StatusResponse:
        try:
            return self._stub.Status(embed_pb2.StatusRequest(), **rpc_options)
        except grpc.RpcError as error:
            if error.code() == grpc.StatusCode.UNIMPLEMENTED:
                raise IncompatibleServer("ShoalEmbed server does not implement Status") from error
            raise

    def require_features(self, *features: str, **rpc_options) -> embed_pb2.StatusResponse:
        status = self.status(**rpc_options)
        if status.protocol_version and status.protocol_version != PROTOCOL_VERSION:
            raise IncompatibleServer(
                f"ShoalEmbed protocol {status.protocol_version!r} is incompatible "
                f"with client {PROTOCOL_VERSION!r}"
            )
        missing = sorted(set(features).difference(status.features))
        if missing:
            identity = status.build_version or "older build"
            raise IncompatibleServer(
                f"ShoalEmbed {identity} lacks required capabilities: {', '.join(missing)}"
            )
        if features and not status.protocol_version:
            raise IncompatibleServer("ShoalEmbed protocol is unspecified; cannot verify capabilities")
        return status

    def _unary(self, name: str, request, features: tuple[str, ...], **rpc_options):
        started = monotonic()
        self.require_features(*features, **rpc_options)
        rpc_options = self._remaining_options(rpc_options, started)
        try:
            return getattr(self._stub, name)(request, **rpc_options)
        except grpc.RpcError as error:
            if error.code() == grpc.StatusCode.UNIMPLEMENTED:
                raise IncompatibleServer(
                    f"ShoalEmbed replica does not implement {name}; check mixed-version servers"
                ) from error
            raise

    def _stream(self, name: str, request, features: tuple[str, ...], **rpc_options):
        started = monotonic()
        self.require_features(*features, **rpc_options)
        rpc_options = self._remaining_options(rpc_options, started)
        try:
            responses = getattr(self._stub, name)(request, **rpc_options)
        except grpc.RpcError as error:
            if error.code() == grpc.StatusCode.UNIMPLEMENTED:
                raise IncompatibleServer(
                    f"ShoalEmbed replica does not implement {name}; check mixed-version servers"
                ) from error
            raise

        def checked_responses():
            try:
                yield from responses
            except grpc.RpcError as error:
                if error.code() == grpc.StatusCode.UNIMPLEMENTED:
                    raise IncompatibleServer(
                        f"ShoalEmbed replica does not implement {name}; check mixed-version servers"
                    ) from error
                raise

        return checked_responses()

    @staticmethod
    def _remaining_options(rpc_options, started):
        if rpc_options.get("timeout") is None:
            return rpc_options
        return {**rpc_options, "timeout": max(0, rpc_options["timeout"] - (monotonic() - started))}

    def create_table(self, request: embed_pb2.CreateTableRequest, **rpc_options):
        if request.default_embedding or request.workload or request.file_format:
            return self.create_table_v2(request, **rpc_options)
        return self._unary("CreateTable", request, (), **rpc_options)

    def create_table_v2(self, request: embed_pb2.CreateTableRequest, **rpc_options):
        return self._unary(
            "CreateTableV2", request,
            ("rpc.create_table_v2", "safety.create_table_v2.default_embedding"),
            **rpc_options,
        )

    def write(self, request: embed_pb2.WriteRequest, **rpc_options):
        if any(mutation.conditions for mutation in request.mutations):
            return self.conditional_write(request, **rpc_options)
        return self._unary("Write", request, (), **rpc_options)

    def conditional_write(self, request: embed_pb2.WriteRequest, **rpc_options):
        return self._unary(
            "ConditionalWrite", request,
            ("rpc.conditional_write", "safety.conditional_write.conditions"),
            **rpc_options,
        )

    def scan(self, request: embed_pb2.ScanRequest, **rpc_options):
        if (request.HasField("term_filter") or request.HasField("vector_search") or
                request.HasField("edge_expand") or request.HasField("score_filter") or
                request.as_of):
            return self.scan_v2(request, **rpc_options)
        return self._stream("Scan", request, (), **rpc_options)

    def scan_v2(self, request: embed_pb2.ScanRequest, **rpc_options):
        features = ["rpc.scan_v2"]
        if request.HasField("vector_search"):
            features.extend(("safety.scan_v2.vector_identity", "safety.scan_v2.vector_search"))
        return self._stream("ScanV2", request, tuple(features), **rpc_options)

    def flush(self, request: embed_pb2.FlushRequest, **rpc_options):
        return self._unary("Flush", request, (), **rpc_options)

    def compact(self, request: embed_pb2.CompactRequest, **rpc_options):
        if request.workload or request.file_format:
            raise IncompatibleServer(
                "Compact target workload/file format lacks a versioned RPC; "
                "an older replica could silently ignore those fields"
            )
        return self._unary("Compact", request, (), **rpc_options)
