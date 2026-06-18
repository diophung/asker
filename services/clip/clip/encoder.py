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
        self, model: str, pretrained: str, clip_dim: int, device: str = "auto"
    ) -> None:
        if clip_dim <= 0:
            raise ValueError(f"clip_dim must be positive, got {clip_dim}")
        self._model_name = model
        self._pretrained = pretrained
        self._dim = clip_dim
        # Operator intent: "auto" (resolve at load), or a forced "cuda"/"cpu".
        self._device_pref = device
        # The resolved torch device string, set in load().
        self._device = "cpu"
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
            # Resolve "auto" now that torch can probe for a visible GPU; an
            # explicit cuda request with no GPU is a misconfiguration we surface
            # loudly rather than silently running 50x slower on CPU.
            if self._device_pref == "auto":
                self._device = "cuda" if torch.cuda.is_available() else "cpu"
            else:
                if self._device_pref == "cuda" and not torch.cuda.is_available():
                    raise RuntimeError(
                        "CLIP_DEVICE=cuda but no CUDA GPU is visible to this "
                        "container (need the nvidia container runtime + a CUDA "
                        "torch build — see docs/two-host-deploy.md)"
                    )
                self._device = self._device_pref
            model.to(self._device)
            self._torch = torch
            self._model = model
            self._preprocess = preprocess
            self._tokenizer = open_clip.get_tokenizer(self._model_name)
            self._Image = Image
            self._loaded = True
            log.info("clip model loaded", extra={"dim": self._dim, "device": self._device})

    def embed_text(self, inputs: list[str]) -> list[list[float]]:
        if not inputs:
            return []
        self._require_loaded()
        with self._lock:
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
        rows = features.detach().cpu().tolist()
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
