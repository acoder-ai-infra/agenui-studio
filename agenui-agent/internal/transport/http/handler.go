package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/exportpackage"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

const (
	finalRoute   = "GET /api/v1/agenui/agent/sessions/{id}/final"
	packageRoute = "GET /api/v1/agenui/agent/sessions/{id}/package"
	publishRoute = "POST /api/v1/agenui/agent/sessions/{id}/publish"
)

var safeSessionID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

type Principal struct {
	TenantID string
	UserID   string
}

type PrincipalResolver func(*http.Request) (Principal, error)

type StepArtifactStore interface {
	Load(context.Context, harness.Identity, string) (string, error)
	LatestRunID(context.Context, harness.Identity, string) (string, error)
}

// latestStepArtifactStore is implemented by the production artifact store. It
// lets the presentation endpoint compare immutable artifacts by their actual
// creation time instead of assuming that any Final is newer than a Design.
// Keeping it optional preserves the small test/dummy store contract.
type latestStepArtifactStore interface {
	LatestStep(context.Context, harness.Identity, string) (stepartifact.LatestStep, error)
}

type PackageExporter interface {
	Export(context.Context, string, string, string) ([]byte, error)
}

type PackagePublisher interface {
	Publish(context.Context, string, []byte) (exportpackage.Publication, error)
}

type Config struct{}

type Dependencies struct {
	ResolvePrincipal PrincipalResolver
	StepArtifacts    StepArtifactStore
	PackageExporter  PackageExporter
}

// Handler exposes only AGenUI domain artifacts. Chat, history, Runs, events,
// controls and resume are mounted directly from Harness by bootstrap.App.
type Handler struct {
	resolvePrincipal PrincipalResolver
	steps            StepArtifactStore
	packages         PackageExporter
	publisher        PackagePublisher
}

func (h *Handler) SetPackagePublisher(publisher PackagePublisher) {
	if h != nil {
		h.publisher = publisher
	}
}

func NewHandler(_ Config, dependencies Dependencies) (*Handler, error) {
	if dependencies.ResolvePrincipal == nil || dependencies.StepArtifacts == nil ||
		dependencies.PackageExporter == nil {
		return nil, errors.New("agenui HTTP: artifact dependencies are required")
	}
	return &Handler{
		resolvePrincipal: dependencies.ResolvePrincipal,
		steps:            dependencies.StepArtifacts,
		packages:         dependencies.PackageExporter,
	}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc(finalRoute, h.finalArtifact)
	mux.HandleFunc(packageRoute, h.downloadPackage)
	mux.HandleFunc(publishRoute, h.publishPackage)
}

func (h *Handler) publishPackage(response http.ResponseWriter, request *http.Request) {
	sessionID := request.PathValue("id")
	if !safeSessionID.MatchString(sessionID) || h.publisher == nil {
		writeJSONError(response, http.StatusNotFound, "package publication unavailable")
		return
	}
	principal, ok := h.principal(response, request)
	if !ok {
		return
	}
	pkg, err := h.packages.Export(request.Context(), principal.TenantID, principal.UserID, sessionID)
	if err != nil {
		writeJSONError(response, http.StatusConflict, "executable package unavailable: "+err.Error())
		return
	}
	event, err := h.publisher.Publish(request.Context(), sessionID, pkg)
	if err != nil {
		writeJSONError(response, http.StatusBadGateway, err.Error())
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"status": "published", "deliveryId": event.DeliveryID,
	})
}

func writeJSONError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": message})
}

func (h *Handler) principal(response http.ResponseWriter, request *http.Request) (Principal, bool) {
	principal, err := h.resolvePrincipal(request)
	if err != nil || strings.TrimSpace(principal.TenantID) == "" || strings.TrimSpace(principal.UserID) == "" {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return Principal{}, false
	}
	return principal, true
}

