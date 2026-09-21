package modelgateway_test

import (
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
)

// gateway_output_options_test.go 覆盖 G-A/G-B 在网关内部的透传：
// ModelRequest.Options 的 ResponseFormat / ImageDetail 必须原样到达
// provider 的 AdapterRequest.Options，否则 agents.yaml 的配置永远
// 到不了请求体。

func TestFacadePassesOutputOptionsToProvider(t *testing.T) {
	provider := &recordingChatProvider{delegate: mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
		mock.TokenChunk("ok"), mock.UsageChunk(1, 1),
	}})}
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "m", Provider: "mock",
			// response_format 需要 structured_output 能力声明。
			Capability: mg.ModelCapability{Model: "m", Provider: "mock", Chat: true, StructuredOutput: true},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
	}
	_, response := collect(t, gw, mg.ModelRequest{
		RequestID: "output-options",
		Options:   mg.ModelOptions{ResponseFormat: "json_object", ImageDetail: "low"},
	})
	if response.Status != mg.StatusSuccess {
		t.Fatalf("call must succeed, got %#v", response)
	}
	if got := provider.request.Options.ResponseFormat; got != "json_object" {
		t.Fatalf("provider must receive response_format, got %q", got)
	}
	if got := provider.request.Options.ImageDetail; got != "low" {
		t.Fatalf("provider must receive image_detail, got %q", got)
	}
}

// 未配置时两字段保持空：不得凭空下发 provider 参数。
func TestFacadeOmitsUnsetOutputOptions(t *testing.T) {
	provider := &recordingChatProvider{delegate: mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
		mock.TokenChunk("ok"), mock.UsageChunk(1, 1),
	}})}
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "m", Provider: "mock",
			Capability: mg.ModelCapability{Model: "m", Provider: "mock", Chat: true},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
	}
	if _, response := collect(t, gw, mg.ModelRequest{RequestID: "no-output-options"}); response.Status != mg.StatusSuccess {
		t.Fatalf("call must succeed, got %#v", response)
	}
	if provider.request.Options.ResponseFormat != "" || provider.request.Options.ImageDetail != "" {
		t.Fatalf("unset output options must stay empty, got %#v", provider.request.Options)
	}
}

// G-A 能力门禁：未声明 structured_output 的模型配了 response_format 时
// 必须 fail closed，不得静默发出模型不支持的参数。
func TestFacadeRejectsResponseFormatWithoutStructuredOutputCapability(t *testing.T) {
	provider := &recordingChatProvider{delegate: mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
		mock.TokenChunk("ok"), mock.UsageChunk(1, 1),
	}})}
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "plain", Provider: "mock",
			Capability: mg.ModelCapability{Model: "plain", Provider: "mock", Chat: true},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
	}
	_, response := collect(t, gw, mg.ModelRequest{
		RequestID: "unsupported-format",
		Options:   mg.ModelOptions{ResponseFormat: "json_object"},
	})
	if response.Status != mg.StatusFailed {
		t.Fatalf("response_format on a non-structured-output model must fail closed, got %#v", response)
	}
	if provider.request.Model != "" {
		t.Fatalf("unsupported request must not reach the provider: %#v", provider.request)
	}
}
