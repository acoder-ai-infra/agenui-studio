package artifact

import (
	"mime"
	"strings"
	"unicode"
)

func validatePutContract(req PutArtifactRequest) error {
	if req.TenantID == "" || req.SessionID == "" || req.RunID == "" {
		return errorf(ErrInvalidArgument, "tenant_id, session_id and run_id are required")
	}
	for _, identifier := range []struct {
		name  string
		value string
	}{
		{"tenant_id", req.TenantID},
		{"session_id", req.SessionID},
		{"run_id", req.RunID},
	} {
		if err := validateIdentifier(identifier.name, identifier.value); err != nil {
			return err
		}
	}
	if req.ArtifactID != "" {
		if err := validateIdentifier("artifact_id", req.ArtifactID); err != nil {
			return err
		}
		if req.IdempotencyKey == "" {
			return errorf(ErrInvalidArgument, "idempotency_key is required with artifact_id")
		}
	}
	if !isOwnerModule(req.OwnerModule) || req.OwnerID == "" {
		return errorf(ErrInvalidArgument, "invalid artifact owner")
	}
	if !isArtifactType(req.ArtifactType) {
		return errorf(ErrInvalidArgument, "invalid artifact_type")
	}
	if _, _, err := mime.ParseMediaType(req.MimeType); err != nil {
		return errorf(ErrInvalidArgument, "invalid mime_type")
	}
	if !isVisibility(req.Visibility) {
		return errorf(ErrInvalidArgument, "invalid visibility")
	}
	if req.RetentionPolicy != "" && !isRetentionPolicy(req.RetentionPolicy) {
		return errorf(ErrInvalidArgument, "invalid retention_policy")
	}
	if req.Content == nil {
		return errorf(ErrInvalidArgument, "content is required")
	}
	for _, item := range req.DerivedFrom {
		if item.ArtifactRef == "" || !isLineageRelation(item.Relation) {
			return errorf(ErrInvalidArgument, "invalid lineage")
		}
	}
	return nil
}

func validatePurpose(value Purpose) error {
	switch value {
	case PurposeView, PurposeDownload, PurposeModelContext, PurposeDebug, PurposeReplay, PurposeHostAccess:
		return nil
	}
	return errorf(ErrInvalidArgument, "invalid purpose")
}

func validateDeleteReason(value DeleteReason) error {
	switch value {
	case DeleteReasonUser, DeleteReasonTTL, DeleteReasonCleanup:
		return nil
	}
	return errorf(ErrInvalidArgument, "invalid delete reason")
}

func isOwnerModule(value OwnerModule) bool {
	switch value {
	case OwnerModuleToolGateway, OwnerModuleContextEngine, OwnerModuleRuntime, OwnerModuleA2AGateway, OwnerModuleObservability, OwnerModuleProtocol, OwnerModuleModelGateway, OwnerModuleSkill, OwnerModuleHost:
		return true
	}
	return false
}

func isArtifactType(value ArtifactType) bool {
	switch value {
	case ArtifactTypeToolResult, ArtifactTypeContextSnapshot, ArtifactTypeCheckpointState, ArtifactTypeDebugPayload, ArtifactTypeFinalResult, ArtifactTypeFile, ArtifactTypeImage, ArtifactTypeSchema, ArtifactTypePrompt, ArtifactTypeControlResponse, ArtifactTypeHostData:
		return true
	}
	return false
}

func isVisibility(value Visibility) bool {
	switch value {
	case VisibilityUserVisible, VisibilityInternal, VisibilityDebug, VisibilityRestricted:
		return true
	}
	return false
}

func isRetentionPolicy(value RetentionPolicy) bool {
	switch value {
	case RetentionRunTTL, RetentionSessionTTL, RetentionDebugShortTTL, RetentionAuditTTL, RetentionCheckpointTTL:
		return true
	}
	return false
}

func isLineageRelation(value LineageRelation) bool {
	switch value {
	case LineageUploadedFrom, LineageTransformedFrom, LineageGeneratedFrom, LineageSummarizedFrom, LineageMergedFrom, LineageReferenced:
		return true
	}
	return false
}

func isActorRole(value ActorRole) bool {
	switch value {
	case ActorUser, ActorRuntime, ActorContextEngine, ActorDebug, ActorAudit, ActorHost:
		return true
	}
	return false
}

func validateIdentifier(name, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return errorf(ErrInvalidArgument, "invalid %s", name)
	}
	return nil
}
