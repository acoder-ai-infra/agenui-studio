package artifact

import (
	"context"
	"io"
	"time"
)

const ArtifactMetaSchemaVersion = "harness.artifact_meta.v1"

type OwnerModule string

const (
	OwnerModuleToolGateway   OwnerModule = "tool_gateway"
	OwnerModuleContextEngine OwnerModule = "context_engine"
	OwnerModuleRuntime       OwnerModule = "runtime"
	OwnerModuleA2AGateway    OwnerModule = "a2a_gateway"
	OwnerModuleObservability OwnerModule = "observability"
	OwnerModuleProtocol      OwnerModule = "protocol"
	OwnerModuleModelGateway  OwnerModule = "model_gateway"
	OwnerModuleSkill         OwnerModule = "skill"
	// OwnerModuleHost 标记经 SDK ArtifactClient 由宿主上传的业务 artifact。
	OwnerModuleHost OwnerModule = "host"
)

type ArtifactType string

const (
	ArtifactTypeToolResult      ArtifactType = "tool_result"
	ArtifactTypeContextSnapshot ArtifactType = "context_snapshot"
	ArtifactTypeCheckpointState ArtifactType = "checkpoint_state"
	ArtifactTypeDebugPayload    ArtifactType = "debug_payload"
	ArtifactTypeFinalResult     ArtifactType = "final_result"
	ArtifactTypeFile            ArtifactType = "file"
	ArtifactTypeImage           ArtifactType = "image"
	ArtifactTypeSchema          ArtifactType = "schema"
	ArtifactTypePrompt          ArtifactType = "prompt"
	ArtifactTypeControlResponse ArtifactType = "control_response"
	// ArtifactTypeHostData 是宿主上传的结构化业务数据（如 VisualSpec）。
	ArtifactTypeHostData ArtifactType = "host_data"
)

type Visibility string

const (
	VisibilityUserVisible Visibility = "user_visible"
	VisibilityInternal    Visibility = "internal"
	VisibilityDebug       Visibility = "debug"
	VisibilityRestricted  Visibility = "restricted"
)

type RetentionPolicy string

const (
	RetentionRunTTL        RetentionPolicy = "run_ttl"
	RetentionSessionTTL    RetentionPolicy = "session_ttl"
	RetentionDebugShortTTL RetentionPolicy = "debug_short_ttl"
	RetentionAuditTTL      RetentionPolicy = "audit_ttl"
	RetentionCheckpointTTL RetentionPolicy = "checkpoint_ttl"
)

type ArtifactStatus string

const (
	ArtifactStatusReady   ArtifactStatus = "ready"
	ArtifactStatusDeleted ArtifactStatus = "deleted"
	ArtifactStatusExpired ArtifactStatus = "expired"
)

type PurgeStatus string

const (
	PurgeStatusNone     PurgeStatus = "none"
	PurgeStatusPending  PurgeStatus = "pending"
	PurgeStatusLeased   PurgeStatus = "leased"
	PurgeStatusRetrying PurgeStatus = "retrying"
	PurgeStatusPurged   PurgeStatus = "purged"
)

type LineageRelation string

const (
	LineageUploadedFrom    LineageRelation = "uploaded_from"
	LineageTransformedFrom LineageRelation = "transformed_from"
	LineageGeneratedFrom   LineageRelation = "generated_from"
	LineageSummarizedFrom  LineageRelation = "summarized_from"
	LineageMergedFrom      LineageRelation = "merged_from"
	LineageReferenced      LineageRelation = "referenced"
)

type Purpose string

const (
	PurposeView         Purpose = "view"
	PurposeDownload     Purpose = "download"
	PurposeModelContext Purpose = "model_context"
	PurposeDebug        Purpose = "debug"
	PurposeReplay       Purpose = "replay"
	// PurposeHostAccess 是宿主经 SDK ArtifactClient 读取业务 artifact 的用途。
	PurposeHostAccess Purpose = "host_access"
)

type DeleteReason string

const (
	DeleteReasonUser    DeleteReason = "user"
	DeleteReasonTTL     DeleteReason = "ttl"
	DeleteReasonCleanup DeleteReason = "cleanup"
)

