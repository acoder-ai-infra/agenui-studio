package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/bootstrap"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	dkRuntime "github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/runtime"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/exportpackage"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/integration/operatorruntime"
	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/ruleworker"
	httptransport "github.com/AGenUI/agenui-studio/agenui-agent/internal/transport/http"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/localadmin"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/localmodel"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	_ "github.com/go-sql-driver/mysql"
	"gopkg.in/natefinch/lumberjack.v2"
	_ "modernc.org/sqlite"
)

func main() {
	configPath := flag.String("config", "", "path to immutable AGenUI TOML config")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	for {
		err := run(ctx, *configPath)
		if errors.Is(err, errRestartRequested) {
			log.Printf("[agenui-server] restarting after local administration request")
			continue
		}
		if err != nil {
			log.Fatalf("agenui-agent: %v", err)
		}
		return
	}
}

var errRestartRequested = errors.New("agenui-agent: restart requested")

func run(parentCtx context.Context, configPath string) error {
	ctx, cancelRun := context.WithCancel(parentCtx)
	defer cancelRun()
	restartRequests := make(chan struct{}, 1)
	var restartQueued atomic.Bool
	config, err := bootstrap.LoadProcessConfig(configPath)
	if err != nil {
		return err
	}
	// Header identity is deliberately limited to local/test profiles. A real
	// deployment must inject an authenticated PrincipalResolver instead of
	// trusting caller-controlled headers.
	switch config.Bootstrap.Environment {
	case "local", "testing", "staging":
	case "production":
		return errors.New("agenui-agent: production requires an authenticated PrincipalResolver; the local header resolver is disabled")
	default:
		return fmt.Errorf(
			"agenui-agent: unsupported environment %q", config.Bootstrap.Environment,
		)
	}
	hostLog, err := configureHostLog(config.Bootstrap.Environment)
	if err != nil {
		return err
	}
	if hostLog != nil {
		defer hostLog.Close()
	}

	appDB, err := openApplicationDB(ctx, config)
	if err != nil {
		return err
	}
	if appDB != nil {
		defer appDB.Close()
	}
	// Publish the saved local connection into Harness' managed provider store
	// before startup. Later saves update the same store and apply to the next Run.
	if config.Bootstrap.Environment == "local" && appDB != nil {
		prepared, configured, prepareErr := localmodel.Prepare(
			ctx, appDB, config.Bootstrap.HarnessConfigPath,
		)
		if prepareErr != nil {
			return fmt.Errorf("agenui-agent: prepare local model configuration: %w", prepareErr)
		}
		if configured {
			config.Bootstrap.HarnessConfigPath = prepared.HarnessConfigPath
			log.Printf("[agenui-model] loaded local user model configuration for Harness Agents")
		} else {
			log.Printf("[agenui-model] no local model configuration yet; management UI is available and model calls will remain unavailable until configured")
		}
	}
	var designKnowledgeManager *dkRuntime.Manager
	var designKnowledgeProvider dkRuntime.RepositoryProvider
	if config.DesignKnowledgeRoot != "" {
		designKnowledgeManager, err = dkRuntime.NewLocalFallbackManager(
			ctx,
			os.DirFS(config.DesignKnowledgeRoot),
			config.DesignKnowledgeRevision,
			dkRuntime.ManagerOptions{
				RetainRevisions: config.DesignKnowledgeWatch.RetainRevisions,
			},
		)
		if err != nil {
			return fmt.Errorf("agenui-agent: load design knowledge: %w", err)
		}
		logDesignKnowledgeActive("designknowledge.local.load.success", designKnowledgeManager.CurrentInfo())
		if err := activateLocalRuleRevision(ctx, config, designKnowledgeManager, "startup"); err != nil {
			return err
		}
		designKnowledgeProvider = designKnowledgeManager
		receiptKey, receiptErr := localReceiptKey(config)
		if receiptErr != nil {
			return receiptErr
		}
		if len(receiptKey) >= 32 {
			config.Bootstrap.DesignKnowledgeReceiptKey = []byte(receiptKey)
		}
	}
	dependencies := bootstrap.Dependencies{
		ResolvePrincipal:        developmentPrincipalResolver,
		DesignKnowledgeProvider: designKnowledgeProvider,
	}
	if err := configureOperatorRuntime(config, &dependencies); err != nil {
		return err
	}
	// Harness and the Studio management plane intentionally use one SQL pool.
	// This is required for local SQLite: opening the same file through mattn and
	// modernc in one process can corrupt POSIX lock ownership and surface as
	// SQLITE_IOERR_LOCK on otherwise ordinary model-config reads/writes.
	if appDB != nil && (config.DatabaseDriver == "sqlite3" || config.DatabaseDriver == "mysql") {
		dependencies.ExtensionOptions = append(
			dependencies.ExtensionOptions,
			harness.WithSharedSQLDatabase(appDB),
		)
		log.Printf("[agenui-storage] shared %s connection enabled for Harness", config.DatabaseDriver)
	}
	if designKnowledgeManager != nil && config.DesignKnowledgeWatch.Enabled {
		startLocalRevisionWatcher(ctx, config, designKnowledgeManager)
	}
	app, err := bootstrap.Build(ctx, config.Bootstrap, dependencies)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	app.RegisterHTTP(mux)
	if config.Bootstrap.Environment == "local" && appDB != nil {
		admin, err := localadmin.RegisterWithDesignKnowledgeSource(
			mux, appDB,
			config.DesignKnowledgeRoot, config.DesignKnowledgeRevision,
			localRuleWorkerRevisionRoot(config), localRuleWorkerPointerPath(config),
		)
		if err != nil {
			return fmt.Errorf("agenui-agent: register local administration: %w", err)
		}
		serviceInstanceID, instanceErr := newServiceInstanceID()
		if instanceErr != nil {
			return fmt.Errorf("agenui-agent: create service instance id: %w", instanceErr)
		}
		admin.SetRestartRequester(serviceInstanceID, func() error {
			if !restartQueued.CompareAndSwap(false, true) {
				return errors.New("Agent restart is already in progress")
			}
			restartRequests <- struct{}{}
			return nil
		})
		publisher, err := exportpackage.NewPublisher(admin, exportpackage.NewCallbackDelivery(nil))
		if err != nil {
			return fmt.Errorf("agenui-agent: configure package publication: %w", err)
		}
		app.Handler.SetPackagePublisher(publisher)
	}
	// Assemble the worker and its lock-admin route before serving;
	// registering routes after ListenAndServe starts would race with requests.
	stopRuleWorker := startRuleWorker(ctx, config, configPath, appDB, designKnowledgeManager, app.Engine, mux, nil)
	server := &http.Server{
		Addr:              config.ListenAddress,
		Handler:           httptransport.CORS(config.AllowedOrigins, mux),
		ReadHeaderTimeout: config.ReadHeaderTimeout,
	}
	// Keep one host-level startup record independent of Harness internals. This
	// makes a successful local startup visible even when the server is idle;
	// request/run diagnostics continue to use the logging policy in harness.yaml.
	log.Printf(
		"[agenui-server] starting environment=%s address=%s harness_config=%s",
		config.Bootstrap.Environment,
		config.ListenAddress,
		config.Bootstrap.HarnessConfigPath,
	)
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	var serveErr error
	restartRequested := false
	select {
	case <-ctx.Done():
	case <-restartRequests:
		restartRequested = true
		cancelRun()
	case serveErr = <-serverErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	workerStopCtx, workerStopCancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	workerStopErr := stopRuleWorker(workerStopCtx)
	workerStopCancel()

	closeCtx, closeCancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	defer closeCancel()
	closeErr := app.Close(closeCtx)
	if combinedErr := errors.Join(serveErr, shutdownErr, workerStopErr, closeErr); combinedErr != nil {
		return combinedErr
	}
	if restartRequested {
		return errRestartRequested
	}
	return nil
}

func newServiceInstanceID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// localReceiptKey preserves fail-closed behavior outside local mode while
// making the clone-and-run profile self-contained on its first launch.
func localReceiptKey(config bootstrap.ProcessConfig) (string, error) {
	if value := os.Getenv(config.DesignKnowledgeReceiptKeyEnv); len(value) >= 32 {
		return value, nil
	}
	if config.Bootstrap.Environment != "local" {
		return "", nil
	}
	path := filepath.Join("var", "local-secrets", "design-knowledge-receipt.key")
	if raw, err := os.ReadFile(path); err == nil && len(raw) >= 32 {
		return string(raw), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("agenui-agent: create local receipt directory: %w", err)
	}
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agenui-agent: generate local receipt key: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("agenui-agent: persist local receipt key: %w", err)
	}
	return value, nil
}

