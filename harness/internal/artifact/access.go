package artifact

import "context"

func requireActor(ctx context.Context) (Actor, error) {
	if ctx == nil {
		return Actor{}, errorf(ErrPermissionDenied, "missing or invalid actor")
	}
	actor, ok := ActorFromContext(ctx)
	if !ok || actor.TenantID == "" || !isActorRole(actor.Role) {
		return Actor{}, errorf(ErrPermissionDenied, "missing or invalid actor")
	}
	if actor.Role == ActorUser || actor.Role == ActorRuntime || actor.Role == ActorContextEngine {
		if actor.SessionID == "" || actor.RunID == "" {
			return Actor{}, errorf(ErrPermissionDenied, "actor session and run are required")
		}
	}
	if actor.Role == ActorHost && actor.SessionID == "" {
		return Actor{}, errorf(ErrPermissionDenied, "actor session is required")
	}
	if (actor.Role == ActorUser || actor.Role == ActorContextEngine || actor.Role == ActorHost) && actor.UserID == "" {
		return Actor{}, errorf(ErrPermissionDenied, "actor user is required")
	}
	return actor, nil
}

func validatePutScope(ctx context.Context, req PutArtifactRequest) error {
	actor, err := requireActor(ctx)
	if err != nil {
		return err
	}
	if actor.TenantID != req.TenantID {
		return errorf(ErrPermissionDenied, "tenant mismatch")
	}
	switch actor.Role {
	case ActorUser, ActorRuntime, ActorContextEngine:
		if actor.SessionID != req.SessionID || actor.RunID != req.RunID {
			return errorf(ErrPermissionDenied, "artifact scope mismatch")
		}
	case ActorHost:
		// 宿主写入是会话作用域：session 必须匹配，不比对 RunID（宿主可为
		// 尚未开始的 Run 预上传资产）。写入面收敛为业务类型与非受限可见性。
		if actor.SessionID != req.SessionID {
			return errorf(ErrPermissionDenied, "session mismatch")
		}
		if err := validateHostPutSurface(req); err != nil {
			return err
		}
	case ActorDebug, ActorAudit:
		if actor.SessionID != "" && actor.SessionID != req.SessionID {
			return errorf(ErrPermissionDenied, "session mismatch")
		}
		if actor.RunID != "" && actor.RunID != req.RunID {
			return errorf(ErrPermissionDenied, "run mismatch")
		}
	}
	if (actor.Role == ActorUser || actor.Role == ActorContextEngine || actor.Role == ActorHost) && actor.UserID != req.UserID {
		return errorf(ErrPermissionDenied, "user mismatch")
	}
	return nil
}

// hostWritableArtifactTypes 是 ActorHost 允许写入的业务类型白名单。
var hostWritableArtifactTypes = map[ArtifactType]struct{}{
	ArtifactTypeFile:     {},
	ArtifactTypeImage:    {},
	ArtifactTypeSchema:   {},
	ArtifactTypeHostData: {},
}

// hostDeniedArtifactTypes 是 ActorHost 读取时拒绝的系统内部类型黑名单。
var hostDeniedArtifactTypes = map[ArtifactType]struct{}{
	ArtifactTypeCheckpointState: {},
	ArtifactTypeContextSnapshot: {},
	ArtifactTypeControlResponse: {},
}

func validateHostPutSurface(req PutArtifactRequest) error {
	if _, ok := hostWritableArtifactTypes[req.ArtifactType]; !ok {
		return errorf(ErrPermissionDenied, "artifact type not writable by host")
	}
	if req.Visibility != VisibilityUserVisible && req.Visibility != VisibilityDebug {
		return errorf(ErrPermissionDenied, "artifact visibility not writable by host")
	}
	return nil
}

func validateRequestedRefScope(actor Actor, ref string) error {
	parts, err := parseCanonicalArtifactRef(ref)
	if err != nil {
		return err
	}
	return validateRefPartsScope(actor, parts)
}

