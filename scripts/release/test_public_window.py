import unittest
from datetime import datetime
from public_window import remaining_seconds


class PublicWindowTests(unittest.TestCase):
    def test_boundaries(self):
        for timestamp, allowed in [
            ("2026-10-26T21:59:59+08:00", False),
            ("2026-10-26T22:00:00+08:00", True),
            ("2026-10-27T07:59:59+08:00", True),
            ("2026-10-27T08:00:00+08:00", False),
            ("2026-10-30T22:00:00+08:00", True),
            ("2026-10-31T07:59:59+08:00", True),
            ("2026-10-31T22:00:00+08:00", False),
            ("2026-11-01T23:00:00+08:00", False),
            ("2026-11-02T07:00:00+08:00", False),
        ]:
            with self.subTest(timestamp=timestamp):
                self.assertEqual(remaining_seconds(datetime.fromisoformat(timestamp)) > 0, allowed)

    def test_utc_and_remaining_time(self):
        self.assertEqual(remaining_seconds(datetime.fromisoformat("2026-10-26T14:00:00+00:00")), 36000)
        self.assertEqual(remaining_seconds(datetime.fromisoformat("2026-10-26T23:59:59+00:00")), 1)

    def test_reject_naive_time(self):
        with self.assertRaises(ValueError):
            remaining_seconds(datetime(2026, 10, 26, 22))
