package ruleworker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/documentcontract"
)

// prompt.go assembles the parser Agent input and its structured output schema.

// BaseRevision is the slice of the base design revision the agent needs to
// understand what already exists (so stable identities stay fixed) and to emit a
// stable typed document changes.
type BaseRevision struct {
	RevisionID   string
	LayoutIDs    []string
	DocSummaries []string          // "id (kind): summary"
	LayoutsMD    string            // concatenated layouts/*.md
	FoundationMD string            // rules/foundation.md
	LayoutMDByID map[string]string // stable document id -> Markdown content
}

// LoadBaseRevision reads the parts of baseDir used to prime the agent.
func LoadBaseRevision(baseDir string) (*BaseRevision, error) {
	var index designknowledge.Index
	if err := readJSONFile(filepath.Join(baseDir, "index.json"), &index); err != nil {
		return nil, err
	}
	base := &BaseRevision{RevisionID: index.RevisionID}
	layoutSet := make(map[string]struct{})
	for _, doc := range index.Documents {
		base.DocSummaries = append(base.DocSummaries,
			fmt.Sprintf("%s (%s): %s", doc.ID, doc.Kind, doc.Summary))
		if doc.Kind == designknowledge.KindLayout {
			layoutSet[doc.ID] = struct{}{}
		}
	}
	for k := range layoutSet {
		base.LayoutIDs = append(base.LayoutIDs, k)
	}
	sort.Strings(base.LayoutIDs)

	layoutsMD, err := concatDir(filepath.Join(baseDir, "layouts"))
	if err != nil {
		return nil, err
	}
	base.LayoutsMD = layoutsMD

	// Per-layout Markdown is keyed by the stable document id so callers can
	// prime a detail request without relying on a product-specific numbering scheme.
	base.LayoutMDByID = map[string]string{}
	if entries, err := os.ReadDir(filepath.Join(baseDir, "layouts")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if raw, err := os.ReadFile(filepath.Join(baseDir, "layouts", e.Name())); err == nil {
				if document, parseErr := documentcontract.Parse(raw); parseErr == nil && document.Kind == string(designknowledge.KindLayout) {
					base.LayoutMDByID[document.ID] = string(raw)
				}
			}
		}
	}

	if foundation, err := os.ReadFile(filepath.Join(baseDir, "rules", "foundation.md")); err == nil {
		base.FoundationMD = string(foundation)
	}
	return base, nil
}

func concatDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n\n===== %s =====\n\n%s", name, raw)
	}
	return b.String(), nil
}

const revisionSystemPrompt = `你是 AGenUI 设计知识版本演进 Agent。输入包含已发布基线、待处理规则 Markdown 和 Renderer Catalog。你只负责生成可校验的结构化设计知识文档，不生成运行时布局索引。

必须只输出 JSON object {"revisionDelta": ...}:
1. newDocuments 与 modifiedDocuments 的每一项都必须提供 document 对象，禁止直接生成 Markdown content；平台会从 document 确定性渲染 Markdown 并执行 render→parse→AST 等价校验。
2. document 公共字段必须包含 documentSchema、id、version、kind、title、summary。documentSchema 与 kind 必须匹配：layout_doc.v1/layout、element_doc.v1/element、atomic_rule_doc.v1/rule。
3. layout 文档只使用 layout 字段，至少包含一个 nodes 节点；节点 id 在文档内稳定且唯一，parentId 必须引用已存在节点。关系强度 strength 只能是 required|recommended|optional|forbidden。
4. element 文档只使用 element 字段：purpose 说明语义用途，catalogComponents 引用当前 Renderer Catalog 的组件；rules、states、mutuallyExclusiveWith 只记录该元素自身的约束。
5. rule 文档只使用 rules 数组，每条原子规则必须有稳定 id、target、strength、单一 effect 和非空 sourceTrace；不要添加业务分类、内部治理范围或执行逻辑。
6. appliesTo、notFor、requires、references、conflictsWith 用稳定文档 ID 表达适用、排除和依赖。布局 ID 使用 layout.<semantic-slug>，一经发布不得改变；modifiedDocuments.id 必须与 document.id 一致。`

func BuildRevisionUserPrompt(base *BaseRevision, docs []RuleDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 基线版本\n\n版本号:%s\n已有布局 ID:%s\n\n", base.RevisionID, strings.Join(base.LayoutIDs, ", "))
	b.WriteString("## 基线文档清单(id / kind / summary)\n")
	for _, summary := range base.DocSummaries {
		fmt.Fprintf(&b, "- %s\n", summary)
	}
	b.WriteString("\n## 基线布局定义\n")
	b.WriteString(base.LayoutsMD)
	b.WriteString("\n\n# 待处理规则文档\n")
	for _, document := range docs {
		fmt.Fprintf(&b, "\n----- 文档 id=%d file=%q content_md5=%s -----\n%s\n", document.ID, document.FileName, document.ContentMd5, document.Content)
	}
	b.WriteString("\n\n# revisionDelta JSON Schema\n")
	b.Write(RevisionPlanOutputSchema())
	b.WriteString("\n\n# 任务\n只输出包含 revisionDelta 的 JSON；不要输出 Standard、layouts、HTML 或解释文字。")
	return b.String()
}

