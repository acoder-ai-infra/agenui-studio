package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

func TestPublishNextStepsReturnsStableHostOwnedContract(t *testing.T) {
	tool := &PublishNextSteps{}
	call := extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
			RunID: "run-1", StepID: "step-next", AgentID: agenuiextensions.MainAgent,
		},
		Name: PublishNextStepsName,
		Arguments: json.RawMessage(`{
			"schema_version":"agenui.next_steps.v2",
			"items":[
				{"label":" 补充分区标题字段 ","description":" 使用优惠数据源中的真实分类名称 ","prompt":" 请补充分区标题字段并重新完成绑定 "},
				{"label":"把卡片主标题加粗","description":"当前主标题与正文层级接近，加粗后更容易识别主题","prompt":"请把卡片主标题字重调高，其他文字、颜色和布局保持不变"}
			]
		}`),
	}

	first, err := tool.Invoke(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tool.Invoke(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Data) != string(second.Data) {
		t.Fatalf("idempotent output drifted:\n%s\n%s", first.Data, second.Data)
	}
	var view nextStepsView
	if err := json.Unmarshal(first.Data, &view); err != nil {
		t.Fatal(err)
	}
	if view.SchemaVersion != NextStepsSchemaVersion || !strings.HasPrefix(view.PlanID, "next_") || view.Title != NextStepsTitle {
		t.Fatalf("unexpected envelope: %#v", view)
	}
	if len(view.Items) != 2 || view.Items[0].ID != view.PlanID+"_1" || view.Items[0].Label != "补充分区标题字段" {
		t.Fatalf("unexpected items: %#v", view.Items)
	}
	if view.Items[0].Prompt != "请补充分区标题字段并重新完成绑定" || view.Items[1].Prompt != "请把卡片主标题字重调高，其他文字、颜色和布局保持不变" {
		t.Fatalf("normalization failed: %#v", view.Items)
	}
	if view.Items[0].Description != "使用优惠数据源中的真实分类名称" || view.Items[1].Description != "当前主标题与正文层级接近，加粗后更容易识别主题" {
		t.Fatalf("description normalization failed: %#v", view.Items)
	}
	if len(first.Data) > 4096 {
		t.Fatalf("next-steps contract exceeds SSE preview budget: %d", len(first.Data))
	}
	if first.Presentation == nil || first.Presentation.Title == "" {
		t.Fatal("user-facing completion presentation is missing")
	}
	if first.Presentation.Title != NextStepsTitle {
		t.Fatalf("unexpected presentation title: %q", first.Presentation.Title)
	}
	if first.Presentation.Summary != NextStepsSchemaVersion+":"+view.PlanID || len(first.Presentation.Details) != 2 {
		t.Fatalf("unexpected durable presentation: %#v", first.Presentation)
	}
	if first.Presentation.Details[0].Label != "补充分区标题字段" {
		t.Fatalf("unexpected presentation item: %#v", first.Presentation.Details[0])
	}
	var presentationValue nextStepsPresentationValue
	if err := json.Unmarshal([]byte(first.Presentation.Details[0].Value), &presentationValue); err != nil {
		t.Fatalf("decode presentation item: %v", err)
	}
	if presentationValue.Description != "使用优惠数据源中的真实分类名称" || presentationValue.Prompt != "请补充分区标题字段并重新完成绑定" {
		t.Fatalf("unexpected presentation value: %#v", presentationValue)
	}
	presentationJSON, err := json.Marshal(first.Presentation)
	if err != nil {
		t.Fatal(err)
	}
	if len(presentationJSON) > nextStepsMaxPresentationBytes {
		t.Fatalf("presentation exceeds durable process preview budget: %d", len(presentationJSON))
	}
}

