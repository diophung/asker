"""Bounded configuration for a host-native encoder; no provider credentials."""
from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass


def positive(env: Mapping[str, str], name: str, default: int, maximum: int, minimum: int = 1) -> int:
    try:
        value = int(env.get(name, str(default)))
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
    if not minimum <= value <= maximum:
        raise ValueError(f"{name} must be between {minimum} and {maximum}")
    return value


@dataclass(frozen=True)
class Config:
    model: str = "BAAI/bge-m3"
    revision: str | None = None
    dimension: int = 1024
    device: str = "auto"
    host: str = "127.0.0.1"
    port: int = 18083
    batch_size: int = 4
    max_length: int = 8192
    max_inputs: int = 32
    max_chars: int = 16384
    max_total_chars: int = 65536
    max_body_bytes: int = 1048576
    max_connections: int = 16
    cpu_threads: int = 4
    admission_wait_ms: int = 600
    max_waiting: int = 3
    body_timeout_ms: int = 500
    socket_timeout_seconds: int = 5

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Config:
        env = os.environ if env is None else env
        device = env.get("LOCAL_EMBED_DEVICE", "auto").strip().lower()
        if device not in ("auto", "cpu", "mps"):
            raise ValueError("LOCAL_EMBED_DEVICE must be auto, cpu or mps")
        model = env.get("LOCAL_EMBED_MODEL", "BAAI/bge-m3").strip()
        if not model:
            raise ValueError("LOCAL_EMBED_MODEL must not be empty")
        return cls(
            model=model,
            revision=env.get("LOCAL_EMBED_REVISION", "").strip() or None,
            dimension=positive(env, "EMBEDDING_DIM", 1024, 8192),
            device=device,
            host=env.get("LOCAL_EMBED_HOST", "127.0.0.1"),
            port=positive(env, "LOCAL_EMBED_PORT", 18083, 65535),
            batch_size=positive(env, "LOCAL_EMBED_BATCH_SIZE", 4, 32),
            max_length=positive(env, "LOCAL_EMBED_MAX_LENGTH", 8192, 8192),
            max_inputs=positive(env, "LOCAL_EMBED_MAX_INPUTS", 32, 256),
            max_chars=positive(env, "LOCAL_EMBED_MAX_CHARS", 16384, 65536),
            max_total_chars=positive(env, "LOCAL_EMBED_MAX_TOTAL_CHARS", 65536, 1048576),
            max_body_bytes=positive(env, "LOCAL_EMBED_MAX_BODY_BYTES", 1048576, 8388608),
            max_connections=positive(env, "LOCAL_EMBED_MAX_CONNECTIONS", 16, 64),
            cpu_threads=positive(env, "LOCAL_EMBED_CPU_THREADS", 4, 16),
            admission_wait_ms=positive(env, "LOCAL_EMBED_ADMISSION_WAIT_MS", 600, 1000, 0),
            max_waiting=positive(env, "LOCAL_EMBED_MAX_QUEUE", 3, 16, 0),
            body_timeout_ms=positive(env, "LOCAL_EMBED_BODY_TIMEOUT_MS", 500, 1000),
            socket_timeout_seconds=positive(env, "LOCAL_EMBED_SOCKET_TIMEOUT_SECONDS", 5, 30),
        )
