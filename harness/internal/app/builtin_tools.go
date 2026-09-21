package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

// This file implements provider-neutral built-in function tools that need no
// external infrastructure: arithmetic (calculator), wall-clock (get_current_time),
// JSON extraction (json_extract), artifact file read/list/edit and a human
// approval gate (request_approval). Each is registered by name in
// configuredFunctionHandlers and declared in configs/catalogs/tools.yaml.

const (
	maxEditFileContentBytes = 1 << 20
	maxListFilesReturned    = 200
)

func jsonResult(payload any) (*toolgateway.FunctionResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "marshal tool result", false, err)
	}
	return &toolgateway.FunctionResult{Data: data, MimeType: "application/json"}, nil
}

func schemaError(message string, cause error) error {
	return toolgateway.NewToolError(toolgateway.ErrorTypeSchemaValidationFailed, message, false, cause)
}

// ---- calculator ------------------------------------------------------------

func calculatorFunctionHandler() toolgateway.FunctionTool {
	return func(_ context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		var args struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return nil, schemaError("calculator arguments must match schema", err)
		}
		expr := strings.TrimSpace(args.Expression)
		if expr == "" {
			return nil, schemaError("expression is required", nil)
		}
		value, err := evalArithmetic(expr)
		if err != nil {
			return nil, schemaError("failed to evaluate expression: "+err.Error(), nil)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, schemaError("expression result is not a finite number", nil)
		}
		return jsonResult(map[string]any{"expression": expr, "result": value})
	}
}

// evalArithmetic is a bounded recursive-descent evaluator over + - * / % ^ and
// parentheses with decimals and unary minus. It touches nothing but the input
// string, so it is safe and deterministic.
func evalArithmetic(input string) (float64, error) {
	p := &arithParser{src: input}
	value, err := p.parseExpression()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return 0, fmt.Errorf("unexpected character at position %d", p.pos)
	}
	return value, nil
}

type arithParser struct {
	src string
	pos int
}

func (p *arithParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *arithParser) parseExpression() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return left, nil
		}
		switch p.src[p.pos] {
		case '+':
			p.pos++
			right, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			left += right
		case '-':
			p.pos++
			right, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			left -= right
		default:
			return left, nil
		}
	}
}

func (p *arithParser) parseTerm() (float64, error) {
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return left, nil
		}
		switch p.src[p.pos] {
		case '*':
			p.pos++
			right, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			left *= right
		case '/':
			p.pos++
			right, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			left /= right
		case '%':
			p.pos++
			right, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			if right == 0 {
				return 0, errors.New("modulo by zero")
			}
			left = math.Mod(left, right)
		default:
			return left, nil
		}
	}
}

func (p *arithParser) parseFactor() (float64, error) {
	base, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '^' {
		p.pos++
		exp, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *arithParser) parseUnary() (float64, error) {
	p.skipSpace()
	if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
		sign := p.src[p.pos]
		p.pos++
		value, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if sign == '-' {
			return -value, nil
		}
		return value, nil
	}
	return p.parsePrimary()
}

func (p *arithParser) parsePrimary() (float64, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0, errors.New("unexpected end of expression")
	}
	if p.src[p.pos] == '(' {
		p.pos++
		value, err := p.parseExpression()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return 0, errors.New("missing closing parenthesis")
		}
		p.pos++
		return value, nil
	}
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' ||
			((c == '+' || c == '-') && p.pos > start && (p.src[p.pos-1] == 'e' || p.src[p.pos-1] == 'E')) {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return 0, fmt.Errorf("unexpected character %q", p.src[p.pos])
	}
	value, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", p.src[start:p.pos])
	}
	return value, nil
}

// ---- get_current_time ------------------------------------------------------

