package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/authcontext"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
)

// tenantAdminScope gates the platform-level tenant directory (Owner console).
// The agent.config.admin super-scope satisfies it, so the insecure authenticator
// and any admin JWT can manage tenants.
const tenantAdminScope = "platform.tenant.admin"

// tenantSessionCookie mirrors app.TenantSessionCookie. It is duplicated here to
// avoid the protocol layer importing internal/app (composition root).
const tenantSessionCookie = "harness_tenant"

func ownerPrincipal(w http.ResponseWriter, r *http.Request) (authcontext.Principal, bool) {
	principal, ok := authcontext.FromContext(r.Context())
	if !ok || !principal.HasScope(tenantAdminScope) {
		writeError(w, http.StatusForbidden, "TENANT_ADMIN_FORBIDDEN", "platform tenant administration permission is required")
		return authcontext.Principal{}, false
	}
	return principal, true
}

// --- Owner CRUD ---------------------------------------------------------------

func (d *Deps) handleListTenants(w http.ResponseWriter, r *http.Request) {
	if _, ok := ownerPrincipal(w, r); !ok || !requireManagementService(w, d.Tenants) {
		return
	}
	items, err := d.Tenants.List(r.Context())
	if err != nil {
		writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (d *Deps) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := ownerPrincipal(w, r)
	if !ok || !requireManagementService(w, d.Tenants) {
		return
	}
	var body struct {
		Definition tenantadmin.TenantDefinition `json:"definition"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	tenant, err := d.Tenants.Create(r.Context(), p.UserID, body.Definition)
	if err != nil {
		writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tenant)
}

func (d *Deps) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := ownerPrincipal(w, r)
	if !ok || !requireManagementService(w, d.Tenants) {
		return
	}
	var body struct {
		Definition       tenantadmin.TenantDefinition `json:"definition"`
		ExpectedRevision int64                        `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.Definition.ID != r.PathValue("id") {
		writeError(w, http.StatusBadRequest, "TENANT_ID_MISMATCH", "path and definition id must match")
		return
	}
	tenant, err := d.Tenants.Update(r.Context(), p.UserID, body.Definition, body.ExpectedRevision)
	if err != nil {
		writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tenant)
}

func (d *Deps) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	if _, ok := ownerPrincipal(w, r); !ok || !requireManagementService(w, d.Tenants) {
		return
	}
	id := r.PathValue("id")
	current, err := d.Tenants.Get(r.Context(), id)
	if err != nil {
		writeTenantError(w, err)
		return
	}
	if err := d.Tenants.Delete(r.Context(), id, current.Revision); err != nil {
		writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

func (d *Deps) handleRegenerateTenantKey(w http.ResponseWriter, r *http.Request) {
	p, ok := ownerPrincipal(w, r)
	if !ok || !requireManagementService(w, d.Tenants) {
		return
	}
	id := r.PathValue("id")
	current, err := d.Tenants.Get(r.Context(), id)
	if err != nil {
		writeTenantError(w, err)
		return
	}
	tenant, err := d.Tenants.RegenerateKey(r.Context(), p.UserID, id, current.Revision)
	if err != nil {
		writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tenant)
}

// --- Tenant options (login picker) --------------------------------------------

// tenantOption is a non-secret directory entry for the login gate's tenant
// picker. It deliberately omits secret_key: the picker lets a user choose which
// tenant to log into, but they must still present that tenant's key.
type tenantOption struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
}

func (d *Deps) handleListTenantOptions(w http.ResponseWriter, r *http.Request) {
	if !requireManagementService(w, d.Tenants) {
		return
	}
	items, err := d.Tenants.List(r.Context())
	if err != nil {
		writeTenantError(w, err)
		return
	}
	options := make([]tenantOption, 0, len(items))
	for _, item := range items {
		options = append(options, tenantOption{TenantID: item.Definition.ID, Name: item.Definition.Name, Status: item.Definition.Status})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": options})
}

// --- Tenant login session -----------------------------------------------------

// tenantSessionView is the redacted body returned to the console after login.
// The secret key is never echoed back here (the console already holds it / the
// Owner page surfaces it); this only confirms which tenant the cookie resolves.
type tenantSessionView struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Plan     string `json:"plan,omitempty"`
	Status   string `json:"status"`
}

func tenantView(t tenantadmin.ManagedTenant) tenantSessionView {
	return tenantSessionView{TenantID: t.Definition.ID, Name: t.Definition.Name, Plan: t.Definition.Plan, Status: t.Definition.Status}
}

func (d *Deps) handleTenantSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleGetTenantSession(w, r)
	case http.MethodPost:
		d.handleCreateTenantSession(w, r)
	case http.MethodDelete:
		d.handleDeleteTenantSession(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "tenant-session supports GET, POST and DELETE")
	}
}

