"""External collaborators for the media enrich pipeline (ADR-013).

Every dependency the media handler touches over the network or a subprocess is
declared here as a small Protocol and given a production implementation. The
handler takes these as injected collaborators (exactly like the existing
Kafka/embedder injection in kafka_worker.py), so unit tests substitute
deterministic fakes and never invoke a real model, binary, or HTTP server.

Heavy libraries (faster-whisper / Pillow / pytesseract) and the ffmpeg/ffprobe
binaries are imported or spawned **lazily, inside the production methods** — so
importing this module (which the worker does at startup) never drags torch-free
CTranslate2, Pillow, etc. into a unit-test process that only uses fakes.
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import subprocess
import tempfile
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

import httpx

log = logging.getLogger("enrich.media")

# The internal hop header the connector-hub validates (ADR-009/013); mirrors
# tenantHeader in services/connector-hub/internal/hub/http.go.
TENANT_HEADER = "x-asker-tenant"

_HTTP_TIMEOUT = 120.0  # decrypt/store + a CLIP forward pass can be slow on CPU

# Wall-clock bound on any single ffmpeg/ffprobe invocation. A malformed or
# adversarial video can make ffmpeg spin or block forever; without a timeout it
# would hang the worker (and its partition) indefinitely. On expiry the subprocess
# is killed and the call raises MediaError, so the record flows through the
# worker's retry/dead-letter loop instead (ADR-013).
_FFMPEG_TIMEOUT = 120.0


class MediaError(Exception):
    """A media collaborator failed; routed through the worker retry/dead-letter."""


# --- value types crossing the collaborator boundary -------------------------


@dataclass(frozen=True)
class BlobRefData:
    """Plain-data BlobRef, mirroring asker.v1.BlobRef + the hub PUT JSON.

    Used both to identify the bytes to fetch (key + sha256 + bucket +
    content_type from Document.original) and to carry a stored thumbnail /
    keyframe back into Document.media.
    """

    bucket: str = ""
    key: str = ""
    size_bytes: int = 0
    content_type: str = ""
    sha256: str = ""


@dataclass(frozen=True)
class FetchedMedia:
    """The decrypted original bytes plus the content type the hub reported."""

    data: bytes
    content_type: str


@dataclass(frozen=True)
class TranscriptSegment:
    """One ASR segment: text plus its [start, end) offset in SECONDS."""

    start: float
    end: float
    text: str


@dataclass(frozen=True)
class Transcript:
    """A full transcription: ordered segments + detected BCP-47 language + total length."""

    segments: list[TranscriptSegment] = field(default_factory=list)
    language: str = ""
    duration_ms: int = 0


@dataclass(frozen=True)
class KeyframeImage:
    """One extracted video frame: JPEG bytes anchored at ts_ms within the video."""

    ts_ms: int
    jpeg: bytes


@dataclass(frozen=True)
class VideoInfo:
    """ffprobe-derived video metadata.

    has_audio reflects whether ffprobe reported any audio stream; a video with
    none (silent screen capture, GIF-style clip) must still index its visual
    content rather than dead-letter on a doomed transcription attempt (ADR-013).
    """

    duration_ms: int = 0
    width: int = 0
    height: int = 0
    has_audio: bool = False


@dataclass(frozen=True)
class ThumbnailResult:
    """A generated thumbnail: JPEG bytes + the source image's pixel dimensions."""

    jpeg: bytes
    width: int
    height: int


# --- collaborator Protocols (what the handler depends on) -------------------


class MediaStore(Protocol):
    """Fetches decrypted originals and stores encrypted derivatives via the hub."""

    async def fetch(self, tenant_id: str, original: BlobRefData) -> FetchedMedia: ...

    async def store(
        self, tenant_id: str, key: str, content_type: str, data: bytes
    ) -> BlobRefData: ...


class ClipClient(Protocol):
    """Embeds image bytes into the CLIP space (len CLIP_DIM, L2-normalized)."""

    async def embed_images(self, images: Sequence[bytes]) -> list[list[float]]: ...


