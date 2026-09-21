package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

const (
	defaultReadArtifactPageBytes  = 8 << 10
	maxReadArtifactPageBytes      = 16 << 10
	maxModelReadableArtifactBytes = 1 << 20
)

type readArtifactArguments struct {
	ArtifactRef string `json:"artifact_ref"`
	OffsetBytes int    `json:"offset_bytes,omitempty"`
	LimitBytes  int    `json:"limit_bytes,omitempty"`
}

type artifactReadPage struct {
	ArtifactRef     string `json:"artifact_ref"`
	ArtifactType    string `json:"artifact_type"`
	MimeType        string `json:"mime_type"`
	SizeBytes       int64  `json:"size_bytes"`
	Content         string `json:"content"`
	OffsetBytes     int    `json:"offset_bytes"`
	NextOffsetBytes int    `json:"next_offset_bytes"`
	EOF             bool   `json:"eof"`
}

func newReadArtifactTool(store artifact.ArtifactStore) toolgateway.FunctionTool {
	return func(ctx context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		if store == nil {
			return nil, errors.New("read_artifact store is unavailable")
		}
		var args readArtifactArguments
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return nil, fmt.Errorf("decode read_artifact arguments: %w", err)
		}
		if args.ArtifactRef == "" || args.OffsetBytes < 0 || args.LimitBytes < 0 || args.LimitBytes > maxReadArtifactPageBytes {
			return nil, errors.New("invalid read_artifact arguments")
		}
		if args.LimitBytes == 0 {
			args.LimitBytes = defaultReadArtifactPageBytes
		}
		trace := req.Trace
		if trace.TenantID == "" || trace.UserID == "" || trace.SessionID == "" || trace.RunID == "" {
			return nil, errors.New("read_artifact execution identity is incomplete")
		}
		actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
			Role: artifact.ActorContextEngine, TenantID: trace.TenantID, UserID: trace.UserID,
			SessionID: trace.SessionID, RunID: trace.RunID, AgentID: trace.AgentID,
		})
		object, err := store.Get(actorCtx, args.ArtifactRef, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
		if err != nil {
			return nil, err
		}
		defer object.Content.Close()
		if !isModelReadableArtifact(object.Meta.ArtifactType) || !isTextualMIME(object.Meta.MimeType) {
			return nil, errors.New("artifact type or mime type is not model-readable")
		}
		if object.Meta.SizeBytes > maxModelReadableArtifactBytes {
			return nil, errors.New("artifact exceeds model-readable size limit")
		}
		body, err := io.ReadAll(io.LimitReader(object.Content, maxModelReadableArtifactBytes+1))
		if err != nil {
			return nil, err
		}
		if len(body) > maxModelReadableArtifactBytes || !utf8.Valid(body) {
			return nil, errors.New("artifact content is too large or is not valid UTF-8")
		}
		if args.OffsetBytes > len(body) || args.OffsetBytes < len(body) && !utf8.RuneStart(body[args.OffsetBytes]) {
			return nil, errors.New("offset_bytes is outside the artifact or splits a UTF-8 character")
		}
		end := args.OffsetBytes + args.LimitBytes
		if end > len(body) {
			end = len(body)
		}
		for end > args.OffsetBytes && end < len(body) && !utf8.RuneStart(body[end]) {
			end--
		}
		if end == args.OffsetBytes && end < len(body) {
			return nil, errors.New("limit_bytes is too small for the next UTF-8 character")
		}
		page := artifactReadPage{
			ArtifactRef: args.ArtifactRef, ArtifactType: string(object.Meta.ArtifactType), MimeType: object.Meta.MimeType,
			SizeBytes: object.Meta.SizeBytes, Content: string(body[args.OffsetBytes:end]),
			OffsetBytes: args.OffsetBytes, NextOffsetBytes: end, EOF: end == len(body),
		}
		data, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		return &toolgateway.FunctionResult{Data: data, MimeType: "application/json"}, nil
	}
}

func isModelReadableArtifact(value artifact.ArtifactType) bool {
	switch value {
	// host_data 是宿主经 ArtifactClient 上传、供 Run 消费的结构化业务数据
	//（如 VisualSpec），属于模型可读面。
	case artifact.ArtifactTypeToolResult, artifact.ArtifactTypeFinalResult, artifact.ArtifactTypeFile, artifact.ArtifactTypeHostData:
		return true
	default:
		return false
	}
}

func isTextualMIME(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}
