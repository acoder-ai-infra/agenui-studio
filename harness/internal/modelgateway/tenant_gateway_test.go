package modelgateway_test

import (
	"context"
	"errors"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type failingTenantResolver struct{ err error }

func (r failingTenantResolver) Facade(context.Context, string) (*mg.Facade, bool, error) {
	return nil, false, r.err
}

type fixedTenantResolver struct {
	facade *mg.Facade
	ok     bool
}

func (r fixedTenantResolver) Facade(context.Context, string) (*mg.Facade, bool, error) {
	return r.facade, r.ok, nil
}

func TestTenantGatewayDoesNotFallbackOnManagedResolverError(t *testing.T) {
	sentinel := errors.New("managed tenant config unavailable")
	provider := mock.New("default", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("must-not-run")}})
	gateway := &mg.TenantGateway{
		Default: &mg.Facade{
			Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Provider: "default", Model: "m"}}, nil),
			Providers: map[string]mg.ChatProvider{"default": provider},
		},
		Resolver: failingTenantResolver{err: sentinel},
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a"})

	checks := []struct {
		name string
		call func() error
	}{
		{name: "chat", call: func() error {
			_, err := gateway.Chat(ctx, mg.ModelRequest{RequestID: "r", Messages: []mg.ChatMessage{mg.TextMessage("user", "hello")}})
			return err
		}},
		{name: "embedding", call: func() error {
			_, err := gateway.Embedding(ctx, mg.EmbeddingRequest{RequestID: "e", Texts: []string{"hello"}})
			return err
		}},
		{name: "rerank", call: func() error {
			_, err := gateway.Rerank(ctx, mg.RerankRequest{RequestID: "rr", Query: "hello", Documents: []string{"world"}})
			return err
		}},
		{name: "judge", call: func() error {
			_, err := gateway.Judge(ctx, mg.JudgeRequest{RequestID: "j", Input: "hello"})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); !errors.Is(err, sentinel) {
				t.Fatalf("error=%v, want resolver error", err)
			}
		})
	}
	if provider.Calls() != 0 {
		t.Fatalf("static default was invoked after managed error: calls=%d", provider.Calls())
	}
}

func TestTenantGatewayFallsBackOnlyOnExplicitManagedMiss(t *testing.T) {
	provider := mock.New("default", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}})
	fallback := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Provider: "default", Model: "m"}}, nil),
		Providers: map[string]mg.ChatProvider{"default": provider},
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a"})
	gateway := &mg.TenantGateway{Default: fallback, Resolver: fixedTenantResolver{}}
	call, err := gateway.Chat(ctx, mg.ModelRequest{RequestID: "r", Messages: []mg.ChatMessage{mg.TextMessage("user", "hello")}})
	if err != nil {
		t.Fatal(err)
	}
	for range call.Events {
	}
	if response, err := call.Await(); err != nil || response.Status != mg.StatusSuccess {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if provider.Calls() != 1 {
		t.Fatalf("explicit miss did not use static fallback: calls=%d", provider.Calls())
	}

	for _, inconsistent := range []fixedTenantResolver{{facade: fallback, ok: false}, {facade: nil, ok: true}} {
		gateway.Resolver = inconsistent
		if _, err := gateway.Chat(ctx, mg.ModelRequest{RequestID: "bad"}); err == nil {
			t.Fatalf("inconsistent resolver state was treated as a miss: %#v", inconsistent)
		}
	}
	if provider.Calls() != 1 {
		t.Fatalf("inconsistent resolver reached fallback: calls=%d", provider.Calls())
	}
}
