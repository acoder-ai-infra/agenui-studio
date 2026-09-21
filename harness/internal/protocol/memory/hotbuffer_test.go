package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/memory"
)

// HotBuffer:Push 写入增量后,Read 按 after-seq 游标只返回该序号之后的增量(补拉语义)。
func TestHotBuffer_PushReadAfterSeq(t *testing.T) {
	buf := memory.NewHotBuffer(memory.WithTTL(time.Minute))
	ctx := context.Background()

	for i := int64(1); i <= 3; i++ {
		if err := buf.Push(ctx, "run_1", protocol.HotDelta{DeltaSeq: i, Content: []byte{byte(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	deltas, expired, err := buf.Read(ctx, "run_1", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if expired {
		t.Fatal("unexpected expired")
	}
	if len(deltas) != 2 {
		t.Fatalf("got %d deltas after seq 1, want 2", len(deltas))
	}
	if deltas[0].DeltaSeq != 2 || deltas[1].DeltaSeq != 3 {
		t.Fatalf("wrong deltas: %+v", deltas)
	}
}

// HotBuffer:TTL 内可读;越过 TTL 后 Read 返回 expired=true(热缓冲仅为短时补拉窗口,
// 真相仍在 EventStore)。用可注入时钟推进时间。
func TestHotBuffer_ExpiryReturnsExpired(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	buf := memory.NewHotBuffer(memory.WithTTL(60*time.Second), memory.WithClock(clock))
	ctx := context.Background()

	if err := buf.Push(ctx, "run_1", protocol.HotDelta{DeltaSeq: 1}); err != nil {
		t.Fatal(err)
	}
	// TTL 内:可读。
	if _, expired, _ := buf.Read(ctx, "run_1", 0, 0); expired {
		t.Fatal("should not be expired before TTL")
	}
	// 把时间推进到 TTL 之后。
	now = now.Add(61 * time.Second)
	_, expired, err := buf.Read(ctx, "run_1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !expired {
		t.Fatal("expected expired=true after TTL")
	}
}

// HotBuffer:对未知 run 的 Read 直接返回 expired=true(引导客户端回退到快照/EventStore)。
func TestHotBuffer_UnknownRunExpired(t *testing.T) {
	buf := memory.NewHotBuffer()
	_, expired, err := buf.Read(context.Background(), "nope", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !expired {
		t.Fatal("unknown run should return expired=true")
	}
}
