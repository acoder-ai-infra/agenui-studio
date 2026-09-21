package anthropic

import mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"

// NewToolSchemaCompiler exposes Anthropic schema lowering for repository-wide
// catalog conformance tests and the live adapter boundary.
func NewToolSchemaCompiler() mg.ToolSchemaCompiler {
	return mg.NewDiscriminatedObjectUnionToolSchemaCompiler("anthropic")
}
