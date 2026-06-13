"""aiokafka worker implementing the ADR-004 pipeline contract for enrich.

Parity with platform/kafkautil is by test, not by construction (ADR-007):
per record we re-validate the tenant_id header against the payload, retry
handler failures 3 times with capped exponential backoff, quarantine to
docs.deadletter with error/origin_topic headers, and commit only AFTER a
successful produce-or-quarantine — at-least-once, never skipped silently.
"""

from __future__ import annotations

import asyncio
import enum
import logging
import re
from collections.abc import Awaitable, Callable, Iterable, Sequence
from typing import Protocol

from aiokafka.structs import TopicPartition
from google.protobuf.message import DecodeError

from asker.v1 import document_pb2

from . import config as cfg

log = logging.getLogger("enrich.worker")

# Mirrors platform/kafkautil's retry policy: maxHandlerAttempts total
# attempts with capped exponential backoff between them.
MAX_HANDLER_ATTEMPTS = 3
HANDLER_BACKOFF = (0.1, 0.5, 2.0)

# Mirrors platform/tenancy's tenant syntax allowlist (ADR-002): short,
# separator-free ASCII, never "." or "..".
_TENANT_PATTERN = re.compile(r"^[A-Za-z0-9._-]{1,128}$")

Headers = list[tuple[str, bytes]]


def valid_tenant(value: str) -> bool:
    """Report whether value is acceptable as a tenant identifier."""
    if value in (".", ".."):
        return False
    return bool(_TENANT_PATTERN.match(value))


def tenant_from_headers(headers: Iterable[tuple[str, bytes]] | None) -> str | None:
    """Extract and re-validate the tenant_id record header.

    Returns None when the header is missing, undecodable, or fails the
    tenant syntax allowlist — a forged or corrupted header never yields a
    usable tenant.
    """
    for key, value in headers or ():
        if key == cfg.HEADER_TENANT_ID:
            try:
                tenant = value.decode("utf-8")
            except UnicodeDecodeError:
                return None
            return tenant if valid_tenant(tenant) else None
    return None


class Outcome(enum.Enum):
    """What happened to one consumed record."""

    ENRICHED = "enriched"
    PASSED_THROUGH = "passed_through"
    DEADLETTERED = "deadlettered"


class Record(Protocol):
    """The subset of aiokafka.ConsumerRecord the worker touches."""

    topic: str
    partition: int
    offset: int
    key: bytes | None
    value: bytes | None
    headers: Sequence[tuple[str, bytes]]


class ConsumerLike(Protocol):
    async def getmany(
        self, *, timeout_ms: int = ..., max_records: int | None = ...
    ) -> dict[TopicPartition, list[Record]]: ...

    async def commit(self, offsets: dict[TopicPartition, int]) -> None: ...


class ProducerLike(Protocol):
    async def send_and_wait(
        self,
        topic: str,
        value: bytes | None = ...,
        key: bytes | None = ...,
        headers: Headers | None = ...,
    ) -> object: ...


class EmbedderLike(Protocol):
    async def embed(self, texts: Sequence[str]) -> list[list[float]]: ...


