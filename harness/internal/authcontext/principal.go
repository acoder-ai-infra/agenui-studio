package authcontext

import "context"

type Principal struct {
	UserID   string
	TenantID string
	Scopes   []string
	Debug    bool
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok && principal.UserID != "" && principal.TenantID != ""
}

func (p Principal) HasScope(scope string) bool {
	for _, current := range p.Scopes {
		if current == scope || current == "agent.config.admin" || current == "*" {
			return true
		}
	}
	return false
}
