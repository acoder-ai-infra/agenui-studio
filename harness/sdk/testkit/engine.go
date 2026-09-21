package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// harnessExtOutputResult mirrors extension.OutputValidateResult for the
// mockExecution stub without pulling the extension package into every test.
type harnessExtOutputResult = extension.OutputValidateResult

// Scenario is a scripted canonical event sequence. The MockEngine replays it
// in order, computing Sequence values automatically. Callers do not have to
// worry about Sequence collisions or ordering.
type Scenario struct {
	// Events is the ordered event list. Sequence numbers are ignored (the
	// engine reassigns them starting at 1).
	Events []harness.Event
	// Result is what GetResult returns after the scenario's terminal event.
	Result harness.ResultView
	// RunError, when non-nil, is returned from Start (before any event).
	RunError error
	// StreamPolicy configures fault injection (delay per event, mid-stream
	// break, panic). Zero-value StreamPolicy replays events with no delay.
	StreamPolicy StreamPolicy
}

// StreamPolicy configures per-event stream behaviour.
type StreamPolicy struct {
	// DelayPerEvent, when non-zero, sleeps for the duration between successive
	// events. Useful for exercising backpressure.
	DelayPerEvent time.Duration
	// BreakAfter, when non-zero, closes the stream mid-way after N events.
	// Callers observe io.EOF (or ErrClosed if the stream was Close'd first).
	BreakAfter int
	// PanicAfter, when non-zero, panics on the Nth Next call. Used to prove
	// the caller recovers cleanly.
	PanicAfter int
}

// MockEngine is a lightweight, in-memory Engine used by SDK consumers to test
// their code without booting the full Composition Root. It implements the
// full harness.Engine interface; every method operates against the current
// Scenario.
//
// Concurrency: MockEngine methods are safe from multiple goroutines. Each
// Start / Resume returns an independent stream so concurrent execution is
// exercisable.
type MockEngine struct {
	mu       sync.Mutex
	scenario Scenario
	// nextSequence is stamped onto Events as they are handed out.
	nextSequence int64
	// active tracks executions for Close draining.
	active   sync.WaitGroup
	closed   atomic.Bool
	fullView harness.RunView
}

// NewMockEngine builds a MockEngine that will replay scenario on every Start.
// A caller can update the scenario mid-test via SetScenario.
func NewMockEngine(scenario Scenario) *MockEngine {
	return &MockEngine{scenario: scenario}
}

// SetScenario swaps the scripted scenario. Callers use it to script different
// responses in table-driven tests.
func (m *MockEngine) SetScenario(scenario Scenario) {
	m.mu.Lock()
	m.scenario = scenario
	m.mu.Unlock()
}

// Start records the request and returns an Execution whose stream replays the
// current Scenario.
func (m *MockEngine) Start(ctx context.Context, req harness.StartRequest) (harness.Execution, error) {
	if m.closed.Load() {
		return nil, harness.ErrClosed
	}
	if req.Identity.TenantID == "" {
		return nil, harness.ErrInvalidRequest
	}
	if len(req.Input.Parts) == 0 {
		return nil, harness.ErrInvalidRequest
	}
	m.mu.Lock()
	sc := m.scenario
	m.mu.Unlock()
	if sc.RunError != nil {
		return nil, sc.RunError
	}
	m.active.Add(1)
	runID := "mock_run"
	if req.Identity.RunID != "" {
		runID = req.Identity.RunID
	}
	sessionID := "mock_session"
	if req.Identity.SessionID != "" {
		sessionID = req.Identity.SessionID
	}
	stream := newMockStream(sc, runID, &m.active, &m.nextSequence)
	handle := harness.RunHandle{
		Identity: harness.Identity{
			TenantID:  req.Identity.TenantID,
			UserID:    req.Identity.UserID,
			SessionID: sessionID,
			RunID:     runID,
			AgentID:   req.Identity.AgentID,
		},
		TraceID:       "trace_" + runID,
		InputManifest: harness.BuildInputManifest(req.Input),
	}
	m.fullView = harness.RunView{
		Identity: handle.Identity,
		Status:   harness.RunStatusRunning,
	}
	return &mockExecution{handle: handle, stream: stream}, nil
}

// Resume returns a fresh stream replaying the scenario again.
func (m *MockEngine) Resume(ctx context.Context, req harness.ResumeRequest) (harness.Execution, error) {
	if m.closed.Load() {
		return nil, harness.ErrClosed
	}
	if req.Identity.RunID == "" {
		return nil, harness.ErrInvalidRequest
	}
	m.mu.Lock()
	sc := m.scenario
	m.mu.Unlock()
	m.active.Add(1)
	stream := newMockStream(sc, req.Identity.RunID, &m.active, &m.nextSequence)
	handle := harness.RunHandle{
		Identity: harness.Identity{
			RunID: req.Identity.RunID,
		},
		TraceID: "trace_" + req.Identity.RunID,
	}
	return &mockExecution{handle: handle, stream: stream}, nil
}

