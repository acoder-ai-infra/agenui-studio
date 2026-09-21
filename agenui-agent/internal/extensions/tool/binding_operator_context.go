package tool

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

var ErrBindingOperatorContextUnavailable = errors.New(
	"binding operator context is unavailable",
)

const (
	BindingDependenciesAgentVersion         = "1.1.1"
	BindingDependenciesPreviousAgentVersion = "1.1.0"
)

// BindingOperatorContextResolver derives the Binder parent from Harness-owned
// ParentRunID/RootRunID facts. It keeps no process-local Run registry.
type BindingOperatorContextResolver struct {
	parentID  string
	parentVer string
}

func NewBindingOperatorContextResolver(
	parentAgentID string,
	parentAgentVersion string,
) (*BindingOperatorContextResolver, error) {
	if !validBindingOperatorIdentity(parentAgentID) ||
		!validBindingOperatorIdentity(parentAgentVersion) {
		return nil, ErrBindingOperatorContextUnavailable
	}
	return &BindingOperatorContextResolver{
		parentID:  parentAgentID,
		parentVer: parentAgentVersion,
	}, nil
}

// BeginBindingOperatorScope is retained as a compatibility no-op for callers
// compiled against the earlier interface. Harness now owns the relationship.
func (r *BindingOperatorContextResolver) BeginBindingOperatorScope(
	parent extension.Context,
) (func(), error) {
	if _, valid := r.normalizeBindingOperatorParent(parent); !valid {
		return nil, ErrBindingOperatorContextUnavailable
	}
	return func() {}, nil
}

func (r *BindingOperatorContextResolver) ResolveExecutionMaterial(
	ctx context.Context,
	child extension.Context,
) (BindingExecutionMaterial, error) {
	parent, err := r.ResolveBindingParentScope(ctx, child)
	if err != nil {
		return BindingExecutionMaterial{}, err
	}
	return BindingExecutionMaterial{ArtifactRunID: parent.RunID}, nil
}

// ResolveBindingParentScope exposes the immutable relationship frozen by
// Harness. It never consults Session latest or model arguments.
func (r *BindingOperatorContextResolver) ResolveBindingParentScope(
	ctx context.Context,
	child extension.Context,
) (extension.Context, error) {
	if ctx == nil {
		return extension.Context{}, ErrBindingOperatorContextUnavailable
	}
	if err := ctx.Err(); err != nil {
		return extension.Context{}, err
	}
	if r == nil {
		return extension.Context{}, ErrBindingOperatorContextUnavailable
	}
	var valid bool
	child, valid = r.normalizeBindingOperatorChild(child)
	if !valid {
		return extension.Context{}, ErrBindingOperatorContextUnavailable
	}

	parentRunID := strings.TrimSpace(child.ParentRunID)
	if parentRunID == "" {
		parentRunID = strings.TrimSpace(child.RootRunID)
	}
	if !validBindingOperatorIdentity(parentRunID) || parentRunID == child.RunID {
		return extension.Context{}, ErrBindingOperatorContextUnavailable
	}
	return extension.Context{
		TenantID: child.TenantID, UserID: child.UserID,
		SessionID: child.SessionID, RunID: parentRunID,
		RootRunID: parentRunID, AgentID: r.parentID,
		AgentVersion: r.parentVer, TraceID: child.TraceID,
	}, nil
}

// BindBindingOperatorChild validates that Harness supplied a usable parent.
func (r *BindingOperatorContextResolver) BindBindingOperatorChild(
	ctx context.Context,
	child extension.Context,
) error {
	_, err := r.ResolveBindingParentScope(ctx, child)
	return err
}

func (r *BindingOperatorContextResolver) normalizeBindingOperatorParent(
	value extension.Context,
) (extension.Context, bool) {
	if r == nil || value.AgentID != r.parentID ||
		(value.AgentVersion != "" && value.AgentVersion != r.parentVer) {
		return extension.Context{}, false
	}
	value.AgentVersion = r.parentVer
	return value, validBindingOperatorParent(value)
}

func (r *BindingOperatorContextResolver) normalizeBindingOperatorChild(
	value extension.Context,
) (extension.Context, bool) {
	if r == nil || value.AgentID != BindingDependenciesAgentID ||
		(value.AgentVersion != "" && !validBindingOperatorIdentity(value.AgentVersion)) {
		return extension.Context{}, false
	}
	if value.AgentVersion == "" {
		value.AgentVersion = BindingDependenciesAgentVersion
	}
	return value, validBindingOperatorChild(value)
}

func validBindingOperatorChildCandidate(value extension.Context) bool {
	if value.AgentVersion == "" {
		value.AgentVersion = BindingDependenciesAgentVersion
	}
	return validBindingOperatorChild(value)
}

func bindingDependenciesAgentVersion(value string) bool {
	return value == BindingDependenciesAgentVersion ||
		value == BindingDependenciesPreviousAgentVersion
}

func validBindingOperatorParent(value extension.Context) bool {
	return validBindingOperatorIdentity(value.TenantID) &&
		validBindingOperatorIdentity(value.UserID) &&
		validBindingOperatorIdentity(value.SessionID) &&
		validBindingOperatorIdentity(value.RunID) &&
		validBindingOperatorIdentity(value.AgentID) &&
		validBindingOperatorIdentity(value.AgentVersion) &&
		validBindingOperatorIdentity(value.TraceID)
}

func validBindingOperatorChild(value extension.Context) bool {
	return validBindingOperatorIdentity(value.TenantID) &&
		validBindingOperatorIdentity(value.UserID) &&
		validBindingOperatorIdentity(value.SessionID) &&
		validBindingOperatorIdentity(value.RunID) &&
		value.AgentID == BindingDependenciesAgentID &&
		validBindingOperatorIdentity(value.AgentVersion) &&
		validBindingOperatorIdentity(value.TraceID)
}

func validBindingOperatorIdentity(value string) bool {
	return value != "" && len(value) <= maxBindingIdentityBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

var _ BindingContextResolver = (*BindingOperatorContextResolver)(nil)
