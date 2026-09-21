package protocol_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// P-001:普通客户端只接收 user_visible 事件;授权的 debug 目标可额外接收
// debug/internal/restricted;VisibilitiesForTarget 对普通客户端恰为 [user_visible]。
func TestP001_VisibilityFilter_OrdinaryClientOnlyUserVisible(t *testing.T) {
	filter := protocol.DefaultVisibilityFilter{}
	ordinary := protocol.ClientTarget{Platform: "app"}

	cases := []struct {
		vis  observability.EventVisibility
		want bool
	}{
		{observability.VisibilityUserVisible, true},
		{observability.VisibilityDebug, false},
		{observability.VisibilityInternal, false},
		{observability.VisibilityRestricted, false},
	}
	for _, c := range cases {
		ev := observability.AgentEvent{Visibility: c.vis}
		if got := filter.Allow(ev, ordinary); got != c.want {
			t.Fatalf("ordinary Allow(%s)=%v want %v", c.vis, got, c.want)
		}
	}

	// 授权的 debug 目标可额外接收 debug/internal/restricted。
	debugTarget := protocol.ClientTarget{
		Platform: "admin",
		AllowedVisibilities: []observability.EventVisibility{
			observability.VisibilityDebug, observability.VisibilityInternal, observability.VisibilityRestricted,
		},
	}
	for _, c := range cases {
		ev := observability.AgentEvent{Visibility: c.vis}
		if !filter.Allow(ev, debugTarget) {
			t.Fatalf("debug target should allow visibility %s", c.vis)
		}
	}

	// 普通客户端的 VisibilitiesForTarget 恰为 [user_visible]。
	vis := protocol.VisibilitiesForTarget(ordinary)
	if len(vis) != 1 || vis[0] != observability.VisibilityUserVisible {
		t.Fatalf("ordinary VisibilitiesForTarget = %v, want [user_visible]", vis)
	}
}

// P-002:view_type 是纯客户端投影别名,绝不是可持久化的 EventType;映射表正确,
// 且这些别名都不在 EventStore 的事件类型注册表中(不可被写回存储)。
func TestP002_ViewModel_AliasNeverStored(t *testing.T) {
	mapper := protocol.DefaultViewModelMapper{}

	cases := map[observability.EventType]string{
		observability.EventToolCallStarted:   protocol.ViewTypeToolStart,
		observability.EventToolCallProgress:  protocol.ViewTypeToolDelta,
		observability.EventToolCallCompleted: protocol.ViewTypeToolEnd,
		observability.EventToolCallFailed:    protocol.ViewTypeToolEnd,
		observability.EventAgentTextDelta:    protocol.ViewTypeMessageDelta,
	}
	for et, want := range cases {
		if got := mapper.ViewType(et); got != want {
			t.Fatalf("ViewType(%s)=%q want %q", et, got, want)
		}
	}

	// 无别名的事件类型返回 ""。
	if got := mapper.ViewType(observability.EventRunCompleted); got != "" {
		t.Fatalf("ViewType(run_completed)=%q want empty", got)
	}

	// view_type 别名必须不是已注册/可持久化的事件类型。
	for _, alias := range []string{
		protocol.ViewTypeToolStart, protocol.ViewTypeToolDelta,
		protocol.ViewTypeToolEnd, protocol.ViewTypeMessageDelta,
	} {
		if observability.IsRegisteredEventType(observability.EventType(alias)) {
			t.Fatalf("view_type alias %q must not be a registered event type", alias)
		}
	}
}

// SSEAdapter.Convert:把 payload_preview→payload、payload_ref→artifact_ref,
// 携带 event_id/sequence/view_type,且绝不内联完整 payload(只用 preview)。
func TestSSEAdapter_Convert_Projection(t *testing.T) {
	adapter := protocol.NewSSEAdapter()
	ev := observability.AgentEvent{
		EventID:        "evt_1",
		Sequence:       7,
		EventType:      observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		PayloadPreview: json.RawMessage(`{"name":"search"}`),
		Payload:        json.RawMessage(`{"name":"search","secret":"full"}`),
		PayloadRef:     "artifact://tenants/t/sessions/s/runs/r/art_1",
		TraceID:        "trace_1",
		RunID:          "run_1",
	}
	frame, err := adapter.Convert(context.Background(), ev, protocol.ClientTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if frame.SSE == nil {
		t.Fatal("nil SSE frame")
	}
	d := frame.SSE.Data
	if frame.SSE.ID != "evt_1" {
		t.Fatalf("SSE id=%q want evt_1 (P3-D1)", frame.SSE.ID)
	}
	if d.SchemaVersion != protocol.SSESchemaVersion {
		t.Fatalf("schema_version=%q", d.SchemaVersion)
	}
	if d.Sequence != 7 || d.EventID != "evt_1" {
		t.Fatalf("sequence/event_id mismatch: %+v", d)
	}
	if d.ViewType != protocol.ViewTypeToolStart {
		t.Fatalf("view_type=%q want tool_start", d.ViewType)
	}
	if string(d.Payload) != `{"name":"search"}` {
		t.Fatalf("payload should come from payload_preview, got %s", d.Payload)
	}
	if d.ArtifactRef != ev.PayloadRef {
		t.Fatalf("artifact_ref=%q", d.ArtifactRef)
	}
}

// EventBroker:Publish 向多个订阅者扇出,按 runID 隔离(别的 run 事件不串台),
// cancel 后停止投递并关闭该订阅通道,其余订阅者不受影响。
func TestEventBroker_FanOutAndCancel(t *testing.T) {
	b := protocol.NewMemoryBroker()
	ctx := context.Background()

	s1, cancel1, err := b.Subscribe(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	s2, cancel2, err := b.Subscribe(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()

	ev := observability.AgentEvent{RunID: "run_1", EventID: "e1", Sequence: 1}
	if err := b.Publish(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if got := <-s1; got.EventID != "e1" {
		t.Fatalf("s1 got %q", got.EventID)
	}
	if got := <-s2; got.EventID != "e1" {
		t.Fatalf("s2 got %q", got.EventID)
	}

	// 另一个 run 的事件不会投递给 run_1 的订阅者。
	_ = b.Publish(ctx, observability.AgentEvent{RunID: "run_other", EventID: "x", Sequence: 1})

	// 取消 s1:停止接收,通道被关闭。
	cancel1()
	if _, open := <-s1; open {
		t.Fatal("s1 channel should be closed after cancel")
	}

	// s2 仍能继续接收。
	_ = b.Publish(ctx, observability.AgentEvent{RunID: "run_1", EventID: "e2", Sequence: 2})
	if got := <-s2; got.EventID != "e2" {
		t.Fatalf("s2 got %q want e2", got.EventID)
	}
}
