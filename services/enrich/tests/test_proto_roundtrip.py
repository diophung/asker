"""Round-trip through the committed asker.v1 document_pb2 gencode.

Guards against drift between the proto source and the Python bindings the
worker actually ships (ADR-007: parity is by test, not by construction).
"""

from google.protobuf import timestamp_pb2

from asker.v1 import document_pb2


def test_document_roundtrip_preserves_all_worker_touched_fields():
    doc = document_pb2.Document(
        tenant_id="tenant-a",
        doc_id="doc-42",
        connector_id="gmail",
        source_native_id="msg-99",
        type=document_pb2.EMAIL,
        title="Quarterly plan",
        body_text="Hello\n\nWorld",
        version_etag="etag-7",
    )
    doc.metadata["thread_id"] = "t-1"
    doc.participants.add(name="Alice", email="alice@example.com", role="from")
    doc.ts.created.CopyFrom(timestamp_pb2.Timestamp(seconds=1700000000))
    doc.ts.ingested.CopyFrom(timestamp_pb2.Timestamp(seconds=1700000100))
    doc.acl.allowed_principals.append("alice@example.com")
    doc.chunks.add(chunk_id="doc-42#0", text="Hello", char_start=0, char_end=5)
    doc.chunks.add(chunk_id="doc-42#1", text="World", char_start=7, char_end=12)

    data = doc.SerializeToString()
    parsed = document_pb2.Document()
    parsed.ParseFromString(data)

    assert parsed.tenant_id == "tenant-a"
    assert parsed.doc_id == "doc-42"
    assert parsed.type == document_pb2.EMAIL
    assert document_pb2.DocType.Name(parsed.type) == "EMAIL"
    assert parsed.version_etag == "etag-7"
    assert parsed.metadata["thread_id"] == "t-1"
    assert parsed.participants[0].email == "alice@example.com"
    assert parsed.ts.created.seconds == 1700000000
    assert [c.chunk_id for c in parsed.chunks] == ["doc-42#0", "doc-42#1"]
    assert not parsed.tombstone.deleted


def test_embedding_fill_roundtrip():
    doc = document_pb2.Document(tenant_id="t", doc_id="d")
    doc.chunks.add(chunk_id="d#0", text="x")
    doc.chunks[0].embedding.extend([0.25, -1.5, 3.0])

    parsed = document_pb2.Document()
    parsed.ParseFromString(doc.SerializeToString())
    # float32 on the wire: these values are exactly representable.
    assert list(parsed.chunks[0].embedding) == [0.25, -1.5, 3.0]


def test_tombstone_roundtrip():
    doc = document_pb2.Document(tenant_id="t", doc_id="d", version_etag="v")
    doc.tombstone.deleted = True
    doc.tombstone.deleted_at.CopyFrom(timestamp_pb2.Timestamp(seconds=1800000000))

    parsed = document_pb2.Document()
    parsed.ParseFromString(doc.SerializeToString())
    assert parsed.tombstone.deleted
    assert parsed.tombstone.deleted_at.seconds == 1800000000
    assert len(parsed.chunks) == 0
