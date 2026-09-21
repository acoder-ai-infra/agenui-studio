package server

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
)

func TestWriteDispatchErrorMapsMCPAuthorizationRequired(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeDispatchError(recorder, errors.Join(errors.New("dispatch wrapper"), &mcp.AuthorizationRequiredError{
		ServerID: "mcp_source_control", Provider: "oauth-provider", Resource: "https://mcp.example.test/source-control",
	}))
	if recorder.Code != 428 {
		t.Fatalf("status = %d, want 428; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"error_code":"MCP_AUTHORIZATION_REQUIRED"`) || !strings.Contains(body, `/api/v1/admin/mcp-servers/mcp_source_control/oauth/start`) {
		t.Fatalf("authorization action missing: %s", body)
	}
}
