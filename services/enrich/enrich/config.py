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


@dataclass(frozen=True)
class Config:
    """Runtime configuration resolved from the environment."""

    kafka_brokers: list[str]
    tei_url: str
    embedding_dim: int
    health_host: str = "0.0.0.0"  # noqa: S104 — container-internal health listener
    health_port: int = 9601
    max_poll_records: int = 32

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Config:
        """Build a Config from env vars, failing fast on bad values.

        EMBEDDING_DIM is REQUIRED with no default (ADR-005): it must match
        the model TEI is actually serving, and a wrong guess would feed
        wrong-size vectors, so startup refuses to proceed without it.
        """
        if env is None:
            env = os.environ

        brokers = [
            b.strip() for b in env.get("KAFKA_BROKERS", "redpanda:9092").split(",") if b.strip()
        ]
        if not brokers:
            raise ConfigError("KAFKA_BROKERS must list at least one broker")

        raw_dim = env.get("EMBEDDING_DIM")
        if raw_dim is None or not raw_dim.strip():
            raise ConfigError(
                "EMBEDDING_DIM is required and has no default; "
                "set it to the dimension of the model TEI serves (ADR-005)"
            )
        try:
            dim = int(raw_dim.strip())
        except ValueError as exc:
            raise ConfigError(f"EMBEDDING_DIM must be an integer, got {raw_dim!r}") from exc
        if dim <= 0:
            raise ConfigError(f"EMBEDDING_DIM must be positive, got {dim}")

        tei_url = env.get("TEI_URL", "http://tei:80").strip().rstrip("/")
        if not tei_url:
            raise ConfigError("TEI_URL must not be empty")

        host, port = health_addr_from_env(env)
        return cls(
            kafka_brokers=brokers,
            tei_url=tei_url,
            embedding_dim=dim,
            health_host=host,
            health_port=port,
        )
