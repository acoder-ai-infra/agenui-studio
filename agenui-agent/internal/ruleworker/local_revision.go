package ruleworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
)

// LocalRevisionPointer is the durable local equivalent of the source remote
// pointer. It selects one immutable revision directory for the next worker
// pass and for service restart recovery.
type LocalRevisionPointer struct {
	RevisionID string `json:"revision_id"`
	IndexHash  string `json:"index_hash"`
}

func ReadLocalRevisionPointer(path string) (LocalRevisionPointer, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return LocalRevisionPointer{}, false, nil
	}
	if err != nil {
		return LocalRevisionPointer{}, false, fmt.Errorf("read local revision pointer: %w", err)
	}
	var pointer LocalRevisionPointer
	if err := json.Unmarshal(raw, &pointer); err != nil {
		return LocalRevisionPointer{}, false, fmt.Errorf("decode local revision pointer: %w", err)
	}
	pointer.RevisionID = strings.TrimSpace(pointer.RevisionID)
	if pointer.RevisionID == "" {
		return LocalRevisionPointer{}, false, errors.New("local revision pointer: revision_id is required")
	}
	return pointer, true, nil
}

func writeLocalRevisionPointer(path string, pointer LocalRevisionPointer) error {
	pointer.RevisionID = strings.TrimSpace(pointer.RevisionID)
	if pointer.RevisionID == "" {
		return errors.New("local revision pointer: revision_id is required")
	}
	raw, err := json.Marshal(pointer)
	if err != nil {
		return fmt.Errorf("encode local revision pointer: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create local pointer directory: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write local revision pointer: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit local revision pointer: %w", err)
	}
	return nil
}

type LocalRevisionBaselineResolver struct {
	FallbackDir  string
	RevisionRoot string
	PointerPath  string
}

func (r LocalRevisionBaselineResolver) Resolve(_ context.Context) (string, func(), error) {
	pointer, found, err := ReadLocalRevisionPointer(r.PointerPath)
	if err != nil {
		return "", nil, err
	}
	if found {
		candidate := filepath.Join(r.RevisionRoot, pointer.RevisionID)
		if info, statErr := os.Stat(filepath.Join(candidate, "manifest.json")); statErr == nil && !info.IsDir() {
			return candidate, func() {}, nil
		}
	}
	if strings.TrimSpace(r.FallbackDir) == "" {
		return "", nil, errors.New("local revision baseline: fallback directory is required")
	}
	return r.FallbackDir, func() {}, nil
}

type LocalRevisionPointerPublisher struct {
	RevisionRoot string
	PointerPath  string
	Activate     func(context.Context, string) error
}

func (p LocalRevisionPointerPublisher) Publish(ctx context.Context, _ string, revisionID, indexHash string) error {
	revisionID = strings.TrimSpace(revisionID)
	if revisionID == "" {
		return errors.New("local revision publication: revision id is required")
	}
	repository, err := designknowledge.Load(ctx, os.DirFS(p.RevisionRoot), revisionID)
	if err != nil {
		return fmt.Errorf("local revision publication: validate %s: %w", revisionID, err)
	}
	if repository.RevisionID() != revisionID {
		return fmt.Errorf("local revision publication: revision identity mismatch %s", revisionID)
	}
	if err := writeLocalRevisionPointer(p.PointerPath, LocalRevisionPointer{RevisionID: revisionID, IndexHash: indexHash}); err != nil {
		return err
	}
	if p.Activate != nil {
		if err := p.Activate(ctx, revisionID); err != nil {
			return fmt.Errorf("local revision publication: activate %s: %w", revisionID, err)
		}
	}
	return nil
}
