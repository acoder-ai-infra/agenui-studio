package ruleworker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeLockAdministrator struct {
	status   LockStatus
	released bool
}

func (f *fakeLockAdministrator) LockStatus(context.Context) (LockStatus, error) {
	return f.status, nil
}

func (f *fakeLockAdministrator) ForceRelease(context.Context) (bool, error) {
	f.released = true
	return true, nil
}

func TestLockAdminHTTPReportsAndReleasesLock(t *testing.T) {
	admin := &fakeLockAdministrator{status: LockStatus{Backend: "redis", Key: "test:pass", Held: true, TTLMillis: 1000}}
	mux := http.NewServeMux()
	RegisterLockAdminHTTP(mux, admin)

	statusRequest := httptest.NewRequest(http.MethodGet, LockAdminPath, nil)
	statusResponse := httptest.NewRecorder()
	mux.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || statusResponse.Body.String() == "" {
		t.Fatalf("status response=%d body=%q", statusResponse.Code, statusResponse.Body.String())
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, LockAdminPath, nil)
	deleteResponse := httptest.NewRecorder()
	mux.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK || !admin.released {
		t.Fatalf("delete response=%d released=%v", deleteResponse.Code, admin.released)
	}
}
