from __future__ import annotations

import asyncio
import ctypes as C
import gc
import random
import threading
import unittest
import weakref

from sharkbite import (
    BatchScanner,
    ClientException,
    Connector,
    Key,
    PythonIterator,
    Results,
    ScannerOptions,
)
from sharkbite._native import Bytes, KeyValueView, Range, ScannerConfig, c_bytes


class Function:
    def __init__(self, callback):
        self.callback = callback

    def __call__(self, *args):
        return self.callback(*args)


class FakeLibrary:
    def __init__(self):
        self._buffers = []
        self._cursors = {}
        self._results = {}
        self._next = 10
        self.configs = []
        self.ranges = []
        self.closed_scanners = 0
        self.closed_cursors = 0
        self.block_next = False
        self.next_started = threading.Event()
        self.next_released = threading.Event()
        self.shoal_connector_config_init = Function(self._init)
        self.shoal_connector_create = Function(self._create)
        self.shoal_connector_close = Function(lambda *_: 0)
        self.shoal_connector_free = Function(lambda *_: None)
        self.shoal_scanner_config_init = Function(self._init)
        self.shoal_range_init = Function(self._init)
        self.shoal_connector_create_batch_scanner = Function(self._scanner_create)
        self.shoal_batch_scanner_stream = Function(self._stream)
        self.shoal_batch_scanner_close = Function(self._scanner_close)
        self.shoal_batch_scanner_free = Function(self._free)
        self.shoal_scan_cursor_next = Function(self._cursor_next)
        self.shoal_scan_cursor_close = Function(self._cursor_close)
        self.shoal_scan_cursor_free = Function(self._free)
        self.shoal_scan_result_count = Function(self._result_count)
        self.shoal_scan_result_get = Function(self._result_get)
        self.shoal_scan_result_free = Function(self._result_free)

    @staticmethod
    def _init(pointer):
        C.cast(pointer, C.POINTER(C.c_uint32)).contents.value = C.sizeof(
            pointer._obj
        )

    def _handle(self):
        self._next += 1
        return self._next

    @staticmethod
    def _create(config, out_handle, error):
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = 1
        return 0

    def _scanner_create(self, connector, config_pointer, out_handle, error):
        config = C.cast(
            config_pointer, C.POINTER(ScannerConfig)
        ).contents
        self.configs.append(
            (
                config.table_name,
                config.parallelism,
                config.authorization_count,
                config.column_count,
                config.iterator_count,
            )
        )
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = self._handle()
        return 0

    def _stream(
        self, scanner, ranges, range_count, timeout, out_cursor, error
    ):
        copied = []
        for index in range(range_count):
            copied.append(
                (
                    self._raw(ranges[index].start.row),
                    self._raw(ranges[index].end.row),
                )
            )
        self.ranges.append(copied)
        cursor = self._handle()
        self._cursors[cursor] = [b"%s:%s" % pair for pair in copied]
        C.cast(out_cursor, C.POINTER(C.c_void_p)).contents.value = cursor
        return 0

    def _cursor_next(
        self, cursor, max_entries, out_result, exhausted, error
    ):
        if self.block_next:
            self.next_started.set()
            self.next_released.wait(2)
            C.cast(exhausted, C.POINTER(C.c_uint8)).contents.value = 1
            return 0
        values = self._cursors[int(cursor.value)]
        batch = values[: int(max_entries)]
        del values[: len(batch)]
        if batch:
            result = self._handle()
            self._results[result] = batch
            C.cast(out_result, C.POINTER(C.c_void_p)).contents.value = result
        C.cast(exhausted, C.POINTER(C.c_uint8)).contents.value = int(not values)
        return 0

    def _result_count(self, result):
        return len(self._results[int(result.value)])

    def _result_get(self, result, index, out_view, error):
        value = self._results[int(result.value)][index]
        view = C.cast(out_view, C.POINTER(KeyValueView)).contents
        view.row = self._value(value)
        view.column_family = self._value(b"cf")
        view.column_qualifier = self._value(b"cq")
        view.column_visibility = self._value(b"")
        view.timestamp = 7
        view.value = self._value(value)
        return 0

    def _result_free(self, result):
        self._results.pop(int(result._obj.value), None)
        result._obj.value = None

    def _scanner_close(self, *_):
        self.closed_scanners += 1
        return 0

    def _cursor_close(self, *_):
        self.closed_cursors += 1
        self.next_released.set()
        return 0

    @staticmethod
    def _free(pointer):
        pointer._obj.value = None

    def _value(self, value):
        buffer = (C.c_uint8 * len(value)).from_buffer_copy(value)
        self._buffers.append(buffer)
        return Bytes(C.cast(buffer, C.POINTER(C.c_uint8)), len(value))

    @staticmethod
    def _raw(value):
        return C.string_at(value.data, value.length) if value.length else b""


