package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/migration"
	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
)

const (
	storageSchemaUp     = "up"
	storageSchemaStatus = "status"
)

// StorageSchemaResult is the operator-facing result of an explicit schema
// operation. Runtime startup never invokes these operations implicitly.
type StorageSchemaResult struct {
	Backend string
	Action  string
	Ready   bool
	Noop    bool
}

// MigrateStorage applies all schema migrations owned by the configured SQL
// backend and then verifies the same readiness contract used by Runtime.
func MigrateStorage(ctx context.Context, config StorageConfig, production bool) (StorageSchemaResult, error) {
	return migrateStorage(ctx, config, production, storageSchemaUp, defaultBuildDependencies())
}

// CheckStorageSchema performs metadata-only readiness checks. It never creates
// directories, tables, columns, or migration markers.
func CheckStorageSchema(ctx context.Context, config StorageConfig, production bool) (StorageSchemaResult, error) {
	return migrateStorage(ctx, config, production, storageSchemaStatus, defaultBuildDependencies())
}

func migrateStorage(ctx context.Context, config StorageConfig, production bool, action string, deps buildDependencies) (result StorageSchemaResult, err error) {
	result = StorageSchemaResult{Backend: config.Backend, Action: action}
	if action != storageSchemaUp && action != storageSchemaStatus {
		return result, fmt.Errorf("unsupported storage schema action %q", action)
	}

	switch config.Backend {
	case "memory", "":
		if production {
			return result, errors.New("memory storage backend is not allowed in production")
		}
		result.Backend = "memory"
		result.Ready = true
		result.Noop = true
		return result, nil
	case "sqlite":
		var sqliteConfig SQLiteConfig
		if err := decodeStrictRawConfig(config.Config, &sqliteConfig); err != nil {
			return result, fmt.Errorf("decode sqlite storage config: %w", err)
		}
		if sqliteConfig.Path == "" {
			return result, errors.New("storage.config.path is required for sqlite backend")
		}
		if action == storageSchemaUp {
			if err := os.MkdirAll(filepath.Dir(sqliteConfig.Path), 0o750); err != nil {
				return result, fmt.Errorf("create sqlite directory: %w", err)
			}
		}
		db, err := sqlite.OpenDatabase(sqliteConfig.Path)
		if err != nil {
			return result, fmt.Errorf("open sqlite storage: %w", err)
		}
		backend := sqlite.New(db)
		defer func() {
			if closeErr := backend.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close sqlite storage: %w", closeErr))
			}
		}()
		if action == storageSchemaUp {
			if err := migration.ApplySQLite(ctx, db,
				migration.Step{ID: "ledger/001", Apply: backend.Migrate},
				migration.Step{ID: "agent_registry/001", Apply: func(ctx context.Context) error {
					return agentregistry.ApplySQLiteBaseline(ctx, db)
				}},
				migration.Step{ID: "agent_registry/002", Apply: func(ctx context.Context) error {
					return agentregistry.ValidateSQLiteSchema(ctx, db)
				}},
				migration.Step{ID: "agent_registry/003", Apply: func(ctx context.Context) error {
					if err := agentregistry.ApplySQLiteConfigControl(ctx, db); err != nil {
						return err
					}
					return agentregistry.ValidateConfigControlSchema(ctx, db)
				}},
				migration.Step{ID: "agent_registry/004", Apply: func(ctx context.Context) error {
					return agentregistry.ApplySQLitePreparedProjection(ctx, db)
				}},
				migration.Step{ID: "skill/001", Apply: func(ctx context.Context) error {
					if err := skill.ApplySQLiteSchema(ctx, db); err != nil {
						return err
					}
					return skill.ValidateSchema(ctx, db)
				}},
				migration.Step{ID: "skill/002", Apply: func(ctx context.Context) error {
					if err := skill.ApplySQLiteLifecycleSchema(ctx, db); err != nil {
						return err
					}
					return skill.ValidateLifecycleSchema(ctx, db)
				}},
				migration.Step{ID: "skill/003", Apply: func(ctx context.Context) error {
					if err := skill.ApplySQLitePackageFileSchema(ctx, db); err != nil {
						return err
					}
					return skill.ValidatePackageFileSchema(ctx, db)
				}},
				migration.Step{ID: "mcp/001", Apply: func(ctx context.Context) error {
					if err := mcp.ApplySQLiteSchema(ctx, db); err != nil {
						return err
					}
					return mcp.ValidateRegistrySchema(ctx, db)
				}},
				migration.Step{ID: "mcp/002", Apply: func(ctx context.Context) error {
					if err := mcp.ApplySQLiteOAuthSchema(ctx, db); err != nil {
						return err
					}
					return mcp.ValidateOAuthBaseSchema(ctx, db)
				}},
				migration.Step{ID: "mcp/003", Apply: func(ctx context.Context) error {
					if err := mcp.ApplySQLiteDebugHistorySchema(ctx, db); err != nil {
						return err
					}
					return mcp.ValidateDebugHistorySchema(ctx, db)
				}},
				migration.Step{ID: "mcp/004", Apply: func(ctx context.Context) error {
					if err := mcp.ApplySQLiteOAuthAuthorizationSchema(ctx, db); err != nil {
						return err
					}
					return mcp.ValidateOAuthAuthorizationSchema(ctx, db)
				}},
				migration.Step{ID: "mcp/005", Apply: func(ctx context.Context) error {
					if err := mcp.ApplySQLiteOAuthRefreshSchema(ctx, db); err != nil {
						return err
					}
					return mcp.ValidateOAuthSchema(ctx, db)
				}},
				migration.Step{ID: "model_provider/001", Apply: func(ctx context.Context) error {
					if err := modeladmin.ApplySQLiteSchema(ctx, db); err != nil {
						return err
					}
					return modeladmin.ValidateSchema(ctx, db)
				}},
				migration.Step{ID: "tool_config/001", Apply: func(ctx context.Context) error {
					if err := toolconfig.ApplySQLiteSchema(ctx, db); err != nil {
						return err
					}
					return toolconfig.ValidateSchema(ctx, db)
				}},
				migration.Step{ID: "http_tool/001", Apply: func(ctx context.Context) error {
					if err := httptooldef.ApplySQLiteSchema(ctx, db); err != nil {
						return err
					}
					return httptooldef.ValidateSchema(ctx, db)
				}},
				migration.Step{ID: "tenant/001", Apply: func(ctx context.Context) error {
					if err := tenantadmin.ApplySQLiteSchema(ctx, db); err != nil {
						return err
					}
					return tenantadmin.ValidateSchema(ctx, db)
				}},
				migration.Step{ID: "tenant/002", Apply: func(ctx context.Context) error {
					if _, err := tenantadmin.NewSQLManagedRegistry(db).EnsureDefaultTenant(ctx, "migration"); err != nil {
						return err
					}
					return tenantadmin.ValidateDefaultTenant(ctx, db)
				}},
			); err != nil {
				return result, fmt.Errorf("migrate sqlite schema: %w", err)
			}
		}
		if err := backend.CheckReady(ctx); err != nil {
			return result, err
		}
		if err := agentregistry.ValidateSQLiteSchema(ctx, db); err != nil {
			return result, err
		}
		if err := agentregistry.ValidateConfigControlSchema(ctx, db); err != nil {
			return result, err
		}
		if err := skill.ValidateSchema(ctx, db); err != nil {
			return result, err
		}
		if err := skill.ValidateLifecycleSchema(ctx, db); err != nil {
			return result, err
		}
		if err := skill.ValidatePackageFileSchema(ctx, db); err != nil {
			return result, err
		}
		if err := mcp.ValidateSchema(ctx, db); err != nil {
			return result, err
		}
		if err := modeladmin.ValidateSchema(ctx, db); err != nil {
			return result, err
		}
		if err := toolconfig.ValidateSchema(ctx, db); err != nil {
			return result, err
		}
		if err := httptooldef.ValidateSchema(ctx, db); err != nil {
			return result, err
		}
		if err := tenantadmin.ValidateSchema(ctx, db); err != nil {
			return result, err
		}
		if err := tenantadmin.ValidateDefaultTenant(ctx, db); err != nil {
			return result, err
		}
		if err := migration.RequireApplied(ctx, db,
			"ledger/001", "agent_registry/001", "agent_registry/002", "agent_registry/003", "agent_registry/004", "skill/001", "skill/002", "skill/003", "mcp/001", "mcp/002", "mcp/003", "mcp/004", "mcp/005", "model_provider/001", "tool_config/001", "http_tool/001", "tenant/001", "tenant/002",
		); err != nil {
			return result, err
		}
		result.Ready = true
		return result, nil
	case "mysql":
		var mysqlConfig MySQLConfig
		if err := decodeStrictRawConfig(config.Config, &mysqlConfig); err != nil {
			return result, fmt.Errorf("decode %s storage config: %w", config.Backend, err)
		}
		if err := validateMySQLBackendConfig(config.Backend, mysqlConfig, production); err != nil {
			return result, err
		}
		resolved, err := mysqlConfig.resolveEnvironment()
		if err != nil {
			return result, err
		}
		frameworkConfig, err := resolved.toFrameworkConfig()
		if err != nil {
			return result, err
		}
		if err := deps.initMySQL(frameworkConfig); err != nil {
			return result, fmt.Errorf("initialize %s storage: %w", config.Backend, err)
		}
		db := deps.getMySQL(frameworkConfig.Name)
		if db == nil {
			return result, fmt.Errorf("initialize %s storage: database pool is nil", config.Backend)
		}
		defer func() {
			if closeErr := db.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close %s storage: %w", config.Backend, closeErr))
			}
		}()
		backend := deps.newMySQLBackend(db, config.Backend, mysqlConfig.SkipMigrationLedgerCheck)
		if backend == nil {
			return result, fmt.Errorf("initialize %s storage: backend factory returned nil", config.Backend)
		}
		if action == storageSchemaUp {
			if err := backend.Migrate(ctx); err != nil {
				return result, fmt.Errorf("migrate %s schema: %w", config.Backend, err)
			}
		}
		if err := backend.CheckReady(ctx); err != nil {
			return result, err
		}
		result.Ready = true
		return result, nil
	default:
		return result, fmt.Errorf("unknown storage backend %q", config.Backend)
	}
}
