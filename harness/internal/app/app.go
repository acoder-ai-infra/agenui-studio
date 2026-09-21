package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/anthropic"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/dashscope"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/openai"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	protocolmem "github.com/AGenUI/agenui-studio/harness/internal/protocol/memory"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/redisstore"
	"github.com/AGenUI/agenui-studio/harness/internal/runtimestore"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	storagemysql "github.com/AGenUI/agenui-studio/harness/internal/storage/mysql"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
)

type mysqlStorageBackend interface {
	CheckReady(context.Context) error
	Migrate(context.Context) error
	Stores() storage.Stores
}

type buildDependencies struct {
	initMySQL           func(frameworkmysql.Config) error
	getMySQL            func(string) *sql.DB
	newMySQLBackend     func(*sql.DB, string, bool) mysqlStorageBackend
	newArtifactMetadata func(*sql.DB) (artifact.MetadataStore, error)
	newRedisClient      func(redisstore.Config) redisstore.Client
	pingRedis           func(context.Context, redisstore.Client) error
}

func defaultBuildDependencies() buildDependencies {
	return buildDependencies{
		initMySQL: frameworkmysql.DBInit,
		getMySQL:  frameworkmysql.GetDB,
		newMySQLBackend: func(db *sql.DB, storageBackend string, skipLedgerCheck bool) mysqlStorageBackend {
			if storageBackend != "mysql" {
				return nil
			}
			return newUnifiedMySQLBackend(db, storagemysql.New(db), skipLedgerCheck)
		},
		newArtifactMetadata: func(db *sql.DB) (artifact.MetadataStore, error) {
			return metamem.NewMySQLMetadataStore(db)
		},
		newRedisClient: redisstore.NewClient,
		pingRedis:      redisstore.Ping,
	}
}

type builtStores struct {
	Stores  storage.Stores
	MySQLDB *sql.DB
	SQLDB   *sql.DB
	Close   func() error
}

var errExternalSQLDatabaseBackend = errors.New("external SQL database requires sqlite or mysql storage backend")

type stableCloser struct {
	once   sync.Once
	close  func() error
	result error
}

func newStableCloser(closeFn func() error) func() error {
	closer := &stableCloser{close: closeFn}
	return closer.Close
}

func (c *stableCloser) Close() error {
	c.once.Do(func() {
		if c.close != nil {
			c.result = c.close()
		}
	})
	return c.result
}

type resourceStack struct {
	once    sync.Once
	closers []func() error
	result  error
}

func (s *resourceStack) Add(closer func() error) {
	if closer != nil {
		s.closers = append(s.closers, closer)
	}
}

func (s *resourceStack) Close() error {
	s.once.Do(func() {
		var closeErrors []error
		for i := len(s.closers) - 1; i >= 0; i-- {
			if err := s.closers[i](); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		s.result = errors.Join(closeErrors...)
	})
	return s.result
}

// App is the assembled harness: a ready HTTP handler, embedded execution entry,
// and the shared wired components behind both surfaces.
type App struct {
	Handler   http.Handler
	Stores    storage.Stores
	Artifacts *artifact.Store
	Gateway   modelgateway.ModelGateway
	Providers []string            // all registered provider names across tenants
	Tenants   map[string][]string // tenant_id → provider names

	// Kernel accessors: the SDK layer wraps the same Composition Root through
	// these fields. They mirror what internal/protocol/server.Deps receives so
	// hosted + SDK entry share one truth.
	RunService     *storage.RunService
	RunEntry       kernel.RunEntry // Dispatch + Resume + Cancel
	Broker         protocol.EventBroker
	HotBuffer      protocol.HotStreamBuffer
	Registry       *agentregistry.Service
	Control        *control.Service
	ControlTickets *controlticket.Codec
	DefaultAgentID string
	Environment    Environment
	// BoundToolHandlers 是被 tools.yaml function 工具引用的 handler 名集合，
	// 供 SDK BuildReport 标注 unbound 扩展实现。
	BoundToolHandlers map[string]bool

	closeOnce       sync.Once
	closeResources  func() error
	closeResult     error
	runLifecycle    *runLifecycle
	runDrainTimeout time.Duration
	embedded        *embeddedEngine
}

// _ kernel.RunEntry asserts formalRunDispatcher satisfies the shared kernel
// contract without introducing a runtime dependency on kernel from this file.
var _ kernel.RunEntry = (*formalRunDispatcher)(nil)

// Close releases all resources owned by the application. It is safe to call
// repeatedly or concurrently; every caller observes the first close result.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.embedded != nil {
			a.embedded.Close()
		}
		if a.runLifecycle != nil {
			if err := a.runLifecycle.Close(a.runDrainTimeout); err != nil {
				a.closeResult = err
				return
			}
		}
		if a.closeResources != nil {
			a.closeResult = a.closeResources()
		}
	})
	return a.closeResult
}

