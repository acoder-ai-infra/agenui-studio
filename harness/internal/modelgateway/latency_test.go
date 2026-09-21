package modelgateway

import (
	"math"
	"testing"
	"time"
)

func TestModelLatencyTrackerMeasuresFirstOutputAndThroughput(t *testing.T) {
	started := time.Unix(100, 0)
	tracker := newModelLatencyTracker(started)
	tracker.observe(NormalizedChunk{Kind: ChunkUsage}, started.Add(100*time.Millisecond))
	tracker.observe(NormalizedChunk{Kind: ChunkToken}, started.Add(150*time.Millisecond))
	tracker.observe(NormalizedChunk{Kind: ChunkToken, TextDelta: "first"}, started.Add(250*time.Millisecond))
	tracker.observe(NormalizedChunk{Kind: ChunkToken, TextDelta: "ignored"}, started.Add(500*time.Millisecond))

	got := tracker.snapshot(ModelUsage{CompletionTokens: 20}, started.Add(1250*time.Millisecond))
	if !got.FirstTokenObserved || got.TotalMS != 1250 || got.FirstTokenMS != 250 || got.GenerationDurationMS != 1000 || math.Abs(got.OutputTokensPerSecond-20) > 1e-9 {
		t.Fatalf("latency = %#v", got)
	}
}

func TestModelLatencyTrackerAcceptsToolOutputAndDistinguishesNoOutput(t *testing.T) {
	started := time.Unix(100, 0)
	withoutOutput := newModelLatencyTracker(started).snapshot(ModelUsage{CompletionTokens: 5}, started.Add(time.Second))
	if withoutOutput.FirstTokenObserved || withoutOutput.FirstTokenMS != 0 || withoutOutput.OutputTokensPerSecond != 0 {
		t.Fatalf("no-output attempt was reported as first token: %#v", withoutOutput)
	}

	tracker := newModelLatencyTracker(started)
	tracker.observe(NormalizedChunk{Kind: ChunkToolCall, ToolCallDelta: &ToolCallDelta{Name: "harness.echo"}}, started.Add(100*time.Millisecond))
	got := tracker.snapshot(ModelUsage{CompletionTokens: 9}, started.Add(400*time.Millisecond))
	if !got.FirstTokenObserved || got.FirstTokenMS != 100 || got.GenerationDurationMS != 300 || math.Abs(got.OutputTokensPerSecond-30) > 1e-9 {
		t.Fatalf("tool-output latency = %#v", got)
	}
}
