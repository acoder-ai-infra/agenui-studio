package documentcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const markerStart = "<!-- agenui-document-contract\n"
const markerEnd = "\n-->"

func Render(document Document) ([]byte, error) {
	document.Normalize()
	if err := document.Validate(); err != nil {
		return nil, err
	}
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	contract := strings.TrimSuffix(payload.String(), "\n")
	var output strings.Builder
	fmt.Fprintf(&output, "# %s\n\n%s%s%s\n\n", document.Title, markerStart, contract, markerEnd)
	fmt.Fprintf(&output, "## 基本信息\n\n- 文档 ID：`%s`\n- 版本：`%s`\n- 类型：`%s`\n- 摘要：%s\n\n", document.ID, document.Version, document.Kind, document.Summary)
	fmt.Fprintf(&output, "## 适用与排除\n\n- 适用：%s\n- 不适用：%s\n\n", join(document.AppliesTo), join(document.NotFor))
	switch document.DocumentSchema {
	case LayoutSchema:
		renderLayout(&output, *document.Layout)
	case ElementSchema:
		renderElement(&output, *document.Element)
	case RuleSchema:
		renderRules(&output, document.Rules)
	}
	return []byte(output.String()), nil
}

func join(values []string) string {
	if len(values) == 0 {
		return "无"
	}
	return strings.Join(values, "；")
}

func renderLayout(output *strings.Builder, body LayoutBody) {
	output.WriteString("## 节点拓扑\n\n| 节点 ID | 父节点 | 类型 | 可选 | 容量 |\n|---|---|---|---:|---|\n")
	for _, node := range body.Nodes {
		fmt.Fprintf(output, "| `%s` | `%s` | `%s` | %t | `%s` |\n", node.ID, node.ParentID, node.Type, node.Optional, node.Capacity)
	}
	output.WriteString("\n## 关系与容量\n\n| 类型 | From | To | 强度 | 说明 |\n|---|---|---|---|---|\n")
	for _, relation := range body.Relations {
		fmt.Fprintf(output, "| `%s` | `%s` | `%s` | `%s` | %s |\n", relation.Type, relation.From, relation.To, relation.Strength, relation.Description)
	}
	output.WriteString("\n## 设计配方\n\n")
	for _, recipe := range body.Recipes {
		fmt.Fprintf(output, "### %s\n\n- 配方 ID：`%s`\n- 适用：%s\n- 不适用：%s\n\n", recipe.Name, recipe.ID, recipe.AppliesWhen, recipe.NotFor)
		for _, instruction := range recipe.Instructions {
			fmt.Fprintf(output, "- %s\n", instruction)
		}
		output.WriteString("\n")
	}
	output.WriteString("## 正反例\n\n### 正例\n\n")
	for _, value := range body.PositiveExamples {
		fmt.Fprintf(output, "- %s\n", value)
	}
	output.WriteString("\n### 反例\n\n")
	for _, value := range body.NegativeExamples {
		fmt.Fprintf(output, "- %s\n", value)
	}
}

func renderElement(output *strings.Builder, body ElementBody) {
	fmt.Fprintf(output, "## 元素定义\n\n- 用途：%s\n- Catalog 组件：%s\n\n", body.Purpose, join(body.CatalogComponents))
	fmt.Fprintf(output, "## 规则与状态\n\n- 元素规则：%s\n- 状态：%s\n- 互斥元素：%s\n- 可访问性：%s\n", join(body.Rules), join(body.States), join(body.MutuallyExclusiveWith), join(body.Accessibility))
}

func renderRules(output *strings.Builder, rules []AtomicRule) {
	output.WriteString("## 原子规则\n\n| 规则 ID | 目标 | 强度 | 单一效果 | source_trace |\n|---|---|---|---|---|\n")
	for _, rule := range rules {
		fmt.Fprintf(output, "| `%s` | `%s` | `%s` | %s | %s |\n", rule.ID, rule.Target, rule.Strength, rule.Effect, join(rule.SourceTrace))
	}
}
