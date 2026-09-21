package designknowledge_test

import (
	"context"
	"os"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
)

func TestPublicDemoDesignKnowledgeLoadsWithoutPrivateAssets(t *testing.T) {
	repository, err := designknowledge.Load(context.Background(), os.DirFS("../.."), "configs/design-public/revisions/demo-v1")
	if err != nil {
		t.Fatalf("public demo baseline must be self-contained: %v", err)
	}
	if repository.RevisionID() != "demo-v1" {
		t.Fatalf("revision = %q", repository.RevisionID())
	}
}