func configureOperatorRuntime(
	config bootstrap.ProcessConfig,
	dependencies *bootstrap.Dependencies,
) error {
	if dependencies == nil {
		return errors.New("agenui-agent: dependencies are required")
	}
	if !config.UsesOperatorRuntime() {
		return nil
	}
	operatorDetailClient, err := platformoperator.NewHTTPDetailClient(
		platformoperator.HTTPDetailClientConfig{Endpoint: config.OperatorDetailEndpoint},
	)
	if err != nil {
		return fmt.Errorf("agenui-agent: build operator detail client: %w", err)
	}
	dependencies.OperatorDetails = operatorDetailClient
	operatorService, err := platformoperator.NewService(
		operatorDetailClient,
		platformoperator.DefaultRegistry(),
	)
	if err != nil {
		return fmt.Errorf("agenui-agent: build operator service: %w", err)
	}
	operatorRuntime, err := operatorruntime.New(operatorService)
	if err != nil {
		return fmt.Errorf("agenui-agent: build operator runtime adapter: %w", err)
	}
	dependencies.OperatorRuntimePort = operatorRuntime
	return nil
}

func configureHostLog(environment string) (io.Closer, error) {
	if environment != "local" {
		return nil, nil
	}
	logDir := filepath.Join("var", "harness", "log")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return nil, fmt.Errorf("agenui-agent: create local host log directory: %w", err)
	}
	rotating := &lumberjack.Logger{
		Filename: filepath.Join(logDir, "agenui-host.log"),
		MaxSize:  100, MaxBackups: 10, MaxAge: 14,
		Compress: true, LocalTime: true,
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stderr, rotating))
	return rotating, nil
}

