"""Deterministic fake work tests for FIFO, expiry, capacity and absolute body time."""
from __future__ import annotations

import io
import threading
import time
import unittest
from email.message import Message
from unittest.mock import patch

from local_inference.admission import (
    Admission,
    AdmissionRejected,
    caller_deadline,
    parse_json,
    read_body,
)


class AdmissionTest(unittest.TestCase):
    def queued(self, admission, count):
        expires = time.monotonic() + 2
        while admission.snapshot()["waiting"] != count and time.monotonic() < expires:
            threading.Event().wait(.001)
        self.assertEqual(admission.snapshot()["waiting"], count)

    def test_four_calls_run_fifo_in_one_slot_and_fifth_is_rejected(self):
        admission = Admission(1000, 3)
        order, errors, workers = [], [], []

        def work(label):
            try:
                with admission.reserve():
                    order.append(label)
            except AdmissionRejected as exc:
                errors.append(str(exc))

        with admission.reserve():
            for index, label in enumerate(("b", "c", "d"), start=1):
                worker = threading.Thread(target=work, args=(label,), daemon=True)
                workers.append(worker)
                worker.start()
                self.queued(admission, index)
            with self.assertRaises(AdmissionRejected), admission.reserve():
                self.fail("overflow work must never be admitted")
            during = admission.snapshot()
            self.assertEqual(during["inFlight"], 1)
            self.assertEqual(during["waiting"], 3)
            self.assertEqual(during["rejectedQueueFull"], 1)
        for worker in workers:
            worker.join(2)
            self.assertFalse(worker.is_alive())
        self.assertEqual(order, ["b", "c", "d"])
        self.assertEqual(errors, [])
        self.assertEqual(admission.snapshot()["accepted"], 4)
        self.assertEqual(admission.snapshot()["released"], 4)

    def test_waiting_caller_expires_without_beginning_work_or_leaking_queue(self):
        admission = Admission(500, 3)
        began, rejected = threading.Event(), threading.Event()

        def work():
            try:
                with admission.reserve(time.monotonic() + .05):
                    began.set()
            except AdmissionRejected:
                rejected.set()

        with admission.reserve():
            worker = threading.Thread(target=work, daemon=True)
            worker.start()
            self.queued(admission, 1)
            self.assertTrue(rejected.wait(2))
            self.assertFalse(began.is_set())
            self.assertEqual(admission.snapshot()["waiting"], 0)
        worker.join(2)
        with admission.reserve():
            pass
        self.assertEqual(admission.snapshot()["accepted"], 2)
        self.assertEqual(admission.snapshot()["rejectedWaitTimeout"], 1)

    def test_idle_expired_caller_is_not_admitted_and_failure_releases_slot(self):
        admission = Admission(100, 1)
        with self.assertRaises(AdmissionRejected), admission.reserve(time.monotonic() - 1):
            self.fail("expired work must never begin")
        with self.assertRaises(RuntimeError), admission.reserve():
            raise RuntimeError("fake inference failure")
        with admission.reserve():
            pass
        self.assertEqual(admission.snapshot()["inFlight"], 0)
        self.assertEqual(admission.snapshot()["released"], 2)

    def test_zero_wait_and_zero_queue_preserve_prompt_rejection(self):
        for admission in (Admission(0, 3), Admission(100, 0)):
            with admission.reserve():
                with self.assertRaises(AdmissionRejected), admission.reserve():
                    self.fail("busy work must never begin")
                self.assertEqual(admission.snapshot()["waiting"], 0)

    def test_lock_scheduling_delay_does_not_admit_expired_idle_request(self):
        admission = Admission(100, 1)
        with (patch("local_inference.admission.time.monotonic", side_effect=[0, .2]),
              self.assertRaises(AdmissionRejected), admission.reserve(.1)):
            self.fail("expired work must not begin after lock scheduling delay")
        self.assertEqual(admission.snapshot()["accepted"], 0)
        self.assertEqual(admission.snapshot()["rejectedWaitTimeout"], 1)

    def test_body_progress_does_not_reset_absolute_deadline(self):
        class Stream:
            calls = 0

            def read1(self, _length):
                self.calls += 1
                return b"x"

        class Connection:
            def __init__(self):
                self.timeouts = []

            def settimeout(self, seconds):
                self.timeouts.append(seconds)

        stream, connection = Stream(), Connection()
        with (patch("local_inference.admission.time.monotonic", side_effect=[0, .04, .09, .12]),
              self.assertRaises(TimeoutError)):
            read_body(stream, connection, 4, .1)
        self.assertEqual(stream.calls, 3)
        self.assertAlmostEqual(connection.timeouts[-1], .01)
        self.assertEqual(read_body(io.BytesIO(b"abc"), connection, 3, time.monotonic() + 1), b"abc")

    def test_caller_budget_validation_and_nonfinite_json(self):
        headers = Message()
        headers["X-Request-Timeout-Ms"] = "100"
        self.assertEqual(caller_deadline(headers, 10), 10.1)
        headers["X-Request-Timeout-Ms"] = "100"
        with self.assertRaises(ValueError):
            caller_deadline(headers, 10)
        for value in ("-1", "0", "30001", "1e3", " 100", ""):
            headers = Message()
            headers["X-Request-Timeout-Ms"] = value
            with self.assertRaises(ValueError):
                caller_deadline(headers, 10)
        for value in (b'{"other": NaN}', b'{"other": Infinity}'):
            with self.assertRaises(ValueError):
                parse_json(value)


if __name__ == "__main__":
    unittest.main()
