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

import hashlib
import json
import logging
import math
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
        revision: str | None = None,
    ) -> None:
        self._model_name = model
        self._revision_pref = revision
        self._revision = None
        self._device_pref = device
        self._precision_pref = precision
        self._max_length = max_length
        self._batch_size = batch_size
        self._device = "cpu"
        self._fp16 = False
        self._actual_precision = None
        self._lock = threading.Lock()
        self._loaded = False
        self._model = None
        self._torch = None
        self._memory_lock = threading.Lock()
        self._memory_samples = 0
        self._largest_tensor_bytes: int | None = None
        self._largest_driver_bytes: int | None = None

    @property
    def loaded(self) -> bool:
        return self._loaded

    @property
    def device(self) -> str:
        return self._device

    def identity(self) -> dict:
        values = {
            "model": self._model_name, "requestedRevision": self._revision_pref,
            "resolvedRevision": self._revision, "revisionVerified": bool(self._revision),
            "device": self._device if self._loaded else None,
            "precision": self._actual_precision if self._loaded else None,
            "requestedPrecision": self._precision_pref,
            "maxLength": self._max_length, "batchSize": self._batch_size,
            "resident": self._loaded, "maxInferenceConcurrency": 1,
        }
        values["fingerprint"] = hashlib.sha256(
            json.dumps(values, sort_keys=True).encode()
        ).hexdigest()
        # Runtime counters never enter the model/config fingerprint.
        values["memory"] = self._sample_memory()
        return values

    def _sample_memory(self) -> dict:
        tensor_bytes = driver_bytes = None
        if self._device == "mps" and self._torch is not None:
            try:
                tensor_bytes = self._torch.mps.current_allocated_memory()
                driver_bytes = self._torch.mps.driver_allocated_memory()
            except (AttributeError, RuntimeError, TypeError):
                tensor_bytes = driver_bytes = None
        with self._memory_lock:
            if type(tensor_bytes) is int and tensor_bytes >= 0 and (
                type(driver_bytes) is int and driver_bytes >= 0
            ):
                self._memory_samples += 1
                self._largest_tensor_bytes = max(self._largest_tensor_bytes or 0, tensor_bytes)
                self._largest_driver_bytes = max(self._largest_driver_bytes or 0, driver_bytes)
            else:
                tensor_bytes = driver_bytes = None
            return {
                "scope": "Sampled process MPS allocator counters; excludes CPU RSS, other "
                         "processes and whole-system peaks.",
                "currentTensorAllocatedBytes": tensor_bytes,
                "currentDriverAllocatedBytes": driver_bytes,
                "largestSampledTensorAllocatedBytes": self._largest_tensor_bytes,
                "largestSampledDriverAllocatedBytes": self._largest_driver_bytes,
                "sampleCount": self._memory_samples,
            }

    def load(self) -> None:
        """Import the ML stack and load the model weights (idempotent)."""
        with self._lock:
            if self._loaded:
                return
            import torch
            from sentence_transformers import CrossEncoder

            self._torch = torch
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
                revision=self._revision_pref,
            )
            if self._fp16:
                self._model.model.half()
            self._revision = getattr(self._model.model.config, "_commit_hash", None)
            actual_device = str(self._model.model.device).split(":", 1)[0]
            if actual_device != self._device:
                raise RuntimeError("Reranker model placement differs from requested device")
            parameter = next(self._model.model.parameters(), None)
            dtype = str(parameter.dtype) if parameter is not None else None
            self._actual_precision = {
                "torch.float32": "fp32", "torch.float16": "fp16", "torch.bfloat16": "bf16"
            }.get(dtype)
            # Readiness includes actual prediction, not just allocated weights.
            warm = self._model.predict(
                [["Asker readiness", "Asker native reranking readiness"]],
                batch_size=1, convert_to_numpy=True, show_progress_bar=False,
            )
            if len(warm) != 1 or not math.isfinite(float(warm[0])):
                raise RuntimeError("Reranker warm-up returned an invalid score")
            self._sample_memory()
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
        if any(not math.isfinite(value) for value in out):
            raise ScoreError("model returned a non-finite score")
        self._sample_memory()
        return out
