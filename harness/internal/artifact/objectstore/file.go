package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
)

type FileObjectStore struct {
	rootDir      string
	downloadBase string
	codec        *downloadtoken.Codec
}

// SetDownloadCodec installs the optional token codec used to mint redeemable
// download tokens. Nil (the default) preserves opaque random tokens.
func (s *FileObjectStore) SetDownloadCodec(codec *downloadtoken.Codec) {
	s.codec = codec
}

func (s *FileObjectStore) DownloadCodec() *downloadtoken.Codec { return s.codec }

func (*FileObjectStore) ProductionReady() bool { return false }

func NewFileObjectStore(rootDir, downloadBase string) (*FileObjectStore, error) {
	if rootDir == "" {
		return nil, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "root dir is required"}
	}
	absoluteRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	absoluteRoot = filepath.Clean(absoluteRoot)
	if err := os.MkdirAll(absoluteRoot, 0o755); err != nil {
		return nil, err
	}
	evaluatedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Stat(evaluatedRoot)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return nil, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "root dir is not a directory"}
	}
	return &FileObjectStore{
		rootDir:      filepath.Clean(evaluatedRoot),
		downloadBase: strings.TrimRight(downloadBase, "/"),
	}, nil
}

func NewFile(rootDir, downloadBase string) (*FileObjectStore, error) {
	return NewFileObjectStore(rootDir, downloadBase)
}

func (s *FileObjectStore) Backend() string {
	return "file"
}

func (s *FileObjectStore) Put(_ context.Context, key string, r io.Reader) (info *artifact.ObjectInfo, err error) {
	finalPath, err := s.pathForKey(key)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(finalPath)
	if err := s.validateNearestExistingAncestor(parent); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	evaluatedParent, err := s.evaluateContainedPath(parent)
	if err != nil {
		return nil, err
	}
	finalPath = filepath.Join(evaluatedParent, filepath.Base(finalPath))

	tmp, err := os.CreateTemp(evaluatedParent, ".artifact-*")
	if err != nil {
		return nil, err
	}
	tmpClosed := false
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupErrors := make([]error, 0, 2)
		if !tmpClosed {
			if closeErr := tmp.Close(); closeErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("close temporary object: %w", closeErr))
			}
			tmpClosed = true
		}
		if removeErr := os.Remove(tmp.Name()); removeErr != nil && !os.IsNotExist(removeErr) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove temporary object: %w", removeErr))
		}
		if len(cleanupErrors) > 0 {
			causes := make([]error, 0, len(cleanupErrors)+1)
			if err != nil {
				causes = append(causes, err)
			}
			causes = append(causes, cleanupErrors...)
			err = errors.Join(causes...)
		}
	}()

	if chmodErr := tmp.Chmod(0o600); chmodErr != nil {
		return nil, fmt.Errorf("chmod temporary object: %w", chmodErr)
	}
	size, copyErr := io.Copy(tmp, r)
	if copyErr != nil {
		return nil, fmt.Errorf("copy object content: %w", copyErr)
	}
	if syncErr := tmp.Sync(); syncErr != nil {
		return nil, fmt.Errorf("sync temporary object: %w", syncErr)
	}
	closeErr := tmp.Close()
	tmpClosed = true
	if closeErr != nil {
		return nil, fmt.Errorf("close temporary object: %w", closeErr)
	}
	if renameErr := os.Rename(tmp.Name(), finalPath); renameErr != nil {
		return nil, fmt.Errorf("commit object: %w", renameErr)
	}
	committed = true
	return &artifact.ObjectInfo{Backend: s.Backend(), Key: key, Size: size}, nil
}

func (s *FileObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	objectPath, exists, err := s.existingObjectPath(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "object not found: " + key}
	}
	f, err := os.Open(objectPath)
	if os.IsNotExist(err) {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "object not found: " + key}
	}
	return f, err
}

func (s *FileObjectStore) Delete(_ context.Context, key string) error {
	objectPath, exists, err := s.existingObjectPath(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	err = os.Remove(objectPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *FileObjectStore) CreateDownloadURL(_ context.Context, key string, ttl time.Duration) (*artifact.DownloadURL, error) {
	_, exists, err := s.existingObjectPath(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "object not found: " + key}
	}
	effectiveTTL, err := downloadTTL(ttl)
	if err != nil {
		return nil, err
	}
	base := s.downloadBase
	if base == "" {
		base = "http://localhost/artifacts"
	}
	expiresAt := time.Now().Add(effectiveTTL)
	token, err := IssueDownloadToken(s.codec, key, expiresAt)
	if err != nil {
		return nil, err
	}
	return &artifact.DownloadURL{
		URL:       base + "/download/" + token,
		ExpiresAt: expiresAt,
	}, nil
}

func (s *FileObjectStore) pathForKey(key string) (string, error) {
	if s == nil || s.rootDir == "" {
		return "", &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "file object store is not configured"}
	}
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	fullPath := filepath.Join(s.rootDir, filepath.FromSlash(key))
	if err := s.requireContained(fullPath); err != nil {
		return "", err
	}
	return fullPath, nil
}

func (s *FileObjectStore) existingObjectPath(key string) (string, bool, error) {
	objectPath, err := s.pathForKey(key)
	if err != nil {
		return "", false, err
	}
	parent := filepath.Dir(objectPath)
	if err := s.validateNearestExistingAncestor(parent); err != nil {
		return "", false, err
	}
	parentInfo, err := os.Stat(parent)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !parentInfo.IsDir() {
		return "", false, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object parent is not a directory"}
	}
	evaluatedParent, err := s.evaluateContainedPath(parent)
	if err != nil {
		return "", false, err
	}
	objectPath = filepath.Join(evaluatedParent, filepath.Base(objectPath))
	objectInfo, err := os.Lstat(objectPath)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if objectInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object path is a symlink"}
	}
	if !objectInfo.Mode().IsRegular() {
		return "", false, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object path is not a regular file"}
	}
	evaluatedObject, err := s.evaluateContainedPath(objectPath)
	if err != nil {
		return "", false, err
	}
	return evaluatedObject, true, nil
}

func (s *FileObjectStore) validateNearestExistingAncestor(candidate string) error {
	ancestor := candidate
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object path has no existing ancestor"}
		}
		ancestor = parent
	}
	_, err := s.evaluateContainedPath(ancestor)
	return err
}

func (s *FileObjectStore) evaluateContainedPath(candidate string) (string, error) {
	evaluated, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	evaluated, err = filepath.Abs(evaluated)
	if err != nil {
		return "", err
	}
	evaluated = filepath.Clean(evaluated)
	if err := s.requireContained(evaluated); err != nil {
		return "", err
	}
	return evaluated, nil
}

func (s *FileObjectStore) requireContained(candidate string) error {
	rel, err := filepath.Rel(s.rootDir, candidate)
	if err != nil {
		return err
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "object path escapes root"}
	}
	return nil
}
