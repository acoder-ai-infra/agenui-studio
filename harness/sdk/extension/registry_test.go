package extension

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type stubIdentityResolver struct{}

func (stubIdentityResolver) Resolve(context.Context, IdentityRequest) (ResolvedIdentity, error) {
	return ResolvedIdentity{TenantID: "t"}, nil
}

type stubFunctionTool struct{ name string }

func (t stubFunctionTool) Name() string { return t.name }
func (t stubFunctionTool) Invoke(context.Context, FunctionCall) (*FunctionResult, error) {
	return &FunctionResult{Data: json.RawMessage(`{}`)}, nil
}

type stubToolProvider struct{}

func (stubToolProvider) FunctionTools() []FunctionTool {
	return []FunctionTool{stubFunctionTool{name: "echo"}}
}

func TestRegistryRegisterAndResolve(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("app.identity", KindIdentityResolver, stubIdentityResolver{}); err != nil {
		t.Fatalf("register identity: %v", err)
	}
	if err := r.Register("app.echo_tool", KindToolProvider, stubToolProvider{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	r.Freeze("build-seed-1")

	impl, err := r.Resolve("app.identity", KindIdentityResolver)
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if _, ok := impl.Impl.(IdentityResolver); !ok {
		t.Fatalf("resolved impl is not IdentityResolver: %T", impl.Impl)
	}
	if impl.Fingerprint == "" {
		t.Fatal("Freeze must populate Fingerprint")
	}

	if _, err := r.Resolve("app.identity", KindToolProvider); !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("kind mismatch: want ErrKindMismatch, got %v", err)
	}
	if _, err := r.Resolve("nope", KindIdentityResolver); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("unknown id: want ErrUnknownID, got %v", err)
	}
}

func TestRegistryFrozenRejectsRegister(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("id.only", KindIdentityResolver, stubIdentityResolver{}); err != nil {
		t.Fatal(err)
	}
	r.Freeze("")
	if err := r.Register("id.two", KindIdentityResolver, stubIdentityResolver{}); err == nil {
		t.Fatal("frozen registry should reject Register")
	}
}

func TestRegistryDuplicateID(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("dup", KindIdentityResolver, stubIdentityResolver{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("dup", KindIdentityResolver, stubIdentityResolver{}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("want ErrDuplicateID, got %v", err)
	}
}

func TestRegistryFingerprintStable(t *testing.T) {
	build := func() string {
		r := NewRegistry()
		_ = r.Register("app.identity", KindIdentityResolver, stubIdentityResolver{})
		_ = r.Register("app.echo_tool", KindToolProvider, stubToolProvider{})
		r.Freeze("stable-seed")
		impl, _ := r.Resolve("app.identity", KindIdentityResolver)
		return impl.Fingerprint
	}
	if build() != build() {
		t.Fatalf("Fingerprint should be stable across identical builds")
	}
}

func TestPhaseOfKnownKinds(t *testing.T) {
	kinds := []Kind{
		KindIdentityResolver, KindRunInitializer, KindContextContributor,
		KindInputNormalizer, KindToolProvider, KindOutputValidator,
		KindProtocolProjector, KindEventObserver,
		KindBeforeModelHook, KindToolCallInterceptor,
	}
	seen := make(map[Phase]bool)
	for _, k := range kinds {
		p := PhaseOf(k)
		if p == 0 {
			t.Fatalf("kind %q has no phase", k)
		}
		if seen[p] {
			t.Fatalf("phase %d assigned twice", p)
		}
		seen[p] = true
	}
	if PhaseOf(Kind("unknown")) != 0 {
		t.Fatal("unknown kind must return phase 0")
	}
}

func TestContextDecodeConfig(t *testing.T) {
	type payload struct {
		N int `json:"n"`
	}
	c := Context{Config: json.RawMessage(`{"n":7}`)}
	var got payload
	if err := c.DecodeConfig(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.N != 7 {
		t.Fatalf("want 7, got %d", got.N)
	}
	// Empty config is a nop.
	empty := Context{}
	if err := empty.DecodeConfig(&got); err != nil {
		t.Fatalf("empty decode: %v", err)
	}
}
