package toolgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

type resolvedToolInput struct {
	arguments    json.RawMessage
	argumentsRef string
}

func resolveToolInput(ctx context.Context, store artifact.ArtifactStore, req ToolCallRequest) (resolvedToolInput, error) {
	hasInline := len(req.Arguments) > 0
	hasRef := req.ArgumentsRef != ""
	if hasInline && hasRef {
		return resolvedToolInput{}, NewToolError(
			ErrorTypeSchemaValidationFailed,
			"arguments and arguments_ref are mutually exclusive",
			false,
			nil,
		)
	}
	if !hasInline && !hasRef {
		return resolvedToolInput{}, NewToolError(
			ErrorTypeSchemaValidationFailed,
			"arguments or arguments_ref is required",
			false,
			nil,
		)
	}
	if hasInline {
		if len(req.Arguments) > maxJSONValueBytes {
			return resolvedToolInput{}, NewToolError(ErrorTypeSchemaValidationFailed, "arguments exceed size limit", false, nil)
		}
		if !json.Valid(req.Arguments) {
			return resolvedToolInput{}, NewToolError(ErrorTypeSchemaValidationFailed, "arguments must be valid JSON", false, nil)
		}
		return resolvedToolInput{arguments: append(json.RawMessage(nil), req.Arguments...)}, nil
	}
	if store == nil {
		return resolvedToolInput{}, NewToolError(ErrorTypeArtifactError, "artifact store is required for arguments_ref", false, nil)
	}

	object, err := store.Get(ctx, req.ArgumentsRef, artifact.GetOptions{Purpose: artifact.PurposeDebug})
	if err != nil {
		return resolvedToolInput{}, NewToolError(ErrorTypeArtifactError, "failed to load arguments artifact", false, err)
	}
	if object == nil || object.Content == nil {
		return resolvedToolInput{}, NewToolError(ErrorTypeArtifactError, "arguments artifact has no content", false, nil)
	}
	defer object.Content.Close()
	if err := validateArtifactScope(object.Meta, req); err != nil {
		return resolvedToolInput{}, err
	}
	content, err := readBoundedArtifact(object.Content, maxJSONValueBytes)
	if err != nil {
		return resolvedToolInput{}, NewToolError(ErrorTypeArtifactError, "failed to read bounded arguments artifact", false, err)
	}
	if !json.Valid(content) {
		return resolvedToolInput{}, NewToolError(ErrorTypeSchemaValidationFailed, "artifact arguments must be valid JSON", false, nil)
	}
	return resolvedToolInput{
		arguments:    json.RawMessage(content),
		argumentsRef: req.ArgumentsRef,
	}, nil
}

func resolveToolSchema(
	ctx context.Context,
	store artifact.ArtifactStore,
	req ToolCallRequest,
	inline json.RawMessage,
	ref string,
) (json.RawMessage, error) {
	if len(inline) > 0 {
		if len(inline) > maxJSONSchemaBytes {
			return nil, NewToolError(ErrorTypeSchemaValidationFailed, "tool schema exceeds size limit", false, nil)
		}
		if !json.Valid(inline) {
			return nil, NewToolError(ErrorTypeSchemaValidationFailed, "tool schema must be valid JSON", false, nil)
		}
		return append(json.RawMessage(nil), inline...), nil
	}
	if ref == "" {
		return nil, nil
	}
	if store == nil {
		return nil, NewToolError(ErrorTypeArtifactError, "artifact store is required for schema ref", false, nil)
	}

	object, err := store.Get(ctx, ref, artifact.GetOptions{Purpose: artifact.PurposeDebug})
	if err != nil {
		return nil, NewToolError(ErrorTypeArtifactError, "failed to load schema artifact", false, err)
	}
	if object == nil || object.Content == nil {
		return nil, NewToolError(ErrorTypeArtifactError, "schema artifact has no content", false, nil)
	}
	defer object.Content.Close()
	if object.Meta.TenantID != req.TenantID {
		return nil, NewToolError(ErrorTypeArtifactError, "schema artifact tenant does not match request", false, nil)
	}
	if object.Meta.ArtifactType != artifact.ArtifactTypeSchema {
		return nil, NewToolError(ErrorTypeArtifactError, "schema ref must identify a schema artifact", false, nil)
	}
	content, err := readBoundedArtifact(object.Content, maxJSONSchemaBytes)
	if err != nil {
		return nil, NewToolError(ErrorTypeArtifactError, "failed to read bounded schema artifact", false, err)
	}
	if !json.Valid(content) {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "schema artifact must contain valid JSON", false, nil)
	}
	return json.RawMessage(content), nil
}

func readBoundedArtifact(reader io.Reader, limit int) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > limit {
		return nil, fmt.Errorf("artifact exceeds %d byte limit", limit)
	}
	return content, nil
}

func validateArtifactScope(meta artifact.ArtifactMeta, req ToolCallRequest) error {
	if meta.TenantID != req.TenantID || meta.SessionID != req.SessionID || meta.RunID != req.RunID {
		return NewToolError(ErrorTypeArtifactError, "arguments artifact scope does not match request", false, nil)
	}
	return nil
}