// RevisionPlanOutputSchema is the parser Agent's sole output contract. The
// deterministic renderer is the only Markdown writer.
func RevisionPlanOutputSchema() json.RawMessage {
	documentSchema := governedDocumentSchema()
	newDocument := strictObject([]string{"kind", "summary", "document"}, map[string]any{
		"id": stringSchema(), "kind": map[string]any{"type": "string", "enum": []string{"layout", "element", "rule"}},
		"version": stringSchema(), "summary": stringSchema(), "tags": stringArraySchema(),
		"appliesTo": stringArraySchema(), "requires": stringArraySchema(), "references": stringArraySchema(),
		"relPath": stringSchema(), "conflictsWith": stringArraySchema(), "notFor": stringArraySchema(),
		"document": documentSchema,
	})
	modifiedDocument := strictObject([]string{"id", "document"}, map[string]any{
		"id": stringSchema(), "version": stringSchema(), "document": documentSchema,
	})
	schema := strictObject([]string{"revisionDelta"}, map[string]any{
		"revisionDelta": strictObject([]string{"newDocuments"}, map[string]any{
			"newDocuments":      map[string]any{"type": "array", "items": newDocument},
			"modifiedDocuments": map[string]any{"type": "array", "items": modifiedDocument},
		}),
	})
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	return raw
}

func stringArraySchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}

func governedDocumentSchema() map[string]any {
	strength := map[string]any{"type": "string", "enum": []string{"required", "recommended", "optional", "forbidden"}}
	node := strictObject([]string{"id", "type"}, map[string]any{
		"id": stringSchema(), "parentId": stringSchema(), "type": stringSchema(),
		"optional": map[string]any{"type": "boolean"}, "capacity": stringSchema(),
	})
	relation := strictObject([]string{"type", "from", "to", "strength"}, map[string]any{
		"type": stringSchema(), "from": stringSchema(), "to": stringSchema(),
		"strength": strength, "description": stringSchema(),
	})
	recipe := strictObject([]string{"id", "name", "appliesWhen", "instructions"}, map[string]any{
		"id": stringSchema(), "name": stringSchema(), "appliesWhen": stringSchema(),
		"notFor": stringSchema(), "instructions": stringArraySchema(),
	})
	layout := strictObject([]string{"nodes"}, map[string]any{
		"contentModes": stringArraySchema(), "topology": stringArraySchema(), "roles": stringArraySchema(),
		"interactions": stringArraySchema(), "designModes": stringArraySchema(),
		"nodes":            map[string]any{"type": "array", "minItems": 1, "items": node},
		"relations":        map[string]any{"type": "array", "items": relation},
		"recipes":          map[string]any{"type": "array", "items": recipe},
		"positiveExamples": stringArraySchema(), "negativeExamples": stringArraySchema(),
	})
	element := strictObject([]string{"purpose", "catalogComponents"}, map[string]any{
		"purpose": stringSchema(), "catalogComponents": stringArraySchema(),
		"rules": stringArraySchema(), "states": stringArraySchema(),
		"mutuallyExclusiveWith": stringArraySchema(), "accessibility": stringArraySchema(),
	})
	atomicRule := strictObject([]string{"id", "target", "strength", "effect", "sourceTrace"}, map[string]any{
		"id": stringSchema(), "target": stringSchema(), "strength": strength, "effect": stringSchema(),
		"appliesTo": stringArraySchema(), "notFor": stringArraySchema(), "sourceTrace": stringArraySchema(),
	})
	return strictObject([]string{"documentSchema", "id", "version", "kind", "title", "summary"}, map[string]any{
		"documentSchema": map[string]any{"type": "string", "enum": []string{"layout_doc.v1", "element_doc.v1", "atomic_rule_doc.v1"}},
		"id":             stringSchema(), "version": stringSchema(),
		"kind":  map[string]any{"type": "string", "enum": []string{"layout", "element", "rule"}},
		"title": stringSchema(), "summary": stringSchema(),
		"appliesTo": stringArraySchema(), "notFor": stringArraySchema(), "requires": stringArraySchema(),
		"references": stringArraySchema(), "conflictsWith": stringArraySchema(),
		"layout": layout, "element": element,
		"rules": map[string]any{"type": "array", "minItems": 1, "items": atomicRule},
	})
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }

func strictObject(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
