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

class GetPersonalizationRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GetPersonalizationResponse(_message.Message):
    __slots__ = ("profile_json", "version", "weights_json", "sample_count", "exists")
    PROFILE_JSON_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    WEIGHTS_JSON_FIELD_NUMBER: _ClassVar[int]
    SAMPLE_COUNT_FIELD_NUMBER: _ClassVar[int]
    EXISTS_FIELD_NUMBER: _ClassVar[int]
    profile_json: str
    version: int
    weights_json: str
    sample_count: int
    exists: bool
    def __init__(self, profile_json: _Optional[str] = ..., version: _Optional[int] = ..., weights_json: _Optional[str] = ..., sample_count: _Optional[int] = ..., exists: _Optional[bool] = ...) -> None: ...

class PutPreferencesRequest(_message.Message):
    __slots__ = ("profile_json",)
    PROFILE_JSON_FIELD_NUMBER: _ClassVar[int]
    profile_json: str
    def __init__(self, profile_json: _Optional[str] = ...) -> None: ...

class PutPreferencesResponse(_message.Message):
    __slots__ = ("version",)
    VERSION_FIELD_NUMBER: _ClassVar[int]
    version: int
    def __init__(self, version: _Optional[int] = ...) -> None: ...

class FeedbackEvent(_message.Message):
    __slots__ = ("doc_id", "doc_type", "connector_id", "senders", "topics", "action", "dwell_ms", "query")
    DOC_ID_FIELD_NUMBER: _ClassVar[int]
    DOC_TYPE_FIELD_NUMBER: _ClassVar[int]
    CONNECTOR_ID_FIELD_NUMBER: _ClassVar[int]
    SENDERS_FIELD_NUMBER: _ClassVar[int]
    TOPICS_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    DWELL_MS_FIELD_NUMBER: _ClassVar[int]
    QUERY_FIELD_NUMBER: _ClassVar[int]
    doc_id: str
    doc_type: str
    connector_id: str
    senders: _containers.RepeatedScalarFieldContainer[str]
    topics: _containers.RepeatedScalarFieldContainer[str]
    action: str
    dwell_ms: int
    query: str
    def __init__(self, doc_id: _Optional[str] = ..., doc_type: _Optional[str] = ..., connector_id: _Optional[str] = ..., senders: _Optional[_Iterable[str]] = ..., topics: _Optional[_Iterable[str]] = ..., action: _Optional[str] = ..., dwell_ms: _Optional[int] = ..., query: _Optional[str] = ...) -> None: ...

class RecordFeedbackRequest(_message.Message):
    __slots__ = ("event",)
    EVENT_FIELD_NUMBER: _ClassVar[int]
    event: FeedbackEvent
    def __init__(self, event: _Optional[_Union[FeedbackEvent, _Mapping]] = ...) -> None: ...

class RecordFeedbackResponse(_message.Message):
    __slots__ = ("weights_json", "sample_count", "learning_paused")
    WEIGHTS_JSON_FIELD_NUMBER: _ClassVar[int]
    SAMPLE_COUNT_FIELD_NUMBER: _ClassVar[int]
    LEARNING_PAUSED_FIELD_NUMBER: _ClassVar[int]
    weights_json: str
    sample_count: int
    learning_paused: bool
    def __init__(self, weights_json: _Optional[str] = ..., sample_count: _Optional[int] = ..., learning_paused: _Optional[bool] = ...) -> None: ...

class ResetLearningRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ResetLearningResponse(_message.Message):
    __slots__ = ("feedback_deleted",)
    FEEDBACK_DELETED_FIELD_NUMBER: _ClassVar[int]
    feedback_deleted: int
    def __init__(self, feedback_deleted: _Optional[int] = ...) -> None: ...

class DeleteTenantRequest(_message.Message):
    __slots__ = ("confirm",)
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    confirm: str
    def __init__(self, confirm: _Optional[str] = ...) -> None: ...

