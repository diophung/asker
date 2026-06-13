"""Media enrich handler tests (ADR-013): all external deps are injected FAKES.

No real model, binary, or HTTP server runs here. Each collaborator is replaced
by a deterministic fake returning known data (fixed OCR text, segments with
known start/end, CLIP vectors of length CLIP_DIM, keyframes), and the tests
assert the chunk / dim / modality contract, MediaInfo assembly, the tenant
header on the hub fetch, zero-chunk-still-produces, and that a collaborator
failure raises (so it flows through the worker retry/dead-letter loop).
"""

import pytest

from asker.v1 import document_pb2
from enrich import media_clients as mc
from enrich.media import (
    MOD_ASR,
    MOD_CAPTION,
    MOD_OCR,
    MediaHandler,
    is_media,
    media_kind,
)

EMBEDDING_DIM = 4
CLIP_DIM = 6
TENANT = "tenant-a"


# --- fakes ------------------------------------------------------------------


class FakeEmbedder:
    """bge-m3 stand-in: returns EMBEDDING_DIM vectors, one per text."""

    def __init__(self, dim=EMBEDDING_DIM, fail=False):
        self.dim = dim
        self.fail = fail
        self.calls: list[list[str]] = []

    async def embed(self, texts):
        if self.fail:
            raise RuntimeError("tei exploded")
        self.calls.append(list(texts))
        return [[float(i)] * self.dim for i, _ in enumerate(texts)]


class FakeClip:
    """CLIP stand-in: returns CLIP_DIM vectors, one per image."""

    def __init__(self, dim=CLIP_DIM, fail=False):
        self.dim = dim
        self.fail = fail
        self.images: list[bytes] = []

    async def embed_images(self, images):
        if self.fail:
            raise mc.MediaError("clip down")
        self.images.extend(images)
        return [[float(7 + i)] * self.dim for i, _ in enumerate(images)]


class FakeStore:
    """Hub media stand-in: records the tenant + the BlobRef on fetch; echoes puts."""

    def __init__(self, data=b"raw-bytes", content_type="image/png", fail_fetch=False):
        self._data = data
        self._content_type = content_type
        self._fail_fetch = fail_fetch
        self.fetched: list[tuple[str, mc.BlobRefData]] = []
        self.stored: list[tuple[str, str, str, bytes]] = []

    async def fetch(self, tenant_id, original):
        if self._fail_fetch:
            raise mc.MediaError("hub fetch 404")
        self.fetched.append((tenant_id, original))
        return mc.FetchedMedia(data=self._data, content_type=self._content_type)

    async def store(self, tenant_id, key, content_type, data):
        self.stored.append((tenant_id, key, content_type, data))
        return mc.BlobRefData(
            bucket="media",
            key=f"{tenant_id}/{key}",
            size_bytes=len(data),
            content_type=content_type,
            sha256="deadbeef",
        )


def fake_ocr_returning(text):
    def ocr(_image: bytes) -> str:
        return text

    return ocr


def fake_transcriber(segments, language="en", duration_ms=4200):
    def transcribe(_audio: bytes, _content_type: str) -> mc.Transcript:
        return mc.Transcript(
            segments=[mc.TranscriptSegment(*s) for s in segments],
            language=language,
            duration_ms=duration_ms,
        )

    return transcribe


def fake_thumbnailer(width=800, height=600):
    def thumbnail(_image: bytes) -> mc.ThumbnailResult:
        return mc.ThumbnailResult(jpeg=b"jpeg-thumb", width=width, height=height)

    return thumbnail


class FakeVideo:
    def __init__(self, info=None, audio=b"wav-bytes", keyframes=None):
        self._info = info or mc.VideoInfo(
            duration_ms=30000, width=1920, height=1080, has_audio=True
        )
        self._audio = audio
        self._keyframes = keyframes if keyframes is not None else []
        self.extract_audio_calls = 0

    def probe(self, _video, _content_type):
        return self._info

    def extract_audio(self, _video, _content_type):
        self.extract_audio_calls += 1
        return self._audio

    def extract_keyframes(self, _video, _content_type, max_keyframes):
        return list(self._keyframes[:max_keyframes])


