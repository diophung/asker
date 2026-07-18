"""Environment configuration for the cross-encoder reranker service.

Mirrors services/clip/clip/config.py. The default model is BAAI/bge-reranker-
v2-m3 (Apache-2.0, 568M) — the latency-safe default that fits the ~300ms /
50-candidate budget on Apple Silicon with fp16/MPS. RERANKER_DEVICE adds "mps"
(Apple GPU) to the cpu/cuda choices the clip service has, since the reranker is
meant to run natively on the M5 Max (MPS) as well as the RTX host (cuda).
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

# Model identity. bge-reranker-v2-m3 is an encoder cross-encoder — small, strong
# multilingual, and the one option that reliably lands in the rerank latency
# budget on Apple Silicon. Swappable via RERANKER_MODEL (e.g. a Qwen3-Reranker).
DEFAULT_MODEL = "BAAI/bge-reranker-v2-m3"
DEFAULT_PORT = 9900

# Compute placement. "auto" prefers cuda (the RTX host), then mps (Apple GPU on
# the M5 Max), then cpu. "precision" governs fp16, only ever on cuda.
DEFAULT_DEVICE = "auto"
DEFAULT_PRECISION = "auto"
ALLOWED_DEVICES = ("auto", "cpu", "cuda", "mps")
ALLOWED_PRECISIONS = ("auto", "fp16", "fp32")

# Request bounds.
DEFAULT_MAX_DOCUMENTS = 200
DEFAULT_MAX_QUERY_CHARS = 4000
DEFAULT_MAX_DOC_CHARS = 8000
DEFAULT_BATCH_SIZE = 32
# Hard ceiling on a single request body (bytes). Text only (no images), so a few
# MiB is generous while rejecting abusive multi-hundred-MB bodies.
DEFAULT_MAX_BODY_BYTES = 8 * 1024 * 1024
# Cross-encoder input truncation (tokens); bge-reranker-v2-m3 handles long pairs.
DEFAULT_MAX_LENGTH = 512


class ConfigError(Exception):
    """Raised when configuration is missing or malformed."""


@dataclass(frozen=True)
class Config:
    """Runtime configuration resolved from the environment."""

    model: str = DEFAULT_MODEL
    device: str = DEFAULT_DEVICE
    precision: str = DEFAULT_PRECISION
    host: str = "0.0.0.0"  # noqa: S104 — container-internal listener (ADR-009 trust boundary)
    port: int = DEFAULT_PORT
    max_documents: int = DEFAULT_MAX_DOCUMENTS
    max_query_chars: int = DEFAULT_MAX_QUERY_CHARS
    max_doc_chars: int = DEFAULT_MAX_DOC_CHARS
    batch_size: int = DEFAULT_BATCH_SIZE
    max_length: int = DEFAULT_MAX_LENGTH
    max_body_bytes: int = DEFAULT_MAX_BODY_BYTES

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Config:
        """Build a Config from env vars, failing fast on bad values."""
        if env is None:
            env = os.environ

        model = env.get("RERANKER_MODEL", DEFAULT_MODEL).strip() or DEFAULT_MODEL
        device = _choice(env, "RERANKER_DEVICE", DEFAULT_DEVICE, ALLOWED_DEVICES)
        precision = _choice(env, "RERANKER_PRECISION", DEFAULT_PRECISION, ALLOWED_PRECISIONS)
        host, port = _addr_from_env(env)

        return cls(
            model=model,
            device=device,
            precision=precision,
            host=host,
            port=port,
            max_documents=_positive_int(env, "RERANKER_MAX_DOCUMENTS", DEFAULT_MAX_DOCUMENTS),
            max_query_chars=_positive_int(env, "RERANKER_MAX_QUERY_CHARS", DEFAULT_MAX_QUERY_CHARS),
            max_doc_chars=_positive_int(env, "RERANKER_MAX_DOC_CHARS", DEFAULT_MAX_DOC_CHARS),
            batch_size=_positive_int(env, "RERANKER_BATCH_SIZE", DEFAULT_BATCH_SIZE),
            max_length=_positive_int(env, "RERANKER_MAX_LENGTH", DEFAULT_MAX_LENGTH),
            max_body_bytes=_positive_int(env, "RERANKER_MAX_BODY_BYTES", DEFAULT_MAX_BODY_BYTES),
        )


def _choice(env: Mapping[str, str], key: str, default: str, allowed: tuple[str, ...]) -> str:
    """Resolve a lower-cased enum env var, failing fast on an unknown value."""
    raw = env.get(key)
    if raw is None or not raw.strip():
        return default
    value = raw.strip().lower()
    if value not in allowed:
        raise ConfigError(f"{key} must be one of {allowed}, got {raw!r}")
    return value


def _positive_int(env: Mapping[str, str], key: str, default: int) -> int:
    raw = env.get(key)
    if raw is None or not raw.strip():
        return default
    try:
        value = int(raw.strip())
    except ValueError as exc:
        raise ConfigError(f"{key} must be an integer, got {raw!r}") from exc
    if value <= 0:
        raise ConfigError(f"{key} must be positive, got {value}")
    return value


def _addr_from_env(env: Mapping[str, str]) -> tuple[str, int]:
    """Resolve the listen (host, port) from RERANKER_PORT.

    Accepts ":9900" or "host:9900" forms, mirroring the clip service.
    """
    addr = env.get("RERANKER_PORT", f":{DEFAULT_PORT}").strip()
    host, sep, port_s = addr.rpartition(":")
    if not sep:
        port_s, host = addr, ""
    try:
        port = int(port_s)
    except ValueError as exc:
        raise ConfigError(
            f"RERANKER_PORT must look like ':9900' or 'host:9900', got {addr!r}"
        ) from exc
    if port <= 0:
        raise ConfigError(f"RERANKER_PORT must be a positive port, got {port}")
    return (host or "0.0.0.0", port)  # noqa: S104 — container-internal listener
