import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DocType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DOC_TYPE_UNSPECIFIED: _ClassVar[DocType]
    EMAIL: _ClassVar[DocType]
    CHAT_MESSAGE: _ClassVar[DocType]
    FILE: _ClassVar[DocType]
    CALENDAR_EVENT: _ClassVar[DocType]
    WIKI_PAGE: _ClassVar[DocType]
    TICKET: _ClassVar[DocType]
    IMAGE: _ClassVar[DocType]
    VIDEO: _ClassVar[DocType]
    AUDIO: _ClassVar[DocType]
DOC_TYPE_UNSPECIFIED: DocType
EMAIL: DocType
CHAT_MESSAGE: DocType
FILE: DocType
CALENDAR_EVENT: DocType
WIKI_PAGE: DocType
TICKET: DocType
IMAGE: DocType
VIDEO: DocType
AUDIO: DocType

class Document(_message.Message):
    __slots__ = ("tenant_id", "doc_id", "connector_id", "source_native_id", "type", "title", "body_text", "chunks", "metadata", "participants", "ts", "acl", "original", "version_etag", "tombstone")
    class MetadataEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    DOC_ID_FIELD_NUMBER: _ClassVar[int]
    CONNECTOR_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_NATIVE_ID_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    BODY_TEXT_FIELD_NUMBER: _ClassVar[int]
    CHUNKS_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    PARTICIPANTS_FIELD_NUMBER: _ClassVar[int]
    TS_FIELD_NUMBER: _ClassVar[int]
    ACL_FIELD_NUMBER: _ClassVar[int]
    ORIGINAL_FIELD_NUMBER: _ClassVar[int]
    VERSION_ETAG_FIELD_NUMBER: _ClassVar[int]
    TOMBSTONE_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    doc_id: str
    connector_id: str
    source_native_id: str
    type: DocType
    title: str
    body_text: str
    chunks: _containers.RepeatedCompositeFieldContainer[Chunk]
    metadata: _containers.ScalarMap[str, str]
    participants: _containers.RepeatedCompositeFieldContainer[Participant]
    ts: Timestamps
    acl: AclInfo
    original: BlobRef
    version_etag: str
    tombstone: Tombstone
    def __init__(self, tenant_id: _Optional[str] = ..., doc_id: _Optional[str] = ..., connector_id: _Optional[str] = ..., source_native_id: _Optional[str] = ..., type: _Optional[_Union[DocType, str]] = ..., title: _Optional[str] = ..., body_text: _Optional[str] = ..., chunks: _Optional[_Iterable[_Union[Chunk, _Mapping]]] = ..., metadata: _Optional[_Mapping[str, str]] = ..., participants: _Optional[_Iterable[_Union[Participant, _Mapping]]] = ..., ts: _Optional[_Union[Timestamps, _Mapping]] = ..., acl: _Optional[_Union[AclInfo, _Mapping]] = ..., original: _Optional[_Union[BlobRef, _Mapping]] = ..., version_etag: _Optional[str] = ..., tombstone: _Optional[_Union[Tombstone, _Mapping]] = ...) -> None: ...

class Chunk(_message.Message):
    __slots__ = ("chunk_id", "text", "embedding_ref", "char_start", "char_end", "embedding")
    CHUNK_ID_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    EMBEDDING_REF_FIELD_NUMBER: _ClassVar[int]
    CHAR_START_FIELD_NUMBER: _ClassVar[int]
    CHAR_END_FIELD_NUMBER: _ClassVar[int]
    EMBEDDING_FIELD_NUMBER: _ClassVar[int]
    chunk_id: str
    text: str
    embedding_ref: str
    char_start: int
    char_end: int
    embedding: _containers.RepeatedScalarFieldContainer[float]
    def __init__(self, chunk_id: _Optional[str] = ..., text: _Optional[str] = ..., embedding_ref: _Optional[str] = ..., char_start: _Optional[int] = ..., char_end: _Optional[int] = ..., embedding: _Optional[_Iterable[float]] = ...) -> None: ...

class Participant(_message.Message):
    __slots__ = ("name", "email", "handle", "role")
    NAME_FIELD_NUMBER: _ClassVar[int]
    EMAIL_FIELD_NUMBER: _ClassVar[int]
    HANDLE_FIELD_NUMBER: _ClassVar[int]
    ROLE_FIELD_NUMBER: _ClassVar[int]
    name: str
    email: str
    handle: str
    role: str
    def __init__(self, name: _Optional[str] = ..., email: _Optional[str] = ..., handle: _Optional[str] = ..., role: _Optional[str] = ...) -> None: ...

class Timestamps(_message.Message):
    __slots__ = ("created", "modified", "ingested")
    CREATED_FIELD_NUMBER: _ClassVar[int]
    MODIFIED_FIELD_NUMBER: _ClassVar[int]
    INGESTED_FIELD_NUMBER: _ClassVar[int]
    created: _timestamp_pb2.Timestamp
    modified: _timestamp_pb2.Timestamp
    ingested: _timestamp_pb2.Timestamp
    def __init__(self, created: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., modified: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., ingested: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class AclInfo(_message.Message):
    __slots__ = ("allowed_principals", "is_private")
    ALLOWED_PRINCIPALS_FIELD_NUMBER: _ClassVar[int]
    IS_PRIVATE_FIELD_NUMBER: _ClassVar[int]
    allowed_principals: _containers.RepeatedScalarFieldContainer[str]
    is_private: bool
    def __init__(self, allowed_principals: _Optional[_Iterable[str]] = ..., is_private: _Optional[bool] = ...) -> None: ...

class BlobRef(_message.Message):
    __slots__ = ("bucket", "key", "size_bytes", "content_type", "sha256")
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    CONTENT_TYPE_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    bucket: str
    key: str
    size_bytes: int
    content_type: str
    sha256: str
    def __init__(self, bucket: _Optional[str] = ..., key: _Optional[str] = ..., size_bytes: _Optional[int] = ..., content_type: _Optional[str] = ..., sha256: _Optional[str] = ...) -> None: ...

class Tombstone(_message.Message):
    __slots__ = ("deleted", "deleted_at")
    DELETED_FIELD_NUMBER: _ClassVar[int]
    DELETED_AT_FIELD_NUMBER: _ClassVar[int]
    deleted: bool
    deleted_at: _timestamp_pb2.Timestamp
    def __init__(self, deleted: _Optional[bool] = ..., deleted_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