def make_handler(
    *,
    embedder=None,
    store=None,
    clip=None,
    ocr=None,
    transcriber=None,
    video=None,
    thumbnailer=None,
    max_keyframes=20,
):
    return MediaHandler(
        embedder=embedder or FakeEmbedder(),
        store=store or FakeStore(),
        clip=clip or FakeClip(),
        ocr=ocr or fake_ocr_returning(""),
        transcriber=transcriber or fake_transcriber([]),
        video=video or FakeVideo(),
        thumbnailer=thumbnailer or fake_thumbnailer(),
        max_keyframes=max_keyframes,
    )


def make_media_doc(doc_type, content_type="", key="t/orig", sha="abc123"):
    doc = document_pb2.Document(
        tenant_id=TENANT,
        doc_id="doc-1",
        connector_id="gdrive",
        type=doc_type,
        version_etag="etag-1",
    )
    doc.original.key = key
    doc.original.sha256 = sha
    doc.original.bucket = "originals"
    if content_type:
        doc.original.content_type = content_type
    return doc


def parse(serialized: bytes) -> document_pb2.Document:
    doc = document_pb2.Document()
    doc.ParseFromString(serialized)
    return doc


def chunks_by_modality(doc, modality):
    return [c for c in doc.chunks if c.modality == modality]


# --- routing ----------------------------------------------------------------


@pytest.mark.parametrize(
    ("doc_type", "content_type", "expected"),
    [
        (document_pb2.IMAGE, "", "image"),
        (document_pb2.AUDIO, "", "audio"),
        (document_pb2.VIDEO, "", "video"),
        (document_pb2.DOC_TYPE_UNSPECIFIED, "image/png", "image"),
        (document_pb2.DOC_TYPE_UNSPECIFIED, "audio/mpeg", "audio"),
        (document_pb2.DOC_TYPE_UNSPECIFIED, "video/mp4", "video"),
    ],
)
def test_is_media_and_kind(doc_type, content_type, expected):
    doc = make_media_doc(doc_type, content_type=content_type)
    assert is_media(doc)
    assert media_kind(doc) == expected


@pytest.mark.parametrize(
    ("doc_type", "content_type"),
    [
        (document_pb2.EMAIL, ""),
        (document_pb2.FILE, "application/pdf"),
        (document_pb2.DOC_TYPE_UNSPECIFIED, "text/plain"),
    ],
)
def test_non_media_is_not_routed(doc_type, content_type):
    doc = make_media_doc(doc_type, content_type=content_type)
    assert not is_media(doc)


# --- IMAGE ------------------------------------------------------------------


async def test_image_produces_ocr_and_caption_chunks_and_thumbnail():
    store = FakeStore(data=b"png-bytes", content_type="image/png")
    embedder = FakeEmbedder()
    clip = FakeClip()
    handler = make_handler(
        store=store,
        embedder=embedder,
        clip=clip,
        ocr=fake_ocr_returning("INVOICE total $42"),
        thumbnailer=fake_thumbnailer(width=800, height=600),
    )
    doc = make_media_doc(document_pb2.IMAGE, content_type="image/png")

    out = parse(await handler.enrich(doc))

    ocr = chunks_by_modality(out, MOD_OCR)
    caption = chunks_by_modality(out, MOD_CAPTION)
    assert len(ocr) == 1
    assert len(caption) == 1

    # OCR chunk: bge-m3 dimension, real text, char offsets, NO time.
    assert ocr[0].text == "INVOICE total $42"
    assert len(ocr[0].embedding) == EMBEDDING_DIM
    assert ocr[0].char_start == 0
    assert ocr[0].char_end == len(b"INVOICE total $42")
    assert ocr[0].start_ms == 0 and ocr[0].end_ms == 0
    assert embedder.calls == [["INVOICE total $42"]]

    # Caption chunk: CLIP dimension, empty text, no time.
    assert caption[0].text == ""
    assert len(caption[0].embedding) == CLIP_DIM
    assert clip.images == [b"png-bytes"]

    # Thumbnail stored + MediaInfo dimensions set.
    assert out.media.width == 800 and out.media.height == 600
    assert out.media.thumbnail.key == f"{TENANT}/thumb/doc-1.jpg"
    thumb_puts = [s for s in store.stored if s[1] == "thumb/doc-1.jpg"]
    assert thumb_puts and thumb_puts[0][2] == "image/jpeg"  # stored as JPEG


