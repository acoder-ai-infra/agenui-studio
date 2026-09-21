package artifact

import (
	"context"
	"io"
	"time"
)

type ObjectInfo struct {
	Backend string
	Key     string
	Size    int64
}

type ObjectStore interface {
	Backend() string
	Put(ctx context.Context, key string, r io.Reader) (*ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	CreateDownloadURL(ctx context.Context, key string, ttl time.Duration) (*DownloadURL, error)
}