// Build wires storage + model gateway (multi-provider) + protocol into a runnable
// server. modelCfg 控制按租户隔离的 provider 路由;storageCfg 控制存储后端。
// Mock provider 只在本地模型配置显式声明 protocol=mock 时注册。
func Build(modelCfg ModelConfig, storageCfg StorageConfig, authCfg AuthConfig, redisCfg RedisConfig, logger observability.StructuredLogger, tracer observability.TraceProvider, options ...BuildOption) (*App, error) {
	return buildWithDependencies(defaultBuildDependencies(), modelCfg, storageCfg, authCfg, redisCfg, logger, tracer, options...)
}

func buildWithDependencies(deps buildDependencies, modelCfg ModelConfig, storageCfg StorageConfig, authCfg AuthConfig, redisCfg RedisConfig, logger observability.StructuredLogger, tracer observability.TraceProvider, options ...BuildOption) (application *App, err error) {
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	settings := buildSettings{}
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}
	if err := validateModelProviderOptions(modelCfg); err != nil {
		return nil, err
	}
	if err := validateModelCostAttribution(modelCfg); err != nil {
		return nil, err
	}
	if err := validateAuthConfig(authCfg); err != nil {
		return nil, err
	}
	prod := isProductionEnv()
	if settings.harness != nil {
		prod = !settings.harness.Environment.IsLocal()
	}
	for _, catalog := range settings.catalogs {
		if len(catalog.Enabled) > 0 && settings.harness == nil {
			return nil, fmt.Errorf("enabled capability %q is not installed in the formal Composition Root", catalog.Enabled[0])
		}
	}
	resources := &resourceStack{}
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		if cleanupErr := resources.Close(); cleanupErr != nil {
			if err == nil {
				err = fmt.Errorf("clean up app resources: %w", cleanupErr)
			} else {
				err = errors.Join(err, fmt.Errorf("clean up app resources: %w", cleanupErr))
			}
		}
	}()

	// 存储后端与租户无关。Ledger、Artifact、Registry、Prompt 共享同一连接池。
	built, err := buildStoresWithSharedSQLDatabase(context.Background(), storageCfg, logger, prod, deps, settings.sharedSQLDatabase)
	if err != nil {
		return nil, err
	}
	resources.Add(built.Close)
	stores := built.Stores
	// SQLDB is the shared SQL pool for both local SQLite and MySQL.
	// (for mysql it equals MySQLDB); the artifact metadata store reuses it.
	art, err := buildArtifactStore(settings.artifact, prod, built.SQLDB, deps)
	if err != nil {
		return nil, err
	}
	var controlTicketKey string
	if settings.artifact != nil {
		controlTicketKey = settings.artifact.DownloadTokenKey
	}
	var localControlTicketKeyPath string
	if settings.harness != nil {
		localControlTicketKeyPath = filepath.Join(settings.harness.Paths.RunDir, "control-ticket.key")
	}
	controlTickets, err := buildControlTicketCodec(prod, controlTicketKey, localControlTicketKeyPath)
	if err != nil {
		return nil, fmt.Errorf("build control ticket codec: %w", err)
	}

	// --- 可选的分布式协调层 (Redis) -----------------------------------------
	// enabled=false 或配置缺失时 rc 为 nil,下游一律回退进程内实现,单实例/CI 不受影响。
	// 但生产环境下若显式启用 Redis 却连接失败,必须 fail closed(不静默回退)。
	rc, redisStoreCfg, err := buildRedis(redisCfg, logger, prod, deps)
	if err != nil {
		return nil, err
	}
	if rc != nil {
		resources.Add(rc.Close)
	}

	// --- model gateway: 按租户构建独立 Facade -------------------------------
	tenantConfigs := modelCfg.TenantConfigs()
	tenantFacades := make(map[string]*modelgateway.Facade, len(tenantConfigs))
	var allNames []string
	tenantProviders := make(map[string][]string, len(tenantConfigs))

	// assembleFacade 把一个 TenantConfig 物化为可执行 Facade(provider 客户端 + 路由
	// + 日志/缓存/产物/用量/配额装配)。启动循环与运行时托管 resolver 共用它,保证
	// 两条路径构建出的 Facade 完全一致。
	assembleFacade := func(tenantID string, tc TenantConfig) (*modelgateway.Facade, []string) {
		facade, pnames := buildFacade(tenantID, tc)
		facade.Logger = logger
		if rc != nil {
			facade.Cache = redisstore.NewModelCache(rc, redisStoreCfg)
		} else {
			facade.Cache = modelgateway.NewMemoryModelCache()
		}
		facade.Output = modelgateway.ArtifactOutputWriter{Store: art}
		facade.UsageLedger = stores.Usage
		if tc.Quota != (modelgateway.TenantQuota{}) {
			quotas := explicitTenantQuotaPolicy(tenantID, tc.Quota)
			if rc != nil {
				facade.Quota = redisstore.NewTenantQuotaManager(rc, redisStoreCfg, quotas, quotaLogAdapter{logger: logger})
			} else {
				facade.Quota = modelgateway.NewMemoryTenantQuotaManager(quotas)
			}
		}
		return facade, pnames
	}

	var managedModelProviders *modeladmin.SQLManagedRegistry
	var managedTenants *tenantadmin.SQLManagedRegistry
	var gateway *modelgateway.TenantGateway
	// 管理面 registry 只要有 SQL 连接就构建（ADR-0005：file 模式下管理接口
	// 照常写库，只是不影响当前运行时）。
	if built.SQLDB != nil {
		managedTenants = tenantadmin.NewSQLManagedRegistry(built.SQLDB)
		managedModelProviders = modeladmin.NewSQLManagedRegistry(built.SQLDB)
	}
	modelsFromDatabase := settings.harness != nil && settings.harness.Components.Models.FromDatabase()
	if modelsFromDatabase {
		// database 模式：数据库是唯一事实源，不做 YAML bootstrap（ADR-0002）；
		// resolver 运行期按租户懒加载，启动期仅枚举 default 租户供日志/巡检。
		if managedModelProviders == nil {
			return nil, fmt.Errorf("components.models: source \"database\" requires a sqlite/mysql storage backend")
		}
		tenantProviders, allNames = managedProviderNames(context.Background(), managedModelProviders, []string{"default"})
		gateway = &modelgateway.TenantGateway{
			Resolver: newManagedModelResolver(managedModelProviders, assembleFacade, logger),
		}
	} else {
		// file 模式：只用启动文件配置构建静态 Facade，运行时完全不读
		// model_providers 表（ADR-0001）。
		for tenantID, tc := range tenantConfigs {
			facade, pnames := assembleFacade(tenantID, tc)
			tenantFacades[tenantID] = facade
			tenantProviders[tenantID] = pnames
			allNames = appendUnique(allNames, pnames)
		}
		// TenantGateway: 按租户 ID 分发;Default 兜底。
		var defaultFacade *modelgateway.Facade
		if f, ok := tenantFacades["default"]; ok {
			defaultFacade = f
		} else if len(tenantFacades) == 1 {
			for _, f := range tenantFacades {
				defaultFacade = f
			}
		}
		gateway = &modelgateway.TenantGateway{
			Tenants: tenantFacades,
			Default: defaultFacade,
		}
	}
	logger.Info(context.Background(), "model gateway: tenant-aware routing",
		observability.String("tenants", fmt.Sprintf("%d", len(tenantProviders))))

	// --- runtime + storage services -------------------------------------------
	state := runtimestore.NewBridge(stores)
	writer := runtimeStorageWriter(state, stores, art)

	// --- protocol layer -------------------------------------------------------
	var broker protocol.EventBroker
	var hotBuffer protocol.HotStreamBuffer
	if rc != nil {
		broker = redisstore.NewBroker(rc, redisStoreCfg)
		hotBuffer = redisstore.NewHotBuffer(rc, redisStoreCfg)
	} else {
		broker = protocol.NewMemoryBroker()
		hotBuffer = protocolmem.NewHotBuffer()
	}
	// EventObserver 扩展在事件发布点做服务端 fan-out（R3）：包装一次即覆盖
	// 所有 Publish 方与所有入口（HTTP/SDK/Resume/Subscribe），不依赖客户端 drain。
	broker = newObservedBroker(broker, settings.extensions, logger)

	// Redis owns only ephemeral coordination and hot projections. The canonical
	// ContextSnapshot remains Artifact-backed so restart/resume never depends on
	// a cache TTL. Tests may still inject an explicit SnapshotStore override.
	// The formal harness composition is the only supported execution path. There
	// is no ad-hoc fallback runtime (gatewayRuntime/runDispatcher were removed):
	// without a harness config the app refuses to build (fail closed) rather than
	// silently running a degraded, single-turn, contract-incomplete path.
	if settings.harness == nil {
		return nil, fmt.Errorf("app: harness composition config is required (pass WithHarnessConfig)")
	}
	// models=database 时 token 预算才从托管库读取；file 模式回退到启动文件
	// 配置的预算分配器（ADR-0001：file 模式运行时不读库）。
	var tokenBudgets agentruntime.TokenBudgetAllocator
	if modelsFromDatabase {
		tokenBudgets = managedTokenBudgetAllocator(managedModelProviders)
	}
	formal, buildErr := buildFormalComposition(
		context.Background(), *settings.harness, modelCfg, stores, art, state,
		newGatewayModelInvoker(gateway, art), writer, broker, hotBuffer, logger, tracer,
		formalCompositionDependencies{
			database:            built.MySQLDB,
			controlDatabase:     built.SQLDB,
			agentsFromDatabase:  settings.harness.Components.Agents.FromDatabase(),
			promptsFromDatabase: settings.harness.Components.Prompts.FromDatabase(),
			contextSnapshots:    settings.contextSnapshots,
			controlTickets:      controlTickets,
			tokenBudgets:        tokenBudgets,
			extensions:          settings.extensions,
		},
	)
	if buildErr != nil {
		return nil, buildErr
	}
	runSvc := formal.runs
	dispatch := server.RunDispatcher(formal.dispatcher)
	agentRegistry := formal.registry // feeds the debug console agent viewer
	// 正式 Dispatcher 同时承接首次执行和基于冻结配置的 Resume。
	ctrl := control.New(stores, formal.dispatcher)
	serverDeps := server.Deps{
		Stores:         stores,
		RunService:     runSvc,
		DefaultAgentID: settings.harness.Runtime.DefaultAgentID,
		// HTTP 入口与 SDK 入口共享同一条预回合扩展管线（R2a）；hosted 无扩展
		// 时 HasStages 为 false，handler 零成本直通。
		TurnPreparer: kernel.NewTurnPipeline(settings.extensions, kernel.TurnEnvironment{
			Environment:    string(settings.harness.Environment),
			SDKVersion:     kernel.SDKContractVersion,
			SchemaVersions: kernel.CanonicalSchemaVersions(),
		}),
		Control:            ctrl,
		ControlTickets:     controlTickets,
		Artifacts:          art,
		Broker:             broker,
		HotBuffer:          hotBuffer,
		Dispatcher:         dispatch,
		Logger:             logger,
		AgentConfigSource:  formal.registry,
		AgentConfigControl: formal.configControl,
		Prompts:            formal.prompts,
		ConfigEnvironment:  agentregistry.ConfigEnvironment(settings.harness.Environment),
		ContextSnapshots:   formal.snapshots,
		ToolCatalog:        formal.toolRegistry,
	}
	if built.SQLDB != nil {
		serverDeps.ManagedMCP = formal.managedMCP
		serverDeps.MCPDebug = formal.mcpService
		serverDeps.MCPOAuth = formal.mcpOAuth
		serverDeps.ManagedSkills = formal.skillService
		serverDeps.ToolConfig = formal.toolConfig
		serverDeps.HTTPTools = formal.httpTools
		serverDeps.ManagedModelProviders = managedModelProviders
		serverDeps.Tenants = managedTenants
		serverDeps.ValidateModelProvider = validateProviderBuildable
	}
	if canceller, ok := dispatch.(server.RunCanceller); ok {
		serverDeps.Canceller = canceller
	}

	auth, secure, err := NewAuthenticator(authCfg)
	if err != nil {
		return nil, err
	}
	// Resolve tenant from the secret-key login cookie ahead of header/JWT auth,
	// so the console's key login/switch drives tenant isolation for all APIs.
	if managedTenants != nil {
		auth = &TenantCookieAuthenticator{Base: auth, Resolver: managedTenants}
	}
	if !secure {
		logger.Warn(context.Background(), "auth: INSECURE header mode enabled by configuration; request identity headers are trusted")
	} else {
		logger.Info(context.Background(), "auth: JWT bearer mode enabled")
	}
	serverDeps.LocalTenantSwitch = !secure

	// --- debug introspection: redacted config snapshot + log query ------------
	authMode := "insecure"
	if secure {
		authMode = "jwt"
	}
	storageBackend := storageCfg.Backend
	if storageBackend == "" {
		storageBackend = "sqlite"
	}
	sysInfo := buildSystemInfo(authMode, storageBackend, os.Getenv("HARNESS_ENV"), rc != nil, tenantConfigs, settings.artifact, settings.catalogs...)
	if raw, err := json.Marshal(sysInfo); err == nil {
		serverDeps.System = raw
	}
	if q, ok := logger.(observability.LogQuerier); ok {
		serverDeps.LogQuery = q
	}
	// Agent viewer for the debug console: a read-only snapshot of each registered
	// agent's effective config (prompt/tools/subagents) resolved at startup.
	serverDeps.AgentCatalog, serverDeps.AgentConfigs = buildAgentDebugCatalog(context.Background(), agentRegistry, logger)

	application = &App{
		Handler: authTraceMiddlewareWithOptions(auth, server.NewRouter(serverDeps), authTraceOptions{
			TestingUIBypass: settings.harness.Environment == EnvironmentTesting,
			Tenants:         managedTenants,
		}),
		Stores:            stores,
		Artifacts:         art,
		Gateway:           gateway,
		Providers:         allNames,
		Tenants:           tenantProviders,
		RunService:        runSvc,
		RunEntry:          formal.dispatcher,
		Broker:            broker,
		HotBuffer:         hotBuffer,
		Registry:          agentRegistry,
		Control:           ctrl,
		ControlTickets:    controlTickets,
		DefaultAgentID:    settings.harness.Runtime.DefaultAgentID,
		Environment:       settings.harness.Environment,
		BoundToolHandlers: formal.boundToolHandlers,
		closeResources:    resources.Close,
		runLifecycle:      formal.lifecycle,
		embedded:          newEmbeddedEngine(runSvc, stores, formal.dispatcher, broker, ctrl, controlTickets, art),
	}
	succeeded = true
	return application, nil
}

