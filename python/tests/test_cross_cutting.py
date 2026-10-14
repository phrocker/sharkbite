from __future__ import annotations

import ctypes as C
import os
import subprocess
import sys
import threading
import unittest

from sharkbite import ForkSafetyError, NativeAPI
from sharkbite.errors import (
    ClientException,
    OutOfMemoryError,
)


@unittest.skipUnless(os.environ.get("SHOAL_LIBRARY"), "SHOAL_LIBRARY is not set")
class CrossCuttingCompiledTests(unittest.TestCase):
    def setUp(self) -> None:
        self.api = NativeAPI()

    def test_concurrent_abi_queries_are_stable(self) -> None:
        failures: list[BaseException] = []

        def query() -> None:
            try:
                for _ in range(2000):
                    self.assertEqual(self.api.lib.shoal_abi_version_major(), 1)
                    self.assertGreaterEqual(
                        self.api.lib.shoal_abi_capability_count(), 30
                    )
            except BaseException as exc:
                failures.append(exc)

        threads = [threading.Thread(target=query) for _ in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual(failures, [])

    def test_ctypes_releases_and_reacquires_gil_for_native_calls(self) -> None:
        sleep = self.api.lib.shoal_test_sleep_ms
        sleep.argtypes = [C.c_int64]
        sleep.restype = None
        enter = threading.Event()
        progressed = threading.Event()
        finish = threading.Event()

        def worker() -> None:
            enter.wait()
            progressed.set()
            finish.wait()

        thread = threading.Thread(target=worker)
        thread.start()
        old_interval = sys.getswitchinterval()
        try:
            sys.setswitchinterval(10.0)
            enter.set()
            sleep(200)
            self.assertTrue(progressed.is_set())
            self.assertEqual(self.api.lib.shoal_abi_version_major(), 1)
        finally:
            sys.setswitchinterval(old_interval)
            finish.set()
            thread.join()

    def test_oom_deadline_and_retry_statuses_remain_distinct(self) -> None:
        with self.assertRaises(OutOfMemoryError):
            self.api.check(3, C.c_void_p())

        create_writer = self.api.lib.shoal_test_batch_writer_create
        create_writer.argtypes = [C.c_int, C.POINTER(C.c_void_p)]
        create_writer.restype = C.c_int
        writer = C.c_void_p()
        self.assertEqual(create_writer(2, C.byref(writer)), 1)
        failure = C.c_void_p()
        error = C.c_void_p()
        status = self.api.lib.shoal_batch_writer_close(
            writer, 1, C.byref(failure), C.byref(error)
        )
        with self.assertRaises(RuntimeError) as raised:
            self.api.check_write(status, failure, error)
        self.assertIs(type(raised.exception), RuntimeError)
        self.assertEqual(raised.exception.status, 8)
        self.api.lib.shoal_batch_writer_free(C.byref(writer))

        self.assertEqual(create_writer(1, C.byref(writer)), 1)
        failure = C.c_void_p()
        error = C.c_void_p()
        status = self.api.lib.shoal_batch_writer_flush(
            writer, 1000, C.byref(failure), C.byref(error)
        )
        with self.assertRaises(ClientException) as raised:
            self.api.check_write(status, failure, error)
        self.assertEqual(raised.exception.status, 18)
        self.assertEqual(raised.exception.write_failure_flags & 0x2, 0x2)
        self.api.lib.shoal_batch_writer_free(C.byref(writer))

    @unittest.skipUnless(hasattr(os, "fork"), "fork is unavailable")
    def test_inherited_native_state_is_rejected_before_go_call(self) -> None:
        read_fd, write_fd = os.pipe()
        child = os.fork()
        if child == 0:
            os.close(read_fd)
            result = b""
            try:
                try:
                    self.api.lib.shoal_abi_version_major()
                except ForkSafetyError:
                    result += b"inherited;"
                try:
                    NativeAPI()
                except ForkSafetyError:
                    result += b"new;"
                os.write(write_fd, result)
            finally:
                os._exit(0)
        os.close(write_fd)
        result = os.read(read_fd, 64)
        os.close(read_fd)
        _, status = os.waitpid(child, 0)
        self.assertEqual(status, 0)
        self.assertEqual(result, b"inherited;new;")

    def test_exec_subprocess_can_load_fresh_native_state(self) -> None:
        completed = subprocess.run(
            [
                sys.executable,
                "-c",
                "from sharkbite import NativeAPI; "
                "assert NativeAPI().info.version[0] == 1",
            ],
            check=False,
            capture_output=True,
            text=True,
            env=os.environ.copy(),
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)


if __name__ == "__main__":
    unittest.main()
