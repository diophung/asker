import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ConnectorStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CONNECTOR_STATUS_UNSPECIFIED: _ClassVar[ConnectorStatus]
    ACTIVE: _ClassVar[ConnectorStatus]
    PAUSED: _ClassVar[ConnectorStatus]
    ERROR: _ClassVar[ConnectorStatus]

class SyncPhase(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SYNC_PHASE_UNSPECIFIED: _ClassVar[SyncPhase]
    PENDING: _ClassVar[SyncPhase]
    FULL_SYNC: _ClassVar[SyncPhase]
    INCREMENTAL: _ClassVar[SyncPhase]
    FAILED: _ClassVar[SyncPhase]
CONNECTOR_STATUS_UNSPECIFIED: ConnectorStatus
ACTIVE: ConnectorStatus
PAUSED: ConnectorStatus
ERROR: ConnectorStatus
SYNC_PHASE_UNSPECIFIED: SyncPhase
PENDING: SyncPhase
FULL_SYNC: SyncPhase
INCREMENTAL: SyncPhase
FAILED: SyncPhase

class Tenant(_message.Message):
    __slots__ = ("tenant_id", "created")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    CREATED_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    created: _timestamp_pb2.Timestamp
    def __init__(self, tenant_id: _Optional[str] = ..., created: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class EnsureTenantRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class EnsureTenantResponse(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: Tenant
    def __init__(self, tenant: _Optional[_Union[Tenant, _Mapping]] = ...) -> None: ...

class ConnectorInstance(_message.Message):
    __slots__ = ("id", "connector_id", "display_name", "config_json", "status", "created", "updated")
    ID_FIELD_NUMBER: _ClassVar[int]
    CONNECTOR_ID_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    CONFIG_JSON_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    CREATED_FIELD_NUMBER: _ClassVar[int]
    UPDATED_FIELD_NUMBER: _ClassVar[int]
    id: str
    connector_id: str
    display_name: str
    config_json: bytes
    status: ConnectorStatus
    created: _timestamp_pb2.Timestamp
    updated: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., connector_id: _Optional[str] = ..., display_name: _Optional[str] = ..., config_json: _Optional[bytes] = ..., status: _Optional[_Union[ConnectorStatus, str]] = ..., created: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., updated: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class CreateConnectorInstanceRequest(_message.Message):
    __slots__ = ("connector_id", "display_name", "config_json")
    CONNECTOR_ID_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    CONFIG_JSON_FIELD_NUMBER: _ClassVar[int]
    connector_id: str
    display_name: str
    config_json: bytes
    def __init__(self, connector_id: _Optional[str] = ..., display_name: _Optional[str] = ..., config_json: _Optional[bytes] = ...) -> None: ...

class CreateConnectorInstanceResponse(_message.Message):
    __slots__ = ("instance",)
    INSTANCE_FIELD_NUMBER: _ClassVar[int]
    instance: ConnectorInstance
    def __init__(self, instance: _Optional[_Union[ConnectorInstance, _Mapping]] = ...) -> None: ...

class ListConnectorInstancesRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListConnectorInstancesResponse(_message.Message):
    __slots__ = ("instances",)
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    instances: _containers.RepeatedCompositeFieldContainer[ConnectorInstance]
    def __init__(self, instances: _Optional[_Iterable[_Union[ConnectorInstance, _Mapping]]] = ...) -> None: ...

class GetConnectorInstanceRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class GetConnectorInstanceResponse(_message.Message):
    __slots__ = ("instance",)
    INSTANCE_FIELD_NUMBER: _ClassVar[int]
    instance: ConnectorInstance
    def __init__(self, instance: _Optional[_Union[ConnectorInstance, _Mapping]] = ...) -> None: ...

class DeleteConnectorInstanceRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class DeleteConnectorInstanceResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class SyncState(_message.Message):
    __slots__ = ("connector_instance_id", "cursor", "phase", "last_sync_started", "last_sync_completed", "last_error", "docs_emitted")
    CONNECTOR_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    CURSOR_FIELD_NUMBER: _ClassVar[int]
    PHASE_FIELD_NUMBER: _ClassVar[int]
    LAST_SYNC_STARTED_FIELD_NUMBER: _ClassVar[int]
    LAST_SYNC_COMPLETED_FIELD_NUMBER: _ClassVar[int]
    LAST_ERROR_FIELD_NUMBER: _ClassVar[int]
    DOCS_EMITTED_FIELD_NUMBER: _ClassVar[int]
    connector_instance_id: str
    cursor: str
    phase: SyncPhase
    last_sync_started: _timestamp_pb2.Timestamp
    last_sync_completed: _timestamp_pb2.Timestamp
    last_error: str
    docs_emitted: int
    def __init__(self, connector_instance_id: _Optional[str] = ..., cursor: _Optional[str] = ..., phase: _Optional[_Union[SyncPhase, str]] = ..., last_sync_started: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_sync_completed: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_error: _Optional[str] = ..., docs_emitted: _Optional[int] = ...) -> None: ...

class GetSyncStateRequest(_message.Message):
    __slots__ = ("connector_instance_id",)
    CONNECTOR_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    connector_instance_id: str
    def __init__(self, connector_instance_id: _Optional[str] = ...) -> None: ...

class GetSyncStateResponse(_message.Message):
    __slots__ = ("state",)
    STATE_FIELD_NUMBER: _ClassVar[int]
    state: SyncState
    def __init__(self, state: _Optional[_Union[SyncState, _Mapping]] = ...) -> None: ...

class SetSyncStateRequest(_message.Message):
    __slots__ = ("state",)
    STATE_FIELD_NUMBER: _ClassVar[int]
    state: SyncState
    def __init__(self, state: _Optional[_Union[SyncState, _Mapping]] = ...) -> None: ...

class SetSyncStateResponse(_message.Message):
    __slots__ = ("state",)
    STATE_FIELD_NUMBER: _ClassVar[int]
    state: SyncState
    def __init__(self, state: _Optional[_Union[SyncState, _Mapping]] = ...) -> None: ...

class PutTokenRequest(_message.Message):
    __slots__ = ("connector_instance_id", "token")
    CONNECTOR_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    TOKEN_FIELD_NUMBER: _ClassVar[int]
    connector_instance_id: str
    token: bytes
    def __init__(self, connector_instance_id: _Optional[str] = ..., token: _Optional[bytes] = ...) -> None: ...

class PutTokenResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GetTokenRequest(_message.Message):
    __slots__ = ("connector_instance_id",)
    CONNECTOR_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    connector_instance_id: str
    def __init__(self, connector_instance_id: _Optional[str] = ...) -> None: ...

class GetTokenResponse(_message.Message):
    __slots__ = ("token",)
    TOKEN_FIELD_NUMBER: _ClassVar[int]
    token: bytes
    def __init__(self, token: _Optional[bytes] = ...) -> None: ...

class DeleteTokenRequest(_message.Message):
    __slots__ = ("connector_instance_id",)
    CONNECTOR_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    connector_instance_id: str
    def __init__(self, connector_instance_id: _Optional[str] = ...) -> None: ...

class DeleteTokenResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListAllInstancesRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListAllInstancesResponse(_message.Message):
    __slots__ = ("instances",)
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    instances: _containers.RepeatedCompositeFieldContainer[TenantInstance]
    def __init__(self, instances: _Optional[_Iterable[_Union[TenantInstance, _Mapping]]] = ...) -> None: ...

class TenantInstance(_message.Message):
    __slots__ = ("tenant_id", "instance")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    instance: ConnectorInstance
    def __init__(self, tenant_id: _Optional[str] = ..., instance: _Optional[_Union[ConnectorInstance, _Mapping]] = ...) -> None: ...
