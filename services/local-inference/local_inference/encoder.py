"""Lazy ML imports allow contract tests to run without model downloads."""
from __future__ import annotations

import hashlib
import json
import math
import threading
import time

from .config import Config


class BusyError(Exception):
    """Defensive guard for direct calls outside the server's bounded admission."""


def resolve_device(preference: str, mps_available: bool) -> str:
    if preference == "mps" and not mps_available:
        raise RuntimeError("MPS was requested but is unavailable; refusing CPU fallback")
    return "mps" if preference == "mps" or (preference == "auto" and mps_available) else "cpu"


class Encoder:
    def __init__(self, config: Config) -> None:
        self.config = config
        self.ready = False
        self.device: str | None = None
        self.revision: str | None = None
        self._model = None
        self._slot = threading.BoundedSemaphore(1)
        self._load_lock = threading.Lock()
        self.load_ms: float | None = None
        self._torch = None
        self._memory_lock = threading.Lock()
        self._memory_samples = 0
        self._largest_tensor_bytes: int | None = None
        self._largest_driver_bytes: int | None = None

    def load(self) -> None:
        """Load once, verify the dimension and warm an actual encode before readiness."""
        with self._load_lock:
            if self.ready:
                return
            import torch
            from sentence_transformers import SentenceTransformer

            started = time.monotonic()
            self._torch = torch
            torch.set_num_threads(self.config.cpu_threads)
            backend = getattr(torch.backends, "mps", None)
            self.device = resolve_device(self.config.device, bool(backend and backend.is_available()))
            self._model = SentenceTransformer(
                self.config.model, revision=self.config.revision, device=self.device,
                trust_remote_code=False,
            )
            dimension = self._model.get_sentence_embedding_dimension()
            if dimension != self.config.dimension:
                raise RuntimeError(f"Model dimension {dimension} does not match EMBEDDING_DIM={self.config.dimension}; reindex explicitly")
            self._model.max_seq_length = self.config.max_length
            first = self._model[0]
            model_config = getattr(getattr(first, "auto_model", None), "config", None)
            self.revision = getattr(model_config, "_commit_hash", None) or self.config.revision
            self._encode(["Asker native embedding readiness"])
            self.load_ms = round((time.monotonic() - started) * 1000, 3)
            self.ready = True

    def identity(self) -> dict:
        precision = model_device = None
        if self._model is not None:
            try:
                parameter = next(self._model.parameters())
                precision, model_device = str(parameter.dtype), str(parameter.device)
            except (AttributeError, StopIteration):
                pass
        values = {"model": self.config.model, "requestedRevision": self.config.revision,
                  "resolvedRevision": self.revision, "revisionVerified": bool(self.revision),
                  "dimension": self.config.dimension, "device": self.device,
                  "actualModelDevice": model_device, "parameterDtype": precision,
                  "normalization": "l2", "maxLength": self.config.max_length,
                  "batchSize": self.config.batch_size, "maxInferenceConcurrency": 1,
                  "resident": self.ready, "loadAndWarmupMs": self.load_ms,
                  "memory": self._sample_memory()}
        binding = {key: values[key] for key in ("model", "resolvedRevision", "dimension", "normalization", "maxLength")}
        values["fingerprint"] = hashlib.sha256(json.dumps(binding, sort_keys=True).encode()).hexdigest()
        return values

    def _sample_memory(self) -> dict:
        """Observe allocator counters; unavailable values are unknown, not zero."""
        tensor_bytes = driver_bytes = None
        if self.device == "mps" and self._torch is not None:
            try:
                tensor_bytes = self._torch.mps.current_allocated_memory()
                driver_bytes = self._torch.mps.driver_allocated_memory()
            except (AttributeError, RuntimeError, TypeError):
                tensor_bytes = driver_bytes = None
        with self._memory_lock:
            if type(tensor_bytes) is int and tensor_bytes >= 0 and type(driver_bytes) is int and driver_bytes >= 0:
                self._memory_samples += 1
                self._largest_tensor_bytes = max(self._largest_tensor_bytes or 0, tensor_bytes)
                self._largest_driver_bytes = max(self._largest_driver_bytes or 0, driver_bytes)
            else:
                tensor_bytes = driver_bytes = None
            return {
                "scope": "Sampled MPS allocator counters; excludes CPU RSS, other processes and whole-system peaks.",
                "currentTensorAllocatedBytes": tensor_bytes,
                "currentDriverAllocatedBytes": driver_bytes,
                "largestSampledTensorAllocatedBytes": self._largest_tensor_bytes,
                "largestSampledDriverAllocatedBytes": self._largest_driver_bytes,
                "sampleCount": self._memory_samples,
            }

    def _encode(self, texts: list[str]) -> list[list[float]]:
        vectors = self._model.encode(
            texts, batch_size=self.config.batch_size, normalize_embeddings=True,
            convert_to_numpy=True, show_progress_bar=False,
        ).tolist()
        if len(vectors) != len(texts) or any(
            len(vector) != self.config.dimension or any(not math.isfinite(float(x)) for x in vector)
            for vector in vectors
        ):
            raise RuntimeError("Encoder returned invalid vectors")
        self._sample_memory()
        return vectors

    def embed(self, texts: list[str]) -> list[list[float]]:
        if not self.ready:
            raise RuntimeError("Model is not ready")
        if not self._slot.acquire(blocking=False):
            raise BusyError("Native encoder is busy")
        try:
            return self._encode(texts) if texts else []
        finally:
            self._slot.release()
