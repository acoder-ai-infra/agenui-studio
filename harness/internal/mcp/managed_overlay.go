package mcp

import (
	"context"
	"errors"
)

type overlayRegistry struct {
	managed  *SQLManagedRegistry
	fallback Registry
}

func (r overlayRegistry) Get(ctx context.Context, serverID string) (ServerDefinition, error) {
	return r.fallback.Get(ctx, serverID)
}

func (r overlayRegistry) GetForPrincipal(ctx context.Context, principal Principal, serverID string) (ServerDefinition, error) {
	definition, err := r.managed.GetForPrincipal(ctx, principal, serverID)
	if err == nil {
		return definition, nil
	}
	if !errors.Is(err, ErrServerNotFound) {
		return ServerDefinition{}, err
	}
	return r.fallback.Get(ctx, serverID)
}

type overlayClientProvider struct {
	managed  ManagedClientProvider
	fallback ClientProvider
}

func (p overlayClientProvider) Client(ctx context.Context, definition ServerDefinition, principal Principal) (Client, error) {
	if definition.Transport == "streamable-http" && definition.Endpoint != "" {
		return p.managed.Client(ctx, definition, principal)
	}
	return p.fallback.Client(ctx, definition, principal)
}

func NewManagedOverlay(base *Service, managed *SQLManagedRegistry) (*Service, error) {
	return NewManagedOverlayWithCredentials(base, managed, nil)
}

func NewManagedOverlayWithCredentials(base *Service, managed *SQLManagedRegistry, credentials CredentialProvider) (*Service, error) {
	if base == nil || managed == nil {
		return nil, errorsConfiguration("base service and managed registry are required")
	}
	service, err := NewService(
		overlayRegistry{managed: managed, fallback: base.registry},
		overlayClientProvider{managed: ManagedClientProvider{Credentials: credentials}, fallback: base.clients},
		base.snapshots, base.ids,
	)
	if err != nil {
		return nil, err
	}
	service.clock = base.clock
	base.approvalMu.RLock()
	service.approvals = base.approvals
	base.approvalMu.RUnlock()
	return service, nil
}

var _ PrincipalRegistry = overlayRegistry{}