func getCurrentTimeFunctionHandler(now func() time.Time) toolgateway.FunctionTool {
	if now == nil {
		now = time.Now
	}
	return func(_ context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		var args struct {
			Timezone string `json:"timezone"`
		}
		if len(req.Arguments) > 0 {
			if err := json.Unmarshal(req.Arguments, &args); err != nil {
				return nil, schemaError("get_current_time arguments must match schema", err)
			}
		}
		location := time.UTC
		tz := strings.TrimSpace(args.Timezone)
		if tz != "" && tz != "UTC" {
			loaded, err := time.LoadLocation(tz)
			if err != nil {
				return nil, schemaError("unknown timezone: "+tz, nil)
			}
			location = loaded
		}
		current := now().In(location)
		zoneName, offsetSeconds := current.Zone()
		return jsonResult(map[string]any{
			"iso8601":        current.Format(time.RFC3339),
			"unix_seconds":   current.Unix(),
			"timezone":       location.String(),
			"zone_name":      zoneName,
			"offset_seconds": offsetSeconds,
		})
	}
}

// ---- json_extract ----------------------------------------------------------

func jsonExtractFunctionHandler() toolgateway.FunctionTool {
	return func(_ context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		var args struct {
			JSON json.RawMessage `json:"json"`
			Path string          `json:"path"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return nil, schemaError("json_extract arguments must match schema", err)
		}
		raw := args.JSON
		// Accept both a JSON value and a JSON-encoded string containing JSON.
		var asString string
		if json.Unmarshal(raw, &asString) == nil && len(strings.TrimSpace(asString)) > 0 && json.Valid([]byte(asString)) {
			raw = json.RawMessage(asString)
		}
		var document any
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, schemaError("json field is not valid JSON", err)
		}
		value, found, err := extractJSONPath(document, strings.TrimSpace(args.Path))
		if err != nil {
			return nil, schemaError(err.Error(), nil)
		}
		return jsonResult(map[string]any{"path": args.Path, "found": found, "value": value})
	}
}

// extractJSONPath walks a dotted path with optional [index] array accessors,
// e.g. "items[0].name". An empty path returns the whole document.
func extractJSONPath(document any, path string) (any, bool, error) {
	if path == "" {
		return document, true, nil
	}
	current := document
	for _, segment := range strings.Split(path, ".") {
		key, indices, err := parsePathSegment(segment)
		if err != nil {
			return nil, false, err
		}
		if key != "" {
			object, ok := current.(map[string]any)
			if !ok {
				return nil, false, nil
			}
			current, ok = object[key]
			if !ok {
				return nil, false, nil
			}
		}
		for _, index := range indices {
			array, ok := current.([]any)
			if !ok || index < 0 || index >= len(array) {
				return nil, false, nil
			}
			current = array[index]
		}
	}
	return current, true, nil
}

func parsePathSegment(segment string) (string, []int, error) {
	bracket := strings.IndexByte(segment, '[')
	if bracket < 0 {
		return segment, nil, nil
	}
	key := segment[:bracket]
	rest := segment[bracket:]
	var indices []int
	for len(rest) > 0 {
		if rest[0] != '[' {
			return "", nil, fmt.Errorf("invalid path segment %q", segment)
		}
		close := strings.IndexByte(rest, ']')
		if close < 0 {
			return "", nil, fmt.Errorf("invalid path segment %q", segment)
		}
		index, err := strconv.Atoi(rest[1:close])
		if err != nil {
			return "", nil, fmt.Errorf("invalid array index in %q", segment)
		}
		indices = append(indices, index)
		rest = rest[close+1:]
	}
	return key, indices, nil
}

// ---- artifact file tools: read_file / list_files / edit_file ---------------

// artifactActor resolves the execution identity for artifact operations,
// preferring an actor already in context (as write_file relies on) and falling
// back to the trace carried on the call.
func artifactActor(ctx context.Context, req toolgateway.FunctionCall) (artifact.Actor, bool) {
	if actor, ok := artifact.ActorFromContext(ctx); ok {
		return actor, true
	}
	trace := req.Trace
	if trace.TenantID == "" || trace.UserID == "" || trace.SessionID == "" || trace.RunID == "" {
		return artifact.Actor{}, false
	}
	return artifact.Actor{
		Role: artifact.ActorContextEngine, TenantID: trace.TenantID, UserID: trace.UserID,
		SessionID: trace.SessionID, RunID: trace.RunID, AgentID: trace.AgentID,
	}, true
}

// artifactPath is the logical file path of an artifact. write_file/edit_file
// preserve the full relative path in Metadata["requested_path"]; the store's
// Name field only keeps the base name, so requested_path is the reliable key.
func artifactPath(meta artifact.ArtifactMeta) string {
	if meta.Metadata != nil {
		if requested := meta.Metadata["requested_path"]; requested != "" {
			return requested
		}
	}
	return meta.Name
}

// findArtifactByPath returns the most recent readable artifact whose logical
// path matches path, preferring current-run files before session uploads from
// earlier turns.
func findArtifactByPath(ctx context.Context, store artifact.ArtifactStore, actor artifact.Actor, path string) (*artifact.ArtifactMeta, error) {
	match, err := findArtifactByPathInScope(ctx, store, artifact.ListQuery{
		TenantID: actor.TenantID, SessionID: actor.SessionID, RunID: actor.RunID,
		Type: artifact.ArtifactTypeFile,
	}, path)
	if err != nil {
		return nil, err
	}
	if match != nil {
		return match, nil
	}
	match, err = findArtifactByPathInScope(ctx, store, artifact.ListQuery{
		TenantID: actor.TenantID, SessionID: actor.SessionID,
		Type: artifact.ArtifactTypeFile,
	}, path)
	if err != nil {
		return nil, err
	}
	if match != nil {
		return match, nil
	}
	return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "file not found: "+path, false, nil)
}

func findArtifactByPathInScope(ctx context.Context, store artifact.ArtifactStore, query artifact.ListQuery, path string) (*artifact.ArtifactMeta, error) {
	metas, err := store.List(ctx, query)
	if err != nil {
		return nil, err
	}
	var match *artifact.ArtifactMeta
	for i := range metas {
		if artifactPath(metas[i]) != path {
			continue
		}
		if match == nil || metas[i].CreatedAt.After(match.CreatedAt) {
			meta := metas[i]
			match = &meta
		}
	}
	return match, nil
}

func readArtifactBody(ctx context.Context, store artifact.ArtifactStore, ref string) (string, *artifact.ArtifactMeta, error) {
	object, err := store.Get(ctx, ref, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		return "", nil, err
	}
	defer object.Content.Close()
	if !isModelReadableArtifact(object.Meta.ArtifactType) || !isTextualMIME(object.Meta.MimeType) {
		return "", nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "artifact type or mime type is not readable text", false, nil)
	}
	if object.Meta.SizeBytes > maxModelReadableArtifactBytes {
		return "", nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "file exceeds readable size limit", false, nil)
	}
	body, err := io.ReadAll(io.LimitReader(object.Content, maxModelReadableArtifactBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(body) > maxModelReadableArtifactBytes || !utf8.Valid(body) {
		return "", nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "file content is too large or not valid UTF-8", false, nil)
	}
	return string(body), &object.Meta, nil
}

func readFileFunctionHandler(store artifact.ArtifactStore) toolgateway.FunctionTool {
	return func(ctx context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		if store == nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "artifact store is required for read_file", false, nil)
		}
		var args struct {
			Path        string `json:"path"`
			ArtifactRef string `json:"artifact_ref"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return nil, schemaError("read_file arguments must match schema", err)
		}
		path := strings.TrimSpace(args.Path)
		ref := strings.TrimSpace(args.ArtifactRef)
		if path == "" && ref == "" {
			return nil, schemaError("either path or artifact_ref is required", nil)
		}
		actor, ok := readFileArtifactActor(ctx, req)
		if !ok {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "read_file execution identity is incomplete", false, nil)
		}
		actorCtx := artifact.ContextWithActor(ctx, actor)
		resolvedPath := path
		if ref == "" {
			meta, err := findArtifactByPath(actorCtx, store, actor, path)
			if err != nil {
				return nil, err
			}
			ref = meta.ArtifactRef
		}
		body, meta, err := readArtifactBody(actorCtx, store, ref)
		if err != nil {
			return nil, err
		}
		if resolvedPath == "" {
			resolvedPath = artifactPath(*meta)
		}
		return jsonResult(map[string]any{
			"path": resolvedPath, "artifact_ref": meta.ArtifactRef, "mime_type": meta.MimeType,
			"size_bytes": meta.SizeBytes, "content": body,
		})
	}
}

