"""The encoder interface and its real open_clip implementation.

One loaded open_clip model provides BOTH the text and image encoders, so the
two sets of vectors live in the same space (ADR-013). Every vector is
L2-normalized here, so Vespa's 'angular'/dotproduct closeness over
clip_embedding is cosine similarity.

The real implementation (`OpenClipEncoder`) imports torch / open_clip / Pillow
*lazily* inside `load()`. The HTTP layer depends only on the `Encoder`
Protocol, so unit tests inject a deterministic fake and never download or
import the heavy ML stack.
"""

from __future__ import annotations

import base64
import binascii
import io
import logging
import math
import threading
from typing import Protocol, runtime_checkable

log = logging.getLogger("clip.encoder")


class EncodeError(Exception):
    """A request could not be encoded (e.g. an image failed to decode)."""


class DimensionMismatchError(Exception):
    """The loaded model produced a vector whose length differs from CLIP_DIM.

    This is operator misconfiguration (CLIP_DIM disagreeing with the model the
    service serves, ADR-013) — retrying cannot fix it.
    """


@runtime_checkable
class Encoder(Protocol):
    """Both encoders of one shared-space CLIP model.

    `embed_text` takes plain strings; `embed_image` takes raw (already
    base64-decoded) image bytes. Both return one L2-normalized CLIP_DIM-length
    vector per input, in input order.
    """

    @property
    def clip_dim(self) -> int: ...

    def embed_text(self, inputs: list[str]) -> list[list[float]]: ...

    def embed_image(self, images: list[bytes]) -> list[list[float]]: ...


def resolve_device(pref: str, cuda_available: bool) -> str:
    """Resolve the requested device preference against what is actually present.

    "auto" -> cuda when visible else cpu. "cuda" requested but unavailable falls
    back to cpu (a GPU image accidentally run without a GPU still serves, just on
    CPU) — the caller logs the downgrade. "cpu" is honored verbatim. Pure so the
    selection is unit-tested without importing torch.
    """
    if pref == "cuda":
        return "cuda" if cuda_available else "cpu"
    if pref == "cpu":
        return "cpu"
    # "auto"
    return "cuda" if cuda_available else "cpu"


def use_fp16(precision: str, device: str) -> bool:
    """Decide half precision: only ever on cuda. "auto" => fp16 on cuda; "fp16"
    is ignored on cpu (cpu half is slow/partially unsupported); "fp32" never."""
    if device != "cuda":
        return False
    return precision in ("auto", "fp16")


def l2_normalize(vector: list[float]) -> list[float]:
    """Return the unit vector; a zero vector is returned unchanged.

    Used by both the real and fake encoders so the contract (unit vectors ->
    cosine via dotproduct) holds identically in tests and production.
    """
    norm = math.sqrt(sum(x * x for x in vector))
    if norm == 0.0:
        return list(vector)
    return [x / norm for x in vector]


def decode_images_b64(images_b64: list[str]) -> list[bytes]:
    """Strict base64-decode each image, raising EncodeError on bad input.

    Accepts an optional data-URI prefix ("data:image/png;base64,") for
    convenience; everything after the comma is decoded.
    """
    out: list[bytes] = []
    for i, encoded in enumerate(images_b64):
        if not isinstance(encoded, str):
            raise EncodeError(f"images_b64[{i}] is not a string")
        payload = encoded.split(",", 1)[1] if encoded.startswith("data:") else encoded
        try:
            out.append(base64.b64decode(payload, validate=True))
        except (binascii.Error, ValueError) as exc:
            raise EncodeError(f"images_b64[{i}] is not valid base64: {exc}") from exc
    return out


