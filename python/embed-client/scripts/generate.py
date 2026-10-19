"""Generate packaged Python stubs from the repository's canonical proto.

Run with the exact dependencies in requirements-codegen.txt. --check compares
fresh output with committed files and is used in CI.
"""

from __future__ import annotations

import ast
import argparse
from importlib import metadata
from pathlib import Path
import shutil
import sys
import tempfile

from grpc_tools import protoc
from google.protobuf import descriptor_pb2


ROOT = Path(__file__).resolve().parents[3]
PACKAGE = ROOT / "python/embed-client/src/shoal_embed_client"
PROTO = ROOT / "proto/embed.proto"
PINNED = {"grpcio-tools": "1.74.0", "grpcio": "1.74.0", "protobuf": "6.33.5"}


def generate(check: bool) -> None:
    for package, expected in PINNED.items():
        actual = metadata.version(package)
        if actual != expected:
            raise SystemExit(f"{package} {actual} is installed; generator requires {expected}")
    with tempfile.TemporaryDirectory() as temporary:
        out = Path(temporary)
        result = protoc.main(
            ["grpc_tools.protoc", "-I", str(PROTO.parent),
             f"--python_out={out}", f"--grpc_python_out={out}", str(PROTO)]
        )
        if result != 0:
            raise SystemExit(f"grpc_tools.protoc failed: {result}")
        grpc_stub = out / "embed_pb2_grpc.py"
        source = grpc_stub.read_text()
        absolute_import = "import embed_pb2 as embed__pb2"
        if source.count(absolute_import) != 1:
            raise SystemExit("generated gRPC stub import changed; inspect generator output")
        grpc_stub.write_text(source.replace(absolute_import, "from . import embed_pb2 as embed__pb2"))
        pb_stub = out / "embed_pb2.py"
        pb_source = pb_stub.read_text()
        prefix = "DESCRIPTOR = _descriptor_pool.Default().AddSerializedFile("
        descriptor_lines = [line for line in pb_source.splitlines() if line.startswith(prefix)]
        if len(descriptor_lines) != 1 or not descriptor_lines[0].endswith(")"):
            raise SystemExit("generated protobuf registry setup changed; inspect generator output")
        descriptor_line = descriptor_lines[0]
        serialized = descriptor_line[len(prefix):-1]
        dependencies = descriptor_pb2.FileDescriptorProto.FromString(
            ast.literal_eval(serialized)
        ).dependency
        if dependencies:
            raise SystemExit("private protobuf fallback needs dependency support: " + ", ".join(dependencies))
        # Keep the normal registry available to dependent protos and reflection.
        # A process that loaded a different embed.proto first can still use this
        # wheel's messages through a private pool instead of failing at import.
        replacement = (
            f"_SERIALIZED_DESCRIPTOR = {serialized}\n"
            "try:\n"
            "    DESCRIPTOR = _descriptor_pool.Default().AddSerializedFile(_SERIALIZED_DESCRIPTOR)\n"
            "except TypeError:\n"
            "    _sym_db = _symbol_database.SymbolDatabase(pool=_descriptor_pool.DescriptorPool())\n"
            "    DESCRIPTOR = _sym_db.pool.AddSerializedFile(_SERIALIZED_DESCRIPTOR)"
        )
        pb_stub.write_text(pb_source.replace(descriptor_line, replacement))
        shutil.copyfile(PROTO, out / "embed.proto")
        for generated, committed in (
            (out / "embed_pb2.py", PACKAGE / "embed_pb2.py"),
            (grpc_stub, PACKAGE / "embed_pb2_grpc.py"),
            (out / "embed.proto", PACKAGE / "_proto/embed.proto"),
        ):
            if check:
                if not committed.exists() or generated.read_bytes() != committed.read_bytes():
                    raise SystemExit(f"generated artifact drift: {committed.relative_to(ROOT)}")
            else:
                committed.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(generated, committed)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="fail on generated-file drift")
    generate(parser.parse_args().check)
