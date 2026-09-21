package artifact_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

func validPutRequest() PutArtifactRequest {
	return PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "tc-1",
		ArtifactType: ArtifactTypeToolResult,
		MimeType:     "application/json",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader(`{"ok":true}`),
	}
}

func TestStoreRejectsUnknownContractValuesAndMalformedMIME(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PutArtifactRequest)
	}{
		{"owner", func(r *PutArtifactRequest) { r.OwnerModule = OwnerModule("private") }},
		{"type", func(r *PutArtifactRequest) { r.ArtifactType = ArtifactType("partial") }},
		{"visibility", func(r *PutArtifactRequest) { r.Visibility = Visibility("user") }},
		{"retention", func(r *PutArtifactRequest) { r.RetentionPolicy = RetentionPolicy("forever") }},
		{"mime", func(r *PutArtifactRequest) { r.MimeType = "not a mime" }},
		{"tenant traversal", func(r *PutArtifactRequest) { r.TenantID = ".." }},
		{"session separator", func(r *PutArtifactRequest) { r.SessionID = "session/child" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validPutRequest()
			tc.mutate(&req)
			_, err := newTestStore(t).Put(runtimeContext(), req)
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestIsErrorCodeTraversesWrappedAndJoinedErrors(t *testing.T) {
	primary := &Error{Code: ErrConflict, Message: "conflict"}
	err := errors.Join(fmt.Errorf("metadata: %w", primary), errors.New("cleanup failed"))
	if !IsErrorCode(err, ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}
