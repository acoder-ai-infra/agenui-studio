package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func ctxT(tenant string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant, TraceID: "trace_x"})
}

type countingResumer struct {
	n        int32
	err      error
	mu       sync.Mutex
	requests []control.ResumeRequest
}

func (r *countingResumer) Resume(_ context.Context, req control.ResumeRequest) error {
	atomic.AddInt32(&r.n, 1)
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	return r.err
}

func (r *countingResumer) lastRequest() control.ResumeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

func (r *countingResumer) allRequests() []control.ResumeRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]control.ResumeRequest(nil), r.requests...)
}

// helper: seed a run in waiting-control-ready state (running) + a checkpoint.
func seed(ctx context.Context, stores storage.Stores) {
	_ = stores.Runs.Create(ctx, &storage.Run{RunID: "run1", SessionID: "s1", Status: storage.RunStatusCreated})
	_, _ = stores.Runs.CompareAndSetStatus(ctx, "run1", storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{})
	_ = stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{CheckpointID: "ckpt1", RunID: "run1", StateRef: "artifact://ckpt"})
}

// execution §3 + T-002: Create requires a persisted checkpoint and moves run to waiting_control.
func TestCreateEntersWaitingControl(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	svc := control.New(stores, &countingResumer{})

	cr, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cr.Status != string(control.StatusPending) {
		t.Fatalf("want pending, got %s", cr.Status)
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusWaitingControl {
		t.Fatalf("run want waiting_control, got %s", run.Status)
	}
}

func TestCreateUsesProvidedRequestIDAndEvent(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	svc := control.New(stores, nil)

	payload := json.RawMessage(`{"request_id":"ctrl_runtime","type":"ask_user","prompt":"Continue?"}`)
	cr, err := svc.Create(ctx, control.CreateRequest{
		RequestID:     "ctrl_runtime",
		RunID:         "run1",
		Type:          "ask_user",
		CheckpointID:  "ckpt1",
		PromptPreview: "Continue?",
		ResumeToken:   "tok",
		Event: observability.AgentEvent{
			EventID:    "event_runtime_control",
			RunID:      "run1",
			EventType:  observability.EventControlRequestCreated,
			Visibility: observability.VisibilityUserVisible,
			Payload:    payload,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cr.RequestID != "ctrl_runtime" || cr.PromptPreview != "Continue?" {
		t.Fatalf("unexpected control request: %#v", cr)
	}
	persisted, err := stores.Controls.Get(ctx, "ctrl_runtime")
	if err != nil || persisted.ResumeTokenHash == "" || persisted.CheckpointID != "ckpt1" {
		t.Fatalf("unexpected persisted control request: %#v err=%v", persisted, err)
	}
	events, err := stores.Events.Query(ctx, storage.EventQuery{RunID: "run1", Limit: 10, Visibilities: []observability.EventVisibility{observability.VisibilityUserVisible}})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) != 1 || events[0].EventID != "event_runtime_control" || string(events[0].Payload) != string(payload) {
		t.Fatalf("unexpected events: %#v", events)
	}
}

// fail closed: missing checkpoint => no control request, run stays running.
func TestCreateFailsClosedWithoutCheckpoint(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	svc := control.New(stores, nil)
	if _, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "missing", ResumeToken: "tok"}); err == nil {
		t.Fatal("expected error for missing checkpoint")
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusRunning {
		t.Fatalf("run must stay running on fail-closed, got %s", run.Status)
	}
}

func TestCreateFailsClosedWithoutResumeToken(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	svc := control.New(stores, nil)

	if _, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1"}); err == nil {
		t.Fatal("expected error for missing resume token")
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusRunning {
		t.Fatalf("run must stay running on fail-closed, got %s", run.Status)
	}
}

// TestCreateAcceptsChildRunControlRequest 验证 platform child run 与父 Run 共用
// control 创建链路（方案 §6.5）：control 事实建在 child run 上并进入
// waiting_control，由 Agent Gateway 内部消费。
func TestCreateAcceptsChildRunControlRequest(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: "child_run_1", SessionID: "s1", ParentRunID: "parent_run_1", Status: storage.RunStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{
		CheckpointID: "child_ckpt_1", RunID: "child_run_1", StateRef: "artifact://child_ckpt",
	}); err != nil {
		t.Fatal(err)
	}
	svc := control.New(stores, nil)

	created, err := svc.Create(ctx, control.CreateRequest{
		RunID: "child_run_1", Type: "ask_user", CheckpointID: "child_ckpt_1", ResumeToken: "tok",
	})
	if err != nil {
		t.Fatalf("child control request must be accepted, got %v", err)
	}
	if created.RunID != "child_run_1" || created.Status != string(control.StatusPending) {
		t.Fatalf("child control request malformed: %#v", created)
	}
	run, getErr := stores.Runs.Get(ctx, "child_run_1")
	if getErr != nil || run.Status != storage.RunStatusWaitingControl {
		t.Fatalf("child run must enter waiting_control: run=%#v err=%v", run, getErr)
	}
}

// TestAnswerAcceptsChildControlRequest 验证 child control 的 Answer 与父 Run
// 走同一条路径（由 Agent Gateway 凭 child 票据发起）。
func TestAnswerAcceptsChildControlRequest(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: "child_run_legacy", SessionID: "s1", ParentRunID: "parent_run_1", Status: storage.RunStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{
		CheckpointID: "child_ckpt_legacy", RunID: "child_run_legacy", StateRef: "artifact://child_ckpt",
	}); err != nil {
		t.Fatal(err)
	}
	resumer := &countingResumer{}
	svc := control.New(stores, resumer)
	created, err := svc.Create(ctx, control.CreateRequest{
		RunID: "child_run_legacy", Type: "ask_user", CheckpointID: "child_ckpt_legacy", ResumeToken: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}

	answered, err := svc.Answer(ctx, control.AnswerRequest{RequestID: created.RequestID, ResumeToken: "tok", ResponseRef: "artifact://response"})
	if err != nil {
		t.Fatalf("child answer must be accepted, got %v", err)
	}
	if answered.Status != string(control.StatusAnswered) {
		t.Fatalf("child control must be answered: %#v", answered)
	}
}

func TestCreateRejectsCheckpointFromAnotherRun(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{CheckpointID: "ckpt_other", RunID: "run_other", StateRef: "artifact://other"}); err != nil {
		t.Fatal(err)
	}
	svc := control.New(stores, nil)

	_, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt_other", ResumeToken: "tok"})
	if !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("want checkpoint/run conflict, got %v", err)
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusRunning {
		t.Fatalf("run must stay running on fail-closed, got %s", run.Status)
	}
}

