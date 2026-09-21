package modelgateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

type quotaCommitRecord struct {
	usage mg.ModelUsage
	cost  mg.ModelCost
}

type recordingQuota struct {
	mu      sync.Mutex
	commits []quotaCommitRecord
}

func (*recordingQuota) Reserve(context.Context, mg.ModelRequest) (mg.QuotaReservation, error) {
	return mg.QuotaReservation{TenantID: "t1", ReservationID: "reservation"}, nil
}

func (q *recordingQuota) Commit(_ context.Context, _ mg.QuotaReservation, usage mg.ModelUsage, cost mg.ModelCost) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.commits = append(q.commits, quotaCommitRecord{usage: usage, cost: cost})
}

func (*recordingQuota) Release(context.Context, mg.QuotaReservation) {}

func (q *recordingQuota) onlyCommit(t *testing.T) quotaCommitRecord {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.commits) != 1 {
		t.Fatalf("quota commits=%d, want exactly 1", len(q.commits))
	}
	return q.commits[0]
}

func (q *recordingQuota) snapshot() []quotaCommitRecord {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]quotaCommitRecord(nil), q.commits...)
}

type timeoutProvider struct{}

func (timeoutProvider) ID() string { return "timeout" }

func (timeoutProvider) InvokeChat(context.Context, mg.AdapterRequest) (mg.AdapterStream, error) {
	return &timeoutStream{}, nil
}

type timeoutStream struct{ sent bool }

func (s *timeoutStream) Next(ctx context.Context) (mg.NormalizedChunk, error) {
	if !s.sent {
		s.sent = true
		return mock.TokenChunk("partial"), nil
	}
	<-ctx.Done()
	return mg.NormalizedChunk{}, ctx.Err()
}

func (*timeoutStream) Close() error { return nil }

func TestFailedProviderAttemptCommitsBestKnownQuota(t *testing.T) {
	tests := []struct {
		name         string
		provider     mg.ChatProvider
		timeoutMS    int
		wantPrompt   int
		wantComplete int
		wantSource   mg.UsageSource
	}{
		{
			name: "stream failure preserves provider usage",
			provider: mock.New("stream-error", mock.Script{Chunks: []mg.NormalizedChunk{
				mock.UsageChunk(7, 3), mock.ErrorChunk("stream failed", false),
			}}),
			wantPrompt: 7, wantComplete: 3, wantSource: mg.UsageSourceGateway,
		},
		{
			name:     "timeout estimates partial attempt",
			provider: timeoutProvider{}, timeoutMS: 20,
			wantPrompt: 2, wantComplete: 2, wantSource: mg.UsageSourceEstimated,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quota := &recordingQuota{}
			stores := storagemem.New().Stores()
			logger := observability.NewRingLogger(observability.NoopLogger{}, 20)
			gateway := &mg.Facade{
				Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
					Model: "m", Provider: test.provider.ID(),
					Cost: mg.ModelCostTable{Currency: "USD", InputPer1K: 1, OutputPer1K: 1},
				}}, nil),
				Providers:   map[string]mg.ChatProvider{test.provider.ID(): test.provider},
				Quota:       quota,
				UsageLedger: stores.Usage,
				Logger:      logger,
			}
			events, response := collectEvents(t, gateway, mg.ModelRequest{
				RequestID: "failed-attempt", Streaming: true, TimeoutMS: test.timeoutMS,
				Trace:    observability.TraceContext{TenantID: "t1"},
				Messages: []mg.ChatMessage{mg.TextMessage("user", "hi")},
			})
			if response.Status != mg.StatusFailed {
				t.Fatalf("response status=%s, want failed", response.Status)
			}
			commit := quota.onlyCommit(t)
			if response.Usage != commit.usage || response.Cost != commit.cost {
				t.Fatalf("failed response did not match settlement: response=%#v commit=%#v", response, commit)
			}
			if commit.usage.PromptTokens != test.wantPrompt || commit.usage.CompletionTokens != test.wantComplete || commit.usage.Source != test.wantSource {
				t.Fatalf("committed usage=%#v", commit.usage)
			}
			if commit.cost.Currency != "USD" || commit.cost.Estimated <= 0 {
				t.Fatalf("committed cost=%#v", commit.cost)
			}
			records, err := stores.Usage.List(context.Background(), storage.ModelUsageQuery{TenantID: "t1"})
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].PromptTokens != commit.usage.PromptTokens || records[0].CompletionTokens != commit.usage.CompletionTokens || records[0].EstimatedCost != commit.cost.Estimated {
				t.Fatalf("usage ledger did not match Redis settlement: records=%#v commit=%#v", records, commit)
			}
			var failed mg.ModelCallFailedPayload
			for _, event := range events {
				if event.EventType == observability.EventModelCallFailed {
					if err := json.Unmarshal(event.Payload, &failed); err != nil {
						t.Fatal(err)
					}
				}
			}
			if failed.Usage != commit.usage || failed.Cost != commit.cost {
				t.Fatalf("failed event did not match settlement: payload=%#v commit=%#v", failed, commit)
			}
			logs := logger.QueryLogs(observability.LogQuery{})
			if len(logs) == 0 || logs[0].Message != "model gateway call failed" ||
				fmt.Sprint(logs[0].Fields["prompt_tokens"]) != fmt.Sprint(commit.usage.PromptTokens) ||
				fmt.Sprint(logs[0].Fields["estimated_cost"]) != fmt.Sprint(commit.cost.Estimated) {
				t.Fatalf("failed log did not match settlement: logs=%#v commit=%#v", logs, commit)
			}
		})
	}
}