func validateRefPartsScope(actor Actor, parts RefParts) error {
	if actor.TenantID != parts.TenantID {
		return errorf(ErrPermissionDenied, "tenant mismatch")
	}
	switch actor.Role {
	case ActorUser, ActorRuntime:
		if actor.SessionID != parts.SessionID || actor.RunID != parts.RunID {
			return errorf(ErrPermissionDenied, "artifact scope mismatch")
		}
	case ActorContextEngine, ActorHost:
		// 会话作用域主体：同 session 内允许跨 Run 读取。
		if actor.SessionID != parts.SessionID {
			return errorf(ErrPermissionDenied, "artifact session mismatch")
		}
	case ActorDebug, ActorAudit:
		if actor.SessionID != "" && actor.SessionID != parts.SessionID {
			return errorf(ErrPermissionDenied, "session mismatch")
		}
		if actor.RunID != "" && actor.RunID != parts.RunID {
			return errorf(ErrPermissionDenied, "run mismatch")
		}
	}
	return nil
}

func authorize(ctx context.Context, meta ArtifactMeta, purpose Purpose) error {
	if err := validatePurpose(purpose); err != nil {
		return err
	}
	if !isVisibility(meta.Visibility) {
		return errorf(ErrInvalidArgument, "invalid visibility")
	}
	actor, err := requireActor(ctx)
	if err != nil {
		return err
	}
	if actor.TenantID != meta.TenantID {
		return errorf(ErrPermissionDenied, "tenant mismatch")
	}
	if actor.Role == ActorUser || actor.Role == ActorRuntime {
		if actor.SessionID != meta.SessionID || actor.RunID != meta.RunID {
			return errorf(ErrPermissionDenied, "artifact scope mismatch")
		}
	}
	if actor.Role == ActorContextEngine {
		if actor.SessionID != meta.SessionID || meta.UserID == "" || actor.UserID != meta.UserID {
			return errorf(ErrPermissionDenied, "artifact context scope mismatch")
		}
	}
	if actor.Role == ActorHost {
		if actor.SessionID != meta.SessionID || meta.UserID == "" || actor.UserID != meta.UserID {
			return errorf(ErrPermissionDenied, "artifact host scope mismatch")
		}
		// 系统内部类型对宿主永远关闭，防止宿主读到 checkpoint / 上下文快照等
		// 平台事实。
		if _, denied := hostDeniedArtifactTypes[meta.ArtifactType]; denied {
			return errorf(ErrPermissionDenied, "artifact type denied for host")
		}
	}
	if actor.Role == ActorUser && (meta.UserID == "" || actor.UserID != meta.UserID) {
		return errorf(ErrPermissionDenied, "user mismatch")
	}
	if actor.Role == ActorDebug || actor.Role == ActorAudit {
		if actor.SessionID != "" && actor.SessionID != meta.SessionID {
			return errorf(ErrPermissionDenied, "session mismatch")
		}
		if actor.RunID != "" && actor.RunID != meta.RunID {
			return errorf(ErrPermissionDenied, "run mismatch")
		}
	}
	return authorizeVisibility(actor.Role, meta.Visibility, purpose)
}

func authorizeVisibility(role ActorRole, visibility Visibility, purpose Purpose) error {
	// Context Engine is a narrowly-scoped model-input principal. It may cross a
	// Run boundary inside one user Session only for governed context assembly.
	if role == ActorContextEngine && purpose != PurposeModelContext {
		return errorf(ErrPermissionDenied, "context engine purpose denied")
	}
	// 宿主主体只能以 host_access / view / download 用途读取。
	if role == ActorHost && purpose != PurposeHostAccess && purpose != PurposeView && purpose != PurposeDownload {
		return errorf(ErrPermissionDenied, "host purpose denied")
	}
	switch visibility {
	case VisibilityUserVisible:
		return nil
	case VisibilityInternal:
		if role == ActorRuntime || role == ActorContextEngine || role == ActorDebug || role == ActorAudit || role == ActorHost {
			return nil
		}
	case VisibilityDebug:
		if role == ActorDebug || role == ActorAudit || role == ActorHost || role == ActorRuntime && purpose == PurposeDebug {
			return nil
		}
	case VisibilityRestricted:
		if role == ActorDebug || role == ActorAudit {
			return nil
		}
	default:
		return errorf(ErrInvalidArgument, "invalid visibility")
	}
	return errorf(ErrPermissionDenied, "artifact visibility denied")
}
