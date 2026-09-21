package artifact

import (
	"fmt"
	"net/url"
	"strings"
)

const refScheme = "artifact"

type RefParts struct {
	TenantID   string
	SessionID  string
	RunID      string
	ArtifactID string
}

func BuildRef(tenantID, sessionID, runID, artifactID string) string {
	return fmt.Sprintf("artifact://tenants/%s/sessions/%s/runs/%s/%s",
		url.PathEscape(tenantID),
		url.PathEscape(sessionID),
		url.PathEscape(runID),
		url.PathEscape(artifactID),
	)
}

func ParseRef(ref string) (RefParts, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return RefParts{}, errorf(ErrInvalidArgument, "parse artifact ref: %v", err)
	}
	if u.Scheme != refScheme || u.Host != "tenants" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(ref, "#") {
		return RefParts{}, errorf(ErrInvalidArgument, "invalid artifact ref: %s", ref)
	}
	escapedPath := u.EscapedPath()
	if !strings.HasPrefix(escapedPath, "/") {
		return RefParts{}, errorf(ErrInvalidArgument, "invalid artifact ref path: %s", ref)
	}
	parts := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(parts) != 6 || parts[1] != "sessions" || parts[3] != "runs" {
		return RefParts{}, errorf(ErrInvalidArgument, "invalid artifact ref path: %s", ref)
	}
	tenantID, err := url.PathUnescape(parts[0])
	if err != nil {
		return RefParts{}, errorf(ErrInvalidArgument, "decode tenant id: %v", err)
	}
	if err := validateIdentifier("tenant_id", tenantID); err != nil {
		return RefParts{}, err
	}
	sessionID, err := url.PathUnescape(parts[2])
	if err != nil {
		return RefParts{}, errorf(ErrInvalidArgument, "decode session id: %v", err)
	}
	if err := validateIdentifier("session_id", sessionID); err != nil {
		return RefParts{}, err
	}
	runID, err := url.PathUnescape(parts[4])
	if err != nil {
		return RefParts{}, errorf(ErrInvalidArgument, "decode run id: %v", err)
	}
	if err := validateIdentifier("run_id", runID); err != nil {
		return RefParts{}, err
	}
	artifactID, err := url.PathUnescape(parts[5])
	if err != nil {
		return RefParts{}, errorf(ErrInvalidArgument, "decode artifact id: %v", err)
	}
	if err := validateIdentifier("artifact_id", artifactID); err != nil {
		return RefParts{}, err
	}
	parsed := RefParts{
		TenantID:   tenantID,
		SessionID:  sessionID,
		RunID:      runID,
		ArtifactID: artifactID,
	}
	if BuildRef(parsed.TenantID, parsed.SessionID, parsed.RunID, parsed.ArtifactID) != ref {
		return RefParts{}, errorf(ErrInvalidArgument, "non-canonical artifact ref")
	}
	return parsed, nil
}