type stopRuleWorkerFunc func(context.Context) error

func noOpStopRuleWorker(context.Context) error { return nil }

// startRuleWorker launches the in-service layout-rule parse worker as a
// background goroutine tied to ctx. It is enabled by default; any assembly
// failure (missing design tables, no gateway token, etc.) is logged and the
// service continues serving.
func startRuleWorker(ctx context.Context, config bootstrap.ProcessConfig, configPath string, appDB *sql.DB, designKnowledgeManager *dkRuntime.Manager, engine harness.Engine, mux *http.ServeMux, reportAssembled func(bool)) stopRuleWorkerFunc {
	if reportAssembled == nil {
		reportAssembled = func(bool) {}
	}
	rw := config.RuleWorker
	if !rw.Enabled {
		reportAssembled(false)
		log.Printf("[rule-worker] disabled via [rule_worker].enabled=false")
		return noOpStopRuleWorker
	}
	if appDB == nil {
		reportAssembled(false)
		log.Printf("[rule-worker] no SQL state DB (driver=%q); skipped", config.DatabaseDriver)
		return noOpStopRuleWorker
	}
	worker, desc, err := ruleworker.Assemble(ctx, ruleworker.AssembleConfig{
		DB:                  appDB,
		ConfigDir:           filepath.Dir(configPath),
		Engine:              engine,
		OutputRoot:          rw.OutputRoot,
		OutputPrefix:        rw.OutputPrefix,
		PollInterval:        rw.PollInterval,
		WorkerID:            inServiceRuleWorkerID(),
		RendererCatalogRoot: config.RendererCatalogRoot,
		// Revisions are local immutable directories; publication switches the
		// current pointer atomically and activates the same provider in-process.
		BaselineDir:       localRuleWorkerBaseline(config),
		LocalRevisionRoot: localRuleWorkerRevisionRoot(config),
		LocalPointerPath:  localRuleWorkerPointerPath(config),
		LocalRevisionActivator: func(activationCtx context.Context, revisionID string) error {
			return activateLocalRuleRevisionID(activationCtx, config, designKnowledgeManager, revisionID, "publish")
		},
	})
	if err != nil {
		reportAssembled(false)
		log.Printf("[rule-worker] not started (non-fatal): %v", err)
		return noOpStopRuleWorker
	}
	log.Printf("[rule-worker] starting in-service; %s", desc)
	reportAssembled(true)
	if admin, ok := worker.LockAdministrator(); ok && mux != nil {
		ruleworker.RegisterLockAdminHTTP(mux, admin)
		log.Printf("[rule-worker] lock admin route registered path=%s", ruleworker.LockAdminPath)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		err := worker.Run(workerCtx)
		if err != nil {
			log.Printf("[rule-worker] stopped: %v", err)
		}
		done <- err
	}()
	return func(stopCtx context.Context) error {
		reportAssembled(false)
		cancelWorker()
		select {
		case err := <-done:
			return err
		case <-stopCtx.Done():
			return fmt.Errorf("stop rule worker: %w", stopCtx.Err())
		}
	}
}