// C-001: answered ControlResponse may retry delivery; Runtime claim owns
// exactly-once execution. This also recovers a crash after Answer was stored.
func TestAnswerDuplicateRedeliversResumeWhileRunIsResumable(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	resumer := &countingResumer{}
	svc := control.New(stores, resumer)
	cr, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "tok", ResponseRef: "artifact://resp"}); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	if _, err := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "tok", ResponseRef: "artifact://resp2"}); err != nil {
		t.Fatalf("second answer should be idempotent: %v", err)
	}
	if got := atomic.LoadInt32(&resumer.n); got != 2 {
		t.Fatalf("Resume delivery attempts = %d, want 2", got)
	}
	if got := resumer.lastRequest().ResponseRef; got != "artifact://resp" {
		t.Fatalf("duplicate answer replaced authoritative response_ref: %q", got)
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusWaitingControl {
		t.Fatalf("Control must not preclaim resuming, got %s", run.Status)
	}
}

func TestConcurrentAnswersAlwaysDeliverFirstPersistedResponse(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	resumer := &countingResumer{}
	svc := control.New(stores, resumer)
	cr, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan *storage.ControlRequest, 2)
	errs := make(chan error, 2)
	for _, ref := range []string{"artifact://answer-a", "artifact://answer-b"} {
		ref := ref
		go func() {
			<-start
			answered, answerErr := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "tok", ResponseRef: ref})
			results <- answered
			errs <- answerErr
		}()
	}
	close(start)
	var authoritative string
	for range 2 {
		if answerErr := <-errs; answerErr != nil {
			t.Fatalf("concurrent answer: %v", answerErr)
		}
		answered := <-results
		if authoritative == "" {
			authoritative = answered.ResponseRef
		} else if answered.ResponseRef != authoritative {
			t.Fatalf("answers observed different persisted refs: %q and %q", authoritative, answered.ResponseRef)
		}
	}
	for _, delivered := range resumer.allRequests() {
		if delivered.ResponseRef != authoritative {
			t.Fatalf("Resume received non-authoritative ref %q, want %q", delivered.ResponseRef, authoritative)
		}
	}
}

func TestAnswerRetriesAfterResumerFailureWithoutStrandingRun(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	resumer := &countingResumer{err: errors.New("process stopped before runtime claim")}
	svc := control.New(stores, resumer)
	cr, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "tok"}); err == nil {
		t.Fatal("expected first resume delivery failure")
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusWaitingControl {
		t.Fatalf("failed delivery stranded run in %s", run.Status)
	}
	resumer.err = nil
	if _, err := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "tok"}); err != nil {
		t.Fatalf("retry answered request: %v", err)
	}
	if got := atomic.LoadInt32(&resumer.n); got != 2 {
		t.Fatalf("resume attempts=%d, want 2", got)
	}
}

// invalid resume token rejected.
func TestAnswerBadToken(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	svc := control.New(stores, &countingResumer{})
	cr, _ := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: time.Hour})
	if _, err := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "wrong"}); !storage.IsErrorCode(err, storage.ErrPermissionDenied) {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestAnswerRejectsExpiredRequestBeforeSweep(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	resumer := &countingResumer{}
	svc := control.New(stores, resumer)
	cr, err := svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: -time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Answer(ctx, control.AnswerRequest{RequestID: cr.RequestID, ResumeToken: "tok"}); !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("want expired conflict, got %v", err)
	}
	if got := atomic.LoadInt32(&resumer.n); got != 0 {
		t.Fatalf("expired control resumed %d times", got)
	}
}

// C-002: expired pending request drives run to expired.
func TestExpire(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	seed(ctx, stores)
	svc := control.New(stores, nil)
	_, _ = svc.Create(ctx, control.CreateRequest{RunID: "run1", Type: "ask_user", CheckpointID: "ckpt1", ResumeToken: "tok", TTL: -time.Minute})

	n, err := svc.Expire(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 expired, got %d", n)
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusExpired {
		t.Fatalf("run want expired, got %s", run.Status)
	}
}
