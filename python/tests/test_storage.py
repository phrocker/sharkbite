from __future__ import annotations

import ctypes as C
import os
import shutil
import unittest
import uuid
from pathlib import Path

from sharkbite import Hdfs, Key, KeyValue, NativeAPI, RFileOperations


class Function:
    def __init__(self, callback):
        self.callback = callback

    def __call__(self, *args):
        return self.callback(*args)


class FakeHdfsAPI:
    def __init__(self):
        self.capabilities = frozenset(range(32))
        self.calls = []
        self.lib = type("FakeHdfsLibrary", (), {})()
        self.lib.shoal_hdfs_client_create = Function(self._create)
        self.lib.shoal_hdfs_client_mkdir = Function(self._mkdir)
        self.lib.shoal_hdfs_client_chown = Function(self._chown)
        self.lib.shoal_hdfs_client_close = Function(lambda *_: 0)
        self.lib.shoal_hdfs_client_free = Function(lambda *_: None)

    @staticmethod
    def _create(address, timeout, out_handle, error):
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = 1
        return 0

    def _mkdir(self, handle, path, timeout, error):
        self.calls.append(("mkdir", path))
        return 0

    def _chown(self, handle, path, owner, group, timeout, error):
        self.calls.append(("chown", path, owner, group))
        return 0

    def require(self, *capabilities):
        if set(capabilities) - self.capabilities:
            raise NotImplementedError

    @staticmethod
    def check(status, error):
        if status:
            raise AssertionError(status)


class StorageShapeTests(unittest.TestCase):
    def test_rfile_operations_are_real_static_methods(self) -> None:
        self.assertIsInstance(RFileOperations.__dict__["openForWrite"], staticmethod)
        self.assertIsInstance(RFileOperations.__dict__["sequentialRead"], staticmethod)

    def test_hdfs_mkdir_and_chown_return_upstream_status_shape(self) -> None:
        api = FakeHdfsAPI()
        with Hdfs(_api=api) as hdfs:
            self.assertEqual(hdfs.mkdir("/warehouse"), 0)
            self.assertEqual(hdfs.chown("/warehouse", "alice", "analytics"), 0)
        self.assertEqual(
            api.calls,
            [
                ("mkdir", b"/warehouse"),
                ("chown", b"/warehouse", b"alice", b"analytics"),
            ],
        )


@unittest.skipUnless(os.environ.get("SHOAL_LIBRARY"), "SHOAL_LIBRARY is not set")
class CompiledStorageTests(unittest.TestCase):
    def test_rfile_named_locality_group_round_trip(self) -> None:
        api = NativeAPI(os.environ["SHOAL_LIBRARY"])
        directory = Path(__file__).resolve().parents[1] / "build" / "storage-test"
        shutil.rmtree(directory, ignore_errors=True)
        directory.mkdir(parents=True)
        try:
            path = str(directory / "groups.rf")
            with RFileOperations.openForWrite(path, _api=api) as writer:
                writer.append(KeyValue(Key(b"z", b"default", b"", b"", 1), b"d"))
                writer.addLocalityGroup("named")
                writer.append(KeyValue(Key(b"a", b"named", b"", b"", 1), b"n"))
            with RFileOperations.sequentialRead(path, _api=api) as reader:
                self.assertEqual([entry.key.row for entry in reader], [b"a", b"z"])
        finally:
            shutil.rmtree(directory, ignore_errors=True)


@unittest.skipUnless(
    os.environ.get("SHOAL_LIBRARY") and os.environ.get("SHOAL_HDFS_ADDRESS"),
    "compiled library or live HDFS address is not set",
)
class LiveHdfsTests(unittest.TestCase):
    def test_mkdir_and_optional_chown(self) -> None:
        root = os.environ.get("SHOAL_HDFS_TEST_ROOT", "/tmp/shoal-client-tests")
        path = f"{root.rstrip('/')}/{uuid.uuid4().hex}"
        with Hdfs(
            os.environ["SHOAL_HDFS_ADDRESS"],
            library=os.environ["SHOAL_LIBRARY"],
        ) as hdfs:
            try:
                self.assertEqual(hdfs.mkdir(path), 0)
                self.assertTrue(hdfs.stat(path).is_directory)
                owner = os.environ.get("SHOAL_HDFS_TEST_OWNER", "")
                group = os.environ.get("SHOAL_HDFS_TEST_GROUP", "")
                if owner or group:
                    self.assertEqual(hdfs.chown(path, owner, group), 0)
            finally:
                hdfs.remove(path, recursive=True)


if __name__ == "__main__":
    unittest.main()