func localRuleWorkerBaseline(config bootstrap.ProcessConfig) string {
	if config.DesignKnowledgeRoot == "" || config.DesignKnowledgeRevision == "" {
		return ""
	}
	return filepath.Join(config.DesignKnowledgeRoot, config.DesignKnowledgeRevision)
}

func localRuleWorkerRevisionRoot(config bootstrap.ProcessConfig) string {
	if config.RuleWorker.OutputRoot == "" {
		return ""
	}
	return config.RuleWorker.OutputRoot
}

func localRuleWorkerPointerPath(config bootstrap.ProcessConfig) string {
	root := localRuleWorkerRevisionRoot(config)
	if root == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(root), "current.json")
}

func activateLocalRuleRevision(ctx context.Context, config bootstrap.ProcessConfig, manager *dkRuntime.Manager, phase string) error {
	pointerPath := localRuleWorkerPointerPath(config)
	if pointerPath == "" || manager == nil {
		return nil
	}
	pointer, found, err := ruleworker.ReadLocalRevisionPointer(pointerPath)
	if err != nil {
		return fmt.Errorf("agenui-agent: read local design knowledge pointer: %w", err)
	}
	if !found {
		return nil
	}
	return activateLocalRuleRevisionID(ctx, config, manager, pointer.RevisionID, phase)
}

func activateLocalRuleRevisionID(ctx context.Context, config bootstrap.ProcessConfig, manager *dkRuntime.Manager, revisionID, phase string) error {
	if manager == nil || revisionID == "" {
		return nil
	}
	root := localRuleWorkerRevisionRoot(config)
	repository, err := designknowledge.Load(ctx, os.DirFS(root), revisionID)
	if err != nil {
		return fmt.Errorf("agenui-agent: load local design knowledge revision %s: %w", revisionID, err)
	}
	if err := manager.Activate(repository, dkRuntime.SnapshotInfo{
		Version: revisionID, RevisionID: repository.RevisionID(), RevisionHash: repository.RevisionHash(), Source: "local_rule_worker", LoadedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("agenui-agent: activate local design knowledge revision %s: %w", revisionID, err)
	}
	log.Printf("designknowledge.local.%s revision_id=%s revision_hash=%s", phase, repository.RevisionID(), repository.RevisionHash())
	return nil
}

func inServiceRuleWorkerID() string {
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return fmt.Sprintf("agenui-agent-svc@%s#%d", host, os.Getpid())
}

func startLocalRevisionWatcher(parent context.Context, config bootstrap.ProcessConfig, manager *dkRuntime.Manager) {
	interval := config.DesignKnowledgeWatch.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case <-ticker.C:
				if err := activateLocalRuleRevision(parent, config, manager, "watch"); err != nil {
					log.Printf("designknowledge.local.watch.reject error=%v", err)
				}
			}
		}
	}()
}