func readFileArtifactActor(ctx context.Context, req toolgateway.FunctionCall) (artifact.Actor, bool) {
	actor, ok := artifactActor(ctx, req)
	if !ok {
		return artifact.Actor{}, false
	}
	userID := strings.TrimSpace(actor.UserID)
	if userID == "" {
		userID = strings.TrimSpace(req.Trace.UserID)
	}
	if actor.TenantID == "" || actor.SessionID == "" || actor.RunID == "" || userID == "" {
		return artifact.Actor{}, false
	}
	return artifact.Actor{
		Role:      artifact.ActorContextEngine,
		TenantID:  actor.TenantID,
		UserID:    userID,
		SessionID: actor.SessionID,
		RunID:     actor.RunID,
		AgentID:   actor.AgentID,
	}, true
}

func listFilesFunctionHandler(store artifact.ArtifactStore) toolgateway.FunctionTool {
	return func(ctx context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		if store == nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "artifact store is required for list_files", false, nil)
		}
		var args struct {
			Prefix string `json:"prefix"`
		}
		if len(req.Arguments) > 0 {
			if err := json.Unmarshal(req.Arguments, &args); err != nil {
				return nil, schemaError("list_files arguments must match schema", err)
			}
		}
		actor, ok := artifactActor(ctx, req)
		if !ok {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "list_files execution identity is incomplete", false, nil)
		}
		actorCtx := artifact.ContextWithActor(ctx, actor)
		metas, err := store.List(actorCtx, artifact.ListQuery{
			TenantID: actor.TenantID, SessionID: actor.SessionID, RunID: actor.RunID,
			Type: artifact.ArtifactTypeFile,
		})
		if err != nil {
			return nil, err
		}
		prefix := strings.TrimSpace(args.Prefix)
		files := make([]map[string]any, 0, len(metas))
		for i := range metas {
			filePath := artifactPath(metas[i])
			if filePath == "" {
				continue
			}
			if prefix != "" && !strings.HasPrefix(filePath, prefix) {
				continue
			}
			files = append(files, map[string]any{
				"path": filePath, "artifact_ref": metas[i].ArtifactRef,
				"mime_type": metas[i].MimeType, "size_bytes": metas[i].SizeBytes,
				"created_at": metas[i].CreatedAt.Format(time.RFC3339),
			})
		}
		sort.Slice(files, func(a, b int) bool { return files[a]["path"].(string) < files[b]["path"].(string) })
		truncated := false
		if len(files) > maxListFilesReturned {
			files = files[:maxListFilesReturned]
			truncated = true
		}
		return jsonResult(map[string]any{"files": files, "count": len(files), "truncated": truncated})
	}
}

