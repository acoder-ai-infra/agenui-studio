package agentregistry

import (
	"database/sql"

	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

// NewServiceWithSQLStore 只负责把外部连接池装配为 Registry Store。
// 连接、Ping、迁移与关闭仍由应用 composition root 统一管理。
func NewServiceWithSQLStore(db *sql.DB, opts ...Option) (*Service, error) {
	store, err := NewSQLStore(db)
	if err != nil {
		return nil, err
	}
	opts = append(opts, WithStore(store))
	service := NewService(opts...)
	if service.prompts != nil {
		return service, nil
	}
	promptStore, err := NewSQLPromptStore(db)
	if err != nil {
		return nil, err
	}
	service.prompts, err = NewPromptResolver(promptStore)
	if err != nil {
		return nil, err
	}
	return service, nil
}

// NewServiceWithSQLStoreAndToolCatalog closes the production authoring
// boundary for Agents that declare tools. The caller still owns the SQL pool;
// this constructor only makes Tool Registry validation impossible to forget.
func NewServiceWithSQLStoreAndToolCatalog(
	db *sql.DB,
	registry toolgateway.ToolRegistry,
	opts ...Option,
) (*Service, error) {
	if registry == nil {
		return nil, newError(CodeDependencyMissing, "tools", "tool registry is required")
	}
	service, err := NewServiceWithSQLStore(db, opts...)
	if err != nil {
		return nil, err
	}
	service.resolver = CompositeDependencyResolver{
		service.resolver,
		ToolCatalogResolver{Registry: registry},
	}
	return service, nil
}

// NewProductionServiceWithSQLStore 是线上 Agent Registry 的唯一完整装配入口。
// 旧 NewServiceWithSQLStore 保留给迁移和本地联调，但无法通过 ValidateProduction。
func NewProductionServiceWithSQLStore(
	db *sql.DB,
	registry toolgateway.ToolRegistry,
	opts ...Option,
) (*Service, error) {
	service, err := NewServiceWithSQLStoreAndToolCatalog(db, registry, opts...)
	if err != nil {
		return nil, err
	}
	if err := service.ValidateProduction(); err != nil {
		return nil, err
	}
	return service, nil
}
