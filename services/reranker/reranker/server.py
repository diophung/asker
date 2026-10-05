"""Stdlib HTTP layer for the reranker service (no web-framework dependency).

Routes (all on the one RERANKER_PORT listener):
  POST /rerank  {"query":"...","documents":["...",...]}
                                       -> {"scores":[float, ...]}   # input order
  GET  /health                         -> 200 {"status":"ok"} once loaded, else 503

The handler depends only on the `Scorer` Protocol and a `ready` callable, so the
request/response machinery is unit-tested against a deterministic fake scorer
with no torch / sentence-transformers / model download.
"""

from __future__ import annotations

import json
import logging
import math
import socket
import threading
import time
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .admission import Admission, AdmissionRejected, caller_deadline, parse_json, read_body
from .config import Config
from .scorer import ScoreError, Scorer

log = logging.getLogger("reranker.server")


class RerankHandler(BaseHTTPRequestHandler):
    """One handler instance per request (threaded server)."""

    server_version = "asker-reranker/0.1"
    protocol_version = "HTTP/1.1"

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self._config.socket_timeout_seconds)

    @property
    def _scorer(self) -> Scorer:
        return self.server.scorer  # type: ignore[attr-defined]

    @property
    def _config(self) -> Config:
        return self.server.config  # type: ignore[attr-defined]

    @property
    def _ready(self) -> Callable[[], bool]:
        return self.server.ready  # type: ignore[attr-defined]

    def log_message(self, _fmt: str, *_args: object) -> None:
        # Request targets as well as bodies can carry private content.
        pass

    # --- routing -----------------------------------------------------------

    def do_GET(self) -> None:  # BaseHTTPRequestHandler dispatch name
        path = self.path.split("?", 1)[0]
        if path in ("/health", "/identity"):
            self._handle_health()
        else:
            self._send_error(404, "not found")

    def do_POST(self) -> None:  # BaseHTTPRequestHandler dispatch name
        path = self.path.split("?", 1)[0]
        if path == "/rerank":
            self._handle_rerank()
        else:
            self._send_error(404, "not found")

    # --- handlers ----------------------------------------------------------

    def _handle_health(self) -> None:
        identity = getattr(self._scorer, "identity", None)
        metadata = identity() if callable(identity) else {
            "model": self._config.model, "requestedRevision": self._config.revision,
            "resolvedRevision": None, "revisionVerified": False,
            "device": None, "precision": None,
        }
        if self._ready():
            self._send_json(200, {"status": "ok", **metadata,
                                  "admission": self.server.admission.snapshot()})
        else:
            self._send_json(503, {"status": "loading", **metadata,
                                  "admission": self.server.admission.snapshot()})

    def _handle_rerank(self) -> None:
        if not self._ready():
            self._send_error(503, "reranker model is not loaded yet")
            return
        started = time.monotonic()
        try:
            deadline = caller_deadline(self.headers, started)
        except ValueError:
            self._send_error(400, "invalid caller timeout")
            return
        body = self._read_json(started, deadline)
        if body is None:
            return
        query = body.get("query")
        if not isinstance(query, str):
            self._send_error(400, "query must be a string")
            return
        documents = body.get("documents")
        if isinstance(documents, list) and len(documents) > self._config.max_documents:
            self._send_error(413, "too many documents")
            return
        if not self._valid_str_list(documents, "documents"):
            return
        if len(query) + sum(map(len, documents)) > self._config.max_total_chars:
            self._send_error(413, "total input bounds exceeded")
            return

        # Defensive server-side truncation (the caller already caps, but a
        # cross-encoder pair should never blow past the configured lengths).
        query = query[: self._config.max_query_chars]
        docs = [d[: self._config.max_doc_chars] for d in documents]
        try:
            with self.server.admission.reserve(deadline) as wait_ms:
                scores = self._scorer.score(query, docs)
        except AdmissionRejected as exc:
            self._send_error(503, str(exc))
            return
        except ScoreError as exc:
            # "not loaded" is a transient 503; anything else is a 500.
            status = 503 if "not loaded" in str(exc) else 500
            self._send_error(
                status, "model loading" if status == 503 else "reranker scoring failed"
            )
            return
        except Exception:
            self._send_error(500, "reranker inference failed")
            return
        if not isinstance(scores, list) or len(scores) != len(docs) or any(
            type(score) not in (int, float) or not math.isfinite(score) for score in scores
        ):
            self._send_error(500, "invalid reranker scores")
            return
        log.info("rerank complete", extra={
            "count": len(docs), "queue_wait_ms": wait_ms,
            "elapsed_ms": round((time.monotonic() - started) * 1000, 3),
        })
        self._send_json(200, {"scores": scores}, **{"X-Rerank-Queue-Wait-Ms": str(wait_ms)})

    # --- request/response helpers -----------------------------------------

    def _read_json(self, started: float, deadline: float | None) -> dict | None:
        if self.headers.get_all("Transfer-Encoding"):
            self._send_error(400, "chunked requests are unsupported")
            return None
        lengths = self.headers.get_all("Content-Length", [])
        if not lengths:
            self._send_error(411, "Content-Length required")
            return None
        try:
            if len(lengths) != 1 or not lengths[0].isascii() or not lengths[0].isdecimal():
                raise ValueError("invalid length")
            length = int(lengths[0])
        except ValueError:
            self._send_error(400, "invalid Content-Length")
            return None
        if length > self._config.max_body_bytes:
            self._send_error(413, f"request body too large: {length} bytes")
            return None
        try:
            body_deadline = started + self._config.body_timeout_ms / 1000
            if deadline is not None:
                body_deadline = min(body_deadline, deadline)
            raw = read_body(self.rfile, self.connection, length, body_deadline)
            self.connection.settimeout(self._config.socket_timeout_seconds)
        except (TimeoutError, OSError):
            self._send_error(408, "request body timeout")
            return None
        if len(raw) != length:
            self._send_error(400, "incomplete request body")
            return None
        try:
            parsed = parse_json(raw)
        except (ValueError, UnicodeDecodeError, RecursionError):
            self._send_error(400, "request body is not valid JSON")
            return None
        if not isinstance(parsed, dict):
            self._send_error(400, "request body must be a JSON object")
            return None
        return parsed

    def _valid_str_list(self, value: object, field: str) -> bool:
        if not isinstance(value, list):
            self._send_error(400, f"{field} must be a list of strings")
            return False
        for item in value:
            if not isinstance(item, str):
                self._send_error(400, f"{field} must contain only strings")
                return False
        return True

    def _send_json(self, status: int, payload: dict, **headers: str) -> None:
        body = json.dumps(payload, allow_nan=False).encode("utf-8")
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

    def _send_error(self, status: int, message: str) -> None:
        # A rejected body may be unread; it must not become the next request.
        self.close_connection = True
        self._send_json(status, {"error": message}, Connection="close")


class BoundedHTTPServer(ThreadingHTTPServer):
    """Finite body-reading threads and FIFO waiting for one model inference slot."""

    daemon_threads = True

    def __init__(self, config: Config, scorer: Scorer, ready: Callable[[], bool]) -> None:
        self.config, self.scorer, self.ready = config, scorer, ready
        self.admission = Admission(config.admission_wait_ms, config.max_waiting)
        self.request_queue_size = config.max_connections
        self._connections = threading.BoundedSemaphore(config.max_connections)
        super().__init__((config.host, config.port), RerankHandler)

    def process_request(self, request: socket.socket, client_address: tuple) -> None:
        if not self._connections.acquire(blocking=False):
            request.settimeout(1)
            try:
                request.sendall(
                    b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n"
                    b"Connection: close\r\n\r\n"
                )
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


def make_server(config: Config, scorer: Scorer, ready: Callable[[], bool]) -> ThreadingHTTPServer:
    """Build a threading HTTP server bound to (config.host, config.port).

    `ready()` decides /health (200 vs 503) so the model can load lazily without
    the listener being down.
    """
    return BoundedHTTPServer(config, scorer, ready)
