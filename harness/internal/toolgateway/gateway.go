package toolgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

type GatewayConfig struct {
	Registry                   ToolRegistry
	SchemaValidator            SchemaValidator
	EventStore                 EventStore
	ArtifactStore              artifact.ArtifactStore
	StepStore                  StepStore
	Idempotency                IdempotencyStore
	PolicyEngine               PolicyEngine
	TraceProvider              observability.TraceProvider
	ToolEventSink              ToolEventSink
	Logger                     observability.StructuredLogger
	Executors                  []ToolExecutor
	IDGenerator                observability.IDGenerator
	Clock                      Clock
	RequireFrozenSourceBinding bool
	TerminalCommitter          ToolTerminalCommitter
}

type DefaultGateway struct {
	registry                   ToolRegistry
	schemaValidator            SchemaValidator
	eventStore                 EventStore
	artifactStore              artifact.ArtifactStore
	stepStore                  StepStore
	idempotency                IdempotencyStore
	policyEngine               PolicyEngine
	traceProvider              observability.TraceProvider
	toolEventSink              ToolEventSink
	logger                     observability.StructuredLogger
	executors                  map[ToolType]ToolExecutor
	ids                        observability.IDGenerator
	clock                      Clock
	requireFrozenSourceBinding bool
	terminalCommitter          ToolTerminalCommitter
}

const terminalPersistenceTimeout = 5 * time.Second

var fallbackIdempotencyClaimMu sync.Mutex

var ErrProductionDependencyInvalid = errors.New("tool gateway production dependency invalid")

