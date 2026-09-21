package input

import "github.com/AGenUI/agenui-studio/harness/sdk/extension"

func beforeModelResult(
	req extension.BeforeModelRequest,
	messages []extension.BeforeModelMessage,
) extension.BeforeModelResult {
	return extension.BeforeModelResult{
		Messages: messages,
		Tools:    req.Tools,
		Options:  req.Options,
	}
}

func cloneBeforeModelMessages(source []extension.BeforeModelMessage) []extension.BeforeModelMessage {
	result := append([]extension.BeforeModelMessage(nil), source...)
	for index := range result {
		result[index].Parts = append([]extension.BeforeModelPart(nil), source[index].Parts...)
		result[index].ToolCalls = append(
			[]extension.BeforeModelToolCall(nil),
			source[index].ToolCalls...,
		)
	}
	return result
}
