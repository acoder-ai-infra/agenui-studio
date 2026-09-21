package ruleworker

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// baseline.go resolves the baseline revision from local pointer (a pointer to an local artifact
// zip) instead of a fixed directory, and publishes the newly generated revision
// back to local pointer. The pointer JSON is the same shape the main service's
// design-knowledge runtime consumes (runtime.RemotePointer), so a revision this
// worker publishes is directly loadable by the service.

// BaselineResolver produces a local directory containing the baseline revision
// (with manifest.json at its root) plus a cleanup to release it.
type BaselineResolver interface {
	Resolve(ctx context.Context) (baseDir string, cleanup func(), err error)
}

// PointerPublisher records the local artifact location of a newly published revision so the
// baseline advances to it (and the service can pick it up).
type PointerPublisher interface {
	Publish(ctx context.Context, artifactPath, revisionID, indexHash string) error
}

// DirBaselineResolver serves the baseline directly from a local directory. Used
// in tests and as a local fallback when local pointer is not wired.
type DirBaselineResolver struct {
	Dir string
}

func (r DirBaselineResolver) Resolve(_ context.Context) (string, func(), error) {
	if r.Dir == "" {
		return "", nil, fmt.Errorf("ruleworker baseline: directory is required")
	}
	log.Printf("[rule-worker] baseline fetch succeeded source=dir path=%s", r.Dir)
	return r.Dir, func() {}, nil
}

// materializeFS copies every file under fsys rooted at subdir into destDir,
// preserving structure relative to subdir.
func materializeFS(fsys fs.FS, subdir, destDir string) error {
	return fs.WalkDir(fsys, subdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(subdir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// revisionInfo reads the published revision id + index hash from a generated
// revision directory's manifest.json.
func revisionInfo(revisionDir string) (revisionID, indexHash string, err error) {
	var manifest struct {
		RevisionID string `json:"revision_id"`
		IndexHash  string `json:"index_hash"`
	}
	if err := readJSONFile(filepath.Join(revisionDir, "manifest.json"), &manifest); err != nil {
		return "", "", err
	}
	return manifest.RevisionID, manifest.IndexHash, nil
}
