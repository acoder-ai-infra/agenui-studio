package app

import (
	"context"
	"database/sql"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/migration"
	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
)

type unifiedMySQLBackend struct {
	db               *sql.DB
	ledger           mysqlStorageBackend
	validateRegistry func(context.Context, *sql.DB) error
	// skipLedgerCheck 关闭运行时就绪的「账本校验」层(CheckReady 结尾的
	// migration.RequireApplied),仅保留「结构校验」层。由 MySQLConfig.SkipMigrationLedgerCheck 透传。
	skipLedgerCheck bool
}

func newUnifiedMySQLBackend(db *sql.DB, ledger mysqlStorageBackend, skipLedgerCheck bool) mysqlStorageBackend {
	return newUnifiedSQLBackend(db, ledger, agentregistry.ValidateMySQLSchemaV2, skipLedgerCheck)
}

func newUnifiedSQLBackend(db *sql.DB, ledger mysqlStorageBackend, validateRegistry func(context.Context, *sql.DB) error, skipLedgerCheck bool) mysqlStorageBackend {
	return &unifiedMySQLBackend{db: db, ledger: ledger, validateRegistry: validateRegistry, skipLedgerCheck: skipLedgerCheck}
}

func (b *unifiedMySQLBackend) Stores() storage.Stores { return b.ledger.Stores() }

func (b *unifiedMySQLBackend) CheckReady(ctx context.Context) error {
	if err := b.ledger.CheckReady(ctx); err != nil {
		return err
	}
	if err := b.validateRegistry(ctx, b.db); err != nil {
		return err
	}
	if err := agentregistry.ValidateConfigControlSchema(ctx, b.db); err != nil {
		return err
	}
	if err := skill.ValidateSchema(ctx, b.db); err != nil {
		return err
	}
	if err := skill.ValidateLifecycleSchema(ctx, b.db); err != nil {
		return err
	}
	if err := skill.ValidatePackageFileSchema(ctx, b.db); err != nil {
		return err
	}
	if err := mcp.ValidateSchema(ctx, b.db); err != nil {
		return err
	}
	if err := modeladmin.ValidateSchema(ctx, b.db); err != nil {
		return err
	}
	if err := toolconfig.ValidateSchema(ctx, b.db); err != nil {
		return err
	}
	if err := httptooldef.ValidateSchema(ctx, b.db); err != nil {
		return err
	}
	if err := tenantadmin.ValidateSchema(ctx, b.db); err != nil {
		return err
	}
	// 「结构校验」层到此为止(上面各 Validate*Schema,读真实库确认表/字段与代码契约一致)。
	// 下面是「账本校验」层:migration.RequireApplied 只查 harness_schema_migrations 里是否
	// 登记过各迁移标记,与库真实状态无关。skipLedgerCheck=true 时跳过——用于表由平台
	// out-of-band 预建、从不经 SDK 迁移器、账本恒为空的库(如生产 publish 库),避免账本
	// 标记缺失导致启动 fail-closed。默认(false)保留该校验。
	// 注:默认租户(public)种子行不在此校验(已移除 ValidateDefaultTenant),缺失不阻断
	// 启动,交由运维 out-of-band 补种。
	if b.skipLedgerCheck {
		return nil
	}
	return migration.RequireApplied(ctx, b.db,
		"ledger/001", "artifact/001", "agent_registry/001", "agent_registry/002", "agent_registry/003", "agent_registry/004", "skill/001", "skill/002", "skill/003", "mcp/001", "mcp/002", "mcp/003", "mcp/004", "mcp/005", "model_provider/001", "tool_config/001", "http_tool/001",
	)
}

