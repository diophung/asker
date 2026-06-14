"""Media collaborator client tests: the hub + CLIP HTTP shapes and pure helpers.

The HTTP clients are exercised via httpx.MockTransport (no live server, same
style as test_embedder.py). The production model/binary collaborators (whisper,
ffmpeg, tesseract, Pillow) are NOT invoked here — only their pure helpers
(showinfo parsing, content-type suffix mapping) and the lazy-import boundary.
"""

import base64
import json
import subprocess
import sys

import httpx
import pytest

from enrich import media_clients as mc

CLIP_DIM = 6
TENANT = "tenant-a"


def _client(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


# --- HubMediaStore ----------------------------------------------------------


async def test_hub_fetch_sends_tenant_header_and_blobref_params():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["tenant"] = request.headers.get(mc.TENANT_HEADER)
        seen["params"] = dict(request.url.params)
        return httpx.Response(
            200, content=b"decrypted-bytes", headers={"content-type": "image/png"}
        )

    store = mc.HubMediaStore("http://hub:9300", client=_client(handler))
    ref = mc.BlobRefData(bucket="b", key="t/o", sha256="sha", content_type="image/png")
    out = await store.fetch(TENANT, ref)

    assert out.data == b"decrypted-bytes"
    assert out.content_type == "image/png"
    assert seen["path"] == "/internal/media"
    assert seen["tenant"] == TENANT
    assert seen["params"]["key"] == "t/o"
    assert seen["params"]["sha256"] == "sha"  # required by the hub contract
    assert seen["params"]["bucket"] == "b"
    assert seen["params"]["content_type"] == "image/png"


async def test_hub_fetch_empty_key_raises_without_call():
    def handler(_request):  # pragma: no cover - must not be called
        raise AssertionError("should not hit the network for an empty key")

    store = mc.HubMediaStore("http://hub:9300", client=_client(handler))
    with pytest.raises(mc.MediaError, match="empty"):
        await store.fetch(TENANT, mc.BlobRefData(key="", sha256="x"))


async def test_hub_fetch_http_error_becomes_media_error():
    def handler(_request):
        return httpx.Response(404, json={"error": "not found"})

    store = mc.HubMediaStore("http://hub:9300", client=_client(handler))
    with pytest.raises(mc.MediaError, match="fetch failed"):
        await store.fetch(TENANT, mc.BlobRefData(key="k", sha256="s"))


async def test_hub_store_returns_blobref_from_json():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["tenant"] = request.headers.get(mc.TENANT_HEADER)
        seen["params"] = dict(request.url.params)
        seen["body"] = request.content
        return httpx.Response(
            201,
            json={
                "bucket": "media",
                "key": "tenant-a/thumb/doc-1.jpg",
                "size_bytes": 1234,
                "content_type": "image/jpeg",
                "sha256": "abc",
            },
        )

    store = mc.HubMediaStore("http://hub:9300", client=_client(handler))
    ref = await store.store(TENANT, "thumb/doc-1.jpg", "image/jpeg", b"jpeg")

    assert seen["method"] == "PUT"
    assert seen["tenant"] == TENANT
    assert seen["params"]["key"] == "thumb/doc-1.jpg"
    assert seen["params"]["content_type"] == "image/jpeg"
    assert seen["body"] == b"jpeg"
    assert ref.bucket == "media"
    assert ref.key == "tenant-a/thumb/doc-1.jpg"
    assert ref.size_bytes == 1234
    assert ref.sha256 == "abc"


async def test_hub_store_http_error_becomes_media_error():
    def handler(_request):
        return httpx.Response(500, json={"error": "boom"})

    store = mc.HubMediaStore("http://hub:9300", client=_client(handler))
    with pytest.raises(mc.MediaError, match="store failed"):
        await store.store(TENANT, "k", "image/jpeg", b"x")


# --- HttpClipClient ---------------------------------------------------------


async def test_clip_embed_images_base64_and_validates_dim():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        body = json.loads(request.content)
        seen["images_b64"] = body["images_b64"]
        n = len(body["images_b64"])
        return httpx.Response(200, json={"embeddings": [[0.1] * CLIP_DIM for _ in range(n)]})

    clip = mc.HttpClipClient("http://clip:9800", CLIP_DIM, client=_client(handler))
    vecs = await clip.embed_images([b"img-a", b"img-b"])

    assert seen["path"] == "/embed/image"
    assert seen["images_b64"] == [
        base64.b64encode(b"img-a").decode(),
        base64.b64encode(b"img-b").decode(),
    ]
    assert len(vecs) == 2
    assert all(len(v) == CLIP_DIM for v in vecs)


async def test_clip_empty_input_skips_call():
    def handler(_request):  # pragma: no cover - must not be called
        raise AssertionError("no call for empty image list")

    clip = mc.HttpClipClient("http://clip:9800", CLIP_DIM, client=_client(handler))
    assert await clip.embed_images([]) == []


async def test_clip_wrong_dim_raises():
    def handler(_request):
        return httpx.Response(200, json={"embeddings": [[0.0] * (CLIP_DIM + 1)]})

    clip = mc.HttpClipClient("http://clip:9800", CLIP_DIM, client=_client(handler))
    with pytest.raises(mc.MediaError, match="CLIP_DIM"):
        await clip.embed_images([b"img"])


async def test_clip_wrong_count_raises():
    def handler(_request):
        return httpx.Response(200, json={"embeddings": [[0.0] * CLIP_DIM]})  # 1 for 2

    clip = mc.HttpClipClient("http://clip:9800", CLIP_DIM, client=_client(handler))
    with pytest.raises(mc.MediaError, match="vectors for 2 images"):
        await clip.embed_images([b"a", b"b"])


async def test_clip_http_error_becomes_media_error():
    def handler(_request):
        raise httpx.ConnectError("clip down")

    clip = mc.HttpClipClient("http://clip:9800", CLIP_DIM, client=_client(handler))
    with pytest.raises(mc.MediaError, match="embed/image failed"):
        await clip.embed_images([b"img"])


async def test_clip_health():
    clip_up = mc.HttpClipClient(
        "http://clip:9800", CLIP_DIM, client=_client(lambda _r: httpx.Response(200))
    )
    ok, msg = await clip_up.healthy()
    assert ok and msg == "ok"

    clip_down = mc.HttpClipClient(
        "http://clip:9800", CLIP_DIM, client=_client(lambda _r: httpx.Response(503))
    )
    ok, msg = await clip_down.healthy()
    assert not ok and "503" in msg


# --- pure helpers -----------------------------------------------------------


def test_parse_showinfo_pts():
    stderr = (
        "[Parsed_showinfo_1 @ 0x55] n:0 pts:48000 pts_time:1.5 pos:1234\n"
        "noise line without marker\n"
        "[Parsed_showinfo_1 @ 0x55] n:1 pts:96000 pts_time:15.25 pos:5678\n"
    )
    assert mc._parse_showinfo_pts(stderr) == [1.5, 15.25]


@pytest.mark.parametrize(
    ("ct", "default", "expected"),
    [
        ("image/png", ".bin", ".png"),
        ("audio/mpeg", ".bin", ".mp3"),
        ("video/mp4; codecs=avc1", ".bin", ".mp4"),  # params stripped
        ("VIDEO/QUICKTIME", ".bin", ".mov"),  # case-insensitive
        ("application/unknown", ".bin", ".bin"),  # default fallback
        ("", ".wav", ".wav"),
    ],
)
def test_suffix_for(ct, default, expected):
    assert mc._suffix_for(ct, default) == expected


# --- _run subprocess timeout ------------------------------------------------


def test_run_passes_timeout_to_subprocess_and_returns_stdout():
    """_run forwards an explicit timeout= to the runner so no call is unbounded."""
    seen = {}

    def fake_runner(args, *, capture_output, check, timeout):
        seen["args"] = args
        seen["capture_output"] = capture_output
        seen["check"] = check
        seen["timeout"] = timeout
        return subprocess.CompletedProcess(args, 0, stdout=b"out-bytes", stderr=b"err-bytes")

    out = mc._run(["ffprobe", "x"], "ffprobe", timeout=7.5, runner=fake_runner)
    assert out == "out-bytes"
    assert seen["timeout"] == 7.5  # an explicit bound is always passed
    assert seen["capture_output"] is True
    assert seen["check"] is True


def test_run_timeout_raises_media_error_via_fake_runner():
    """An expired subprocess (TimeoutExpired) becomes MediaError -> retry/dead-letter."""

    def hanging_runner(args, *, capture_output, check, timeout):
        raise subprocess.TimeoutExpired(cmd=args, timeout=timeout)

    with pytest.raises(mc.MediaError, match="timed out after"):
        mc._run(["ffmpeg", "hang"], "ffmpeg keyframes", timeout=0.01, runner=hanging_runner)


def test_run_timeout_raises_media_error_on_real_sleep():
    """A real child that outlives a tiny timeout is killed and surfaces MediaError.

    Uses the test interpreter to sleep — never spawns ffmpeg — so the timeout
    path is exercised end-to-end through subprocess.run without any binary.
    """
    args = [sys.executable, "-c", "import time; time.sleep(5)"]
    with pytest.raises(mc.MediaError, match="timed out after"):
        mc._run(args, "ffmpeg audio extract", timeout=0.2)


def test_ffmpeg_extractor_threads_timeout_into_run(monkeypatch):
    """FFmpegVideoExtractor(timeout=...) reaches the underlying _run call."""
    captured = {}

    def fake_run(args, what, capture_stderr=False, timeout=mc._FFMPEG_TIMEOUT, runner=None):
        captured["timeout"] = timeout
        return json.dumps({"streams": [], "format": {}})

    monkeypatch.setattr(mc, "_run", fake_run)
    extractor = mc.FFmpegVideoExtractor(timeout=42)
    extractor.probe(b"video-bytes", "video/mp4")
    assert captured["timeout"] == 42


# --- probe() audio-stream detection -----------------------------------------


def test_probe_sets_has_audio_when_audio_stream_present(monkeypatch):
    meta = {
        "streams": [
            {"codec_type": "video", "width": 1280, "height": 720},
            {"codec_type": "audio"},
        ],
        "format": {"duration": "12.5"},
    }
    monkeypatch.setattr(mc, "_run", lambda *a, **k: json.dumps(meta))
    info = mc.FFmpegVideoExtractor().probe(b"v", "video/mp4")
    assert info.has_audio is True
    assert info.width == 1280 and info.height == 720
    assert info.duration_ms == 12500


def test_probe_has_audio_false_when_no_audio_stream(monkeypatch):
    meta = {
        "streams": [{"codec_type": "video", "width": 640, "height": 480}],
        "format": {"duration": "8"},
    }
    monkeypatch.setattr(mc, "_run", lambda *a, **k: json.dumps(meta))
    info = mc.FFmpegVideoExtractor().probe(b"v", "video/mp4")
    assert info.has_audio is False
    assert info.width == 640 and info.height == 480