class FakeAPI:
    def __init__(self):
        self.lib = FakeLibrary()
        self.capabilities = frozenset(range(30))

    def require(self, *capabilities):
        if set(capabilities) - self.capabilities:
            raise NotImplementedError

    @staticmethod
    def check(status, error):
        if status:
            raise AssertionError(status)

    @staticmethod
    def copy_view(view):
        def raw(value):
            return C.string_at(value.data, value.length) if value.length else b""

        return (
            (
                raw(view.row),
                raw(view.column_family),
                raw(view.column_qualifier),
                raw(view.column_visibility),
                view.timestamp,
            ),
            raw(view.value),
        )


def row_range(start: bytes, end: bytes) -> tuple[Range, list[object]]:
    value = Range()
    value.struct_size = C.sizeof(Range)
    start_view, start_buffer = c_bytes(start)
    end_view, end_buffer = c_bytes(end)
    value.start.kind = 1
    value.start.row = start_view
    value.end.kind = 1
    value.end.row = end_view
    value.start_inclusive = 1
    return value, [start_buffer, end_buffer]


class StreamingCompatibilityTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        self.connector = Connector("i", "zk", "u", "p", _api=self.api)

    def tearDown(self):
        self.connector.close()

    def test_call_shapes_lifecycle_and_restartable_results(self):
        scan_range, keepalive = row_range(b"a", b"z")
        with BatchScanner(self.connector, "t", (), 2) as scanner:
            self.assertIs(scanner.withRange(scan_range), scanner)
            scanner.fetchColumn(b"cf")
            results = scanner.getResultSet()
            self.assertIsInstance(results, Results)
            first = list(results)
            second = list(results)
            self.assertEqual([entry.getValue() for entry in first], [b"a:z"])
            self.assertEqual([entry.getValue() for entry in second], [b"a:z"])
        self.assertEqual(self.api.lib.configs[0], (b"t", 2, 0, 1, 0))
        self.assertGreaterEqual(self.api.lib.closed_scanners, 2)
        self.assertGreaterEqual(self.api.lib.closed_cursors, 2)
        self.assertIsNotNone(keepalive)

    def test_none_authorizations_raise_client_exception(self):
        with self.assertRaises(ClientException):
            self.connector.tableOps("t").createScanner(None, 2)

    def test_context_manager_does_not_swallow_exceptions(self):
        with self.assertRaisesRegex(RuntimeError, "body"):
            with BatchScanner(self.connector, "t", (), 1):
                raise RuntimeError("body")

    def test_iterator_and_scanner_option_call_shapes(self):
        class IterInfo:
            def getName(self):
                return "iter"

            def getClass(self):
                return "example.Iterator"

            def getPriority(self):
                return 7

        scanner = BatchScanner(self.connector, "t", (), 2)
        scan_range, keepalive = row_range(b"a", b"z")
        scanner.addRange(scan_range)
        scanner.addIterator(IterInfo())
        list(scanner.getResultSet())
        self.assertEqual(self.api.lib.configs[-1][-1], 1)
        python_iterator = PythonIterator("python", 7)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-007"):
            scanner.addIterator(python_iterator)
        for method in (scanner.setOption, scanner.removeOption):
            with self.subTest(method=method.__name__):
                with self.assertRaisesRegex(NotImplementedError, "SB-DIV-008"):
                    method(ScannerOptions.HedgedReads)
        scanner.close()
        self.assertIsNotNone(keepalive)

    def test_range_snapshots_are_property_checked(self):
        scanner = BatchScanner(self.connector, "t", (), 3)
        expected = []
        keepalive = []
        randomizer = random.Random(193)
        for _ in range(40):
            start = randomizer.randbytes(randomizer.randrange(0, 12))
            end = randomizer.randbytes(randomizer.randrange(0, 12))
            scan_range, retained = row_range(start, end)
            keepalive.extend(retained)
            scanner.addRange(scan_range)
            expected.append((start, end))
        list(scanner.getResultSet())
        self.assertEqual(self.api.lib.ranges[-1], expected)
        scanner.close()

    def test_concurrent_result_sets_are_thread_safe(self):
        scanner = BatchScanner(self.connector, "t", (), 4)
        scan_range, keepalive = row_range(b"a", b"z")
        scanner.addRange(scan_range)
        results = []
        errors = []

        def read():
            try:
                results.append(next(scanner.getResultSet()).getKey())
            except BaseException as exc:
                errors.append(exc)

        threads = [threading.Thread(target=read) for _ in range(12)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertFalse(errors)
        self.assertEqual(results, [Key(b"a:z", b"cf", b"cq", b"", 7)] * 12)
        scanner.close()
        self.assertIsNotNone(keepalive)

    def test_scanner_does_not_retain_abandoned_result_sets(self):
        scanner = BatchScanner(self.connector, "t", (), 2)
        scan_range, keepalive = row_range(b"a", b"z")
        scanner.addRange(scan_range)
        result = scanner.getResultSet()
        self.assertEqual(next(result).getValue(), b"a:z")
        retained = weakref.ref(result)
        del result
        gc.collect()
        self.assertIsNone(retained())
        scanner.close()
        self.assertIsNotNone(keepalive)

    def test_close_cancels_a_blocked_next_without_deadlock(self):
        scanner = BatchScanner(self.connector, "t", (), 2)
        scan_range, keepalive = row_range(b"a", b"z")
        scanner.addRange(scan_range)
        result = scanner.getResultSet()
        self.api.lib.block_next = True
        errors = []

        def read():
            try:
                next(result)
            except StopIteration:
                pass
            except BaseException as exc:
                errors.append(exc)

        thread = threading.Thread(target=read)
        thread.start()
        self.assertTrue(self.api.lib.next_started.wait(1))
        result.close()
        thread.join(1)
        self.assertFalse(thread.is_alive())
        self.assertFalse(errors)
        scanner.close()
        self.assertIsNotNone(keepalive)

    def test_async_scans_run_concurrently(self):
        scanner = BatchScanner(self.connector, "t", (), 2)
        scan_range, keepalive = row_range(b"a", b"z")
        scanner.addRange(scan_range)

        async def read_one():
            result = scanner.getResultSet()
            async for entry in result:
                return entry.getValue()
            return None

        async def read_all():
            return await asyncio.gather(*(read_one() for _ in range(4)))

        self.assertEqual(asyncio.run(read_all()), [b"a:z"] * 4)
        scanner.close()
        self.assertIsNotNone(keepalive)

    def test_results_are_awaitable_and_return_the_restartable_iterator(self):
        scanner = BatchScanner(self.connector, "t", (), 2)
        scan_range, keepalive = row_range(b"a", b"z")
        scanner.addRange(scan_range)
        result = scanner.getResultSet()

        async def await_result():
            iterator = await result
            return iterator, next(iterator).getValue()

        iterator, value = asyncio.run(await_result())
        self.assertIs(iterator, result)
        self.assertEqual(value, b"a:z")
        result.close()
        scanner.close()
        self.assertIsNotNone(keepalive)


if __name__ == "__main__":
    unittest.main()
