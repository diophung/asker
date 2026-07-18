"""The scorer interface and its real sentence-transformers implementation.

The HTTP layer depends only on the `Scorer` Protocol, so unit tests inject a
deterministic fake and never import torch / sentence-transformers or download a
model. The real implementation (`CrossEncoderScorer`) imports the ML stack
lazily inside `load()`.

Device selection adds "mps" (Apple GPU) to the cpu/cuda options: the reranker is
meant to run natively on the M5 Max (MPS) and the RTX host (cuda). The pure
`resolve_device` / `use_fp16` helpers are unit-tested without importing torch.
"""

from __future__ import annotations

import logging
import threading
from typing import Protocol, runtime_checkable

log = logging.getLogger("reranker.scorer")


class ScoreError(Exception):
    """A batch could not be scored (e.g. the model is not loaded)."""


@runtime_checkable
class Scorer(Protocol):
    """A cross-encoder relevance scorer.

    `score` takes the query and a list of candidate documents and returns one
    relevance score per document, IN INPUT ORDER (higher = more relevant).
    """

    def score(self, query: str, documents: list[str]) -> list[float]: ...


def resolve_device(pref: str, cuda_available: bool, mps_available: bool) -> str:
    """Resolve the requested device against what is actually present.

    "auto" -> cuda, else mps, else cpu. "cuda"/"mps"/"cpu" are honored when
    available; an unavailable accelerator falls back to cpu (the caller decides
    whether that is fatal — see load()). Pure, so it is unit-tested without torch.
    """
    if pref == "cpu":
        return "cpu"
    if pref == "cuda":
        return "cuda" if cuda_available else "cpu"
    if pref == "mps":
        return "mps" if mps_available else "cpu"
    # "auto"
    if cuda_available:
        return "cuda"
    if mps_available:
        return "mps"
    return "cpu"


def use_fp16(precision: str, device: str) -> bool:
    """Half precision only ever on cuda ("auto" => fp16 on cuda; "fp16" ignored
    on cpu/mps where half is slow/partially unsupported; "fp32" never)."""
    if device != "cuda":
        return False
    return precision in ("auto", "fp16")


class CrossEncoderScorer:
    """Production scorer backed by a sentence-transformers CrossEncoder.

    Construction is cheap (records config only); `load()` does the heavy import
    + weight load and must be called once before scoring. CrossEncoder.predict
    is not guaranteed thread-safe across calls, so scoring is serialized under a
    lock — adequate for a dev/CI service; production scales via replicas.
    """

    def __init__(
        self,
        model: str,
        device: str = "auto",
        precision: str = "auto",
        max_length: int = 512,
        batch_size: int = 32,
    ) -> None:
        self._model_name = model
        self._device_pref = device
        self._precision_pref = precision
        self._max_length = max_length
        self._batch_size = batch_size
        self._device = "cpu"
        self._fp16 = False
        self._lock = threading.Lock()
        self._loaded = False
        self._model = None

    @property
    def loaded(self) -> bool:
        return self._loaded

    @property
    def device(self) -> str:
        return self._device

    def load(self) -> None:
        """Import the ML stack and load the model weights (idempotent)."""
        with self._lock:
            if self._loaded:
                return
            import torch
            from sentence_transformers import CrossEncoder

            cuda_available = bool(torch.cuda.is_available())
            mps_backend = getattr(torch.backends, "mps", None)
            mps_available = bool(mps_backend and mps_backend.is_available())
            resolved = resolve_device(self._device_pref, cuda_available, mps_available)
            # An EXPLICIT accelerator request must not silently degrade to CPU and
            # then report healthy — the point of the GPU/MPS deploy is throughput,
            # and a silent CPU fallback only shows up as latency. Fail the load so
            # /health stays 503 until the device is fixed. "auto" stays lenient.
            if self._device_pref in ("cuda", "mps") and resolved != self._device_pref:
                raise RuntimeError(
                    f"RERANKER_DEVICE={self._device_pref} but that device is not available; "
                    f"refusing to serve on CPU. Fix the device or set RERANKER_DEVICE=auto."
                )
            self._device = resolved
            self._fp16 = use_fp16(self._precision_pref, self._device)

            log.info(
                "loading reranker model",
                extra={"model": self._model_name, "device": self._device},
            )
            self._model = CrossEncoder(
                self._model_name,
                device=self._device,
                max_length=self._max_length,
            )
            if self._fp16:
                self._model.model.half()
            self._loaded = True
            log.info("reranker model loaded", extra={"device": self._device, "fp16": self._fp16})

    def score(self, query: str, documents: list[str]) -> list[float]:
        if not documents:
            return []
        if not self._loaded:
            raise ScoreError("reranker model is not loaded yet")
        pairs = [[query, doc] for doc in documents]
        with self._lock:
            scores = self._model.predict(
                pairs,
                batch_size=self._batch_size,
                convert_to_numpy=True,
                show_progress_bar=False,
            )
        out = [float(s) for s in scores.tolist()]
        if len(out) != len(documents):
            raise ScoreError(f"model returned {len(out)} scores for {len(documents)} documents")
        return out
