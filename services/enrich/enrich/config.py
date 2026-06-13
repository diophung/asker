"""Environment configuration and pipeline constants for the enrich worker.

Topic and header names mirror the exported constants in platform/kafkautil
(ADR-004); the two implementations must never drift.
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

# Pipeline topic names (ADR-004). Every stage consumes and produces the
# canonical asker.v1.Document; there are no stage-private message types.
TOPIC_DOCS_RAW = "docs.raw"
TOPIC_DOCS_CHUNKED = "docs.chunked"
TOPIC_DOCS_ENRICHED = "docs.enriched"
TOPIC_DOCS_DEADLETTER = "docs.deadletter"

# Record header keys. tenant_id, doc_id and version_etag travel on every
# document record; error and origin_topic are added on dead-letter quarantine.
HEADER_TENANT_ID = "tenant_id"
HEADER_DOC_ID = "doc_id"
HEADER_VERSION_ETAG = "version_etag"
HEADER_ERROR = "error"
HEADER_ORIGIN_TOPIC = "origin_topic"

CONSUMER_GROUP = "enrich"

# Dev partition count for EnsureTopics parity (ADR-004: 4 in dev).
DEFAULT_PARTITIONS = 4

DEFAULT_HEALTH_ADDR = ":9601"


class ConfigError(Exception):
    """Raised when required configuration is missing or malformed."""


def health_addr_from_env(env: Mapping[str, str] | None = None) -> tuple[str, int]:
    """Return the (host, port) the health server listens on.

    Reads ENRICH_HEALTH_ADDR (":9601" by default). Accepts ":port" or
    "host:port" forms; this never needs EMBEDDING_DIM so the -healthcheck
    self-probe works even on a misconfigured container.
    """
    if env is None:
        env = os.environ
    addr = env.get("ENRICH_HEALTH_ADDR", DEFAULT_HEALTH_ADDR).strip()
    host, sep, port_s = addr.rpartition(":")
    if not sep:
        raise ConfigError(f"ENRICH_HEALTH_ADDR must look like ':9601' or 'host:9601', got {addr!r}")
    try:
        port = int(port_s)
    except ValueError as exc:
        raise ConfigError(f"ENRICH_HEALTH_ADDR has a non-integer port: {addr!r}") from exc
    return (host or "0.0.0.0", port)  # noqa: S104 — container-internal health listener


def _require_positive_dim(env: Mapping[str, str], name: str, adr: str) -> int:
    """Parse a required, positive integer dimension env var (ADR-005 pattern).

    Like EMBEDDING_DIM, a vector dimension must match the model a service is
    actually serving; a wrong guess feeds wrong-size vectors to Vespa, which
    is strictly worse than refusing to start, so there is no default.
    """
    raw = env.get(name)
    if raw is None or not raw.strip():
        raise ConfigError(
            f"{name} is required and has no default; "
            f"set it to the dimension of the model it serves ({adr})"
        )
    try:
        value = int(raw.strip())
    except ValueError as exc:
        raise ConfigError(f"{name} must be an integer, got {raw!r}") from exc
    if value <= 0:
        raise ConfigError(f"{name} must be positive, got {value}")
    return value


@dataclass(frozen=True)
class Config:
    """Runtime configuration resolved from the environment."""

    kafka_brokers: list[str]
    tei_url: str
    embedding_dim: int
    # Media pipeline (M3, ADR-013). CLIP lives in a separate model service and a
    # separate embedding space; media bytes reach this Python worker through the
    # connector-hub's internal decrypt/encrypt endpoint (the envelope crypto
    # stays in Go).
    clip_url: str
    clip_dim: int
    hub_media_url: str
    whisper_model: str = "tiny"
    whisper_compute_type: str = "int8"
    # Cap scene-detection keyframes per video so a long/busy clip cannot fan out
    # into thousands of CLIP calls + blob writes (ADR-013).
    max_keyframes: int = 20
    # Longest edge of a generated thumbnail / poster frame, in pixels.
    thumbnail_max_px: int = 512
    # Wall-clock bound (seconds) on each ffmpeg/ffprobe invocation so a malformed
    # or adversarial video cannot hang the worker indefinitely (ADR-013).
    ffmpeg_timeout_s: int = 120
    health_host: str = "0.0.0.0"  # noqa: S104 — container-internal health listener
    health_port: int = 9601
    max_poll_records: int = 32

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Config:
        """Build a Config from env vars, failing fast on bad values.

        EMBEDDING_DIM and CLIP_DIM are REQUIRED with no default (ADR-005/013):
        each must match the model its service is actually serving, and a wrong
        guess would feed wrong-size vectors, so startup refuses to proceed
        without them.
        """
        if env is None:
            env = os.environ

        brokers = [
            b.strip() for b in env.get("KAFKA_BROKERS", "redpanda:9092").split(",") if b.strip()
        ]
        if not brokers:
            raise ConfigError("KAFKA_BROKERS must list at least one broker")

        dim = _require_positive_dim(env, "EMBEDDING_DIM", "ADR-005")
        clip_dim = _require_positive_dim(env, "CLIP_DIM", "ADR-013")

        tei_url = env.get("TEI_URL", "http://tei:80").strip().rstrip("/")
        if not tei_url:
            raise ConfigError("TEI_URL must not be empty")

        clip_url = env.get("CLIP_URL", "http://clip:9800").strip().rstrip("/")
        if not clip_url:
            raise ConfigError("CLIP_URL must not be empty")

        hub_media_url = env.get("HUB_MEDIA_URL", "http://connector-hub:9300").strip().rstrip("/")
        if not hub_media_url:
            raise ConfigError("HUB_MEDIA_URL must not be empty")

        whisper_model = env.get("WHISPER_MODEL", "tiny").strip() or "tiny"
        whisper_compute_type = env.get("WHISPER_COMPUTE_TYPE", "int8").strip() or "int8"

        max_keyframes = cls._optional_positive_int(env, "MAX_KEYFRAMES", 20)
        thumbnail_max_px = cls._optional_positive_int(env, "THUMBNAIL_MAX_PX", 512)
        ffmpeg_timeout_s = cls._optional_positive_int(env, "FFMPEG_TIMEOUT_S", 120)

        host, port = health_addr_from_env(env)
        return cls(
            kafka_brokers=brokers,
            tei_url=tei_url,
            embedding_dim=dim,
            clip_url=clip_url,
            clip_dim=clip_dim,
            hub_media_url=hub_media_url,
            whisper_model=whisper_model,
            whisper_compute_type=whisper_compute_type,
            max_keyframes=max_keyframes,
            thumbnail_max_px=thumbnail_max_px,
            ffmpeg_timeout_s=ffmpeg_timeout_s,
            health_host=host,
            health_port=port,
        )

    @staticmethod
    def _optional_positive_int(env: Mapping[str, str], name: str, default: int) -> int:
        """Parse an optional positive-int knob, defaulting when unset/blank."""
        raw = env.get(name)
        if raw is None or not raw.strip():
            return default
        try:
            value = int(raw.strip())
        except ValueError as exc:
            raise ConfigError(f"{name} must be an integer, got {raw!r}") from exc
        if value <= 0:
            raise ConfigError(f"{name} must be positive, got {value}")
        return value
