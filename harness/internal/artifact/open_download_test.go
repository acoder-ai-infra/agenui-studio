package artifact_test

import (
	"io"
	"path"
	"strings"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
)

func tokenFromURL(t *testing.T, rawURL string) string {
	t.Helper()
	return path.Base(rawURL)
}

func newCodecStore(t *testing.T, clock Clock) (*Store, *downloadtoken.Codec) {
	t.Helper()
	codec, err := downloadtoken.New([]byte("store-open-download-secret"))
	if err != nil {
		t.Fatalf("downloadtoken.New() error = %v", err)
	}
	objects, err := objectstore.NewFile(t.TempDir(), "http://artifact.local")
	if err != nil {
		t.Fatalf("new file object store: %v", err)
	}
	objects.SetDownloadCodec(codec)
	store := NewStore(StoreConfig{
		ObjectStore:    objects,
		MetadataStore:  metastore.NewMemory(),
		Clock:          clock,
		MaxObjectBytes: 1024 * 1024,
		DownloadCodec:  codec,
	})
	return store, codec
}

func seedArtifact(t *testing.T, store *Store, content string) *ArtifactMeta {
	t.Helper()
	meta, err := store.Put(actorContext(), PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleProtocol,
		OwnerID:      "dl-1",
		ArtifactType: ArtifactTypeImage,
		MimeType:     "image/png",
		Name:         "pixel.png",
		Visibility:   VisibilityUserVisible,
		Content:      strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	return meta
}

func TestOpenDownloadRoundTrip(t *testing.T) {
	clock := fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}
	store, _ := newCodecStore(t, clock)
	meta := seedArtifact(t, store, "png bytes")

	url, err := store.CreateDownloadURL(actorContext(), meta.ArtifactRef, DownloadURLOptions{TTL: time.Minute})
	if err != nil {
		t.Fatalf("create download url: %v", err)
	}
	if !strings.HasPrefix(url.URL, "http://artifact.local/download/") || strings.Contains(url.URL, meta.StorageKey) {
		t.Fatalf("download url should be controlled and opaque: %#v", url)
	}

	obj, err := store.OpenDownload(actorContext(), tokenFromURL(t, url.URL))
	if err != nil {
		t.Fatalf("OpenDownload() error = %v", err)
	}
	defer obj.Content.Close()
	got, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatalf("read redeemed content: %v", err)
	}
	if string(got) != "png bytes" {
		t.Fatalf("redeemed content = %q, want %q", got, "png bytes")
	}
	if obj.Meta.MimeType != "image/png" {
		t.Fatalf("redeemed mime = %q, want image/png", obj.Meta.MimeType)
	}
}

func TestOpenDownloadRejectsExpiredToken(t *testing.T) {
	clock := fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}
	store, codec := newCodecStore(t, clock)
	meta := seedArtifact(t, store, "content")

	// Seal a token whose wall-clock expiry is already in the past.
	expired, err := codec.Seal(meta.StorageKey, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if _, err := store.OpenDownload(actorContext(), expired); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("OpenDownload(expired) error = %v, want ErrPermissionDenied", err)
	}
}

func TestOpenDownloadRejectsForeignToken(t *testing.T) {
	clock := fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}
	store, _ := newCodecStore(t, clock)
	meta := seedArtifact(t, store, "content")

	other, err := downloadtoken.New([]byte("a-different-secret"))
	if err != nil {
		t.Fatalf("downloadtoken.New() error = %v", err)
	}
	forged, err := other.Seal(meta.StorageKey, clock.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if _, err := store.OpenDownload(actorContext(), forged); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("OpenDownload(foreign) error = %v, want ErrPermissionDenied", err)
	}
}

func TestOpenDownloadRejectsDeletedArtifact(t *testing.T) {
	clock := fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}
	store, _ := newCodecStore(t, clock)
	meta := seedArtifact(t, store, "content")

	url, err := store.CreateDownloadURL(actorContext(), meta.ArtifactRef, DownloadURLOptions{TTL: time.Minute})
	if err != nil {
		t.Fatalf("create download url: %v", err)
	}
	if err := store.Delete(actorContext(), meta.ArtifactRef, DeleteReasonUser); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if _, err := store.OpenDownload(actorContext(), tokenFromURL(t, url.URL)); err == nil {
		t.Fatal("OpenDownload(deleted) error = nil, want error")
	}
}

func TestOpenDownloadWithoutCodecFails(t *testing.T) {
	store := NewStore(StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	if _, err := store.OpenDownload(actorContext(), "any-token"); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("OpenDownload(no codec) error = %v, want ErrInvalidArgument", err)
	}
}
