# Rule documents / 规则文档

Upload Markdown files (`.md` or `.markdown`) from **Rules**. You may select
multiple files or a folder; non-Markdown files are skipped. Studio stores the
original Markdown, parses it asynchronously, and displays its parsed result
read-only. You do not upload rule JSON.

请在“规则”页面上传 Markdown 文件（`.md` 或 `.markdown`）。支持多选和文件夹上传，
非 Markdown 文件会被跳过。Studio 保存原始 Markdown，由异步 worker 解析，并以只读
方式展示解析结果；用户不需要上传规则 JSON。

Download one of the Layout, Element, or Atomic Rule templates from the Rules
page when starting a new document. The upload boundary validates only Markdown
file shape and size; the model-backed Worker decides its meaning from the full
document and returns a parse error when it cannot form a valid typed rule. The
Host does not classify documents by hard-coded headings or keywords.

新建规则时可从“规则”页面下载 Layout、Element 或 Atomic Rule 模板。上传边界只校验
Markdown 文件形态与大小；文档语义由 Worker 阅读全文判断，无法形成合法结构化规则时返回
解析错误。Host 不通过固定标题或关键词给规则分类。

## Minimal example / 最小示例

```md
# Order summary card

## Structure

- Show one short primary title and one supporting summary.
- Keep the title and summary stable while data refreshes.

## Visual rules

- Use only components available in the active renderer catalog.
- Keep one primary task per card and make loading and empty states explicit.
- Do not infer APIs, fields, or rules that were not supplied.
```

This is the same plain-Markdown style as the bundled local demo rules under
`agenui-agent/configs/design-public/revisions/demo-v1/`: one global baseline
and three distinct layouts (summary, information with one action, and repeated
item list). The parser may create structured internal records and a revision
candidate, but those are generated artifacts—not an authoring format.

Layout documents use stable semantic IDs such as `layout.summary-card`; they do
not use ordered names. The Worker receives a compact authoring view generated
from the active renderer catalog. The view is not a second catalog and is never
a Gate.

这与内置本地示例规则（`agenui-agent/configs/design-public/revisions/demo-v1/`）
使用相同的纯 Markdown 写法：一份全局约束，加上三种布局（摘要、带单一操作的信息卡、
重复项列表）。解析器可能生成结构化内部记录和 revision 候选，但那些是生成产物，不是
用户的编写格式。

布局文档使用 `layout.summary-card` 这样稳定的语义 ID，不使用过程编号。Worker
只接收由当前 Renderer Catalog 生成的精简 authoring view；它不是第二份 Catalog，
也不参与门禁。

## Publication / 发布

After a valid parse and catalog-reference check, the Worker atomically publishes
an immutable revision and advances the active pointer. Parse failures remain
visible in the job and Harness Run logs; fix the Markdown and retry. The
uploaded Markdown remains the original authoring document.

解析成功并通过 Catalog 引用校验后，Worker 原子发布不可变 revision 并切换 active
pointer。解析失败会保留在任务和 Harness Run 日志中；修正文档后人工重试即可。上传的
Markdown 始终是原始编写文档。
