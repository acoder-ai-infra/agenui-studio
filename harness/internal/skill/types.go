package skill

import (
	"context"
	"errors"
	"time"
)

var (
	ErrPrincipalRequired    = errors.New("skill principal required")
	ErrPermissionDenied     = errors.New("skill permission denied")
	ErrNotFound             = errors.New("skill not found")
	ErrVersionConflict      = errors.New("skill version content conflict")
	ErrInvalidDefinition    = errors.New("invalid skill definition")
	ErrDependencyCycle      = errors.New("skill dependency cycle")
	ErrContentIntegrity     = errors.New("skill content integrity check failed")
	ErrLifecycleUnsupported = errors.New("skill lifecycle operation unsupported")
)

type Principal struct {
	TenantID string
	AgentID  string
	System   bool
}

func (p Principal) Validate() error {
	if p.System {
		return nil
	}
	if p.TenantID == "" || p.AgentID == "" {
		return ErrPrincipalRequired
	}
	return nil
}

type Scope string

const (
	ScopeTenant Scope = "tenant"
	ScopeGlobal Scope = "global"
)

type InjectionStrategy string

const (
	InjectSystem   InjectionStrategy = "system_inject"
	InjectOnDemand InjectionStrategy = "on_demand"
	InjectToolOnly InjectionStrategy = "tool_binding_only"
)

type Ref struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Dependencies struct {
	Tools      []string `json:"tools,omitempty"`
	MCPServers []string `json:"mcp_servers,omitempty"`
	Skills     []Ref    `json:"skills,omitempty"`
}

type Policy struct {
	Scope          Scope    `json:"scope"`
	AllowedAgents  []string `json:"allowed_agents,omitempty"`
	MaxInputTokens int      `json:"max_input_tokens,omitempty"`
}

type Definition struct {
	ID                string            `json:"id"`
	Version           string            `json:"version"`
	TenantID          string            `json:"tenant_id,omitempty"`
	Description       string            `json:"description,omitempty"`
	InjectionStrategy InjectionStrategy `json:"injection_strategy"`
	Dependencies      Dependencies      `json:"dependencies,omitempty"`
	Policy            Policy            `json:"policy"`
}

type Record struct {
	Definition  Definition `json:"definition"`
	ContentHash string     `json:"content_hash"`
	ContentSize int64      `json:"content_size"`
	PublishedAt time.Time  `json:"published_at"`
}

type FileRecord struct {
	Path        string `json:"path"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentHash string `json:"content_hash"`
	Previewable bool   `json:"previewable"`
	Content     []byte `json:"-"`
	ArtifactRef string `json:"-"`
}

// PackageObject is the immutable ZIP persisted by the attachment store for one
// Skill version. Database repositories retain only its ref and file index.
type PackageObject struct {
	TenantID string
	SkillID  string
	Version  string
	Archive  []byte
}

type PackageObjectStore interface {
	PutPackage(ctx context.Context, object PackageObject) (string, error)
	GetPackage(ctx context.Context, tenantID, artifactRef string) ([]byte, error)
}

type FileList struct {
	SkillID string       `json:"skill_id"`
	Version string       `json:"version"`
	Files   []FileRecord `json:"files"`
}

type FilePreview struct {
	SkillID     string `json:"skill_id"`
	Version     string `json:"version"`
	Path        string `json:"path"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentHash string `json:"content_hash"`
	Content     string `json:"content"`
}

// Retirement is a discovery tombstone. The immutable version and its content
// remain readable by exact reference so frozen historical runs can recover.
type Retirement struct {
	TenantID  string    `json:"tenant_id"`
	SkillID   string    `json:"skill_id"`
	Version   string    `json:"version"`
	RetiredAt time.Time `json:"retired_at"`
}

// Repository owns the atomic version/CAS boundary. A production implementation
// may use an OLTP transaction plus content-addressed object storage, but must
// expose the same idempotent semantics.
type Repository interface {
	PublishAtomic(ctx context.Context, record Record, content []byte) (Record, bool, error)
	Get(ctx context.Context, tenantID, skillID, version string) (Record, error)
	LoadContent(ctx context.Context, contentHash string) ([]byte, error)
	List(ctx context.Context, tenantID string) ([]Record, error)
}

type FileRepository interface {
	PublishAtomicWithFiles(ctx context.Context, record Record, content []byte, files []FileRecord) (Record, bool, error)
	ListFiles(ctx context.Context, tenantID, skillID, version string) ([]FileRecord, error)
	LoadFile(ctx context.Context, tenantID, skillID, version, filePath string) (FileRecord, error)
}

type RetirementRepository interface {
	Retire(ctx context.Context, retirement Retirement) (Retirement, bool, error)
}

type TokenEstimator interface {
	Estimate(ctx context.Context, content string) (int, error)
}

type Snapshot struct {
	ID                string            `json:"id"`
	SkillID           string            `json:"skill_id"`
	Version           string            `json:"version"`
	TenantID          string            `json:"tenant_id,omitempty"`
	ContentHash       string            `json:"content_hash"`
	ContentSize       int64             `json:"content_size"`
	EstimatedTokens   int               `json:"estimated_tokens"`
	InjectionStrategy InjectionStrategy `json:"injection_strategy"`
	Dependencies      Dependencies      `json:"dependencies,omitempty"`
	ResolvedAt        time.Time         `json:"resolved_at"`
}

type Resolution struct {
	Root         Snapshot             `json:"root"`
	Dependencies []ResolvedDependency `json:"dependencies,omitempty"`
	Instructions string               `json:"instructions,omitempty"`
}

type ResolvedDependency struct {
	Snapshot     Snapshot `json:"snapshot"`
	Instructions string   `json:"instructions,omitempty"`
}