class Worker:
    """Consumes docs.chunked, embeds chunk texts via TEI, produces docs.enriched.

    The Kafka consumer/producer and the embedder are injected so tests run
    against fakes — no live Kafka or TEI required (ADR-007).
    """

    def __init__(
        self,
        consumer: ConsumerLike,
        producer: ProducerLike,
        embedder: EmbedderLike,
        *,
        backoff: Sequence[float] = HANDLER_BACKOFF,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
        poll_timeout_ms: int = 1000,
        max_poll_records: int = 32,
    ) -> None:
        self._consumer = consumer
        self._producer = producer
        self._embedder = embedder
        self._backoff = tuple(backoff) or HANDLER_BACKOFF
        self._sleep = sleep
        self._poll_timeout_ms = poll_timeout_ms
        self._max_poll_records = max_poll_records
        self._stop = asyncio.Event()

    def request_stop(self) -> None:
        """Ask the run loop to exit after the in-flight record (signal-safe)."""
        self._stop.set()

    async def run(self) -> None:
        """Poll and process records until request_stop is called.

        Records are processed and committed strictly in order within each
        partition, so a commit never advances past an unhandled record. A
        record in flight when stop is requested finishes; later uncommitted
        records are simply redelivered (at-least-once).
        """
        while not self._stop.is_set():
            batches = await self._consumer.getmany(
                timeout_ms=self._poll_timeout_ms, max_records=self._max_poll_records
            )
            for records in batches.values():
                for record in records:
                    if self._stop.is_set():
                        return
                    await self.process(record)

    async def process(self, record: Record) -> Outcome:
        """Apply the full per-record ADR-004 contract to one record."""
        headers: Headers = list(record.headers or ())
        value = record.value or b""

        # Malformed records (invalid tenant header, undecodable proto,
        # header/payload tenant mismatch) skip retries and go straight to
        # the dead letter — kafkautil parity.
        tenant = tenant_from_headers(headers)
        if tenant is None:
            return await self._quarantine(record, headers, "missing or invalid tenant_id header")

        doc = document_pb2.Document()
        try:
            doc.ParseFromString(value)
        except DecodeError as exc:
            return await self._quarantine(record, headers, f"unmarshal document: {exc}")

        # Defense in depth: the header (which routed the record) and the
        # payload must agree, or the document would be processed under the
        # wrong tenant.
        if doc.tenant_id != tenant:
            return await self._quarantine(
                record,
                headers,
                f"tenant header {tenant!r} does not match document tenant {doc.tenant_id!r}",
            )

        # Tombstones and zero-chunk documents pass through to docs.enriched
        # byte-for-byte unchanged; everything else gets embeddings filled in.
        # Both paths produce-then-commit through the SAME retry / dead-letter
        # loop (kafkautil parity): a transient broker error must not crash the
        # worker — least of all on a delete tombstone, the freshness-sensitive
        # path. The passthrough "build" is a no-op returning the original bytes;
        # the enriched "build" calls TEI (so an embedding failure also retries).
        passthrough = doc.tombstone.deleted or not doc.chunks

        async def build() -> bytes:
            return value if passthrough else await self._enrich(doc)

        last_err: Exception | None = None
        for attempt in range(1, MAX_HANDLER_ATTEMPTS + 1):
            if attempt > 1:
                await self._sleep(self._backoff[min(attempt - 2, len(self._backoff) - 1)])
            try:
                out = await build()
                await self._producer.send_and_wait(
                    cfg.TOPIC_DOCS_ENRICHED, value=out, key=record.key, headers=headers
                )
                await self._commit(record)
                log.info(
                    "passed through unchanged" if passthrough else "document enriched",
                    extra={
                        "doc_id": doc.doc_id,
                        "tenant_id": doc.tenant_id,
                        "tombstone": doc.tombstone.deleted,
                        "chunks": len(doc.chunks),
                        "attempt": attempt,
                    },
                )
                return Outcome.PASSED_THROUGH if passthrough else Outcome.ENRICHED
            except Exception as exc:  # any handler error retries, then dead-letters
                last_err = exc
                log.warning(
                    "handler failed",
                    extra={
                        "topic": record.topic,
                        "partition": record.partition,
                        "offset": record.offset,
                        "doc_id": doc.doc_id,
                        "attempt": attempt,
                        "max_attempts": MAX_HANDLER_ATTEMPTS,
                        "error": str(exc),
                    },
                )
        return await self._quarantine(
            record, headers, f"handler failed after {MAX_HANDLER_ATTEMPTS} attempts: {last_err}"
        )

    async def _enrich(self, doc: document_pb2.Document) -> bytes:
        """Fill every chunk's embedding from TEI and serialize the document."""
        texts = [chunk.text for chunk in doc.chunks]
        vectors = await self._embedder.embed(texts)
        if len(vectors) != len(texts):
            raise RuntimeError(f"embedder returned {len(vectors)} vectors for {len(texts)} chunks")
        for chunk, vector in zip(doc.chunks, vectors, strict=True):
            del chunk.embedding[:]  # idempotent across in-process retries
            chunk.embedding.extend(vector)
        return doc.SerializeToString()

    async def _quarantine(self, record: Record, headers: Headers, reason: str) -> Outcome:
        """Produce record to docs.deadletter (original key/value/headers plus
        error and origin_topic headers), then commit the original record.

        If the dead-letter produce fails, the exception propagates with
        nothing committed, so the record is redelivered rather than silently
        skipped — kafkautil parity.
        """
        if record.topic == cfg.TOPIC_DOCS_DEADLETTER:
            # Never re-quarantine the dead-letter topic into itself.
            log.error(
                "dead-letter record failed processing; committing without re-quarantine",
                extra={"partition": record.partition, "offset": record.offset, "error": reason},
            )
            await self._commit(record)
            return Outcome.DEADLETTERED

        dl_headers: Headers = [
            *headers,
            (cfg.HEADER_ERROR, reason.encode("utf-8")),
            (cfg.HEADER_ORIGIN_TOPIC, record.topic.encode("utf-8")),
        ]
        await self._producer.send_and_wait(
            cfg.TOPIC_DOCS_DEADLETTER,
            value=record.value,
            key=record.key,
            headers=dl_headers,
        )
        log.error(
            "record quarantined to dead-letter topic",
            extra={
                "origin_topic": record.topic,
                "partition": record.partition,
                "offset": record.offset,
                "error": reason,
            },
        )
        await self._commit(record)
        return Outcome.DEADLETTERED

    async def _commit(self, record: Record) -> None:
        """Commit record's offset; a failed commit is logged, not fatal.

        At-least-once means the record may be redelivered, which downstream
        idempotent (doc_id, version_etag) writes absorb (ADR-004).
        """
        tp = TopicPartition(record.topic, record.partition)
        try:
            await self._consumer.commit({tp: record.offset + 1})
        except Exception as exc:  # commit failure must not crash the loop
            log.warning(
                "offset commit failed; record may be redelivered",
                extra={
                    "topic": record.topic,
                    "partition": record.partition,
                    "offset": record.offset,
                    "error": str(exc),
                },
            )


