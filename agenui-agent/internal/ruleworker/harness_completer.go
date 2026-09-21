package ruleworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	harness "github.com/AGenUI/agenui-studio/harness/sdk"
)

const defaultRuleParserAgentID = "agenui_rule_parser"

// HarnessCompleter runs rule parsing through the same model gateway, usage
// accounting, retries and event store as every other AGenUI Agent. Parsing
// failures remain visible as ordinary Harness Runs and can be retried from the
// management page after a user fixes the Markdown or model configuration.
type HarnessCompleter struct {
	engine  harness.Engine
	agentID string
}

func NewHarnessCompleter(engine harness.Engine, agentID string) (*HarnessCompleter, error) {
	if engine == nil {
		return nil, fmt.Errorf("ruleworker: Harness engine is required")
	}
	if strings.TrimSpace(agentID) == "" {
		agentID = defaultRuleParserAgentID
	}
	return &HarnessCompleter{engine: engine, agentID: agentID}, nil
}

func (c *HarnessCompleter) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	execution, err := c.engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{
			TenantID: "default",
			UserID:   "rule-worker",
			AgentID:  c.agentID,
		},
		Input:    harness.TextMessage(systemPrompt + "\n\n" + userPrompt),
		Metadata: map[string]string{"purpose": "design-rule-parse"},
	})
	if err != nil {
		return "", fmt.Errorf("start parser Run: %w", err)
	}
	defer execution.Events().Close()
	for {
		event, nextErr := execution.Events().Next(ctx)
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				break
			}
			return "", fmt.Errorf("read parser Run: %w", nextErr)
		}
		switch event.EventType {
		case harness.EventRunCompleted:
			result, resultErr := c.engine.GetResult(ctx, harness.GetResultRequest{Identity: execution.Handle().Identity})
			if resultErr != nil {
				return "", fmt.Errorf("read parser result: %w", resultErr)
			}
			content := strings.TrimSpace(result.Content)
			if content == "" && result.ContentRef != "" {
				artifact, artifactErr := c.engine.Artifacts().Get(ctx, harness.GetArtifactRequest{
					Identity: execution.Handle().Identity,
					Ref:      result.ContentRef,
				})
				if artifactErr != nil {
					return "", fmt.Errorf("read parser result artifact: %w", artifactErr)
				}
				raw, readErr := io.ReadAll(artifact.Content)
				closeErr := artifact.Content.Close()
				if readErr != nil || closeErr != nil {
					return "", fmt.Errorf("read parser result artifact: %w", errors.Join(readErr, closeErr))
				}
				content = strings.TrimSpace(string(raw))
			}
			return unwrapJSONFence(content), nil
		case harness.EventRunFailed, harness.EventRunCancelled, harness.EventRunExpired:
			if event.Error != nil && event.Error.Message != "" {
				return "", fmt.Errorf("parser Run %s: %s", event.EventType, event.Error.Message)
			}
			return "", fmt.Errorf("parser Run ended with %s", event.EventType)
		}
	}
	return "", fmt.Errorf("parser Run ended without a result")
}

func unwrapJSONFence(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "```") {
		return value
	}
	if newline := strings.IndexByte(value, '\n'); newline >= 0 {
		value = value[newline+1:]
	}
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "```")
	return strings.TrimSpace(value)
}