class OpenClipEncoder:
    """The production encoder backed by a single loaded open_clip model.

    Construction is cheap (records config only); `load()` does the heavy import
    + weight load and must be called once before encoding. open_clip / torch
    forward passes are not guaranteed thread-safe, so encoding is serialized
    under a lock — adequate for a CPU dev/CI service; production scales via
    replicas (ADR-013).
    """

    def __init__(
        self,
        model: str,
        pretrained: str,
        clip_dim: int,
        device: str = "auto",
        precision: str = "auto",
    ) -> None:
        if clip_dim <= 0:
            raise ValueError(f"clip_dim must be positive, got {clip_dim}")
        self._model_name = model
        self._pretrained = pretrained
        self._dim = clip_dim
        self._device_pref = device
        self._precision_pref = precision
        # Resolved at load() once torch can report CUDA availability.
        self._device = "cpu"
        self._fp16 = False
        self._lock = threading.Lock()
        self._loaded = False
        self._torch = None
        self._model = None
        self._preprocess = None
        self._tokenizer = None
        self._Image = None

    @property
    def clip_dim(self) -> int:
        return self._dim

    @property
    def loaded(self) -> bool:
        return self._loaded

    @property
    def device(self) -> str:
        """The resolved compute device ("cpu"/"cuda"); "cpu" until load()."""
        return self._device

    @property
    def fp16(self) -> bool:
        return self._fp16

    def load(self) -> None:
        """Import the ML stack and load the model weights (idempotent).

        First call may download weights (dev/CI pre-pull via CLIP cache volume,
        see README) and takes several seconds; subsequent calls are no-ops.
        """
        with self._lock:
            if self._loaded:
                return
            # Lazy imports: keep torch / open_clip / Pillow out of unit tests.
            import open_clip
            import torch
            from PIL import Image

            log.info(
                "loading clip model",
                extra={"model": self._model_name, "pretrained": self._pretrained},
            )
            model, _, preprocess = open_clip.create_model_and_transforms(
                self._model_name, pretrained=self._pretrained
            )
            model.eval()
            torch.set_grad_enabled(False)

            # Resolve placement now that torch can report CUDA availability, then
            # move the weights onto the device. "cuda" requested without a GPU
            # downgrades to cpu (logged) rather than crashing — the same image
            # serves on the 3090 and on the emulated dev Mac.
            cuda_available = bool(torch.cuda.is_available())
            resolved = resolve_device(self._device_pref, cuda_available)
            # An EXPLICIT cuda request must not silently degrade to CPU and then
            # report healthy — the whole point of the GPU deploy is fp16-on-cuda
            # throughput, and a silent CPU fallback only shows up as latency. Fail
            # the load so /health stays 503 until the GPU/driver/toolkit is fixed
            # (main.py keeps the listener up and retries). "auto" stays lenient.
            if self._device_pref == "cuda" and resolved != "cuda":
                raise RuntimeError(
                    "CLIP_DEVICE=cuda but no CUDA device is visible "
                    "(torch.cuda.is_available() is False); refusing to serve on CPU. "
                    "Fix the GPU/driver/NVIDIA-container-toolkit, or set CLIP_DEVICE=auto "
                    "to allow a CPU fallback."
                )
            self._device = resolved
            self._fp16 = use_fp16(self._precision_pref, self._device)
            model = model.to(self._device)
            if self._fp16:
                model = model.half()

            self._torch = torch
            self._model = model
            self._preprocess = preprocess
            self._tokenizer = open_clip.get_tokenizer(self._model_name)
            self._Image = Image
            self._loaded = True
            log.info(
                "clip model loaded",
                extra={"dim": self._dim, "device": self._device, "fp16": self._fp16},
            )

    def embed_text(self, inputs: list[str]) -> list[list[float]]:
        if not inputs:
            return []
        self._require_loaded()
        with self._lock:
            # Token ids are integer tensors (no dtype cast) — only place them on
            # the model's device.
            tokens = self._tokenizer(inputs).to(self._device)
            with self._torch.no_grad():
                features = self._model.encode_text(tokens)
            return self._to_unit_vectors(features, len(inputs))

    def embed_image(self, images: list[bytes]) -> list[list[float]]:
        if not images:
            return []
        self._require_loaded()
        tensors = [self._preprocess_one(raw, i) for i, raw in enumerate(images)]
        with self._lock:
            batch = self._torch.stack(tensors).to(self._device)
            # preprocess yields float32; match the model's dtype when running half.
            if self._fp16:
                batch = batch.half()
            with self._torch.no_grad():
                features = self._model.encode_image(batch)
            return self._to_unit_vectors(features, len(images))

    def _preprocess_one(self, raw: bytes, index: int):
        try:
            with self._Image.open(io.BytesIO(raw)) as img:
                rgb = img.convert("RGB")
            return self._preprocess(rgb)
        except Exception as exc:  # PIL raises a zoo of errors on bad bytes
            raise EncodeError(f"images[{index}] could not be decoded: {exc}") from exc

    def _to_unit_vectors(self, features, expected: int) -> list[list[float]]:
        # .float() upcasts half (cuda fp16) back to float32 before leaving the
        # GPU, so the JSON vectors are full-precision regardless of compute dtype.
        rows = features.detach().float().cpu().tolist()
        if len(rows) != expected:
            raise EncodeError(f"model returned {len(rows)} vectors for {expected} inputs")
        out: list[list[float]] = []
        for vec in rows:
            if len(vec) != self._dim:
                raise DimensionMismatchError(
                    f"model returned a vector of length {len(vec)}, want CLIP_DIM={self._dim}; "
                    f"CLIP_DIM must match the model the service serves (ADR-013)"
                )
            out.append(l2_normalize([float(x) for x in vec]))
        return out

    def _require_loaded(self) -> None:
        if not self._loaded:
            raise EncodeError("clip model is not loaded yet")
