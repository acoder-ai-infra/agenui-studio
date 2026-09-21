package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

type httpToolSnapshotProvider struct {
	store *httptooldef.SQLManagedRegistry
}

func (p httpToolSnapshotProvider) ResolveHTTPToolSnapshot(ctx context.Context, tenantID, _, _, name string) (agentruntime.HTTPToolSnapshot, error) {
	if p.store == nil {
		return agentruntime.HTTPToolSnapshot{}, errors.New("http tool store is unavailable")
	}
	def, err := p.store.GetForPrincipal(ctx, tenantID, name)
	if err != nil {
		return agentruntime.HTTPToolSnapshot{}, err
	}
	return agentruntime.HTTPToolSnapshot{
		Name: def.ToolName, Description: def.Description,
		InputSchema: append(json.RawMessage(nil), def.InputSchema...),
		Method:      def.Method, BaseURL: def.BaseURL, ResponseMode: def.ResponseMode,
		Write: def.Write, HeaderEnv: def.HeaderEnv, TimeoutMS: def.TimeoutMS,
		RiskLevel: def.RiskLevel, DefinitionHash: def.Hash(),
	}, nil
}

var toolConfigEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func httpToolConfigResolver(store *toolconfig.SQLManagedRegistry) toolgateway.HTTPConfigResolver {
	if store == nil {
		return nil
	}
	return func(ctx context.Context, def *toolgateway.ToolDefinition) (*toolgateway.HTTPToolSpec, error) {
		if def == nil || def.HTTP == nil || len(def.ConfigSchema) == 0 {
			return nil, nil
		}
		actor, ok := artifact.ActorFromContext(ctx)
		if !ok || actor.TenantID == "" {
			return nil, nil
		}
		managed, err := store.GetManaged(ctx, actor.TenantID, def.Name)
		if err != nil {
			if errors.Is(err, toolconfig.ErrToolConfigNotFound) {
				return nil, nil
			}
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "load tenant tool config", false, err)
		}
		var cfg struct {
			BaseURL    string            `json:"base_url"`
			HeadersEnv map[string]string `json:"headers_env"`
			TimeoutMS  int               `json:"timeout_ms"`
		}
		if err := json.Unmarshal(managed.Definition.Config, &cfg); err != nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "decode tenant tool config", false, err)
		}
		effective := *def.HTTP
		headers := make(map[string]string, len(def.HTTP.Headers)+len(cfg.HeadersEnv))
		for name, value := range def.HTTP.Headers {
			headers[name] = value
		}
		if raw := strings.TrimSpace(cfg.BaseURL); raw != "" {
			endpoint, err := url.Parse(raw)
			if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil {
				return nil, toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "tenant base_url must be an http(s) URL without credentials", false, nil)
			}
			effective.URL = raw
		}
		for header, envName := range cfg.HeadersEnv {
			if err := validateConfiguredHTTPHeader(header, ""); err != nil || !toolConfigEnvName.MatchString(envName) {
				return nil, toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "tenant header configuration is invalid", false, err)
			}
			value, ok := os.LookupEnv(envName)
			if !ok || value == "" {
				return nil, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "tenant credential environment variable is not set", false, nil)
			}
			headers[http.CanonicalHeaderKey(header)] = value
		}
		effective.Headers = headers
		if cfg.TimeoutMS > 0 {
			effective.Timeout = time.Duration(cfg.TimeoutMS) * time.Millisecond
		}
		return &effective, nil
	}
}
