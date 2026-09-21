package ruleworker

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

const LockAdminPath = "/api/v1/agenui/admin/rule-worker/lock"

type lockAdminHandler struct {
	admin LockAdministrator
}

// RegisterLockAdminHTTP installs the operational endpoint for inspecting and
// manually releasing the rule-worker pass lock.
func RegisterLockAdminHTTP(mux *http.ServeMux, admin LockAdministrator) {
	handler := &lockAdminHandler{admin: admin}
	mux.Handle("GET "+LockAdminPath, handler)
	mux.Handle("DELETE "+LockAdminPath, handler)
}

func (h *lockAdminHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if h.admin == nil {
		writeLockAdminError(response, http.StatusServiceUnavailable, "rule-worker lock administration is disabled")
		return
	}

	switch request.Method {
	case http.MethodGet:
		status, err := h.admin.LockStatus(request.Context())
		if err != nil {
			writeLockAdminError(response, http.StatusBadGateway, err.Error())
			return
		}
		writeLockAdminJSON(response, http.StatusOK, status)
	case http.MethodDelete:
		released, err := h.admin.ForceRelease(request.Context())
		if errors.Is(err, ErrManualUnlockUnsupported) {
			writeLockAdminError(response, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeLockAdminError(response, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("[rule-worker] pass lock manually released released=%t remote=%s", released, request.RemoteAddr)
		writeLockAdminJSON(response, http.StatusOK, map[string]bool{"released": released})
	default:
		writeLockAdminError(response, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func writeLockAdminError(response http.ResponseWriter, status int, message string) {
	writeLockAdminJSON(response, status, map[string]string{"error": message})
}

func writeLockAdminJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