type Preview struct {
	Text      string         `json:"text,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
}

type ArtifactLineage struct {
	ArtifactRef string          `json:"artifact_ref"`
	Relation    LineageRelation `json:"relation"`
}

type ArtifactMeta struct {
	ArtifactID      string            `json:"artifact_id"`
	ArtifactRef     string            `json:"artifact_ref"`
	TenantID        string            `json:"tenant_id"`
	UserID          string            `json:"user_id,omitempty"`
	SessionID       string            `json:"session_id"`
	RunID           string            `json:"run_id"`
	StepID          string            `json:"step_id,omitempty"`
	OwnerModule     OwnerModule       `json:"owner_module"`
	OwnerID         string            `json:"owner_id"`
	ArtifactType    ArtifactType      `json:"artifact_type"`
	MimeType        string            `json:"mime_type"`
	Name            string            `json:"name,omitempty"`
	SizeBytes       int64             `json:"size_bytes"`
	Hash            string            `json:"hash"`
	Visibility      Visibility        `json:"visibility"`
	StorageBackend  string            `json:"storage_backend"`
	StorageKey      string            `json:"storage_key"`
	Preview         Preview           `json:"preview"`
	RetentionPolicy RetentionPolicy   `json:"retention_policy"`
	ExpiresAt       time.Time         `json:"expires_at,omitempty"`
	CreatedBy       string            `json:"created_by,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	Status          ArtifactStatus    `json:"status"`
	DerivedFrom     []ArtifactLineage `json:"derived_from,omitempty"`
	SchemaVersion   string            `json:"schema_version"`
	DeletedAt       time.Time         `json:"deleted_at,omitempty"`
	DeleteReason    DeleteReason      `json:"delete_reason,omitempty"`
	PurgeStatus     PurgeStatus       `json:"purge_status,omitempty"`
	PurgedAt        time.Time         `json:"purged_at,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type PurgeJob struct {
	ArtifactRef  string      `json:"artifact_ref"`
	TenantID     string      `json:"tenant_id"`
	SessionID    string      `json:"session_id"`
	RunID        string      `json:"run_id"`
	StorageKey   string      `json:"storage_key"`
	Status       PurgeStatus `json:"status"`
	Attempts     int         `json:"attempts"`
	LastError    string      `json:"last_error,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	LeaseOwner   string      `json:"lease_owner,omitempty"`
	LeaseUntil   time.Time   `json:"lease_until,omitempty"`
	LeaseVersion int64       `json:"lease_version,omitempty"`
}

type PurgeQuery struct {
	TenantID  string
	SessionID string
	RunID     string
	Limit     int
	ReadyAt   time.Time
}

type ProductionDependency interface {
	ProductionReady() bool
}

type PutArtifactRequest struct {
	// ArtifactID optionally reserves a deterministic identity. Callers using it
	// must also provide an idempotency key so retries converge on one object.
	ArtifactID      string
	TenantID        string
	UserID          string
	SessionID       string
	RunID           string
	StepID          string
	OwnerModule     OwnerModule
	OwnerID         string
	ArtifactType    ArtifactType
	MimeType        string
	Name            string
	Visibility      Visibility
	Content         io.Reader
	PreviewHint     PreviewHint
	RetentionPolicy RetentionPolicy
	ExpiresAt       time.Time
	CreatedBy       string
	DerivedFrom     []ArtifactLineage
	IdempotencyKey  string
	Metadata        map[string]string
}

type PreviewHint struct {
	MaxBytes int
}

type GetOptions struct {
	Purpose Purpose
}

type DownloadURLOptions struct {
	TTL time.Duration
}

type DownloadURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ArtifactObject struct {
	Meta    ArtifactMeta
	Content io.ReadCloser
}

type ListQuery struct {
	TenantID          string
	SessionID         string
	RunID             string
	OwnerModule       OwnerModule
	OwnerID           string
	Type              ArtifactType
	Visibility        Visibility
	ExpiredAtOrBefore time.Time
	IncludeDeleted    bool
}

type CleanupResult struct {
	Expired int
	Deleted int
}

type ArtifactStore interface {
	Put(ctx context.Context, req PutArtifactRequest) (*ArtifactMeta, error)
	Get(ctx context.Context, ref string, opts GetOptions) (*ArtifactObject, error)
	Head(ctx context.Context, ref string) (*ArtifactMeta, error)
	Delete(ctx context.Context, ref string, reason DeleteReason) error
	CreateDownloadURL(ctx context.Context, ref string, opts DownloadURLOptions) (*DownloadURL, error)
	List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error)
	CleanupExpired(ctx context.Context, now time.Time) (*CleanupResult, error)
}
