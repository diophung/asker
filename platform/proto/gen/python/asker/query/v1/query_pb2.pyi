import datetime

from asker.v1 import document_pb2 as _document_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SearchMode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SEARCH_MODE_UNSPECIFIED: _ClassVar[SearchMode]
    HYBRID: _ClassVar[SearchMode]
    KEYWORD: _ClassVar[SearchMode]
    VECTOR: _ClassVar[SearchMode]
SEARCH_MODE_UNSPECIFIED: SearchMode
HYBRID: SearchMode
KEYWORD: SearchMode
VECTOR: SearchMode

class SearchRequest(_message.Message):
    __slots__ = ("query", "doc_types", "from_date", "to_date", "participant", "limit", "offset", "mode")
    QUERY_FIELD_NUMBER: _ClassVar[int]
    DOC_TYPES_FIELD_NUMBER: _ClassVar[int]
    FROM_DATE_FIELD_NUMBER: _ClassVar[int]
    TO_DATE_FIELD_NUMBER: _ClassVar[int]
    PARTICIPANT_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    OFFSET_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    query: str
    doc_types: _containers.RepeatedScalarFieldContainer[_document_pb2.DocType]
    from_date: _timestamp_pb2.Timestamp
    to_date: _timestamp_pb2.Timestamp
    participant: str
    limit: int
    offset: int
    mode: SearchMode
    def __init__(self, query: _Optional[str] = ..., doc_types: _Optional[_Iterable[_Union[_document_pb2.DocType, str]]] = ..., from_date: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., to_date: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., participant: _Optional[str] = ..., limit: _Optional[int] = ..., offset: _Optional[int] = ..., mode: _Optional[_Union[SearchMode, str]] = ...) -> None: ...

class SearchResponse(_message.Message):
    __slots__ = ("hits", "total", "degraded", "took_ms", "cached")
    HITS_FIELD_NUMBER: _ClassVar[int]
    TOTAL_FIELD_NUMBER: _ClassVar[int]
    DEGRADED_FIELD_NUMBER: _ClassVar[int]
    TOOK_MS_FIELD_NUMBER: _ClassVar[int]
    CACHED_FIELD_NUMBER: _ClassVar[int]
    hits: _containers.RepeatedCompositeFieldContainer[Hit]
    total: int
    degraded: str
    took_ms: int
    cached: bool
    def __init__(self, hits: _Optional[_Iterable[_Union[Hit, _Mapping]]] = ..., total: _Optional[int] = ..., degraded: _Optional[str] = ..., took_ms: _Optional[int] = ..., cached: _Optional[bool] = ...) -> None: ...

class Hit(_message.Message):
    __slots__ = ("doc_id", "connector_id", "type", "title", "snippet", "score", "created", "modified", "metadata")
    class MetadataEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    DOC_ID_FIELD_NUMBER: _ClassVar[int]
    CONNECTOR_ID_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    SNIPPET_FIELD_NUMBER: _ClassVar[int]
    SCORE_FIELD_NUMBER: _ClassVar[int]
    CREATED_FIELD_NUMBER: _ClassVar[int]
    MODIFIED_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    doc_id: str
    connector_id: str
    type: _document_pb2.DocType
    title: str
    snippet: str
    score: float
    created: _timestamp_pb2.Timestamp
    modified: _timestamp_pb2.Timestamp
    metadata: _containers.ScalarMap[str, str]
    def __init__(self, doc_id: _Optional[str] = ..., connector_id: _Optional[str] = ..., type: _Optional[_Union[_document_pb2.DocType, str]] = ..., title: _Optional[str] = ..., snippet: _Optional[str] = ..., score: _Optional[float] = ..., created: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., modified: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., metadata: _Optional[_Mapping[str, str]] = ...) -> None: ...