async def ensure_topics(
    brokers: Sequence[str],
    partitions: int = cfg.DEFAULT_PARTITIONS,
    topics: Sequence[str] = (
        cfg.TOPIC_DOCS_RAW,
        cfg.TOPIC_DOCS_CHUNKED,
        cfg.TOPIC_DOCS_ENRICHED,
        cfg.TOPIC_DOCS_DEADLETTER,
    ),
) -> None:
    """Idempotently create the pipeline topics (EnsureTopics parity).

    Topics that already exist are left untouched. Unlike the Go kadm client,
    aiokafka's NewTopic rejects replication_factor=-1 ("broker default"), so
    the factor comes from ENRICH_TOPIC_REPLICATION (default 1 — single-broker
    dev; production overrides via env, ADR-004).
    """
    import os

    from aiokafka.admin import AIOKafkaAdminClient, NewTopic
    from aiokafka.errors import TopicAlreadyExistsError, for_code

    replication = int(os.environ.get("ENRICH_TOPIC_REPLICATION", "1"))
    admin = AIOKafkaAdminClient(bootstrap_servers=list(brokers))
    await admin.start()
    try:
        new = [
            NewTopic(name=t, num_partitions=partitions, replication_factor=replication)
            for t in topics
        ]
        try:
            response = await admin.create_topics(new)
        except TopicAlreadyExistsError:
            return
        # aiokafka returns the raw response; surface per-topic errors except
        # "already exists" (error code 36), which is the idempotent case.
        for entry in getattr(response, "topic_errors", []) or []:
            topic, error_code = entry[0], entry[1]
            if error_code == 0:
                continue
            err = for_code(error_code)
            if err is TopicAlreadyExistsError:
                continue
            message = entry[2] if len(entry) > 2 else None
            raise err(f"ensure topic {topic!r}: {message or err.__name__}")
    finally:
        await admin.close()