// Cancel is a no-op in the mock (streams close naturally).
func (m *MockEngine) Cancel(ctx context.Context, req harness.CancelRequest) error {
	if m.closed.Load() {
		return harness.ErrClosed
	}
	if req.Identity.RunID == "" {
		return harness.ErrInvalidRequest
	}
	return nil
}

// Subscribe returns a fresh replay of the scenario.
func (m *MockEngine) Subscribe(ctx context.Context, req harness.SubscribeRequest) (harness.EventStream, error) {
	if m.closed.Load() {
		return nil, harness.ErrClosed
	}
	if req.Identity.RunID == "" {
		return nil, harness.ErrInvalidRequest
	}
	m.mu.Lock()
	sc := m.scenario
	m.mu.Unlock()
	m.active.Add(1)
	stream := newMockStream(sc, req.Identity.RunID, &m.active, &m.nextSequence)
	if req.AfterSequence > 0 {
		stream.skipUntil = req.AfterSequence
	}
	return stream, nil
}

// GetRun returns the recorded RunView.
func (m *MockEngine) GetRun(ctx context.Context, req harness.GetRunRequest) (harness.RunView, error) {
	if m.closed.Load() {
		return harness.RunView{}, harness.ErrClosed
	}
	if req.Identity.RunID == "" {
		return harness.RunView{}, harness.ErrInvalidRequest
	}
	return m.fullView, nil
}

// GetResult returns the recorded Result once the run reached a terminal event.
func (m *MockEngine) GetResult(ctx context.Context, req harness.GetResultRequest) (harness.ResultView, error) {
	if m.closed.Load() {
		return harness.ResultView{}, harness.ErrClosed
	}
	m.mu.Lock()
	res := m.scenario.Result
	m.mu.Unlock()
	return res, nil
}

// GetSession is unsupported: MockEngine has no session/message ledger behind
// it. Scenario tests that need query behaviour should use a real Build.
func (m *MockEngine) GetSession(_ context.Context, _ harness.GetSessionRequest) (harness.SessionView, error) {
	if m.closed.Load() {
		return harness.SessionView{}, harness.ErrClosed
	}
	return harness.SessionView{}, harness.ErrUnsupportedCapability
}

// ListSessions is unsupported for the same reason as GetSession.
func (m *MockEngine) ListSessions(_ context.Context, _ harness.ListSessionsRequest) (harness.SessionPage, error) {
	if m.closed.Load() {
		return harness.SessionPage{}, harness.ErrClosed
	}
	return harness.SessionPage{}, harness.ErrUnsupportedCapability
}

// ListMessages is unsupported for the same reason as GetSession.
func (m *MockEngine) ListMessages(_ context.Context, _ harness.ListMessagesRequest) (harness.MessagePage, error) {
	if m.closed.Load() {
		return harness.MessagePage{}, harness.ErrClosed
	}
	return harness.MessagePage{}, harness.ErrUnsupportedCapability
}

// Artifacts returns an unsupported stub: MockEngine has no artifact store.
// Scenario tests that need artifact behaviour should use a real Build.
func (m *MockEngine) Artifacts() harness.ArtifactClient {
	return unsupportedArtifactClient{}
}

type unsupportedArtifactClient struct{}

func (unsupportedArtifactClient) Put(context.Context, harness.PutArtifactRequest) (harness.ArtifactInfo, error) {
	return harness.ArtifactInfo{}, harness.ErrUnsupportedCapability
}

func (unsupportedArtifactClient) Get(context.Context, harness.GetArtifactRequest) (*harness.ArtifactContent, error) {
	return nil, harness.ErrUnsupportedCapability
}

func (unsupportedArtifactClient) Head(context.Context, harness.GetArtifactRequest) (harness.ArtifactInfo, error) {
	return harness.ArtifactInfo{}, harness.ErrUnsupportedCapability
}

func (unsupportedArtifactClient) List(context.Context, harness.ListArtifactsRequest) (harness.ArtifactPage, error) {
	return harness.ArtifactPage{}, harness.ErrUnsupportedCapability
}

// Readiness always reports Ready.
func (m *MockEngine) Readiness(context.Context) (harness.ReadinessReport, error) {
	return harness.ReadinessReport{Ready: true, CheckedAt: time.Now()}, nil
}

// Report returns a static BuildReport describing the mock.
func (m *MockEngine) Report() harness.BuildReport {
	return harness.BuildReport{
		SDKVersion:  harness.Version,
		Environment: "testkit",
		Providers:   []string{"testkit.mock"},
	}
}