async def test_image_with_no_ocr_text_skips_ocr_chunk():
    handler = make_handler(ocr=fake_ocr_returning("   "))  # whitespace only
    doc = make_media_doc(document_pb2.IMAGE, content_type="image/jpeg")
    out = parse(await handler.enrich(doc))
    assert chunks_by_modality(out, MOD_OCR) == []
    assert len(chunks_by_modality(out, MOD_CAPTION)) == 1  # CLIP still embeds the image


async def test_image_sets_tenant_on_hub_fetch_with_full_blobref():
    store = FakeStore()
    handler = make_handler(store=store)
    doc = make_media_doc(document_pb2.IMAGE, content_type="image/png", key="t/o", sha="zz")
    await handler.enrich(doc)
    assert len(store.fetched) == 1
    tenant, ref = store.fetched[0]
    assert tenant == TENANT  # x-asker-tenant scoping (the store impl sends it)
    assert ref.key == "t/o" and ref.sha256 == "zz" and ref.bucket == "originals"


# --- AUDIO ------------------------------------------------------------------


async def test_audio_produces_asr_chunks_with_ms_anchoring():
    embedder = FakeEmbedder()
    # Two short, well-separated segments -> two windows (merge keeps them apart
    # only by size; short texts merge into one window, so use distinct content).
    transcriber = fake_transcriber(
        [(1.5, 4.2, "hello world"), (10.0, 12.5, "second segment")],
        language="en",
        duration_ms=12500,
    )
    handler = make_handler(embedder=embedder, transcriber=transcriber)
    doc = make_media_doc(document_pb2.AUDIO, content_type="audio/mpeg")

    out = parse(await handler.enrich(doc))
    asr = chunks_by_modality(out, MOD_ASR)
    assert len(asr) == 1  # both short segments merge into one ~512-token window
    # Window keeps first start + last end (seconds -> ms).
    assert asr[0].start_ms == 1500
    assert asr[0].end_ms == 12500
    assert "hello world" in asr[0].text and "second segment" in asr[0].text
    assert len(asr[0].embedding) == EMBEDDING_DIM  # bge-m3, NOT clip
    assert out.media.duration_ms == 12500
    assert out.media.transcript_lang == "en"
    assert chunks_by_modality(out, MOD_CAPTION) == []  # no images in audio


async def test_audio_long_segments_split_into_multiple_windows():
    big = "word " * 500  # ~2500 chars > merge cap, so each is its own window
    transcriber = fake_transcriber([(0.0, 5.0, big), (5.0, 10.0, big)], language="es")
    handler = make_handler(transcriber=transcriber)
    doc = make_media_doc(document_pb2.AUDIO, content_type="audio/wav")
    out = parse(await handler.enrich(doc))
    asr = chunks_by_modality(out, MOD_ASR)
    assert len(asr) == 2
    assert asr[0].start_ms == 0 and asr[0].end_ms == 5000
    assert asr[1].start_ms == 5000 and asr[1].end_ms == 10000


async def test_silent_audio_zero_chunks_still_produces():
    handler = make_handler(transcriber=fake_transcriber([], language="", duration_ms=3000))
    doc = make_media_doc(document_pb2.AUDIO, content_type="audio/wav")
    out = parse(await handler.enrich(doc))
    assert len(out.chunks) == 0  # silent: no ASR chunks
    assert out.media.duration_ms == 3000  # but metadata still set + doc still produced


# --- VIDEO ------------------------------------------------------------------


