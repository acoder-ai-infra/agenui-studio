package observability

import "go.uber.org/zap"

type Field = zap.Field

func String(key, value string) Field {
	return zap.String(key, value)
}

func Int(key string, value int) Field {
	return zap.Int(key, value)
}

func Int64(key string, value int64) Field {
	return zap.Int64(key, value)
}

func Bool(key string, value bool) Field {
	return zap.Bool(key, value)
}

func Any(key string, value any) Field {
	return zap.Any(key, value)
}

func Error(err error) Field {
	return zap.Error(err)
}

func TraceFields(tc TraceContext) []Field {
	fields := make([]Field, 0, 16)
	if tc.TraceID != "" {
		fields = append(fields, String("trace_id", tc.TraceID))
	}
	if tc.SpanID != "" {
		fields = append(fields, String("span_id", tc.SpanID))
	}
	if tc.ParentSpanID != "" {
		fields = append(fields, String("parent_span_id", tc.ParentSpanID))
	}
	if tc.SessionID != "" {
		fields = append(fields, String("session_id", tc.SessionID))
	}
	if tc.RunID != "" {
		fields = append(fields, String("run_id", tc.RunID))
	}
	if tc.ParentRunID != "" {
		fields = append(fields, String("parent_run_id", tc.ParentRunID))
	}
	if tc.RootRunID != "" {
		fields = append(fields, String("root_run_id", tc.RootRunID))
	}
	if tc.ConversationID != "" {
		fields = append(fields, String("conversation_id", tc.ConversationID))
	}
	if tc.RequestID != "" {
		fields = append(fields, String("request_id", tc.RequestID))
	}
	if tc.TenantID != "" {
		fields = append(fields, String("tenant_id", tc.TenantID))
	}
	if tc.UserID != "" {
		fields = append(fields, String("user_id", tc.UserID))
	}
	if tc.AgentID != "" {
		fields = append(fields, String("agent_id", tc.AgentID))
	}
	if tc.AgentVersion != "" {
		fields = append(fields, String("agent_version", tc.AgentVersion))
	}
	if tc.Protocol != "" {
		fields = append(fields, String("protocol", tc.Protocol))
	}
	if tc.Source != "" {
		fields = append(fields, String("source", tc.Source))
	}
	return fields
}