class OCRFunc(Protocol):
    """Extracts text from image bytes (Tesseract in production)."""

    def __call__(self, image: bytes) -> str: ...


class Transcriber(Protocol):
    """Transcribes audio bytes (faster-whisper in production)."""

    def __call__(self, audio: bytes, content_type: str) -> Transcript: ...


class Thumbnailer(Protocol):
    """Resizes image bytes to a thumbnail JPEG and reports source dimensions."""

    def __call__(self, image: bytes) -> ThumbnailResult: ...


class VideoExtractor(Protocol):
    """ffmpeg/ffprobe over a video: audio track, scene keyframes, and metadata."""

    def probe(self, video: bytes, content_type: str) -> VideoInfo: ...

    def extract_audio(self, video: bytes, content_type: str) -> bytes: ...

    def extract_keyframes(
        self, video: bytes, content_type: str, max_keyframes: int
    ) -> list[KeyframeImage]: ...


# --- production implementations ---------------------------------------------


class HubMediaStore:
    """MediaStore backed by the connector-hub /internal/media endpoint (ADR-013).

    GET returns decrypted bytes (the worker passes back the full BlobRef it
    already holds — key/sha256/bucket/content_type — because blob.Get verifies
    the plaintext digest). PUT stores encrypted and returns a BlobRef JSON.
    The x-asker-tenant header scopes both to the calling tenant; the crypto
    never leaves Go.
    """

    def __init__(
        self,
        base_url: str,
        client: httpx.AsyncClient | None = None,
        timeout: float = _HTTP_TIMEOUT,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(timeout=httpx.Timeout(timeout))

    async def fetch(self, tenant_id: str, original: BlobRefData) -> FetchedMedia:
        if not original.key:
            raise MediaError("document.original.key is empty; nothing to fetch")
        params = {"key": original.key, "sha256": original.sha256}
        if original.bucket:
            params["bucket"] = original.bucket
        if original.content_type:
            params["content_type"] = original.content_type
        try:
            resp = await self._client.get(
                f"{self._base}/internal/media",
                params=params,
                headers={TENANT_HEADER: tenant_id},
            )
            resp.raise_for_status()
        except httpx.HTTPError as exc:
            raise MediaError(f"hub media fetch failed: {exc}") from exc
        return FetchedMedia(
            data=resp.content,
            content_type=resp.headers.get("content-type", original.content_type),
        )

    async def store(self, tenant_id: str, key: str, content_type: str, data: bytes) -> BlobRefData:
        try:
            resp = await self._client.put(
                f"{self._base}/internal/media",
                params={"key": key, "content_type": content_type},
                headers={TENANT_HEADER: tenant_id},
                content=data,
            )
            resp.raise_for_status()
            body = resp.json()
        except (httpx.HTTPError, ValueError) as exc:
            raise MediaError(f"hub media store failed: {exc}") from exc
        if not isinstance(body, dict):
            raise MediaError(f"hub media store returned non-object: {type(body).__name__}")
        return BlobRefData(
            bucket=str(body.get("bucket", "")),
            key=str(body.get("key", "")),
            size_bytes=int(body.get("size_bytes", 0) or 0),
            content_type=str(body.get("content_type", "")),
            sha256=str(body.get("sha256", "")),
        )

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()


class HttpClipClient:
    """ClipClient calling the clip service POST /embed/image (services/clip)."""

    def __init__(
        self,
        base_url: str,
        clip_dim: int,
        client: httpx.AsyncClient | None = None,
        timeout: float = _HTTP_TIMEOUT,
    ) -> None:
        if clip_dim <= 0:
            raise ValueError(f"clip_dim must be positive, got {clip_dim}")
        self._base = base_url.rstrip("/")
        self._dim = clip_dim
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(timeout=httpx.Timeout(timeout))

    @property
    def clip_dim(self) -> int:
        return self._dim

    async def embed_images(self, images: Sequence[bytes]) -> list[list[float]]:
        if not images:
            return []
        b64 = [base64.b64encode(img).decode("ascii") for img in images]
        try:
            resp = await self._client.post(f"{self._base}/embed/image", json={"images_b64": b64})
            resp.raise_for_status()
            body = resp.json()
        except (httpx.HTTPError, ValueError) as exc:
            raise MediaError(f"clip /embed/image failed: {exc}") from exc
        vecs = body.get("embeddings") if isinstance(body, dict) else None
        if not isinstance(vecs, list) or len(vecs) != len(images):
            raise MediaError(
                f"clip /embed/image returned {len(vecs) if isinstance(vecs, list) else '?'} "
                f"vectors for {len(images)} images"
            )
        out: list[list[float]] = []
        for vec in vecs:
            if not isinstance(vec, list) or len(vec) != self._dim:
                raise MediaError(
                    f"clip returned a vector of length "
                    f"{len(vec) if isinstance(vec, list) else '?'}, want CLIP_DIM={self._dim}; "
                    f"CLIP_DIM must match the clip service's model (ADR-013)"
                )
            out.append([float(x) for x in vec])
        return out

    async def healthy(self) -> tuple[bool, str]:
        try:
            resp = await self._client.get(f"{self._base}/health")
        except httpx.HTTPError as exc:
            return False, f"clip unreachable: {exc}"
        if resp.status_code == 200:
            return True, "ok"
        return False, f"clip /health returned status {resp.status_code}"

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()


def tesseract_ocr(image: bytes) -> str:
    """Run Tesseract OCR over image bytes, returning the stripped text.

    Pillow + pytesseract + the tesseract binary are imported/invoked lazily so
    unit tests (which inject a fake OCRFunc) never need them installed.
    """
    import io

    import pytesseract
    from PIL import Image

    try:
        with Image.open(io.BytesIO(image)) as img:
            return pytesseract.image_to_string(img).strip()
    except Exception as exc:
        raise MediaError(f"tesseract OCR failed: {exc}") from exc


def pillow_thumbnailer(max_px: int) -> Thumbnailer:
    """Build a Thumbnailer that resizes to <= max_px on the longest edge (JPEG)."""

    def thumbnail(image: bytes) -> ThumbnailResult:
        import io

        from PIL import Image

        try:
            with Image.open(io.BytesIO(image)) as img:
                width, height = img.size
                rgb = img.convert("RGB")
                rgb.thumbnail((max_px, max_px))
                buf = io.BytesIO()
                rgb.save(buf, format="JPEG", quality=85)
                return ThumbnailResult(jpeg=buf.getvalue(), width=width, height=height)
        except Exception as exc:
            raise MediaError(f"thumbnail generation failed: {exc}") from exc

    return thumbnail


class WhisperTranscriber:
    """Transcriber backed by faster-whisper (CTranslate2, CPU, int8) — no torch.

    The model is loaded lazily on first use and reused; the weights download on
    first run (note for the integrator: cache the model directory on a volume).
    """

    def __init__(self, model: str = "tiny", compute_type: str = "int8") -> None:
        self._model_name = model
        self._compute_type = compute_type
        self._model = None  # lazy: avoids importing faster-whisper at startup

    def _ensure_model(self) -> object:
        if self._model is None:
            from faster_whisper import WhisperModel

            self._model = WhisperModel(
                self._model_name, device="cpu", compute_type=self._compute_type
            )
            log.info(
                "whisper model loaded",
                extra={"model": self._model_name, "compute_type": self._compute_type},
            )
        return self._model

    def __call__(self, audio: bytes, content_type: str) -> Transcript:
        model = self._ensure_model()
        suffix = _suffix_for(content_type, default=".wav")
        with tempfile.NamedTemporaryFile(suffix=suffix) as tmp:
            tmp.write(audio)
            tmp.flush()
            try:
                segments, info = model.transcribe(tmp.name)  # type: ignore[attr-defined]
                segs = [
                    TranscriptSegment(start=float(s.start), end=float(s.end), text=s.text.strip())
                    for s in segments
                ]
            except Exception as exc:
                raise MediaError(f"whisper transcription failed: {exc}") from exc
        duration_ms = int(getattr(info, "duration", 0.0) * 1000)
        return Transcript(
            segments=segs,
            language=getattr(info, "language", "") or "",
            duration_ms=duration_ms,
        )


class FFmpegVideoExtractor:
    """VideoExtractor shelling out to ffmpeg/ffprobe (the binaries in the image).

    ``timeout`` caps each ffmpeg/ffprobe invocation so a malformed or adversarial
    video cannot hang the worker indefinitely (ADR-013); on expiry the call raises
    MediaError and the record routes to retry/dead-letter.
    """

    def __init__(self, scene_threshold: float = 0.4, timeout: float = _FFMPEG_TIMEOUT) -> None:
        self._scene_threshold = scene_threshold
        self._timeout = timeout

    def probe(self, video: bytes, content_type: str) -> VideoInfo:
        suffix = _suffix_for(content_type, default=".mp4")
        with tempfile.NamedTemporaryFile(suffix=suffix) as tmp:
            tmp.write(video)
            tmp.flush()
            args = [
                "ffprobe",
                "-v",
                "error",
                "-print_format",
                "json",
                "-show_format",
                "-show_streams",
                tmp.name,
            ]
            out = _run(args, "ffprobe", timeout=self._timeout)
        try:
            meta = json.loads(out)
        except ValueError as exc:
            raise MediaError(f"ffprobe returned non-JSON: {exc}") from exc
        width = height = 0
        has_video = has_audio = False
        for stream in meta.get("streams", []):
            codec_type = stream.get("codec_type")
            if codec_type == "video" and not has_video:
                width = int(stream.get("width", 0) or 0)
                height = int(stream.get("height", 0) or 0)
                has_video = True
            elif codec_type == "audio":
                has_audio = True
        duration_s = 0.0
        try:
            duration_s = float(meta.get("format", {}).get("duration", 0.0) or 0.0)
        except (TypeError, ValueError):
            duration_s = 0.0
        return VideoInfo(
            duration_ms=int(duration_s * 1000),
            width=width,
            height=height,
            has_audio=has_audio,
        )

    def extract_audio(self, video: bytes, content_type: str) -> bytes:
        in_suffix = _suffix_for(content_type, default=".mp4")
        with tempfile.TemporaryDirectory() as d:
            src = Path(d) / f"in{in_suffix}"
            dst = Path(d) / "audio.wav"
            src.write_bytes(video)
            # 16 kHz mono PCM — what whisper wants, smallest sane decode.
            args = [
                "ffmpeg",
                "-nostdin",
                "-y",
                "-i",
                str(src),
                "-vn",
                "-ac",
                "1",
                "-ar",
                "16000",
                "-f",
                "wav",
                str(dst),
            ]
            _run(args, "ffmpeg audio extract", timeout=self._timeout)
            if not dst.exists():
                raise MediaError("ffmpeg produced no audio track")
            return dst.read_bytes()

    def extract_keyframes(
        self, video: bytes, content_type: str, max_keyframes: int
    ) -> list[KeyframeImage]:
        if max_keyframes <= 0:
            return []
        in_suffix = _suffix_for(content_type, default=".mp4")
        with tempfile.TemporaryDirectory() as d:
            src = Path(d) / f"in{in_suffix}"
            src.write_bytes(video)
            frames = self._extract_with_filter(
                src,
                Path(d),
                f"select='gt(scene,{self._scene_threshold})',showinfo",
                max_keyframes,
            )
            if not frames:
                # Fallback: periodic sampling (1 frame / 5 s) for low-motion clips
                # that trip no scene cut.
                frames = self._extract_with_filter(src, Path(d), "fps=1/5,showinfo", max_keyframes)
            return frames

    def _extract_with_filter(
        self, src: Path, workdir: Path, vf: str, max_keyframes: int
    ) -> list[KeyframeImage]:
        pattern = str(workdir / "kf-%05d.jpg")
        args = [
            "ffmpeg",
            "-nostdin",
            "-y",
            "-i",
            str(src),
            "-vf",
            vf,
            "-vsync",
            "vfr",
            "-frames:v",
            str(max_keyframes),
            pattern,
        ]
        stderr = _run(args, "ffmpeg keyframes", capture_stderr=True, timeout=self._timeout)
        timestamps = _parse_showinfo_pts(stderr)
        files = sorted(workdir.glob("kf-*.jpg"))
        out: list[KeyframeImage] = []
        for i, fpath in enumerate(files[:max_keyframes]):
            ts_ms = int(timestamps[i] * 1000) if i < len(timestamps) else 0
            out.append(KeyframeImage(ts_ms=ts_ms, jpeg=fpath.read_bytes()))
        return out


# --- helpers ----------------------------------------------------------------

_CONTENT_TYPE_SUFFIX = {
    "image/jpeg": ".jpg",
    "image/png": ".png",
    "image/webp": ".webp",
    "image/gif": ".gif",
    "audio/wav": ".wav",
    "audio/x-wav": ".wav",
    "audio/mpeg": ".mp3",
    "audio/mp4": ".m4a",
    "audio/ogg": ".ogg",
    "video/mp4": ".mp4",
    "video/quicktime": ".mov",
    "video/webm": ".webm",
    "video/x-matroska": ".mkv",
}


def _suffix_for(content_type: str, default: str) -> str:
    """Map a MIME type to a temp-file suffix ffmpeg/whisper can sniff."""
    return _CONTENT_TYPE_SUFFIX.get((content_type or "").split(";", 1)[0].strip().lower(), default)


def _parse_showinfo_pts(stderr: str) -> list[float]:
    """Pull pts_time values (seconds) from ffmpeg's showinfo log, in order."""
    times: list[float] = []
    for line in stderr.splitlines():
        marker = "pts_time:"
        idx = line.find(marker)
        if idx == -1:
            continue
        rest = line[idx + len(marker) :]
        token = rest.split()[0] if rest.split() else ""
        try:
            times.append(float(token))
        except ValueError:
            continue
    return times


def _run(
    args: list[str],
    what: str,
    capture_stderr: bool = False,
    timeout: float = _FFMPEG_TIMEOUT,
    runner: Callable[..., subprocess.CompletedProcess[bytes]] = subprocess.run,
) -> str:
    """Run a trusted, fixed-arg ffmpeg/ffprobe command; raise MediaError on failure.

    Returns stdout, or stderr when capture_stderr (ffmpeg writes showinfo there).
    A per-call ``timeout`` (seconds) bounds the wall clock: on expiry the child
    is killed and MediaError is raised, so a hung/adversarial video routes to
    retry/dead-letter rather than blocking the worker forever. ``runner`` is the
    subprocess.run-shaped callable, injectable so tests can simulate a timeout
    without spawning a real process.
    """
    try:
        # args are fixed, not shell-interpolated (the default runner is subprocess.run).
        proc = runner(
            args,
            capture_output=True,
            check=True,
            timeout=timeout,
        )
    except FileNotFoundError as exc:
        raise MediaError(f"{what}: binary not found ({args[0]})") from exc
    except subprocess.TimeoutExpired as exc:
        raise MediaError(f"{what} timed out after {timeout:g}s") from exc
    except subprocess.CalledProcessError as exc:
        tail = (exc.stderr or b"").decode("utf-8", "replace")[-500:]
        raise MediaError(f"{what} exited {exc.returncode}: {tail}") from exc
    return (proc.stderr if capture_stderr else proc.stdout).decode("utf-8", "replace")


def to_thread(fn: Callable[..., object], *args: object) -> Awaitable[object]:
    """Run a blocking collaborator off the event loop (CPU/subprocess work)."""
    return asyncio.to_thread(fn, *args)
