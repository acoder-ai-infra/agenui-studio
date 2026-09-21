package openai

import (
	"context"
	"errors"
	"io"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// toolSchemaNegotiationStream buffers only the first tool-bearing response for
// an unknown OpenAI-compatible deployment. Some company gateways tunnel an
// upstream invalid_function_parameters error as HTTP 200 assistant text, so an
// HTTP-only capability probe cannot detect it. A normal response marks the
// deployment native; a recognized schema rejection is discarded and retried
// once with the restricted projection.
type toolSchemaNegotiationStream struct {
	primary    mg.AdapterStream
	retry      func(context.Context) (mg.AdapterStream, error)
	markNative func()

	prepared   bool
	prepareErr error
	buffered   []mg.NormalizedChunk
	index      int
	current    mg.AdapterStream
}

func newToolSchemaNegotiationStream(
	primary mg.AdapterStream,
	retry func(context.Context) (mg.AdapterStream, error),
	markNative func(),
) mg.AdapterStream {
	return &toolSchemaNegotiationStream{primary: primary, retry: retry, markNative: markNative}
}

func (s *toolSchemaNegotiationStream) Next(ctx context.Context) (mg.NormalizedChunk, error) {
	if !s.prepared {
		s.prepare(ctx)
	}
	if s.prepareErr != nil {
		return mg.NormalizedChunk{}, s.prepareErr
	}
	if s.current != nil {
		return s.current.Next(ctx)
	}
	if s.index >= len(s.buffered) {
		return mg.NormalizedChunk{}, io.EOF
	}
	chunk := s.buffered[s.index]
	s.index++
	return chunk, nil
}

func (s *toolSchemaNegotiationStream) prepare(ctx context.Context) {
	s.prepared = true
	if s.primary == nil {
		s.prepareErr = errors.New("openai schema negotiation stream is missing its primary response")
		return
	}
	var content strings.Builder
	for {
		chunk, err := s.primary.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.prepareErr = err
			_ = s.primary.Close()
			return
		}
		s.buffered = append(s.buffered, chunk)
		if chunk.Kind == mg.ChunkToken {
			content.WriteString(chunk.TextDelta)
		}
		if chunk.Err != nil {
			content.WriteString(chunk.Err.Message)
		}
	}
	_ = s.primary.Close()
	if !isUnsupportedToolSchemaPayload([]byte(content.String())) {
		if s.markNative != nil {
			s.markNative()
		}
		return
	}
	s.buffered = nil
	s.index = 0
	if s.retry == nil {
		s.prepareErr = errors.New("openai restricted schema retry is unavailable")
		return
	}
	s.current, s.prepareErr = s.retry(ctx)
}

func (s *toolSchemaNegotiationStream) Close() error {
	var closeErr error
	if s.primary != nil {
		closeErr = s.primary.Close()
	}
	if s.current != nil {
		if err := s.current.Close(); closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}