func logDesignKnowledgeActive(prefix string, info dkRuntime.SnapshotInfo) {
	log.Printf(
		"%s source=%s version=%s revision_id=%s revision_hash=%s loaded_at=%s",
		prefix,
		info.Source,
		info.Version,
		info.RevisionID,
		info.RevisionHash,
		info.LoadedAt.Format(time.RFC3339),
	)
}

func openApplicationDB(
	ctx context.Context,
	config bootstrap.ProcessConfig,
) (*sql.DB, error) {
	if config.DatabaseDriver == "" {
		log.Printf("[agenui-storage] application database is disabled")
		return nil, nil
	}
	dsn := strings.TrimSpace(os.Getenv(config.DatabaseDSNEnv))
	if dsn == "" {
		return nil, fmt.Errorf(
			"agenui-agent: database DSN environment %s is empty",
			config.DatabaseDSNEnv,
		)
	}
	if config.DatabaseDriver != "sqlite" && config.DatabaseDriver != "mysql" {
		return nil, fmt.Errorf("agenui-agent: unsupported application database driver %q (supported: sqlite, mysql)", config.DatabaseDriver)
	}
	log.Printf("[agenui-storage] opening application database driver=%q", config.DatabaseDriver)
	db, err := sql.Open(config.DatabaseDriver, dsn)
	if err != nil {
		log.Printf("[agenui-storage] sql.Open failed driver=%q: %v", config.DatabaseDriver, err)
		return nil, fmt.Errorf("agenui-agent: open application database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("agenui-agent: ping application database: %w", err)
	}
	if config.DatabaseDriver == "sqlite" {
		// Harness opens the same local database through modernc SQLite in DELETE
		// journal mode. Keep the management connection on the identical driver and
		// journal contract: mixing mattn WAL with modernc DELETE can unlink a WAL
		// that another live connection still owns, splitting model configuration
		// from the Provider registry and eventually corrupting the database.
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("agenui-agent: configure application sqlite busy timeout: %w", err)
		}
		var journalMode string
		if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&journalMode); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("agenui-agent: configure application sqlite journal mode: %w", err)
		}
		if !strings.EqualFold(journalMode, "delete") {
			_ = db.Close()
			return nil, fmt.Errorf("agenui-agent: application sqlite journal mode is %q, want delete", journalMode)
		}
	}
	log.Printf("[agenui-storage] application database ready driver=%q", config.DatabaseDriver)
	return db, nil
}

func developmentPrincipalResolver(request *http.Request) (httptransport.Principal, error) {
	tenantID := strings.TrimSpace(request.Header.Get("X-AGenUI-Tenant-ID"))
	userID := strings.TrimSpace(request.Header.Get("X-AGenUI-User-ID"))
	// Keep the accepted admin UI's existing header names as a compatibility
	// fallback while all public documentation and new clients use X-AGenUI-*.
	if tenantID == "" {
		tenantID = strings.TrimSpace(request.Header.Get("X-AGenUI-Tenant-ID"))
	}
	if userID == "" {
		userID = strings.TrimSpace(request.Header.Get("X-AGenUI-User-ID"))
	}
	if tenantID == "" || userID == "" {
		return httptransport.Principal{}, fmt.Errorf(
			"missing X-AGenUI-Tenant-ID or X-AGenUI-User-ID",
		)
	}
	return httptransport.Principal{TenantID: tenantID, UserID: userID}, nil
}