func TestPublishNextStepsPublishesThreeChineseActionsWithoutFailing(t *testing.T) {
	tool := &PublishNextSteps{}
	result, err := tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{AgentID: agenuiextensions.MainAgent},
		Name: PublishNextStepsName,
		Arguments: json.RawMessage(`{
			"schema_version":"agenui.next_steps.v2",
			"items":[
				{"label":"修正分区标题绑定","description":"改用真实分类名称字段","prompt":"把分区标题从 catalog_url 改为优惠数据源中真实的分类名称字段"},
				{"label":"检查移动端预览效果","description":"确认文字和分隔线在窄屏下正常","prompt":"请在移动端视口下预览这张优惠列表卡，确认文字不裁切且分隔线位置正确"},
				{"label":"接入真实优惠数据源","description":"保持现有算子和动作绑定不变","prompt":"将优惠列表的数据源切换为真实接口，并保持金额算子和动作绑定不变"}
			]
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Presentation == nil || len(result.Presentation.Details) != 3 {
		t.Fatalf("presentation missing: %#v", result.Presentation)
	}
	presentationJSON, err := encodeNextStepsPresentation(result.Presentation)
	if err != nil {
		t.Fatal(err)
	}
	if len(presentationJSON) > nextStepsMaxPresentationBytes {
		t.Fatalf("presentation exceeds budget: %d", len(presentationJSON))
	}
}

func TestPublishNextStepsAcceptsConcreteSuggestionsFromGeneratedCard(t *testing.T) {
	tool := &PublishNextSteps{}
	result, err := tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			SessionID: "session-generated", RunID: "run-generated", StepID: "step-generated",
			AgentID: agenuiextensions.MainAgent,
		},
		Name: PublishNextStepsName,
		Arguments: json.RawMessage(`{
			"schema_version":"agenui.next_steps.v2",
			"items":[
				{"label":"给菜品名称加上描述文字","description":"当前卡片只展示了菜品名称、图片和价格，名称下方有空白区域可以放置菜品描述（如口味、配料），让用户了解更多信息再决定是否查看详情。","prompt":"在菜品名称下方、价格上方新增一行菜品描述文字，内容来自数据源的描述字段，字体小一些、颜色用灰色。其余部分保持不变。"},
				{"label":"把查看详情按钮改为加购按钮","description":"当前底部是橙色查看详情按钮，如果业务目标是促成下单，可以改为加入购物车动作，并保持同样的橙色主题风格。","prompt":"将底部查看详情按钮改为加入购物车按钮，动作改为加购，颜色和样式保持不变。其余部分保持不变。"},
				{"label":"给菜品图片右上角加收藏按钮","description":"当前菜品图片占据卡片顶部，右上角空间充足，适合叠加一个收藏按钮，方便用户快速标记喜欢的菜品。","prompt":"在菜品图片右上角叠加一个收藏图标按钮，点击可切换收藏状态。其余部分保持不变。"}
			]
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Presentation == nil || len(result.Presentation.Details) != 3 {
		t.Fatalf("presentation missing: %#v", result.Presentation)
	}
	for index, detail := range result.Presentation.Details {
		var value nextStepsPresentationValue
		if err := json.Unmarshal([]byte(detail.Value), &value); err != nil {
			t.Fatalf("decode item %d: %v", index, err)
		}
		if value.Description == "" {
			t.Fatalf("description %d was dropped: %#v", index, detail)
		}
	}
}

func TestPublishNextStepsCompactsVerbosePromptsToActionLabels(t *testing.T) {
	tool := &PublishNextSteps{}
	longDescriptions := []string{
		strings.Repeat("菜", nextStepsMaxDescriptionRunes),
		strings.Repeat("端", nextStepsMaxDescriptionRunes),
		strings.Repeat("验", nextStepsMaxDescriptionRunes),
	}
	arguments, err := json.Marshal(nextStepsDraft{
		SchemaVersion: NextStepsSchemaVersion,
		Items: []nextStepDraft{
			{Label: "接入真实菜品列表数据", Description: longDescriptions[0], Prompt: strings.Repeat("改", nextStepsMaxPromptRunes)},
			{Label: "把卡片内边距调大", Description: longDescriptions[1], Prompt: strings.Repeat("改", nextStepsMaxPromptRunes)},
			{Label: "验证购买按钮动作", Description: longDescriptions[2], Prompt: strings.Repeat("改", nextStepsMaxPromptRunes)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			SessionID: "session-compact", RunID: "run-compact", StepID: "step-compact",
			AgentID: agenuiextensions.MainAgent,
		},
		Name:      PublishNextStepsName,
		Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Presentation == nil || len(result.Presentation.Details) != 3 {
		t.Fatalf("presentation missing: %#v", result.Presentation)
	}
	for index, detail := range result.Presentation.Details {
		var value nextStepsPresentationValue
		if err := json.Unmarshal([]byte(detail.Value), &value); err != nil {
			t.Fatalf("decode compacted presentation value: %v", err)
		}
		if value.Prompt != detail.Label {
			t.Fatalf("verbose prompt was not compacted: %#v", detail)
		}
		if value.Description != longDescriptions[index] {
			t.Fatalf("description was not preserved: %#v", detail)
		}
	}
	presentationJSON, err := encodeNextStepsPresentation(result.Presentation)
	if err != nil {
		t.Fatal(err)
	}
	if len(presentationJSON) > nextStepsMaxPresentationBytes {
		t.Fatalf("compacted presentation exceeds budget: %d", len(presentationJSON))
	}
}

func TestPublishNextStepsRejectsNonRootCaller(t *testing.T) {
	tool := &PublishNextSteps{}
	arguments := json.RawMessage(`{"schema_version":"agenui.next_steps.v2","items":[{"label":"调整样式","prompt":"请调整样式"}]}`)
	for _, callContext := range []extension.Context{
		{AgentID: agenuiextensions.StyleAgent},
		{AgentID: agenuiextensions.MainAgent, ParentRunID: "run-parent"},
	} {
		if _, err := tool.Invoke(context.Background(), extension.FunctionCall{
			Ctx: callContext, Name: PublishNextStepsName, Arguments: arguments,
		}); err == nil {
			t.Fatalf("unauthorized context accepted: %#v", callContext)
		}
	}
}

func TestPublishNextStepsRejectsMalformedContract(t *testing.T) {
	tool := &PublishNextSteps{}
	ctx := extension.Context{AgentID: agenuiextensions.MainAgent}
	for _, arguments := range []string{
		`{"schema_version":"agenui.next_steps.v1","items":[{"label":"调整样式"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"importance":"optional","label":"调整样式"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"label":" ","prompt":"请调整样式"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"label":"把主标题字号调大","prompt":"请把主标题字号调大"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"label":"把主标题字号调大","description":"当前标题层级不明显"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"label":"调整卡片样式","description":"当前卡片需要优化","prompt":"请调整卡片样式"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[],"extra":true}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"label":"后续一","prompt":"` + strings.Repeat("很", nextStepsMaxPromptRunes+1) + `"}]}`,
		`{"schema_version":"agenui.next_steps.v2","items":[{"label":"后续一","description":"` + strings.Repeat("长", nextStepsMaxDescriptionRunes+1) + `"}]}`,
	} {
		if _, err := tool.Invoke(context.Background(), extension.FunctionCall{
			Ctx: ctx, Name: PublishNextStepsName, Arguments: json.RawMessage(arguments),
		}); err == nil {
			t.Fatalf("malformed contract accepted: %s", arguments)
		}
	}
}