func databaseStorageBackend(backend string) bool {
	switch backend {
	case "sqlite", "mysql":
		return true
	default:
		return false
	}
}

// isProductionEnv reports whether the process runs in a production-like
// environment, where fail-open fallbacks are forbidden. Development backends
// require both a recognized non-production environment and an explicit opt-in;
// an unset or misspelled environment therefore fails safe.
func isProductionEnv() bool {
	if strings.TrimSpace(os.Getenv("HARNESS_ALLOW_DEV_BACKENDS")) != "1" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HARNESS_ENV"))) {
	case "local", "test", "ci", "dev", "development":
		return false
	}
	return true
}

const artifactTokenSecretBytes = 32

const artifactDownloadBase = "/api/v1/artifacts"

// buildDownloadCodec builds the AEAD codec that mints and redeems stateless
// artifact download tokens. 线上实例必须共享部署侧密钥；本地未配置时只生成
// 当前进程有效的随机密钥，避免源码内置密钥被用于伪造 bearer token。
func buildDownloadCodec(production bool, configuredSecret string) (*downloadtoken.Codec, error) {
	secret := strings.TrimSpace(configuredSecret)
	if secret == "" {
		if production {
			return nil, fmt.Errorf("artifact download token key is required outside local environment")
		}
		ephemeral := make([]byte, artifactTokenSecretBytes)
		if _, err := rand.Read(ephemeral); err != nil {
			return nil, fmt.Errorf("generate ephemeral artifact token key: %w", err)
		}
		return downloadtoken.New(ephemeral)
	}
	if len([]byte(secret)) < artifactTokenSecretBytes {
		return nil, fmt.Errorf("artifact download token key must contain at least %d bytes", artifactTokenSecretBytes)
	}
	return downloadtoken.New([]byte(secret))
}

