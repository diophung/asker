"""Embedder: TEI calls via httpx.MockTransport — retries, dimension validation."""

import json

import httpx
import pytest

from enrich.embedder import (
    MAX_INPUT_CHARS,
    DimensionMismatchError,
    Embedder,
    EmbeddingError,
)

DIM = 4


async def _no_sleep(_delay: float) -> None:
    return None


def _make_embedder(handler, dim=DIM, attempts=3):
    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return Embedder("http://tei:80", dim, client=client, attempts=attempts, sleep=_no_sleep)


def _ok_handler(calls):
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/embed"
        inputs = json.loads(request.content)["inputs"]
        calls.append(inputs)
        return httpx.Response(200, json=[[float(len(t))] * DIM for t in inputs])

    return handler


async def test_embed_returns_dim_validated_vectors_in_order():
    calls: list[list[str]] = []
    embedder = _make_embedder(_ok_handler(calls))
    vectors = await embedder.embed(["a", "bb", "ccc"])
    assert vectors == [[1.0] * DIM, [2.0] * DIM, [3.0] * DIM]
    assert calls == [["a", "bb", "ccc"]]  # one batch, order preserved


async def test_embed_batches_large_inputs_across_calls():
    calls: list[list[str]] = []
    embedder = _make_embedder(_ok_handler(calls))
    texts = ["x" * 2048] * 8  # 512 estimated tokens each -> 6 + 2 split
    vectors = await embedder.embed(texts)
    assert len(vectors) == 8
    assert [len(c) for c in calls] == [6, 2]


async def test_embed_truncates_oversized_input_to_avoid_413():
    # A runaway chunk (e.g. base64/minified blob the chunker couldn't split)
    # must be truncated before it reaches TEI, or TEI returns 413 and the whole
    # document is dead-lettered (never indexed, not even keyword-searchable).
    calls: list[list[str]] = []
    embedder = _make_embedder(_ok_handler(calls))
    huge = "x" * 500_000
    vectors = await embedder.embed([huge])
    assert len(vectors) == 1
    assert len(calls) == 1 and len(calls[0]) == 1
    assert len(calls[0][0]) == MAX_INPUT_CHARS  # capped, not the raw 500 KB


async def test_embed_rejects_wrong_dimension():
    def handler(request: httpx.Request) -> httpx.Response:
        inputs = json.loads(request.content)["inputs"]
        return httpx.Response(200, json=[[0.0] * (DIM + 1) for _ in inputs])

    embedder = _make_embedder(handler)
    with pytest.raises(DimensionMismatchError, match="EMBEDDING_DIM"):
        await embedder.embed(["hello"])


async def test_embed_rejects_wrong_vector_count():
    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json=[[0.0] * DIM])  # one vector for two inputs

    embedder = _make_embedder(handler)
    with pytest.raises(EmbeddingError):
        await embedder.embed(["a", "b"])


async def test_embed_retries_then_succeeds():
    attempts = []

    def handler(request: httpx.Request) -> httpx.Response:
        attempts.append(1)
        if len(attempts) < 3:
            return httpx.Response(500, text="boom")
        inputs = json.loads(request.content)["inputs"]
        return httpx.Response(200, json=[[0.5] * DIM for _ in inputs])

    embedder = _make_embedder(handler)
    vectors = await embedder.embed(["hello"])
    assert vectors == [[0.5] * DIM]
    assert len(attempts) == 3


async def test_embed_fails_after_three_attempts():
    attempts = []

    def handler(_request: httpx.Request) -> httpx.Response:
        attempts.append(1)
        raise httpx.ConnectError("tei down")

    embedder = _make_embedder(handler)
    with pytest.raises(EmbeddingError, match="after 3 attempts"):
        await embedder.embed(["hello"])
    assert len(attempts) == 3


async def test_healthy_probes_tei_health():
    def up(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/health"
        return httpx.Response(200)

    ok, msg = await _make_embedder(up).healthy()
    assert ok and msg == "ok"

    def down(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(503)

    ok, msg = await _make_embedder(down).healthy()
    assert not ok and "503" in msg

    def unreachable(_request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("no route")

    ok, msg = await _make_embedder(unreachable).healthy()
    assert not ok and "unreachable" in msg