func (d *Deps) handleGetTenantSession(w http.ResponseWriter, r *http.Request) {
	if !requireManagementService(w, d.Tenants) {
		return
	}
	cookie, err := r.Cookie(tenantSessionCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "TENANT_SESSION_ABSENT", "no active tenant session")
		return
	}
	tenant, err := d.Tenants.ResolveSecretKey(r.Context(), cookie.Value)
	if err != nil {
		clearTenantSessionCookie(w)
		writeError(w, http.StatusUnauthorized, "TENANT_SESSION_INVALID", "tenant session is no longer valid")
		return
	}
	writeJSON(w, http.StatusOK, tenantView(tenant))
}

func (d *Deps) handleCreateTenantSession(w http.ResponseWriter, r *http.Request) {
	if !requireManagementService(w, d.Tenants) {
		return
	}
	var body struct {
		SecretKey string `json:"secret_key"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	key := strings.TrimSpace(body.SecretKey)
	if key == "" {
		writeError(w, http.StatusBadRequest, "TENANT_KEY_REQUIRED", "secret_key is required")
		return
	}
	tenant, err := d.Tenants.ResolveSecretKey(r.Context(), key)
	if err != nil {
		if errors.Is(err, tenantadmin.ErrTenantPaused) {
			writeError(w, http.StatusForbidden, "TENANT_PAUSED", "tenant is paused")
			return
		}
		writeError(w, http.StatusUnauthorized, "TENANT_KEY_INVALID", "no tenant matches this key")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     tenantSessionCookie,
		Value:    key,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, tenantView(tenant))
}

func (d *Deps) handleDeleteTenantSession(w http.ResponseWriter, r *http.Request) {
	clearTenantSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ended": true})
}

func clearTenantSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     tenantSessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func writeTenantError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenantadmin.ErrTenantNotFound):
		writeError(w, http.StatusNotFound, "TENANT_NOT_FOUND", strings.TrimSpace(err.Error()))
	case errors.Is(err, tenantadmin.ErrTenantConflict):
		writeError(w, http.StatusConflict, "TENANT_CONFLICT", strings.TrimSpace(err.Error()))
	case errors.Is(err, tenantadmin.ErrDuplicateID):
		writeError(w, http.StatusConflict, "TENANT_DUPLICATE_ID", strings.TrimSpace(err.Error()))
	case errors.Is(err, tenantadmin.ErrDuplicateKey):
		writeError(w, http.StatusConflict, "TENANT_DUPLICATE_KEY", strings.TrimSpace(err.Error()))
	case errors.Is(err, tenantadmin.ErrTenantPaused):
		writeError(w, http.StatusForbidden, "TENANT_PAUSED", strings.TrimSpace(err.Error()))
	case errors.Is(err, tenantadmin.ErrInvalidDefinition):
		writeError(w, http.StatusBadRequest, "TENANT_INVALID", strings.TrimSpace(err.Error()))
	default:
		writeError(w, http.StatusInternalServerError, "TENANT_ERROR", strings.TrimSpace(err.Error()))
	}
}
