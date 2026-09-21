package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

type agentRegistryBuildOptions struct {
	Production   bool
	DatabaseOnly bool
	Database     *sql.DB
	// PromptsFromDatabase 为 true 时 prompt store 走 SQL 后端且跳过 YAML
	// 导入（ADR-0002）；为 false 时用内存 store + YAML 目录导入。
	PromptsFromDatabase bool
	ToolRegistry        toolgateway.ToolRegistry
	Loader              agentregistry.Loader
	PromptCatalogPath   string
	Logger              observability.StructuredLogger
	Tracer              observability.TraceProvider
}

type agentRegistryPromptCatalog struct {
	SchemaVersion string                        `yaml:"schema_version"`
	Prompts       []agentregistry.PromptVersion `yaml:"prompts"`
}

// buildAgentRegistry 只负责 Registry 业务装配；连接池和线上迁移仍由 composition root 管理。
func buildAgentRegistry(
	ctx context.Context,
	options agentRegistryBuildOptions,
) (*agentregistry.Service, agentregistry.PromptResolver, *agentregistry.BootstrapResult, error) {
	if ctx == nil {
		return nil, nil, nil, errors.New("app: agent registry context is required")
	}
	if options.DatabaseOnly && options.Database == nil {
		return nil, nil, nil, errors.New("app: database-backed agent registry requires database")
	}
	if !options.DatabaseOnly && options.Loader == nil {
		return nil, nil, nil, errors.New("app: agent registry loader is required")
	}
	if !options.PromptsFromDatabase && strings.TrimSpace(options.PromptCatalogPath) == "" {
		return nil, nil, nil, errors.New("app: agent registry prompt catalog path is required")
	}
	if options.Production || options.DatabaseOnly {
		if options.Database == nil {
			return nil, nil, nil, errors.New("app: production agent registry database is required")
		}
		if options.ToolRegistry == nil {
			return nil, nil, nil, errors.New("app: production agent registry tool registry is required")
		}
	}

	promptStore, err := agentRegistryPromptStore(ctx, options)
	if err != nil {
		return nil, nil, nil, err
	}
	// prompts=database 时数据库是唯一事实源，不再从 YAML 目录导入（ADR-0002）。
	if !options.PromptsFromDatabase {
		if err := importAgentRegistryPrompts(ctx, promptStore, options.PromptCatalogPath); err != nil {
			return nil, nil, nil, err
		}
	}
	promptResolver, err := agentregistry.NewPromptResolver(promptStore)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("app: build agent registry prompt resolver: %w", err)
	}

	serviceOptions := []agentregistry.Option{
		agentregistry.WithPromptResolver(promptResolver),
	}
	if !options.DatabaseOnly {
		serviceOptions = append(serviceOptions, agentregistry.WithLoader(options.Loader))
	}
	if options.Logger != nil {
		serviceOptions = append(serviceOptions, agentregistry.WithLogger(options.Logger))
	}
	if options.Tracer != nil {
		serviceOptions = append(serviceOptions, agentregistry.WithTracer(options.Tracer))
	}

	var service *agentregistry.Service
	if options.Production || options.DatabaseOnly {
		service, err = agentregistry.NewProductionServiceWithSQLStore(
			options.Database,
			options.ToolRegistry,
			serviceOptions...,
		)
	} else {
		serviceOptions = append(serviceOptions, agentregistry.WithStore(agentregistry.NewMemoryStore()))
		if options.ToolRegistry != nil {
			serviceOptions = append(serviceOptions, agentregistry.WithDependencyResolver(
				agentregistry.CompositeDependencyResolver{
					agentregistry.CatalogDependencyResolver{},
					agentregistry.ToolCatalogResolver{Registry: options.ToolRegistry},
				},
			))
		}
		service = agentregistry.NewService(serviceOptions...)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("app: build agent registry service: %w", err)
	}
	if options.DatabaseOnly {
		return service, promptResolver, nil, nil
	}

	result, err := service.Bootstrap(ctx)
	if err != nil {
		// result 在 Bootstrap 失败时可能为 nil，错误路径不能解引用它。
		if result == nil {
			return service, promptResolver, nil, fmt.Errorf("app: bootstrap agent registry: %w", err)
		}
		return service, promptResolver, result, fmt.Errorf("app: bootstrap agent registry: %w (issues=%+v)", err, result.Issues)
	}
	return service, promptResolver, result, nil
}

func agentRegistryPromptStore(ctx context.Context, options agentRegistryBuildOptions) (agentregistry.PromptStore, error) {
	// prompts=file：内存 store，由 YAML 目录导入，完全不读写库（ADR-0001）。
	if !options.PromptsFromDatabase {
		return agentregistry.NewMemoryPromptStore(), nil
	}
	if options.Database == nil {
		return nil, errors.New("app: database-backed prompt store requires database")
	}
	store, err := agentregistry.NewSQLPromptStore(options.Database)
	if err != nil {
		return nil, fmt.Errorf("app: build agent registry prompt store: %w", err)
	}
	if !options.Production {
		if err := store.EnsureSchema(ctx); err != nil {
			return nil, fmt.Errorf("app: ensure local agent registry prompt schema: %w", err)
		}
	}
	return store, nil
}

func importAgentRegistryPrompts(ctx context.Context, store agentregistry.PromptStore, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("app: read agent registry prompt catalog %s: %w", path, err)
	}
	var catalog agentRegistryPromptCatalog
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&catalog); err != nil {
		return fmt.Errorf("app: decode agent registry prompt catalog %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple YAML documents are not allowed")
		}
		return fmt.Errorf("app: decode agent registry prompt catalog %s: %w", path, err)
	}
	if catalog.SchemaVersion != "harness.prompts.v1" || len(catalog.Prompts) == 0 {
		return fmt.Errorf("app: invalid or empty agent registry prompt catalog %s", path)
	}
	for _, prompt := range catalog.Prompts {
		if err := store.Create(ctx, prompt); err != nil {
			return fmt.Errorf("app: import agent registry prompt %s@%s: %w", prompt.Ref, prompt.Version, err)
		}
	}
	return nil
}