func TestFallbackSettlesEachProviderAttempt(t *testing.T) {
	quota := &recordingQuota{}
	stores := storagemem.New().Stores()
	gateway := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary:  mg.ModelTarget{Model: "m", Provider: "primary"},
			Fallback: []mg.ModelTarget{{Model: "m", Provider: "fallback"}},
		}, nil),
		Providers: map[string]mg.ChatProvider{
			"primary":  mock.New("primary", mock.Script{Chunks: []mg.NormalizedChunk{mock.ErrorChunk("retry", true)}}),
			"fallback": mock.New("fallback", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok"), mock.UsageChunk(4, 2)}}),
		},
		Quota:       quota,
		UsageLedger: stores.Usage,
	}
	events, response := collectEvents(t, gateway, mg.ModelRequest{
		RequestID: "fallback-attempts", Streaming: true,
		Trace:    observability.TraceContext{TenantID: "t1"},
		Messages: []mg.ChatMessage{mg.TextMessage("user", "hi")},
	})
	if response.Status != mg.StatusSuccess {
		t.Fatalf("response status=%s, want success", response.Status)
	}
	commits := quota.snapshot()
	if len(commits) != 2 {
		t.Fatalf("quota commits=%d, want one per provider attempt", len(commits))
	}
	if commits[0].usage.Source != mg.UsageSourceEstimated || commits[1].usage.Source != mg.UsageSourceGateway {
		t.Fatalf("fallback attempt usage sources=%q, %q", commits[0].usage.Source, commits[1].usage.Source)
	}
	records, err := stores.Usage.List(context.Background(), storage.ModelUsageQuery{TenantID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("usage ledger records=%d, want one per provider attempt", len(records))
	}
	byAttempt := map[int]*storage.ModelUsageRecord{}
	for _, record := range records {
		byAttempt[record.Attempt] = record
	}
	for attempt, commit := range commits {
		record := byAttempt[attempt]
		if record == nil || record.PromptTokens != commit.usage.PromptTokens || record.CompletionTokens != commit.usage.CompletionTokens || record.EstimatedCost != commit.cost.Estimated {
			t.Fatalf("attempt %d ledger=%#v commit=%#v", attempt, record, commit)
		}
	}
	var failed mg.ModelCallFailedPayload
	for _, event := range events {
		if event.EventType == observability.EventModelCallFailed {
			if err := json.Unmarshal(event.Payload, &failed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if failed.Attempt != 0 || failed.Usage != commits[0].usage || failed.Cost != commits[0].cost {
		t.Fatalf("primary failed event=%#v, want attempt-0 settlement=%#v", failed, commits[0])
	}
}
