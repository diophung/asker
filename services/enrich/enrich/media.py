"""The media enrich handler (ADR-013): IMAGE/AUDIO/VIDEO -> chunks + MediaInfo.

A media document carries no body_text, so the text path's chunker produced no
chunks; this handler produces them. It conforms to the two-embedding-space
contract:

- TEXT/OCR/ASR chunks carry a **bge-m3** vector (len EMBEDDING_DIM, from TEI via
  the existing embedder). modality "ocr" | "asr".
- IMAGE / video-KEYFRAME chunks carry a **CLIP** image vector (len CLIP_DIM, from
  the clip service). modality "caption", text "".

The index-writer routes a chunk's single Chunk.embedding to Vespa's `embedding`
field when len == EMBEDDING_DIM, or `clip_embedding` when len == CLIP_DIM
(vespa/README.md). modality + start_ms/end_ms become the parallel
chunk_modalities / chunk_starts_ms arrays for deep-linking.

Every external dependency (hub fetch/store, CLIP, OCR, whisper, ffmpeg,
thumbnail) is an injected collaborator so tests run against fakes. A collaborator
failure raises (MediaError or otherwise) and flows through the worker's existing
retry/dead-letter loop — it never crashes the worker. A media document that
yields zero chunks still produces (so the doc exists and is searchable by
metadata), and that is logged.
"""

from __future__ import annotations

import logging
from collections.abc import Sequence

from asker.v1 import document_pb2

from . import media_clients as mc
from .kafka_worker import EmbedderLike

log = logging.getLogger("enrich.media")

# Document.type values this handler owns.
MEDIA_TYPES = frozenset(
    {
        document_pb2.IMAGE,
        document_pb2.AUDIO,
        document_pb2.VIDEO,
    }
)

# content_type prefixes that mark a document as media even if Document.type was
# left UNSPECIFIED by the connector (defensive; routing prefers Document.type).
_MEDIA_CT_PREFIXES = ("image/", "audio/", "video/")

# Roughly target ~512 tokens (chars/4 estimate, matching embedder.estimate_tokens)
# when merging adjacent ASR segments into one chunk, so each ASR chunk is a
# meaningful retrieval unit without exceeding the embed budget.
_ASR_MERGE_MAX_CHARS = 2000

Modality = str
MOD_OCR: Modality = "ocr"
MOD_ASR: Modality = "asr"
MOD_CAPTION: Modality = "caption"


def is_media(doc: document_pb2.Document) -> bool:
    """Report whether doc should take the media path rather than text embedding."""
    if doc.type in MEDIA_TYPES:
        return True
    ct = (doc.original.content_type or "").lower()
    return any(ct.startswith(p) for p in _MEDIA_CT_PREFIXES)


def media_kind(doc: document_pb2.Document) -> str:
    """Classify a media doc as 'image' | 'audio' | 'video' (type first, then CT)."""
    if doc.type == document_pb2.IMAGE:
        return "image"
    if doc.type == document_pb2.AUDIO:
        return "audio"
    if doc.type == document_pb2.VIDEO:
        return "video"
    ct = (doc.original.content_type or "").lower()
    if ct.startswith("image/"):
        return "image"
    if ct.startswith("audio/"):
        return "audio"
    if ct.startswith("video/"):
        return "video"
    return "image"  # unreachable for is_media() docs; conservative default


