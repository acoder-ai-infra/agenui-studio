package observability

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap/zapcore"
)

// LogEntry is one captured structured-log line, retained in memory so a debug
// console can pull logs correlated by trace_id/run_id. Trace fields are lifted
// from the request's TraceContext; caller-supplied fields are flattened to a map.
type LogEntry struct {
	Time     time.Time      `json:"time"`
	Level    string         `json:"level"`
	Message  string         `json:"message"`
	TraceID  string         `json:"trace_id,omitempty"`
	RunID    string         `json:"run_id,omitempty"`
	TenantID string         `json:"tenant_id,omitempty"`
	Error    string         `json:"error,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
}

// LogQuery filters retained log entries. Empty fields match everything; Limit<=0
// returns all matches.
type LogQuery struct {
	TraceID  string
	RunID    string
	TenantID string
	Level    string
	Limit    int
}

// LogQuerier is the read side a debug endpoint depends on. RingLogger implements
// it; a plain ZapLogger does not, so the endpoint degrades gracefully (501).
type LogQuerier interface {
	QueryLogs(q LogQuery) []LogEntry
}

// logRing is a fixed-capacity ring buffer of log entries, safe for concurrent
// writers/readers. When full it overwrites the oldest entry.
type logRing struct {
	mu   sync.Mutex
	buf  []LogEntry
	next int
	size int
	capN int
}

func (r *logRing) add(e LogEntry) {
	r.mu.Lock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % r.capN
	if r.size < r.capN {
		r.size++
	}
	r.mu.Unlock()
}

// query returns matching entries newest-first, capped by q.Limit.
func (r *logRing) query(q LogQuery) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := 0
	if r.size == r.capN {
		start = r.next // oldest slot once wrapped
	}
	out := make([]LogEntry, 0, r.size)
	for i := 0; i < r.size; i++ {
		e := r.buf[(start+i)%r.capN]
		if logMatches(e, q) {
			out = append(out, e)
		}
	}
	// newest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

func logMatches(e LogEntry, q LogQuery) bool {
	if q.TraceID != "" && e.TraceID != q.TraceID {
		return false
	}
	if q.RunID != "" && e.RunID != q.RunID {
		return false
	}
	if q.TenantID != "" && e.TenantID != q.TenantID {
		return false
	}
	if q.Level != "" && !strings.EqualFold(e.Level, q.Level) {
		return false
	}
	return true
}

// RingLogger decorates a StructuredLogger: every log line is forwarded to the
// inner logger unchanged and also appended to a shared in-memory ring for later
// trace-correlated retrieval. It implements both StructuredLogger and LogQuerier.
type RingLogger struct {
	inner StructuredLogger
	ring  *logRing
	bound []Field
	now   func() time.Time
}

// NewRingLogger wraps inner, retaining up to capacity entries (default 2000).
func NewRingLogger(inner StructuredLogger, capacity int) *RingLogger {
	if capacity <= 0 {
		capacity = 2000
	}
	return &RingLogger{
		inner: inner,
		ring:  &logRing{buf: make([]LogEntry, capacity), capN: capacity},
		now:   time.Now,
	}
}

func (l *RingLogger) Debug(ctx context.Context, msg string, fields ...Field) {
	l.inner.Debug(ctx, msg, fields...)
	l.record(ctx, "debug", msg, nil, fields)
}

func (l *RingLogger) Info(ctx context.Context, msg string, fields ...Field) {
	l.inner.Info(ctx, msg, fields...)
	l.record(ctx, "info", msg, nil, fields)
}

func (l *RingLogger) Warn(ctx context.Context, msg string, fields ...Field) {
	l.inner.Warn(ctx, msg, fields...)
	l.record(ctx, "warn", msg, nil, fields)
}

func (l *RingLogger) Error(ctx context.Context, msg string, err error, fields ...Field) {
	l.inner.Error(ctx, msg, err, fields...)
	l.record(ctx, "error", msg, err, fields)
}

func (l *RingLogger) With(fields ...Field) StructuredLogger {
	return &RingLogger{
		inner: l.inner.With(fields...),
		ring:  l.ring, // shared buffer
		bound: append(append([]Field{}, l.bound...), fields...),
		now:   l.now,
	}
}

func (l *RingLogger) Sync() { l.inner.Sync() }

// QueryLogs returns retained entries matching q, newest-first.
func (l *RingLogger) QueryLogs(q LogQuery) []LogEntry {
	return l.ring.query(q)
}

func (l *RingLogger) record(ctx context.Context, level, msg string, err error, fields []Field) {
	e := LogEntry{Time: l.now(), Level: level, Message: msg}
	if err != nil {
		e.Error = err.Error()
	}
	if tc, ok := TraceContextFrom(ctx); ok {
		e.TraceID, e.RunID, e.TenantID = tc.TraceID, tc.RunID, tc.TenantID
	}
	all := fields
	if len(l.bound) > 0 {
		all = append(append([]Field{}, l.bound...), fields...)
	}
	e.Fields = extractFields(all)
	l.ring.add(e)
}

// extractFields flattens zap fields to a plain map via zapcore's map encoder.
func extractFields(fields []Field) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	if len(enc.Fields) == 0 {
		return nil
	}
	return enc.Fields
}
