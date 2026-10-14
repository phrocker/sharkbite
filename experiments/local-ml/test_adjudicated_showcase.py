# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import unittest

from run_adjudicated_showcase import validate_window


class CaptureWindowTests(unittest.TestCase):
    def test_nanosecond_order_is_not_truncated(self):
        keys = ('StartedAt', 'FirstPassFinishedAt', 'SecondPassStartedAt', 'CompletedAt')
        stamps = ['2026-10-08T14:00:00.00000000' + str(n) + 'Z' for n in range(1, 5)]
        validate_window(dict(zip(keys, stamps)))
        stamps[1], stamps[2] = stamps[2], stamps[1]
        with self.assertRaises(RuntimeError):
            validate_window(dict(zip(keys, stamps)))


if __name__ == '__main__':
    unittest.main()
