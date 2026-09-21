package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

func TestBindingOperatorContextResolverScopesHarnessTrace(t *testing.T) {
	resolver, err := NewBindingOperatorContextResolver("agenui_agent", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	parent := extension.Context{
		InvocationKind: extension.InvocationRoot,
		TenantID:       "public", UserID: "local", SessionID: "session-a",
		RunID: "root-a", AgentID: "agenui_agent", AgentVersion: "1.0.0", TraceID: "trace-a",
	}
	release, err := resolver.BeginBindingOperatorScope(parent)
	if err != nil {
		t.Fatal(err)
	}
	child := extension.Context{
		InvocationKind: extension.InvocationSubAgent,
		TenantID:       "public", UserID: "local", SessionID: "session-a",
		RunID: "child-a", ParentRunID: "root-a", RootRunID: "root-a",
		AgentID: BindingDependenciesAgentID, AgentVersion: "1.0.0", TraceID: "trace-a",
	}
	material, err := resolver.ResolveExecutionMaterial(context.Background(), child)
	if err != nil || material.ArtifactRunID != parent.RunID {
		t.Fatalf("material = %#v, %v", material, err)
	}
	missingParent := child
	missingParent.ParentRunID = ""
	missingParent.RootRunID = ""
	if _, err := resolver.ResolveExecutionMaterial(context.Background(), missingParent); !errors.Is(err, ErrBindingOperatorContextUnavailable) {
		t.Fatalf("missing parent error = %v", err)
	}
	release()
	if material, err := resolver.ResolveExecutionMaterial(context.Background(), child); err != nil || material.ArtifactRunID != parent.RunID {
		t.Fatalf("Harness relationship changed after release: %#v, %v", material, err)
	}
}

func TestBindingOperatorContextResolverRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewBindingOperatorContextResolver("", "1.0.0"); !errors.Is(err, ErrBindingOperatorContextUnavailable) {
		t.Fatalf("error = %v", err)
	}
}