func NewGateway(config GatewayConfig) *DefaultGateway {
	ids := config.IDGenerator
	if ids == nil {
		generator := observability.NewULIDGenerator("")
		ids = generator
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	validator := config.SchemaValidator
	if validator == nil {
		validator = NewJSONSchemaValidator(1024)
	}
	logger := config.Logger
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	executors := make(map[ToolType]ToolExecutor, len(config.Executors))
	for _, executor := range config.Executors {
		if executor != nil {
			executors[executor.Type()] = executor
		}
	}
	idempotency := config.Idempotency
	if idempotency == nil {
		idempotency = NewMemoryIdempotencyStore()
	}
	terminalCommitter := config.TerminalCommitter
	if terminalCommitter == nil && config.EventStore != nil {
		terminalCommitter = NewBestEffortMemoryToolTerminalCommitter(config.EventStore)
	}
	return &DefaultGateway{
		registry:                   config.Registry,
		schemaValidator:            validator,
		eventStore:                 config.EventStore,
		artifactStore:              config.ArtifactStore,
		stepStore:                  config.StepStore,
		idempotency:                idempotency,
		policyEngine:               defaultPolicyEngine(config),
		traceProvider:              config.TraceProvider,
		toolEventSink:              config.ToolEventSink,
		logger:                     logger,
		executors:                  executors,
		ids:                        ids,
		clock:                      clock,
		requireFrozenSourceBinding: config.RequireFrozenSourceBinding,
		terminalCommitter:          terminalCommitter,
	}
}

// NewProductionGateway fails at startup when a development-only persistence
// dependency would make idempotency or event ordering process-local.
func NewProductionGateway(config GatewayConfig) (*DefaultGateway, error) {
	if config.Idempotency == nil {
		return nil, fmt.Errorf("%w: durable idempotency store is required", ErrProductionDependencyInvalid)
	}
	config.RequireFrozenSourceBinding = true
	gateway := NewGateway(config)
	if err := gateway.ValidateProduction(); err != nil {
		return nil, err
	}
	return gateway, nil
}

func (g *DefaultGateway) ValidateProduction() error {
	if g == nil || g.registry == nil || g.eventStore == nil || g.idempotency == nil {
		return fmt.Errorf("%w: registry, event store and idempotency store are required", ErrProductionDependencyInvalid)
	}
	if dependency, ok := g.eventStore.(ProductionDependency); !ok || !dependency.ProductionReady() {
		return fmt.Errorf("%w: event store must declare durable production readiness", ErrProductionDependencyInvalid)
	}
	if dependency, ok := g.idempotency.(ProductionDependency); !ok || !dependency.ProductionReady() {
		return fmt.Errorf("%w: idempotency store must declare durable production readiness", ErrProductionDependencyInvalid)
	}
	if _, ok := g.idempotency.(IdempotencyResumer); !ok {
		return fmt.Errorf("%w: idempotency store must support resumable tool CAS", ErrProductionDependencyInvalid)
	}
	if dependency, ok := g.terminalCommitter.(ProductionDependency); !ok || !dependency.ProductionReady() {
		return fmt.Errorf("%w: terminal committer must declare same-database transaction readiness", ErrProductionDependencyInvalid)
	}
	if _, ok := g.schemaValidator.(BasicSchemaValidator); ok {
		return fmt.Errorf("%w: BasicSchemaValidator is forbidden", ErrProductionDependencyInvalid)
	}
	return nil
}

func (g *DefaultGateway) Invoke(ctx context.Context, req ToolCallRequest) (*ToolCallResult, error) {
	return g.invoke(ctx, req)
}

func (g *DefaultGateway) InvokeWithEvents(ctx context.Context, req ToolCallRequest, sink PersistedEventSink) (*ToolCallResult, error) {
	return g.invoke(withPersistedEventSink(ctx, sink), req)
}

func (g *DefaultGateway) invoke(ctx context.Context, req ToolCallRequest) (result *ToolCallResult, retErr error) {
	startedAt := g.clock.Now()
	if err := validateToolCallResume(req.Resume); err != nil {
		return nil, err
	}
	resuming := req.Resume != nil
	trustedTrace, ok := observability.TraceContextFrom(ctx)
	if !ok || trustedTrace.TraceID == "" {
		return nil, NewToolError(ErrorTypeTraceMissing, "trace context is required", false, nil)
	}
	if g.eventStore == nil {
		return nil, NewToolError(ErrorTypeInternal, "event store is required", false, nil)
	}
	if g.registry == nil {
		return nil, NewToolError(ErrorTypeInternal, "tool registry is required", false, nil)
	}
	var err error
	req, err = reconcileToolCallIdentity(req, trustedTrace)
	if err != nil {
		return nil, err
	}
	ctx = artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: req.TenantID, UserID: req.UserID, SessionID: req.SessionID,
		RunID: req.RunID, Role: artifact.ActorRuntime,
	})
	argumentIdentity := string(req.Arguments)
	if len(req.Arguments) == 0 && req.ArgumentsRef != "" {
		argumentIdentity = "artifact-ref:" + req.ArgumentsRef
	}
	if req.ToolCallID == "" {
		req.ToolCallID = "tc_" + hashString(req.RunID + ":" + req.ToolName + ":" + argumentIdentity)[:24]
	}
	tc := trustedTrace
	var span observability.Span
	if g.traceProvider != nil {
		ctx, span = g.traceProvider.Start(ctx, "tool.call", toolCallSpanStartFields(trustedTrace, req)...)
		defer span.End()
		tc = span.TraceContext()
		if tc.TraceID == "" {
			tc.TraceID = req.TraceID
		}
		if tc.SpanID != "" {
			req.SpanID = tc.SpanID
		}
	}
	var def *ToolDefinition
	lifecycleStarted := false
	defer func() {
		if !lifecycleStarted {
			g.observeToolCallStarted(ctx, span, req, def)
		}
		g.observeToolCallTerminal(ctx, span, startedAt, req, def, result, retErr)
	}()
	if _, ok := g.idempotency.(IdempotencyReleaser); !ok {
		return nil, NewToolError(ErrorTypeInternal, "idempotency store must support releasing an unexecuted claim", false, nil)
	}
	unresolvedArgumentsHash := hashString(argumentIdentity)
	logicalIdempotencyKey := req.Policy.IdempotencyKey
	if logicalIdempotencyKey == "" {
		logicalIdempotencyKey = req.RunID + ":" + req.ToolCallID
	}
	def, err = g.registry.Get(ctx, req.ToolName, req.ToolVersion)
	if err != nil {
		errorType, retryable := classifyExecutionError(err)
		def = &ToolDefinition{Name: req.ToolName, Version: req.ToolVersion, Type: ToolTypeFunction, Visibility: observability.VisibilityUserVisible}
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, unresolvedArgumentsHash,
			errorType, retryable, safeFailureMessage(errorType),
		)
	}
	req.ToolVersion = def.Version
	if !resuming {
		g.observeToolCallStarted(ctx, span, req, def)
	}
	lifecycleStarted = true
	if err := validateFrozenSourceBinding(req, def, g.requireFrozenSourceBinding); err != nil {
		errorType, retryable := classifyExecutionError(err)
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, unresolvedArgumentsHash,
			errorType, retryable, safeFailureMessage(errorType),
		)
	}
	resolved, err := resolveToolInput(ctx, g.artifactStore, req)
	if err != nil {
		errorType, retryable := classifyExecutionError(err)
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, unresolvedArgumentsHash,
			errorType, retryable, safeFailureMessage(errorType),
		)
	}
	req.Arguments = resolved.arguments
	req.ArgumentsRef = resolved.argumentsRef
	argumentsHash := hashString(string(req.Arguments))
	inputSchema, err := resolveToolSchema(ctx, g.artifactStore, req, def.InputSchema, def.InputSchemaRef)
	if err != nil {
		g.observeToolSchemaDecision(ctx, span, req, def, "input", "block")
		errorType, retryable := classifyExecutionError(err)
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, argumentsHash,
			errorType, retryable, safeFailureMessage(errorType),
		)
	}
	outputSchema, err := resolveToolSchema(ctx, g.artifactStore, req, def.OutputSchema, def.OutputSchemaRef)
	if err != nil {
		g.observeToolSchemaDecision(ctx, span, req, def, "output", "block")
		errorType, retryable := classifyExecutionError(err)
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, argumentsHash,
			errorType, retryable, safeFailureMessage(errorType),
		)
	}
	if coerced, changed := coerceArgumentsToSchema(inputSchema, req.Arguments); changed {
		req.Arguments = coerced
		argumentsHash = hashString(string(req.Arguments))
	}
	if err := g.schemaValidator.Validate(ctx, inputSchema, req.Arguments); err != nil {
		g.observeToolSchemaDecision(ctx, span, req, def, "input", "block")
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			errorType, retryable := classifyExecutionError(err)
			return g.recordPreExecutionFailure(
				ctx, startedAt, tc, req, def, logicalIdempotencyKey, argumentsHash,
				errorType, retryable, safeFailureMessage(errorType),
			)
		}
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, argumentsHash,
			ErrorTypeSchemaValidationFailed, true, schemaValidationRepairMessage(inputSchema, req.Arguments, err),
		)
	}
	g.observeToolSchemaDecision(ctx, span, req, def, "input", "allow")
	decision, err := g.policyEngine.EvaluateToolCall(ctx, def, req)
	if err != nil {
		g.observeToolPermissionDecision(ctx, span, req, def, "error")
		return nil, err
	}
	permissionDecision := DecisionBlock
	if decision != nil {
		permissionDecision = decision.Decision
	}
	g.observeToolPermissionDecision(ctx, span, req, def, string(permissionDecision))
	if decision == nil || decision.Decision != DecisionAllow {
		errorType := ErrorTypePermissionDenied
		message := "工具调用未通过权限检查。"
		if decision != nil {
			if decision.ReasonCode == string(ErrorTypeControlRequired) {
				errorType = ErrorTypeControlRequired
			}
			if decision.SafeMessage != "" {
				message = decision.SafeMessage
			}
		}
		return g.recordPreExecutionFailure(
			ctx, startedAt, tc, req, def, logicalIdempotencyKey, argumentsHash,
			errorType, false, message,
		)
	}
	definitionHash := stableHash(def)
	reservation, existing, found, err := g.claimExecutionIdempotency(
		ctx, req, logicalIdempotencyKey, argumentsHash, definitionHash,
	)
	if err != nil {
		return nil, err
	}
	if found {
		if existing.ToolCallID != req.ToolCallID || existing.ToolName != req.ToolName || existing.ToolVersion != req.ToolVersion ||
			existing.ArgumentsHash != argumentsHash || existing.DefinitionHash != definitionHash ||
			existing.LogicalKeyHash != hashString(logicalIdempotencyKey) {
			return nil, NewToolError(ErrorTypeIdempotencyConflict, "idempotency key belongs to a different tool call", false, nil)
		}
		if existing.Result != nil {
			return cloneToolCallResult(existing.Result), nil
		}
		if existing.Failure != nil {
			return failureResult(req, existing.Failure, ToolUsage{}), nil
		}
		if resuming && existing.Status == ToolCallSuspended {
			if err := g.resumeExecutionClaims(ctx, reservation.keys(), req.ToolCallID); err != nil {
				return nil, err
			}
		} else {
			return nil, NewToolError(ErrorTypeDuplicateInflight, "same tool call is already running or waiting for control", true, nil)
		}
	}
	ctx = withToolTerminalBinding(ctx, newToolTerminalBinding(
		reservation.keys(), req, logicalIdempotencyKey, argumentsHash, definitionHash,
	))
	var argumentArtifact *artifact.ArtifactMeta
	if resolved.argumentsRef == "" {
		argumentArtifact, err = g.offloadLargeInlineArguments(ctx, req, def.ResultPolicy.MaxInlineBytes)
		if err != nil {
			result, persistErr := g.failFromError(ctx, startedAt, tc, req, def, err, false)
			if persistErr != nil {
				return nil, persistErr
			}
			g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		if argumentArtifact != nil {
			req.ArgumentsRef = argumentArtifact.ArtifactRef
		}
	}
	if g.stepStore != nil && !resuming {
		if _, err := g.stepStore.StartToolStep(ctx, StartToolStepRequest{
			TenantID:     req.TenantID,
			SessionID:    req.SessionID,
			RunID:        req.RunID,
			StepID:       req.StepID,
			ParentStepID: req.ParentStepID,
			AgentID:      req.AgentID,
			ToolCallID:   req.ToolCallID,
			ToolName:     req.ToolName,
			Trace:        tc,
		}); err != nil {
			releaseErr := g.releaseUnexecutedClaims(ctx, reservation.keys(), req.ToolCallID)
			return nil, errors.Join(err, releaseErr)
		}
	}
	events := make([]observability.AgentEvent, 0, 2)
	if !resuming {
		startedEvent := buildToolStartedToolEvent(g.ids, g.clock.Now(), tc, req, def)
		persistedStarted, err := g.appendToolEvent(ctx, startedEvent)
		if err != nil {
			g.failPreExecutionStep(ctx, req)
			releaseErr := g.releaseUnexecutedClaims(ctx, reservation.keys(), req.ToolCallID)
			return nil, errors.Join(err, releaseErr)
		}
		events = append(events, persistedStarted)
	}
	var toolCtx *defaultToolContext
	finalizeResult := func(result *ToolCallResult) *ToolCallResult {
		if result == nil {
			return nil
		}
		var processEvents []observability.AgentEvent
		if toolCtx != nil {
			processEvents = toolCtx.persistedEvents()
		}
		result.Events = mergePersistedToolEvents(events, processEvents, result.Events)
		return result
	}
	if argumentArtifact != nil {
		artifactEvent := buildToolArgumentArtifactCreatedToolEvent(g.ids, g.clock.Now(), tc, req, def, argumentArtifact)
		persistedArtifact, err := g.appendToolEvent(ctx, artifactEvent)
		if err != nil {
			result, persistErr := g.failRequiredArtifactEvent(ctx, startedAt, tc, req, def, err, false, 0)
			if persistErr != nil {
				return nil, persistErr
			}
			result = finalizeResult(result)
			g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		events = append(events, persistedArtifact)
	}
	executor, ok := g.executors[def.Type]
	if !ok {
		result, persistErr := g.fail(ctx, startedAt, tc, req, def, ErrorTypeToolNotFound, false, false, "工具执行器未注册。")
		if persistErr != nil {
			return nil, persistErr
		}
		result = finalizeResult(result)
		g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
		return result, nil
	}
	execCtx := ctx
	cancel := func() {}
	timeout := req.Policy.Timeout
	if timeout <= 0 {
		timeout = def.Timeout
	}
	if timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	toolCtx = newDefaultToolContext(execCtx, tc, req, def, g.toolEventSink, g.logger)
	var raw *ToolRawResult
	var execErr error
	retryCount := 0
	maxAttempts := 1
	definitionAttempts := def.Retry.MaxAttempts
	if definitionAttempts < 1 {
		definitionAttempts = 1
	}
	requestRetries := req.Policy.MaxRetries
	if requestRetries < 0 {
		requestRetries = 0
	}
	if def.Retry.Idempotent && requestRetries > 0 && definitionAttempts > 1 {
		allowedRetries := requestRetries
		if definitionAttempts-1 < allowedRetries {
			allowedRetries = definitionAttempts - 1
		}
		maxAttempts += allowedRetries
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if contextual, ok := executor.(ContextAwareToolExecutor); ok {
			raw, execErr = contextual.ExecuteWithContext(execCtx, def, req, toolCtx)
		} else {
			raw, execErr = executor.Execute(execCtx, def, req)
		}
		if IsToolInterrupted(execErr) {
			if suspendErr := g.suspendExecutionClaims(ctx, reservation.keys(), req.ToolCallID); suspendErr != nil {
				execErr = NewToolError(ErrorTypeInternal, "failed to persist suspended tool call", false, suspendErr)
			} else {
				return nil, execErr
			}
		}
		if execErr == nil {
			if contextErr := execCtx.Err(); contextErr == nil {
				break
			} else {
				execErr = contextErr
			}
		}
		errorType, retryable := classifyExecutionError(execErr)
		contextDone := false
		if contextErr := execCtx.Err(); contextErr != nil {
			contextDone = true
			execErr = contextErr
			errorType, retryable = classifyExecutionError(contextErr)
		}
		if contextDone || !def.Retry.Idempotent || !retryable || attempt == maxAttempts {
			if raw != nil && raw.Partial {
				break
			}
			result, persistErr := g.failWithRetryCount(ctx, startedAt, tc, req, def, errorType, true, retryable, safeFailureMessage(errorType), retryCount)
			if persistErr != nil {
				return nil, persistErr
			}
			result = finalizeResult(result)
			g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		g.observeToolCallRetry(ctx, span, req, def, retryCount+1, errorType)
		retryCount++
	}
	if raw != nil && raw.Partial {
		partialCtx, cancelPartialPersistence := terminalPersistenceContext(ctx)
		defer cancelPartialPersistence()
		normalized, normalizeErr := normalizeResult(partialCtx, g.artifactStore, req, raw, def.ResultPolicy)
		if normalizeErr != nil && normalized.resultRef == "" {
			errorType := ErrorTypeNormalizationFailed
			if IsErrorType(normalizeErr, ErrorTypeArtifactError) {
				errorType = ErrorTypeArtifactError
			}
			result, persistErr := g.failWithRetryCount(ctx, startedAt, tc, req, def, errorType, true, false, "工具部分结果归一化失败。", retryCount)
			if persistErr != nil {
				return nil, persistErr
			}
			result = finalizeResult(result)
			g.storeIdempotency(partialCtx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		partialResultEventPersisted := false
		persistedArtifacts := make([]normalizedArtifact, 0, len(normalized.artifacts))
		for _, item := range normalized.artifacts {
			artifactEvent := buildToolArtifactCreatedToolEvent(g.ids, g.clock.Now(), tc, req, def, item)
			persistedArtifact, appendErr := g.appendToolEvent(partialCtx, artifactEvent)
			if appendErr != nil {
				if partialResultEventPersisted {
					failureNormalized := normalized
					failureNormalized.artifacts = append([]normalizedArtifact(nil), persistedArtifacts...)
					if item.ref == normalized.debugRef {
						failureNormalized.debugRef = ""
					}
					result, terminalErr := g.failWithRetryCountAndPartialResult(
						ctx,
						startedAt,
						tc,
						req,
						def,
						ErrorTypeInternal,
						false,
						safeFailureMessage(ErrorTypeInternal),
						retryCount,
						failureNormalized,
					)
					if terminalErr != nil {
						return nil, errors.Join(appendErr, terminalErr)
					}
					result = finalizeResult(result)
					g.storeIdempotency(partialCtx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
					return result, nil
				}
				result, persistErr := g.failRequiredArtifactEvent(ctx, startedAt, tc, req, def, appendErr, true, retryCount)
				if persistErr != nil {
					return nil, persistErr
				}
				result = finalizeResult(result)
				g.storeIdempotency(partialCtx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
				return result, nil
			}
			events = append(events, persistedArtifact)
			persistedArtifacts = append(persistedArtifacts, item)
			if item.ref == normalized.resultRef {
				partialResultEventPersisted = true
			}
		}
		if normalizeErr != nil {
			errorType := ErrorTypeNormalizationFailed
			if IsErrorType(normalizeErr, ErrorTypeArtifactError) {
				errorType = ErrorTypeArtifactError
			}
			result, persistErr := g.failWithRetryCountAndPartialResult(
				ctx, startedAt, tc, req, def, errorType, false, "工具部分结果归一化失败。", retryCount, normalized,
			)
			if persistErr != nil {
				return nil, persistErr
			}
			result = finalizeResult(result)
			g.storeIdempotency(partialCtx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		errorType := ErrorTypeNormalizationFailed
		retryable := false
		safeMessage := "工具返回了不完整结果。"
		if execErr != nil {
			errorType, retryable = classifyExecutionError(execErr)
			safeMessage = safeFailureMessage(errorType)
		}
		result, persistErr := g.failWithRetryCountAndPartialResult(
			ctx, startedAt, tc, req, def, errorType, retryable, safeMessage, retryCount, normalized,
		)
		if persistErr != nil {
			return nil, persistErr
		}
		result = finalizeResult(result)
		g.storeIdempotency(partialCtx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
		return result, nil
	}
	if execErr != nil {
		return nil, execErr
	}
	if len(outputSchema) > 0 {
		data := rawResultData(raw)
		if err := g.schemaValidator.Validate(ctx, outputSchema, data); err != nil {
			g.observeToolSchemaDecision(ctx, span, req, def, "output", "block")
			result, persistErr := g.failWithRetryCount(ctx, startedAt, tc, req, def, ErrorTypeSchemaValidationFailed, true, false, "工具输出不符合 schema。", retryCount)
			if persistErr != nil {
				return nil, persistErr
			}
			result = finalizeResult(result)
			g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		g.observeToolSchemaDecision(ctx, span, req, def, "output", "allow")
	}
	normalized, err := normalizeResult(ctx, g.artifactStore, req, raw, def.ResultPolicy)
	if err != nil {
		errorType := ErrorTypeNormalizationFailed
		if IsErrorType(err, ErrorTypeArtifactError) {
			errorType = ErrorTypeArtifactError
		}
		result, persistErr := g.failWithRetryCount(ctx, startedAt, tc, req, def, errorType, true, false, "工具结果归一化失败。", retryCount)
		if persistErr != nil {
			return nil, persistErr
		}
		result = finalizeResult(result)
		g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
		return result, nil
	}
	duration := g.clock.Now().Sub(startedAt)
	for _, item := range normalized.artifacts {
		artifactEvent := buildToolArtifactCreatedToolEvent(g.ids, g.clock.Now(), tc, req, def, item)
		persistedArtifact, err := g.appendToolEvent(ctx, artifactEvent)
		if err != nil {
			result, persistErr := g.failRequiredArtifactEvent(ctx, startedAt, tc, req, def, err, true, retryCount)
			if persistErr != nil {
				return nil, persistErr
			}
			result = finalizeResult(result)
			g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
			return result, nil
		}
		events = append(events, persistedArtifact)
	}
	completedEvent := buildToolCompletedToolEvent(g.ids, g.clock.Now(), tc, req, def, normalized, duration, retryCount)
	persistedCompleted, err := g.appendToolEvent(ctx, completedEvent)
	if err != nil {
		return nil, err
	}
	events = append(events, persistedCompleted)
	if g.stepStore != nil {
		if err := g.stepStore.CompleteToolStep(ctx, CompleteToolStepRequest{
			RunID:       req.RunID,
			StepID:      req.StepID,
			ToolCallID:  req.ToolCallID,
			ResultRef:   normalized.resultRef,
			CompletedAt: g.clock.Now(),
		}); err != nil {
			g.observeToolProjectionDegradation(ctx, req, def, ToolCallSucceeded, "step_store", "complete_tool_step")
		}
	}
	result = &ToolCallResult{
		ToolCallID:         req.ToolCallID,
		ToolName:           req.ToolName,
		ToolVersion:        req.ToolVersion,
		Status:             ToolCallSucceeded,
		ResultPreview:      normalized.preview,
		ResultRef:          normalized.resultRef,
		ModelContextResult: normalized.modelContextResult,
		DebugRef:           normalized.debugRef,
		Usage: ToolUsage{
			Duration:     duration,
			InputBytes:   int64(len(req.Arguments)),
			OutputBytes:  normalized.outputBytes,
			RetryCount:   retryCount,
			ArtifactRefs: normalizedArtifactRefs(normalized.artifacts),
		},
		Events: events,
	}
	result = finalizeResult(result)
	g.storeIdempotency(ctx, reservation.keys(), req, def, logicalIdempotencyKey, argumentsHash, definitionHash, result)
	return result, nil
}

func toolCallSpanStartFields(tc observability.TraceContext, req ToolCallRequest) []observability.Field {
	fields := []observability.Field{
		observability.String("span_type", "tool.call"),
		observability.String("trace_id", tc.TraceID),
		observability.String("session_id", req.SessionID),
		observability.String("run_id", req.RunID),
		observability.String("step_id", req.StepID),
		observability.String("agent_id", req.AgentID),
		observability.String("tool_call_id", req.ToolCallID),
		observability.String("tool_name", req.ToolName),
		observability.String("tool_version", req.ToolVersion),
		observability.String("risk_level", string(req.Policy.RiskLevel)),
	}
	if tc.SpanID != "" {
		fields = append(fields, observability.String("parent_span_id", tc.SpanID))
	}
	return fields
}

func toolLifecycleIdentityFields(ctx context.Context, req ToolCallRequest, def *ToolDefinition) []observability.Field {
	traceID := req.TraceID
	spanID := req.SpanID
	parentSpanID := ""
	if tc, ok := observability.TraceContextFrom(ctx); ok {
		if tc.TraceID != "" {
			traceID = tc.TraceID
		}
		if tc.SpanID != "" {
			spanID = tc.SpanID
		}
		parentSpanID = tc.ParentSpanID
	}
	toolVersion := req.ToolVersion
	toolType := ToolType("")
	riskLevel := req.Policy.RiskLevel
	if def != nil {
		if def.Version != "" {
			toolVersion = def.Version
		}
		toolType = def.Type
		if def.RiskLevel != "" {
			riskLevel = def.RiskLevel
		}
	}
	fields := []observability.Field{
		observability.String("trace_id", traceID),
		observability.String("span_id", spanID),
		observability.String("session_id", req.SessionID),
		observability.String("run_id", req.RunID),
		observability.String("step_id", req.StepID),
		observability.String("agent_id", req.AgentID),
		observability.String("tool_call_id", req.ToolCallID),
		observability.String("tool_name", req.ToolName),
		observability.String("tool_version", toolVersion),
		observability.String("tool_type", string(toolType)),
		observability.String("risk_level", string(riskLevel)),
	}
	if parentSpanID != "" {
		fields = append(fields, observability.String("parent_span_id", parentSpanID))
	}
	return fields
}

func (g *DefaultGateway) observeToolCallStarted(ctx context.Context, span observability.Span, req ToolCallRequest, def *ToolDefinition) {
	fields := toolLifecycleIdentityFields(ctx, req, def)
	g.logger.Info(ctx, "tool call started", fields...)
	if span != nil {
		span.AddEvent("tool.call.started", fields...)
	}
}

func (g *DefaultGateway) observeToolSchemaDecision(ctx context.Context, span observability.Span, req ToolCallRequest, def *ToolDefinition, phase, decision string) {
	fields := append(toolLifecycleIdentityFields(ctx, req, def),
		observability.String("schema_phase", phase),
		observability.String("schema_decision", decision),
	)
	g.logger.Info(ctx, "tool schema decision", fields...)
	if span != nil {
		span.AddEvent("tool.schema", fields...)
	}
}

func (g *DefaultGateway) observeToolPermissionDecision(ctx context.Context, span observability.Span, req ToolCallRequest, def *ToolDefinition, decision string) {
	fields := append(toolLifecycleIdentityFields(ctx, req, def), observability.String("permission_decision", decision))
	g.logger.Info(ctx, "tool permission decision", fields...)
	if span != nil {
		span.AddEvent("tool.permission", fields...)
	}
}

func (g *DefaultGateway) observeToolCallRetry(ctx context.Context, span observability.Span, req ToolCallRequest, def *ToolDefinition, retryCount int, errorType ErrorType) {
	fields := append(toolLifecycleIdentityFields(ctx, req, def),
		observability.Int("retry_count", retryCount),
		observability.String("error_type", string(errorType)),
	)
	g.logger.Info(ctx, "tool call retry", fields...)
	if span != nil {
		span.AddEvent("tool.call.retry", fields...)
	}
}

func (g *DefaultGateway) observeToolCallTerminal(
	ctx context.Context,
	span observability.Span,
	startedAt time.Time,
	req ToolCallRequest,
	def *ToolDefinition,
	result *ToolCallResult,
	invokeErr error,
) {
	if IsToolInterrupted(invokeErr) {
		fields := append(toolLifecycleIdentityFields(ctx, req, def),
			observability.String("status", string(ToolCallSuspended)),
			observability.Int64("duration_ms", g.clock.Now().Sub(startedAt).Milliseconds()),
		)
		g.logger.Info(ctx, "tool call suspended", fields...)
		if span != nil {
			span.AddEvent("tool.call.suspended", fields...)
		}
		return
	}
	status := ToolCallFailed
	success := false
	errorType := ErrorTypeInternal
	retryable := false
	retryCount := 0
	artifactCount := 0
	artifactRef := ""
	if result != nil {
		status = result.Status
		success = invokeErr == nil && result.Status == ToolCallSucceeded
		retryCount = result.Usage.RetryCount
		artifactCount = len(result.Usage.ArtifactRefs)
		artifactRef = result.ResultRef
		if artifactRef == "" && result.Failure != nil {
			artifactRef = result.Failure.PartialResultRef
		}
		if artifactRef == "" && len(result.Usage.ArtifactRefs) > 0 {
			artifactRef = result.Usage.ArtifactRefs[0]
		}
		if artifactRef == "" {
			artifactRef = result.DebugRef
		}
		if result.Failure != nil {
			errorType = ErrorType(result.Failure.ErrorType)
			retryable = result.Failure.Retryable
		}
	}
	if invokeErr != nil {
		errorType, retryable = classifyExecutionError(invokeErr)
	}
	if success {
		errorType = ""
	}
	fields := append(toolLifecycleIdentityFields(ctx, req, def),
		observability.String("status", string(status)),
		observability.Bool("success", success),
		observability.Int64("duration_ms", g.clock.Now().Sub(startedAt).Milliseconds()),
		observability.Int("retry_count", retryCount),
		observability.Int("artifact_count", artifactCount),
		observability.String("artifact_ref", artifactRef),
		observability.String("error_type", string(errorType)),
	)
	if success {
		g.logger.Info(ctx, "tool call completed", fields...)
		if span != nil {
			span.AddEvent("tool.call.completed", fields...)
		}
		return
	}
	safeErr := NewToolError(errorType, safeFailureMessage(errorType), retryable, nil)
	g.logger.Error(ctx, "tool call failed", safeErr, fields...)
	if span != nil {
		span.AddEvent("tool.call.failed", fields...)
		span.RecordError(safeErr, fields...)
	}
}

func (g *DefaultGateway) observeToolProjectionDegradation(
	ctx context.Context,
	req ToolCallRequest,
	def *ToolDefinition,
	status ToolCallStatus,
	projection string,
	operation string,
) {
	fields := append(toolLifecycleIdentityFields(ctx, req, def),
		observability.String("fallback_type", "secondary_projection_degraded"),
		observability.String("from", projection),
		observability.String("to", "event_store_canonical_terminal"),
		observability.String("reason", "projection_write_failed"),
		observability.String("impact", "canonical_terminal_preserved_reconciliation_required"),
		observability.String("projection", projection),
		observability.String("operation", operation),
		observability.String("status", string(status)),
	)
	g.logger.Warn(ctx, "tool secondary projection degraded", fields...)
}

func (g *DefaultGateway) appendEvent(ctx context.Context, event observability.AgentEvent) (observability.AgentEvent, error) {
	if g.eventStore == nil {
		return observability.AgentEvent{}, NewToolError(ErrorTypeInternal, "event store is required", false, nil)
	}
	var result *EventAppendResult
	var err error
	terminalCommit := false
	if binding, ok := toolTerminalBindingFrom(ctx); ok && isTerminalToolEvent(event.EventType) && g.terminalCommitter != nil {
		terminalCommit = true
		result, err = g.terminalCommitter.CommitTerminal(ctx, event, binding)
	} else {
		result, err = g.eventStore.AppendEvent(ctx, event)
	}
	if err != nil {
		if terminalCommit {
			g.logger.Error(ctx, "tool terminal commit failed", err,
				observability.String("event_type", string(event.EventType)),
				observability.String("run_id", event.RunID),
				observability.String("step_id", event.StepID),
				observability.String("agent_id", event.AgentID),
			)
		}
		return observability.AgentEvent{}, err
	}
	if result == nil {
		return observability.AgentEvent{}, NewToolError(ErrorTypeInternal, "event store returned no persisted event", false, nil)
	}
	if err := validatePersistedEvent(event, result.Event); err != nil {
		return observability.AgentEvent{}, err
	}
	if err := emitPersistedEvent(ctx, result.Event); err != nil {
		return observability.AgentEvent{}, err
	}
	return result.Event, nil
}

func validatePersistedEvent(submitted, persisted observability.AgentEvent) error {
	invalid := func() error {
		return NewToolError(ErrorTypeInternal, "event store returned invalid persisted event", false, nil)
	}
	switch {
	case persisted.SchemaVersion != observability.AgentEventSchemaVersion:
		return invalid()
	case persisted.Sequence <= 0:
		return invalid()
	case persisted.CreatedAt.IsZero():
		return invalid()
	case persisted.EventID == "":
		return invalid()
	case persisted.TraceID == "":
		return invalid()
	case persisted.RunID == "" || persisted.RunID != submitted.RunID:
		return invalid()
	case persisted.SessionID == "" || persisted.SessionID != submitted.SessionID:
		return invalid()
	case persisted.StepID != submitted.StepID:
		return invalid()
	case persisted.AgentID != submitted.AgentID:
		return invalid()
	case !isCanonicalEventType(persisted.EventType) || persisted.EventType != submitted.EventType:
		return invalid()
	case persisted.IdempotencyKey == "" || persisted.IdempotencyKey != submitted.IdempotencyKey:
		return invalid()
	case !isCanonicalEventVisibility(persisted.Visibility) || persisted.Visibility != submitted.Visibility:
		return invalid()
	case persisted.PayloadRef != submitted.PayloadRef:
		return invalid()
	case persisted.DebugRef != submitted.DebugRef:
		return invalid()
	default:
		return nil
	}
}

func normalizedArtifactRefs(items []normalizedArtifact) []string {
	if len(items) == 0 {
		return nil
	}
	refs := make([]string, 0, len(items))
	for _, item := range items {
		refs = append(refs, item.ref)
	}
	return refs
}

func (g *DefaultGateway) appendToolEvent(ctx context.Context, event ToolEvent) (observability.AgentEvent, error) {
	normalized, err := NormalizeToolEvent(g.ids, event)
	if err != nil {
		return observability.AgentEvent{}, err
	}
	return g.appendEvent(ctx, normalized)
}

func mergePersistedToolEvents(groups ...[]observability.AgentEvent) []observability.AgentEvent {
	type sequenceKey struct {
		runID    string
		sequence int64
	}
	seen := make(map[sequenceKey]struct{})
	merged := make([]observability.AgentEvent, 0)
	for _, group := range groups {
		for _, event := range group {
			if event.Sequence > 0 {
				key := sequenceKey{runID: event.RunID, sequence: event.Sequence}
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
			}
			merged = append(merged, cloneAgentEvent(event))
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].Sequence < merged[j].Sequence
	})
	return merged
}

type idempotencyReservation struct {
	callKey    string
	logicalKey string
}

func (r idempotencyReservation) keys() []string {
	return []string{r.callKey, r.logicalKey}
}

func (g *DefaultGateway) claimExecutionIdempotency(
	ctx context.Context,
	req ToolCallRequest,
	logicalKey string,
	argumentsHash string,
	definitionHash string,
) (idempotencyReservation, *IdempotencyRecord, bool, error) {
	reservation := idempotencyReservation{
		callKey:    scopedToolCallIdempotencyStoreKey(req),
		logicalKey: scopedIdempotencyStoreKey(req, logicalKey),
	}
	record := IdempotencyRecord{
		Key:            reservation.callKey,
		ToolCallID:     req.ToolCallID,
		ToolName:       req.ToolName,
		ToolVersion:    req.ToolVersion,
		ArgumentsHash:  argumentsHash,
		DefinitionHash: definitionHash,
		LogicalKeyHash: hashString(logicalKey),
		Status:         ToolCallRunning,
		CreatedAt:      g.clock.Now(),
	}
	existing, found, err := g.claimIdempotency(ctx, record)
	if err != nil || found {
		return reservation, existing, found, err
	}

	record.Key = reservation.logicalKey
	existing, found, err = g.claimIdempotency(ctx, record)
	if err != nil || found {
		releaseErr := g.releaseUnexecutedClaims(ctx, []string{reservation.callKey}, req.ToolCallID)
		if err != nil {
			return idempotencyReservation{}, nil, false, errors.Join(err, releaseErr)
		}
		if releaseErr != nil {
			return idempotencyReservation{}, nil, false, releaseErr
		}
		return idempotencyReservation{}, existing, true, nil
	}
	return reservation, nil, false, nil
}

func (g *DefaultGateway) suspendExecutionClaims(ctx context.Context, keys []string, toolCallID string) error {
	resumer, ok := g.idempotency.(IdempotencyResumer)
	if !ok {
		return NewToolError(ErrorTypeInternal, "idempotency store cannot suspend a tool call", false, nil)
	}
	return resumer.Suspend(ctx, keys, toolCallID)
}

func (g *DefaultGateway) resumeExecutionClaims(ctx context.Context, keys []string, toolCallID string) error {
	resumer, ok := g.idempotency.(IdempotencyResumer)
	if !ok {
		return NewToolError(ErrorTypeInternal, "idempotency store cannot resume a suspended tool call", false, nil)
	}
	won, err := resumer.Resume(ctx, keys, toolCallID)
	if err != nil {
		return err
	}
	if !won {
		return NewToolError(ErrorTypeDuplicateInflight, "tool call resume is already running", true, nil)
	}
	return nil
}

func (g *DefaultGateway) claimIdempotency(ctx context.Context, rec IdempotencyRecord) (*IdempotencyRecord, bool, error) {
	if claimer, ok := g.idempotency.(IdempotencyClaimer); ok {
		claimed, inserted, err := claimer.Claim(ctx, rec)
		if err != nil {
			return nil, false, err
		}
		return claimed, !inserted, nil
	}
	fallbackIdempotencyClaimMu.Lock()
	defer fallbackIdempotencyClaimMu.Unlock()
	existing, found, err := g.idempotency.Get(ctx, rec.Key)
	if err != nil || found {
		return existing, found, err
	}
	if err := g.idempotency.Put(ctx, rec); err != nil {
		return nil, false, err
	}
	return &rec, false, nil
}

func (g *DefaultGateway) releaseUnexecutedClaims(ctx context.Context, keys []string, toolCallID string) error {
	releaser, ok := g.idempotency.(IdempotencyReleaser)
	if !ok {
		return NewToolError(ErrorTypeInternal, "idempotency store cannot release an unexecuted claim", false, nil)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		if keys[i] == "" {
			continue
		}
		if err := releaser.Release(ctx, keys[i], toolCallID); err != nil {
			return err
		}
	}
	return nil
}

func (g *DefaultGateway) offloadLargeInlineArguments(
	ctx context.Context,
	req ToolCallRequest,
	maxInlineBytes int,
) (*artifact.ArtifactMeta, error) {
	if maxInlineBytes <= 0 {
		maxInlineBytes = 4096
	}
	if len(req.Arguments) <= maxInlineBytes {
		return nil, nil
	}
	if g.artifactStore == nil {
		return nil, NewToolError(ErrorTypeArtifactError, "artifact store is required for large tool arguments", false, nil)
	}
	meta, err := g.artifactStore.Put(ctx, artifact.PutArtifactRequest{
		TenantID:        req.TenantID,
		UserID:          req.UserID,
		SessionID:       req.SessionID,
		RunID:           req.RunID,
		StepID:          req.StepID,
		OwnerModule:     artifact.OwnerModuleToolGateway,
		OwnerID:         req.ToolCallID,
		ArtifactType:    artifact.ArtifactTypeDebugPayload,
		MimeType:        "application/json",
		Name:            req.ToolName + "-arguments.json",
		Visibility:      artifact.VisibilityDebug,
		Content:         bytes.NewReader(req.Arguments),
		RetentionPolicy: artifact.RetentionDebugShortTTL,
		CreatedBy:       "tool:" + req.ToolName,
		IdempotencyKey:  req.RunID + ":" + req.ToolCallID + ":tool-arguments",
		Metadata: map[string]string{
			"tool_name":    req.ToolName,
			"payload_kind": "arguments",
		},
	})
	if err != nil {
		return nil, NewToolError(ErrorTypeArtifactError, "write tool arguments artifact", false, err)
	}
	if meta == nil || meta.ArtifactRef == "" {
		return nil, NewToolError(ErrorTypeArtifactError, "tool arguments artifact has no reference", false, nil)
	}
	return meta, nil
}

func (g *DefaultGateway) failPreExecutionStep(ctx context.Context, req ToolCallRequest) {
	if g.stepStore == nil {
		return
	}
	_ = g.stepStore.FailToolStep(ctx, FailToolStepRequest{
		RunID:      req.RunID,
		StepID:     req.StepID,
		ToolCallID: req.ToolCallID,
		ErrorType:  string(ErrorTypeInternal),
		FailedAt:   g.clock.Now(),
	})
}

func fillRequestFromTrace(req ToolCallRequest, tc observability.TraceContext) ToolCallRequest {
	if req.TraceID == "" {
		req.TraceID = tc.TraceID
	}
	if req.SpanID == "" {
		req.SpanID = tc.SpanID
	}
	if req.TenantID == "" {
		req.TenantID = tc.TenantID
	}
	if req.UserID == "" {
		req.UserID = tc.UserID
	}
	if req.SessionID == "" {
		req.SessionID = tc.SessionID
	}
	if req.RunID == "" {
		req.RunID = tc.RunID
	}
	if req.AgentID == "" {
		req.AgentID = tc.AgentID
	}
	return req
}

func reconcileToolCallIdentity(req ToolCallRequest, tc observability.TraceContext) (ToolCallRequest, error) {
	mismatch := func(field, requestValue, trustedValue string) error {
		if requestValue != "" && trustedValue != "" && requestValue != trustedValue {
			return NewToolError(ErrorTypePermissionDenied, "tool call "+field+" does not match trusted context", false, nil)
		}
		return nil
	}
	for _, identity := range []struct {
		field   string
		request string
		trusted string
	}{
		{field: "trace id", request: req.TraceID, trusted: tc.TraceID},
		{field: "tenant id", request: req.TenantID, trusted: tc.TenantID},
		{field: "user id", request: req.UserID, trusted: tc.UserID},
		{field: "session id", request: req.SessionID, trusted: tc.SessionID},
		{field: "run id", request: req.RunID, trusted: tc.RunID},
		{field: "agent id", request: req.AgentID, trusted: tc.AgentID},
	} {
		if err := mismatch(identity.field, identity.request, identity.trusted); err != nil {
			return ToolCallRequest{}, err
		}
	}
	req = fillRequestFromTrace(req, tc)
	if req.Caller.AgentID != "" && req.Caller.AgentID != req.AgentID {
		return ToolCallRequest{}, NewToolError(ErrorTypePermissionDenied, "tool caller agent does not match effective agent", false, nil)
	}
	if req.TraceID == "" {
		return ToolCallRequest{}, NewToolError(ErrorTypeTraceMissing, "trace id is required", false, nil)
	}
	for field, value := range map[string]string{
		"tenant id":  req.TenantID,
		"session id": req.SessionID,
		"run id":     req.RunID,
		"step id":    req.StepID,
		"agent id":   req.AgentID,
		"tool name":  req.ToolName,
	} {
		if strings.TrimSpace(value) == "" {
			return ToolCallRequest{}, NewToolError(ErrorTypeInternal, "tool call "+field+" is required", false, nil)
		}
	}
	return req, nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func scopedIdempotencyStoreKey(req ToolCallRequest, logicalKey string) string {
	return "tool_idem_" + stableHash(struct {
		Namespace  string `json:"namespace"`
		TenantID   string `json:"tenant_id"`
		UserID     string `json:"user_id"`
		AgentID    string `json:"agent_id"`
		LogicalKey string `json:"logical_key"`
	}{
		Namespace: "logical", TenantID: req.TenantID, UserID: req.UserID, AgentID: req.AgentID, LogicalKey: logicalKey,
	})
}

func scopedToolCallIdempotencyStoreKey(req ToolCallRequest) string {
	return "tool_call_idem_" + stableHash(struct {
		Namespace  string `json:"namespace"`
		TenantID   string `json:"tenant_id"`
		UserID     string `json:"user_id"`
		AgentID    string `json:"agent_id"`
		RunID      string `json:"run_id"`
		ToolCallID string `json:"tool_call_id"`
	}{
		Namespace: "tool_call", TenantID: req.TenantID, UserID: req.UserID, AgentID: req.AgentID,
		RunID: req.RunID, ToolCallID: req.ToolCallID,
	})
}

func defaultPolicyEngine(config GatewayConfig) PolicyEngine {
	if config.PolicyEngine != nil {
		return config.PolicyEngine
	}
	return DefaultPolicyEngine{}
}

func (g *DefaultGateway) recordPreExecutionFailure(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	logicalKey string,
	argumentsHash string,
	errorType ErrorType,
	retryable bool,
	safeMessage string,
) (*ToolCallResult, error) {
	// A failed pre-execution check is already a canonical terminal for this
	// ToolCallID. Reserve only that call identity; the logical cache key remains
	// untouched until all checks allow execution.
	key := scopedToolCallIdempotencyStoreKey(req)
	definitionHash := stableHash(def)
	existing, found, err := g.claimIdempotency(ctx, IdempotencyRecord{
		Key:            key,
		ToolCallID:     req.ToolCallID,
		ToolName:       req.ToolName,
		ToolVersion:    req.ToolVersion,
		ArgumentsHash:  argumentsHash,
		DefinitionHash: definitionHash,
		LogicalKeyHash: hashString(logicalKey),
		Status:         ToolCallRunning,
		CreatedAt:      g.clock.Now(),
	})
	if err != nil {
		return nil, err
	}
	if found && existing.ToolCallID == req.ToolCallID {
		return nil, NewToolError(errorType, safeMessage, retryable, nil)
	}
	ctx = withToolTerminalBinding(ctx, newToolTerminalBinding(
		[]string{key}, req, logicalKey, argumentsHash, definitionHash,
	))
	result, err := g.fail(ctx, startedAt, tc, req, def, errorType, false, retryable, safeMessage)
	if err != nil {
		return nil, err
	}
	if !found {
		g.storeIdempotency(ctx, []string{key}, req, def, logicalKey, argumentsHash, definitionHash, result)
	}
	return result, nil
}

func (g *DefaultGateway) failFromError(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	err error,
	executed bool,
) (*ToolCallResult, error) {
	errorType, retryable := classifyExecutionError(err)
	return g.fail(ctx, startedAt, tc, req, def, errorType, executed, retryable, safeFailureMessage(errorType))
}

func (g *DefaultGateway) failRequiredArtifactEvent(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	appendErr error,
	executed bool,
	retryCount int,
) (*ToolCallResult, error) {
	result, terminalErr := g.failWithRetryCount(
		ctx,
		startedAt,
		tc,
		req,
		def,
		ErrorTypeInternal,
		executed,
		false,
		safeFailureMessage(ErrorTypeInternal),
		retryCount,
	)
	if terminalErr != nil {
		return nil, errors.Join(appendErr, terminalErr)
	}
	return result, nil
}

func (g *DefaultGateway) fail(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	errorType ErrorType,
	executed bool,
	retryable bool,
	safeMessage string,
) (*ToolCallResult, error) {
	return g.failWithRetryCount(ctx, startedAt, tc, req, def, errorType, executed, retryable, safeMessage, 0)
}

func (g *DefaultGateway) failWithRetryCount(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	errorType ErrorType,
	executed bool,
	retryable bool,
	safeMessage string,
	retryCount int,
) (*ToolCallResult, error) {
	return g.persistFailure(
		ctx, startedAt, tc, req, def, errorType, executed, retryable, safeMessage, retryCount, "", 0, nil,
	)
}

func (g *DefaultGateway) failWithRetryCountAndPartialResult(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	errorType ErrorType,
	retryable bool,
	safeMessage string,
	retryCount int,
	normalized normalizedResult,
) (*ToolCallResult, error) {
	result, err := g.persistFailure(
		ctx,
		startedAt,
		tc,
		req,
		def,
		errorType,
		true,
		retryable,
		safeMessage,
		retryCount,
		normalized.resultRef,
		normalized.outputBytes,
		normalizedArtifactRefs(normalized.artifacts),
	)
	if result != nil {
		result.DebugRef = normalized.debugRef
	}
	return result, err
}

func (g *DefaultGateway) persistFailure(
	ctx context.Context,
	startedAt time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	errorType ErrorType,
	executed bool,
	retryable bool,
	safeMessage string,
	retryCount int,
	partialResultRef string,
	outputBytes int64,
	artifactRefs []string,
) (*ToolCallResult, error) {
	persistenceCtx, cancelPersistence := terminalPersistenceContext(ctx)
	defer cancelPersistence()
	failure := &ToolFailure{
		ToolCallID:       req.ToolCallID,
		ToolName:         req.ToolName,
		ErrorType:        string(errorType),
		Retryable:        retryable,
		UserVisible:      true,
		Executed:         executed,
		PartialResultRef: partialResultRef,
		SafeUserMessage:  safeMessage,
		ModelGuidance: ToolFailureModelGuidance{
			Instruction:        "不要声称工具调用已经成功。请基于已有上下文说明工具调用失败。",
			AllowedNextActions: []string{"retry_tool", "ask_user", "answer_with_limitation"},
			ForbiddenClaims:    []string{"已完成工具调用", "工具结果显示"},
		},
	}
	if errorType == ErrorTypeSchemaValidationFailed && retryable {
		failure.ModelGuidance.Instruction = safeMessage
		failure.ModelGuidance.AllowedNextActions = []string{"retry_tool", "ask_user", "answer_with_limitation"}
		failure.ModelGuidance.RetryBudget = 1
	}
	event := buildToolFailedToolEvent(g.ids, g.clock.Now(), tc, req, def, failure, g.clock.Now().Sub(startedAt), retryCount)
	persisted, err := g.appendToolEvent(persistenceCtx, event)
	if err != nil {
		return nil, err
	}
	if g.stepStore != nil {
		if err := g.stepStore.FailToolStep(persistenceCtx, FailToolStepRequest{
			RunID:      req.RunID,
			StepID:     req.StepID,
			ToolCallID: req.ToolCallID,
			ErrorType:  string(errorType),
			FailedAt:   g.clock.Now(),
		}); err != nil {
			g.observeToolProjectionDegradation(persistenceCtx, req, def, ToolCallFailed, "step_store", "fail_tool_step")
		}
	}
	result := failureResult(req, failure, ToolUsage{
		Duration:     g.clock.Now().Sub(startedAt),
		InputBytes:   int64(len(req.Arguments)),
		OutputBytes:  outputBytes,
		RetryCount:   retryCount,
		ArtifactRefs: append([]string(nil), artifactRefs...),
	}, persisted)
	if errorType == ErrorTypeSchemaValidationFailed && retryable {
		result.ModelContextResult = failureModelContextResult(failure)
	}
	return result, nil
}

func failureModelContextResult(failure *ToolFailure) json.RawMessage {
	if failure == nil {
		return nil
	}
	encoded, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": string(ErrorTypeSchemaValidationFailed), "message": failure.SafeUserMessage,
			"retryable": failure.Retryable, "retry_budget": failure.ModelGuidance.RetryBudget,
		},
		"instruction":          failure.ModelGuidance.Instruction,
		"allowed_next_actions": failure.ModelGuidance.AllowedNextActions,
	})
	if err != nil {
		return nil
	}
	return encoded
}

func terminalPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalPersistenceTimeout)
}

func failureResult(req ToolCallRequest, failure *ToolFailure, usage ToolUsage, events ...observability.AgentEvent) *ToolCallResult {
	return &ToolCallResult{
		ToolCallID:  req.ToolCallID,
		ToolName:    req.ToolName,
		ToolVersion: req.ToolVersion,
		Status:      ToolCallFailed,
		Failure:     failure,
		Usage:       usage,
		Events:      events,
	}
}

func classifyExecutionError(err error) (ErrorType, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTypeTimeout, true
	}
	if errors.Is(err, context.Canceled) {
		return ErrorTypeCancelled, false
	}
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		if toolErr.Type == ErrorTypeCancelled {
			return ErrorTypeCancelled, false
		}
		return toolErr.Type, toolErr.Retryable
	}
	// External adapters must declare upstream failures explicitly. Treating an
	// untyped local validation or implementation error as an upstream outage
	// produces a false dependency signal and encourages unsafe blind retries.
	return ErrorTypeInternal, false
}

func safeFailureMessage(errorType ErrorType) string {
	switch errorType {
	case ErrorTypeTimeout:
		return "工具调用超时，可以稍后重试。"
	case ErrorTypeCancelled:
		return "工具调用已取消。"
	case ErrorTypeRateLimited:
		return "工具调用被限流，可以稍后重试。"
	case ErrorTypePermissionDenied:
		return "当前上下文无权调用该工具。"
	case ErrorTypeInvalidArgument:
		return "工具参数未通过业务校验，请修正后重试。"
	default:
		return "工具调用失败。"
	}
}

func (g *DefaultGateway) storeIdempotency(
	ctx context.Context,
	keys []string,
	req ToolCallRequest,
	def *ToolDefinition,
	logicalKey string,
	argumentsHash string,
	definitionHash string,
	result *ToolCallResult,
) {
	if g.idempotency == nil || len(keys) == 0 || result == nil {
		return
	}
	persistenceCtx, cancelPersistence := terminalPersistenceContext(ctx)
	defer cancelPersistence()
	for _, key := range keys {
		if key == "" {
			continue
		}
		record := IdempotencyRecord{
			Key: key, ToolCallID: req.ToolCallID, ToolName: req.ToolName, ToolVersion: req.ToolVersion,
			ArgumentsHash: argumentsHash, DefinitionHash: definitionHash, LogicalKeyHash: hashString(logicalKey),
			Status: result.Status, Result: result, Failure: result.Failure, CreatedAt: g.clock.Now(),
		}
		if err := g.idempotency.Put(persistenceCtx, record); err != nil {
			g.observeToolProjectionDegradation(persistenceCtx, req, def, result.Status, "idempotency_store", "put_terminal")
			return
		}
	}
}

func cloneToolCallResult(in *ToolCallResult) *ToolCallResult {
	if in == nil {
		return nil
	}
	out := *in
	out.ResultPreview = append([]byte(nil), in.ResultPreview...)
	out.ModelContextResult = append([]byte(nil), in.ModelContextResult...)
	out.Events = make([]observability.AgentEvent, len(in.Events))
	for i := range in.Events {
		out.Events[i] = cloneAgentEvent(in.Events[i])
	}
	out.Usage.ArtifactRefs = append([]string(nil), in.Usage.ArtifactRefs...)
	if in.Failure != nil {
		failure := *in.Failure
		failure.ModelGuidance.AllowedNextActions = append([]string(nil), in.Failure.ModelGuidance.AllowedNextActions...)
		failure.ModelGuidance.ForbiddenClaims = append([]string(nil), in.Failure.ModelGuidance.ForbiddenClaims...)
		out.Failure = &failure
	}
	return &out
}
