"""Worker contract tests (ADR-004 parity): pass-through, tenant checks,
retry/dead-letter flow, commit-after-produce — all against injected fakes."""

from dataclasses import dataclass, field

import pytest
from aiokafka.structs import TopicPartition

from asker.v1 import document_pb2
from enrich import config as cfg
from enrich.kafka_worker import Outcome, Worker, tenant_from_headers, valid_tenant

DIM = 4
TENANT = "tenant-a"


# --- fakes -----------------------------------------------------------------


@dataclass
class FakeRecord:
    topic: str = cfg.TOPIC_DOCS_CHUNKED
    partition: int = 0
    offset: int = 7
    key: bytes | None = TENANT.encode()
    value: bytes | None = b""
    headers: list[tuple[str, bytes]] = field(default_factory=list)


class FakeConsumer:
    def __init__(self, batches=None):
        self.commits: list[dict] = []
        self._batches = list(batches or [])
        self.events: list[str] | None = None

    async def getmany(self, *, timeout_ms=0, max_records=None):
        if self._batches:
            return self._batches.pop(0)
        return {}

    async def commit(self, offsets):
        if self.events is not None:
            self.events.append("commit")
        self.commits.append(offsets)


@dataclass
class Produced:
    topic: str
    value: bytes | None
    key: bytes | None
    headers: list[tuple[str, bytes]]


class FakeProducer:
    def __init__(self, fail_topics=()):
        self.produced: list[Produced] = []
        self.fail_topics = set(fail_topics)
        self.events: list[str] | None = None

    async def send_and_wait(self, topic, value=None, key=None, headers=None):
        if topic in self.fail_topics:
            raise RuntimeError(f"produce to {topic} failed")
        if self.events is not None:
            self.events.append(f"produce:{topic}")
        self.produced.append(Produced(topic, value, key, list(headers or [])))


class FakeEmbedder:
    def __init__(self, dim=DIM, fail_times=0):
        self.dim = dim
        self.fail_times = fail_times
        self.calls = 0

    async def embed(self, texts):
        self.calls += 1
        if self.calls <= self.fail_times:
            raise RuntimeError("tei exploded")
        return [[float(i)] * self.dim for i, _ in enumerate(texts)]


async def _no_sleep(_delay: float) -> None:
    return None


def make_worker(consumer, producer, embedder=None):
    return Worker(
        consumer,
        producer,
        embedder or FakeEmbedder(),
        backoff=(0, 0, 0),
        sleep=_no_sleep,
    )


# --- helpers ----------------------------------------------------------------


def make_doc(tenant=TENANT, chunks=2, tombstone=False) -> document_pb2.Document:
    doc = document_pb2.Document(
        tenant_id=tenant,
        doc_id="doc-1",
        connector_id="gmail",
        type=document_pb2.EMAIL,
        version_etag="etag-1",
    )
    for i in range(chunks):
        doc.chunks.add(chunk_id=f"doc-1#{i}", text=f"chunk text {i}", char_start=i, char_end=i + 1)
    if tombstone:
        doc.tombstone.deleted = True
    return doc


def make_record(doc: document_pb2.Document, tenant_header: str | bytes | None = TENANT):
    headers: list[tuple[str, bytes]] = []
    if tenant_header is not None:
        raw = tenant_header if isinstance(tenant_header, bytes) else tenant_header.encode()
        headers.append((cfg.HEADER_TENANT_ID, raw))
    headers.append((cfg.HEADER_DOC_ID, doc.doc_id.encode()))
    headers.append((cfg.HEADER_VERSION_ETAG, doc.version_etag.encode()))
    return FakeRecord(key=doc.tenant_id.encode(), value=doc.SerializeToString(), headers=headers)


def header(headers, key):
    return dict(headers)[key]


# --- tenant validation -------------------------------------------------------


def test_valid_tenant_allowlist():
    assert valid_tenant("tenant-a")
    assert valid_tenant("a" * 128)
    assert not valid_tenant("a" * 129)
    assert not valid_tenant("")
    assert not valid_tenant(".")
    assert not valid_tenant("..")
    assert not valid_tenant("ten/ant")
    assert not valid_tenant("tenant a")
    assert not valid_tenant("tenänt")


def test_tenant_from_headers():
    assert tenant_from_headers([(cfg.HEADER_TENANT_ID, b"t1")]) == "t1"
    assert tenant_from_headers([]) is None
    assert tenant_from_headers(None) is None
    assert tenant_from_headers([(cfg.HEADER_TENANT_ID, b"bad tenant")]) is None
    assert tenant_from_headers([(cfg.HEADER_TENANT_ID, b"\xff\xfe")]) is None


