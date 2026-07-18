"""Shared test fixtures: a deterministic fake scorer + a live test server.

The fake returns a score per document WITHOUT importing torch / sentence-
transformers or downloading a model (the real model is too heavy for unit tests;
the real-model check is an integration smoke, see README).
"""

from __future__ import annotations

import threading

import pytest

from reranker.config import Config
from reranker.scorer import ScoreError
from reranker.server import make_server


class FakeScorer:
    """Deterministic Scorer.

    By default each document scores as its character length (so ordering is
    predictable). `scores` overrides with a fixed list. `not_loaded` / `fail`
    drive the error paths the server maps to 503 / 500.
    """

    def __init__(
        self,
        *,
        scores: list[float] | None = None,
        not_loaded: bool = False,
        fail: bool = False,
    ) -> None:
        self._scores = scores
        self.not_loaded = not_loaded
        self.fail = fail
        self.calls: list[tuple[str, list[str]]] = []

    def score(self, query: str, documents: list[str]) -> list[float]:
        self.calls.append((query, list(documents)))
        if self.not_loaded:
            raise ScoreError("reranker model is not loaded yet")
        if self.fail:
            raise ScoreError("scoring boom")
        if self._scores is not None:
            return list(self._scores)
        return [float(len(d)) for d in documents]


@pytest.fixture
def fake_scorer() -> FakeScorer:
    return FakeScorer()


def serve(scorer: FakeScorer, *, ready: bool = True, config: Config | None = None):
    """Start a real ThreadingHTTPServer on an ephemeral port; yield its base URL."""
    cfg = config or Config(host="127.0.0.1", port=0)
    server = make_server(cfg, scorer, ready=lambda: ready)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return f"http://127.0.0.1:{port}", server, thread


@pytest.fixture
def live_server(fake_scorer: FakeScorer):
    base_url, server, thread = serve(fake_scorer)
    try:
        yield base_url, fake_scorer
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
