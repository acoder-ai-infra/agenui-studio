package objectstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const task6ObjectKey = "tenants/tenant-a/sessions/sess-1/runs/run-1/art_download"
const task6OpaqueObjectKey = "secret-object-key"

func TestMemoryDownloadURLRequiresObjectExistence(t *testing.T) {
	store := NewMemory()
	if url, err := store.CreateDownloadURL(context.Background(), task6ObjectKey, time.Minute); url != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("missing object URL = %#v, error = %v; want not_found", url, err)
	}
	if _, err := store.Put(context.Background(), task6ObjectKey, strings.NewReader("data")); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	if url, err := store.CreateDownloadURL(context.Background(), task6ObjectKey, time.Minute); err != nil || url == nil {
		t.Fatalf("existing object URL = %#v, error = %v", url, err)
	}
}

func TestMemoryDownloadURLTTLBoundaries(t *testing.T) {
	store := NewMemory()
	if _, err := store.Put(context.Background(), task6ObjectKey, strings.NewReader("data")); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	assertTask6DownloadTTLTable(t, func(ttl time.Duration) (*artifact.DownloadURL, error) {
		return store.CreateDownloadURL(context.Background(), task6ObjectKey, ttl)
	})
}

func TestMemoryDownloadURLTokensAreOpaqueUniqueAndDecodeTo32Bytes(t *testing.T) {
	store := NewMemory()
	if _, err := store.Put(context.Background(), task6OpaqueObjectKey, strings.NewReader("data")); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	assertTask6OpaqueDownloadURLs(t, task6OpaqueObjectKey, func() (*artifact.DownloadURL, error) {
		return store.CreateDownloadURL(context.Background(), task6OpaqueObjectKey, time.Minute)
	})
}

func assertTask6OpaqueDownloadURLs(t *testing.T, key string, issue func() (*artifact.DownloadURL, error)) {
	t.Helper()
	wantAbsent := []string{
		key,
		url.PathEscape(key),
		url.QueryEscape(key),
		base64.RawURLEncoding.EncodeToString([]byte(key)),
		base64.URLEncoding.EncodeToString([]byte(key)),
		base64.RawStdEncoding.EncodeToString([]byte(key)),
		base64.StdEncoding.EncodeToString([]byte(key)),
	}
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		grant, err := issue()
		if err != nil || grant == nil {
			t.Fatalf("grant %d = %#v, error = %v", i, grant, err)
		}
		parsed, err := url.Parse(grant.URL)
		if err != nil {
			t.Fatalf("parse grant %d URL %q: %v", i, grant.URL, err)
		}
		token := path.Base(parsed.Path)
		rawToken, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(rawToken) != 32 {
			t.Fatalf("grant %d token %q decoded to %d bytes, error = %v; want 32", i, token, len(rawToken), err)
		}
		if _, duplicate := seen[token]; duplicate {
			t.Fatalf("grant %d reused token %q", i, token)
		}
		seen[token] = struct{}{}
		if bytes.Contains(rawToken, []byte(key)) {
			t.Fatalf("grant %d decoded token contains key %q", i, key)
		}

		decodedForms := []string{grant.URL, token}
		for round := 0; round < 4; round++ {
			for _, encoded := range append([]string(nil), decodedForms...) {
				pathDecoded, pathErr := url.PathUnescape(encoded)
				if pathErr != nil {
					t.Fatalf("grant %d path-unescape %q: %v", i, encoded, pathErr)
				}
				queryDecoded, queryErr := url.QueryUnescape(encoded)
				if queryErr != nil {
					t.Fatalf("grant %d query-unescape %q: %v", i, encoded, queryErr)
				}
				decodedForms = append(decodedForms, pathDecoded, queryDecoded)
			}
		}
		for _, decoded := range decodedForms {
			for _, forbidden := range wantAbsent {
				if forbidden != "" && strings.Contains(decoded, forbidden) {
					t.Fatalf("grant %d leaks key representation %q after decoding: %q", i, forbidden, decoded)
				}
			}
		}
	}
}

func assertTask6DownloadTTLTable(t *testing.T, issue func(time.Duration) (*artifact.DownloadURL, error)) {
	t.Helper()
	tests := []struct {
		name      string
		ttl       time.Duration
		effective time.Duration
		wantCode  artifact.ErrorCode
	}{
		{name: "negative", ttl: -time.Nanosecond, wantCode: artifact.ErrInvalidArgument},
		{name: "one nanosecond", ttl: time.Nanosecond, effective: time.Nanosecond},
		{name: "zero defaults", ttl: 0, effective: 5 * time.Minute},
		{name: "exact maximum", ttl: 5 * time.Minute, effective: 5 * time.Minute},
		{name: "above maximum", ttl: 5*time.Minute + time.Nanosecond, wantCode: artifact.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			url, err := issue(tc.ttl)
			after := time.Now()
			if tc.wantCode != "" {
				if url != nil || !artifact.IsErrorCode(err, tc.wantCode) {
					t.Fatalf("URL = %#v, error = %v; want %s", url, err, tc.wantCode)
				}
				return
			}
			if err != nil || url == nil {
				t.Fatalf("URL = %#v, error = %v", url, err)
			}
			if url.ExpiresAt.Before(before.Add(tc.effective)) || url.ExpiresAt.After(after.Add(tc.effective)) {
				t.Fatalf("ExpiresAt = %v, want between %v and %v", url.ExpiresAt, before.Add(tc.effective), after.Add(tc.effective))
			}
		})
	}
}
