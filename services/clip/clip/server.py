"""Stdlib HTTP layer for the CLIP service (no extra deps, ADR-013 contract).

Routes (all on the one CLIP_PORT listener):
  POST /embed/text  {"inputs":[...]}      -> {"embeddings":[[float x CLIP_DIM], ...]}
  POST /embed/image {"images_b64":[...]}  -> {"embeddings":[[float x CLIP_DIM], ...]}
  GET  /health                            -> 200 {"status":"ok"} once loaded, else 503

The handler depends only on the `Encoder` Protocol and a `ready` callable, so
the request/response machinery is unit-tested against a deterministic fake
encoder with no torch / open_clip / model download.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .config import Config
from .encoder import (
    DimensionMismatchError,
    EncodeError,
    Encoder,
    decode_images_b64,
)

log = logging.getLogger("clip.server")


class ClipHandler(BaseHTTPRequestHandler):
    """One handler instance per request (threaded server).

    The encoder, config and readiness probe are bound to the server instance
    (see `make_server`) rather than passed per-request, matching how
    BaseHTTPRequestHandler is constructed.
    """

    # Bound on the server instance in make_server(); declared for type clarity.
    server_version = "asker-clip/0.1"
    protocol_version = "HTTP/1.1"

    @property
    def _encoder(self) -> Encoder:
        return self.server.encoder  # type: ignore[attr-defined]

    @property
    def _config(self) -> Config:
        return self.server.config  # type: ignore[attr-defined]

    @property
    def _ready(self) -> Callable[[], bool]:
        return self.server.ready  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: object) -> None:
        # Route the stdlib access log through structured logging; never log
        # request bodies (they carry tenant content) — only method/path/status.
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
        if path == "/embed/text":
            self._handle_embed_text()
        elif path == "/embed/image":
            self._handle_embed_image()
        else:
            self._send_error(404, "not found")

    # --- handlers ----------------------------------------------------------

    def _handle_health(self) -> None:
        if self._ready():
            self._send_json(200, {"status": "ok"})
        else:
            self._send_json(503, {"status": "loading"})

    def _handle_embed_text(self) -> None:
        body = self._read_json()
        if body is None:
            return
        inputs = body.get("inputs")
        if not self._valid_str_list(inputs, "inputs"):
            return
        if len(inputs) > self._config.max_texts:
            self._send_error(413, f"too many inputs: {len(inputs)} > max {self._config.max_texts}")
            return
        try:
            embeddings = self._encoder.embed_text(list(inputs))
        except DimensionMismatchError as exc:
            log.error("clip dimension mismatch", extra={"error": str(exc)})
            self._send_error(500, "model dimension mismatch")
            return
        except EncodeError as exc:
            self._send_error(503, str(exc))
            return
        self._send_json(200, {"embeddings": embeddings})

    def _handle_embed_image(self) -> None:
        body = self._read_json()
        if body is None:
            return
        images_b64 = body.get("images_b64")
        if not self._valid_str_list(images_b64, "images_b64"):
            return
        if len(images_b64) > self._config.max_images:
            self._send_error(
                413, f"too many images: {len(images_b64)} > max {self._config.max_images}"
            )
            return
        try:
            raw = decode_images_b64(list(images_b64))
            embeddings = self._encoder.embed_image(raw)
        except DimensionMismatchError as exc:
            log.error("clip dimension mismatch", extra={"error": str(exc)})
            self._send_error(500, "model dimension mismatch")
            return
        except EncodeError as exc:
            # Bad image bytes are a client error; "not loaded" is a 503 — both
            # surface as EncodeError, so distinguish on the message.
            status = 503 if "not loaded" in str(exc) else 400
            self._send_error(status, str(exc))
            return
        self._send_json(200, {"embeddings": embeddings})

    # --- request/response helpers -----------------------------------------

    def _read_json(self) -> dict | None:
        """Read + parse the JSON body, enforcing the body-size cap.

        On any problem an error response is already sent and None is returned;
        callers must stop processing when this returns None.
        """
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
        """Validate value is a list[str]; send a 400 and return False if not."""
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


def make_server(config: Config, encoder: Encoder, ready: Callable[[], bool]) -> ThreadingHTTPServer:
    """Build a threading HTTP server bound to (config.host, config.port).

    `ready()` decides /health (200 vs 503) so the model can load lazily without
    the listener being down. The encoder and config are attached to the server
    instance for the per-request handler to read.
    """
    server = ThreadingHTTPServer((config.host, config.port), ClipHandler)
    server.encoder = encoder  # type: ignore[attr-defined]
    server.config = config  # type: ignore[attr-defined]
    server.ready = ready  # type: ignore[attr-defined]
    return server
