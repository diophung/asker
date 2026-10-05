from __future__ import annotations

import json
import socket
import threading
import time
import urllib.error
import urllib.request
from unittest.mock import patch

import pytest

from reranker.admission import read_body
from reranker.config import Config
from tests.conftest import FakeScorer, serve


def _post(base_url: str, path: str, payload: dict, headers=None) -> tuple[int, dict]:
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        base_url + path, data=body,
        headers={"Content-Type": "application/json", **(headers or {})}, method="POST"
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


def test_busy_inference_wait_is_bounded_and_health_remains_ready() -> None:
    entered, release = threading.Event(), threading.Event()

    class BlockingScorer(FakeScorer):
        def score(self, query, documents):
            entered.set()
            assert release.wait(3)
            return super().score(query, documents)

    base_url, server, thread = serve(BlockingScorer())
    worker = threading.Thread(
        target=_post, args=(base_url, "/rerank", {"query": "q", "documents": ["a"]})
    )
    try:
        worker.start()
        assert entered.wait(2)
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["b"]})[0] == 503
        assert not release.is_set()  # expiry completed while the first call is still held
        assert server.admission.snapshot()["rejectedWaitTimeout"] == 1
        assert _get(base_url, "/health")[0] == 200
    finally:
        release.set()
        worker.join(3)
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_identity_reports_runtime_metadata_and_loading_does_not_score() -> None:
    scorer = FakeScorer()
    scorer.identity = lambda: {"model": "fake", "device": "mps", "precision": "fp32"}
    base_url, server, thread = serve(scorer, ready=False)
    try:
        status, body = _get(base_url, "/identity")
        assert status == 503
        assert body["model"] == "fake"
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 503
        assert not scorer.calls
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_admission_released_after_failed_inference(live_server) -> None:
    base_url, scorer = live_server
    scorer.fail = True
    assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 500
    scorer.fail = False
    assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 200


@pytest.mark.parametrize("scores", [[float("nan")], [float("inf")], ["invalid"], [True], []])
def test_invalid_scores_fail_closed(scores) -> None:
    base_url, server, thread = serve(FakeScorer(scores=scores))
    try:
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 500
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_stalled_body_times_out_and_releases_admission() -> None:
    cfg = Config(host="127.0.0.1", port=0, socket_timeout_seconds=1)
    base_url, server, thread = serve(FakeScorer(), config=cfg)
    try:
        with socket.create_connection(server.server_address, timeout=3) as connection:
            connection.sendall(
                b"POST /rerank HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n"
            )
            response = connection.recv(4096)
            assert b"408" in response
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 200
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_connection_capacity_rejects_without_allocating_a_thread() -> None:
    cfg = Config(host="127.0.0.1", port=0, max_connections=1)
    base_url, server, thread = serve(FakeScorer(), config=cfg)
    try:
        # Reserve the finite connection admission slot without a slow/body race.
        server._connections.acquire()
        with socket.create_connection(server.server_address, timeout=2) as connection:
            connection.sendall(b"GET /health HTTP/1.1\r\nHost: localhost\r\n\r\n")
            assert b"503" in connection.recv(4096)
        server._connections.release()
        assert _get(base_url, "/health")[0] == 200
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_four_http_requests_reuse_one_inference_slot_without_busy_fallback() -> None:
    entered, release = threading.Event(), threading.Event()

    class BlockingScorer(FakeScorer):
        def score(self, query, documents):
            if not self.calls:
                entered.set()
                assert release.wait(2)
            return super().score(query, documents)

    scorer = BlockingScorer()
    cfg = Config(host="127.0.0.1", port=0, admission_wait_ms=750)
    base_url, server, thread = serve(scorer, config=cfg)
    results, workers = [], []
    try:
        for index, label in enumerate(("a", "b", "c", "d")):
            worker = threading.Thread(target=lambda query=label: results.append(
                _post(base_url, "/rerank", {"query": query, "documents": ["doc"]})),
                daemon=True)
            workers.append(worker)
            worker.start()
            if index == 0:
                assert entered.wait(1)
            else:
                expires = time.monotonic() + 1
                while (server.admission.snapshot()["waiting"] != index
                       and time.monotonic() < expires):
                    threading.Event().wait(.001)
                assert server.admission.snapshot()["waiting"] == index
        assert _get(base_url, "/health")[0] == 200
        assert _get(base_url, "/identity")[1]["admission"]["waiting"] == 3
    finally:
        release.set()
        for worker in workers:
            worker.join(2)
        server.shutdown()
        server.server_close()
        thread.join(3)
    assert [query for query, _docs in scorer.calls] == ["a", "b", "c", "d"]
    assert [status for status, _body in results] == [200] * 4


def test_caller_deadline_expires_before_scoring() -> None:
    scorer = FakeScorer()
    base_url, server, thread = serve(scorer)
    try:
        with server.admission.reserve():
            status, _body = _post(base_url, "/rerank", {"query": "q", "documents": ["a"]},
                                  {"X-Request-Timeout-Ms": "50"})
        assert status == 503
        assert not scorer.calls
        assert server.admission.snapshot()["rejectedWaitTimeout"] == 1
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 200
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_stalled_body_does_not_reserve_inference() -> None:
    reading = threading.Event()

    def observe(stream, connection, length, deadline):
        if length == 100:
            reading.set()
        return read_body(stream, connection, length, deadline)

    base_url, server, thread = serve(FakeScorer())
    try:
        with patch("reranker.server.read_body", side_effect=observe):
            with socket.create_connection(server.server_address, timeout=2) as connection:
                connection.sendall(
                    b"POST /rerank HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n"
                )
                assert reading.wait(1)
                assert _post(base_url, "/rerank", {"query": "q", "documents": ["a"]})[0] == 200
                assert _get(base_url, "/health")[0] == 200
                assert b"408" in connection.recv(4096)
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_total_input_bound_applies_before_scoring_and_releases_admission() -> None:
    scorer = FakeScorer()
    cfg = Config(host="127.0.0.1", port=0, max_total_chars=5)
    base_url, server, thread = serve(scorer, config=cfg)
    try:
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["abc", "de"]})[0] == 413
        assert not scorer.calls
        assert server.admission.snapshot()["accepted"] == 0
        assert _post(base_url, "/rerank", {"query": "q", "documents": ["abc"]})[0] == 200
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)


def test_ambiguous_framing_and_invalid_json_do_not_reserve_inference() -> None:
    base_url, server, thread = serve(FakeScorer())
    try:
        for extra in (b"Content-Length: 2\r\nContent-Length: 2\r\n",
                      b"Transfer-Encoding:\r\nContent-Length: 2\r\n"):
            with socket.create_connection(server.server_address, timeout=2) as connection:
                connection.sendall(
                    b"POST /rerank HTTP/1.1\r\nHost: localhost\r\n" + extra + b"\r\n{}"
                )
                assert b"400" in connection.recv(4096)
        for raw in (b'{"query":"q","documents":[],"extra":NaN}',
                    b"[" * 1200 + b"0" + b"]" * 1200):
            request = urllib.request.Request(base_url + "/rerank", data=raw, method="POST")
            with pytest.raises(urllib.error.HTTPError) as caught:
                urllib.request.urlopen(request)
            assert caught.value.code == 400
            caught.value.close()
        assert server.admission.snapshot()["accepted"] == 0
    finally:
        server.shutdown()
        server.server_close()
        thread.join(3)
