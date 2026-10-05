"""Environment configuration for the cross-encoder reranker service.

Mirrors services/clip/clip/config.py. The default model is BAAI/bge-reranker-
v2-m3 (Apache-2.0, 568M). Latency depends on hardware, candidate lengths and
batching and must be measured. MPS currently uses fp32. RERANKER_DEVICE adds "mps"
(Apple GPU) to the cpu/cuda choices the clip service has, since the reranker is
meant to run natively on the M5 Max (MPS) as well as the RTX host (cuda).
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

# Model identity. Swapping encoders requires relevance/latency checks; fitting
# into memory does not establish a latency guarantee.
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
    revision: str | None = None
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
    max_connections: int = 16
    socket_timeout_seconds: int = 5
    admission_wait_ms: int = 300
    max_waiting: int = 3
    body_timeout_ms: int = 500
    max_total_chars: int = 262144

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
            revision=env.get("RERANKER_REVISION", "").strip() or None,
            device=device,
            precision=precision,
            host=host,
            port=port,
            max_documents=_positive_int(
                env, "RERANKER_MAX_DOCUMENTS", DEFAULT_MAX_DOCUMENTS, maximum=1000
            ),
            max_query_chars=_positive_int(
                env, "RERANKER_MAX_QUERY_CHARS", DEFAULT_MAX_QUERY_CHARS, maximum=16384
            ),
            max_doc_chars=_positive_int(
                env, "RERANKER_MAX_DOC_CHARS", DEFAULT_MAX_DOC_CHARS, maximum=65536
            ),
            batch_size=_positive_int(env, "RERANKER_BATCH_SIZE", DEFAULT_BATCH_SIZE, maximum=256),
            max_length=_positive_int(env, "RERANKER_MAX_LENGTH", DEFAULT_MAX_LENGTH, maximum=8192),
            max_body_bytes=_positive_int(
                env, "RERANKER_MAX_BODY_BYTES", DEFAULT_MAX_BODY_BYTES, maximum=16 * 1024 * 1024
            ),
            max_connections=_positive_int(env, "RERANKER_MAX_CONNECTIONS", 16, maximum=64),
            socket_timeout_seconds=_positive_int(
                env, "RERANKER_SOCKET_TIMEOUT_SECONDS", 5, maximum=30
            ),
            admission_wait_ms=_positive_int(
                env, "RERANKER_ADMISSION_WAIT_MS", 300, maximum=3000, minimum=0
            ),
            max_waiting=_positive_int(env, "RERANKER_MAX_QUEUE", 3, maximum=16, minimum=0),
            body_timeout_ms=_positive_int(env, "RERANKER_BODY_TIMEOUT_MS", 500, maximum=1000),
            max_total_chars=_positive_int(env, "RERANKER_MAX_TOTAL_CHARS", 262144, maximum=1048576),
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


def _positive_int(
    env: Mapping[str, str], key: str, default: int, *, maximum: int | None = None, minimum: int = 1
) -> int:
    raw = env.get(key)
    if raw is None or not raw.strip():
        return default
    try:
        value = int(raw.strip())
    except ValueError as exc:
        raise ConfigError(f"{key} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise ConfigError(f"{key} must be at least {minimum}, got {value}")
    if maximum is not None and value > maximum:
        raise ConfigError(f"{key} must be at most {maximum}, got {value}")
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
    if not 1 <= port <= 65535:
        raise ConfigError(f"RERANKER_PORT must be between 1 and 65535, got {port}")
    return (host or "0.0.0.0", port)  # noqa: S104 — container-internal listener