async def test_video_produces_asr_and_keyframe_chunks_and_poster():
    embedder = FakeEmbedder()
    clip = FakeClip()
    store = FakeStore(data=b"mp4-bytes", content_type="video/mp4")
    video = FakeVideo(
        info=mc.VideoInfo(duration_ms=30000, width=1920, height=1080, has_audio=True),
        audio=b"wav",
        keyframes=[
            mc.KeyframeImage(ts_ms=2000, jpeg=b"frame-0"),
            mc.KeyframeImage(ts_ms=15000, jpeg=b"frame-1"),
        ],
    )
    transcriber = fake_transcriber([(1.5, 4.2, "quarterly numbers")], language="en")
    handler = make_handler(
        embedder=embedder, clip=clip, store=store, video=video, transcriber=transcriber
    )
    doc = make_media_doc(document_pb2.VIDEO, content_type="video/mp4")

    out = parse(await handler.enrich(doc))

    asr = chunks_by_modality(out, MOD_ASR)
    caption = chunks_by_modality(out, MOD_CAPTION)
    assert len(asr) == 1
    assert asr[0].start_ms == 1500 and asr[0].end_ms == 4200
    assert len(asr[0].embedding) == EMBEDDING_DIM  # bge-m3

    # Two keyframe caption chunks, CLIP dim, time-anchored.
    assert len(caption) == 2
    assert [c.start_ms for c in caption] == [2000, 15000]
    assert all(len(c.embedding) == CLIP_DIM for c in caption)
    assert clip.images == [b"frame-0", b"frame-1"]

    # MediaInfo: dims, duration, two Keyframe entries pointing at the caption chunks.
    assert out.media.width == 1920 and out.media.height == 1080
    assert out.media.duration_ms == 30000
    assert len(out.media.keyframes) == 2
    assert out.media.keyframes[0].ts_ms == 2000
    assert out.media.keyframes[0].chunk_id == caption[0].chunk_id
    assert out.media.keyframes[0].image.key == f"{TENANT}/keyframes/doc-1/00000.jpg"

    # Poster thumbnail = the first keyframe.
    assert out.media.thumbnail.key == f"{TENANT}/thumb/doc-1.jpg"
    poster_puts = [s for s in store.stored if s[1] == "thumb/doc-1.jpg"]
    assert poster_puts and poster_puts[0][3] == b"frame-0"


async def test_video_caps_keyframes_at_max():
    frames = [mc.KeyframeImage(ts_ms=i * 1000, jpeg=f"f{i}".encode()) for i in range(50)]
    video = FakeVideo(keyframes=frames)
    handler = make_handler(video=video, max_keyframes=3)
    doc = make_media_doc(document_pb2.VIDEO, content_type="video/mp4")
    out = parse(await handler.enrich(doc))
    assert len(chunks_by_modality(out, MOD_CAPTION)) == 3
    assert len(out.media.keyframes) == 3


async def test_video_with_no_audio_stream_indexes_visual_content_without_transcribing():
    """A silent / audio-less video must index keyframes + metadata, NOT dead-letter.

    faster-whisper errors on an audio-less input; if transcription were attempted
    unconditionally the whole document would dead-letter and lose its keyframe/CLIP
    indexing. With VideoInfo.has_audio=False the audio arm is skipped entirely (no
    extract_audio, no transcribe) and the visual arm still produces.
    """
    clip = FakeClip()
    store = FakeStore(data=b"mp4-bytes", content_type="video/mp4")
    video = FakeVideo(
        info=mc.VideoInfo(duration_ms=8000, width=1280, height=720, has_audio=False),
        keyframes=[
            mc.KeyframeImage(ts_ms=1000, jpeg=b"frame-0"),
            mc.KeyframeImage(ts_ms=4000, jpeg=b"frame-1"),
        ],
    )

    def transcriber_must_not_be_called(_audio, _content_type):
        raise AssertionError("transcription must not be attempted on an audio-less video")

    handler = make_handler(
        clip=clip, store=store, video=video, transcriber=transcriber_must_not_be_called
    )
    doc = make_media_doc(document_pb2.VIDEO, content_type="video/mp4")

    # Enrich succeeds (does NOT raise / dead-letter) and returns serialized bytes.
    out = parse(await handler.enrich(doc))

    # The audio arm was skipped entirely: no extract_audio, no transcription.
    assert video.extract_audio_calls == 0
    assert chunks_by_modality(out, MOD_ASR) == []
    assert not out.media.transcript_lang

    # The visual arm still produced keyframe/caption chunks + MediaInfo.
    caption = chunks_by_modality(out, MOD_CAPTION)
    assert len(caption) == 2
    assert [c.start_ms for c in caption] == [1000, 4000]
    assert all(len(c.embedding) == CLIP_DIM for c in caption)
    assert clip.images == [b"frame-0", b"frame-1"]
    assert out.media.width == 1280 and out.media.height == 720
    assert out.media.duration_ms == 8000
    assert len(out.media.keyframes) == 2
    assert out.media.thumbnail.key == f"{TENANT}/thumb/doc-1.jpg"


