"""TEI wire contract, bounded admission, and content-free diagnostics."""
from __future__ import annotations

import json
import logging
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .admission import (
    Admission,
    AdmissionRejected,
    caller_deadline,
    parse_json,
    read_body,
)
from .config import Config
from .encoder import BusyError

log = logging.getLogger(__name__)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self.server.config.socket_timeout_seconds)

    def log_message(self, _format: str, *_args: object) -> None:
        # Request paths/bodies may contain personal data; log only bounded stages.
        pass

    def send_json(self, status: int, data: object, **headers: str) -> None:
        body = json.dumps(data, allow_nan=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for name, value in headers.items():
            self.send_header(name, value)
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def fail(self, status: int, message: str) -> None:
        self.close_connection = True
        self.send_json(status, {"error": message}, Connection="close")

    def do_GET(self) -> None:
        path = self.path.split("?", 1)[0]
        if path not in ("/health", "/identity"):
            self.fail(404, "not found")
            return
        encoder = self.server.encoder
        self.send_json(200 if encoder.ready else 503,
                       {"status": "ok" if encoder.ready else "loading", **encoder.identity(),
                        "admission": self.server.admission.snapshot()})

    def do_POST(self) -> None:
        if self.path != "/embed":
            self.fail(404, "not found")
            return
        config = self.server.config
        request_started = time.monotonic()
        if self.headers.get_all("Transfer-Encoding"):
            self.fail(400, "chunked requests are unsupported")
            return
        lengths = self.headers.get_all("Content-Length", [])
        if not lengths:
            self.fail(411, "Content-Length required")
            return
        try:
            if len(lengths) != 1 or not lengths[0].isascii() or not lengths[0].isdecimal():
                raise ValueError("invalid length")
            length = int(lengths[0])
            deadline = caller_deadline(self.headers, request_started)
        except ValueError:
            self.fail(400, "invalid Content-Length or caller timeout")
            return
        if length > config.max_body_bytes:
            self.fail(413, "request body too large")
            return
        try:
            body_deadline = request_started + config.body_timeout_ms / 1000
            if deadline is not None:
                body_deadline = min(body_deadline, deadline)
            raw = read_body(self.rfile, self.connection, length, body_deadline)
            self.connection.settimeout(config.socket_timeout_seconds)
            if len(raw) != length:
                self.fail(400, "incomplete body")
                return
            payload = parse_json(raw)
        except (ValueError, UnicodeError, RecursionError):
            self.fail(400, "invalid JSON")
            return
        except (TimeoutError, OSError):
            self.fail(408, "body timeout")
            return
        inputs = payload.get("inputs") if isinstance(payload, dict) else None
        texts = [inputs] if isinstance(inputs, str) else inputs
        if not isinstance(texts, list) or any(not isinstance(text, str) for text in texts):
            self.fail(400, "inputs must be a string or an array of strings")
            return
        if len(texts) > config.max_inputs or any(len(text) > config.max_chars for text in texts) or sum(map(len, texts)) > config.max_total_chars:
            self.fail(413, "input bounds exceeded; split the batch")
            return
        if not self.server.encoder.ready:
            self.fail(503, "model loading")
            return
        started = time.monotonic()
        try:
            with self.server.admission.reserve(deadline) as wait_ms:
                result = self.server.encoder.embed(texts)
        except AdmissionRejected as exc:
            self.fail(503, str(exc))
            return
        except BusyError:
            self.fail(503, "encoder busy; retry within caller budget")
            return
        except Exception:  # noqa: BLE001 - do not return personal model input in exception bodies
            log.error("native embedding failed")
            self.fail(500, "embedding failed")
            return
        elapsed = round((time.monotonic() - started) * 1000, 3)
        log.info("embedding complete count=%d elapsed_ms=%s queue_wait_ms=%s", len(texts), elapsed, wait_ms)
        self.send_json(200, result, **{"X-Embedding-Elapsed-Ms": str(elapsed),
                                     "X-Embedding-Queue-Wait-Ms": str(wait_ms),
                                     "X-Embedding-Fingerprint": self.server.encoder.identity()["fingerprint"]})


class BoundedServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, config: Config, encoder: object) -> None:
        self.config, self.encoder = config, encoder
        self.admission = Admission(config.admission_wait_ms, config.max_waiting)
        self.request_queue_size = config.max_connections
        self._connections = threading.BoundedSemaphore(config.max_connections)
        super().__init__((config.host, config.port), Handler)

    def process_request(self, request: socket.socket, client_address: tuple) -> None:
        if not self._connections.acquire(blocking=False):
            request.settimeout(1)
            try:
                request.sendall(b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            except OSError:
                pass
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self._connections.release()
            raise

    def process_request_thread(self, request: socket.socket, client_address: tuple) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._connections.release()
