from asker.v1 import document_pb2 as _document_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AuthType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    AUTH_TYPE_UNSPECIFIED: _ClassVar[AuthType]
    AUTH_NONE: _ClassVar[AuthType]
    AUTH_OAUTH2: _ClassVar[AuthType]
    AUTH_TOKEN: _ClassVar[AuthType]
AUTH_TYPE_UNSPECIFIED: AuthType
AUTH_NONE: AuthType
AUTH_OAUTH2: AuthType
AUTH_TOKEN: AuthType

class Config(_message.Message):
    __slots__ = ("tenant_id", "instance_id", "config_json", "token")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    CONFIG_JSON_FIELD_NUMBER: _ClassVar[int]
    TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    instance_id: str
    config_json: bytes
    token: bytes
    def __init__(self, tenant_id: _Optional[str] = ..., instance_id: _Optional[str] = ..., config_json: _Optional[bytes] = ..., token: _Optional[bytes] = ...) -> None: ...

class SpecRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class SpecResponse(_message.Message):
    __slots__ = ("id", "display_name", "auth_type", "config_schema", "supports_webhook")
    ID_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    AUTH_TYPE_FIELD_NUMBER: _ClassVar[int]
    CONFIG_SCHEMA_FIELD_NUMBER: _ClassVar[int]
    SUPPORTS_WEBHOOK_FIELD_NUMBER: _ClassVar[int]
    id: str
    display_name: str
    auth_type: AuthType
    config_schema: bytes
    supports_webhook: bool
    def __init__(self, id: _Optional[str] = ..., display_name: _Optional[str] = ..., auth_type: _Optional[_Union[AuthType, str]] = ..., config_schema: _Optional[bytes] = ..., supports_webhook: _Optional[bool] = ...) -> None: ...

class ValidateRequest(_message.Message):
    __slots__ = ("config",)
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    config: Config
    def __init__(self, config: _Optional[_Union[Config, _Mapping]] = ...) -> None: ...

class ValidateResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class FullSyncRequest(_message.Message):
    __slots__ = ("config",)
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    config: Config
    def __init__(self, config: _Optional[_Union[Config, _Mapping]] = ...) -> None: ...

class IncrementalSyncRequest(_message.Message):
    __slots__ = ("config", "cursor")
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    CURSOR_FIELD_NUMBER: _ClassVar[int]
    config: Config
    cursor: str
    def __init__(self, config: _Optional[_Union[Config, _Mapping]] = ..., cursor: _Optional[str] = ...) -> None: ...

class HandleWebhookRequest(_message.Message):
    __slots__ = ("config", "request")
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    REQUEST_FIELD_NUMBER: _ClassVar[int]
    config: Config
    request: HttpRequest
    def __init__(self, config: _Optional[_Union[Config, _Mapping]] = ..., request: _Optional[_Union[HttpRequest, _Mapping]] = ...) -> None: ...

class HttpRequest(_message.Message):
    __slots__ = ("method", "url", "headers", "body")
    class HeadersEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: HeaderValues
        def __init__(self, key: _Optional[str] = ..., value: _Optional[_Union[HeaderValues, _Mapping]] = ...) -> None: ...
    METHOD_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    method: str
    url: str
    headers: _containers.MessageMap[str, HeaderValues]
    body: bytes
    def __init__(self, method: _Optional[str] = ..., url: _Optional[str] = ..., headers: _Optional[_Mapping[str, HeaderValues]] = ..., body: _Optional[bytes] = ...) -> None: ...

class HeaderValues(_message.Message):
    __slots__ = ("values",)
    VALUES_FIELD_NUMBER: _ClassVar[int]
    values: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, values: _Optional[_Iterable[str]] = ...) -> None: ...

class SyncEvent(_message.Message):
    __slots__ = ("document", "checkpoint", "done")
    DOCUMENT_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_FIELD_NUMBER: _ClassVar[int]
    DONE_FIELD_NUMBER: _ClassVar[int]
    document: _document_pb2.Document
    checkpoint: str
    done: SyncDone
    def __init__(self, document: _Optional[_Union[_document_pb2.Document, _Mapping]] = ..., checkpoint: _Optional[str] = ..., done: _Optional[_Union[SyncDone, _Mapping]] = ...) -> None: ...

class SyncDone(_message.Message):
    __slots__ = ("cursor",)
    CURSOR_FIELD_NUMBER: _ClassVar[int]
    cursor: str
    def __init__(self, cursor: _Optional[str] = ...) -> None: ...