func (b *unifiedMySQLBackend) Migrate(ctx context.Context) error {
	return migration.Apply(ctx, b.db,
		migration.Step{ID: "ledger/001", Apply: b.ledger.Migrate},
		migration.Step{ID: "artifact/001", Apply: func(ctx context.Context) error {
			return metastore.InitializeMySQLSchema(ctx, b.db)
		}},
		migration.Step{ID: "agent_registry/001", Apply: func(ctx context.Context) error {
			return agentregistry.ApplyMySQLBaseline(ctx, b.db)
		}},
		migration.Step{ID: "agent_registry/002", Apply: func(ctx context.Context) error {
			return b.validateRegistry(ctx, b.db)
		}},
		migration.Step{ID: "agent_registry/003", Apply: func(ctx context.Context) error {
			if err := agentregistry.ApplyMySQLConfigControl(ctx, b.db); err != nil {
				return err
			}
			return agentregistry.ValidateConfigControlSchema(ctx, b.db)
		}},
		migration.Step{ID: "agent_registry/004", Apply: func(ctx context.Context) error {
			return agentregistry.ApplyMySQLPreparedProjection(ctx, b.db)
		}},
		migration.Step{ID: "skill/001", Apply: func(ctx context.Context) error {
			if err := skill.ApplyMySQLSchema(ctx, b.db); err != nil {
				return err
			}
			return skill.ValidateSchema(ctx, b.db)
		}},
		migration.Step{ID: "skill/002", Apply: func(ctx context.Context) error {
			if err := skill.ApplyMySQLLifecycleSchema(ctx, b.db); err != nil {
				return err
			}
			return skill.ValidateLifecycleSchema(ctx, b.db)
		}},
		migration.Step{ID: "skill/003", Apply: func(ctx context.Context) error {
			if err := skill.ApplyMySQLPackageFileSchema(ctx, b.db); err != nil {
				return err
			}
			return skill.ValidatePackageFileSchema(ctx, b.db)
		}},
		migration.Step{ID: "mcp/001", Apply: func(ctx context.Context) error {
			if err := mcp.ApplyMySQLSchema(ctx, b.db); err != nil {
				return err
			}
			return mcp.ValidateRegistrySchema(ctx, b.db)
		}},
		migration.Step{ID: "mcp/002", Apply: func(ctx context.Context) error {
			if err := mcp.ApplyMySQLOAuthSchema(ctx, b.db); err != nil {
				return err
			}
			return mcp.ValidateOAuthBaseSchema(ctx, b.db)
		}},
		migration.Step{ID: "mcp/003", Apply: func(ctx context.Context) error {
			if err := mcp.ApplyMySQLDebugHistorySchema(ctx, b.db); err != nil {
				return err
			}
			return mcp.ValidateDebugHistorySchema(ctx, b.db)
		}},
		migration.Step{ID: "mcp/004", Apply: func(ctx context.Context) error {
			if err := mcp.ApplyMySQLOAuthAuthorizationSchema(ctx, b.db); err != nil {
				return err
			}
			return mcp.ValidateOAuthAuthorizationSchema(ctx, b.db)
		}},
		migration.Step{ID: "mcp/005", Apply: func(ctx context.Context) error {
			if err := mcp.ApplyMySQLOAuthRefreshSchema(ctx, b.db); err != nil {
				return err
			}
			return mcp.ValidateOAuthSchema(ctx, b.db)
		}},
		migration.Step{ID: "model_provider/001", Apply: func(ctx context.Context) error {
			if err := modeladmin.ApplyMySQLSchema(ctx, b.db); err != nil {
				return err
			}
			return modeladmin.ValidateSchema(ctx, b.db)
		}},
		migration.Step{ID: "tool_config/001", Apply: func(ctx context.Context) error {
			if err := toolconfig.ApplyMySQLSchema(ctx, b.db); err != nil {
				return err
			}
			return toolconfig.ValidateSchema(ctx, b.db)
		}},
		migration.Step{ID: "http_tool/001", Apply: func(ctx context.Context) error {
			if err := httptooldef.ApplyMySQLSchema(ctx, b.db); err != nil {
				return err
			}
			return httptooldef.ValidateSchema(ctx, b.db)
		}},
		migration.Step{ID: "tenant/001", Apply: func(ctx context.Context) error {
			if err := tenantadmin.ApplyMySQLSchema(ctx, b.db); err != nil {
				return err
			}
			return tenantadmin.ValidateSchema(ctx, b.db)
		}},
		migration.Step{ID: "tenant/002", Apply: func(ctx context.Context) error {
			if _, err := tenantadmin.NewSQLManagedRegistry(b.db).EnsureDefaultTenant(ctx, "migration"); err != nil {
				return err
			}
			return tenantadmin.ValidateDefaultTenant(ctx, b.db)
		}},
	)
}
