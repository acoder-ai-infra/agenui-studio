package kernel

import "context"

// IdentityResolverFunc is the trusted-source identity mapping the kernel
// applies before AgentBinding. It is populated by the SDK from
// harness/extension.IdentityResolver at Build time; when nil, the kernel uses
// the identity supplied by the caller as-is.
//
// The kernel wraps the function in a per-run timeout when a Policy.Timeout is
// registered alongside; the SDK adapter is responsible for stamping the
// timeout onto ctx.
type IdentityResolverFunc func(ctx context.Context, req IdentityRequest) (IdentityResult, error)

// IdentityRequest is what the kernel hands to the IdentityResolverFunc.
type IdentityRequest struct {
	BusinessTenantID  string
	BusinessUserID    string
	BusinessSessionID string
	Metadata          map[string]string
}

// IdentityResult is the resolver's return. Non-empty fields override the
// incoming identity; empty fields are preserved.
type IdentityResult struct {
	TenantID  string
	UserID    string
	SessionID string
	Metadata  map[string]string
}

// ApplyIdentity applies a resolver result on top of the caller-provided
// identity, preserving caller values where the resolver returned empty. It
// exists so multiple call sites (Start / Resume / Subscribe) use one merge
// rule.
func ApplyIdentity(base IdentityRequest, resolved IdentityResult) IdentityResult {
	out := IdentityResult{
		TenantID:  base.BusinessTenantID,
		UserID:    base.BusinessUserID,
		SessionID: base.BusinessSessionID,
		Metadata:  cloneMetadata(base.Metadata),
	}
	if resolved.TenantID != "" {
		out.TenantID = resolved.TenantID
	}
	if resolved.UserID != "" {
		out.UserID = resolved.UserID
	}
	if resolved.SessionID != "" {
		out.SessionID = resolved.SessionID
	}
	for k, v := range resolved.Metadata {
		if out.Metadata == nil {
			out.Metadata = make(map[string]string)
		}
		out.Metadata[k] = v
	}
	return out
}

func cloneMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