// buildControlTicketCodec shares the deployment root secret with artifact
// downloads, while controlticket.New derives a domain-separated AEAD key. A
// formal local Harness persists a private generated key in its runtime run
// directory so pending ControlRequests remain answerable after process restart.
func buildControlTicketCodec(production bool, configuredSecret, localKeyPath string) (*controlticket.Codec, error) {
	secret := strings.TrimSpace(configuredSecret)
	if secret == "" {
		if production {
			return nil, fmt.Errorf("control ticket key is required outside local environment")
		}
		if localKeyPath == "" {
			ephemeral := make([]byte, artifactTokenSecretBytes)
			if _, err := rand.Read(ephemeral); err != nil {
				return nil, fmt.Errorf("generate ephemeral control ticket key: %w", err)
			}
			return controlticket.New(ephemeral)
		}
		localSecret, err := loadOrCreateLocalControlTicketKey(localKeyPath)
		if err != nil {
			return nil, err
		}
		return controlticket.New(localSecret)
	}
	if len([]byte(secret)) < artifactTokenSecretBytes {
		return nil, fmt.Errorf("control ticket key must contain at least %d bytes", artifactTokenSecretBytes)
	}
	return controlticket.New([]byte(secret))
}

func loadOrCreateLocalControlTicketKey(path string) ([]byte, error) {
	if key, err := readLocalControlTicketKey(path); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create local control ticket key directory: %w", err)
	}
	key := make([]byte, artifactTokenSecretBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate local control ticket key: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".control-ticket-key-*")
	if err != nil {
		return nil, fmt.Errorf("create local control ticket key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("secure local control ticket key: %w", err)
	}
	if _, err := temporary.Write(key); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write local control ticket key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync local control ticket key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close local control ticket key: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("install local control ticket key: %w", err)
	}
	return readLocalControlTicketKey(path)
}

func readLocalControlTicketKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("invalid local control ticket key file permissions or type")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read local control ticket key: %w", err)
	}
	if len(key) != artifactTokenSecretBytes {
		return nil, fmt.Errorf("invalid local control ticket key length")
	}
	return key, nil
}

func validateArtifactDownloadBase(raw string) error {
	if raw == "" || raw != strings.TrimSpace(raw) || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return fmt.Errorf("artifact download base must be a same-origin absolute path")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(raw, "\\") {
		return fmt.Errorf("artifact download base must be a same-origin absolute path")
	}
	if strings.TrimRight(parsed.Path, "/") != artifactDownloadBase {
		return fmt.Errorf("artifact download base must resolve to the registered route %s", artifactDownloadBase)
	}
	return nil
}

func buildArtifactStore(cfg *ArtifactConfig, production bool, sharedDB *sql.DB, deps buildDependencies) (*artifact.Store, error) {
	var downloadTokenKey string
	if cfg != nil {
		downloadTokenKey = cfg.DownloadTokenKey
	}
	codec, err := buildDownloadCodec(production, downloadTokenKey)
	if err != nil {
		return nil, fmt.Errorf("build artifact download codec: %w", err)
	}
	if cfg == nil {
		if production {
			return nil, fmt.Errorf("artifact config is required outside local environment")
		}
		objects := objmem.NewMemory()
		return artifact.NewStore(artifact.StoreConfig{ObjectStore: objects, MetadataStore: metamem.NewMemory(), DownloadCodec: codec}), nil
	}
	var objects artifact.ObjectStore
	switch cfg.ObjectBackend {
	case "memory":
		store := objmem.NewMemory()
		objects = store
	case "file":
		store, err := objmem.NewFile(cfg.Root, cfg.DownloadBase)
		if err != nil {
			return nil, fmt.Errorf("artifact file store: %w", err)
		}
		objects = store
	default:
		return nil, fmt.Errorf("unsupported artifact object backend %q", cfg.ObjectBackend)
	}
	var metadata artifact.MetadataStore
	switch cfg.MetadataBackend {
	case "memory":
		metadata = metamem.NewMemory()
	case "sqlite":
		// Durable local metadata: without it, every restart orphans in-flight
		// runs (context snapshots become unresolvable and Resume fails with
		// CONTEXT_SNAPSHOT_UNAVAILABLE). Reuses the shared local SQLite pool.
		if sharedDB == nil {
			return nil, fmt.Errorf("artifact metadata backend %q requires a sqlite storage backend", cfg.MetadataBackend)
		}
		store, err := metamem.NewSQLiteMetadataStore(sharedDB)
		if err != nil {
			return nil, fmt.Errorf("artifact sqlite metadata store: %w", err)
		}
		metadata = store
	case "mysql":
		if sharedDB == nil {
			return nil, fmt.Errorf("artifact metadata backend %q requires a mysql storage backend", cfg.MetadataBackend)
		}
		store, err := deps.newArtifactMetadata(sharedDB)
		if err != nil {
			return nil, fmt.Errorf("artifact mysql metadata store: %w", err)
		}
		metadata = store
	default:
		return nil, fmt.Errorf("artifact metadata backend %q adapter is not installed", cfg.MetadataBackend)
	}
	storeConfig := artifact.StoreConfig{ObjectStore: objects, MetadataStore: metadata, MaxObjectBytes: cfg.MaxObjectBytes, DownloadCodec: codec}
	return artifact.NewStore(storeConfig), nil
}

