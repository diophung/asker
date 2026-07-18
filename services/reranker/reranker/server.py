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
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .config import Config
from .scorer import ScoreError, Scorer

log = logging.getLogger("reranker.server")


class RerankHandler(BaseHTTPRequestHandler):
    """One handler instance per request (threaded server)."""

    server_version = "asker-reranker/0.1"
    protocol_version = "HTTP/1.1"

    @property
    def _scorer(self) -> Scorer:
        return self.server.scorer  # type: ignore[attr-defined]

    @property
    def _config(self) -> Config:
        return self.server.config  # type: ignore[attr-defined]

    @property
    def _ready(self) -> Callable[[], bool]:
        return self.server.ready  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: object) -> None:
        # Never log request bodies (they carry tenant content) — only the access line.
        log.info("request", extra={"client": self.address_string(), "line": fmt % args})

    # --- routing -----------------------------------------------------------

    def do_GET(self) -> None:  # BaseHTTPRequestHandler dispatch name
        path = self.path.split("?", 1)[0]
        if path == "/health":
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
        if self._ready():
            self._send_json(200, {"status": "ok"})
        else:
            self._send_json(503, {"status": "loading"})

    def _handle_rerank(self) -> None:
        body = self._read_json()
        if body is None:
            return
        query = body.get("query")
        if not isinstance(query, str):
            self._send_error(400, "query must be a string")
            return
        documents = body.get("documents")
        if not self._valid_str_list(documents, "documents"):
            return
        if len(documents) > self._config.max_documents:
            self._send_error(
                413, f"too many documents: {len(documents)} > max {self._config.max_documents}"
            )
            return

        # Defensive server-side truncation (the caller already caps, but a
        # cross-encoder pair should never blow past the configured lengths).
        query = query[: self._config.max_query_chars]
        docs = [d[: self._config.max_doc_chars] for d in documents]
        try:
            scores = self._scorer.score(query, docs)
        except ScoreError as exc:
            # "not loaded" is a transient 503; anything else is a 500.
            status = 503 if "not loaded" in str(exc) else 500
            self._send_error(status, str(exc))
            return
        self._send_json(200, {"scores": scores})

    # --- request/response helpers -----------------------------------------

    def _read_json(self) -> dict | None:
        length_raw = self.headers.get("Content-Length")
        if length_raw is None:
            self._send_error(411, "Content-Length required")
            return None
        try:
            length = int(length_raw)
        except ValueError:
            self._send_error(400, "invalid Content-Length")
            return None
        if length < 0:
            self._send_error(400, "invalid Content-Length")
            return None
        if length > self._config.max_body_bytes:
            self._send_error(413, f"request body too large: {length} bytes")
            return None
        raw = self.rfile.read(length)
        try:
            parsed = json.loads(raw or b"null")
        except (json.JSONDecodeError, UnicodeDecodeError):
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

    def _send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_error(self, status: int, message: str) -> None:
        self._send_json(status, {"error": message})


def make_server(config: Config, scorer: Scorer, ready: Callable[[], bool]) -> ThreadingHTTPServer:
    """Build a threading HTTP server bound to (config.host, config.port).

    `ready()` decides /health (200 vs 503) so the model can load lazily without
    the listener being down.
    """
    server = ThreadingHTTPServer((config.host, config.port), RerankHandler)
    server.scorer = scorer  # type: ignore[attr-defined]
    server.config = config  # type: ignore[attr-defined]
    server.ready = ready  # type: ignore[attr-defined]
    return server