class DeleteReport(_message.Message):
    __slots__ = ("tenant_id", "connector_instances_deleted", "tokens_deleted", "dek_destroyed", "vespa_group_purged", "blobs_deleted", "redis_purged", "verified_empty", "actor")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    CONNECTOR_INSTANCES_DELETED_FIELD_NUMBER: _ClassVar[int]
    TOKENS_DELETED_FIELD_NUMBER: _ClassVar[int]
    DEK_DESTROYED_FIELD_NUMBER: _ClassVar[int]
    VESPA_GROUP_PURGED_FIELD_NUMBER: _ClassVar[int]
    BLOBS_DELETED_FIELD_NUMBER: _ClassVar[int]
    REDIS_PURGED_FIELD_NUMBER: _ClassVar[int]
    VERIFIED_EMPTY_FIELD_NUMBER: _ClassVar[int]
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    connector_instances_deleted: int
    tokens_deleted: int
    dek_destroyed: bool
    vespa_group_purged: bool
    blobs_deleted: int
    redis_purged: bool
    verified_empty: bool
    actor: str
    def __init__(self, tenant_id: _Optional[str] = ..., connector_instances_deleted: _Optional[int] = ..., tokens_deleted: _Optional[int] = ..., dek_destroyed: _Optional[bool] = ..., vespa_group_purged: _Optional[bool] = ..., blobs_deleted: _Optional[int] = ..., redis_purged: _Optional[bool] = ..., verified_empty: _Optional[bool] = ..., actor: _Optional[str] = ...) -> None: ...

class DeleteTenantResponse(_message.Message):
    __slots__ = ("report",)
    REPORT_FIELD_NUMBER: _ClassVar[int]
    report: DeleteReport
    def __init__(self, report: _Optional[_Union[DeleteReport, _Mapping]] = ...) -> None: ...

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

class TenantUsage(_message.Message):
    __slots__ = ("tenant_id", "created", "connector_instances", "docs_emitted", "suspended")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    CREATED_FIELD_NUMBER: _ClassVar[int]
    CONNECTOR_INSTANCES_FIELD_NUMBER: _ClassVar[int]
    DOCS_EMITTED_FIELD_NUMBER: _ClassVar[int]
    SUSPENDED_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    created: _timestamp_pb2.Timestamp
    connector_instances: int
    docs_emitted: int
    suspended: bool
    def __init__(self, tenant_id: _Optional[str] = ..., created: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., connector_instances: _Optional[int] = ..., docs_emitted: _Optional[int] = ..., suspended: _Optional[bool] = ...) -> None: ...

class ListTenantsRequest(_message.Message):
    __slots__ = ("limit", "page_token")
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    limit: int
    page_token: str
    def __init__(self, limit: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListTenantsResponse(_message.Message):
    __slots__ = ("tenants", "next_page_token")
    TENANTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenants: _containers.RepeatedCompositeFieldContainer[TenantUsage]
    next_page_token: str
    def __init__(self, tenants: _Optional[_Iterable[_Union[TenantUsage, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class GetTenantUsageRequest(_message.Message):
    __slots__ = ("tenant_id",)
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    def __init__(self, tenant_id: _Optional[str] = ...) -> None: ...

class GetTenantUsageResponse(_message.Message):
    __slots__ = ("usage",)
    USAGE_FIELD_NUMBER: _ClassVar[int]
    usage: TenantUsage
    def __init__(self, usage: _Optional[_Union[TenantUsage, _Mapping]] = ...) -> None: ...

class SuspendTenantRequest(_message.Message):
    __slots__ = ("tenant_id", "suspended")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    SUSPENDED_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    suspended: bool
    def __init__(self, tenant_id: _Optional[str] = ..., suspended: _Optional[bool] = ...) -> None: ...

class SuspendTenantResponse(_message.Message):
    __slots__ = ("instances_changed",)
    INSTANCES_CHANGED_FIELD_NUMBER: _ClassVar[int]
    instances_changed: int
    def __init__(self, instances_changed: _Optional[int] = ...) -> None: ...

class AdminDeleteTenantRequest(_message.Message):
    __slots__ = ("tenant_id",)
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    def __init__(self, tenant_id: _Optional[str] = ...) -> None: ...

class AdminDeleteTenantResponse(_message.Message):
    __slots__ = ("report",)
    REPORT_FIELD_NUMBER: _ClassVar[int]
    report: DeleteReport
    def __init__(self, report: _Optional[_Union[DeleteReport, _Mapping]] = ...) -> None: ...
