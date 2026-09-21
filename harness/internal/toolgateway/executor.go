package toolgateway

import (
	"context"
	"encoding/json"
)

type ToolExecutor interface {
	Type() ToolType
	Execute(ctx context.Context, def *ToolDefinition, req ToolCallRequest) (*ToolRawResult, error)
}

type ContextAwareToolExecutor interface {
	ToolExecutor
	ExecuteWithContext(ctx context.Context, def *ToolDefinition, req ToolCallRequest, toolCtx ToolContext) (*ToolRawResult, error)
}

type ToolRawResult struct {
	StatusCode   int
	MimeType     string
	Data         json.RawMessage
	Text         string
	Bytes        []byte
	Headers      map[string]string
	ArtifactRefs []string
	Debug        json.RawMessage
	Partial      bool
	Presentation *ResultPresentation
}

type ResultPresentation struct {
	Title   string                     `json:"title"`
	Summary string                     `json:"summary,omitempty"`
	Details []ResultPresentationDetail `json:"details,omitempty"`
}

type ResultPresentationDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
