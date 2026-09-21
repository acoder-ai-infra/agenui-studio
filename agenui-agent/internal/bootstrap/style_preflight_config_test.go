package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicStyleAgentKeepsContractFirstCapabilityPreflight(t *testing.T) {
	root := filepath.Join("..", "..", "configs")
	assertFileContains(t, filepath.Join(root, "environments", "local", "agents", "agenui_style.yaml"),
		"agenui_preflight_capabilities", "agenui-knowrag")
	assertFileContains(t, filepath.Join(root, "harness", "catalogs", "tools.yaml"),
		"agenui_preflight_capabilities@1.0.0", "allowed_agents: [agenui_style, agenui_binder]",
		"name: agenui_workspace",
		"top_k: {type: integer, minimum: 1, maximum: 3, default: 1}")
	assertFileContains(t, filepath.Join(root, "harness", "catalogs", "mcp.local.yaml"),
		"- agenui_style", "- agenui_binder")
	assertFileContains(t, filepath.Join(root, "harness", "prompts.yaml"),
		"恰好调用一次 search_developer_apis", "{\"contract_source\":\"frozen\"}")
}

func TestRootAgentAnswersQuestionsWithoutStartingAnEdit(t *testing.T) {
	root := filepath.Join("..", "..", "configs")
	assertFileContains(t, filepath.Join(root, "environments", "local", "agents", "agenui_agent.yaml"),
		"prompt_version: 1.11.0", "agenui_publish_next_steps", "version: 1.3.0")
	assertFileContains(t, filepath.Join(root, "harness", "catalogs", "tools.yaml"),
		"agenui_publish_next_steps@1.3.0", "const: agenui.next_steps.v2", "allowed_agents: [agenui_agent]",
		"required: [label, description, prompt]", "const: 下一步建议")
	assertFileContains(t, filepath.Join(root, "harness", "prompts.yaml"),
		"用户询问当前卡片、数据源、算子、规则、生成过程、失败原因或使用方式时",
		"不调用生成、编辑或绑定工具",
		"请求与 AGenUI 界面生成及当前产物无关时",
		"极短的追问应结合上一条尚未回答的用户消息理解",
		"第一次发起工具调用的同一条 assistant 消息中",
		"不得只输出行动说明后结束本轮",
		"只要能形成最小内容契约就直接继续",
		"参考图片是设计与语义参考，不是待展示的数据源",
		"严禁从附件的文件名、尺寸、分辨率、格式、大小、色彩模式等文件元数据创建 contents",
		"下一步建议只通过 agenui_publish_next_steps 发布",
		"不得把阻塞问题留到下一步建议",
		"结合本轮实际生成结果给出一至三条可直接执行的建议",
		"把菜品主标题字号调大",
		"label、description 和 prompt",
		"delivery_mode=design_preview",
		"Style 提交成功后直接结束本轮，禁止调用 Binder",
		"不得复述子 Agent 的分析过程")
}

func TestStyleAgentTreatsInheritedImagesAsReferences(t *testing.T) {
	root := filepath.Join("..", "..", "configs")
	assertFileContains(t, filepath.Join(root, "environments", "local", "agents", "agenui_style.yaml"),
		"prompt_version: 1.3.0")
	assertFileContains(t, filepath.Join(root, "harness", "prompts.yaml"),
		"图片 Artifact", "布局层级、视觉节奏、配色和组件关系", "不得把图片当成可复制 DSL")
}

func assertFileContains(t *testing.T, path string, fragments ...string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, fragment := range fragments {
		if !strings.Contains(string(content), fragment) {
			t.Fatalf("%s does not contain %q", path, fragment)
		}
	}
}