# --- pass-through ------------------------------------------------------------


async def test_tombstone_passes_through_unchanged():
    consumer, producer = FakeConsumer(), FakeProducer()
    record = make_record(make_doc(chunks=0, tombstone=True))
    outcome = await make_worker(consumer, producer).process(record)

    assert outcome is Outcome.PASSED_THROUGH
    assert len(producer.produced) == 1
    out = producer.produced[0]
    assert out.topic == cfg.TOPIC_DOCS_ENRICHED
    assert out.value == record.value  # byte-for-byte unchanged
    assert out.key == record.key
    assert out.headers == record.headers
    assert consumer.commits == [{TopicPartition(record.topic, record.partition): record.offset + 1}]


async def test_tombstone_with_chunks_still_passes_through():
    consumer, producer = FakeConsumer(), FakeProducer()
    embedder = FakeEmbedder()
    record = make_record(make_doc(chunks=2, tombstone=True))
    outcome = await make_worker(consumer, producer, embedder).process(record)

    assert outcome is Outcome.PASSED_THROUGH
    assert embedder.calls == 0  # never embeds a tombstone
    assert producer.produced[0].value == record.value


async def test_zero_chunk_doc_passes_through_unchanged():
    consumer, producer = FakeConsumer(), FakeProducer()
    record = make_record(make_doc(chunks=0))
    outcome = await make_worker(consumer, producer).process(record)

    assert outcome is Outcome.PASSED_THROUGH
    assert producer.produced[0].topic == cfg.TOPIC_DOCS_ENRICHED
    assert producer.produced[0].value == record.value
    assert len(consumer.commits) == 1


# --- enrichment happy path ----------------------------------------------------


async def test_enriches_chunks_with_same_key_and_headers():
    consumer, producer = FakeConsumer(), FakeProducer()
    record = make_record(make_doc(chunks=3))
    outcome = await make_worker(consumer, producer).process(record)

    assert outcome is Outcome.ENRICHED
    out = producer.produced[0]
    assert out.topic == cfg.TOPIC_DOCS_ENRICHED
    assert out.key == record.key
    assert out.headers == record.headers  # SAME headers, no additions

    enriched = document_pb2.Document()
    enriched.ParseFromString(out.value)
    assert len(enriched.chunks) == 3
    for i, chunk in enumerate(enriched.chunks):
        assert list(chunk.embedding) == pytest.approx([float(i)] * DIM)
        assert chunk.text == f"chunk text {i}"  # rest of the doc untouched
    assert enriched.tenant_id == TENANT
    assert consumer.commits == [{TopicPartition(record.topic, record.partition): record.offset + 1}]


async def test_commit_happens_after_produce():
    consumer, producer = FakeConsumer(), FakeProducer()
    events: list[str] = []
    consumer.events = events
    producer.events = events
    record = make_record(make_doc())
    await make_worker(consumer, producer).process(record)
    assert events == [f"produce:{cfg.TOPIC_DOCS_ENRICHED}", "commit"]


# --- malformed records: straight to dead letter, no retries -------------------


@pytest.mark.parametrize(
    ("tenant_header", "reason_fragment"),
    [
        (None, "tenant_id header"),  # missing
        ("not a tenant!", "tenant_id header"),  # fails allowlist
        (b"\xff\xfe", "tenant_id header"),  # undecodable
        ("tenant-b", "does not match"),  # header/payload mismatch
    ],
)
async def test_bad_tenant_goes_straight_to_deadletter(tenant_header, reason_fragment):
    consumer, producer = FakeConsumer(), FakeProducer()
    embedder = FakeEmbedder()
    record = make_record(make_doc(tenant=TENANT), tenant_header=tenant_header)
    outcome = await make_worker(consumer, producer, embedder).process(record)

    assert outcome is Outcome.DEADLETTERED
    assert embedder.calls == 0  # malformed: no retries, no embedding
    assert len(producer.produced) == 1
    dl = producer.produced[0]
    assert dl.topic == cfg.TOPIC_DOCS_DEADLETTER
    assert dl.value == record.value  # original record preserved
    assert dl.key == record.key
    assert reason_fragment in header(dl.headers, cfg.HEADER_ERROR).decode()
    assert header(dl.headers, cfg.HEADER_ORIGIN_TOPIC) == cfg.TOPIC_DOCS_CHUNKED.encode()
    assert dl.headers[: len(record.headers)] == record.headers  # originals preserved
    assert len(consumer.commits) == 1