class MediaHandler:
    """Enriches one media Document, mutating it in place into its enriched form.

    Collaborators are injected (ADR-007 testability): the hub media store, the
    CLIP client, the OCR function, the whisper transcriber, the video extractor,
    and the thumbnailer. The bge-m3 embedder is the SAME one the text path uses.
    """

    def __init__(
        self,
        *,
        embedder: EmbedderLike,
        store: mc.MediaStore,
        clip: mc.ClipClient,
        ocr: mc.OCRFunc,
        transcriber: mc.Transcriber,
        video: mc.VideoExtractor,
        thumbnailer: mc.Thumbnailer,
        max_keyframes: int = 20,
    ) -> None:
        self._embedder = embedder
        self._store = store
        self._clip = clip
        self._ocr = ocr
        self._transcriber = transcriber
        self._video = video
        self._thumbnailer = thumbnailer
        self._max_keyframes = max_keyframes

    async def enrich(self, doc: document_pb2.Document) -> bytes:
        """Produce chunks + MediaInfo for doc and return the serialized bytes.

        Idempotent across in-process retries: clears any chunks/media a prior
        failed attempt left behind before rebuilding.
        """
        del doc.chunks[:]
        doc.ClearField("media")

        original = mc.BlobRefData(
            bucket=doc.original.bucket,
            key=doc.original.key,
            size_bytes=doc.original.size_bytes,
            content_type=doc.original.content_type,
            sha256=doc.original.sha256,
        )
        fetched = await self._store.fetch(doc.tenant_id, original)

        kind = media_kind(doc)
        if kind == "image":
            await self._enrich_image(doc, fetched)
        elif kind == "audio":
            await self._enrich_audio(doc, fetched)
        else:
            await self._enrich_video(doc, fetched)

        if not doc.chunks:
            log.info(
                "media document produced zero chunks; emitting metadata-only document",
                extra={
                    "doc_id": doc.doc_id,
                    "tenant_id": doc.tenant_id,
                    "kind": kind,
                    "content_type": doc.original.content_type,
                },
            )
        return doc.SerializeToString()

    # --- IMAGE --------------------------------------------------------------

    async def _enrich_image(self, doc: document_pb2.Document, fetched: mc.FetchedMedia) -> None:
        # OCR (bge-m3) — only when there is text to embed.
        ocr_text = (await mc.to_thread(self._ocr, fetched.data)) or ""
        ocr_text = ocr_text.strip() if isinstance(ocr_text, str) else ""
        if ocr_text:
            vectors = await self._embedder.embed([ocr_text])
            self._add_text_chunk(
                doc,
                text=ocr_text,
                modality=MOD_OCR,
                embedding=vectors[0],
                char_start=0,
                char_end=len(ocr_text.encode("utf-8")),
            )

        # CLIP image embedding (clip_embedding) on a caption chunk. Degrade
        # gracefully if the clip service is unavailable: the image still indexes
        # its OCR text (and metadata) — a CLIP outage must not lose the document
        # (ADR-006/013 degradation, mirroring the query service's CLIP arm).
        clip_vecs = await self._embed_images_or_degrade(doc, [fetched.data])
        if clip_vecs:
            self._add_caption_chunk(doc, embedding=clip_vecs[0])

        # Thumbnail + dimensions.
        thumb = await mc.to_thread(self._thumbnailer, fetched.data)
        if isinstance(thumb, mc.ThumbnailResult):
            ref = await self._store.store(
                doc.tenant_id, f"thumb/{doc.doc_id}.jpg", "image/jpeg", thumb.jpeg
            )
            doc.media.width = thumb.width
            doc.media.height = thumb.height
            _set_blobref(doc.media.thumbnail, ref)

    # --- AUDIO --------------------------------------------------------------

    async def _enrich_audio(self, doc: document_pb2.Document, fetched: mc.FetchedMedia) -> None:
        transcript = await mc.to_thread(self._transcriber, fetched.data, fetched.content_type)
        await self._add_asr_chunks(doc, transcript)
        if transcript.duration_ms:
            doc.media.duration_ms = transcript.duration_ms
        if transcript.language:
            doc.media.transcript_lang = transcript.language

    # --- VIDEO --------------------------------------------------------------

    async def _enrich_video(self, doc: document_pb2.Document, fetched: mc.FetchedMedia) -> None:
        info = await mc.to_thread(self._video.probe, fetched.data, fetched.content_type)
        has_audio = False
        if isinstance(info, mc.VideoInfo):
            has_audio = info.has_audio
            if info.duration_ms:
                doc.media.duration_ms = info.duration_ms
            if info.width:
                doc.media.width = info.width
            if info.height:
                doc.media.height = info.height

        # Audio track -> ASR chunks (time-anchored). A video with no audio stream
        # (silent screen capture, GIF-style clip) is NOT transcribed: faster-whisper
        # errors on an audio-less input, which would dead-letter the whole document
        # and lose its keyframe/CLIP indexing. We skip straight to the visual arm so
        # the doc still indexes its frames (ADR-013).
        if has_audio:
            audio = await mc.to_thread(
                self._video.extract_audio, fetched.data, fetched.content_type
            )
            if audio:
                transcript = await mc.to_thread(self._transcriber, audio, "audio/wav")
                await self._add_asr_chunks(doc, transcript)
                if transcript.language and not doc.media.transcript_lang:
                    doc.media.transcript_lang = transcript.language
                if transcript.duration_ms and not doc.media.duration_ms:
                    doc.media.duration_ms = transcript.duration_ms
        else:
            log.info(
                "video has no audio stream; skipping transcription, indexing visual content only",
                extra={
                    "doc_id": doc.doc_id,
                    "tenant_id": doc.tenant_id,
                    "content_type": doc.original.content_type,
                },
            )

        # Scene keyframes -> CLIP caption chunks + Keyframe entries.
        keyframes = await mc.to_thread(
            self._video.extract_keyframes,
            fetched.data,
            fetched.content_type,
            self._max_keyframes,
        )
        keyframes = list(keyframes or [])
        # Degrade gracefully if the clip service is unavailable: the video still
        # indexes its ASR transcript (the spoken-phrase exit criterion) — a CLIP
        # outage must not dead-letter the document and lose its transcript.
        clip_vecs = await self._embed_images_or_degrade(doc, [kf.jpeg for kf in keyframes])
        if keyframes and clip_vecs:
            for i, (kf, vec) in enumerate(zip(keyframes, clip_vecs, strict=True)):
                chunk = self._add_caption_chunk(
                    doc, embedding=vec, start_ms=kf.ts_ms, end_ms=kf.ts_ms
                )
                ref = await self._store.store(
                    doc.tenant_id,
                    f"keyframes/{doc.doc_id}/{i:05d}.jpg",
                    "image/jpeg",
                    kf.jpeg,
                )
                frame = doc.media.keyframes.add(ts_ms=kf.ts_ms, chunk_id=chunk.chunk_id)
                _set_blobref(frame.image, ref)
            # Poster thumbnail = the first keyframe.
            poster = await self._store.store(
                doc.tenant_id, f"thumb/{doc.doc_id}.jpg", "image/jpeg", keyframes[0].jpeg
            )
            _set_blobref(doc.media.thumbnail, poster)

    async def _embed_images_or_degrade(
        self, doc: document_pb2.Document, images: Sequence[bytes]
    ) -> list[list[float]]:
        """CLIP-embed images, degrading to [] if the clip service is unavailable.

        A clip outage must NOT dead-letter the document: an image still indexes
        its OCR text and a video still indexes its ASR transcript. The vector
        (text/keyword/OCR/ASR) arm carries the document; only pure text->image
        visual matching is lost until clip recovers (ADR-006/013). A re-sync
        after clip is back re-adds the CLIP chunks (idempotent doc_id upsert).
        """
        if not images:
            return []
        try:
            return await self._clip.embed_images(list(images))
        except mc.MediaError as exc:
            log.warning(
                "clip unavailable; indexing without CLIP embeddings (text/OCR/ASR only)",
                extra={"doc_id": doc.doc_id, "tenant_id": doc.tenant_id, "error": str(exc)},
            )
            return []

    # --- chunk builders -----------------------------------------------------

    async def _add_asr_chunks(self, doc: document_pb2.Document, transcript: mc.Transcript) -> None:
        """Merge ASR segments into ~512-token windows, embed (bge-m3), append."""
        windows = _merge_segments(transcript.segments)
        if not windows:
            return
        texts = [w.text for w in windows]
        vectors = await self._embedder.embed(texts)
        if len(vectors) != len(windows):
            raise mc.MediaError(
                f"embedder returned {len(vectors)} vectors for {len(windows)} ASR windows"
            )
        for window, vector in zip(windows, vectors, strict=True):
            chunk = doc.chunks.add(
                chunk_id=f"{doc.doc_id}#{len(doc.chunks)}",
                text=window.text,
                modality=MOD_ASR,
                start_ms=int(window.start * 1000),
                end_ms=int(window.end * 1000),
            )
            chunk.embedding.extend(vector)

    def _add_text_chunk(
        self,
        doc: document_pb2.Document,
        *,
        text: str,
        modality: str,
        embedding: Sequence[float],
        char_start: int,
        char_end: int,
    ) -> document_pb2.Chunk:
        chunk = doc.chunks.add(
            chunk_id=f"{doc.doc_id}#{len(doc.chunks)}",
            text=text,
            modality=modality,
            char_start=char_start,
            char_end=char_end,
        )
        chunk.embedding.extend(embedding)
        return chunk

    def _add_caption_chunk(
        self,
        doc: document_pb2.Document,
        *,
        embedding: Sequence[float],
        start_ms: int = 0,
        end_ms: int = 0,
    ) -> document_pb2.Chunk:
        chunk = doc.chunks.add(
            chunk_id=f"{doc.doc_id}#{len(doc.chunks)}",
            text="",
            modality=MOD_CAPTION,
            start_ms=start_ms,
            end_ms=end_ms,
        )
        chunk.embedding.extend(embedding)
        return chunk


