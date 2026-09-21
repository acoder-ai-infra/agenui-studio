package runtime

import (
	"context"
	"os"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
)

func TestStaticProviderCurrentAndGet(t *testing.T) {
	t.Parallel()
	repository, err := designknowledge.Load(
		context.Background(),
		os.DirFS("../../../configs/design-public"),
		"revisions/demo-v1",
	)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewStaticProvider(repository)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Current() != repository {
		t.Fatal("Current() did not return repository")
	}
	info := provider.CurrentInfo()
	if info.RevisionID != repository.RevisionID() ||
		info.RevisionHash != repository.RevisionHash() ||
		info.Source != SourceLocalFallback {
		t.Fatalf("CurrentInfo() = %#v", info)
	}
	got, ok := provider.Get(repository.RevisionID(), repository.RevisionHash())
	if !ok || got != repository {
		t.Fatalf("Get() = %#v, %v", got, ok)
	}
	if got, ok := provider.Get(repository.RevisionID(), "sha256:missing"); ok || got != nil {
		t.Fatalf("Get() mismatch = %#v, %v", got, ok)
	}
}

func TestStaticProviderRequiresRepository(t *testing.T) {
	t.Parallel()
	if _, err := NewStaticProvider(nil); err == nil {
		t.Fatal("expected nil repository error")
	}
}
