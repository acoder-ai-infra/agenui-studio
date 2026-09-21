package observability

import (
	"bufio"
	"net"
	"net/http"
	"time"
)

const (
	HeaderTraceID = "x-trace-id"
	HeaderSpanID  = "x-span-id"
	HeaderRunID   = "x-run-id"
)

type HTTPMiddlewareConfig struct {
	ServiceName string
	Logger      StructuredLogger
	AccessLog   AccessLogger
	Tracer      TraceProvider
	IDs         IDGenerator
}

func HTTPMiddleware(cfg HTTPMiddlewareConfig) func(http.Handler) http.Handler {
	if cfg.IDs == nil {
		cfg.IDs = NewULIDGenerator("")
	}
	if cfg.Tracer == nil {
		cfg.Tracer = NewNoopTracer(cfg.ServiceName)
	}
	if cfg.Logger == nil {
		cfg.Logger = NoopLogger{}
	}
	if cfg.AccessLog == nil {
		cfg.AccessLog = StructuredAccessLogger{Logger: cfg.Logger}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startedAt := time.Now()
			tc := TraceContext{
				TraceID:        firstNonEmpty(r.Header.Get(HeaderTraceID), r.URL.Query().Get("traceId"), cfg.IDs.NewTraceID()),
				ConversationID: firstNonEmpty(r.Header.Get("x-conversation-id"), r.URL.Query().Get("conversationId")),
				RequestID:      headerOrNew(r, "x-request-id", cfg.IDs.NewRequestID),
				TenantID:       r.Header.Get("x-tenant-id"),
				UserID:         r.Header.Get("x-user-id"),
				Protocol:       "http",
				Source:         r.UserAgent(),
				Sampled:        true,
			}
			ctx := WithTraceContext(r.Context(), tc)
			ctx = WithLogger(ctx, cfg.Logger)
			ctx, span := cfg.Tracer.Start(ctx, "http.request",
				String("http.method", r.Method),
				String("http.path", safeHTTPPath(r)),
			)
			defer span.End()

			recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
			w.Header().Set(HeaderTraceID, span.TraceContext().TraceID)
			w.Header().Set(HeaderSpanID, span.TraceContext().SpanID)
			if span.TraceContext().ConversationID != "" {
				w.Header().Set("x-conversation-id", span.TraceContext().ConversationID)
			}

			next.ServeHTTP(recorder, r.WithContext(ctx))

			cfg.AccessLog.Log(ctx, NewAccessEntry(r, recorder.statusCode, recorder.bytesWritten, startedAt, 0))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	n, err := r.ResponseWriter.Write(data)
	r.bytesWritten += n
	return n, err
}

// Flush delegates to the underlying ResponseWriter so SSE streaming works through
// this middleware (the wrapper must implement http.Flusher explicitly).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying ResponseWriter so protocol upgrades
// (WebSocket) continue to work through the access-log wrapper.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func headerOrNew(r *http.Request, name string, gen func() string) string {
	if value := r.Header.Get(name); value != "" {
		return value
	}
	return gen()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
