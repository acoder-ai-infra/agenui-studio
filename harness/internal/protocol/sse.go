package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// SSEAdapter projects a canonical observability.AgentEvent into an SSE frame
// (canonical §8.1). It applies view_type mapping and never writes back to the
// EventStore.
type SSEAdapter struct {
	Mapper ViewModelMapper
}

// NewSSEAdapter builds an SSEAdapter with the default view-model mapper.
func NewSSEAdapter() *SSEAdapter { return &SSEAdapter{Mapper: DefaultViewModelMapper{}} }

func (a *SSEAdapter) mapper() ViewModelMapper {
	if a.Mapper != nil {
		return a.Mapper
	}
	return DefaultViewModelMapper{}
}

// ID identifies the adapter.
func (a *SSEAdapter) ID() string { return "sse" }

// Capabilities reports the SSE adapter capabilities.
func (a *SSEAdapter) Capabilities(context.Context) ProtocolCapabilities {
	return ProtocolCapabilities{ID: "sse", Streaming: true, MediaType: "text/event-stream"}
}

// Match reports whether the request selects SSE.
func (a *SSEAdapter) Match(_ context.Context, req ClientProtocolRequest) bool {
	if req.Protocol == "sse" {
		return true
	}
	return strings.Contains(strings.ToLower(req.Accept), "text/event-stream")
}

// Convert projects an AgentEvent into an SSE ProtocolFrame. Visibility filtering
// is the caller's responsibility; Convert only projects fields. Debug refs are
// attached only when the target is authorized for the event's visibility.
func (a *SSEAdapter) Convert(_ context.Context, event observability.AgentEvent, target ClientTarget) (ProtocolFrame, error) {
	// Reuse the transport-neutral projection, then wrap it in the SSE shell
	// (schema tag + SSEFrame envelope).
	data := SSEData{
		SchemaVersion: SSESchemaVersion,
		EventData:     ProjectEvent(event, target, a.mapper()),
	}
	// 游标分层:只有已落库事件才带 SSE "id:" 行(== event_id),作为断线重连锚点
	// (Last-Event-ID → SequenceOf → after_sequence 补拉)。高频逐字增量不落库、无
	// sequence,若给它们发 id:,客户端会以一个无法解析的 event_id 重连并被 410。
	// 逐字增量的补拉走 HotBuffer 的 after_delta_seq,与此处的 sequence 游标解耦。
	id := event.EventID
	if observability.IsEphemeralDelta(event.EventType) && event.Sequence == 0 {
		// 仅当逐字增量没有可解析的 sequence(即未落库的实时帧)时才去掉 id:。
		// 落库过的事件(有 sequence)始终可作为重连锚点,保留 id:。
		id = ""
	}
	return ProtocolFrame{Kind: "sse", SSE: &SSEFrame{ID: id, Event: SSEEventName, Data: data}}, nil
}

// Validate checks the frame is a well-formed SSE frame.
func (a *SSEAdapter) Validate(_ context.Context, frame ProtocolFrame) error {
	if frame.Kind != "sse" || frame.SSE == nil {
		return &ProtocolError{Code: "PROTOCOL_INVALID_FRAME", Message: "not an SSE frame"}
	}
	if frame.SSE.Data.SchemaVersion != SSESchemaVersion {
		return &ProtocolError{Code: "PROTOCOL_INVALID_FRAME", Message: "unexpected schema_version"}
	}
	return nil
}

var _ ProtocolAdapter = (*SSEAdapter)(nil)

// WriteSSE serializes an SSE frame to w in the wire format (canonical §8.1) and
// flushes if w supports http.Flusher.
func WriteSSE(w io.Writer, frame SSEFrame) error {
	body, err := json.Marshal(frame.Data)
	if err != nil {
		return err
	}
	var b strings.Builder
	if frame.ID != "" {
		b.WriteString("id: ")
		b.WriteString(frame.ID)
		b.WriteByte('\n')
	}
	event := frame.Event
	if event == "" {
		event = SSEEventName
	}
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// WriteSSEComment writes a comment/heartbeat line (": <text>\n\n").
func WriteSSEComment(w io.Writer, text string) error {
	if _, err := fmt.Fprintf(w, ": %s\n\n", text); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// SeqString renders a sequence for diagnostic use.
func SeqString(seq int64) string { return strconv.FormatInt(seq, 10) }
