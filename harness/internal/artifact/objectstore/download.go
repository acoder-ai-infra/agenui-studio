package objectstore

import (
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
)

const maxDownloadTTL = 5 * time.Minute

func downloadTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return maxDownloadTTL, nil
	}
	if ttl < 0 || ttl > maxDownloadTTL {
		return 0, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "download ttl must be positive and at most five minutes"}
	}
	return ttl, nil
}

func opaqueDownloadToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

// NormalizeDownloadTTL applies the shared Artifact download TTL contract.
func NormalizeDownloadTTL(ttl time.Duration) (time.Duration, error) {
	return downloadTTL(ttl)
}

// NewOpaqueDownloadToken creates a shared Artifact opaque download token.
func NewOpaqueDownloadToken() (string, error) {
	return opaqueDownloadToken()
}

// IssueDownloadToken mints the token embedded in a download URL. When codec is
// nil it returns a random opaque token (the historical behaviour, non-redeemable
// by the platform). When codec is set it seals the object storage key and the
// URL's expiry into an encrypted token that Store.OpenDownload can redeem
// statelessly.
func IssueDownloadToken(codec *downloadtoken.Codec, key string, expiresAt time.Time) (string, error) {
	if codec == nil {
		return opaqueDownloadToken()
	}
	return codec.Seal(key, expiresAt)
}
