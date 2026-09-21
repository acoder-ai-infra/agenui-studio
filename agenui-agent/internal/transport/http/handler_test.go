package httptransport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	harness "github.com/AGenUI/agenui-studio/harness/sdk"
)

type missingStepArtifacts struct {
	err error
}

func (store missingStepArtifacts) Load(context.Context, harness.Identity, string) (string, error) {
	return "", store.err
}

func (store missingStepArtifacts) LatestRunID(context.Context, harness.Identity, string) (string, error) {
	return "", store.err
}

type presentationArtifactStore struct {
	latest  map[string]stepartifact.LatestStep
	payload map[string]string
}

func (s presentationArtifactStore) LatestStep(_ context.Context, _ harness.Identity, step string) (stepartifact.LatestStep, error) {
	latest, ok := s.latest[step]
	if !ok {
		return stepartifact.LatestStep{}, stepartifact.ErrNotFound
	}
	return latest, nil
}

func (s presentationArtifactStore) LatestRunID(ctx context.Context, identity harness.Identity, step string) (string, error) {
	latest, err := s.LatestStep(ctx, identity, step)
	return latest.RunID, err
}

func (s presentationArtifactStore) Load(_ context.Context, identity harness.Identity, step string) (string, error) {
	payload, ok := s.payload[identity.RunID+":"+step]
	if !ok {
		return "", stepartifact.ErrNotFound
	}
	return payload, nil
}

type unusedPackageExporter struct{}

func (unusedPackageExporter) Export(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func TestFinalArtifactReturnsNoContentBeforeDesignExists(t *testing.T) {
	handler := &Handler{
		resolvePrincipal: func(*http.Request) (Principal, error) {
			return Principal{TenantID: "tenant", UserID: "user"}, nil
		},
		steps: missingStepArtifacts{err: stepartifact.ErrNotFound},
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	recorder := httptest.NewRecorder()

	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agenui/agent/sessions/session_waiting/final",
		nil,
	))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("204 response must be empty, got %q", recorder.Body.String())
	}
}

func TestFinalArtifactReportsArtifactStoreFailure(t *testing.T) {
	handler := &Handler{
		resolvePrincipal: func(*http.Request) (Principal, error) {
			return Principal{TenantID: "tenant", UserID: "user"}, nil
		},
		steps: missingStepArtifacts{err: errors.New("store unavailable")},
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	recorder := httptest.NewRecorder()

	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet,
		"/api/v1/agenui/agent/sessions/session_failed/final",
		nil,
	))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestFinalArtifactPrefersNewerDesignOverStaleFinal(t *testing.T) {
	then := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	store := presentationArtifactStore{
		latest: map[string]stepartifact.LatestStep{
			stepartifact.StepFinal:  {RunID: "final-run", CreatedAt: then},
			stepartifact.StepDesign: {RunID: "design-edit-run", CreatedAt: then.Add(time.Minute)},
		},
		payload: map[string]string{
			"final-run:" + stepartifact.StepFinal:        `[{"updateComponents":{"components":[{"id":"root","styles":{"background-color":"#0B0E14"}}]}}]`,
			"design-edit-run:" + stepartifact.StepDesign: `{"schema_version":"agenui.design-artifact/v1","messages":[{"updateComponents":{"components":[{"id":"root","styles":{"background-color":"#FFFFFF"}}]}}],"field_slots":[],"action_slots":[]}`,
		},
	}
	handler, err := NewHandler(Config{}, Dependencies{
		ResolvePrincipal: func(*http.Request) (Principal, error) { return Principal{TenantID: "tenant", UserID: "user"}, nil },
		StepArtifacts:    store,
		PackageExporter:  unusedPackageExporter{},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/agenui/agent/sessions/session/final", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"draft":true`) {
		t.Fatalf("expected newer design to be returned as draft, got %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"runId":"design-edit-run"`) {
		t.Fatalf("expected design edit run, got %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `#FFFFFF`) || strings.Contains(recorder.Body.String(), `#0B0E14`) {
		t.Fatalf("expected latest white design instead of stale final, got %s", recorder.Body.String())
	}
}

func TestFinalArtifactTreatsBlockedBindingAsDraft(t *testing.T) {
	then := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	store := presentationArtifactStore{
		latest: map[string]stepartifact.LatestStep{
			stepartifact.StepDesign:  {RunID: "run", CreatedAt: then},
			stepartifact.StepBinding: {RunID: "run", CreatedAt: then.Add(time.Second)},
			stepartifact.StepFinal:   {RunID: "run", CreatedAt: then.Add(2 * time.Second)},
		},
		payload: map[string]string{
			"run:" + stepartifact.StepDesign:  `{"schema_version":"agenui.design-artifact/v1","messages":[{"updateComponents":{"components":[{"id":"root","styles":{"background-color":"#FFFFFF"}}]}}],"field_slots":[],"action_slots":[]}`,
			"run:" + stepartifact.StepBinding: `{"schema_version":"agenui.binding-submission/v1","result":{"schema_version":"agenui.bind-result/v1","status":"blocked","bindings":[],"issues":[]},"plan":{"schema_version":"agenui.executable-binding/v1","field_mappings":[],"action_mappings":[]}}`,
			"run:" + stepartifact.StepFinal:   `[{"updateComponents":{"components":[{"id":"root","styles":{"background-color":"#000000"}}]}}]`,
		},
	}
	body := finalArtifactBody(t, store)
	for _, fragment := range []string{`"draft":true`, `"executable":false`, `"publishable":false`, `"bindingStatus":"blocked"`, `#FFFFFF`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("blocked presentation missing %s: %s", fragment, body)
		}
	}
	if strings.Contains(body, `#000000`) {
		t.Fatalf("blocked presentation exposed executable Final: %s", body)
	}
}

func TestFinalArtifactMarksReadyCurrentBindingExecutable(t *testing.T) {
	then := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	store := presentationArtifactStore{
		latest: map[string]stepartifact.LatestStep{
			stepartifact.StepDesign:  {RunID: "run", CreatedAt: then},
			stepartifact.StepBinding: {RunID: "run", CreatedAt: then.Add(time.Second)},
			stepartifact.StepFinal:   {RunID: "run", CreatedAt: then.Add(2 * time.Second)},
		},
		payload: map[string]string{
			"run:" + stepartifact.StepDesign:  `{"schema_version":"agenui.design-artifact/v1","messages":[],"field_slots":[],"action_slots":[]}`,
			"run:" + stepartifact.StepBinding: `{"schema_version":"agenui.binding-submission/v1","result":{"schema_version":"agenui.bind-result/v1","status":"ready","bindings":[],"issues":[]},"plan":{"schema_version":"agenui.executable-binding/v1","field_mappings":[],"action_mappings":[]}}`,
			"run:" + stepartifact.StepFinal:   `[{"updateComponents":{"components":[{"id":"root"}]}}]`,
		},
	}
	body := finalArtifactBody(t, store)
	for _, fragment := range []string{`"draft":false`, `"executable":true`, `"publishable":true`, `"bindingStatus":"ready"`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("ready presentation missing %s: %s", fragment, body)
		}
	}
}

func finalArtifactBody(t *testing.T, store presentationArtifactStore) string {
	t.Helper()
	handler, err := NewHandler(Config{}, Dependencies{
		ResolvePrincipal: func(*http.Request) (Principal, error) { return Principal{TenantID: "tenant", UserID: "user"}, nil },
		StepArtifacts:    store,
		PackageExporter:  unusedPackageExporter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/agenui/agent/sessions/session/final", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}
