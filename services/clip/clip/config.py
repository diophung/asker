"""Environment configuration for the CLIP model service.

CLIP_DIM is deploy-time configuration (ADR-013), exactly like EMBEDDING_DIM for
TEI (ADR-005): it must match the model the service actually serves, and a wrong
value would feed wrong-size vectors to Vespa, so every embedding is validated
against it. Dev/CI default ViT-B-32 / openai => 512-d.
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

# Open-clip model identity. ViT-B-32 / openai is the small dev/CI model
# (ADR-013): a 512-d shared text+image space that fits a constrained dev VM.
DEFAULT_MODEL = "ViT-B-32"
DEFAULT_PRETRAINED = "openai"
DEFAULT_DIM = 512
DEFAULT_PORT = 9800

# Request bounds: cap how much work one call can ask for. The image arm is the
# expensive one (decode + preprocess + forward pass), so it gets a tighter cap.
DEFAULT_MAX_TEXTS = 64
DEFAULT_MAX_IMAGES = 32
# Hard ceiling on a single request body (bytes). base64 images are large; 32
# images at ~a few hundred KB each plus JSON overhead -> 32 MiB is generous
# while still rejecting accidental/abusive multi-hundred-MB bodies.
DEFAULT_MAX_BODY_BYTES = 32 * 1024 * 1024


class ConfigError(Exception):
    """Raised when configuration is missing or malformed."""


@dataclass(frozen=True)
class Config:
    """Runtime configuration resolved from the environment."""

    model: str = DEFAULT_MODEL
    pretrained: str = DEFAULT_PRETRAINED
    clip_dim: int = DEFAULT_DIM
    host: str = "0.0.0.0"  # noqa: S104 — container-internal listener (ADR-009 trust boundary)
    port: int = DEFAULT_PORT
    max_texts: int = DEFAULT_MAX_TEXTS
    max_images: int = DEFAULT_MAX_IMAGES
    max_body_bytes: int = DEFAULT_MAX_BODY_BYTES

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Config:
        """Build a Config from env vars, failing fast on bad values.

        CLIP_DIM defaults to 512 (ViT-B-32/openai), but if set it must be a
        positive integer; it has to match the loaded model's output dimension
        or embeddings are rejected at validation time (ADR-013).
        """
        if env is None:
            env = os.environ

        model = env.get("CLIP_MODEL", DEFAULT_MODEL).strip() or DEFAULT_MODEL
        pretrained = env.get("CLIP_PRETRAINED", DEFAULT_PRETRAINED).strip() or DEFAULT_PRETRAINED
        clip_dim = _positive_int(env, "CLIP_DIM", DEFAULT_DIM)
        host, port = _addr_from_env(env)

        return cls(
            model=model,
            pretrained=pretrained,
            clip_dim=clip_dim,
            host=host,
            port=port,
            max_texts=_positive_int(env, "CLIP_MAX_TEXTS", DEFAULT_MAX_TEXTS),
            max_images=_positive_int(env, "CLIP_MAX_IMAGES", DEFAULT_MAX_IMAGES),
            max_body_bytes=_positive_int(env, "CLIP_MAX_BODY_BYTES", DEFAULT_MAX_BODY_BYTES),
        )


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
    """Resolve the listen (host, port) from CLIP_PORT.

    Accepts ":9800" or "host:9800" forms, mirroring the enrich health addr.
    """
    addr = env.get("CLIP_PORT", f":{DEFAULT_PORT}").strip()
    host, sep, port_s = addr.rpartition(":")
    if not sep:
        # A bare value with no colon is treated as a port number.
        port_s, host = addr, ""
    try:
        port = int(port_s)
    except ValueError as exc:
        raise ConfigError(f"CLIP_PORT must look like ':9800' or 'host:9800', got {addr!r}") from exc
    if port <= 0:
        raise ConfigError(f"CLIP_PORT must be a positive port, got {port}")
    return (host or "0.0.0.0", port)  # noqa: S104 — container-internal listener