async def test_video_no_keyframes_still_produces_asr():
    video = FakeVideo(keyframes=[])
    transcriber = fake_transcriber([(0.0, 2.0, "talking")], language="en")
    handler = make_handler(video=video, transcriber=transcriber)
    doc = make_media_doc(document_pb2.VIDEO, content_type="video/mp4")
    out = parse(await handler.enrich(doc))
    assert len(chunks_by_modality(out, MOD_ASR)) == 1
    assert len(chunks_by_modality(out, MOD_CAPTION)) == 0
    assert len(out.media.keyframes) == 0
    assert not out.media.HasField("thumbnail")  # no poster without a keyframe


# --- dim routing contract ---------------------------------------------------


async def test_dim_routing_text_arms_use_embedding_dim_caption_uses_clip_dim():
    """text/ocr/asr chunks carry EMBEDDING_DIM vectors; caption chunks CLIP_DIM."""
    embedder = FakeEmbedder()
    clip = FakeClip()
    video = FakeVideo(keyframes=[mc.KeyframeImage(ts_ms=1000, jpeg=b"kf")])
    transcriber = fake_transcriber([(0.0, 2.0, "spoken words")])
    handler = make_handler(embedder=embedder, clip=clip, video=video, transcriber=transcriber)
    doc = make_media_doc(document_pb2.VIDEO, content_type="video/mp4")
    out = parse(await handler.enrich(doc))
    for chunk in out.chunks:
        if chunk.modality == MOD_CAPTION:
            assert len(chunk.embedding) == CLIP_DIM
        else:  # ocr / asr
            assert len(chunk.embedding) == EMBEDDING_DIM


# --- idempotency + failure flow ---------------------------------------------


async def test_enrich_is_idempotent_across_retries():
    handler = make_handler(ocr=fake_ocr_returning("text"), thumbnailer=fake_thumbnailer())
    doc = make_media_doc(document_pb2.IMAGE, content_type="image/png")
    first = parse(await handler.enrich(doc))
    second = parse(await handler.enrich(doc))  # re-run on the same (mutated) doc
    assert len(first.chunks) == len(second.chunks)
    assert len(first.media.keyframes) == len(second.media.keyframes)


@pytest.mark.parametrize(
    "broken",
    ["fetch", "clip", "whisper"],
)
async def test_collaborator_failure_raises_for_retry_loop(broken):
    """A media collaborator failure raises (not crashes) so the worker retries."""
    if broken == "fetch":
        handler = make_handler(store=FakeStore(fail_fetch=True))
        doc = make_media_doc(document_pb2.IMAGE, content_type="image/png")
    elif broken == "clip":
        handler = make_handler(clip=FakeClip(fail=True))
        doc = make_media_doc(document_pb2.IMAGE, content_type="image/png")
    else:  # whisper

        def boom(_a, _c):
            raise mc.MediaError("whisper crashed")

        handler = make_handler(transcriber=boom)
        doc = make_media_doc(document_pb2.AUDIO, content_type="audio/wav")

    # The failure propagates (raises) rather than being swallowed, so the
    # worker's retry/dead-letter loop sees it — never a silent crash.
    with pytest.raises(mc.MediaError):
        await handler.enrich(doc)