async def test_undecodable_proto_goes_straight_to_deadletter():
    consumer, producer = FakeConsumer(), FakeProducer()
    record = FakeRecord(
        value=b"\x05not-a-proto\xff", headers=[(cfg.HEADER_TENANT_ID, TENANT.encode())]
    )
    outcome = await make_worker(consumer, producer).process(record)

    assert outcome is Outcome.DEADLETTERED
    dl = producer.produced[0]
    assert dl.topic == cfg.TOPIC_DOCS_DEADLETTER
    assert "unmarshal" in header(dl.headers, cfg.HEADER_ERROR).decode()
    assert len(consumer.commits) == 1


# --- retry / dead-letter flow ---------------------------------------------------


async def test_transient_failure_retries_then_succeeds():
    consumer, producer = FakeConsumer(), FakeProducer()
    embedder = FakeEmbedder(fail_times=2)  # fails twice, succeeds on attempt 3
    record = make_record(make_doc())
    outcome = await make_worker(consumer, producer, embedder).process(record)

    assert outcome is Outcome.ENRICHED
    assert embedder.calls == 3
    assert producer.produced[0].topic == cfg.TOPIC_DOCS_ENRICHED
    assert len(consumer.commits) == 1


async def test_persistent_failure_deadletters_after_three_attempts():
    consumer, producer = FakeConsumer(), FakeProducer()
    embedder = FakeEmbedder(fail_times=99)
    record = make_record(make_doc())
    outcome = await make_worker(consumer, producer, embedder).process(record)

    assert outcome is Outcome.DEADLETTERED
    assert embedder.calls == 3  # exactly maxHandlerAttempts
    dl = producer.produced[0]
    assert dl.topic == cfg.TOPIC_DOCS_DEADLETTER
    assert dl.value == record.value  # ORIGINAL value, not partially enriched
    error = header(dl.headers, cfg.HEADER_ERROR).decode()
    assert "after 3 attempts" in error
    assert "tei exploded" in error
    assert header(dl.headers, cfg.HEADER_ORIGIN_TOPIC) == cfg.TOPIC_DOCS_CHUNKED.encode()
    assert len(consumer.commits) == 1


async def test_deadletter_produce_failure_propagates_without_commit():
    consumer = FakeConsumer()
    producer = FakeProducer(fail_topics={cfg.TOPIC_DOCS_DEADLETTER})
    record = make_record(make_doc(), tenant_header=None)  # malformed -> quarantine path
    with pytest.raises(RuntimeError):
        await make_worker(consumer, producer).process(record)
    assert consumer.commits == []  # nothing committed: record will be redelivered


async def test_deadletter_topic_never_requarantined():
    consumer, producer = FakeConsumer(), FakeProducer()
    record = FakeRecord(topic=cfg.TOPIC_DOCS_DEADLETTER, value=b"junk", headers=[])
    outcome = await make_worker(consumer, producer).process(record)
    assert outcome is Outcome.DEADLETTERED
    assert producer.produced == []  # committed without producing anywhere
    assert len(consumer.commits) == 1


async def test_commit_failure_is_logged_not_fatal():
    class CommitFailsConsumer(FakeConsumer):
        async def commit(self, offsets):
            raise RuntimeError("rebalance in progress")

    consumer, producer = CommitFailsConsumer(), FakeProducer()
    record = make_record(make_doc())
    outcome = await make_worker(consumer, producer).process(record)
    assert outcome is Outcome.ENRICHED  # at-least-once: redelivery absorbed downstream


# --- run loop -----------------------------------------------------------------


async def test_run_processes_batches_and_stops():
    doc_record = make_record(make_doc())
    tp = TopicPartition(cfg.TOPIC_DOCS_CHUNKED, 0)

    class StoppingConsumer(FakeConsumer):
        def __init__(self, worker_ref):
            super().__init__(batches=[{tp: [doc_record]}])
            self.worker_ref = worker_ref

        async def getmany(self, *, timeout_ms=0, max_records=None):
            batch = await super().getmany(timeout_ms=timeout_ms, max_records=max_records)
            if not batch:
                self.worker_ref[0].request_stop()
            return batch

    producer = FakeProducer()
    worker_ref: list[Worker] = []
    consumer = StoppingConsumer(worker_ref)
    worker = make_worker(consumer, producer)
    worker_ref.append(worker)

    await worker.run()
    assert len(producer.produced) == 1
    assert producer.produced[0].topic == cfg.TOPIC_DOCS_ENRICHED
    assert len(consumer.commits) == 1
