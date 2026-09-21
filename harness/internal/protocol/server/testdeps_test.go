package server_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	protocolmemory "github.com/AGenUI/agenui-studio/harness/internal/protocol/memory"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagememory "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

var testEventSequence atomic.Uint64

func newDeps() (server.Deps, storage.Stores) {
	stores := storagememory.New().Stores()
	artifacts := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	return server.Deps{
		Stores:         stores,
		RunService:     storage.NewRunService(stores, nil),
		DefaultAgentID: "test-agent",
		Control:        control.New(stores, nil),
		Artifacts:      artifacts,
		Broker:         protocol.NewMemoryBroker(),
		HotBuffer:      protocolmemory.NewHotBuffer(),
		Logger:         observability.NoopLogger{},
	}, stores
}

func withUser(request *http.Request, userID string) *http.Request {
	tc := observability.MustTraceContext(request.Context())
	tc.UserID = userID
	return request.WithContext(observability.WithTraceContext(request.Context(), tc))
}

func seedRun(t *testing.T, stores storage.Stores, sessionID, runID, tenantID string) {
	t.Helper()
	ctx := context.Background()
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: sessionID, TenantID: tenantID, Status: storage.SessionStatusActive}); err != nil && !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: runID, SessionID: sessionID, TenantID: tenantID, Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
}

func seedEvent(t *testing.T, stores storage.Stores, runID string, eventType observability.EventType, visibility observability.EventVisibility) observability.AgentEvent {
	t.Helper()
	run, err := stores.Runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	event := observability.AgentEvent{
		EventID:       fmt.Sprintf("test-event-%d", testEventSequence.Add(1)),
		SchemaVersion: observability.AgentEventSchemaVersion,
		SessionID:     run.SessionID, RunID: runID, EventType: eventType, Visibility: visibility,
	}
	result, err := stores.Events.Append(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	return result.Event
}

func parseSSE(t *testing.T, reader *bufio.Reader, count int) []map[string]string {
	t.Helper()
	frames := make([]map[string]string, 0, count)
	current := map[string]string{}
	for len(frames) < count {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE frame: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(current) > 0 {
				frames = append(frames, current)
				current = map[string]string{}
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if ok {
			current[key] = strings.TrimPrefix(value, " ")
		}
	}
	return frames
}
