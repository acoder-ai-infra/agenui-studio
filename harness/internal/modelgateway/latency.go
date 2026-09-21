package modelgateway

import "time"

type modelLatencyTracker struct {
	startedAt   time.Time
	firstOutput time.Time
}

func newModelLatencyTracker(startedAt time.Time) *modelLatencyTracker {
	return &modelLatencyTracker{startedAt: startedAt}
}

func (t *modelLatencyTracker) observe(chunk NormalizedChunk, at time.Time) {
	if t == nil || !t.firstOutput.IsZero() || !isSemanticOutput(chunk) {
		return
	}
	t.firstOutput = at
}

func (t *modelLatencyTracker) snapshot(usage ModelUsage, completedAt time.Time) ModelLatency {
	if t == nil || t.startedAt.IsZero() || completedAt.Before(t.startedAt) {
		return ModelLatency{}
	}
	latency := ModelLatency{TotalMS: completedAt.Sub(t.startedAt).Milliseconds()}
	if t.firstOutput.IsZero() || completedAt.Before(t.firstOutput) {
		return latency
	}
	latency.FirstTokenObserved = true
	latency.FirstTokenMS = t.firstOutput.Sub(t.startedAt).Milliseconds()
	generation := completedAt.Sub(t.firstOutput)
	latency.GenerationDurationMS = generation.Milliseconds()
	if usage.CompletionTokens > 0 && generation > 0 {
		latency.OutputTokensPerSecond = float64(usage.CompletionTokens) / generation.Seconds()
	}
	return latency
}

func isSemanticOutput(chunk NormalizedChunk) bool {
	switch chunk.Kind {
	case ChunkToken:
		return chunk.TextDelta != ""
	case ChunkThought:
		return chunk.ThoughtDelta != ""
	case ChunkToolCall:
		return chunk.ToolCallDelta != nil && (chunk.ToolCallDelta.ToolCallID != "" || chunk.ToolCallDelta.Name != "" || chunk.ToolCallDelta.ArgumentsDelta != "")
	default:
		return false
	}
}
