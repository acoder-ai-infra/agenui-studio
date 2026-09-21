package app

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

// native 路径：transformer 按 Definition 声明的 ID 在每轮模型调用前改写
// ModelInvokeRequest，Round 透传、空 window 自动物化、Parts 与工具配对字段
// 双向保真；未声明的 agent 直通。
func TestNewExtensionNativeModelTransform(t *testing.T) {
	hook := &stubHook{id: "rw", fn: func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		messages := append([]extension.BeforeModelMessage(nil), req.Messages...)
		messages = append(messages, extension.BeforeModelMessage{Role: "user", Content: "追加"})
		return extension.BeforeModelResult{Messages: messages}, nil
	}}
	catalog, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{
		{ID: "rw", Kind: kernel.ExtBeforeModelHook, Implementation: hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	transform := newExtensionNativeModelTransform(catalog, kernel.TurnEnvironment{Environment: "local"})
	if transform == nil {
		t.Fatal("transform must always be installed (binding is per-agent)")
	}
	boundDef := agentruntime.AgentDefinition{AgentID: "agent_native", BeforeModelHooks: []string{"rw"}}
	req := agentruntime.ModelInvokeRequest{
		Round: 2,
		Package: agentruntime.ModelContextPackage{Messages: agentruntime.ModelContextMessages{ConversationWindow: []agentruntime.ModelContextMessage{
			{Role: "user", Content: "看图", ContentParts: []agentruntime.ModelContentPart{
				{Type: "text", Text: "看图"},
				{Type: "image_url", URL: "artifact://t/img1", MIMEType: "image/png"},
			}},
		}}},
	}
	out, err := transform(context.Background(), boundDef, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 || out.Messages[1].Content != "追加" {
		t.Fatalf("messages = %#v", out.Messages)
	}
	if len(out.Messages[0].ContentParts) != 2 || out.Messages[0].ContentParts[1].URL != "artifact://t/img1" {
		t.Fatalf("parts must survive round trip: %#v", out.Messages[0].ContentParts)
	}
	if hook.rounds[0] != 2 {
		t.Fatalf("round must propagate: %v", hook.rounds)
	}

	// 未声明 transformer 的 agent：原样直通，不调用任何实现。
	callsBefore := len(hook.rounds)
	passthrough, err := transform(context.Background(), agentruntime.AgentDefinition{AgentID: "agent_plain"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(passthrough.Messages) != 0 {
		t.Fatalf("undeclared agent must pass through untouched: %#v", passthrough.Messages)
	}
	if len(hook.rounds) != callsBefore {
		t.Fatal("undeclared agent must not invoke transformers")
	}

	// 未知 ID fail closed。
	if _, err := transform(context.Background(), agentruntime.AgentDefinition{AgentID: "agent_bad", BeforeModelHooks: []string{"missing"}}, req); err == nil {
		t.Fatal("unknown transformer id must fail closed")
	}
}

// 项 2 尾巴：artifact:// 图片引用在送 provider 前解引用为 data URI。
// 此处覆盖 resolveArtifactImageParts 的路由语义：非 artifact:// 引用与
// data URI 原样透传、artifact 引用在无 artifact store 时 fail closed；
// 完整解引用行为由宿主接入验收基线（方案 4.2）端到端覆盖。
func TestResolveArtifactImagePartsRouting(t *testing.T) {
	invoker := &gatewayModelInvoker{}
	// data URI 与 http URL 不触发解引用，artifacts 为 nil 也不报错。
	messages := []agentruntime.ModelCallMessage{{
		Role: "user",
		ContentParts: []agentruntime.ModelContentPart{
			{Type: "image_url", URL: "data:image/png;base64,QUJD"},
			{Type: "image_url", URL: "https://example.com/a.png"},
			{Type: "text", Text: "hi"},
		},
	}}
	out, err := invoker.resolveArtifactParts(context.Background(), agentruntime.ModelInvokeRequest{}, messages)
	if err != nil {
		t.Fatalf("passthrough must not fail: %v", err)
	}
	if out[0].ContentParts[0].URL != "data:image/png;base64,QUJD" {
		t.Fatalf("data URI must pass through: %#v", out[0].ContentParts)
	}
	// artifact:// 引用在无 artifact store 时 fail closed。
	messages[0].ContentParts = append(messages[0].ContentParts, agentruntime.ModelContentPart{Type: "image_url", URL: "artifact://t/img1"})
	if _, err := invoker.resolveArtifactParts(context.Background(), agentruntime.ModelInvokeRequest{}, messages); err == nil {
		t.Fatal("artifact image without store must fail closed")
	}
}
