package observability

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const artifactDownloadPathPattern = "/api/v1/artifacts/download/{token}"

type AccessLogger interface {
	Log(ctx context.Context, entry AccessEntry)
}

type AccessEntry struct {
	StartedAt       time.Time
	Duration        time.Duration
	Method          string
	Path            string
	QuerySafe       string
	Protocol        string
	StatusCode      int
	RequestBytes    int64
	ResponseBytes   int
	ClientIP        string
	CDNSrcIP        string
	Host            string
	UserAgent       string
	RequestPreview  string
	ResponsePreview string
}

type StructuredAccessLogger struct {
	Logger StructuredLogger
}

func (l StructuredAccessLogger) Log(ctx context.Context, entry AccessEntry) {
	logger := l.Logger
	if logger == nil {
		logger = NoopLogger{}
	}
	logger.Info(ctx, "access",
		String("method", entry.Method),
		String("path", entry.Path),
		String("query_safe", entry.QuerySafe),
		String("proto", entry.Protocol),
		Int("status", entry.StatusCode),
		Int64("duration_ms", entry.Duration.Milliseconds()),
		Int64("request_bytes", entry.RequestBytes),
		Int("response_bytes", entry.ResponseBytes),
		String("client_ip", entry.ClientIP),
		String("cdn_src_ip", entry.CDNSrcIP),
		String("host", entry.Host),
		String("user_agent", entry.UserAgent),
		String("ts", "["+entry.StartedAt.Format("2006-01-02 15:04:05.000")+"]"),
		String("latency", strconv.FormatInt(entry.Duration.Milliseconds(), 10)+"ms"),
		String("reqBody", entry.RequestPreview),
		String("resBody", entry.ResponsePreview),
	)
}

func NewAccessEntry(r *http.Request, statusCode int, responseBytes int, startedAt time.Time, maxPreviewBytes int) AccessEntry {
	requestBytes := r.ContentLength
	if requestBytes < 0 {
		requestBytes = 0
	}
	return AccessEntry{
		StartedAt:     startedAt,
		Duration:      time.Since(startedAt),
		Method:        r.Method,
		Path:          safeHTTPPath(r),
		QuerySafe:     RedactBasic(r.URL.RawQuery),
		Protocol:      r.Proto,
		StatusCode:    statusCode,
		RequestBytes:  requestBytes,
		ResponseBytes: responseBytes,
		ClientIP:      clientIPFromRequest(r),
		CDNSrcIP:      valueOrDash(r.Header.Get("cdn-src-ip")),
		Host:          r.Host,
		UserAgent:     r.UserAgent(),
	}
}

// safeHTTPPath removes bearer capabilities from traces and access logs. The
// route pattern is available after ServeMux dispatch; the prefix check also
// protects the span that starts before routing has populated Request.Pattern.
func safeHTTPPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	if r.Pattern == "GET "+artifactDownloadPathPattern || strings.HasPrefix(r.URL.Path, "/api/v1/artifacts/download/") {
		return artifactDownloadPathPattern
	}
	return r.URL.Path
}

func clientIPFromRequest(r *http.Request) string {
	for _, header := range []string{"x-forwarded-for", "x-real-ip", "x-client-ip"} {
		if value := r.Header.Get(header); value != "" {
			return value
		}
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "-"
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
