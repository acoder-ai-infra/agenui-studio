package objectstore

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

func validateObjectKey(key string) error {
	if key == "" || path.IsAbs(key) || filepath.IsAbs(key) || strings.Contains(key, `\`) || strings.IndexByte(key, 0) >= 0 {
		return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "invalid object key"}
	}
	if clean := path.Clean(key); clean == "." || clean != key {
		return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object key is not canonical"}
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object key is not canonical"}
		}
	}
	return nil
}

// ValidateObjectKey applies the canonical object-key rules shared by all
// Artifact ObjectStore adapters.
func ValidateObjectKey(key string) error {
	return validateObjectKey(key)
}