func (h *Handler) finalArtifact(response http.ResponseWriter, request *http.Request) {
	sessionID := request.PathValue("id")
	if !safeSessionID.MatchString(sessionID) {
		http.Error(response, "final artifact unavailable", http.StatusNotFound)
		return
	}
	principal, ok := h.principal(response, request)
	if !ok {
		return
	}
	identity := harness.Identity{
		TenantID: principal.TenantID, UserID: principal.UserID, SessionID: sessionID,
	}
	final, finalErr := latestStep(request.Context(), h.steps, identity, stepartifact.StepFinal)
	if finalErr != nil && !errors.Is(finalErr, stepartifact.ErrNotFound) {
		http.Error(response, "load final artifact", http.StatusInternalServerError)
		return
	}
	design, designErr := latestStep(request.Context(), h.steps, identity, stepartifact.StepDesign)
	if designErr != nil && !errors.Is(designErr, stepartifact.ErrNotFound) {
		http.Error(response, "load design artifact", http.StatusInternalServerError)
		return
	}
	if finalErr != nil && designErr != nil {
		// A valid session can legitimately have no design yet while ask_user is
		// waiting. Treat that as an empty read result instead of a missing route.
		response.WriteHeader(http.StatusNoContent)
		return
	}
	bindingStep, bindingErr := latestStep(request.Context(), h.steps, identity, stepartifact.StepBinding)
	binding := ""
	bindingStatus := ""
	bindingExecutable := false
	if bindingErr == nil {
		bindingIdentity := identity
		bindingIdentity.RunID = bindingStep.RunID
		if loaded, loadErr := h.steps.Load(request.Context(), bindingIdentity, stepartifact.StepBinding); loadErr == nil {
			binding = loaded
			if submission, parseErr := bindingcontract.ParseSubmission(loaded); parseErr == nil {
				bindingStatus = submission.Result.Status
				bindingExecutable = bindingcontract.IsExecutableStatus(bindingStatus)
			}
		}
	}
	// Delivery readiness is stricter than Run completion. A Final is executable
	// only when it is at least as new as the latest Design and executable
	// Binding. Blocked/uncertain Bindings remain valid outcomes but render the
	// latest Design as a non-publishable draft.
	executable := finalErr == nil && bindingErr == nil && bindingExecutable &&
		(designErr != nil || !design.CreatedAt.After(final.CreatedAt)) &&
		!bindingStep.CreatedAt.After(final.CreatedAt)
	draft := !executable
	selected := final
	useDesign := draft && designErr == nil
	if useDesign {
		selected = design
	}
	identity.RunID = selected.RunID
	result := ""
	if useDesign {
		design, loadErr := h.steps.Load(request.Context(), identity, stepartifact.StepDesign)
		if loadErr != nil {
			http.Error(response, "design artifact unavailable", http.StatusNotFound)
			return
		}
		result, loadErr = workspace.ArtifactMessagesJSON(design)
		if loadErr != nil || !json.Valid([]byte(result)) {
			http.Error(response, "design artifact is invalid", http.StatusInternalServerError)
			return
		}
	} else {
		finalResult, loadErr := h.steps.Load(request.Context(), identity, stepartifact.StepFinal)
		if loadErr != nil {
			http.Error(response, "final artifact unavailable", http.StatusNotFound)
			return
		}
		result = finalResult
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"sessionId": sessionID, "runId": selected.RunID, "result": json.RawMessage(result),
		"bindings": binding, "bindingStatus": bindingStatus,
		"draft": draft, "executable": executable, "publishable": executable,
	})
}

func latestStep(ctx context.Context, store StepArtifactStore, identity harness.Identity, step string) (stepartifact.LatestStep, error) {
	if timed, ok := store.(latestStepArtifactStore); ok {
		return timed.LatestStep(ctx, identity, step)
	}
	runID, err := store.LatestRunID(ctx, identity, step)
	if err != nil {
		return stepartifact.LatestStep{}, err
	}
	return stepartifact.LatestStep{RunID: runID}, nil
}

func (h *Handler) downloadPackage(response http.ResponseWriter, request *http.Request) {
	sessionID := request.PathValue("id")
	if !safeSessionID.MatchString(sessionID) {
		http.Error(response, "package unavailable", http.StatusNotFound)
		return
	}
	principal, ok := h.principal(response, request)
	if !ok {
		return
	}
	pkg, err := h.packages.Export(request.Context(), principal.TenantID, principal.UserID, sessionID)
	if err != nil {
		// Export failures are deterministic domain validation errors. Return the
		// concrete reason so the Studio (and an integrating Agent) can correct
		// the admitted source/binding instead of surfacing an opaque conflict.
		http.Error(response, "executable package unavailable: "+err.Error(), http.StatusConflict)
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="agenui-%s.json"`, sessionID))
	_, _ = response.Write(pkg)
}
