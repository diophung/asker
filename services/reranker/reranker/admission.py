"""Content-free FIFO admission with one active model call and a finite wait."""
from __future__ import annotations

import json
import threading
import time
from collections import deque
from contextlib import contextmanager


class AdmissionRejected(Exception):
    """Work was not admitted; callers retain their own deadline and retry policy."""


class Admission:
    def __init__(self, wait_ms: int, max_waiting: int) -> None:
        self.wait_ms, self.max_waiting = wait_ms, max_waiting
        self._condition = threading.Condition()
        self._waiting: deque[object] = deque()
        self._active = False
        self._accepted = self._released = self._queue_full = self._expired = 0
        self._total_wait_ms = self._largest_wait_ms = 0.0

    def snapshot(self) -> dict:
        with self._condition:
            return {"maxInferenceConcurrency": 1, "maxWaiting": self.max_waiting,
                    "maxWaitMs": self.wait_ms, "inFlight": int(self._active),
                    "waiting": len(self._waiting), "accepted": self._accepted,
                    "released": self._released, "rejectedQueueFull": self._queue_full,
                    "rejectedWaitTimeout": self._expired,
                    "totalAcceptedWaitMs": round(self._total_wait_ms, 3),
                    "largestAcceptedWaitMs": round(self._largest_wait_ms, 3)}

    @contextmanager
    def reserve(self, deadline: float | None = None):
        started = time.monotonic()
        expires = started + self.wait_ms / 1000
        if deadline is not None:
            expires = min(expires, deadline)
        with self._condition:
            # An expired caller must not get the idle slot either.
            if deadline is not None and deadline <= time.monotonic():
                self._expired += 1
                raise AdmissionRejected("admission deadline exceeded")
            if not self._active and not self._waiting:
                self._active = True
            else:
                if not self.wait_ms or len(self._waiting) >= self.max_waiting:
                    self._queue_full += 1
                    raise AdmissionRejected("inference queue full")
                ticket = object()
                self._waiting.append(ticket)
                try:
                    while True:
                        remaining = expires - time.monotonic()
                        if remaining <= 0:
                            self._expired += 1
                            raise AdmissionRejected("admission deadline exceeded")
                        if not self._active and self._waiting[0] is ticket:
                            self._waiting.popleft()
                            self._active = True
                            break
                        self._condition.wait(remaining)
                finally:
                    if ticket in self._waiting:
                        self._waiting.remove(ticket)
                        self._condition.notify_all()
            waited_ms = (time.monotonic() - started) * 1000
            self._accepted += 1
            self._total_wait_ms += waited_ms
            self._largest_wait_ms = max(self._largest_wait_ms, waited_ms)
        try:
            yield round(waited_ms, 3)
        finally:
            with self._condition:
                self._active = False
                self._released += 1
                self._condition.notify_all()


def caller_deadline(headers: object, started: float) -> float | None:
    """Optional remaining caller budget; it can only shorten local body/queue bounds."""
    values = headers.get_all("X-Request-Timeout-Ms", [])
    if not values:
        return None
    if len(values) != 1 or not values[0].isascii() or not values[0].isdecimal():
        raise ValueError("invalid X-Request-Timeout-Ms")
    milliseconds = int(values[0])
    if not 1 <= milliseconds <= 30000:
        raise ValueError("X-Request-Timeout-Ms must be in 1..30000")
    return started + milliseconds / 1000


def read_body(stream: object, connection: object, length: int, deadline: float) -> bytes:
    """An absolute deadline prevents a trickle of bytes resetting socket timeouts."""
    chunks = []
    remaining = length
    while remaining:
        seconds = deadline - time.monotonic()
        if seconds <= 0:
            raise TimeoutError("request body deadline exceeded")
        connection.settimeout(seconds)
        chunk = stream.read1(min(remaining, 65536))
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def parse_json(raw: bytes) -> object:
    def reject_constant(_value: str) -> None:
        raise ValueError("non-finite JSON constant")
    return json.loads(raw, parse_constant=reject_constant)
