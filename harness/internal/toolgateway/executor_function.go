package toolgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type FunctionTool func(ctx context.Context, req FunctionCall) (*FunctionResult, error)

type FunctionCall struct {
	ToolCallID string
	// ToolName / ToolVersion 是被调用工具在目录中的名称与版本，供
	// 多工具共用一个 handler 或扩展 handler 回传上下文使用。
	ToolName    string
	ToolVersion string
	Arguments   json.RawMessage
	Resume      *ToolCallResume
	Trace       observability.TraceContext
	ToolContext ToolContext
}

type FunctionResult struct {
	Data         json.RawMessage
	Text         string
	MimeType     string
	Presentation *ResultPresentation
}

type FunctionExecutor struct {
	handlers map[string]FunctionTool
}

func NewFunctionExecutor(handlers map[string]FunctionTool) *FunctionExecutor {
	copied := make(map[string]FunctionTool, len(handlers))
	for name, handler := range handlers {
		copied[name] = handler
	}
	return &FunctionExecutor{handlers: copied}
}

func (e *FunctionExecutor) Type() ToolType {
	return ToolTypeFunction
}

func (e *FunctionExecutor) Execute(ctx context.Context, def *ToolDefinition, req ToolCallRequest) (raw *ToolRawResult, err error) {
	return e.execute(ctx, def, req, nil)
}

func (e *FunctionExecutor) ExecuteWithContext(ctx context.Context, def *ToolDefinition, req ToolCallRequest, toolCtx ToolContext) (raw *ToolRawResult, err error) {
	return e.execute(ctx, def, req, toolCtx)
}

func (e *FunctionExecutor) execute(ctx context.Context, def *ToolDefinition, req ToolCallRequest, toolCtx ToolContext) (raw *ToolRawResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = NewToolError(ErrorTypeInternal, fmt.Sprintf("function handler panic: %v", recovered), false, fmt.Errorf("%s", debug.Stack()))
			raw = nil
		}
	}()
	if def.Function == nil || def.Function.HandlerName == "" {
		return nil, NewToolError(ErrorTypeInternal, "function handler name is required", false, nil)
	}
	handler, ok := e.handlers[def.Function.HandlerName]
	if !ok {
		return nil, NewToolError(ErrorTypeToolNotFound, fmt.Sprintf("function handler not found: %s", def.Function.HandlerName), false, nil)
	}
	var resume *ToolCallResume
	if req.Resume != nil {
		copy := *req.Resume
		copy.State = append(json.RawMessage(nil), req.Resume.State...)
		copy.Payload = append(json.RawMessage(nil), req.Resume.Payload...)
		resume = &copy
	}
	result, err := handler(ctx, FunctionCall{
		ToolCallID:  req.ToolCallID,
		ToolName:    def.Name,
		ToolVersion: def.Version,
		Arguments:   append(json.RawMessage(nil), req.Arguments...),
		Resume:      resume,
		Trace:       observability.MustTraceContext(ctx),
		ToolContext: toolCtx,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &FunctionResult{}
	}
	return &ToolRawResult{
		MimeType:     result.MimeType,
		Data:         append(json.RawMessage(nil), result.Data...),
		Text:         result.Text,
		Presentation: cloneResultPresentation(result.Presentation),
	}, nil
}
