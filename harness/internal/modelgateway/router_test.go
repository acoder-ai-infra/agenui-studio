package modelgateway_test

import (
	"context"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

func TestStaticRouter(t *testing.T) {
	r := mg.NewStaticRouter(
		mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}},
		map[string]mg.Route{
			"travel_agent": {Primary: mg.ModelTarget{Model: "qwen-max", Provider: "dashscope"}},
		},
	)

	got, err := r.Route(context.Background(), mg.ModelRequest{AgentID: "travel_agent"})
	if err != nil || got.Primary.Provider != "dashscope" {
		t.Fatalf("agent route = %+v err=%v", got, err)
	}

	def, err := r.Route(context.Background(), mg.ModelRequest{AgentID: "unknown"})
	if err != nil || def.Primary.Provider != "mock" {
		t.Fatalf("default route = %+v err=%v", def, err)
	}
}

func TestStaticRouterNoDefault(t *testing.T) {
	r := mg.NewStaticRouter(mg.Route{}, nil)
	if _, err := r.Route(context.Background(), mg.ModelRequest{AgentID: "x"}); err == nil {
		t.Fatal("expected error when no route and no default")
	}
}
