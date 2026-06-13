"""Shared test fixtures: a deterministic fake encoder + a live test server.

The fake returns CLIP_DIM-length unit vectors derived from each input so tests
can assert shape, normalization and order WITHOUT importing torch / open_clip
or downloading a model (the real model is too heavy for unit tests; the
real-model check is an integration smoke, see README).
"""

from __future__ import annotations

import math
import threading

import pytest

from clip.config import Config
from clip.encoder import DimensionMismatchError, EncodeError, l2_normalize
from clip.server import make_server

TEST_DIM = 8


class FakeEncoder:
    """Deterministic Encoder: each input -> a distinct CLIP_DIM unit vector.

    Optional flags drive the error paths: `fail_text` / `fail_image` raise an
    EncodeError, and `wrong_dim` returns a non-CLIP_DIM vector (to exercise the
    DimensionMismatch handling in the server).
    """

    def __init__(
        self,
        dim: int = TEST_DIM,
        *,
        fail_text: bool = False,
        fail_image: bool = False,
        wrong_dim: bool = False,
        not_loaded: bool = False,
    ) -> None:
        self._dim = dim
        self.fail_text = fail_text
        self.fail_image = fail_image
        self.wrong_dim = wrong_dim
        self.not_loaded = not_loaded
        self.text_calls: list[list[str]] = []
        self.image_calls: list[list[bytes]] = []

    @property
    def clip_dim(self) -> int:
        return self._dim

    def _vector(self, seed: int) -> list[float]:
        if self.wrong_dim:
            raise DimensionMismatchError("model returned a vector of length 999, want CLIP_DIM=8")
        # A non-trivial, non-uniform vector so l2_normalize is actually tested.
        raw = [math.sin(seed + i + 1) for i in range(self._dim)]
        return l2_normalize(raw)

    def embed_text(self, inputs: list[str]) -> list[list[float]]:
        self.text_calls.append(list(inputs))
        if self.not_loaded:
            raise EncodeError("clip model is not loaded yet")
        if self.fail_text:
            raise EncodeError("text encode boom")
        return [self._vector(hash(t) % 1000) for t in inputs]

    def embed_image(self, images: list[bytes]) -> list[list[float]]:
        self.image_calls.append(list(images))
        if self.not_loaded:
            raise EncodeError("clip model is not loaded yet")
        if self.fail_image:
            raise EncodeError("images[0] could not be decoded: bad")
        return [self._vector(len(raw)) for raw in images]


@pytest.fixture
def fake_encoder() -> FakeEncoder:
    return FakeEncoder()


def serve(encoder: FakeEncoder, *, ready: bool = True, config: Config | None = None):
    """Start a real ThreadingHTTPServer on an ephemeral port; yield its base URL.

    Returns (base_url, server, thread); the caller is responsible for shutdown,
    or use the `live_server` fixture which tears down automatically.
    """
    cfg = config or Config(clip_dim=encoder.clip_dim, host="127.0.0.1", port=0)
    server = make_server(cfg, encoder, ready=lambda: ready)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return f"http://127.0.0.1:{port}", server, thread


@pytest.fixture
def live_server(fake_encoder: FakeEncoder):
    base_url, server, thread = serve(fake_encoder)
    try:
        yield base_url, fake_encoder
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