// Close marks the engine closed and waits for active streams to drain.
func (m *MockEngine) Close(ctx context.Context) error {
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	done := make(chan struct{})
	go func() {
		m.active.Wait()
		close(done)
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Assert MockEngine satisfies the Engine interface at compile time.
var _ harness.Engine = (*MockEngine)(nil)

// mockExecution is the Execution returned by MockEngine.Start / Resume.
type mockExecution struct {
	handle harness.RunHandle
	stream *mockStream
}

func (x *mockExecution) Handle() harness.RunHandle   { return x.handle }
func (x *mockExecution) Events() harness.EventStream { return x.stream }

// OutputValidation is a testkit stub that always returns the zero result;
// scripted OutputValidator behaviour lives in a Scenario field once needed.
func (x *mockExecution) OutputValidation() harnessExtOutputResult {
	return harnessExtOutputResult{}
}

// ProjectedFrames is a testkit stub that returns an empty frame slice;
// scripted projector behaviour lives in a Scenario field once needed.
func (x *mockExecution) ProjectedFrames() []extension.Frame {
	return nil
}

// mockStream replays a Scenario's Events one at a time.
type mockStream struct {
	events    []harness.Event
	policy    StreamPolicy
	runID     string
	cursor    atomic.Int64
	next      int
	skipUntil int64
	// closed is set once the stream is drained or Close is called.
	closed atomic.Bool
	active *sync.WaitGroup
	// panicked is used to guarantee we release the WaitGroup exactly once.
	released atomic.Bool
	seqPool  *int64
}

func newMockStream(sc Scenario, runID string, active *sync.WaitGroup, seqPool *int64) *mockStream {
	// Stamp Sequences deterministically starting at 1 per run.
	events := make([]harness.Event, len(sc.Events))
	for i, ev := range sc.Events {
		ev.Sequence = int64(i + 1)
		if ev.RunID == "" {
			ev.RunID = runID
		}
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = time.Now()
		}
		events[i] = ev
	}
	return &mockStream{
		events:  events,
		policy:  sc.StreamPolicy,
		runID:   runID,
		active:  active,
		seqPool: seqPool,
	}
}

func (s *mockStream) release() {
	if s.released.CompareAndSwap(false, true) && s.active != nil {
		s.active.Done()
	}
}

func (s *mockStream) Next(ctx context.Context) (harness.Event, error) {
	if s.closed.Load() {
		return harness.Event{}, harness.ErrClosed
	}
	for {
		if ctx == nil {
			ctx = context.Background()
		}
		if s.policy.PanicAfter > 0 && s.next+1 == s.policy.PanicAfter {
			s.release()
			panic("testkit: scripted panic")
		}
		if s.policy.BreakAfter > 0 && s.next >= s.policy.BreakAfter {
			s.release()
			return harness.Event{}, io.EOF
		}
		if s.next >= len(s.events) {
			s.release()
			return harness.Event{}, io.EOF
		}
		ev := s.events[s.next]
		s.next++
		if s.skipUntil > 0 && ev.Sequence <= s.skipUntil {
			continue
		}
		s.cursor.Store(ev.Sequence)
		if s.policy.DelayPerEvent > 0 {
			select {
			case <-time.After(s.policy.DelayPerEvent):
			case <-ctx.Done():
				return harness.Event{}, ctx.Err()
			}
		}
		return ev, nil
	}
}

func (s *mockStream) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.release()
	return nil
}

func (s *mockStream) Cursor() harness.Cursor {
	return harness.Cursor{AfterSequence: s.cursor.Load(), RunID: s.runID}
}

// TerminalEvent produces a well-formed terminal event for a Scenario. It is
// a small helper so table-driven tests do not have to hand-craft the
// Visibility / Sequence pair.
func TerminalEvent(kind harness.EventType) harness.Event {
	if kind == "" {
		kind = harness.EventRunCompleted
	}
	return harness.Event{
		EventType:  kind,
		Visibility: harness.VisibilityDebug,
	}
}

// TextDeltaEvent produces a user-visible agent_text_delta event carrying text
// as a JSON preview.
func TextDeltaEvent(text string) harness.Event {
	preview, _ := json.Marshal(map[string]string{"text": text})
	return harness.Event{
		EventType:      harness.EventAgentTextDelta,
		Visibility:     harness.VisibilityUserVisible,
		PayloadPreview: preview,
	}
}

// FinalResponseEvent produces a user-visible final_response event.
func FinalResponseEvent(text string) harness.Event {
	preview, _ := json.Marshal(map[string]string{"text": text})
	return harness.Event{
		EventType:      harness.EventFinalResponse,
		Visibility:     harness.VisibilityUserVisible,
		PayloadPreview: preview,
	}
}

// ErrScriptedFailure is a canned sentinel that scripts can return from
// Scenario.RunError to model an entry-level failure.
var ErrScriptedFailure = errors.New("testkit: scripted start failure")
