from __future__ import annotations

import json
import urllib.error
import urllib.request

import pytest

from reranker.config import Config
from tests.conftest import FakeScorer, serve


def _post(base_url: str, path: str, payload: dict) -> tuple[int, dict]:
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        base_url + path, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def _get(base_url: str, path: str) -> tuple[int, dict]:
    try:
        with urllib.request.urlopen(base_url + path) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def test_rerank_returns_scores_in_order(live_server) -> None:
    base_url, fake = live_server
    status, body = _post(base_url, "/rerank", {"query": "q", "documents": ["a", "abcd", "ab"]})
    assert status == 200
    # FakeScorer scores by length, IN INPUT ORDER (the service does not sort).
    assert body["scores"] == [1.0, 4.0, 2.0]
    assert fake.calls[0][0] == "q"


def test_rerank_empty_documents_ok(live_server) -> None:
    base_url, _ = live_server
    status, body = _post(base_url, "/rerank", {"query": "q", "documents": []})
    assert status == 200
    assert body["scores"] == []


@pytest.mark.parametrize(
    "payload",
    [
        {"documents": ["a"]},  # missing query
        {"query": 5, "documents": ["a"]},  # non-string query
        {"query": "q"},  # missing documents
        {"query": "q", "documents": "notalist"},
        {"query": "q", "documents": [1, 2]},  # non-string items
    ],
)
def test_rerank_bad_request_is_400(live_server, payload: dict) -> None:
    base_url, _ = live_server
    status, _ = _post(base_url, "/rerank", payload)
    assert status == 400


def test_rerank_too_many_documents_is_413() -> None:
    scorer = FakeScorer()
    cfg = Config(host="127.0.0.1", port=0, max_documents=2)
    base_url, server, thread = serve(scorer, config=cfg)
    try:
        status, _ = _post(base_url, "/rerank", {"query": "q", "documents": ["a", "b", "c"]})
        assert status == 413
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_rerank_truncates_long_inputs() -> None:
    scorer = FakeScorer()
    cfg = Config(host="127.0.0.1", port=0, max_query_chars=3, max_doc_chars=4)
    base_url, server, thread = serve(scorer, config=cfg)
    try:
        status, _ = _post(base_url, "/rerank", {"query": "abcdef", "documents": ["hello world"]})
        assert status == 200
        seen_query, seen_docs = scorer.calls[0]
        assert seen_query == "abc"  # capped to max_query_chars
        assert seen_docs == ["hell"]  # capped to max_doc_chars
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_not_loaded_scorer_is_503() -> None:
    scorer = FakeScorer(not_loaded=True)
    base_url, server, thread = serve(scorer)
    try:
        status, _ = _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})
        assert status == 503
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_scoring_failure_is_500() -> None:
    scorer = FakeScorer(fail=True)
    base_url, server, thread = serve(scorer)
    try:
        status, _ = _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})
        assert status == 500
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_health_ok_and_loading() -> None:
    scorer = FakeScorer()
    # ready=True -> 200
    base_url, server, thread = serve(scorer, ready=True)
    try:
        assert _get(base_url, "/health")[0] == 200
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
    # ready=False -> 503
    base_url, server, thread = serve(scorer, ready=False)
    try:
        assert _get(base_url, "/health")[0] == 503
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_unknown_route_is_404(live_server) -> None:
    base_url, _ = live_server
    assert _get(base_url, "/nope")[0] == 404
