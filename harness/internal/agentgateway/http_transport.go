package agentgateway

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Wire-level response caps enforced before the SDK reads a response body.
const (
	agentCardMaxWireBytes = 256 * 1024
	jsonRPCMaxWireBytes   = 1 * 1024 * 1024
)

var errResponseTooLarge = errors.New("a2a wire response exceeds limit")

// sharedA2ATransport is a process-wide connection pool reused by per-invocation
// clients; only the per-client credential/limits differ, never the pool.
var sharedA2ATransport = &http.Transport{
	MaxIdleConns:        32,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     90 * time.Second,
}

// boundedTransport injects credential headers only for the expected origin,
// strips them on any cross-origin request (redirects), and caps the response
// body before the SDK reads it.
type boundedTransport struct {
	base           http.RoundTripper
	maxBytes       int64
	credential     http.Header
	expectedOrigin string
}

func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.expectedOrigin != "" && requestOrigin(req.URL) == t.expectedOrigin {
		for key, values := range t.credential {
			req.Header.Del(key)
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	} else {
		// Cross-origin (e.g. a redirect target): strip every credential header key.
		for key := range t.credential {
			req.Header.Del(key)
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.maxBytes > 0 {
		if resp.ContentLength > t.maxBytes {
			resp.Body.Close()
			return nil, errResponseTooLarge
		}
		resp.Body = &limitedReadCloser{inner: resp.Body, remaining: t.maxBytes + 1}
	}
	return resp, nil
}

// newBoundedA2AClient builds a per-invocation client with a bounded transport,
// credential injection for the expected origin, and an HTTPS-preserving redirect
// policy. It reuses the shared connection pool.
func newBoundedA2AClient(expectedOrigin string, credential http.Header, maxBytes int64, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &boundedTransport{
			base:           sharedA2ATransport,
			maxBytes:       maxBytes,
			credential:     cloneHeader(credential),
			expectedOrigin: expectedOrigin,
		},
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Never silently downgrade https to http on redirect.
			if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme == "http" {
				return errors.New("insecure redirect downgrade rejected")
			}
			return nil
		},
	}
}

type limitedReadCloser struct {
	inner     io.ReadCloser
	remaining int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.inner.Read(p)
	l.remaining -= int64(n)
	if l.remaining <= 0 && err == nil {
		return n, errResponseTooLarge
	}
	return n, err
}

func (l *limitedReadCloser) Close() error { return l.inner.Close() }

func requestOrigin(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func cloneHeader(header http.Header) http.Header {
	if len(header) == 0 {
		return http.Header{}
	}
	out := make(http.Header, len(header))
	for key, values := range header {
		out[key] = append([]string(nil), values...)
	}
	return out
}
