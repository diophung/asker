"""HTTP layer tests against a deterministic fake encoder (no torch/model).

Covers routing, JSON shapes, base64 decode, batch handling, request bounds,
error cases, health, and the pinned response contract (embeddings are
CLIP_DIM-length unit vectors).
"""

from __future__ import annotations

import base64
import json
import math
import urllib.error
import urllib.request

import pytest

from clip.config import Config

from .conftest import TEST_DIM, FakeEncoder, serve


def _post(base_url: str, path: str, payload, *, raw: bytes | None = None):
    data = raw if raw is not None else json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(f"{base_url}{path}", data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def _get(base_url: str, path: str):
    try:
        with urllib.request.urlopen(f"{base_url}{path}", timeout=5) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def _is_unit(vec: list[float]) -> bool:
    return math.isclose(math.sqrt(sum(x * x for x in vec)), 1.0, rel_tol=1e-6)


# --- /embed/text -----------------------------------------------------------


def test_embed_text_returns_unit_vectors_in_order(live_server):
    base_url, encoder = live_server
    status, body = _post(base_url, "/embed/text", {"inputs": ["a cat", "a dog", "a bird"]})
    assert status == 200
    embeddings = body["embeddings"]
    assert len(embeddings) == 3
    assert all(len(v) == TEST_DIM for v in embeddings)
    assert all(_is_unit(v) for v in embeddings)
    assert encoder.text_calls == [["a cat", "a dog", "a bird"]]


def test_embed_text_empty_list_is_ok(live_server):
    base_url, _ = live_server
    status, body = _post(base_url, "/embed/text", {"inputs": []})
    assert status == 200
    assert body == {"embeddings": []}


def test_embed_text_missing_field_is_400(live_server):
    base_url, _ = live_server
    status, body = _post(base_url, "/embed/text", {"wrong": ["x"]})
    assert status == 400
    assert "inputs" in body["error"]


def test_embed_text_non_string_item_is_400(live_server):
    base_url, _ = live_server
    status, body = _post(base_url, "/embed/text", {"inputs": ["ok", 5]})
    assert status == 400
    assert "strings" in body["error"]


def test_embed_text_too_many_inputs_is_413():
    encoder = FakeEncoder()
    cfg = Config(clip_dim=TEST_DIM, host="127.0.0.1", port=0, max_texts=2)
    base_url, server, thread = serve(encoder, config=cfg)
    try:
        status, body = _post(base_url, "/embed/text", {"inputs": ["a", "b", "c"]})
        assert status == 413
        assert "too many" in body["error"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


# --- /embed/image ----------------------------------------------------------


def test_embed_image_decodes_base64_and_returns_unit_vectors(live_server):
    base_url, encoder = live_server
    img_a = base64.b64encode(b"PNGDATA-A").decode()
    img_b = base64.b64encode(b"PNGDATA-LONGER-B").decode()
    status, body = _post(base_url, "/embed/image", {"images_b64": [img_a, img_b]})
    assert status == 200
    embeddings = body["embeddings"]
    assert len(embeddings) == 2
    assert all(len(v) == TEST_DIM and _is_unit(v) for v in embeddings)
    # The encoder received raw decoded bytes, not the base64 strings.
    assert encoder.image_calls == [[b"PNGDATA-A", b"PNGDATA-LONGER-B"]]


def test_embed_image_accepts_data_uri_prefix(live_server):
    base_url, encoder = live_server
    encoded = "data:image/png;base64," + base64.b64encode(b"BYTES").decode()
    status, _ = _post(base_url, "/embed/image", {"images_b64": [encoded]})
    assert status == 200
    assert encoder.image_calls == [[b"BYTES"]]


def test_embed_image_invalid_base64_is_400(live_server):
    base_url, _ = live_server
    status, body = _post(base_url, "/embed/image", {"images_b64": ["not!!base64"]})
    assert status == 400
    assert "base64" in body["error"]


def test_embed_image_too_many_images_is_413():
    encoder = FakeEncoder()
    cfg = Config(clip_dim=TEST_DIM, host="127.0.0.1", port=0, max_images=1)
    base_url, server, thread = serve(encoder, config=cfg)
    try:
        good = base64.b64encode(b"x").decode()
        status, body = _post(base_url, "/embed/image", {"images_b64": [good, good]})
        assert status == 413
        assert "too many" in body["error"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_embed_image_decode_failure_is_503():
    encoder = FakeEncoder(fail_image=True)
    base_url, server, thread = serve(encoder)
    try:
        good = base64.b64encode(b"x").decode()
        # fail_image raises EncodeError WITHOUT "not loaded" -> treated as client 400.
        status, body = _post(base_url, "/embed/image", {"images_b64": [good]})
        assert status == 400
        assert "decoded" in body["error"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


# --- error / contract cases ------------------------------------------------


def test_dimension_mismatch_is_500():
    encoder = FakeEncoder(wrong_dim=True)
    base_url, server, thread = serve(encoder)
    try:
        status, body = _post(base_url, "/embed/text", {"inputs": ["x"]})
        assert status == 500
        assert "dimension" in body["error"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_not_loaded_encoder_is_503():
    encoder = FakeEncoder(not_loaded=True)
    base_url, server, thread = serve(encoder)
    try:
        status, body = _post(base_url, "/embed/text", {"inputs": ["x"]})
        assert status == 503
        assert "not loaded" in body["error"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_malformed_json_is_400(live_server):
    base_url, _ = live_server
    status, body = _post(base_url, "/embed/text", None, raw=b"{not json")
    assert status == 400
    assert "JSON" in body["error"]


def test_non_object_body_is_400(live_server):
    base_url, _ = live_server
    status, _ = _post(base_url, "/embed/text", None, raw=b"[1,2,3]")
    assert status == 400


def test_oversized_body_is_413():
    encoder = FakeEncoder()
    cfg = Config(clip_dim=TEST_DIM, host="127.0.0.1", port=0, max_body_bytes=16)
    base_url, server, thread = serve(encoder, config=cfg)
    try:
        status, body = _post(base_url, "/embed/text", {"inputs": ["a" * 1000]})
        assert status == 413
        assert "too large" in body["error"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_unknown_route_is_404(live_server):
    base_url, _ = live_server
    status, _ = _post(base_url, "/nope", {})
    assert status == 404


def test_get_on_embed_route_is_404(live_server):
    base_url, _ = live_server
    status, _ = _get(base_url, "/embed/text")
    assert status == 404


# --- /health ---------------------------------------------------------------


def test_health_ok_when_ready(live_server):
    base_url, _ = live_server
    status, body = _get(base_url, "/health")
    assert status == 200
    assert body == {"status": "ok"}


def test_health_503_while_loading():
    encoder = FakeEncoder()
    cfg = Config(clip_dim=TEST_DIM, host="127.0.0.1", port=0)
    from clip.server import make_server

    server = make_server(cfg, encoder, ready=lambda: False)
    import threading

    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        status, body = _get(f"http://127.0.0.1:{port}", "/health")
        assert status == 503
        assert body == {"status": "loading"}
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


@pytest.mark.parametrize("path", ["/embed/text", "/embed/image"])
def test_missing_content_length_is_411(live_server, path):
    base_url, _ = live_server
    # Build a raw request without Content-Length by using a chunked-ish trick:
    # urllib always sets Content-Length, so hit the handler directly via socket.
    import socket

    host = base_url.removeprefix("http://")
    hostname, port = host.split(":")
    with socket.create_connection((hostname, int(port)), timeout=5) as sock:
        sock.sendall(f"POST {path} HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n".encode())
        resp = sock.recv(4096).decode("latin-1")
    assert "411" in resp.split("\r\n", 1)[0]