func editFileFunctionHandler(store artifact.ArtifactStore) toolgateway.FunctionTool {
	return func(ctx context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		if store == nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "artifact store is required for edit_file", false, nil)
		}
		var args struct {
			Path      string  `json:"path"`
			OldString *string `json:"old_string"`
			NewString *string `json:"new_string"`
			Content   *string `json:"content"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return nil, schemaError("edit_file arguments must match schema", err)
		}
		path, err := validateWriteFilePath(args.Path)
		if err != nil {
			return nil, err
		}
		actor, ok := artifactActor(ctx, req)
		if !ok {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "edit_file execution identity is incomplete", false, nil)
		}
		actorCtx := artifact.ContextWithActor(ctx, actor)
		current, err := findArtifactByPath(actorCtx, store, actor, path)
		if err != nil {
			return nil, err
		}
		body, meta, err := readArtifactBody(actorCtx, store, current.ArtifactRef)
		if err != nil {
			return nil, err
		}
		var updated string
		switch {
		case args.Content != nil:
			updated = *args.Content
		case args.OldString != nil && args.NewString != nil:
			if *args.OldString == "" {
				return nil, schemaError("old_string must not be empty", nil)
			}
			count := strings.Count(body, *args.OldString)
			if count == 0 {
				return nil, schemaError("old_string was not found in the file", nil)
			}
			if count > 1 {
				return nil, schemaError(fmt.Sprintf("old_string is not unique (found %d occurrences)", count), nil)
			}
			updated = strings.Replace(body, *args.OldString, *args.NewString, 1)
		default:
			return nil, schemaError("provide either content, or both old_string and new_string", nil)
		}
		if len(updated) > maxEditFileContentBytes {
			return nil, schemaError("edited content exceeds 1MiB", nil)
		}
		mimeType := meta.MimeType
		if strings.TrimSpace(mimeType) == "" {
			mimeType = "text/markdown; charset=utf-8"
		}
		newMeta, err := store.Put(actorCtx, artifact.PutArtifactRequest{
			TenantID: actor.TenantID, UserID: actor.UserID, SessionID: actor.SessionID, RunID: actor.RunID,
			StepID: req.ToolCallID, OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: req.ToolCallID,
			ArtifactType: artifact.ArtifactTypeFile, MimeType: mimeType, Name: path,
			Visibility: artifact.VisibilityUserVisible, Content: bytes.NewReader([]byte(updated)),
			PreviewHint: artifact.PreviewHint{MaxBytes: 2048}, RetentionPolicy: artifact.RetentionSessionTTL,
			IdempotencyKey: "edit_file:" + req.ToolCallID + ":" + path,
			Metadata:       map[string]string{"requested_path": path, "tool": "edit_file"},
		})
		if err != nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "failed to persist edit_file artifact", false, err)
		}
		return jsonResult(map[string]any{
			"path": path, "artifact_ref": newMeta.ArtifactRef, "mime_type": newMeta.MimeType, "size_bytes": newMeta.SizeBytes,
		})
	}
}

// ---- request_approval ------------------------------------------------------

// requestApprovalFunctionHandler mirrors ask_user's interrupt/resume protocol
// but frames the interaction as an approve/reject decision gate for high-risk
// actions. It carries no side effects itself.
func requestApprovalFunctionHandler() toolgateway.FunctionTool {
	return func(_ context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		if req.Resume == nil {
			var arguments struct {
				Action  string `json:"action"`
				Reason  string `json:"reason"`
				Details string `json:"details"`
			}
			if err := json.Unmarshal(req.Arguments, &arguments); err != nil || strings.TrimSpace(arguments.Action) == "" {
				return nil, schemaError("request_approval requires a non-empty action", err)
			}
			return nil, &toolgateway.ToolInterruptedError{
				Info: map[string]any{
					"type":    "request_approval",
					"action":  arguments.Action,
					"reason":  arguments.Reason,
					"details": arguments.Details,
				},
				State: json.RawMessage(`{"schema_version":"harness.request_approval.state.v1"}`),
			}
		}
		if !req.Resume.WasInterrupted || !req.Resume.IsResumeTarget || len(req.Resume.Payload) == 0 || !json.Valid(req.Resume.Payload) {
			return nil, schemaError("request_approval resume payload is invalid", nil)
		}
		var decision struct {
			Approved *bool  `json:"approved"`
			Comment  string `json:"comment"`
		}
		if err := json.Unmarshal(req.Resume.Payload, &decision); err != nil || decision.Approved == nil {
			return nil, schemaError("approval decision must include a boolean 'approved'", err)
		}
		return jsonResult(map[string]any{"approved": *decision.Approved, "comment": decision.Comment})
	}
}