// buildRedis 把 RedisConfig 翻译成 redisstore 的连接 + 配置。enabled=false、地址为空
// 时返回 nil client,调用方回退到进程内实现(单实例/CI 无感)。显式启用 Redis 后
// Ping 失败一律 fail closed,避免配置看似生效但实际静默退回进程内协调。
func buildRedis(cfg RedisConfig, logger observability.StructuredLogger, prod bool, deps buildDependencies) (redisstore.Client, redisstore.Config, error) {
	_ = prod
	if !cfg.Enabled || len(cfg.Addrs) == 0 {
		return nil, redisstore.Config{}, nil
	}
	storeCfg := redisstore.Config{
		Addrs:          cfg.Addrs,
		Username:       cfg.Username,
		Password:       cfg.Password,
		DB:             cfg.DB,
		KeyPrefix:      cfg.KeyPrefix,
		CacheTTL:       time.Duration(cfg.CacheTTLs) * time.Second,
		HotTTL:         time.Duration(cfg.HotTTLs) * time.Second,
		SnapshotTTL:    time.Duration(cfg.SnapshotTTLs) * time.Second,
		QuotaLeaseTTL:  time.Duration(cfg.QuotaLeaseTTLs) * time.Second,
		QuotaCommitTTL: time.Duration(cfg.QuotaCommitTTLs) * time.Second,
		QuotaFailOpen:  cfg.QuotaFailOpen,
	}
	client := deps.newRedisClient(storeCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := deps.pingRedis(ctx, client); err != nil {
		primary := fmt.Errorf("redis enabled but ping failed: %w", err)
		if closeErr := client.Close(); closeErr != nil {
			primary = errors.Join(primary, fmt.Errorf("close failed Redis client: %w", closeErr))
		}
		return nil, redisstore.Config{}, primary
	}
	logger.Info(context.Background(), "redis: distributed coordination enabled", observability.Any("addrs", cfg.Addrs))
	return client, storeCfg, nil
}

// quotaLogAdapter bridges the Composition Root logger to the narrow Redis
// quota logging contract without coupling redisstore to observability.
type quotaLogAdapter struct {
	logger observability.StructuredLogger
}

func (a quotaLogAdapter) Warnf(ctx context.Context, format string, args ...interface{}) {
	a.logger.Warn(ctx, fmt.Sprintf(format, args...))
}

// explicitTenantQuotaPolicy scopes each facade to the policy selected by tenant
// routing. When the default facade serves an otherwise unknown tenant, quota
// enforcement uses the default bucket rather than minting a bucket per caller.
func explicitTenantQuotaPolicy(tenantID string, quota modelgateway.TenantQuota) map[string]modelgateway.TenantQuota {
	return map[string]modelgateway.TenantQuota{tenantID: quota}
}

// buildFacade 为单个租户构建 Facade(独立 provider 注册表 + 路由)。
func buildFacade(tenantID string, tc TenantConfig) (*modelgateway.Facade, []string) {
	providers := map[string]modelgateway.ChatProvider{}
	var names []string

	for _, p := range tc.Providers {
		if p.Protocol == "scenario_mock" {
			providers[p.Name] = mock.NewScenario(p.Name)
			names = append(names, p.Name)
			continue
		}
		if p.Protocol == "mock" {
			providers[p.Name] = mock.New(p.Name, mock.Script{Chunks: []modelgateway.NormalizedChunk{mock.TokenChunk("你好，"), mock.TokenChunk("我是 mock 模型的流式回复。"), mock.UsageChunk(12, 8)}})
			names = append(names, p.Name)
			continue
		}
		if p.Name == "" || p.BaseURL == "" || p.APIKey == "" {
			continue // skip incomplete entries
		}
		switch p.Protocol {
		case "anthropic":
			var options []anthropic.Option
			if p.PromptCacheSessionAffinity && modelgateway.PromptCacheSessionAffinityAllowed(p.ProviderKind, p.Protocol, p.BaseURL) {
				options = append(options, anthropic.WithPromptCacheSessionAffinity())
			}
			providers[p.Name] = anthropic.New(p.Name, p.BaseURL, p.APIKey, options...)
		case "", "openai_compatible":
			providers[p.Name] = openai.New(p.Name, p.BaseURL, p.APIKey)
		case "dashscope_rerank":
			// DashScope 原生 rerank 协议（非 compatible-mode）：只服务
			// Facade.Rerank，chat 调用 fail closed。
			providers[p.Name] = dashscope.NewRerank(p.Name, p.BaseURL, p.APIKey)
		default:
			continue
		}
		names = append(names, p.Name)
	}

	defProvider, defModel := tc.DefaultProvider, tc.DefaultModel
	if defProvider == "" || providers[defProvider] == nil || defModel == "" {
		return &modelgateway.Facade{Providers: providers}, names
	}
	capByProvider := map[string]modelgateway.ModelCapability{}
	for _, p := range tc.Providers {
		if p.Name != "" {
			cap := p.Capability
			if cap.Model == "" && len(p.Models) > 0 {
				cap.Model = p.Models[0]
			}
			if cap.Provider == "" {
				cap.Provider = p.Name
			}
			if !cap.Chat {
				cap.Chat = true
			}
			if !cap.Streaming {
				cap.Streaming = true
			}
			if !cap.ToolCalling {
				cap.ToolCalling = true
			}
			if !cap.StructuredOutput {
				cap.StructuredOutput = true
			}
			if !cap.JSONSchema {
				cap.JSONSchema = true
			}
			capByProvider[p.Name] = cap
		}
	}
	capabilityFor := func(providerName, model string) modelgateway.ModelCapability {
		for _, provider := range tc.Providers {
			if provider.Name == providerName {
				return capabilityForConfiguredModel(provider, model)
			}
		}
		cap := capByProvider[providerName]
		cap.Model = model
		return cap
	}

	// 路由:primary = 默认 provider/模型;其余每个 (provider, model) 组合都建成可路由
	// target。这样 ModelHint 能命中任一声明过的模型(命中不到才 fail-closed 报错);
	// 无 hint 时它们作为降级链。
	var fallback []modelgateway.ModelTarget
	for _, p := range tc.Providers {
		if p.Name == "" || providers[p.Name] == nil {
			continue
		}
		models := p.Models
		if len(models) == 0 {
			models = []string{defModel}
		}
		for _, model := range models {
			if p.Name == defProvider && model == defModel {
				continue // 已作为 Primary
			}
			cap := capabilityFor(p.Name, model)
			fallback = append(fallback, modelgateway.ModelTarget{Model: model, Provider: p.Name, Cost: p.Costs[model], Capability: cap})
		}
	}
	primaryCap := capabilityFor(defProvider, defModel)

	facade := &modelgateway.Facade{
		Router: modelgateway.NewStaticRouter(
			modelgateway.Route{
				Primary:  modelgateway.ModelTarget{Model: defModel, Provider: defProvider, Cost: providerCost(tc.Providers, defProvider, defModel), Capability: primaryCap},
				Fallback: fallback,
			},
			nil,
		),
		Providers: providers,
	}
	return facade, names
}

func providerCost(providers []ProviderConfig, providerName, model string) modelgateway.ModelCostTable {
	for _, provider := range providers {
		if provider.Name == providerName {
			return provider.Costs[model]
		}
	}
	return modelgateway.ModelCostTable{}
}

func appendUnique(dst []string, items []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range items {
		if !seen[s] {
			dst = append(dst, s)
			seen[s] = true
		}
	}
	return dst
}

func headerOr(r *http.Request, key, def string) string {
	if v := r.Header.Get(key); v != "" {
		return v
	}
	return def
}

// buildStores 根据 StorageConfig 选择存储后端实现及其幂等关闭函数。配置的后端
// 初始化失败一律 fail closed；memory 仅在明确选择且处于本地环境时可用。
// 连接池默认由本函数根据 StorageConfig 创建并拥有（managed）。
// Embedded SDK 的受控外部连接池路径由 buildStoresWithSharedSQLDatabase 处理。
func buildStores(ctx context.Context, sc StorageConfig, logger observability.StructuredLogger, prod bool, deps buildDependencies) (builtStores, error) {
	return buildStoresWithSharedSQLDatabase(ctx, sc, logger, prod, deps, nil)
}

func buildStoresWithSharedSQLDatabase(ctx context.Context, sc StorageConfig, logger observability.StructuredLogger, prod bool, deps buildDependencies, sharedDB *sql.DB) (builtStores, error) {
	// A configured backend never silently changes semantics. Local memory is
	// available only when it is selected explicitly.
	fallbackOrFail := func(reason string, cause error) (builtStores, error) {
		return builtStores{}, fmt.Errorf("storage backend %q init failed (%s): %w", sc.Backend, reason, cause)
	}
	if sharedDB != nil && sc.Backend != "sqlite" && sc.Backend != "mysql" {
		return fallbackOrFail("external SQL database incompatible", errExternalSQLDatabaseBackend)
	}
	switch sc.Backend {
	case "sqlite":
		var c SQLiteConfig
		if len(sc.Config) > 0 {
			if err := decodeStrictRawConfig(sc.Config, &c); err != nil {
				return fallbackOrFail("sqlite config invalid", err)
			}
		}
		if c.Path == "" {
			return fallbackOrFail("sqlite path missing", fmt.Errorf("storage.config.path is required for sqlite backend"))
		}
		if sharedDB != nil {
			backend := storagesqlite.New(sharedDB)
			if err := backend.CheckReady(ctx); err != nil {
				return fallbackOrFail("sqlite schema readiness failed", err)
			}
			logger.Info(context.Background(), "storage backend: external sql",
				observability.String("backend", sc.Backend),
				observability.String("ownership", "caller"),
			)
			return builtStores{
				Stores: backend.Stores(),
				SQLDB:  sharedDB,
				Close:  func() error { return nil },
			}, nil
		}
		if err := os.MkdirAll(filepath.Dir(c.Path), 0o750); err != nil {
			return fallbackOrFail("sqlite directory creation failed", err)
		}
		db, err := storagesqlite.OpenDatabase(c.Path)
		if err != nil {
			return fallbackOrFail("sqlite open failed", err)
		}
		sq := storagesqlite.New(db)
		closeSQLite := newStableCloser(sq.Close)
		if err := sq.CheckReady(ctx); err != nil {
			primary := fmt.Errorf("storage backend %q init failed (sqlite schema readiness failed): %w", sc.Backend, err)
			if closeErr := closeSQLite(); closeErr != nil {
				primary = errors.Join(primary, fmt.Errorf("close sqlite after readiness failure: %w", closeErr))
			}
			return builtStores{}, primary
		}
		logger.Info(context.Background(), "storage backend: sqlite", observability.String("path", c.Path))
		return builtStores{Stores: sq.Stores(), SQLDB: db, Close: closeSQLite}, nil
	case "mysql":
		var c MySQLConfig
		if len(sc.Config) > 0 {
			if err := decodeStrictRawConfig(sc.Config, &c); err != nil {
				return fallbackOrFail("mysql config invalid", err)
			}
		}
		if err := validateMySQLBackendConfig(sc.Backend, c, prod); err != nil {
			return fallbackOrFail("mysql config invalid", err)
		}
		config, err := c.toFrameworkConfig()
		if err != nil {
			return fallbackOrFail("mysql config invalid", err)
		}
		if sharedDB != nil {
			backend := deps.newMySQLBackend(sharedDB, sc.Backend, c.SkipMigrationLedgerCheck)
			if backend == nil {
				return fallbackOrFail("mysql backend missing", errors.New("mysql backend factory returned nil"))
			}
			if err := backend.CheckReady(ctx); err != nil {
				return fallbackOrFail("mysql schema readiness failed", err)
			}
			logger.Info(context.Background(), "storage backend: external sql",
				observability.String("backend", sc.Backend),
				observability.String("ownership", "caller"),
			)
			return builtStores{
				Stores:  backend.Stores(),
				MySQLDB: sharedDB,
				SQLDB:   sharedDB,
				Close:   func() error { return nil },
			}, nil
		}
		if err := deps.initMySQL(config); err != nil {
			return fallbackOrFail("mysql init failed", err)
		}
		db := deps.getMySQL(config.Name)
		if db == nil {
			return fallbackOrFail("mysql get failed", errors.New("mysql getter returned a nil pool"))
		}
		closeMySQL := newStableCloser(db.Close)
		backend := deps.newMySQLBackend(db, sc.Backend, c.SkipMigrationLedgerCheck)
		if backend == nil {
			primary := fmt.Errorf("storage backend %q init failed (mysql backend missing)", sc.Backend)
			if closeErr := closeMySQL(); closeErr != nil {
				primary = errors.Join(primary, fmt.Errorf("close mysql after backend failure: %w", closeErr))
			}
			return builtStores{}, primary
		}
		if err := backend.CheckReady(ctx); err != nil {
			primary := fmt.Errorf("storage backend %q init failed (mysql schema readiness failed): %w", sc.Backend, err)
			if closeErr := closeMySQL(); closeErr != nil {
				primary = errors.Join(primary, fmt.Errorf("close mysql after readiness failure: %w", closeErr))
			}
			return builtStores{}, primary
		}
		logger.Info(context.Background(), "storage backend: mysql")
		return builtStores{Stores: backend.Stores(), MySQLDB: db, SQLDB: db, Close: closeMySQL}, nil
	case "memory", "":
		if prod {
			return builtStores{}, fmt.Errorf("memory storage backend is not allowed in production; configure sqlite or mysql")
		}
		logger.Info(context.Background(), "storage backend: memory")
		return builtStores{Stores: storagemem.New().Stores(), Close: newStableCloser(nil)}, nil
	default:
		return builtStores{}, fmt.Errorf("unknown storage backend %q", sc.Backend)
	}
}
