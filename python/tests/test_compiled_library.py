from __future__ import annotations

import os
import ctypes as C
import logging
import unittest
from concurrent.futures import ThreadPoolExecutor

from sharkbite import LoggingConfiguration, Mutation, NativeAPI


class CompiledLibraryTests(unittest.TestCase):
    @unittest.skipUnless(
        os.environ.get("SHOAL_LIBRARY"), "SHOAL_LIBRARY is not set"
    )
    def test_writer_and_administration_symbols_match_reported_capabilities(self):
        api = NativeAPI()
        api.require(4, 5, 6, 7, 8, 9, 10, 11, 12, 19, 24, 29, 30, 31)
        self.assertGreaterEqual(api.info.version, (1, 19, 0))
        with Mutation(b"row", _api=api) as mutation:
            mutation.put(b"cf", b"cq", b"A", 7, b"value")
            mutation.delete(b"cf", b"cq", b"A", 8)
            self.assertGreater(mutation.size(), 0)
        LoggingConfiguration._set(1, api)
        self.assertEqual(api.lib.shoal_logging_get_level(), 1)
        LoggingConfiguration._set(0, api)
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(lambda _: api.require(9, 10, 11, 12, 19), range(128)))

        records = []

        class Handler(logging.Handler):
            def emit(self, record):
                records.append(record)

        logger = logging.Logger("shoal-compiled")
        logger.addHandler(Handler())
        LoggingConfiguration.configure(logger, api=api)
        try:
            LoggingConfiguration._set(1, api)
        finally:
            LoggingConfiguration._set(0, api)
            LoggingConfiguration.configure(None, api=api)
        self.assertEqual(records[0].msg, "shoal.logging.level_changed")
        self.assertEqual(records[0].shoal, {"level": "debug"})

        error = C.c_void_p()
        status = api.lib.shoal_logging_set_level(99, C.byref(error))
        with self.assertRaises(RuntimeError) as raised:
            api.check(status, error)
        self.assertEqual(type(raised.exception), RuntimeError)
        self.assertEqual(
            raised.exception.source_name, "IllegalArgumentException"
        )
        self.assertEqual(
            raised.exception.__cause__.source_name, "IllegalArgumentException"
        )


if __name__ == "__main__":
    unittest.main()