def _set_blobref(ref: document_pb2.BlobRef, data: mc.BlobRefData) -> None:
    ref.bucket = data.bucket
    ref.key = data.key
    ref.size_bytes = data.size_bytes
    ref.content_type = data.content_type
    ref.sha256 = data.sha256


def _merge_segments(
    segments: Sequence[mc.TranscriptSegment],
    max_chars: int = _ASR_MERGE_MAX_CHARS,
) -> list[mc.TranscriptSegment]:
    """Merge adjacent ASR segments into windows of up to ~max_chars.

    Each window keeps its own start (first segment's start) and end (last
    segment's end) so a search hit deep-links to the window's moment. Empty
    segments are dropped.
    """
    windows: list[mc.TranscriptSegment] = []
    cur_texts: list[str] = []
    cur_start = 0.0
    cur_end = 0.0
    cur_len = 0

    def flush() -> None:
        nonlocal cur_texts, cur_len
        if cur_texts:
            windows.append(
                mc.TranscriptSegment(start=cur_start, end=cur_end, text=" ".join(cur_texts).strip())
            )
        cur_texts = []
        cur_len = 0

    for seg in segments:
        text = (seg.text or "").strip()
        if not text:
            continue
        if cur_texts and cur_len + len(text) > max_chars:
            flush()
        if not cur_texts:
            cur_start = seg.start
        cur_end = seg.end
        cur_texts.append(text)
        cur_len += len(text) + 1
    flush()
    return windows


__all__ = [
    "MOD_ASR",
    "MOD_CAPTION",
    "MOD_OCR",
    "MediaHandler",
    "is_media",
    "media_kind",
]
